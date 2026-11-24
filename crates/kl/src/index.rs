use crate::manifest::check_name;
use anyhow::{Result, ensure};
use kido_trust::{BlobRef, valid_sha256};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;

#[derive(Serialize, Deserialize, Clone, Debug, PartialEq, Eq)]
pub struct Package {
    pub name: String,
    pub version: String,
    pub summary: String,
    pub license: String,
    pub blob: BlobRef,
    pub installed_size: u64,
    #[serde(default)]
    pub provides: Vec<String>,
    #[serde(default)]
    pub depends: Vec<String>,
}

#[derive(Serialize, Deserialize, Clone, Debug)]
pub struct Index {
    pub version: u64,
    pub arch: String,
    pub packages: Vec<Package>,
}

impl Index {
    pub fn parse(bytes: &[u8], arch: &str) -> Result<Self> {
        let index: Index = serde_json::from_slice(bytes)?;
        ensure!(
            index.arch == arch,
            "the index is for {}, not {arch}",
            index.arch
        );
        let mut names = BTreeSet::new();
        for p in &index.packages {
            check_name(&p.name)?;
            ensure!(valid_sha256(&p.blob.sha256), "{} has a bad sha256", p.name);
            ensure!(names.insert(p.name.as_str()), "{} is listed twice", p.name);
        }
        Ok(index)
    }

    #[must_use]
    pub fn find(&self, name: &str) -> Option<&Package> {
        self.packages.iter().find(|p| p.name == name)
    }

    #[must_use]
    pub fn provider(&self, what: &str) -> Option<&Package> {
        self.packages
            .iter()
            .find(|p| p.provides.iter().any(|x| x == what))
    }
}
