use crate::repo::Repo;
use anyhow::{Context, Result, anyhow, bail, ensure};
use kido_trust::{
    DEFAULT_CHANNEL, DEFAULT_KL_VERSION, OsVersion, RepoTarget, check_arch, check_build,
    check_channel, check_kl_version, parse_canonical_u32,
};
use rustix::fs::{CWD, Mode, OFlags, openat, readlinkat};
use rustix::io::Errno;
use std::collections::{BTreeMap, BTreeSet};
use std::fs::File;
use std::io::Read;
use std::path::Path;

pub const CONFIG_REL_PATH: &str = "etc/kl.conf";
pub const SYSTEM_VERSION_REL_PATH: &str = "System/Library/CoreServices/SystemVersion.plist";
const CONFIG_LIMIT: u64 = 64 << 10;
const MAX_MIRRORS: usize = 16;
const MAX_SYMLINK_HOPS: usize = 8;
const PLIST_MAX_DEPTH: usize = 64;

#[derive(Default, Debug, Clone, PartialEq, Eq)]
pub struct KlConf {
    pub repo: Option<String>,
    pub mirrors: Vec<String>,
    pub kl_version: Option<String>,
    pub os_version: Option<OsVersion>,
    pub os_build: Option<String>,
    pub channel: Option<String>,
    pub arch: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SystemVersion {
    pub os_version: OsVersion,
    pub os_build: String,
}

impl KlConf {
    pub fn load(root: &Path) -> Result<Self> {
        let Some(text) = read_confined_utf8(root, CONFIG_REL_PATH, CONFIG_LIMIT)? else {
            return Ok(KlConf::default());
        };
        Self::parse(&text)
            .with_context(|| format!("parse {}", root.join(CONFIG_REL_PATH).display()))
    }

    pub fn parse(text: &str) -> Result<Self> {
        let mut conf = KlConf::default();
        let mut seen_keys = BTreeSet::new();
        let mut major: Option<u32> = None;
        let mut minor: Option<u32> = None;
        let mut patch: Option<u32> = None;
        let mut has_os_version = false;

        for (idx, raw_line) in text.lines().enumerate() {
            let line_no = idx + 1;
            let line = raw_line.trim();
            if line.is_empty() || line.starts_with('#') {
                continue;
            }
            let Some((raw_key, raw_val)) = line.split_once('=') else {
                bail!("line {line_no}: expected key = value");
            };
            let key = raw_key.trim();
            let val = strip_quotes(raw_val.trim())
                .with_context(|| format!("line {line_no}: bad value"))?;
            ensure!(!val.is_empty(), "line {line_no}: value cannot be empty");
            ensure!(
                !val.bytes().any(|b| b < 0x20 || b == 0x7f),
                "line {line_no}: value contains control characters"
            );

            if key != "mirror" {
                ensure!(
                    seen_keys.insert(key.to_owned()),
                    "line {line_no}: duplicate key {key:?}"
                );
            }

            match key {
                "repo" => {
                    Repo::validate_url(val, false)?;
                    conf.repo = Some(val.to_owned());
                }
                "mirror" => {
                    ensure!(
                        conf.mirrors.len() < MAX_MIRRORS,
                        "too many mirror entries (max {MAX_MIRRORS})"
                    );
                    Repo::validate_url(val, true)?;
                    conf.mirrors.push(val.to_owned());
                }
                "kl_version" => {
                    check_kl_version(val)?;
                    conf.kl_version = Some(val.to_owned());
                }
                "os_version" => {
                    has_os_version = true;
                    conf.os_version = Some(OsVersion::parse(val)?);
                }
                "major" => {
                    major = Some(parse_canonical_u32(val, "major", val)?);
                }
                "minor" => {
                    minor = Some(parse_canonical_u32(val, "minor", val)?);
                }
                "patch" => {
                    patch = Some(parse_canonical_u32(val, "patch", val)?);
                }
                "os_build" => {
                    check_build(val)?;
                    conf.os_build = Some(val.to_owned());
                }
                "channel" => {
                    check_channel(val)?;
                    conf.channel = Some(val.to_owned());
                }
                "arch" => {
                    check_arch(val)?;
                    conf.arch = Some(val.to_owned());
                }
                _ => bail!("line {line_no}: unknown configuration key"),
            }
        }

        let has_split = major.is_some() || minor.is_some() || patch.is_some();
        ensure!(
            !(has_os_version && has_split),
            "os_version cannot be combined with major/minor/patch"
        );
        if has_split {
            let maj = major.ok_or_else(|| anyhow!("major is required when minor/patch is set"))?;
            let min = minor.ok_or_else(|| anyhow!("minor is required when major is set"))?;
            conf.os_version = Some(OsVersion {
                major: maj,
                minor: min,
                patch: patch.unwrap_or(0),
            });
        }

        Ok(conf)
    }
}

impl SystemVersion {
    pub fn load(root: &Path) -> Result<Option<Self>> {
        let Some(text) = read_confined_utf8(root, SYSTEM_VERSION_REL_PATH, CONFIG_LIMIT)? else {
            return Ok(None);
        };
        Self::parse(&text)
            .with_context(|| format!("parse {}", root.join(SYSTEM_VERSION_REL_PATH).display()))
            .map(Some)
    }

