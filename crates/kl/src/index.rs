use crate::manifest::{ENTRY_LIMIT, check_meta_text, check_name, check_path, fold_path};
use anyhow::{Result, ensure};
use kido_trust::{BlobRef, RepoTarget, valid_sha256};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;

const META_FIELD_LIMIT: usize = 1024;
const META_LIST_LIMIT: usize = 10_000;

#[derive(Serialize, Deserialize, Clone, Debug, PartialEq, Eq)]
pub struct Package {
    pub name: String,
    pub version: String,
    pub summary: String,
    pub license: String,
    pub blob: BlobRef,
    pub installed_size: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub chunks: Option<BlobRef>,
    #[serde(default)]
    pub provides: Vec<String>,
    #[serde(default)]
    pub depends: Vec<String>,
    #[serde(default)]
    pub files: Vec<String>,
}

#[derive(Serialize, Deserialize, Clone, Debug)]
pub struct Index {
    pub version: u64,
    pub target: RepoTarget,
    pub arch: String,
    pub packages: Vec<Package>,
}

impl Index {
    pub fn parse(bytes: &[u8], want_target: &RepoTarget, arch: &str) -> Result<Self> {
        crate::check_arch(arch)?;
        let index: Index = serde_json::from_slice(bytes)?;
        index.target.validate()?;
        ensure!(index.version >= 1, "index version must be at least 1");
        ensure!(
            index.target == *want_target,
            "the index is for {}, not {want_target}",
            index.target
        );
        ensure!(
            index.arch == arch,
            "the index is for {}, not {arch}",
            index.arch
        );
        let mut names_ci = BTreeSet::new();
        for p in &index.packages {
            check_name(&p.name)?;
            ensure!(!p.version.is_empty(), "{} has empty version", p.name);
            check_meta_text("version", &p.version, 128)?;
            check_meta_text("license", &p.license, 256)?;
            check_meta_text("summary", &p.summary, META_FIELD_LIMIT)?;
            ensure!(
                p.provides.len() <= META_LIST_LIMIT && p.depends.len() <= META_LIST_LIMIT,
                "{} lists too many provides or depends entries",
                p.name
            );
            for prov in &p.provides {
                check_meta_text("provides", prov, META_FIELD_LIMIT)?;
            }
            for dep in &p.depends {
                check_meta_text("depends", dep, META_FIELD_LIMIT)?;
            }
            ensure!(valid_sha256(&p.blob.sha256), "{} has a bad sha256", p.name);
            ensure!(
                p.chunks.as_ref().is_none_or(|c| valid_sha256(&c.sha256)),
                "{} has a bad chunk list sha256",
                p.name
            );
            ensure!(
                p.files.len() <= ENTRY_LIMIT,
                "{} lists too many files in the index",
                p.name
            );
            let mut file_set_ci = BTreeSet::new();
            for f in &p.files {
                check_path(f)?;
                ensure!(
                    file_set_ci.insert(fold_path(f)),
                    "{} lists {f} twice in the index",
                    p.name
                );
            }
            ensure!(
                names_ci.insert(fold_path(&p.name)),
                "{} is listed twice (or collides case-insensitively)",
                p.name
            );
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

    #[must_use]
    pub fn owner_of(&self, rel: &str) -> Option<&Package> {
        self.packages
            .iter()
            .find(|p| p.files.iter().any(|f| f == rel))
    }
}
