use crate::manifest::{Manifest, check_name, fold_path};
use anyhow::{Context, Result, bail, ensure};
use kido_trust::BlobRef;
use rustix::fs::{CWD, FlockOperation, Mode, OFlags, flock, mkdirat, openat, renameat, unlinkat};
use rustix::io::Errno;
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File};
use std::io::{ErrorKind, Read, Write};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

const DB_SMALL_LIMIT: u64 = 4 << 20;
const DB_MANIFEST_LIMIT: u64 = 16 << 20;
static TMP_COUNTER: AtomicU64 = AtomicU64::new(0);

#[derive(Serialize, Deserialize, Clone, Debug)]
pub struct Installed {
    pub manifest: Manifest,
    pub blob: BlobRef,
}

#[derive(Serialize, Deserialize, Default, Debug)]
pub struct State {
    pub seen: BTreeMap<String, u64>,
}

pub struct Db {
    pub root: PathBuf,
    dir: PathBuf,
    _lock: Option<File>,
}

impl Db {
    pub fn open(root: &Path) -> Result<Self> {
        Self::open_rw(root)
    }

    pub fn open_rw(root: &Path) -> Result<Self> {
        fs::create_dir_all(root).with_context(|| format!("create root {}", root.display()))?;
        let root_fd = openat(
            CWD,
            root,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
            Mode::empty(),
        )
        .with_context(|| format!("open root {}", root.display()))?;
        let db_fd = ensure_dir_chain(&root_fd, &["var", "db", "kl"])?;
        let _ = ensure_dir_chain(&db_fd, &["installed"])?;
        let lock_fd = openat(
            &db_fd,
            ".lock",
            OFlags::RDWR | OFlags::CREATE | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::from_raw_mode(0o600),
        )
        .with_context(|| format!("open {}/var/db/kl/.lock", root.display()))?;
        let lock_file = File::from(lock_fd);
        flock(&lock_file, FlockOperation::LockExclusive)
            .with_context(|| format!("lock {}/var/db/kl/.lock", root.display()))?;
        Ok(Db {
            root: root.to_path_buf(),
            dir: root.join("var/db/kl"),
            _lock: Some(lock_file),
        })
    }

    pub fn open_readonly(root: &Path) -> Result<Self> {
        let dir = root.join("var/db/kl");
        let lock = match openat(
            CWD,
            &dir,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
            Mode::empty(),
        ) {
            Ok(db_fd) => match openat(
                &db_fd,
                ".lock",
                OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::empty(),
            ) {
                Ok(fd) => {
                    let f = File::from(fd);
                    flock(&f, FlockOperation::LockShared)
                        .with_context(|| format!("lock {}", dir.join(".lock").display()))?;
                    Some(f)
                }
                Err(Errno::NOENT) => None,
                Err(e) => {
                    return Err(e).with_context(|| format!("open {}", dir.join(".lock").display()));
                }
            },
            Err(Errno::NOENT) => None,
            Err(e) => return Err(e).with_context(|| format!("open {}", dir.display())),
        };
        Ok(Db {
            root: root.to_path_buf(),
            dir,
            _lock: lock,
        })
    }

    #[must_use]
    pub fn has_journal(&self) -> bool {
        fs::symlink_metadata(self.journal()).is_ok()
    }

    #[must_use]
    pub fn staging(&self) -> PathBuf {
        self.dir.join("staging")
    }

    #[must_use]
    pub fn journal(&self) -> PathBuf {
        self.dir.join("journal.json")
    }

    pub fn world(&self) -> Result<BTreeSet<String>> {
        let Some(bytes) = read_file_nofollow(&self.dir.join("world"), DB_SMALL_LIMIT)? else {
            return Ok(BTreeSet::new());
        };
        let text = String::from_utf8(bytes).context("world file is not valid UTF-8")?;
        let mut out = BTreeSet::new();
        for name in text.lines().map(str::trim).filter(|l| !l.is_empty()) {
            check_name(name)?;
            out.insert(name.to_owned());
        }
        Ok(out)
    }

    pub fn set_world(&self, world: &BTreeSet<String>) -> Result<()> {
        let mut text = String::new();
        for name in world {
            check_name(name)?;
            text.push_str(name);
            text.push('\n');
        }
        write_atomic(&self.dir.join("world"), text.as_bytes())
    }