    pub fn parse(xml: &str) -> Result<Self> {
        let entries = parse_plist_top_dict(xml)?;
        let raw_ver = entries
            .get("ProductVersion")
            .ok_or_else(|| anyhow!("SystemVersion.plist is missing ProductVersion"))?;
        let raw_build = entries
            .get("ProductBuildVersion")
            .ok_or_else(|| anyhow!("SystemVersion.plist is missing ProductBuildVersion"))?;
        let os_version = OsVersion::parse(raw_ver.trim())?;
        let os_build = raw_build.trim().to_owned();
        check_build(&os_build)?;
        Ok(SystemVersion {
            os_version,
            os_build,
        })
    }
}

#[derive(Default, Debug, Clone)]
pub struct Overrides {
    pub repo: Option<String>,
    pub mirrors: Vec<String>,
    pub kl_version: Option<String>,
    pub os_version: Option<String>,
    pub os_build: Option<String>,
    pub channel: Option<String>,
    pub arch: Option<String>,
}

#[derive(Debug, Clone)]
pub struct EffectiveConfig {
    pub repo: String,
    pub mirrors: Vec<String>,
    pub kl_version: String,
    pub os_version: Option<OsVersion>,
    pub os_build: Option<String>,
    pub channel: String,
    pub arch: String,
}

impl EffectiveConfig {
    pub fn resolve(
        root: &Path,
        overrides: &Overrides,
        default_arch: impl FnOnce() -> String,
    ) -> Result<Self> {
        let conf = KlConf::load(root)?;

        let repo = overrides
            .repo
            .clone()
            .or(conf.repo)
            .unwrap_or_else(|| crate::DEFAULT_REPO.to_owned());
        Repo::validate_url(&repo, false)?;

        let mirrors = if overrides.mirrors.is_empty() {
            conf.mirrors
        } else {
            ensure!(
                overrides.mirrors.len() <= MAX_MIRRORS,
                "too many --mirror flags (max {MAX_MIRRORS})"
            );
            for m in &overrides.mirrors {
                Repo::validate_url(m, true)?;
            }
            overrides.mirrors.clone()
        };

        let kl_version = overrides
            .kl_version
            .clone()
            .or(conf.kl_version)
            .unwrap_or_else(|| DEFAULT_KL_VERSION.to_owned());
        check_kl_version(&kl_version)?;

        let os_version = match &overrides.os_version {
            Some(v) => Some(OsVersion::parse(v)?),
            None => conf.os_version,
        };

        let os_build = match &overrides.os_build {
            Some(b) => {
                check_build(b)?;
                Some(b.clone())
            }
            None => conf.os_build,
        };

        let channel = overrides
            .channel
            .clone()
            .or(conf.channel)
            .unwrap_or_else(|| DEFAULT_CHANNEL.to_owned());
        check_channel(&channel)?;

        let arch = overrides
            .arch
            .clone()
            .or(conf.arch)
            .unwrap_or_else(default_arch);
        check_arch(&arch)?;

        Ok(EffectiveConfig {
            repo,
            mirrors,
            kl_version,
            os_version,
            os_build,
            channel,
            arch,
        })
    }

