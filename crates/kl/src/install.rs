use crate::db::{Db, Installed, read_file_nofollow, write_atomic};
use crate::index::{Index, Package};
use crate::manifest::{
    ENTRY_LIMIT, FileEntry, Kind, MANIFEST_LIMIT, MANIFEST_PATH, Manifest, check_name, check_path,
    fold_path,
};
use crate::repo::Repo;
use anyhow::{Context, Result, anyhow, bail, ensure};
use kido_trust::{
    BlobRef, CHUNK_SIZE, ChunkList, ChunkReader, DEFAULT_CHANNEL, DEFAULT_KL_VERSION,
    HashingReader, RepoTarget, valid_sha256,
};
use rustix::fd::OwnedFd;
use rustix::fs::{
    AtFlags, CWD, Mode, OFlags, chmodat, mkdirat, openat, readlinkat, renameat, statat, symlinkat,
    unlinkat,
};
use rustix::io::Errno;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File, Permissions};
use std::io::{self, Cursor, ErrorKind, Read};
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

const STAGING_PREFIX: &str = "var/db/kl/staging/";
const MAX_ZSTD_WINDOW_SIZE: u64 = 128 << 20;
const MAX_CHUNK_LIST_SIZE: u64 = 4 << 20;
const MAX_JOURNAL_SIZE: u64 = 64 << 20;
const ZSTD_MAGIC: [u8; 4] = [0x28, 0xb5, 0x2f, 0xfd];
const FILE_MODE_MASK: u32 = 0o775;
static XDEV_COUNTER: AtomicU64 = AtomicU64::new(0);

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
                if let Some(rel) = safe_abs_path(path) {
                    if confined_path_exists(root, rel) {
                        continue;
                    }
                }
            }
            bail!("{name} needs {dep}, which nothing provides");
        }
    }
    Ok(out)
}

fn safe_abs_path(path: &str) -> Option<&str> {
    let rel = path.strip_prefix('/')?;
    if rel.is_empty()
        || rel.ends_with('/')
        || rel.contains('\0')
        || !rel
            .split('/')
            .all(|c| !c.is_empty() && c != "." && c != "..")
    {
        return None;
    }
    Some(rel)
}

fn confined_path_exists(root: &Path, rel: &str) -> bool {
    let Ok(Some((parent_fd, leaf))) = open_parent_existing(root, rel) else {
        return false;
    };
    statat(&parent_fd, leaf, AtFlags::SYMLINK_NOFOLLOW).is_ok()
}

#[must_use]
pub fn installed_index(installed: &BTreeMap<String, Installed>, arch: &str) -> Index {
    let target = RepoTarget {
        kl_version: DEFAULT_KL_VERSION.to_owned(),
        major: 0,
        minor: 0,
        patch: 0,
        build: "local".to_owned(),
        channel: DEFAULT_CHANNEL.to_owned(),
    };
    Index {
        version: 1,
        target,
        arch: arch.to_owned(),
        packages: installed
            .values()
            .map(|i| Package {
                name: i.manifest.name.clone(),
                version: i.manifest.version.clone(),
                summary: i.manifest.summary.clone(),
                license: i.manifest.license.clone(),
                blob: i.blob.clone(),
                installed_size: i.manifest.file_bytes().unwrap_or(0),
                chunks: None,
                provides: i.manifest.provides.clone(),
                depends: i.manifest.depends.clone(),
                files: i
                    .manifest
                    .files
                    .iter()
                    .filter(|f| f.kind != Kind::Dir)
                    .map(|f| f.path.clone())
                    .collect(),
            })
            .collect(),
    }
}

