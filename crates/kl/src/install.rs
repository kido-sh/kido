use crate::db::{Db, Installed, write_atomic};
use crate::index::{Index, Package};
use crate::manifest::{FileEntry, Kind, MANIFEST_LIMIT, MANIFEST_PATH, Manifest};
use crate::repo::Repo;
use anyhow::{Context, Result, anyhow, bail, ensure};
use kido_trust::{BlobRef, HashingReader};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File, OpenOptions};
use std::io::{self, ErrorKind, Read};
use std::os::unix::fs::{OpenOptionsExt, symlink};
use std::path::{Path, PathBuf};

#[derive(Serialize, Deserialize, Debug, PartialEq, Eq)]
#[serde(tag = "op", rename_all = "lowercase")]
pub enum Op {
    Mkdir { path: String },
    Rename { from: String, to: String },
    Remove { path: String },
    Rmdir { path: String },
}

#[derive(Serialize, Deserialize, Debug, Default)]
pub struct Journal {
    pub ops: Vec<Op>,
    pub put: Vec<Installed>,
    pub delete: Vec<String>,
    pub world: BTreeSet<String>,
    pub seen: Option<(String, u64)>,
}

pub fn closure<'a>(
    index: &'a Index,
    root: &Path,
    wanted: &BTreeSet<String>,
) -> Result<BTreeMap<String, &'a Package>> {
    let mut out = BTreeMap::new();
    let mut queue: Vec<String> = wanted.iter().cloned().collect();
    while let Some(name) = queue.pop() {
        if out.contains_key(&name) {
            continue;
        }
        let p = index
            .find(&name)
            .ok_or_else(|| anyhow!("no package named {name}"))?;
        out.insert(name.clone(), p);
        for dep in &p.depends {
            if let Some(q) = index.find(dep).or_else(|| index.provider(dep)) {
                if q.name != p.name {
                    queue.push(q.name.clone());
                }
                continue;
            }
            if let Some(path) = dep.strip_prefix("dylib:") {
                if root.join(path.trim_start_matches('/')).exists() {
                    continue;
                }
            }
            bail!("{name} needs {dep}, which nothing provides");
        }
    }
    Ok(out)
}

#[must_use]
pub fn installed_index(installed: &BTreeMap<String, Installed>, arch: &str) -> Index {
    Index {
        version: 0,
        arch: arch.to_owned(),
        packages: installed
            .values()
            .map(|i| Package {
                name: i.manifest.name.clone(),
                version: i.manifest.version.clone(),
                summary: i.manifest.summary.clone(),
                license: i.manifest.license.clone(),
                blob: i.blob.clone(),
                installed_size: i.manifest.file_bytes(),
                provides: i.manifest.provides.clone(),
                depends: i.manifest.depends.clone(),
            })
            .collect(),
    }
}

pub fn sync(
    db: &Db,
    repo: Option<&Repo>,
    arch: &str,
    wanted: &BTreeMap<String, &Package>,
    world: BTreeSet<String>,
    seen: Option<(String, u64)>,
) -> Result<Vec<String>> {
    let installed = db.installed()?;
    let to_install: Vec<&Package> = wanted
        .values()
        .copied()
        .filter(|p| installed.get(&p.name).is_none_or(|i| i.blob != p.blob))
        .collect();
    let to_remove: Vec<&Installed> = installed
        .values()
        .filter(|i| !wanted.contains_key(&i.manifest.name))
        .collect();
    let staging = db.staging();
    discard(&staging)?;
    fs::create_dir_all(&staging)?;
    let result = (|| {
        let mut staged = Vec::new();
        for p in &to_install {
            let repo = repo.ok_or_else(|| anyhow!("{} has to be downloaded", p.name))?;
            staged.push(((*p).clone(), stage(repo, p, arch, &staging)?));
        }
        plan(&db.root, &installed, &staged, &to_remove, world, seen)
    })();
    let (journal, warnings) = match result {
        Ok(x) => x,
        Err(e) => {
            discard(&staging)?;
            return Err(e);
        }
    };
    write_atomic(&db.journal(), &serde_json::to_vec(&journal)?)?;
    apply(db, &journal)?;
    Ok(warnings)
}

