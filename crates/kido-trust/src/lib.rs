use ed25519_dalek::VerifyingKey;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use ssh_key::{Algorithm, PublicKey, SshSig};
use std::collections::BTreeMap;
use std::fmt;
use std::io::{self, Read};
use time::OffsetDateTime;
use time::format_description::well_known::Rfc3339;

pub const DEFAULT_KL_VERSION: &str = "v1";
pub const DEFAULT_CHANNEL: &str = "latest";
pub const CHANNELS: &[&str] = &["latest", "nightly"];
pub const ARCHES: &[&str] = &["arm64", "arm64e"];

#[derive(Debug)]
pub enum Error {
    NoKeys,
    BadKey(String),
    BadSignature(String),
    Untrusted,
    Expired {
        expires: String,
    },
    Rollback {
        seen: u64,
        got: u64,
    },
    WrongTarget {
        want: Box<RepoTarget>,
        got: Box<RepoTarget>,
    },
    InvalidTarget(String),
    Hash {
        want: String,
        got: String,
    },
    Size {
        want: u64,
        got: u64,
    },
    Parse(String),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::NoKeys => write!(f, "no trusted keys"),
            Error::BadKey(e) => write!(f, "bad trusted key: {e}"),
            Error::BadSignature(e) => write!(f, "bad signature: {e}"),
            Error::Untrusted => write!(f, "signature is not from a trusted key"),
            Error::Expired { expires } => write!(f, "signed metadata expired at {expires}"),
            Error::Rollback { seen, got } => {
                write!(
                    f,
                    "signed metadata version {got} is older than version {seen} seen before"
                )
            }
            Error::WrongTarget { want, got } => {
                write!(f, "signed metadata is for target {got}, not {want}")
            }
            Error::InvalidTarget(e) => write!(f, "invalid repository target: {e}"),
            Error::Hash { want, got } => write!(f, "sha256 {got} does not match {want}"),
            Error::Size { want, got } => write!(f, "size {got} does not match {want}"),
            Error::Parse(e) => write!(f, "cannot parse signed metadata: {e}"),
        }
    }
}

impl std::error::Error for Error {}

#[derive(Serialize, Deserialize, Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct OsVersion {
    pub major: u32,
    pub minor: u32,
    pub patch: u32,
}

impl OsVersion {
    pub fn parse(s: &str) -> Result<Self, Error> {
        let parts: Vec<&str> = s.split('.').collect();
        if parts.len() != 2 && parts.len() != 3 {
            return Err(Error::InvalidTarget(format!(
                "bad OS version {s:?}, expected MAJOR.MINOR or MAJOR.MINOR.PATCH"
            )));
        }
        let major = parse_canonical_u32(parts[0], "major", s)?;
        let minor = parse_canonical_u32(parts[1], "minor", s)?;
        let patch = if parts.len() == 3 {
            parse_canonical_u32(parts[2], "patch", s)?
        } else {
            0
        };
        Ok(OsVersion {
            major,
            minor,
            patch,
        })
    }
}

impl fmt::Display for OsVersion {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}.{}.{}", self.major, self.minor, self.patch)
    }
}

pub fn parse_canonical_u32(segment: &str, label: &str, raw: &str) -> Result<u32, Error> {
    if segment.is_empty()
        || !segment.bytes().all(|b| b.is_ascii_digit())
        || (segment.len() > 1 && segment.starts_with('0'))
    {
        return Err(Error::InvalidTarget(format!(
            "bad {label} version segment {segment:?} in {raw:?}"
        )));
    }
    segment.parse::<u32>().map_err(|_| {
        Error::InvalidTarget(format!(
            "bad {label} version segment {segment:?} in {raw:?}"
        ))
    })
}

pub fn check_kl_version(kl_version: &str) -> Result<(), Error> {
    let Some(rest) = kl_version.strip_prefix('v') else {
        return Err(Error::InvalidTarget(format!(
            "bad kl_version {kl_version:?}, expected v<positive-integer>"
        )));
    };
    let ok = !rest.is_empty()
        && rest.len() <= 10
        && matches!(rest.as_bytes()[0], b'1'..=b'9')
        && rest.bytes().all(|b| b.is_ascii_digit())
        && rest.parse::<u32>().is_ok();
    if !ok {
        return Err(Error::InvalidTarget(format!(
            "bad kl_version {kl_version:?}, expected v<positive-integer>"
        )));
    }
    Ok(())
}

