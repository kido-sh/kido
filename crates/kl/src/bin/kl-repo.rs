use anyhow::{Context, Result, anyhow, bail, ensure};
use clap::{Parser, Subcommand};
use goblin::mach::{Mach, MachO, SingleArch};
use kido_trust::{
    BlobRef, DEFAULT_CHANNEL, DEFAULT_KL_VERSION, HashingReader, OsVersion, RepoTarget, Timestamp,
    valid_sha256,
};
use kl::db::write_atomic;
use kl::index::{Index, Package};
use kl::manifest::{
    FileEntry, Kind, MANIFEST_PATH, Manifest, check_name, check_path, resolve_link,
};
use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File};
use std::io::ErrorKind;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::Command;
use time::format_description::well_known::Rfc3339;
use time::{Duration, OffsetDateTime};

#[derive(Parser)]
#[command(name = "kl-repo", version, about = "Build and sign a kl repository")]
struct Cli {
    #[arg(long, global = true)]
    repo: Option<PathBuf>,
    #[arg(long, global = true, default_value = DEFAULT_KL_VERSION)]
    kl_version: String,
    #[arg(long, global = true)]
    os_version: Option<String>,
    #[arg(long, global = true)]
    os_build: Option<String>,
    #[arg(long, global = true, default_value = DEFAULT_CHANNEL)]
    channel: String,
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    Pack {
        #[arg(long)]
        arch: String,
        #[arg(long)]
        from: PathBuf,
        #[arg(long)]
        name: String,
        #[arg(long)]
        version: String,
        #[arg(long)]
        license: String,
        #[arg(long, default_value = "")]
        summary: String,
        #[arg(long = "depend")]
        depends: Vec<String>,
        #[arg(required = true)]
        paths: Vec<String>,
    },
    Index {
        #[arg(long)]
        arch: String,
        #[arg(long)]
        version: u64,
    },
    Timestamp {
        #[arg(long)]
        version: u64,
        #[arg(long, default_value_t = 7)]
        days: i64,
        #[arg(long)]
        expires: Option<String>,
    },
    Sign {
        #[arg(long)]
        key: PathBuf,
        #[arg(long)]
        arch: Option<String>,
    },
}

struct PackArgs<'a> {
    arch: &'a str,
    from: &'a Path,
    name: &'a str,
    version: &'a str,
    license: &'a str,
    summary: &'a str,
    depends: &'a [String],
    paths: &'a [String],
}

fn main() -> Result<()> {
    let cli = Cli::parse();
    let repo = cli
        .repo
        .as_deref()
        .ok_or_else(|| anyhow!("--repo is required"))?;
    let os_version = cli
        .os_version
        .as_deref()
        .ok_or_else(|| anyhow!("--os-version is required"))?;
    let os_build = cli
        .os_build
        .as_deref()
        .ok_or_else(|| anyhow!("--os-build is required"))?;
    let target = RepoTarget::new(
        &cli.kl_version,
        OsVersion::parse(os_version)?,
        os_build,
        &cli.channel,
    )?;
    match &cli.cmd {
        Cmd::Pack {
            arch,
            from,
            name,
            version,
            license,
            summary,
            depends,
            paths,
        } => pack(
            repo,
            &target,
            &PackArgs {
                arch,
                from,
                name,
                version,
                license,
                summary,
                depends,
                paths,
            },
        ),
        Cmd::Index { arch, version } => index(repo, &target, arch, *version),
        Cmd::Timestamp {
            version,
            days,
            expires,
        } => timestamp(repo, &target, *version, *days, expires.as_deref()),
        Cmd::Sign { key, arch } => sign(repo, &target, key, arch.as_deref()),
    }
}

fn write_blob_dedup(blobs_dir: &Path, blob_ref: &BlobRef, bytes: &[u8]) -> Result<()> {
    ensure!(
        valid_sha256(&blob_ref.sha256),
        "bad blob sha256 {}",
        blob_ref.sha256
    );
    let path = blobs_dir.join(&blob_ref.sha256);
    match fs::symlink_metadata(&path) {
        Ok(md) => {
            ensure!(
                md.is_file(),
                "existing blob path {} is not a regular file",
                path.display()
            );
            verify_blob_on_disk(&path, blob_ref)
                .with_context(|| format!("existing blob {} is corrupt", path.display()))?;
            return Ok(());
        }
        Err(e) if e.kind() == ErrorKind::NotFound => {}
        Err(e) => return Err(e.into()),
    }
    write_atomic(&path, bytes)
}

