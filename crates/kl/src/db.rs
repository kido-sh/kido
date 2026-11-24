use crate::manifest::Manifest;
use anyhow::{Context, Result};
use kido_trust::BlobRef;
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File};
use std::io::{ErrorKind, Write};
use std::path::{Path, PathBuf};

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
}

impl Db {
    pub fn open(root: &Path) -> Result<Self> {
        let dir = root.join("var/db/kl");
        fs::create_dir_all(dir.join("installed"))
            .with_context(|| format!("create {}", dir.display()))?;
        Ok(Db {
            root: root.to_path_buf(),
            dir,
        })
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
        match fs::read_to_string(self.dir.join("world")) {
            Ok(s) => Ok(s
                .lines()
                .map(str::trim)
                .filter(|l| !l.is_empty())
                .map(str::to_owned)
                .collect()),
            Err(e) if e.kind() == ErrorKind::NotFound => Ok(BTreeSet::new()),
            Err(e) => Err(e.into()),
        }
    }

    pub fn set_world(&self, world: &BTreeSet<String>) -> Result<()> {
        let mut text = String::new();
        for name in world {
            text.push_str(name);
            text.push('\n');
        }
        write_atomic(&self.dir.join("world"), text.as_bytes())
    }

    pub fn installed(&self) -> Result<BTreeMap<String, Installed>> {
        let mut out = BTreeMap::new();
        for entry in fs::read_dir(self.dir.join("installed"))? {
            let path = entry?.path();
            if path.extension().is_some_and(|e| e == "json") {
                let i: Installed = serde_json::from_slice(&fs::read(&path)?)
                    .with_context(|| format!("read {}", path.display()))?;
                out.insert(i.manifest.name.clone(), i);
            }
        }
        Ok(out)
    }

    pub fn put(&self, installed: &Installed) -> Result<()> {
        let path = self
            .dir
            .join("installed")
            .join(format!("{}.json", installed.manifest.name));
        write_atomic(&path, &serde_json::to_vec_pretty(installed)?)
    }

    pub fn delete(&self, name: &str) -> Result<()> {
        match fs::remove_file(self.dir.join("installed").join(format!("{name}.json"))) {
            Err(e) if e.kind() != ErrorKind::NotFound => Err(e.into()),
            _ => Ok(()),
        }
    }

    pub fn state(&self) -> Result<State> {
        match fs::read(self.dir.join("state.json")) {
            Ok(b) => Ok(serde_json::from_slice(&b)?),
            Err(e) if e.kind() == ErrorKind::NotFound => Ok(State::default()),
            Err(e) => Err(e.into()),
        }
    }

    pub fn put_state(&self, state: &State) -> Result<()> {
        write_atomic(
            &self.dir.join("state.json"),
            &serde_json::to_vec_pretty(state)?,
        )
    }
}

pub fn write_atomic(path: &Path, bytes: &[u8]) -> Result<()> {
    let tmp = path.with_extension("tmp");
    let mut f = File::create(&tmp).with_context(|| format!("create {}", tmp.display()))?;
    f.write_all(bytes)?;
    f.sync_all()?;
    fs::rename(&tmp, path).with_context(|| format!("rename to {}", path.display()))?;
    Ok(())
}
