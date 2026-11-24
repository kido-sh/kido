use anyhow::{Result, anyhow, ensure};
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, BTreeSet};

pub const ALLOWED: &[&str] = &[
    "usr/lib/",
    "usr/libexec/",
    "usr/bin/",
    "usr/share/",
    "System/Library/Frameworks/",
    "System/Library/PrivateFrameworks/",
];

pub const MANIFEST_PATH: &str = ".kl/manifest.json";
pub const MANIFEST_LIMIT: u64 = 4 << 20;
pub const ENTRY_LIMIT: usize = 100_000;

#[derive(Serialize, Deserialize, Clone, Copy, Debug, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum Kind {
    File,
    Dir,
    Symlink,
}

#[derive(Serialize, Deserialize, Clone, Debug, PartialEq, Eq)]
pub struct FileEntry {
    pub path: String,
    pub kind: Kind,
    pub mode: u32,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub size: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sha256: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub target: Option<String>,
}

#[derive(Serialize, Deserialize, Clone, Debug)]
pub struct Manifest {
    pub name: String,
    pub version: String,
    pub arch: String,
    pub license: String,
    pub summary: String,
    #[serde(default)]
    pub provides: Vec<String>,
    #[serde(default)]
    pub depends: Vec<String>,
    pub files: Vec<FileEntry>,
}

pub fn check_name(name: &str) -> Result<()> {
    ensure!(
        !name.is_empty()
            && name.len() <= 64
            && !name.starts_with(['.', '-'])
            && name
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"-_.+".contains(&b)),
        "bad package name {name:?}"
    );
    Ok(())
}

pub fn check_path(path: &str) -> Result<()> {
    ensure!(
        !path.is_empty() && !path.contains('\0') && !path.starts_with('/') && !path.ends_with('/'),
        "bad path {path:?}"
    );
    ensure!(
        path.split('/')
            .all(|c| !c.is_empty() && c != "." && c != ".."),
        "bad path {path:?}"
    );
    ensure!(
        ALLOWED.iter().any(|p| path.starts_with(p)),
        "path {path:?} is outside the directories kl manages"
    );
    Ok(())
}

pub fn resolve_link(path: &str, target: &str) -> Result<String> {
    ensure!(
        !target.is_empty() && !target.starts_with('/') && !target.contains('\0'),
        "symlink {path:?} has a bad target {target:?}"
    );
    let mut parts: Vec<&str> = path.split('/').collect();
    parts.pop();
    for c in target.split('/') {
        match c {
            "" | "." => {}
            ".." => {
                ensure!(parts.pop().is_some(), "symlink {path:?} leaves the root");
            }
            c => parts.push(c),
        }
    }
    let joined = parts.join("/");
    check_path(&joined)
        .map_err(|_| anyhow!("symlink {path:?} points outside the directories kl manages"))?;
    Ok(joined)
}

impl Manifest {
    pub fn validate(&self, name: &str, version: &str, arch: &str) -> Result<()> {
        ensure!(
            self.name == name && self.version == version && self.arch == arch,
            "the manifest is for {} {} {}, not {name} {version} {arch}",
            self.name,
            self.version,
            self.arch
        );
        check_name(&self.name)?;
        ensure!(
            self.files.len() <= ENTRY_LIMIT,
            "{} lists too many files",
            self.name
        );
        let links: BTreeSet<&str> = self
            .files
            .iter()
            .filter(|f| f.kind == Kind::Symlink)
            .map(|f| f.path.as_str())
            .collect();
        let mut seen = BTreeSet::new();
        for f in &self.files {
            check_path(&f.path)?;
            ensure!(seen.insert(f.path.as_str()), "{} is listed twice", f.path);
            let mut ancestor = f.path.as_str();
            while let Some(i) = ancestor.rfind('/') {
                ancestor = &ancestor[..i];
                ensure!(
                    !links.contains(ancestor),
                    "{} lies under the symlink {ancestor}",
                    f.path
                );
            }
            match f.kind {
                Kind::File => ensure!(
                    f.size.is_some()
                        && f.sha256.as_deref().is_some_and(kido_trust::valid_sha256)
                        && f.target.is_none(),
                    "file {} needs a size and a sha256",
                    f.path
                ),
                Kind::Dir => ensure!(
                    f.size.is_none() && f.sha256.is_none() && f.target.is_none(),
                    "directory {} carries file fields",
                    f.path
                ),
                Kind::Symlink => {
                    let target = f
                        .target
                        .as_deref()
                        .ok_or_else(|| anyhow!("symlink {} has no target", f.path))?;
                    resolve_link(&f.path, target)?;
                }
            }
        }
        Ok(())
    }

    #[must_use]
    pub fn by_path(&self) -> BTreeMap<&str, &FileEntry> {
        self.files.iter().map(|f| (f.path.as_str(), f)).collect()
    }

    #[must_use]
    pub fn file_bytes(&self) -> u64 {
        self.files.iter().filter_map(|f| f.size).sum()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn paths_outside_the_managed_directories_are_refused() {
        for bad in [
            "",
            "/usr/lib/x",
            "usr/lib/",
            "usr/lib/../../etc/passwd",
            "etc/passwd",
            "usr/lib/./x",
            "usr//lib/x",
        ] {
            assert!(check_path(bad).is_err(), "{bad:?} was accepted");
        }
        check_path("usr/lib/libfoo.dylib").unwrap();
        check_path("System/Library/Frameworks/Foo.framework/Foo").unwrap();
    }

    #[test]
    fn symlinks_that_leave_the_managed_directories_are_refused() {
        assert_eq!(
            resolve_link("usr/lib/libfoo.1.dylib", "libfoo.dylib").unwrap(),
            "usr/lib/libfoo.dylib"
        );
        assert_eq!(
            resolve_link(
                "System/Library/Frameworks/F.framework/F",
                "Versions/Current/F"
            )
            .unwrap(),
            "System/Library/Frameworks/F.framework/Versions/Current/F"
        );
        for (p, t) in [
            ("usr/lib/x", "/etc/passwd"),
            ("usr/lib/x", "../../../etc/passwd"),
            ("usr/lib/x", "../../etc"),
        ] {
            assert!(resolve_link(p, t).is_err(), "{p} -> {t} was accepted");
        }
    }

    #[test]
    fn names_are_restricted() {
        for bad in ["", ".hidden", "-rf", "a/b", "a b", &"x".repeat(65)] {
            assert!(check_name(bad).is_err(), "{bad:?} was accepted");
        }
        check_name("libsqlite3").unwrap();
        check_name("c++-runtime").unwrap();
    }
}