pub fn stage(repo: &Repo, pkg: &Package, arch: &str, staging: &Path) -> Result<Manifest> {
    let dir = staging.join(&pkg.name);
    fs::create_dir_all(&dir)?;
    let raw = repo.open(&format!("blobs/sha256/{}", pkg.blob.sha256))?;
    let mut blob = HashingReader::new(raw, pkg.blob.size);
    let manifest =
        unpack(&mut blob, pkg, arch, &dir).with_context(|| format!("unpack {}", pkg.name))?;
    blob.finish(&pkg.blob)
        .with_context(|| format!("verify {}", pkg.name))?;
    Ok(manifest)
}

fn unpack(blob: &mut impl Read, pkg: &Package, arch: &str, dir: &Path) -> Result<Manifest> {
    let decoder = ruzstd::decoding::StreamingDecoder::new(blob).map_err(|e| anyhow!("{e}"))?;
    let mut archive = tar::Archive::new(decoder);
    let mut entries = archive.entries()?;
    let mut first = entries
        .next()
        .ok_or_else(|| anyhow!("the package is empty"))??;
    ensure!(
        entry_path(&first)? == MANIFEST_PATH,
        "the manifest is not the first member"
    );
    let mut json = Vec::new();
    (&mut first)
        .take(MANIFEST_LIMIT + 1)
        .read_to_end(&mut json)?;
    ensure!(
        json.len() as u64 <= MANIFEST_LIMIT,
        "the manifest is too large"
    );
    drop(first);
    let manifest: Manifest = serde_json::from_slice(&json)?;
    manifest.validate(&pkg.name, &pkg.version, arch)?;
    ensure!(
        manifest.provides == pkg.provides && manifest.depends == pkg.depends,
        "the manifest's provides or depends differ from the index"
    );
    ensure!(
        manifest.file_bytes() <= pkg.installed_size,
        "the files are larger than the index allows"
    );
    let want = manifest.by_path();
    let mut seen = BTreeSet::new();
    for entry in entries {
        let mut entry = entry?;
        let path = entry_path(&entry)?;
        let f = *want
            .get(path.as_str())
            .ok_or_else(|| anyhow!("{path} is not in the manifest"))?;
        ensure!(seen.insert(path.clone()), "{path} appears twice");
        let dest = dir.join(&path);
        let ty = entry.header().entry_type();
        if let Some(parent) = dest.parent() {
            fs::create_dir_all(parent)?;
        }
        match f.kind {
            Kind::Dir => {
                ensure!(ty.is_dir(), "{path} should be a directory");
                fs::create_dir_all(&dest)?;
            }
            Kind::File => {
                ensure!(ty.is_file(), "{path} should be a file");
                let want = BlobRef {
                    sha256: f.sha256.clone().unwrap_or_default(),
                    size: f.size.unwrap_or_default(),
                };
                let mut out = OpenOptions::new()
                    .write(true)
                    .create_new(true)
                    .mode(f.mode & 0o777)
                    .open(&dest)
                    .with_context(|| format!("create {}", dest.display()))?;
                let mut hashed = HashingReader::new(&mut entry, want.size);
                io::copy(&mut hashed, &mut out)?;
                hashed.finish(&want).with_context(|| path.clone())?;
            }
            Kind::Symlink => {
                ensure!(ty.is_symlink(), "{path} should be a symlink");
                let target = f.target.clone().unwrap_or_default();
                let link = entry
                    .link_name()?
                    .ok_or_else(|| anyhow!("{path} has no link target"))?;
                ensure!(
                    link.to_str() == Some(target.as_str()),
                    "{path} links somewhere the manifest does not say"
                );
                symlink(&target, &dest)?;
            }
        }
    }
    ensure!(
        seen.len() == manifest.files.len(),
        "the package lacks files its manifest lists"
    );
    Ok(manifest)
}