    pub fn installed(&self) -> Result<BTreeMap<String, Installed>> {
        let installed_dir = self.dir.join("installed");
        let entries = match fs::read_dir(&installed_dir) {
            Ok(e) => e,
            Err(e) if e.kind() == ErrorKind::NotFound => return Ok(BTreeMap::new()),
            Err(e) => return Err(e.into()),
        };
        let mut out = BTreeMap::new();
        let mut seen_ci = BTreeSet::new();
        for entry in entries {
            let path = entry?.path();
            if path.extension().is_some_and(|e| e == "json") {
                let Some(bytes) = read_file_nofollow(&path, DB_MANIFEST_LIMIT)? else {
                    continue;
                };
                let i: Installed = serde_json::from_slice(&bytes)
                    .with_context(|| format!("read {}", path.display()))?;
                check_name(&i.manifest.name)?;
                ensure!(
                    path.file_stem().and_then(|s| s.to_str()) == Some(i.manifest.name.as_str()),
                    "{} does not match package name {}",
                    path.display(),
                    i.manifest.name
                );
                ensure!(
                    seen_ci.insert(fold_path(&i.manifest.name)),
                    "duplicate case-colliding package entry {}",
                    i.manifest.name
                );
                out.insert(i.manifest.name.clone(), i);
            }
        }
        Ok(out)
    }

    pub fn put(&self, installed: &Installed) -> Result<()> {
        check_name(&installed.manifest.name)?;
        let path = self
            .dir
            .join("installed")
            .join(format!("{}.json", installed.manifest.name));
        write_atomic(&path, &serde_json::to_vec_pretty(installed)?)
    }

    pub fn delete(&self, name: &str) -> Result<()> {
        check_name(name)?;
        match fs::remove_file(self.dir.join("installed").join(format!("{name}.json"))) {
            Err(e) if e.kind() != ErrorKind::NotFound => Err(e.into()),
            _ => Ok(()),
        }
    }

    pub fn state(&self) -> Result<State> {
        let Some(bytes) = read_file_nofollow(&self.dir.join("state.json"), DB_SMALL_LIMIT)? else {
            return Ok(State::default());
        };
        Ok(serde_json::from_slice(&bytes)?)
    }

    pub fn put_state(&self, state: &State) -> Result<()> {
        write_atomic(
            &self.dir.join("state.json"),
            &serde_json::to_vec_pretty(state)?,
        )
    }
}

fn ensure_dir_chain(start: &rustix::fd::OwnedFd, parts: &[&str]) -> Result<rustix::fd::OwnedFd> {
    let mut cur = start.try_clone()?;
    for part in parts {
        match mkdirat(&cur, *part, Mode::from_raw_mode(0o755)) {
            Ok(()) | Err(Errno::EXIST) => {}
            Err(e) => return Err(e).with_context(|| format!("create directory {part}")),
        }
        cur = openat(
            &cur,
            *part,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        )
        .with_context(|| format!("open directory {part} (must not be a symlink)"))?;
    }
    Ok(cur)
}

pub fn read_file_nofollow(path: &Path, limit: u64) -> Result<Option<Vec<u8>>> {
    let fd = match openat(
        CWD,
        path,
        OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
        Mode::empty(),
    ) {
        Ok(fd) => fd,
        Err(Errno::NOENT) => return Ok(None),
        Err(Errno::LOOP) => bail!("{} must not be a symlink", path.display()),
        Err(e) => return Err(e).with_context(|| format!("open {}", path.display())),
    };
    let file = File::from(fd);
    let md = file
        .metadata()
        .with_context(|| format!("stat {}", path.display()))?;
    ensure!(md.is_file(), "{} is not a regular file", path.display());
    let mut buf = Vec::new();
    file.take(limit.saturating_add(1))
        .read_to_end(&mut buf)
        .with_context(|| format!("read {}", path.display()))?;
    ensure!(
        buf.len() as u64 <= limit,
        "{} is larger than {limit} bytes",
        path.display()
    );
    Ok(Some(buf))
}

pub fn write_atomic(path: &Path, bytes: &[u8]) -> Result<()> {
    let parent = path
        .parent()
        .with_context(|| format!("{} has no parent directory", path.display()))?;
    let name = path
        .file_name()
        .and_then(|n| n.to_str())
        .with_context(|| format!("bad file name {}", path.display()))?;
    let parent_fd = openat(
        CWD,
        parent,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
        Mode::empty(),
    )
    .with_context(|| format!("open {}", parent.display()))?;
    let seq = TMP_COUNTER.fetch_add(1, Ordering::Relaxed);
    let tmp_name = format!(".{name}.tmp.{}.{seq}", std::process::id());
    let write_res = (|| -> Result<()> {
        let fd = openat(
            &parent_fd,
            tmp_name.as_str(),
            OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::from_raw_mode(0o644),
        )
        .with_context(|| format!("create {}", parent.join(&tmp_name).display()))?;
        let mut f = File::from(fd);
        f.write_all(bytes)?;
        f.sync_all()?;
        drop(f);
        renameat(&parent_fd, tmp_name.as_str(), &parent_fd, name)
            .with_context(|| format!("rename to {}", path.display()))?;
        // Ensure rename is durable in parent directory.
        let dir_file = File::from(parent_fd.try_clone()?);
        let _ = dir_file.sync_all();
        Ok(())
    })();
    if write_res.is_err() {
        let _ = unlinkat(&parent_fd, tmp_name.as_str(), rustix::fs::AtFlags::empty());
    }
    write_res
}