fn verify_blob_on_disk(path: &Path, blob_ref: &BlobRef) -> Result<()> {
    ensure!(
        valid_sha256(&blob_ref.sha256),
        "bad blob sha256 {}",
        blob_ref.sha256
    );
    let md = fs::symlink_metadata(path).with_context(|| format!("stat {}", path.display()))?;
    ensure!(md.is_file(), "{} is not a regular file", path.display());
    ensure!(
        md.len() == blob_ref.size,
        "{} size {} does not match expected {}",
        path.display(),
        md.len(),
        blob_ref.size
    );
    let file = File::open(path).with_context(|| format!("open {}", path.display()))?;
    HashingReader::new(file, blob_ref.size)
        .finish(blob_ref)
        .with_context(|| format!("verify {}", path.display()))?;
    Ok(())
}

fn pack(repo: &Path, target: &RepoTarget, a: &PackArgs<'_>) -> Result<()> {
    kl::check_arch(a.arch)?;
    check_name(a.name)?;
    let mut files = BTreeMap::new();
    for p in a.paths {
        collect(a.from, p.trim_matches('/'), &mut files)?;
    }
    let mut provides = BTreeSet::new();
    let mut needs = BTreeSet::new();
    for f in files.values().filter(|f| f.kind == Kind::File) {
        scan(&a.from.join(&f.path), &mut provides, &mut needs)?;
    }
    let provides: Vec<String> = provides.into_iter().map(|p| format!("dylib:{p}")).collect();
    let mut depends: BTreeSet<String> = needs
        .into_iter()
        .map(|p| format!("dylib:{p}"))
        .filter(|d| !provides.contains(d))
        .collect();
    depends.extend(a.depends.iter().cloned());
    let manifest = Manifest {
        name: a.name.to_owned(),
        version: a.version.to_owned(),
        arch: a.arch.to_owned(),
        license: a.license.to_owned(),
        summary: a.summary.to_owned(),
        provides,
        depends: depends.into_iter().collect(),
        files: files.into_values().collect(),
    };
    manifest.validate(a.name, a.version, a.arch)?;
    let blob = archive(a.from, &manifest)?;
    let blob_ref = BlobRef::of(&blob);
    let blobs = repo.join(target.blobs_prefix()?);
    fs::create_dir_all(&blobs)?;
    write_blob_dedup(&blobs, &blob_ref, &blob)?;
    let chunks = if blob.len() as u64 > kido_trust::CHUNK_SIZE {
        let list = serde_json::to_vec(&kido_trust::ChunkList::of(&blob, kido_trust::CHUNK_SIZE))?;
        let list_ref = BlobRef::of(&list);
        write_blob_dedup(&blobs, &list_ref, &list)?;
        Some(list_ref)
    } else {
        None
    };
    let file_list: Vec<String> = manifest
        .files
        .iter()
        .filter(|f| f.kind != Kind::Dir)
        .map(|f| f.path.clone())
        .collect();
    let package = Package {
        name: manifest.name.clone(),
        version: manifest.version.clone(),
        summary: manifest.summary.clone(),
        license: manifest.license.clone(),
        installed_size: manifest.file_bytes()?,
        chunks,
        provides: manifest.provides.clone(),
        depends: manifest.depends.clone(),
        files: file_list,
        blob: blob_ref,
    };
    let dir = repo
        .join(target.metadata_prefix()?)
        .join(a.arch)
        .join("packages.d");
    fs::create_dir_all(&dir)?;
    write_atomic(
        &dir.join(format!("{}.json", a.name)),
        &serde_json::to_vec_pretty(&package)?,
    )?;
    println!(
        "{} {}: {} bytes, sha256 {}",
        package.name, package.version, package.blob.size, package.blob.sha256
    );
    Ok(())
}

fn no_symlink_ancestors(root: &Path, rel: &str) -> Result<()> {
    let Some(parent) = Path::new(rel).parent() else {
        return Ok(());
    };
    let mut cur = root.to_path_buf();
    for c in parent.components() {
        cur.push(c);
        let md = fs::symlink_metadata(&cur)
            .with_context(|| format!("stat ancestor {}", cur.display()))?;
        ensure!(
            md.is_dir(),
            "ancestor {} is not a real directory (symlinks are not allowed)",
            cur.display()
        );
    }
    Ok(())
}