pub fn check_build(build: &str) -> Result<(), Error> {
    let ok = !build.is_empty()
        && build.len() <= 64
        && !build.starts_with('.')
        && build != "."
        && build != ".."
        && build
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'));
    if !ok {
        return Err(Error::InvalidTarget(format!("bad build {build:?}")));
    }
    Ok(())
}

pub fn check_channel(channel: &str) -> Result<(), Error> {
    if !CHANNELS.contains(&channel) {
        return Err(Error::InvalidTarget(format!(
            "unknown channel {channel:?}, expected one of: {}",
            CHANNELS.join(", ")
        )));
    }
    Ok(())
}

pub fn check_arch(arch: &str) -> Result<(), Error> {
    if !ARCHES.contains(&arch) {
        return Err(Error::InvalidTarget(format!(
            "unknown architecture {arch:?}, expected one of: {}",
            ARCHES.join(", ")
        )));
    }
    Ok(())
}

#[derive(Serialize, Deserialize, Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct RepoTarget {
    pub kl_version: String,
    pub major: u32,
    pub minor: u32,
    pub patch: u32,
    pub build: String,
    pub channel: String,
}

impl RepoTarget {
    pub fn new(
        kl_version: impl Into<String>,
        os: OsVersion,
        build: impl Into<String>,
        channel: impl Into<String>,
    ) -> Result<Self, Error> {
        let target = RepoTarget {
            kl_version: kl_version.into(),
            major: os.major,
            minor: os.minor,
            patch: os.patch,
            build: build.into(),
            channel: channel.into(),
        };
        target.validate()?;
        Ok(target)
    }

    pub fn validate(&self) -> Result<(), Error> {
        check_kl_version(&self.kl_version)?;
        check_build(&self.build)?;
        check_channel(&self.channel)?;
        Ok(())
    }

    #[must_use]
    pub fn os_version(&self) -> OsVersion {
        OsVersion {
            major: self.major,
            minor: self.minor,
            patch: self.patch,
        }
    }

    pub fn metadata_prefix(&self) -> Result<String, Error> {
        self.validate()?;
        Ok(format!(
            "{}/{}/{}/{}/{}/{}",
            self.kl_version, self.major, self.minor, self.patch, self.build, self.channel
        ))
    }

    pub fn timestamp_path(&self) -> Result<String, Error> {
        Ok(format!("{}/timestamp.json", self.metadata_prefix()?))
    }

    pub fn timestamp_sig_path(&self) -> Result<String, Error> {
        Ok(format!("{}/timestamp.json.sig", self.metadata_prefix()?))
    }

    pub fn index_path(&self, arch: &str, sha256: &str) -> Result<String, Error> {
        check_arch(arch)?;
        if !valid_sha256(sha256) {
            return Err(Error::Parse(format!("bad sha256 {sha256}")));
        }
        Ok(format!(
            "{}/{arch}/index.{sha256}.json",
            self.metadata_prefix()?
        ))
    }

    pub fn blobs_prefix(&self) -> Result<String, Error> {
        check_kl_version(&self.kl_version)?;
        Ok(format!("{}/blobs/sha256", self.kl_version))
    }

    pub fn blob_path(&self, sha256: &str) -> Result<String, Error> {
        if !valid_sha256(sha256) {
            return Err(Error::Parse(format!("bad sha256 {sha256}")));
        }
        Ok(format!("{}/{sha256}", self.blobs_prefix()?))
    }

    pub fn state_key(&self) -> Result<String, Error> {
        self.metadata_prefix()
    }
}

impl fmt::Display for RepoTarget {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "{}/{}/{}/{}/{}/{}",
            self.kl_version, self.major, self.minor, self.patch, self.build, self.channel
        )
    }
}

#[derive(Debug, Default)]
pub struct TrustedKeys(Vec<PublicKey>);