pub fn sync(
    db: &Db,
    sources: Option<&[&Repo]>,
    target: Option<&RepoTarget>,
    arch: &str,
    wanted: &BTreeMap<String, &Package>,
    world: BTreeSet<String>,
    seen: Option<(String, u64)>,
) -> Result<Vec<String>> {
    for p in wanted.values() {
        check_name(&p.name)?;
    }
    for name in &world {
        check_name(name)?;
    }
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
    no_symlink_ancestors(&db.root, "var/db/kl/staging")?;
    discard(&staging)?;
    let _ = open_dir_chain_create(&db.root, "var/db/kl/staging")?;
    let mut warnings = Vec::new();
    let result = (|| {
        let mut staged = Vec::new();
        for p in &to_install {
            let repos = sources
                .filter(|s| !s.is_empty())
                .ok_or_else(|| anyhow!("{} has to be downloaded", p.name))?;
            let tgt = target.ok_or_else(|| anyhow!("{} has no repository target", p.name))?;
            staged.push((
                (*p).clone(),
                stage_with_failover(repos, tgt, p, arch, &staging, &mut warnings)?,
            ));
        }
        plan(&db.root, &installed, &staged, &to_remove, world, seen)
    })();
    let (journal, plan_warnings) = match result {
        Ok(x) => x,
        Err(e) => {
            let _ = discard(&staging);
            return Err(e);
        }
    };
    warnings.extend(plan_warnings);
    write_atomic(&db.journal(), &serde_json::to_vec(&journal)?)?;
    apply(db, &journal)?;
    Ok(warnings)
}

fn stage_with_failover(
    sources: &[&Repo],
    target: &RepoTarget,
    pkg: &Package,
    arch: &str,
    staging: &Path,
    warnings: &mut Vec<String>,
) -> Result<Manifest> {
    check_name(&pkg.name)?;
    let mut last_err = None;
    for (idx, repo) in sources.iter().enumerate() {
        let dir = staging.join(&pkg.name);
        discard(&dir)?;
        match stage(repo, target, pkg, arch, staging) {
            Ok(manifest) => return Ok(manifest),
            Err(e) => {
                let _ = discard(&dir);
                if idx + 1 < sources.len() {
                    warnings.push(format!(
                        "warning: mirror {} failed for {} ({e:#}); trying next source",
                        repo.url(),
                        pkg.name
                    ));
                    last_err = Some(e);
                } else {
                    return Err(e);
                }
            }
        }
    }
    Err(last_err.unwrap_or_else(|| anyhow!("no repository source available for {}", pkg.name)))
}

pub fn stage(
    repo: &Repo,
    target: &RepoTarget,
    pkg: &Package,
    arch: &str,
    staging: &Path,
) -> Result<Manifest> {
    check_name(&pkg.name)?;
    let dir = staging.join(&pkg.name);
    let _ = open_dir_chain_create(staging, &pkg.name)?;
    let chunks = chunk_list(repo, target, pkg)?;
    let blob_rel = target.blob_path(&pkg.blob.sha256)?;
    let raw = repo.open(&blob_rel)?;
    let mut blob = ChunkReader::new(raw, chunks, &pkg.blob)?;
    let manifest =
        unpack(&mut blob, pkg, arch, &dir).with_context(|| format!("unpack {}", pkg.name))?;
    blob.finish(&pkg.blob)
        .with_context(|| format!("verify {}", pkg.name))?;
    Ok(manifest)
}

fn chunk_list(repo: &Repo, target: &RepoTarget, pkg: &Package) -> Result<ChunkList> {
    let Some(list_ref) = &pkg.chunks else {
        ensure!(
            pkg.blob.size <= CHUNK_SIZE,
            "{} is larger than one chunk but lists no chunk hashes",
            pkg.name
        );
        return Ok(ChunkList::whole(&pkg.blob));
    };
    ensure!(
        list_ref.size <= MAX_CHUNK_LIST_SIZE,
        "{} chunk list is larger than {MAX_CHUNK_LIST_SIZE} bytes",
        pkg.name
    );
    let rel_path = target.blob_path(&list_ref.sha256)?;
    let bytes = repo.read(&rel_path, list_ref.size)?;
    list_ref.check(&bytes)?;
    let list: ChunkList = serde_json::from_slice(&bytes)?;
    list.validate(&pkg.blob)?;
    Ok(list)
}

struct LimitedReader<R> {
    inner: R,
    remaining: u64,
}

impl<R: Read> LimitedReader<R> {
    fn new(inner: R, limit: u64) -> Self {
        LimitedReader {
            inner,
            remaining: limit,
        }
    }
}