fn entry_path<R: Read>(entry: &tar::Entry<'_, R>) -> Result<String> {
    let path = entry.path()?;
    let s = path
        .to_str()
        .ok_or_else(|| anyhow!("a path in the package is not UTF-8"))?;
    Ok(s.trim_end_matches('/').to_owned())
}

fn plan(
    root: &Path,
    installed: &BTreeMap<String, Installed>,
    staged: &[(Package, Manifest)],
    removed: &[&Installed],
    world: BTreeSet<String>,
    seen: Option<(String, u64)>,
) -> Result<(Journal, Vec<String>)> {
    let leaving: BTreeSet<&str> = removed.iter().map(|i| i.manifest.name.as_str()).collect();
    let mut owner: BTreeMap<&str, &str> = BTreeMap::new();
    for i in installed.values() {
        for f in i.manifest.files.iter().filter(|f| f.kind != Kind::Dir) {
            owner.insert(&f.path, &i.manifest.name);
        }
    }
    let mut journal = Journal {
        world,
        seen,
        ..Journal::default()
    };
    let mut claimed: BTreeMap<&str, &str> = BTreeMap::new();
    let mut new_dirs = BTreeSet::new();
    let mut renames = Vec::new();
    for (p, m) in staged {
        for f in &m.files {
            if f.kind == Kind::Dir {
                new_dirs.insert(f.path.as_str());
                continue;
            }
            if let Some(other) = claimed.insert(&f.path, &p.name) {
                bail!("{other} and {} both contain {}", p.name, f.path);
            }
            match fs::symlink_metadata(root.join(&f.path)) {
                Ok(md) => {
                    ensure!(!md.is_dir(), "{} is a directory on disk", f.path);
                    match owner.get(f.path.as_str()) {
                        Some(o) if *o == p.name || leaving.contains(o) => {}
                        Some(o) => bail!("{} already belongs to {o}", f.path),
                        None => bail!("{} exists and no package owns it", f.path),
                    }
                }
                Err(e) if e.kind() == ErrorKind::NotFound => {}
                Err(e) => return Err(e.into()),
            }
            renames.push(Op::Rename {
                from: format!("var/db/kl/staging/{}/{}", p.name, f.path),
                to: f.path.clone(),
            });
        }
        journal.put.push(Installed {
            manifest: m.clone(),
            blob: p.blob.clone(),
        });
    }
    journal.ops.extend(new_dirs.iter().map(|d| Op::Mkdir {
        path: (*d).to_owned(),
    }));
    journal.ops.extend(renames);

    let mut stale: Vec<&FileEntry> = removed
        .iter()
        .flat_map(|i| i.manifest.files.iter())
        .collect();
    for (p, m) in staged {
        if let Some(old) = installed.get(&p.name) {
            let keep = m.by_path();
            stale.extend(
                old.manifest
                    .files
                    .iter()
                    .filter(|f| !keep.contains_key(f.path.as_str())),
            );
        }
    }
    let mut warnings = Vec::new();
    let mut stale_dirs = BTreeSet::new();
    for f in stale {
        if claimed.contains_key(f.path.as_str()) || new_dirs.contains(f.path.as_str()) {
            continue;
        }
        if f.kind == Kind::Dir {
            stale_dirs.insert(f.path.clone());
        } else if unchanged(root, f) {
            journal.ops.push(Op::Remove {
                path: f.path.clone(),
            });
        } else if fs::symlink_metadata(root.join(&f.path)).is_ok() {
            warnings.push(format!(
                "kept {}: it was changed after installation",
                f.path
            ));
        }
    }
    journal
        .ops
        .extend(stale_dirs.into_iter().rev().map(|path| Op::Rmdir { path }));
    journal.delete = removed.iter().map(|i| i.manifest.name.clone()).collect();
    Ok((journal, warnings))
}