impl TrustedKeys {
    pub fn parse(text: &str) -> Result<Self, Error> {
        let mut keys = Vec::new();
        for line in text
            .lines()
            .map(str::trim)
            .filter(|l| !l.is_empty() && !l.starts_with('#'))
        {
            let key = PublicKey::from_openssh(line).map_err(|e| Error::BadKey(e.to_string()))?;
            if key.algorithm() != Algorithm::Ed25519 {
                return Err(Error::BadKey(format!(
                    "unsupported key algorithm {}, expected ssh-ed25519",
                    key.algorithm()
                )));
            }
            let ed = key
                .key_data()
                .ed25519()
                .ok_or_else(|| Error::BadKey("missing ed25519 key material".to_owned()))?;
            let vk = VerifyingKey::from_bytes(&ed.0)
                .map_err(|e| Error::BadKey(format!("invalid ed25519 public key: {e}")))?;
            if vk.is_weak() {
                return Err(Error::BadKey("small-order ed25519 public key".to_owned()));
            }
            keys.push(key);
        }
        Ok(TrustedKeys(keys))
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }

    pub fn verify(&self, namespace: &str, msg: &[u8], sig_pem: &[u8]) -> Result<(), Error> {
        if self.0.is_empty() {
            return Err(Error::NoKeys);
        }
        if namespace.is_empty() {
            return Err(Error::BadSignature("empty SSHSIG namespace".to_owned()));
        }
        let sig = SshSig::from_pem(sig_pem).map_err(|e| Error::BadSignature(e.to_string()))?;
        if sig.version() != SshSig::VERSION {
            return Err(Error::BadSignature(format!(
                "unsupported SSHSIG version {}",
                sig.version()
            )));
        }
        if sig.algorithm() != Algorithm::Ed25519 {
            return Err(Error::BadSignature(format!(
                "unsupported SSHSIG algorithm {}",
                sig.algorithm()
            )));
        }
        if self
            .0
            .iter()
            .any(|k| k.verify(namespace, msg, &sig).is_ok())
        {
            Ok(())
        } else {
            Err(Error::Untrusted)
        }
    }
}

#[derive(Serialize, Deserialize, Clone, Debug, PartialEq, Eq)]
pub struct BlobRef {
    pub sha256: String,
    pub size: u64,
}

impl BlobRef {
    #[must_use]
    pub fn of(bytes: &[u8]) -> Self {
        BlobRef {
            sha256: sha256_hex(bytes),
            size: bytes.len() as u64,
        }
    }

    pub fn check(&self, bytes: &[u8]) -> Result<(), Error> {
        if !valid_sha256(&self.sha256) {
            return Err(Error::Parse(format!("bad sha256 {}", self.sha256)));
        }
        if bytes.len() as u64 != self.size {
            return Err(Error::Size {
                want: self.size,
                got: bytes.len() as u64,
            });
        }
        let got = sha256_hex(bytes);
        if got != self.sha256 {
            return Err(Error::Hash {
                want: self.sha256.clone(),
                got,
            });
        }
        Ok(())
    }
}

#[derive(Serialize, Deserialize, Debug, Clone)]
pub struct Timestamp {
    pub version: u64,
    pub expires: String,
    pub target: RepoTarget,
    pub indexes: BTreeMap<String, BlobRef>,
}

pub fn verify_timestamp(
    keys: &TrustedKeys,
    namespace: &str,
    bytes: &[u8],
    sig: &[u8],
    want_target: &RepoTarget,
    now: OffsetDateTime,
    seen: u64,
) -> Result<Timestamp, Error> {
    want_target.validate()?;
    keys.verify(namespace, bytes, sig)?;
    let ts: Timestamp = serde_json::from_slice(bytes).map_err(|e| Error::Parse(e.to_string()))?;
    ts.target.validate()?;
    if ts.target != *want_target {
        return Err(Error::WrongTarget {
            want: Box::new(want_target.clone()),
            got: Box::new(ts.target),
        });
    }
    if ts.version == 0 {
        return Err(Error::Parse(
            "timestamp version must be at least 1".to_owned(),
        ));
    }
    let expires =
        OffsetDateTime::parse(&ts.expires, &Rfc3339).map_err(|e| Error::Parse(e.to_string()))?;
    if expires <= now {
        return Err(Error::Expired {
            expires: ts.expires,
        });
    }
    if ts.version < seen {
        return Err(Error::Rollback {
            seen,
            got: ts.version,
        });
    }
    for (arch, blob) in &ts.indexes {
        check_arch(arch)?;
        if !valid_sha256(&blob.sha256) {
            return Err(Error::Parse(format!("bad sha256 {}", blob.sha256)));
        }
    }
    Ok(ts)
}