fn collect(from: &Path, rel: &str, out: &mut BTreeMap<String, FileEntry>) -> Result<()> {
    check_path(rel)?;
    no_symlink_ancestors(from, rel)?;
    let path = from.join(rel);
    let md = fs::symlink_metadata(&path).with_context(|| format!("read {}", path.display()))?;
    let mode = md.permissions().mode() & 0o775;
    let entry = |kind, size, sha256, target| FileEntry {
        path: rel.to_owned(),
        kind,
        mode,
        size,
        sha256,
        target,
    };
    if md.file_type().is_symlink() {
        let target = fs::read_link(&path)?
            .to_str()
            .ok_or_else(|| anyhow!("{rel}: target is not UTF-8"))?
            .to_owned();
        resolve_link(rel, &target)?;
        out.insert(
            rel.to_owned(),
            entry(Kind::Symlink, None, None, Some(target)),
        );
    } else if md.is_dir() {
        out.insert(rel.to_owned(), entry(Kind::Dir, None, None, None));
        let mut names = fs::read_dir(&path)?
            .map(|e| e.map(|e| e.file_name()))
            .collect::<Result<Vec<_>, _>>()?;
        names.sort();
        for n in names {
            let n = n
                .to_str()
                .ok_or_else(|| anyhow!("{rel}: a name is not UTF-8"))?;
            collect(from, &format!("{rel}/{n}"), out)?;
        }
    } else if md.is_file() {
        let bytes = fs::read(&path)?;
        out.insert(
            rel.to_owned(),
            entry(
                Kind::File,
                Some(bytes.len() as u64),
                Some(kido_trust::sha256_hex(&bytes)),
                None,
            ),
        );
    } else {
        bail!("{rel} is not a file, a directory or a symlink");
    }
    Ok(())
}

fn scan(path: &Path, provides: &mut BTreeSet<String>, needs: &mut BTreeSet<String>) -> Result<()> {
    let bytes = fs::read(path)?;
    let Ok(mach) = Mach::parse(&bytes) else {
        return Ok(());
    };
    match mach {
        Mach::Binary(m) => record(&m, provides, needs),
        Mach::Fat(fat) => {
            for i in 0..fat.narches {
                if let Ok(SingleArch::MachO(m)) = fat.get(i) {
                    record(&m, provides, needs);
                }
            }
        }
    }
    Ok(())
}

fn record(m: &MachO<'_>, provides: &mut BTreeSet<String>, needs: &mut BTreeSet<String>) {
    if let Some(name) = m.name {
        provides.insert(name.to_owned());
    }
    for lib in m.libs.iter().filter(|l| **l != "self") {
        needs.insert((*lib).to_owned());
    }
}

fn archive(from: &Path, manifest: &Manifest) -> Result<Vec<u8>> {
    let mut encoder = zstd::stream::write::Encoder::new(Vec::new(), 22)?;
    {
        let mut tar = tar::Builder::new(&mut encoder);
        let json = serde_json::to_vec_pretty(manifest)?;
        tar.append_data(
            &mut header(tar::EntryType::Regular, 0o644, json.len() as u64),
            MANIFEST_PATH,
            json.as_slice(),
        )?;
        for f in &manifest.files {
            match f.kind {
                Kind::Dir => tar.append_data(
                    &mut header(tar::EntryType::Directory, f.mode, 0),
                    &f.path,
                    std::io::empty(),
                )?,
                Kind::File => {
                    let bytes = fs::read(from.join(&f.path))?;
                    ensure!(
                        Some(kido_trust::sha256_hex(&bytes)) == f.sha256,
                        "{} changed while it was being packed",
                        f.path
                    );
                    tar.append_data(
                        &mut header(tar::EntryType::Regular, f.mode, bytes.len() as u64),
                        &f.path,
                        bytes.as_slice(),
                    )?;
                }
                Kind::Symlink => tar.append_link(
                    &mut header(tar::EntryType::Symlink, f.mode, 0),
                    &f.path,
                    f.target.as_deref().unwrap_or_default(),
                )?,
            }
        }
        tar.finish()?;
    }
    Ok(encoder.finish()?)
}

fn header(kind: tar::EntryType, mode: u32, size: u64) -> tar::Header {
    let mut h = tar::Header::new_gnu();
    h.set_entry_type(kind);
    h.set_mode(mode);
    h.set_size(size);
    h.set_mtime(0);
    h.set_uid(0);
    h.set_gid(0);
    h
}