pub fn apply(db: &Db, journal: &Journal) -> Result<()> {
    let root = &db.root;
    for op in &journal.ops {
        match op {
            Op::Mkdir { path } => real_dirs(root, Path::new(path))?,
            Op::Rename { from, to } => {
                let from = root.join(from);
                if fs::symlink_metadata(&from).is_ok() {
                    if let Some(parent) = Path::new(to).parent() {
                        real_dirs(root, parent)?;
                    }
                    fs::rename(&from, root.join(to)).with_context(|| format!("install {to}"))?;
                }
            }
            Op::Remove { path } => match fs::remove_file(root.join(path)) {
                Err(e) if e.kind() != ErrorKind::NotFound => {
                    return Err(e).context(format!("remove {path}"));
                }
                _ => {}
            },
            Op::Rmdir { path } => {
                let _ = fs::remove_dir(root.join(path));
            }
        }
    }
    for i in &journal.put {
        db.put(i)?;
    }
    for name in &journal.delete {
        db.delete(name)?;
    }
    db.set_world(&journal.world)?;
    if let Some((channel, version)) = &journal.seen {
        let mut state = db.state()?;
        let seen = state.seen.entry(channel.clone()).or_default();
        *seen = (*seen).max(*version);
        db.put_state(&state)?;
    }
    discard(&db.staging())?;
    fs::remove_file(db.journal())?;
    Ok(())
}

pub fn recover(db: &Db) -> Result<bool> {
    match fs::read(db.journal()) {
        Ok(bytes) => {
            let journal: Journal = serde_json::from_slice(&bytes).context("read the journal")?;
            apply(db, &journal)?;
            Ok(true)
        }
        Err(e) if e.kind() == ErrorKind::NotFound => {
            discard(&db.staging())?;
            Ok(false)
        }
        Err(e) => Err(e.into()),
    }
}

#[must_use]
pub fn verify(root: &Path, installed: &Installed) -> Vec<String> {
    let mut problems = Vec::new();
    for f in &installed.manifest.files {
        let path = root.join(&f.path);
        let present = fs::symlink_metadata(&path).is_ok();
        let ok = match f.kind {
            Kind::Dir => path.is_dir(),
            _ => unchanged(root, f),
        };
        if !ok {
            problems.push(format!(
                "{}: {}",
                f.path,
                if present { "changed" } else { "missing" }
            ));
        }
    }
    problems
}

fn unchanged(root: &Path, f: &FileEntry) -> bool {
    let path = root.join(&f.path);
    match f.kind {
        Kind::Dir => path.is_dir(),
        Kind::Symlink => fs::read_link(&path).is_ok_and(|t| t.to_str() == f.target.as_deref()),
        Kind::File => {
            fs::symlink_metadata(&path).is_ok_and(|m| m.is_file())
                && file_sha256(&path).is_ok_and(|s| Some(s) == f.sha256)
        }
    }
}

fn file_sha256(path: &Path) -> io::Result<String> {
    let mut file = File::open(path)?;
    let mut hasher = Sha256::new();
    io::copy(&mut file, &mut hasher)?;
    Ok(hex::encode(hasher.finalize()))
}

fn real_dirs(root: &Path, rel: &Path) -> Result<()> {
    let mut cur: PathBuf = root.to_path_buf();
    for c in rel.components() {
        cur.push(c);
        match fs::symlink_metadata(&cur) {
            Ok(md) if md.is_dir() => {}
            Ok(md) if md.file_type().is_symlink() => {
                bail!(
                    "{} is a symlink; kl does not install through one",
                    cur.display()
                )
            }
            Ok(_) => bail!("{} is not a directory", cur.display()),
            Err(e) if e.kind() == ErrorKind::NotFound => {
                fs::create_dir(&cur).with_context(|| format!("create {}", cur.display()))?;
            }
            Err(e) => return Err(e.into()),
        }
    }
    Ok(())
}

fn discard(staging: &Path) -> Result<()> {
    match fs::remove_dir_all(staging) {
        Err(e) if e.kind() != ErrorKind::NotFound => Err(e).context("clear the staging directory"),
        _ => Ok(()),
    }
}