#[must_use]
pub fn sha256_hex(bytes: &[u8]) -> String {
    hex::encode(Sha256::digest(bytes))
}

#[must_use]
pub fn valid_sha256(s: &str) -> bool {
    s.len() == 64
        && s.bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

pub struct HashingReader<R> {
    inner: R,
    hasher: Sha256,
    count: u64,
    limit: u64,
}

impl<R: Read> HashingReader<R> {
    pub fn new(inner: R, limit: u64) -> Self {
        HashingReader {
            inner,
            hasher: Sha256::new(),
            count: 0,
            limit,
        }
    }

    pub fn finish(mut self, want: &BlobRef) -> Result<(), Error> {
        if let Err(e) = io::copy(&mut self, &mut io::sink()) {
            if self.count > self.limit {
                return Err(Error::Size {
                    want: want.size,
                    got: self.count,
                });
            }
            return Err(Error::Parse(e.to_string()));
        }
        if self.count != want.size {
            return Err(Error::Size {
                want: want.size,
                got: self.count,
            });
        }
        let got = hex::encode(self.hasher.finalize());
        if got != want.sha256 {
            return Err(Error::Hash {
                want: want.sha256.clone(),
                got,
            });
        }
        Ok(())
    }
}

impl<R: Read> Read for HashingReader<R> {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        let n = self.inner.read(buf)?;
        self.count = self
            .count
            .checked_add(n as u64)
            .ok_or_else(|| io::Error::other("blob size overflow"))?;
        if self.count > self.limit {
            return Err(io::Error::other("blob is larger than its index entry"));
        }
        self.hasher.update(&buf[..n]);
        Ok(n)
    }
}

pub const CHUNK_SIZE: u64 = 4 << 20;
const CHUNK_MIN: u64 = 64 << 10;
const CHUNK_MAX: u64 = 16 << 20;

#[derive(Serialize, Deserialize, Clone, Debug, PartialEq, Eq)]
pub struct ChunkList {
    pub chunk_size: u64,
    pub chunks: Vec<String>,
}

impl ChunkList {
    #[must_use]
    pub fn of(bytes: &[u8], chunk_size: u64) -> Self {
        let effective = chunk_size.max(1);
        let size = usize::try_from(effective).unwrap_or(usize::MAX);
        ChunkList {
            chunk_size: effective,
            chunks: bytes.chunks(size).map(sha256_hex).collect(),
        }
    }

    #[must_use]
    pub fn whole(blob: &BlobRef) -> Self {
        ChunkList {
            chunk_size: blob.size.max(1),
            chunks: if blob.size == 0 {
                Vec::new()
            } else {
                vec![blob.sha256.clone()]
            },
        }
    }

    pub fn validate(&self, blob: &BlobRef) -> Result<(), Error> {
        if !(CHUNK_MIN..=CHUNK_MAX).contains(&self.chunk_size) {
            return Err(Error::Parse(format!(
                "chunk size {} is out of range",
                self.chunk_size
            )));
        }
        self.validate_coverage(blob)
    }

    fn validate_coverage(&self, blob: &BlobRef) -> Result<(), Error> {
        if !valid_sha256(&blob.sha256) {
            return Err(Error::Parse(format!("bad blob sha256 {}", blob.sha256)));
        }
        if self.chunk_size == 0 || self.chunk_size > CHUNK_MAX {
            return Err(Error::Parse(format!(
                "chunk size {} is out of range",
                self.chunk_size
            )));
        }
        let want = blob.size.div_ceil(self.chunk_size);
        if self.chunks.len() as u64 != want {
            return Err(Error::Parse(format!(
                "{} chunk hashes for {want} chunks",
                self.chunks.len()
            )));
        }
        if let Some(bad) = self.chunks.iter().find(|c| !valid_sha256(c)) {
            return Err(Error::Parse(format!("bad chunk sha256 {bad}")));
        }
        Ok(())
    }
}