impl<R: Read> Read for LimitedReader<R> {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        if buf.is_empty() {
            return Ok(0);
        }
        let max = usize::try_from(self.remaining.saturating_add(1)).unwrap_or(usize::MAX);
        let to_read = buf.len().min(max);
        let n = self.inner.read(&mut buf[..to_read])?;
        if n as u64 > self.remaining {
            return Err(io::Error::other(
                "decompressed package stream exceeds allowed size",
            ));
        }
        self.remaining -= n as u64;
        Ok(n)
    }
}

/// Inspects an RFC 8878 Zstandard frame header BEFORE handing bytes to `ruzstd`, enforcing strict
/// window-size and declared frame-content-size caps.
fn inspect_zstd_header(reader: &mut impl Read, max_decompressed: u64) -> Result<Vec<u8>> {
    let mut hdr = Vec::with_capacity(18);
    let mut prefix = [0u8; 5];
    reader
        .read_exact(&mut prefix)
        .context("read zstd frame header")?;
    ensure!(prefix[..4] == ZSTD_MAGIC, "blob is not a zstd frame");
    hdr.extend_from_slice(&prefix);

    let fhd = prefix[4];
    let fcs_flag = (fhd >> 6) & 3;
    let single_segment = ((fhd >> 5) & 1) != 0;
    let dict_id_flag = fhd & 3;
    ensure!(
        dict_id_flag == 0,
        "zstd external dictionaries are not allowed"
    );

    let mut window_size: Option<u64> = None;
    if !single_segment {
        let mut wd = [0u8; 1];
        reader
            .read_exact(&mut wd)
            .context("read zstd window descriptor")?;
        hdr.push(wd[0]);
        let exponent = u32::from(wd[0] >> 3);
        let mantissa = u64::from(wd[0] & 7);
        let window_log = 10 + exponent;
        ensure!(window_log <= 31, "zstd window size exceeds maximum");
        let base = 1u64 << window_log;
        let ws = base + (base / 8) * mantissa;
        ensure!(
            ws <= MAX_ZSTD_WINDOW_SIZE,
            "zstd window size {ws} exceeds limit {MAX_ZSTD_WINDOW_SIZE}"
        );
        window_size = Some(ws);
    }

    let fcs_len = match (fcs_flag, single_segment) {
        (0, false) => 0,
        (0, true) => 1,
        (1, _) => 2,
        (2, _) => 4,
        (3, _) => 8,
        _ => unreachable!(),
    };
    if fcs_len > 0 {
        let mut fcs_buf = [0u8; 8];
        reader
            .read_exact(&mut fcs_buf[..fcs_len])
            .context("read zstd frame content size")?;
        hdr.extend_from_slice(&fcs_buf[..fcs_len]);
        let fcs = match fcs_len {
            1 => u64::from(fcs_buf[0]),
            2 => u64::from(u16::from_le_bytes([fcs_buf[0], fcs_buf[1]])) + 256,
            4 => u64::from(u32::from_le_bytes([
                fcs_buf[0], fcs_buf[1], fcs_buf[2], fcs_buf[3],
            ])),
            8 => u64::from_le_bytes(fcs_buf),
            _ => unreachable!(),
        };
        ensure!(
            fcs <= max_decompressed,
            "zstd frame content size {fcs} exceeds allowed size {max_decompressed}"
        );
        if single_segment {
            ensure!(
                fcs <= MAX_ZSTD_WINDOW_SIZE,
                "zstd single-segment window size {fcs} exceeds limit {MAX_ZSTD_WINDOW_SIZE}"
            );
        }
    } else if window_size.is_none() {
        bail!("zstd frame lacks window size");
    }

    Ok(hdr)
}

fn max_decompressed_bytes(pkg: &Package) -> Result<u64> {
    let entries_est = (pkg.files.len() as u64).saturating_add(1024);
    let tar_overhead = entries_est
        .saturating_mul(2048)
        .min((ENTRY_LIMIT as u64).saturating_mul(1024))
        .min(64 << 20);
    pkg.installed_size
        .checked_add(MANIFEST_LIMIT)
        .and_then(|n| n.checked_add(tar_overhead))
        .ok_or_else(|| anyhow!("package installed_size overflows"))
}