    pub fn resolve_target(&self, root: &Path) -> Result<RepoTarget> {
        let detected = if self.os_version.is_none() || self.os_build.is_none() {
            SystemVersion::load(root)?
        } else {
            None
        };

        let os_version = self
            .os_version
            .or_else(|| detected.as_ref().map(|d| d.os_version))
            .ok_or_else(|| {
                anyhow!(
                    "cannot determine OS version: /{SYSTEM_VERSION_REL_PATH} not found; set os_version in /{CONFIG_REL_PATH} or pass --os-version"
                )
            })?;

        let os_build = self
            .os_build
            .clone()
            .or_else(|| detected.map(|d| d.os_build))
            .ok_or_else(|| {
                anyhow!(
                    "cannot determine OS build: /{SYSTEM_VERSION_REL_PATH} not found; set os_build in /{CONFIG_REL_PATH} or pass --os-build"
                )
            })?;

        Ok(RepoTarget::new(
            self.kl_version.clone(),
            os_version,
            os_build,
            self.channel.clone(),
        )?)
    }
}

/// Safely opens a regular file strictly confined inside `root`.
///
/// Intermediate directory symlinks (such as macOS `/etc -> private/etc`) are allowed ONLY when
/// they are relative and remain strictly within `root` (within `MAX_SYMLINK_HOPS`). The final
/// leaf file must NOT be a symlink and is opened with `O_NOFOLLOW | O_NONBLOCK` so FIFOs and
/// escaping symlinks are rejected without blocking.
pub fn open_confined_regular_file(root: &Path, rel: &str) -> Result<Option<File>> {
    let root_fd = match openat(
        CWD,
        root,
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC,
        Mode::empty(),
    ) {
        Ok(fd) => fd,
        Err(Errno::NOENT) => return Ok(None),
        Err(e) => return Err(e).with_context(|| format!("open root {}", root.display())),
    };

    let mut pending: Vec<String> = rel.split('/').map(str::to_owned).collect();
    ensure!(
        !pending.is_empty()
            && pending
                .iter()
                .all(|c| !c.is_empty() && c != "." && c != ".."),
        "bad confined path {rel:?}"
    );

    let mut hops = 0usize;
    let mut stack: Vec<String> = Vec::new();

    'restart: loop {
        let mut dir_fd = root_fd
            .try_clone()
            .context("clone root directory descriptor")?;
        let mut cur_components: Vec<String> = Vec::new();
        let total = pending.len();

        for (idx, comp) in pending.iter().enumerate() {
            let is_leaf = idx + 1 == total;
            if is_leaf {
                return open_confined_leaf(&dir_fd, comp, root, rel);
            }

            match openat(
                &dir_fd,
                comp.as_str(),
                OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::empty(),
            ) {
                Ok(next_fd) => {
                    dir_fd = next_fd;
                    cur_components.push(comp.clone());
                }
                Err(Errno::NOENT) => return Ok(None),
                Err(Errno::LOOP | Errno::NOTDIR) => {
                    hops += 1;
                    ensure!(
                        hops <= MAX_SYMLINK_HOPS,
                        "too many symlinks resolving {rel}"
                    );
                    resolve_confined_dir_symlink(
                        &dir_fd,
                        comp,
                        rel,
                        &cur_components,
                        &pending[idx + 1..],
                        &mut stack,
                    )?;
                    pending.clone_from(&stack);
                    continue 'restart;
                }
                Err(e) => {
                    return Err(e).with_context(|| format!("open directory {comp} in {rel}"));
                }
            }
        }
        return Ok(None);
    }
}