pub struct ChunkReader<R> {
    inner: R,
    chunks: Vec<String>,
    chunk_size: u64,
    left: u64,
    next: usize,
    buf: Vec<u8>,
    pos: usize,
    whole: Sha256,
    failed: bool,
}

impl<R: Read> ChunkReader<R> {
    pub fn new(inner: R, list: ChunkList, blob: &BlobRef) -> Result<Self, Error> {
        list.validate_coverage(blob)?;
        Ok(ChunkReader {
            inner,
            chunks: list.chunks,
            chunk_size: list.chunk_size,
            left: blob.size,
            next: 0,
            buf: Vec::new(),
            pos: 0,
            whole: Sha256::new(),
            failed: false,
        })
    }

    pub fn finish(mut self, blob: &BlobRef) -> Result<(), Error> {
        if self.failed {
            return Err(Error::Parse("chunk verification already failed".to_owned()));
        }
        io::copy(&mut self, &mut io::sink()).map_err(|e| Error::Parse(e.to_string()))?;
        if self.failed || self.left != 0 || self.next != self.chunks.len() {
            return Err(Error::Size {
                want: blob.size,
                got: blob.size.saturating_sub(self.left),
            });
        }
        let got = hex::encode(self.whole.finalize());
        if got != blob.sha256 {
            return Err(Error::Hash {
                want: blob.sha256.clone(),
                got,
            });
        }
        Ok(())
    }

    fn fail<T>(&mut self, err: io::Error) -> io::Result<T> {
        self.failed = true;
        self.buf.clear();
        self.pos = 0;
        Err(err)
    }

    fn fill(&mut self) -> io::Result<()> {
        if self.failed {
            return Err(io::Error::other("chunk reader already failed"));
        }
        if self.next == self.chunks.len() {
            if self.left != 0 {
                return self.fail(io::Error::other("chunk list does not cover the blob"));
            }
            let mut extra = [0u8; 1];
            loop {
                match self.inner.read(&mut extra) {
                    Ok(0) => return Ok(()),
                    Ok(_) => {
                        return self
                            .fail(io::Error::other("the blob is longer than its index entry"));
                    }
                    Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
                    Err(e) => return self.fail(e),
                }
            }
        }
        let len = usize::try_from(self.chunk_size.min(self.left)).map_err(io::Error::other)?;
        let mut candidate = vec![0u8; len];
        if let Err(e) = self.inner.read_exact(&mut candidate) {
            let err = if e.kind() == io::ErrorKind::UnexpectedEof {
                io::Error::other("the blob is shorter than its index entry")
            } else {
                e
            };
            return self.fail(err);
        }
        if sha256_hex(&candidate) != self.chunks[self.next] {
            return self.fail(io::Error::other(format!(
                "chunk {} of the blob does not match its hash",
                self.next
            )));
        }
        self.whole.update(&candidate);
        self.left -= len as u64;
        self.next += 1;
        self.buf = candidate;
        self.pos = 0;
        Ok(())
    }
}