fn unpack(blob: &mut impl Read, pkg: &Package, arch: &str, dir: &Path) -> Result<Manifest> {
    let max_decompressed = max_decompressed_bytes(pkg)?;
    let hdr = inspect_zstd_header(blob, max_decompressed)?;
    let stream = Cursor::new(hdr).chain(blob);
    let decoder = ruzstd::decoding::StreamingDecoder::new(stream).map_err(|e| anyhow!("{e}"))?;
    let bounded = LimitedReader::new(decoder, max_decompressed);
    let mut archive = tar::Archive::new(bounded);
    let mut entries = archive.entries()?;
    let mut first = entries
        .next()
        .ok_or_else(|| anyhow!("the package is empty"))??;
    ensure!(
        entry_path(&first)? == MANIFEST_PATH,
        "the manifest is not the first member"
    );
    ensure!(
        first.header().entry_type().is_file(),
        "the manifest must be a regular file"
    );
    ensure!(first.size() <= MANIFEST_LIMIT, "the manifest is too large");
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
    let manifest_files: BTreeSet<&str> = manifest
        .files
        .iter()
        .filter(|f| f.kind != Kind::Dir)
        .map(|f| f.path.as_str())
        .collect();
    let index_files: BTreeSet<&str> = pkg.files.iter().map(String::as_str).collect();
    ensure!(
        manifest_files == index_files,
        "the manifest's file list differs from the index"
    );
    ensure!(
        manifest.file_bytes()? <= pkg.installed_size,
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
        unpack_entry(&mut entry, f, &path, dir)?;
    }
    ensure!(
        seen.len() == manifest.files.len(),
        "the package lacks files its manifest lists"
    );
    Ok(manifest)
}

fn unpack_entry<R: Read>(
    entry: &mut tar::Entry<'_, R>,
    f: &FileEntry,
    path: &str,
    dir: &Path,
) -> Result<()> {
    let ty = entry.header().entry_type();
    match f.kind {
        Kind::Dir => {
            ensure!(ty.is_dir(), "{path} should be a directory");
            ensure!(entry.size() == 0, "{path} directory has non-zero tar size");
            let _ = open_dir_chain_create(dir, path)?;
        }
        Kind::File => {
            ensure!(ty.is_file(), "{path} should be a file");
            let want = BlobRef {
                sha256: f.sha256.clone().unwrap_or_default(),
                size: f.size.unwrap_or_default(),
            };
            ensure!(
                entry.size() == want.size,
                "{path} tar size does not match manifest"
            );
            let (parent_fd, leaf) = open_parent_create(dir, path)?;
            let mode = sanitize_file_mode(f.mode);
            let fd = openat(
                &parent_fd,
                leaf,
                OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::from_raw_mode(mode_t(mode)),
            )
            .with_context(|| format!("create {}", dir.join(path).display()))?;
            let mut out = File::from(fd);
            out.set_permissions(Permissions::from_mode(mode))
                .with_context(|| format!("chmod {}", dir.join(path).display()))?;
            let mut hashed = HashingReader::new(entry, want.size);
            io::copy(&mut hashed, &mut out)?;
            hashed.finish(&want).with_context(|| path.to_owned())?;
        }
        Kind::Symlink => {
            ensure!(ty.is_symlink(), "{path} should be a symlink");
            ensure!(entry.size() == 0, "{path} symlink has non-zero tar size");
            let target = f.target.clone().unwrap_or_default();
            let link = entry
                .link_name()?
                .ok_or_else(|| anyhow!("{path} has no link target"))?;
            ensure!(
                link.to_str() == Some(target.as_str()),
                "{path} links somewhere the manifest does not say"
            );
            let (parent_fd, leaf) = open_parent_create(dir, path)?;
            symlinkat(target.as_str(), &parent_fd, leaf)
                .with_context(|| format!("symlink {}", dir.join(path).display()))?;
        }
    }
    Ok(())
}

fn sanitize_file_mode(mode: u32) -> u32 {
    mode & FILE_MODE_MASK
}

