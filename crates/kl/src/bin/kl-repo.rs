use anyhow::{Context, Result, anyhow, bail, ensure};
use clap::{Parser, Subcommand};
use goblin::mach::{Mach, MachO, SingleArch};
use kido_trust::{BlobRef, Timestamp};
use kl::index::{Index, Package};
use kl::manifest::{
    FileEntry, Kind, MANIFEST_PATH, Manifest, check_name, check_path, resolve_link,
};
use std::collections::{BTreeMap, BTreeSet};
use std::fs;
use std::io::ErrorKind;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::Command;
use time::format_description::well_known::Rfc3339;
use time::{Duration, OffsetDateTime};

#[derive(Parser)]
#[command(name = "kl-repo", version, about = "Build and sign a kl repository")]
struct Cli {
    #[arg(long)]
    repo: PathBuf,
    #[arg(long, default_value = "latest")]
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
        #[arg(long, default_value_t = 30)]
        days: i64,
        #[arg(long)]
        expires: Option<String>,
    },
    Sign {
        #[arg(long)]
        key: PathBuf,
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
    kl::check_channel(&cli.channel)?;
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
            &cli,
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
        Cmd::Index { arch, version } => index(&cli, arch, *version),
        Cmd::Timestamp {
            version,
            days,
            expires,
        } => timestamp(&cli, *version, *days, expires.as_deref()),
        Cmd::Sign { key } => sign(&cli, key),
    }
}

fn pack(cli: &Cli, a: &PackArgs<'_>) -> Result<()> {
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
    let blobs = cli.repo.join("blobs/sha256");
    fs::create_dir_all(&blobs)?;
    fs::write(blobs.join(&blob_ref.sha256), &blob)?;
    let chunks = if blob.len() as u64 > kido_trust::CHUNK_SIZE {
        let list = serde_json::to_vec(&kido_trust::ChunkList::of(&blob, kido_trust::CHUNK_SIZE))?;
        let list_ref = BlobRef::of(&list);
        fs::write(blobs.join(&list_ref.sha256), &list)?;
        Some(list_ref)
    } else {
        None
    };
    let package = Package {
        name: manifest.name.clone(),
        version: manifest.version.clone(),
        summary: manifest.summary.clone(),
        license: manifest.license.clone(),
        installed_size: manifest.file_bytes(),
        chunks,
        provides: manifest.provides.clone(),
        depends: manifest.depends.clone(),
        blob: blob_ref,
    };
    let dir = cli.repo.join(&cli.channel).join(a.arch).join("packages.d");
    fs::create_dir_all(&dir)?;
    fs::write(
        dir.join(format!("{}.json", a.name)),
        serde_json::to_vec_pretty(&package)?,
    )?;
    println!(
        "{} {}: {} bytes, sha256 {}",
        package.name, package.version, package.blob.size, package.blob.sha256
    );
    Ok(())
}

fn collect(from: &Path, rel: &str, out: &mut BTreeMap<String, FileEntry>) -> Result<()> {
    check_path(rel)?;
    let path = from.join(rel);
    let md = fs::symlink_metadata(&path).with_context(|| format!("read {}", path.display()))?;
    let mode = md.permissions().mode() & 0o777;
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

fn index(cli: &Cli, arch: &str, version: u64) -> Result<()> {
    kl::check_arch(arch)?;
    let dir = cli.repo.join(&cli.channel).join(arch);
    let mut packages = Vec::new();
    for entry in
        fs::read_dir(dir.join("packages.d")).context("no packages; run kl-repo pack first")?
    {
        let path = entry?.path();
        if path.extension().is_some_and(|e| e == "json") {
            packages.push(serde_json::from_slice::<Package>(&fs::read(&path)?)?);
        }
    }
    packages.sort_by(|a, b| a.name.cmp(&b.name));
    let bytes = serde_json::to_vec_pretty(&Index {
        version,
        arch: arch.to_owned(),
        packages,
    })?;
    Index::parse(&bytes, arch)?;
    let blob = BlobRef::of(&bytes);
    fs::write(dir.join(format!("index.{}.json", blob.sha256)), &bytes)?;
    fs::write(dir.join("CURRENT"), serde_json::to_vec(&blob)?)?;
    println!(
        "{} {arch} index version {version}: sha256 {}",
        cli.channel, blob.sha256
    );
    Ok(())
}

fn timestamp(cli: &Cli, version: u64, days: i64, expires: Option<&str>) -> Result<()> {
    let mut indexes = BTreeMap::new();
    for arch in kl::ARCHES {
        match fs::read(cli.repo.join(&cli.channel).join(arch).join("CURRENT")) {
            Ok(b) => {
                indexes.insert((*arch).to_owned(), serde_json::from_slice::<BlobRef>(&b)?);
            }
            Err(e) if e.kind() == ErrorKind::NotFound => {}
            Err(e) => return Err(e.into()),
        }
    }
    ensure!(!indexes.is_empty(), "no index; run kl-repo index first");
    let expires = match expires {
        Some(e) => e.to_owned(),
        None => (OffsetDateTime::now_utc() + Duration::days(days)).format(&Rfc3339)?,
    };
    let ts = Timestamp {
        version,
        expires,
        channel: cli.channel.clone(),
        indexes,
    };
    let dir = cli.repo.join(&cli.channel);
    match fs::remove_file(dir.join("timestamp.json.sig")) {
        Err(e) if e.kind() != ErrorKind::NotFound => return Err(e.into()),
        _ => {}
    }
    fs::write(dir.join("timestamp.json"), serde_json::to_vec_pretty(&ts)?)?;
    println!(
        "{} timestamp version {version}, expires {}",
        cli.channel, ts.expires
    );
    Ok(())
}

fn sign(cli: &Cli, key: &Path) -> Result<()> {
    let stamp = cli.repo.join(&cli.channel).join("timestamp.json");
    ensure!(stamp.exists(), "no timestamp; run kl-repo timestamp first");
    match fs::remove_file(stamp.with_extension("json.sig")) {
        Err(e) if e.kind() != ErrorKind::NotFound => return Err(e.into()),
        _ => {}
    }
    let status = Command::new("ssh-keygen")
        .args(["-q", "-Y", "sign", "-n", kl::NAMESPACE, "-f"])
        .arg(key)
        .arg(&stamp)
        .status()
        .context("run ssh-keygen")?;
    ensure!(
        status.success(),
        "ssh-keygen could not sign {}",
        stamp.display()
    );
    Ok(())
}