impl<R: Read> Read for ChunkReader<R> {
    fn read(&mut self, out: &mut [u8]) -> io::Result<usize> {
        if self.failed {
            return Err(io::Error::other("chunk reader already failed"));
        }
        if self.pos == self.buf.len() {
            self.buf.clear();
            self.pos = 0;
            self.fill()?;
        }
        let n = out.len().min(self.buf.len() - self.pos);
        out[..n].copy_from_slice(&self.buf[self.pos..self.pos + n]);
        self.pos += n;
        Ok(n)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::process::Command;
    use time::Duration;

    struct Signer {
        dir: tempfile::TempDir,
    }

    impl Signer {
        fn new() -> Self {
            let dir = tempfile::tempdir().unwrap();
            for name in ["a", "b"] {
                let ok = Command::new("ssh-keygen")
                    .args(["-q", "-t", "ed25519", "-N", "", "-C", "test", "-f"])
                    .arg(dir.path().join(name))
                    .status()
                    .unwrap()
                    .success();
                assert!(ok);
            }
            Signer { dir }
        }

        fn keys(&self, name: &str) -> TrustedKeys {
            let text =
                std::fs::read_to_string(self.dir.path().join(format!("{name}.pub"))).unwrap();
            TrustedKeys::parse(&text).unwrap()
        }

        fn sign(&self, name: &str, namespace: &str, msg: &[u8]) -> Vec<u8> {
            let file = self.dir.path().join("msg");
            std::fs::write(&file, msg).unwrap();
            let _ = std::fs::remove_file(file.with_extension("sig"));
            let ok = Command::new("ssh-keygen")
                .args(["-q", "-Y", "sign", "-n", namespace, "-f"])
                .arg(self.dir.path().join(name))
                .arg(&file)
                .status()
                .unwrap()
                .success();
            assert!(ok);
            std::fs::read(file.with_extension("sig")).unwrap()
        }
    }

    fn target(os: &str, build: &str, channel: &str) -> RepoTarget {
        RepoTarget::new("v1", OsVersion::parse(os).unwrap(), build, channel).unwrap()
    }

    fn stamp(version: u64, target: RepoTarget, expires: OffsetDateTime) -> Vec<u8> {
        let ts = Timestamp {
            version,
            expires: expires.format(&Rfc3339).unwrap(),
            target,
            indexes: [("arm64e".to_owned(), BlobRef::of(b"index"))].into(),
        };
        serde_json::to_vec(&ts).unwrap()
    }

    #[test]
    fn constructs_locked_v1_paths_for_two_and_three_part_versions() {
        let beta = target("26.1.1", "24B52", "latest");
        let release = target("26.1.1", "24B56", "latest");
        let macos26_5 = target("26.5", "25F79", "latest");
        let macos27_0 = target("27.0", "26B118", "latest");

        assert_eq!(beta.metadata_prefix().unwrap(), "v1/26/1/1/24B52/latest");
        assert_eq!(release.metadata_prefix().unwrap(), "v1/26/1/1/24B56/latest");
        assert_eq!(
            macos26_5.metadata_prefix().unwrap(),
            "v1/26/5/0/25F79/latest"
        );
        assert_eq!(
            macos27_0.metadata_prefix().unwrap(),
            "v1/27/0/0/26B118/latest"
        );

        let digest = "a".repeat(64);
        assert_eq!(
            release.timestamp_path().unwrap(),
            "v1/26/1/1/24B56/latest/timestamp.json"
        );
        assert_eq!(
            release.timestamp_sig_path().unwrap(),
            "v1/26/1/1/24B56/latest/timestamp.json.sig"
        );
        assert_eq!(
            release.index_path("arm64e", &digest).unwrap(),
            format!("v1/26/1/1/24B56/latest/arm64e/index.{digest}.json")
        );
        assert_eq!(
            beta.blob_path(&digest).unwrap(),
            format!("v1/blobs/sha256/{digest}")
        );
        assert_eq!(
            release.blob_path(&digest).unwrap(),
            format!("v1/blobs/sha256/{digest}")
        );
        assert_eq!(
            macos27_0.blob_path(&digest).unwrap(),
            format!("v1/blobs/sha256/{digest}")
        );
    }

    #[test]
    fn rejects_invalid_target_components() {
        for bad in [
            "", "v", "v0", "v01", "V1", "1", "v1/", "../v1", "v1 ", "v+1", "v-1",
        ] {
            assert!(
                check_kl_version(bad).is_err(),
                "kl_version {bad:?} was accepted"
            );
        }
        for good in ["v1", "v2", "v10"] {
            check_kl_version(good).unwrap();
        }

        for bad in [
            "",
            "26",
            "26.1.1.1",
            "26..1",
            ".26.1",
            "26.1.",
            "+26.1",
            "-1.0",
            "26.01",
            "026.5",
            "26/1/1",
            "26.5 ",
            "99999999999999.0",
        ] {
            assert!(
                OsVersion::parse(bad).is_err(),
                "os_version {bad:?} was accepted"
            );
        }

        for bad in [
            "",
            ".",
            "..",
            ".24B52",
            "../24B52",
            "24B52/x",
            "24B52\\x",
            "24B 52",
            "24B52?",
            &"A".repeat(65),
        ] {
            assert!(check_build(bad).is_err(), "build {bad:?} was accepted");
        }
        for good in [
            "24B52",
            "24B56",
            "25F79",
            "25F79a",
            "26B118",
            "25F79_1.2-rc1",
        ] {
            check_build(good).unwrap();
        }

        for bad in ["", "beta", "LATEST", "../latest", "latest/x", "a b"] {
            assert!(check_channel(bad).is_err(), "channel {bad:?} was accepted");
        }
        for good in ["latest", "nightly"] {
            check_channel(good).unwrap();
        }

        for bad in ["", "x86_64", "ARM64", "../arm64"] {
            assert!(check_arch(bad).is_err(), "arch {bad:?} was accepted");
        }
    }

    #[test]
    fn accepts_a_fresh_signed_timestamp_and_refuses_every_variation() {
        let s = Signer::new();
        let now = OffsetDateTime::now_utc();
        let want = target("26.1.1", "24B56", "latest");
        let good = stamp(7, want.clone(), now + Duration::days(1));
        let sig = s.sign("a", "kl@kido.sh", &good);
        let keys = s.keys("a");
        let check = |bytes: &[u8], sig: &[u8], keys: &TrustedKeys, seen| {
            verify_timestamp(keys, "kl@kido.sh", bytes, sig, &want, now, seen)
        };
        assert_eq!(check(&good, &sig, &keys, 7).unwrap().version, 7);
        assert!(matches!(
            check(&good, &sig, &s.keys("b"), 0),
            Err(Error::Untrusted)
        ));
        assert!(matches!(
            check(&good, &sig, &TrustedKeys::default(), 0),
            Err(Error::NoKeys)
        ));
        assert!(matches!(
            check(&good, &sig, &keys, 8),
            Err(Error::Rollback { seen: 8, got: 7 })
        ));
        let mut changed = good.clone();
        changed[0] = b' ';
        assert!(matches!(
            check(&changed, &sig, &keys, 0),
            Err(Error::Untrusted)
        ));
        let other_ns = s.sign("a", "image@kido.sh", &good);
        assert!(matches!(
            check(&good, &other_ns, &keys, 0),
            Err(Error::Untrusted)
        ));
        let old = stamp(7, want.clone(), now - Duration::seconds(1));
        assert!(matches!(
            check(&old, &s.sign("a", "kl@kido.sh", &old), &keys, 0),
            Err(Error::Expired { .. })
        ));
        let nightly = stamp(
            7,
            target("26.1.1", "24B56", "nightly"),
            now + Duration::days(1),
        );
        let nightly_sig = s.sign("a", "kl@kido.sh", &nightly);
        assert!(matches!(
            check(&nightly, &nightly_sig, &keys, 0),
            Err(Error::WrongTarget { .. })
        ));
        let beta_build = stamp(
            7,
            target("26.1.1", "24B52", "latest"),
            now + Duration::days(1),
        );
        let beta_sig = s.sign("a", "kl@kido.sh", &beta_build);
        assert!(matches!(
            check(&beta_build, &beta_sig, &keys, 0),
            Err(Error::WrongTarget { .. })
        ));
        let diff_os = stamp(
            7,
            target("26.5", "24B56", "latest"),
            now + Duration::days(1),
        );
        let diff_os_sig = s.sign("a", "kl@kido.sh", &diff_os);
        assert!(matches!(
            check(&diff_os, &diff_os_sig, &keys, 0),
            Err(Error::WrongTarget { .. })
        ));
        let other_ver = stamp(
            7,
            RepoTarget::new("v2", OsVersion::parse("26.1.1").unwrap(), "24B56", "latest").unwrap(),
            now + Duration::days(1),
        );
        let other_ver_sig = s.sign("a", "kl@kido.sh", &other_ver);
        assert!(matches!(
            check(&other_ver, &other_ver_sig, &keys, 0),
            Err(Error::WrongTarget { .. })
        ));
        let zero = stamp(0, want.clone(), now + Duration::days(1));
        let zero_sig = s.sign("a", "kl@kido.sh", &zero);
        assert!(matches!(
            check(&zero, &zero_sig, &keys, 0),
            Err(Error::Parse(_))
        ));
    }

    #[test]
    fn hashing_reader_checks_size_and_hash() {
        let data = b"some bytes";
        let want = BlobRef::of(data);
        HashingReader::new(&data[..], want.size)
            .finish(&want)
            .unwrap();
        let other = BlobRef::of(b"some bytez");
        assert!(matches!(
            HashingReader::new(&data[..], want.size).finish(&other),
            Err(Error::Hash { .. })
        ));
        let mut long = HashingReader::new(&b"some bytes and more"[..], want.size);
        assert!(io::copy(&mut long, &mut io::sink()).is_err());
        let short = BlobRef { size: 11, ..want };
        assert!(matches!(
            HashingReader::new(&data[..], 11).finish(&short),
            Err(Error::Size { .. })
        ));
    }

    #[test]
    fn sha256_strings_must_be_lowercase_hex() {
        assert!(valid_sha256(&"a".repeat(64)));
        assert!(!valid_sha256(&"A".repeat(64)));
        assert!(!valid_sha256(&"a".repeat(63)));
        assert!(!valid_sha256(&format!("../{}", "a".repeat(61))));
    }

    struct Counting<R> {
        inner: R,
        total: usize,
    }

    impl<R: Read> Read for Counting<R> {
        fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
            let n = self.inner.read(buf)?;
            self.total += n;
            Ok(n)
        }
    }