#[allow(clippy::cast_possible_truncation)]
fn mode_t(mode: u32) -> rustix::fs::RawMode {
    mode as rustix::fs::RawMode
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
    let mut owner_ci: BTreeMap<String, &str> = BTreeMap::new();
    for i in installed.values() {
        for f in i.manifest.files.iter().filter(|f| f.kind != Kind::Dir) {
            owner_ci.insert(fold_path(&f.path), &i.manifest.name);
        }
    }
    let mut journal = Journal {
        world,
        seen,
        ..Journal::default()
    };
    let mut claimed_ci: BTreeMap<String, &str> = BTreeMap::new();
    let mut new_dirs = BTreeSet::new();
    let mut new_dirs_ci = BTreeSet::new();
    let mut renames = Vec::new();
    for (p, m) in staged {
        for f in &m.files {
            no_symlink_ancestors(root, &f.path)?;
            let folded = fold_path(&f.path);
            if f.kind == Kind::Dir {
                if let Ok(md) = fs::symlink_metadata(root.join(&f.path)) {
                    ensure!(
                        md.is_dir(),
                        "{} already exists and is not a directory",
                        f.path
                    );
                }
                new_dirs.insert(f.path.as_str());
                new_dirs_ci.insert(folded);
                continue;
            }
            if let Some(other) = claimed_ci.insert(folded.clone(), &p.name) {
                bail!("{other} and {} both contain {}", p.name, f.path);
            }
            match fs::symlink_metadata(root.join(&f.path)) {
                Ok(md) => {
                    ensure!(!md.is_dir(), "{} is a directory on disk", f.path);
                    match owner_ci.get(&folded) {
                        Some(o) if *o == p.name || leaving.contains(o) => {}
                        Some(o) => bail!("{} already belongs to {o}", f.path),
                        None => bail!("{} exists and no package owns it", f.path),
                    }
                }
                Err(e) if e.kind() == ErrorKind::NotFound => {}
                Err(e) => return Err(e.into()),
            }
            renames.push(Op::Rename {
                from: format!("{STAGING_PREFIX}{}/{}", p.name, f.path),
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

    let stale = collect_stale(installed, staged, removed);
    let warnings = plan_removals(root, &stale, &claimed_ci, &new_dirs_ci, &mut journal);
    journal.delete = removed.iter().map(|i| i.manifest.name.clone()).collect();
    Ok((journal, warnings))
}

fn collect_stale<'a>(
    installed: &'a BTreeMap<String, Installed>,
    staged: &'a [(Package, Manifest)],
    removed: &[&'a Installed],
) -> Vec<&'a FileEntry> {
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
    stale
}

fn plan_removals(
    root: &Path,
    stale: &[&FileEntry],
    claimed_ci: &BTreeMap<String, &str>,
    new_dirs_ci: &BTreeSet<String>,
    journal: &mut Journal,
) -> Vec<String> {
    let mut warnings = Vec::new();
    let mut stale_dirs = BTreeSet::new();
    for f in stale {
        let folded = fold_path(&f.path);
        if claimed_ci.contains_key(&folded) || new_dirs_ci.contains(&folded) {
            continue;
        }
        if no_symlink_ancestors(root, &f.path).is_err() {
            warnings.push(format!(
                "kept {}: an ancestor directory is now a symlink",
                f.path
            ));
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
    warnings
}

fn validate_journal(journal: &Journal) -> Result<()> {
    for op in &journal.ops {
        match op {
            Op::Mkdir { path } | Op::Remove { path } | Op::Rmdir { path } => check_path(path)?,
            Op::Rename { from, to } => {
                check_staging_path(from)?;
                check_path(to)?;
            }
        }
    }
    for i in &journal.put {
        i.manifest
            .validate(&i.manifest.name, &i.manifest.version, &i.manifest.arch)?;
        ensure!(
            valid_sha256(&i.blob.sha256),
            "journal blob {} has bad sha256",
            i.manifest.name
        );
    }
    for name in &journal.delete {
        check_name(name)?;
    }
    for name in &journal.world {
        check_name(name)?;
    }
    if let Some((key, version)) = &journal.seen {
        ensure!(
            !key.is_empty() && *version >= 1 && !key.bytes().any(|b| b < 0x20 || b == 0x7f),
            "invalid seen entry in journal"
        );
    }
    Ok(())
}

fn check_staging_path(path: &str) -> Result<()> {
    let rest = path
        .strip_prefix(STAGING_PREFIX)
        .ok_or_else(|| anyhow!("journal staging path {path:?} escapes staging"))?;
    ensure!(
        !rest.is_empty()
            && !rest.contains('\0')
            && !rest.ends_with('/')
            && rest
                .split('/')
                .all(|c| !c.is_empty() && c != "." && c != ".."),
        "bad journal staging path {path:?}"
    );
    Ok(())
}

pub fn apply(db: &Db, journal: &Journal) -> Result<()> {
    validate_journal(journal)?;
    let root = &db.root;
    for op in &journal.ops {
        match op {
            Op::Mkdir { path } => {
                let _ = open_dir_chain_create(root, path)?;
            }
            Op::Rename { from, to } => {
                apply_rename(root, from, to)?;
            }
            Op::Remove { path } => {
                if let Some((parent_fd, leaf)) = open_parent_existing(root, path)? {
                    match unlinkat(&parent_fd, leaf, AtFlags::empty()) {
                        Ok(()) | Err(Errno::NOENT) => {}
                        Err(e) => return Err(e).with_context(|| format!("remove {path}")),
                    }
                }
            }
            Op::Rmdir { path } => {
                if let Ok(Some((parent_fd, leaf))) = open_parent_existing(root, path) {
                    let _ = unlinkat(&parent_fd, leaf, AtFlags::REMOVEDIR);
                }
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
    if let Some((state_key, version)) = &journal.seen {
        let mut state = db.state()?;
        let seen = state.seen.entry(state_key.clone()).or_default();
        *seen = (*seen).max(*version);
        db.put_state(&state)?;
    }
    // Remove journal BEFORE discarding staging so a crash between the two cannot re-run apply().
    if let Some((db_dir_fd, leaf)) = open_parent_existing(root, "var/db/kl/journal.json")? {
        match unlinkat(&db_dir_fd, leaf, AtFlags::empty()) {
            Ok(()) | Err(Errno::NOENT) => {}
            Err(e) => return Err(e).context("remove journal.json"),
        }
        let dir_file = File::from(db_dir_fd);
        let _ = dir_file.sync_all();
    }
    discard(&db.staging())?;
    Ok(())
}

fn apply_rename(root: &Path, from: &str, to: &str) -> Result<()> {
    let from_open = open_parent_existing(root, from)?;
    let from_present = match &from_open {
        Some((from_dir, from_leaf)) => {
            statat(from_dir, *from_leaf, AtFlags::SYMLINK_NOFOLLOW).is_ok()
        }
        None => false,
    };

    if !from_present {
        // During crash recovery, the rename may already have completed; verify destination exists!
        let to_present = match open_parent_existing(root, to)? {
            Some((to_dir, to_leaf)) => statat(&to_dir, to_leaf, AtFlags::SYMLINK_NOFOLLOW).is_ok(),
            None => false,
        };
        ensure!(
            to_present,
            "staged file {from} is missing and destination {to} does not exist"
        );
        return Ok(());
    }

    let Some((from_dir, from_leaf)) = from_open else {
        unreachable!();
    };
    let (to_dir, to_leaf) = open_parent_create(root, to)?;
    match renameat(&from_dir, from_leaf, &to_dir, to_leaf) {
        Ok(()) => Ok(()),
        Err(Errno::XDEV) => move_cross_device(&from_dir, from_leaf, &to_dir, to_leaf, to),
        Err(e) => Err(e).with_context(|| format!("install {to}")),
    }
}

fn move_cross_device(
    from_dir: &OwnedFd,
    from_leaf: &str,
    to_dir: &OwnedFd,
    to_leaf: &str,
    to_label: &str,
) -> Result<()> {
    let st = statat(from_dir, from_leaf, AtFlags::SYMLINK_NOFOLLOW)
        .with_context(|| format!("stat staged {from_leaf}"))?;
    let seq = XDEV_COUNTER.fetch_add(1, Ordering::Relaxed);
    let tmp_name = format!(".kl-xdev.{}.{seq}", std::process::id());
    let ftype = st.st_mode & 0o170_000;
    let res = (|| -> Result<()> {
        if ftype == 0o120_000 {
            // Symlink
            let target = readlinkat(from_dir, from_leaf, Vec::new())?;
            let target_str = target
                .to_str()
                .map_err(|_| anyhow!("non-UTF-8 symlink target"))?;
            symlinkat(target_str, to_dir, tmp_name.as_str())?;
        } else {
            let src_fd = openat(
                from_dir,
                from_leaf,
                OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
                Mode::empty(),
            )?;
            let mut src = File::from(src_fd);
            let mode = sanitize_file_mode(u32::from(st.st_mode));
            let dst_fd = openat(
                to_dir,
                tmp_name.as_str(),
                OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::from_raw_mode(mode_t(mode)),
            )?;
            let mut dst = File::from(dst_fd);
            dst.set_permissions(Permissions::from_mode(mode))?;
            io::copy(&mut src, &mut dst)?;
            dst.sync_all()?;
        }
        renameat(to_dir, tmp_name.as_str(), to_dir, to_leaf)
            .with_context(|| format!("install {to_label}"))?;
        let _ = unlinkat(from_dir, from_leaf, AtFlags::empty());
        Ok(())
    })();
    if res.is_err() {
        let _ = unlinkat(to_dir, tmp_name.as_str(), AtFlags::empty());
    }
    res
}

pub fn recover(db: &Db) -> Result<bool> {
    let Some(bytes) = read_file_nofollow(&db.journal(), MAX_JOURNAL_SIZE)? else {
        discard(&db.staging())?;
        return Ok(false);
    };
    let journal: Journal = serde_json::from_slice(&bytes).context("read the journal")?;
    apply(db, &journal)?;
    Ok(true)
}

#[must_use]
pub fn verify(root: &Path, installed: &Installed) -> Vec<String> {
    let mut problems = Vec::new();
    for f in &installed.manifest.files {
        let present = open_parent_existing(root, &f.path)
            .ok()
            .flatten()
            .is_some_and(|(dir, leaf)| statat(&dir, leaf, AtFlags::SYMLINK_NOFOLLOW).is_ok());
        let ok = no_symlink_ancestors(root, &f.path).is_ok() && unchanged(root, f);
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
    let Ok(Some((parent_fd, leaf))) = open_parent_existing(root, &f.path) else {
        return false;
    };
    match f.kind {
        Kind::Dir => {
            let Ok(st) = statat(&parent_fd, leaf, AtFlags::SYMLINK_NOFOLLOW) else {
                return false;
            };
            let is_dir = (st.st_mode & 0o170_000) == 0o040_000;
            let not_world_writable = (st.st_mode & 0o002) == 0;
            is_dir && not_world_writable
        }
        Kind::Symlink => {
            readlinkat(&parent_fd, leaf, Vec::new())
                .ok()
                .and_then(|t| t.to_str().ok().map(str::to_owned))
                .as_deref()
                == f.target.as_deref()
        }
        Kind::File => {
            let Ok(fd) = openat(
                &parent_fd,
                leaf,
                OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
                Mode::empty(),
            ) else {
                return false;
            };
            let mut file = File::from(fd);
            let Ok(md) = file.metadata() else {
                return false;
            };
            if !md.is_file() {
                return false;
            }
            if Some(md.len()) != f.size {
                return false;
            }
            let actual_mode = md.permissions().mode() & 0o7777;
            if actual_mode != sanitize_file_mode(f.mode) {
                return false;
            }
            hash_open_file(&mut file).is_ok_and(|s| Some(s) == f.sha256)
        }
    }
}

fn hash_open_file(file: &mut File) -> io::Result<String> {
    let mut hasher = Sha256::new();
    io::copy(file, &mut hasher)?;
    Ok(hex::encode(hasher.finalize()))
}

fn open_dir_chain_create(root: &Path, rel_dir: &str) -> Result<OwnedFd> {
    let mut cur = openat(
        CWD,
        root,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .with_context(|| format!("open root {}", root.display()))?;
    for comp in rel_dir.split('/').filter(|c| !c.is_empty()) {
        ensure!(
            comp != "." && comp != "..",
            "bad directory path {rel_dir:?}"
        );
        let created = match mkdirat(&cur, comp, Mode::from_raw_mode(0o755)) {
            Ok(()) => true,
            Err(Errno::EXIST) => false,
            Err(e) => {
                return Err(e).with_context(|| format!("create directory {comp} in {rel_dir}"));
            }
        };
        if created {
            // Explicitly enforce 0o755 regardless of process umask (guards against umask 0000).
            let _ = chmodat(&cur, comp, Mode::from_raw_mode(0o755), AtFlags::empty());
        }
        cur = match openat(
            &cur,
            comp,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        ) {
            Ok(fd) => fd,
            Err(Errno::LOOP) => {
                bail!("{comp} in {rel_dir} is a symlink; kl does not install through one");
            }
            Err(Errno::NOTDIR) => bail!("{comp} in {rel_dir} is not a directory"),
            Err(e) => return Err(e).with_context(|| format!("open {comp} in {rel_dir}")),
        };
    }
    Ok(cur)
}

fn open_parent_create<'a>(root: &Path, rel: &'a str) -> Result<(OwnedFd, &'a str)> {
    let (parent, leaf) = match rel.rsplit_once('/') {
        Some((p, l)) => (p, l),
        None => ("", rel),
    };
    ensure!(
        !leaf.is_empty() && leaf != "." && leaf != "..",
        "bad path {rel:?}"
    );
    let dir_fd = open_dir_chain_create(root, parent)?;
    Ok((dir_fd, leaf))
}

fn open_parent_existing<'a>(root: &Path, rel: &'a str) -> Result<Option<(OwnedFd, &'a str)>> {
    let (parent, leaf) = match rel.rsplit_once('/') {
        Some((p, l)) => (p, l),
        None => ("", rel),
    };
    ensure!(
        !leaf.is_empty() && leaf != "." && leaf != "..",
        "bad path {rel:?}"
    );
    let mut cur = match openat(
        CWD,
        root,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
        Mode::empty(),
    ) {
        Ok(fd) => fd,
        Err(Errno::NOENT) => return Ok(None),
        Err(e) => return Err(e).with_context(|| format!("open root {}", root.display())),
    };
    for comp in parent.split('/').filter(|c| !c.is_empty()) {
        ensure!(comp != "." && comp != "..", "bad path {rel:?}");
        cur = match openat(
            &cur,
            comp,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        ) {
            Ok(fd) => fd,
            Err(Errno::NOENT) => return Ok(None),
            Err(Errno::LOOP) => {
                bail!("{comp} in {rel} is a symlink; kl does not traverse through one");
            }
            Err(Errno::NOTDIR) => bail!("{comp} in {rel} is not a directory"),
            Err(e) => return Err(e).with_context(|| format!("open {comp} in {rel}")),
        };
    }
    Ok(Some((cur, leaf)))
}

fn no_symlink_ancestors(root: &Path, rel: &str) -> Result<()> {
    let Some(parent) = Path::new(rel).parent() else {
        return Ok(());
    };
    let mut cur: PathBuf = root.to_path_buf();
    for c in parent.components() {
        cur.push(c);
        match fs::symlink_metadata(&cur) {
            Ok(md) if md.is_dir() => {}
            Ok(md) if md.file_type().is_symlink() => {
                bail!(
                    "{} is a symlink; kl does not traverse through one",
                    cur.display()
                )
            }
            Ok(_) => bail!("{} is not a directory", cur.display()),
            Err(e) if e.kind() == ErrorKind::NotFound => return Ok(()),
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_zstd_huge_window_size_before_decoding() {
        // Craft a minimal zstd frame header with single_segment=0, Window_Descriptor=0xFF (exponent=31 => > 128 MiB).
        let frame = [0x28u8, 0xb5, 0x2f, 0xfd, 0x00, 0xff];
        let mut cursor = Cursor::new(&frame);
        let err = inspect_zstd_header(&mut cursor, 1 << 20).unwrap_err();
        assert!(err.to_string().contains("window size"), "{err}");

        // Craft a single-segment zstd frame declaring 8-byte frame_content_size = u64::MAX.
        let mut huge = vec![0x28u8, 0xb5, 0x2f, 0xfd, 0xe0]; // fcs_flag=3, single_segment=1
        huge.extend_from_slice(&u64::MAX.to_le_bytes());
        let mut cursor2 = Cursor::new(&huge);
        let err2 = inspect_zstd_header(&mut cursor2, 1 << 20).unwrap_err();
        assert!(err2.to_string().contains("exceeds allowed size"), "{err2}");
    }
}