fn load_existing_index(repo: &Path, target: &RepoTarget, arch: &str) -> Result<Option<Index>> {
    let target_dir = repo.join(target.metadata_prefix()?);
    let arch_dir = target_dir.join(arch);
    let prev_ref: Option<BlobRef> = match fs::read(arch_dir.join("CURRENT")) {
        Ok(b) => Some(serde_json::from_slice(&b).context("read CURRENT")?),
        Err(e) if e.kind() == ErrorKind::NotFound => {
            match fs::read(target_dir.join("timestamp.json")) {
                Ok(b) => {
                    let ts: Timestamp =
                        serde_json::from_slice(&b).context("read existing timestamp.json")?;
                    ensure!(
                        ts.target == *target,
                        "existing timestamp.json is for {}, not {target}",
                        ts.target
                    );
                    ts.indexes.get(arch).cloned()
                }
                Err(e) if e.kind() == ErrorKind::NotFound => None,
                Err(e) => return Err(e.into()),
            }
        }
        Err(e) => return Err(e.into()),
    };

    let Some(blob_ref) = prev_ref else {
        return Ok(None);
    };
    ensure!(
        valid_sha256(&blob_ref.sha256),
        "existing index reference has bad sha256 {}",
        blob_ref.sha256
    );
    let index_path = arch_dir.join(format!("index.{}.json", blob_ref.sha256));
    let bytes =
        fs::read(&index_path).with_context(|| format!("read existing {}", index_path.display()))?;
    blob_ref
        .check(&bytes)
        .with_context(|| format!("verify existing {}", index_path.display()))?;
    let parsed = Index::parse(&bytes, target, arch)
        .with_context(|| format!("parse existing {}", index_path.display()))?;
    Ok(Some(parsed))
}

fn index(repo: &Path, target: &RepoTarget, arch: &str, version: u64) -> Result<()> {
    kl::check_arch(arch)?;
    ensure!(version >= 1, "index version must be at least 1");
    let dir = repo.join(target.metadata_prefix()?).join(arch);
    let mut by_name: BTreeMap<String, Package> = BTreeMap::new();

    if let Some(prev) = load_existing_index(repo, target, arch)? {
        for p in prev.packages {
            by_name.insert(p.name.clone(), p);
        }
    }

    let pkg_dir = dir.join("packages.d");
    match fs::read_dir(&pkg_dir) {
        Ok(entries) => {
            for entry in entries {
                let path = entry?.path();
                if path.extension().is_some_and(|e| e == "json") {
                    let pkg: Package = serde_json::from_slice(&fs::read(&path)?)
                        .with_context(|| format!("read {}", path.display()))?;
                    check_name(&pkg.name)?;
                    by_name.insert(pkg.name.clone(), pkg);
                }
            }
        }
        Err(e) if e.kind() == ErrorKind::NotFound => {}
        Err(e) => return Err(e.into()),
    }

    ensure!(!by_name.is_empty(), "no packages; run kl-repo pack first");

    let blobs_dir = repo.join(target.blobs_prefix()?);
    for pkg in by_name.values() {
        ensure!(
            valid_sha256(&pkg.blob.sha256),
            "{} has bad blob sha256 {}",
            pkg.name,
            pkg.blob.sha256
        );
        let blob_path = blobs_dir.join(&pkg.blob.sha256);
        verify_blob_on_disk(&blob_path, &pkg.blob)
            .with_context(|| format!("verify blob for {}", pkg.name))?;
        if let Some(chunks) = &pkg.chunks {
            ensure!(
                valid_sha256(&chunks.sha256),
                "{} has bad chunk list sha256 {}",
                pkg.name,
                chunks.sha256
            );
            let chunk_path = blobs_dir.join(&chunks.sha256);
            verify_blob_on_disk(&chunk_path, chunks)
                .with_context(|| format!("verify chunk list for {}", pkg.name))?;
        }
    }

    let packages: Vec<Package> = by_name.into_values().collect();
    let bytes = serde_json::to_vec_pretty(&Index {
        version,
        target: target.clone(),
        arch: arch.to_owned(),
        packages,
    })?;
    Index::parse(&bytes, target, arch)?;
    let blob = BlobRef::of(&bytes);
    fs::create_dir_all(&dir)?;
    write_atomic(&dir.join(format!("index.{}.json", blob.sha256)), &bytes)?;
    write_atomic(&dir.join("CURRENT"), &serde_json::to_vec(&blob)?)?;
    println!(
        "{target} {arch} index version {version}: sha256 {}",
        blob.sha256
    );
    Ok(())
}