fn open_confined_leaf(
    dir_fd: &rustix::fd::OwnedFd,
    comp: &str,
    root: &Path,
    rel: &str,
) -> Result<Option<File>> {
    let fd = match openat(
        dir_fd,
        comp,
        OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
        Mode::empty(),
    ) {
        Ok(fd) => fd,
        Err(Errno::NOENT) => return Ok(None),
        Err(Errno::LOOP) => bail!("{rel} must not be a symlink"),
        Err(e) => return Err(e).with_context(|| format!("open {}", root.join(rel).display())),
    };
    let file = File::from(fd);
    let md = file
        .metadata()
        .with_context(|| format!("stat {}", root.join(rel).display()))?;
    ensure!(
        md.is_file(),
        "{} is not a regular file",
        root.join(rel).display()
    );
    Ok(Some(file))
}

fn resolve_confined_dir_symlink(
    dir_fd: &rustix::fd::OwnedFd,
    comp: &str,
    rel: &str,
    cur_components: &[String],
    tail: &[String],
    out: &mut Vec<String>,
) -> Result<()> {
    let Ok(target_cstr) = readlinkat(dir_fd, comp, Vec::new()) else {
        bail!("component {comp:?} in {rel} is not a directory or readable symlink");
    };
    let target = target_cstr
        .to_str()
        .map_err(|_| anyhow!("non-UTF-8 symlink target in {rel}"))?;
    ensure!(
        !target.is_empty() && !target.starts_with('/'),
        "absolute symlink in {rel} escapes root"
    );
    out.clear();
    out.extend_from_slice(cur_components);
    for seg in target.split('/') {
        match seg {
            "" | "." => {}
            ".." => {
                ensure!(
                    out.pop().is_some(),
                    "relative symlink in {rel} escapes root"
                );
            }
            s => out.push(s.to_owned()),
        }
    }
    out.extend_from_slice(tail);
    Ok(())
}

fn read_confined_utf8(root: &Path, rel: &str, limit: u64) -> Result<Option<String>> {
    let Some(file) = open_confined_regular_file(root, rel)? else {
        return Ok(None);
    };
    let mut buf = Vec::new();
    file.take(limit + 1)
        .read_to_end(&mut buf)
        .with_context(|| format!("read {}", root.join(rel).display()))?;
    ensure!(
        buf.len() as u64 <= limit,
        "{} is larger than {limit} bytes",
        root.join(rel).display()
    );
    let text = String::from_utf8(buf)
        .with_context(|| format!("{} is not valid UTF-8", root.join(rel).display()))?;
    Ok(Some(text))
}

fn strip_quotes(s: &str) -> Result<&str> {
    if let Some(rest) = s.strip_prefix('"') {
        rest.strip_suffix('"')
            .ok_or_else(|| anyhow!("unclosed double quote"))
    } else if let Some(rest) = s.strip_prefix('\'') {
        rest.strip_suffix('\'')
            .ok_or_else(|| anyhow!("unclosed single quote"))
    } else {
        Ok(s)
    }
}

