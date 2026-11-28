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
const META_FIELD_LIMIT: usize = 1024;
const META_LIST_LIMIT: usize = 10_000;

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

pub fn check_meta_text(label: &str, text: &str, max_len: usize) -> Result<()> {
    ensure!(
        text.len() <= max_len && !text.bytes().any(|b| b < 0x20 || b == 0x7f),
        "bad {label} metadata value"
    );
    Ok(())
}

pub fn check_path(path: &str) -> Result<()> {
    ensure!(
        !path.is_empty()
            && !path.contains('\0')
            && !path.starts_with('/')
            && !path.ends_with('/')
            && !path.bytes().any(|b| b < 0x20 || b == 0x7f),
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
        !target.is_empty()
            && !target.starts_with('/')
            && !target.ends_with('/')
            && !target.contains('\0')
            && !target.bytes().any(|b| b < 0x20 || b == 0x7f),
        "symlink {path:?} has a bad target {target:?}"
    );
    let mut parts: Vec<&str> = path.split('/').collect();
    parts.pop();
    let mut saw_non_parent = false;
    for c in target.split('/') {
        match c {
            "" | "." => {
                anyhow::bail!("symlink {path:?} has non-canonical target {target:?}");
            }
            ".." => {
                ensure!(
                    !saw_non_parent,
                    "symlink {path:?} has '..' after a path segment in {target:?}"
                );
                ensure!(parts.pop().is_some(), "symlink {path:?} leaves the root");
            }
            c => {
                saw_non_parent = true;
                parts.push(c);
            }
        }
    }
    let joined = parts.join("/");
    check_path(&joined)
        .map_err(|_| anyhow!("symlink {path:?} points outside the directories kl manages"))?;
    Ok(joined)
}

#[must_use]
pub fn fold_path(path: &str) -> String {
    path.to_lowercase()
}

impl Manifest {
    pub fn validate(&self, name: &str, version: &str, arch: &str) -> Result<()> {
        crate::check_arch(arch)?;
        ensure!(
            self.name == name && self.version == version && self.arch == arch,
            "the manifest is for {} {} {}, not {name} {version} {arch}",
            self.name,
            self.version,
            self.arch
        );
        check_name(&self.name)?;
        ensure!(!self.version.is_empty(), "package version cannot be empty");
        check_meta_text("version", &self.version, 128)?;
        check_meta_text("license", &self.license, 256)?;
        check_meta_text("summary", &self.summary, META_FIELD_LIMIT)?;
        ensure!(
            self.provides.len() <= META_LIST_LIMIT && self.depends.len() <= META_LIST_LIMIT,
            "{} lists too many provides or depends entries",
            self.name
        );
        for p in &self.provides {
            check_meta_text("provides", p, META_FIELD_LIMIT)?;
        }
        for d in &self.depends {
            check_meta_text("depends", d, META_FIELD_LIMIT)?;
        }
        ensure!(
            self.files.len() <= ENTRY_LIMIT,
            "{} lists too many files",
            self.name
        );
        let links_ci: BTreeSet<String> = self
            .files
            .iter()
            .filter(|f| f.kind == Kind::Symlink)
            .map(|f| fold_path(&f.path))
            .collect();
        let mut seen_ci = BTreeSet::new();
        for f in &self.files {
            check_path(&f.path)?;
            ensure!(
                seen_ci.insert(fold_path(&f.path)),
                "{} collides with another path in the manifest",
                f.path
            );
            ensure!(
                (f.mode & !0o777) == 0,
                "{} carries disallowed mode bits {:o}",
                f.path,
                f.mode
            );
            let mut ancestor = f.path.as_str();
            while let Some(i) = ancestor.rfind('/') {
                ancestor = &ancestor[..i];
                ensure!(
                    !links_ci.contains(&fold_path(ancestor)),
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
                    ensure!(
                        f.size.is_none() && f.sha256.is_none(),
                        "symlink {} carries file fields",
                        f.path
                    );
                    let target = f
                        .target
                        .as_deref()
                        .ok_or_else(|| anyhow!("symlink {} has no target", f.path))?;
                    resolve_link(&f.path, target)?;
                }
            }
        }
        self.file_bytes()?;
        Ok(())
    }

    #[must_use]
    pub fn by_path(&self) -> BTreeMap<&str, &FileEntry> {
        self.files.iter().map(|f| (f.path.as_str(), f)).collect()
    }

    pub fn file_bytes(&self) -> Result<u64> {
        self.files
            .iter()
            .filter_map(|f| f.size)
            .try_fold(0u64, |acc, s| {
                acc.checked_add(s)
                    .ok_or_else(|| anyhow!("manifest file sizes overflow u64"))
            })
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
    fn symlinks_that_leave_the_managed_directories_or_chain_dotdot_are_refused() {
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
        assert_eq!(
            resolve_link("usr/lib/sub/libfoo.dylib", "../libfoo.dylib").unwrap(),
            "usr/lib/libfoo.dylib"
        );
        for (p, t) in [
            ("usr/lib/x", "/etc/passwd"),
            ("usr/lib/x", "../../../etc/passwd"),
            ("usr/lib/x", "../../etc"),
            ("usr/lib/escape", "deep/a/b/c/d/../../../etc/passwd"),
            ("usr/lib/x", "./libfoo.dylib"),
            ("usr/lib/x", "a//b"),
            ("usr/lib/x", "dir/"),
        ] {
            assert!(resolve_link(p, t).is_err(), "{p} -> {t} was accepted");
        }
    }

    #[test]
    fn manifest_rejects_case_insensitive_symlink_ancestor_and_size_overflow() {
        let ci_escape = Manifest {
            name: "evil".into(),
            version: "1".into(),
            arch: "arm64e".into(),
            license: "MIT".into(),
            summary: String::new(),
            provides: vec![],
            depends: vec![],
            files: vec![
                FileEntry {
                    path: "usr/lib/foo".into(),
                    kind: Kind::Symlink,
                    mode: 0o755,
                    size: None,
                    sha256: None,
                    target: Some("bar".into()),
                },
                FileEntry {
                    path: "usr/lib/FOO/pwned".into(),
                    kind: Kind::File,
                    mode: 0o644,
                    size: Some(1),
                    sha256: Some("a".repeat(64)),
                    target: None,
                },
            ],
        };
        assert!(ci_escape.validate("evil", "1", "arm64e").is_err());

        let overflow = Manifest {
            name: "big".into(),
            version: "1".into(),
            arch: "arm64e".into(),
            license: "MIT".into(),
            summary: String::new(),
            provides: vec![],
            depends: vec![],
            files: vec![
                FileEntry {
                    path: "usr/lib/a".into(),
                    kind: Kind::File,
                    mode: 0o644,
                    size: Some(u64::MAX),
                    sha256: Some("a".repeat(64)),
                    target: None,
                },
                FileEntry {
                    path: "usr/lib/b".into(),
                    kind: Kind::File,
                    mode: 0o644,
                    size: Some(1),
                    sha256: Some("b".repeat(64)),
                    target: None,
                },
            ],
        };
        assert!(overflow.validate("big", "1", "arm64e").is_err());
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