fn timestamp(
    repo: &Path,
    target: &RepoTarget,
    version: u64,
    days: i64,
    expires: Option<&str>,
) -> Result<()> {
    ensure!(version >= 1, "timestamp version must be at least 1");
    ensure!(days >= 1, "--days must be at least 1");
    let dir = repo.join(target.metadata_prefix()?);
    let mut indexes = BTreeMap::new();
    for arch in kl::ARCHES {
        let arch_dir = dir.join(arch);
        match fs::read(arch_dir.join("CURRENT")) {
            Ok(b) => {
                let blob_ref: BlobRef = serde_json::from_slice(&b)?;
                ensure!(
                    valid_sha256(&blob_ref.sha256),
                    "CURRENT for {arch} has bad sha256"
                );
                let idx_path = arch_dir.join(format!("index.{}.json", blob_ref.sha256));
                let idx_bytes =
                    fs::read(&idx_path).with_context(|| format!("read {}", idx_path.display()))?;
                blob_ref.check(&idx_bytes)?;
                Index::parse(&idx_bytes, target, arch)?;
                indexes.insert((*arch).to_owned(), blob_ref);
            }
            Err(e) if e.kind() == ErrorKind::NotFound => {}
            Err(e) => return Err(e.into()),
        }
    }
    ensure!(!indexes.is_empty(), "no index; run kl-repo index first");
    let expires = match expires {
        Some(e) => {
            OffsetDateTime::parse(e, &Rfc3339).with_context(|| format!("bad --expires {e:?}"))?;
            e.to_owned()
        }
        None => (OffsetDateTime::now_utc() + Duration::days(days)).format(&Rfc3339)?,
    };
    let ts = Timestamp {
        version,
        expires,
        target: target.clone(),
        indexes,
    };
    write_atomic(
        &dir.join("timestamp.json"),
        &serde_json::to_vec_pretty(&ts)?,
    )?;
    println!(
        "{target} timestamp version {version}, expires {}",
        ts.expires
    );
    Ok(())
}

fn ssh_keygen_bin() -> &'static str {
    for candidate in ["/usr/bin/ssh-keygen", "/opt/homebrew/bin/ssh-keygen"] {
        if Path::new(candidate).is_file() {
            return candidate;
        }
    }
    "ssh-keygen"
}

fn sign(repo: &Path, target: &RepoTarget, key: &Path, arch: Option<&str>) -> Result<()> {
    if let Some(a) = arch {
        kl::check_arch(a)?;
    }
    let dir = repo.join(target.metadata_prefix()?);
    let stamp = dir.join("timestamp.json");
    ensure!(stamp.exists(), "no timestamp; run kl-repo timestamp first");
    let stamp_bytes = fs::read(&stamp)?;
    let ts: Timestamp = serde_json::from_slice(&stamp_bytes)
        .with_context(|| format!("parse {}", stamp.display()))?;
    ensure!(ts.version >= 1, "timestamp version must be at least 1");
    ensure!(
        ts.target == *target,
        "{} is for {}, not {target}",
        stamp.display(),
        ts.target
    );
    for (idx_arch, blob) in &ts.indexes {
        kl::check_arch(idx_arch)?;
        ensure!(
            valid_sha256(&blob.sha256),
            "timestamp has bad sha256 for {idx_arch}"
        );
    }
    if let Some(a) = arch {
        ensure!(
            ts.indexes.contains_key(a),
            "{} has no {a} index",
            stamp.display()
        );
    }

    // Sign a temporary copy and atomically replace timestamp.json.sig (no unsigned window).
    let tmp_stamp = dir.join(format!(".timestamp.sign.{}.json", std::process::id()));
    let tmp_sig = dir.join(format!(".timestamp.sign.{}.json.sig", std::process::id()));
    let _ = fs::remove_file(&tmp_stamp);
    let _ = fs::remove_file(&tmp_sig);
    let res = (|| -> Result<()> {
        fs::write(&tmp_stamp, &stamp_bytes)?;
        let status = Command::new(ssh_keygen_bin())
            .args(["-q", "-Y", "sign", "-n", kl::NAMESPACE, "-f"])
            .arg(key)
            .arg("--")
            .arg(&tmp_stamp)
            .status()
            .context("run ssh-keygen")?;
        ensure!(
            status.success(),
            "ssh-keygen could not sign {}",
            stamp.display()
        );
        let sig_bytes = fs::read(&tmp_sig).context("read generated signature")?;
        write_atomic(&dir.join("timestamp.json.sig"), &sig_bytes)?;
        Ok(())
    })();
    let _ = fs::remove_file(&tmp_stamp);
    let _ = fs::remove_file(&tmp_sig);
    res
}