#[derive(Debug)]
enum XmlToken<'a> {
    Start(&'a str),
    End(&'a str),
    Empty(&'a str),
    Text(&'a str),
    Cdata(&'a str),
}

fn tokenize_xml(mut s: &str) -> Result<Vec<XmlToken<'_>>> {
    let mut tokens = Vec::new();
    while !s.is_empty() {
        if let Some(rest) = s.strip_prefix("<!--") {
            let Some(end) = rest.find("-->") else {
                bail!("unterminated XML comment");
            };
            s = &rest[end + 3..];
        } else if let Some(rest) = s.strip_prefix("<?") {
            let Some(end) = rest.find("?>") else {
                bail!("unterminated XML processing instruction");
            };
            s = &rest[end + 2..];
        } else if let Some(rest) = s.strip_prefix("<![CDATA[") {
            let Some(end) = rest.find("]]>") else {
                bail!("unterminated XML CDATA section");
            };
            tokens.push(XmlToken::Cdata(&rest[..end]));
            s = &rest[end + 3..];
        } else if let Some(rest) = s.strip_prefix("<!") {
            // Skip <!DOCTYPE ...> while strictly refusing DTD internal subsets (`[`).
            let end = find_doctype_end(rest)?;
            s = &rest[end + 1..];
        } else if let Some(rest) = s.strip_prefix("</") {
            let Some(end) = rest.find('>') else {
                bail!("unterminated XML end tag");
            };
            let name = rest[..end].trim();
            ensure!(!name.is_empty(), "empty XML end tag");
            tokens.push(XmlToken::End(name));
            s = &rest[end + 1..];
        } else if let Some(rest) = s.strip_prefix('<') {
            let Some(end) = find_tag_end(rest) else {
                bail!("unterminated XML start tag");
            };
            let inside = rest[..end].trim();
            let (inside, empty) = match inside.strip_suffix('/') {
                Some(stripped) => (stripped.trim_end(), true),
                None => (inside, false),
            };
            let name = inside
                .split(|c: char| c.is_ascii_whitespace())
                .next()
                .unwrap_or("");
            ensure!(!name.is_empty(), "empty XML start tag");
            if empty {
                tokens.push(XmlToken::Empty(name));
            } else {
                tokens.push(XmlToken::Start(name));
            }
            s = &rest[end + 1..];
        } else {
            let next = s.find('<').unwrap_or(s.len());
            tokens.push(XmlToken::Text(&s[..next]));
            s = &s[next..];
        }
    }
    Ok(tokens)
}

fn find_tag_end(s: &str) -> Option<usize> {
    let mut quote: Option<u8> = None;
    for (i, b) in s.bytes().enumerate() {
        match quote {
            Some(q) if b == q => quote = None,
            None if b == b'"' || b == b'\'' => quote = Some(b),
            None if b == b'>' => return Some(i),
            Some(_) | None => {}
        }
    }
    None
}

fn find_doctype_end(s: &str) -> Result<usize> {
    let mut quote: Option<u8> = None;
    for (i, b) in s.bytes().enumerate() {
        match quote {
            Some(q) if b == q => quote = None,
            None if b == b'"' || b == b'\'' => quote = Some(b),
            None if b == b'[' => bail!("DTD internal subsets are not allowed in plist"),
            None if b == b'>' => return Ok(i),
            Some(_) | None => {}
        }
    }
    bail!("unterminated XML declaration")
}

fn unescape_xml(s: &str) -> Result<String> {
    let mut out = String::with_capacity(s.len());
    let mut rest = s;
    while let Some(i) = rest.find('&') {
        out.push_str(&rest[..i]);
        let after = &rest[i + 1..];
        let Some(semi) = after.find(';') else {
            bail!("unterminated XML entity reference");
        };
        let entity = &after[..semi];
        match entity {
            "amp" => out.push('&'),
            "lt" => out.push('<'),
            "gt" => out.push('>'),
            "quot" => out.push('"'),
            "apos" => out.push('\''),
            _ => bail!("unsupported XML entity &{entity};"),
        }
        rest = &after[semi + 1..];
    }
    out.push_str(rest);
    Ok(out)
}

#[derive(Default, PartialEq, Eq)]
enum PlistPhase {
    #[default]
    BeforePlist,
    InPlist,
    InTopDict,
    AfterTopDict,
    Done,
}

#[derive(Default)]
struct PlistParser<'a> {
    stack: Vec<&'a str>,
    phase: PlistPhase,
    current_key: Option<String>,
    collecting: Option<(&'a str, String)>,
    entries: BTreeMap<String, String>,
}

impl<'a> PlistParser<'a> {
    fn on_start(&mut self, name: &'a str) -> Result<()> {
        ensure!(
            self.phase != PlistPhase::Done,
            "unexpected trailing element <{name}>"
        );
        ensure!(
            self.collecting.is_none(),
            "unexpected nested element <{name}> inside <key> or <string>"
        );
        ensure!(self.stack.len() < PLIST_MAX_DEPTH, "plist nesting too deep");
        if self.phase == PlistPhase::BeforePlist {
            ensure!(name == "plist", "expected <plist>, got <{name}>");
            self.phase = PlistPhase::InPlist;
            self.stack.push(name);
            return Ok(());
        }
        if self.stack.len() == 1 {
            ensure!(
                name == "dict" && self.phase == PlistPhase::InPlist,
                "expected a single top-level <dict> inside <plist>"
            );
            self.phase = PlistPhase::InTopDict;
            self.stack.push(name);
            return Ok(());
        }
        if self.stack.len() == 2 && self.phase == PlistPhase::InTopDict {
            match name {
                "key" => {
                    ensure!(
                        self.current_key.is_none(),
                        "consecutive <key> tags in plist dict"
                    );
                    self.collecting = Some(("key", String::new()));
                }
                "string" if self.current_key.is_some() => {
                    self.collecting = Some(("string", String::new()));
                }
                _ => self.current_key = None,
            }
        }
        self.stack.push(name);
        Ok(())
    }

    fn on_end(&mut self, name: &str) -> Result<()> {
        let Some(top) = self.stack.pop() else {
            bail!("unexpected closing tag </{name}>");
        };
        ensure!(top == name, "mismatched XML tag </{name}> for <{top}>");
        if let Some((tag, val)) = self.collecting.take() {
            ensure!(tag == name, "mismatched XML tag </{name}> for <{tag}>");
            if tag == "key" {
                self.current_key = Some(val);
            } else if tag == "string" {
                if let Some(k) = self.current_key.take() {
                    ensure!(
                        self.entries.insert(k.clone(), val).is_none(),
                        "duplicate plist key {k:?}"
                    );
                }
            }
        }
        if self.stack.len() == 1 && self.phase == PlistPhase::InTopDict {
            self.phase = PlistPhase::AfterTopDict;
        }
        if self.stack.is_empty() && self.phase == PlistPhase::AfterTopDict {
            self.phase = PlistPhase::Done;
        }
        Ok(())
    }
}

fn parse_plist_top_dict(xml: &str) -> Result<BTreeMap<String, String>> {
    let tokens = tokenize_xml(xml)?;
    let mut p = PlistParser::default();
    for tok in tokens {
        match tok {
            XmlToken::Start(name) => p.on_start(name)?,
            XmlToken::Empty(name) => {
                ensure!(
                    p.phase != PlistPhase::Done,
                    "unexpected trailing element <{name}/>"
                );
                ensure!(
                    p.collecting.is_none(),
                    "unexpected nested element <{name}/> inside <key> or <string>"
                );
                if p.phase == PlistPhase::InTopDict && p.stack.len() == 2 {
                    ensure!(name != "key", "empty <key/> tag in plist dict");
                    if name == "string" {
                        if let Some(k) = p.current_key.take() {
                            ensure!(
                                p.entries.insert(k.clone(), String::new()).is_none(),
                                "duplicate plist key {k:?}"
                            );
                        }
                    } else {
                        p.current_key = None;
                    }
                }
            }
            XmlToken::Text(t) => {
                if p.phase == PlistPhase::Done {
                    ensure!(
                        t.trim().is_empty(),
                        "unexpected trailing text after </plist>"
                    );
                } else if let Some((_, buf)) = &mut p.collecting {
                    buf.push_str(&unescape_xml(t)?);
                }
            }
            XmlToken::Cdata(t) => {
                ensure!(
                    p.phase != PlistPhase::Done,
                    "unexpected trailing CDATA after </plist>"
                );
                if let Some((_, buf)) = &mut p.collecting {
                    buf.push_str(t);
                }
            }
            XmlToken::End(name) => p.on_end(name)?,
        }
    }
    ensure!(
        p.stack.is_empty() && p.phase == PlistPhase::Done && p.current_key.is_none(),
        "incomplete plist document"
    );
    Ok(p.entries)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;

    #[test]
    fn parses_kl_conf_with_mirrors_and_os_version() {
        let conf = KlConf::parse(
            r#"
            # comment
            repo = https://kl.kido.sh
            mirror = https://mirror1.internal/kl
            mirror = http://127.0.0.1:8080
            kl_version = v1
            os_version = "26.5"
            os_build = 25F79
            channel = nightly
            arch = arm64e
            "#,
        )
        .unwrap();
        assert_eq!(conf.repo.as_deref(), Some("https://kl.kido.sh"));
        assert_eq!(
            conf.mirrors,
            vec!["https://mirror1.internal/kl", "http://127.0.0.1:8080"]
        );
        assert_eq!(conf.kl_version.as_deref(), Some("v1"));
        assert_eq!(
            conf.os_version,
            Some(OsVersion {
                major: 26,
                minor: 5,
                patch: 0
            })
        );
        assert_eq!(conf.os_build.as_deref(), Some("25F79"));
        assert_eq!(conf.channel.as_deref(), Some("nightly"));
        assert_eq!(conf.arch.as_deref(), Some("arm64e"));
    }

    #[test]
    fn parses_kl_conf_with_split_major_minor_patch() {
        let conf = KlConf::parse("major = 26\nminor = 1\npatch = 1\nos_build = 24B56\n").unwrap();
        assert_eq!(
            conf.os_version,
            Some(OsVersion {
                major: 26,
                minor: 1,
                patch: 1
            })
        );
        let padded = KlConf::parse("major = 27\nminor = 0\n").unwrap();
        assert_eq!(
            padded.os_version,
            Some(OsVersion {
                major: 27,
                minor: 0,
                patch: 0
            })
        );
    }

    #[test]
    fn rejects_invalid_or_ambiguous_kl_conf() {
        for bad in [
            "unknown_key = foo",
            "channel = beta",
            "kl_version = ../v1",
            "os_build = ../24B56",
            "os_version = 26.5\nmajor = 26\nminor = 5",
            "major = 26",
            "minor = 5",
            "channel = latest\nchannel = nightly",
            "repo = ",
            "repo = http://kl.kido.sh",
            "mirror = http://8.8.8.8/kl",
            "no_equals_sign",
            "repo = \"unclosed",
        ] {
            assert!(KlConf::parse(bad).is_err(), "{bad:?} was accepted");
        }
    }

    #[test]
    fn parses_system_version_plist_safely() {
        let plist = r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<!-- <key>ProductVersion</key><string>99.9</string> -->
	<key>Nested</key>
	<dict>
		<key>ProductVersion</key>
		<string>1.0</string>
		<key>ProductBuildVersion</key>
		<string>WRONG</string>
	</dict>
	<key>Flag</key>
	<true/>
	<key>ProductBuildVersion</key>
	<string>25F74</string>
	<key>ProductName</key>
	<string>KidoDarwin</string>
	<key>ProductVersion</key>
	<string>26.5</string>
</dict>
</plist>"#;
        let sv = SystemVersion::parse(plist).unwrap();
        assert_eq!(
            sv.os_version,
            OsVersion {
                major: 26,
                minor: 5,
                patch: 0
            }
        );
        assert_eq!(sv.os_build, "25F74");
    }

    #[test]
    fn rejects_malformed_duplicate_nested_or_xxe_plist() {
        for bad in [
            r#"<?xml version="1.0"?>
<!DOCTYPE plist [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>
<plist version="1.0"><dict>
<key>ProductVersion</key><string>&xxe;</string>
<key>ProductBuildVersion</key><string>25F74</string>
</dict></plist>"#,
            r#"<plist version="1.0"><dict>
<key>ProductVersion</key><string>26.5</string>
<key>ProductBuildVersion</key><string>../evil</string>
</dict></plist>"#,
            // Duplicate key
            r#"<plist version="1.0"><dict>
<key>ProductVersion</key><string>26.5</string>
<key>ProductVersion</key><string>27.0</string>
<key>ProductBuildVersion</key><string>25F74</string>
</dict></plist>"#,
            // Nested tag inside <string>
            r#"<plist version="1.0"><dict>
<key>ProductVersion</key><string>26.5<x/>0</string>
<key>ProductBuildVersion</key><string>25F74</string>
</dict></plist>"#,
            // Trailing element after </plist>
            r#"<plist version="1.0"><dict>
<key>ProductVersion</key><string>26.5</string>
<key>ProductBuildVersion</key><string>25F74</string>
</dict></plist><junk/>"#,
        ] {
            assert!(SystemVersion::parse(bad).is_err(), "{bad} was accepted");
        }
    }

    #[test]
    fn confined_loader_allows_macos_private_etc_symlink_and_rejects_escapes() {
        let outer = tempfile::tempdir().unwrap();
        let outside = outer.path().join("outside.conf");
        std::fs::write(&outside, "channel = nightly\n").unwrap();

        let root = outer.path().join("root");
        std::fs::create_dir_all(root.join("private/etc")).unwrap();
        std::fs::write(root.join("private/etc/kl.conf"), "channel = nightly\n").unwrap();
        symlink("private/etc", root.join("etc")).unwrap();

        // Relative confined /etc -> private/etc works!
        let conf = KlConf::load(&root).unwrap();
        assert_eq!(conf.channel.as_deref(), Some("nightly"));

        // Leaf symlink kl.conf -> outside.conf is rejected!
        std::fs::remove_file(root.join("private/etc/kl.conf")).unwrap();
        symlink(&outside, root.join("private/etc/kl.conf")).unwrap();
        assert!(KlConf::load(&root).is_err());

        // Escaping directory symlink etc -> ../.. is rejected!
        std::fs::remove_file(root.join("etc")).unwrap();
        symlink("..", root.join("etc")).unwrap();
        assert!(KlConf::load(&root).is_err());
    }

    #[test]
    fn resolves_precedence_cli_over_conf_over_plist() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        std::fs::create_dir_all(root.join("etc")).unwrap();
        std::fs::create_dir_all(root.join("System/Library/CoreServices")).unwrap();
        std::fs::write(
            root.join(SYSTEM_VERSION_REL_PATH),
            r#"<plist version="1.0"><dict>
<key>ProductVersion</key><string>26.5</string>
<key>ProductBuildVersion</key><string>25F74</string>
</dict></plist>"#,
        )
        .unwrap();
        std::fs::write(
            root.join(CONFIG_REL_PATH),
            "channel = nightly\nmirror = https://m1.example/kl\n",
        )
        .unwrap();

        let eff =
            EffectiveConfig::resolve(root, &Overrides::default(), || "arm64e".into()).unwrap();
        assert_eq!(eff.channel, "nightly");
        assert_eq!(eff.mirrors, vec!["https://m1.example/kl"]);
        let target = eff.resolve_target(root).unwrap();
        assert_eq!(target.to_string(), "v1/26/5/0/25F74/nightly");

        let cli = Overrides {
            os_version: Some("26.1.1".into()),
            os_build: Some("24B56".into()),
            channel: Some("latest".into()),
            mirrors: vec!["http://127.0.0.1:9999".into()],
            ..Overrides::default()
        };
        let eff2 = EffectiveConfig::resolve(root, &cli, || "arm64e".into()).unwrap();
        assert_eq!(eff2.mirrors, vec!["http://127.0.0.1:9999"]);
        let target2 = eff2.resolve_target(root).unwrap();
        assert_eq!(target2.to_string(), "v1/26/1/1/24B56/latest");
    }
}