    fn data(len: usize) -> Vec<u8> {
        (0..len)
            .map(|i| u8::try_from(i * 7 % 251).unwrap())
            .collect()
    }

    #[test]
    fn chunk_reader_releases_only_verified_chunks_and_poisons_after_error() {
        let chunk = 64 << 10;
        let good = data(3 * chunk + 100);
        let blob = BlobRef::of(&good);
        let list = ChunkList::of(&good, chunk as u64);
        list.validate(&blob).unwrap();
        let mut out = Vec::new();
        let mut reader = ChunkReader::new(good.as_slice(), list.clone(), &blob).unwrap();
        reader.read_to_end(&mut out).unwrap();
        assert_eq!(out, good);
        reader.finish(&blob).unwrap();

        let mut bad = good.clone();
        bad[chunk + 5] ^= 1;
        let mut source = Counting {
            inner: bad.as_slice(),
            total: 0,
        };
        let mut reader = ChunkReader::new(&mut source, list, &blob).unwrap();
        let mut out = Vec::new();
        let err = reader.read_to_end(&mut out).unwrap_err();
        assert!(err.to_string().contains("chunk 1 of the blob"), "{err}");
        assert_eq!(out.len(), chunk);
        // Subsequent reads MUST remain poisoned and release ZERO unverified bytes.
        let mut retry = [0u8; 64];
        assert!(reader.read(&mut retry).is_err());
        assert!(reader.finish(&blob).is_err());
        assert_eq!(source.total, 2 * chunk);
    }

    #[test]
    fn chunk_reader_refuses_short_and_long_blobs_and_bad_lists() {
        let chunk = 64 << 10;
        let good = data(2 * chunk);
        let blob = BlobRef::of(&good);
        let list = ChunkList::of(&good, chunk as u64);
        let mut sink = Vec::new();
        let short = &good[..good.len() - 1];
        assert!(
            ChunkReader::new(short, list.clone(), &blob)
                .unwrap()
                .read_to_end(&mut sink)
                .is_err()
        );
        let mut long = good.clone();
        long.push(0);
        sink.clear();
        assert!(
            ChunkReader::new(long.as_slice(), list.clone(), &blob)
                .unwrap()
                .read_to_end(&mut sink)
                .is_err()
        );
        let missing = ChunkList {
            chunk_size: chunk as u64,
            chunks: list.chunks[..1].to_vec(),
        };
        assert!(missing.validate(&blob).is_err());
        assert!(ChunkReader::new(good.as_slice(), missing, &blob).is_err());
        let tiny = ChunkList {
            chunk_size: 1,
            chunks: list.chunks.clone(),
        };
        assert!(tiny.validate(&blob).is_err());
    }
}
