use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use ssh_key::{PublicKey, SshSig};
use std::collections::BTreeMap;
use std::fmt;
use std::io::{self, Read};
use time::OffsetDateTime;
use time::format_description::well_known::Rfc3339;

#[derive(Debug)]
pub enum Error {
    NoKeys,
    BadKey(String),
    BadSignature(String),
    Untrusted,
    Expired { expires: String },
    Rollback { seen: u64, got: u64 },
    WrongChannel { want: String, got: String },
    Hash { want: String, got: String },
    Size { want: u64, got: u64 },
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
            Error::WrongChannel { want, got } => {
                write!(f, "metadata is for channel {got}, not {want}")
            }
            Error::Hash { want, got } => write!(f, "sha256 {got} does not match {want}"),
            Error::Size { want, got } => write!(f, "size {got} does not match {want}"),
            Error::Parse(e) => write!(f, "cannot parse signed metadata: {e}"),
        }
    }
}

impl std::error::Error for Error {}

#[derive(Debug, Default)]
pub struct TrustedKeys(Vec<PublicKey>);

impl TrustedKeys {
    pub fn parse(text: &str) -> Result<Self, Error> {
        let keys = text
            .lines()
            .map(str::trim)
            .filter(|l| !l.is_empty() && !l.starts_with('#'))
            .map(|l| PublicKey::from_openssh(l).map_err(|e| Error::BadKey(e.to_string())))
            .collect::<Result<Vec<_>, _>>()?;
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
        let sig = SshSig::from_pem(sig_pem).map_err(|e| Error::BadSignature(e.to_string()))?;
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
    pub channel: String,
    pub indexes: BTreeMap<String, BlobRef>,
}

pub fn verify_timestamp(
    keys: &TrustedKeys,
    namespace: &str,
    bytes: &[u8],
    sig: &[u8],
    channel: &str,
    now: OffsetDateTime,
    seen: u64,
) -> Result<Timestamp, Error> {
    keys.verify(namespace, bytes, sig)?;
    let ts: Timestamp = serde_json::from_slice(bytes).map_err(|e| Error::Parse(e.to_string()))?;
    if ts.channel != channel {
        return Err(Error::WrongChannel {
            want: channel.to_owned(),
            got: ts.channel,
        });
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
    for blob in ts.indexes.values() {
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
        io::copy(&mut self, &mut io::sink()).map_err(|_| Error::Size {
            want: want.size,
            got: self.count,
        })?;
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
        self.count += n as u64;
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
        let size = usize::try_from(chunk_size).unwrap_or(usize::MAX);
        ChunkList {
            chunk_size,
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
}

impl<R: Read> ChunkReader<R> {
    pub fn new(inner: R, list: ChunkList, blob: &BlobRef) -> Self {
        ChunkReader {
            inner,
            chunks: list.chunks,
            chunk_size: list.chunk_size,
            left: blob.size,
            next: 0,
            buf: Vec::new(),
            pos: 0,
            whole: Sha256::new(),
        }
    }

    pub fn finish(mut self, blob: &BlobRef) -> Result<(), Error> {
        io::copy(&mut self, &mut io::sink()).map_err(|e| Error::Parse(e.to_string()))?;
        let got = hex::encode(self.whole.finalize());
        if got != blob.sha256 {
            return Err(Error::Hash {
                want: blob.sha256.clone(),
                got,
            });
        }
        Ok(())
    }

    fn fill(&mut self) -> io::Result<()> {
        if self.next == self.chunks.len() {
            let mut extra = [0u8; 1];
            if self.inner.read(&mut extra)? != 0 {
                return Err(io::Error::other("the blob is longer than its index entry"));
            }
            return Ok(());
        }
        let len = usize::try_from(self.chunk_size.min(self.left)).map_err(io::Error::other)?;
        self.buf.resize(len, 0);
        self.inner.read_exact(&mut self.buf).map_err(|e| {
            if e.kind() == io::ErrorKind::UnexpectedEof {
                io::Error::other("the blob is shorter than its index entry")
            } else {
                e
            }
        })?;
        if sha256_hex(&self.buf) != self.chunks[self.next] {
            return Err(io::Error::other(format!(
                "chunk {} of the blob does not match its hash",
                self.next
            )));
        }
        self.whole.update(&self.buf);
        self.left -= len as u64;
        self.next += 1;
        self.pos = 0;
        Ok(())
    }
}

impl<R: Read> Read for ChunkReader<R> {
    fn read(&mut self, out: &mut [u8]) -> io::Result<usize> {
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

    fn stamp(version: u64, channel: &str, expires: OffsetDateTime) -> Vec<u8> {
        let ts = Timestamp {
            version,
            expires: expires.format(&Rfc3339).unwrap(),
            channel: channel.to_owned(),
            indexes: [("arm64e".to_owned(), BlobRef::of(b"index"))].into(),
        };
        serde_json::to_vec(&ts).unwrap()
    }

    #[test]
    fn accepts_a_fresh_signed_timestamp_and_refuses_every_variation() {
        let s = Signer::new();
        let now = OffsetDateTime::now_utc();
        let good = stamp(7, "latest", now + Duration::days(1));
        let sig = s.sign("a", "kl@kido.sh", &good);
        let keys = s.keys("a");
        let check = |bytes: &[u8], sig: &[u8], keys: &TrustedKeys, seen| {
            verify_timestamp(keys, "kl@kido.sh", bytes, sig, "latest", now, seen)
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
        let old = stamp(7, "latest", now - Duration::seconds(1));
        assert!(matches!(
            check(&old, &s.sign("a", "kl@kido.sh", &old), &keys, 0),
            Err(Error::Expired { .. })
        ));
        let nightly = stamp(7, "nightly", now + Duration::days(1));
        let nightly_sig = s.sign("a", "kl@kido.sh", &nightly);
        assert!(matches!(
            check(&nightly, &nightly_sig, &keys, 0),
            Err(Error::WrongChannel { .. })
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
    fn chunk_reader_releases_only_verified_chunks_and_stops_at_the_first_bad_one() {
        let chunk = 64 << 10;
        let good = data(3 * chunk + 100);
        let blob = BlobRef::of(&good);
        let list = ChunkList::of(&good, chunk as u64);
        list.validate(&blob).unwrap();
        let mut out = Vec::new();
        let mut reader = ChunkReader::new(good.as_slice(), list.clone(), &blob);
        reader.read_to_end(&mut out).unwrap();
        assert_eq!(out, good);
        reader.finish(&blob).unwrap();

        let mut bad = good.clone();
        bad[chunk + 5] ^= 1;
        let mut source = Counting {
            inner: bad.as_slice(),
            total: 0,
        };
        let mut reader = ChunkReader::new(&mut source, list, &blob);
        let mut out = Vec::new();
        let err = reader.read_to_end(&mut out).unwrap_err();
        assert!(err.to_string().contains("chunk 1 of the blob"), "{err}");
        assert_eq!(out.len(), chunk);
        drop(reader);
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
                .read_to_end(&mut sink)
                .is_err()
        );
        let mut long = good.clone();
        long.push(0);
        sink.clear();
        assert!(
            ChunkReader::new(long.as_slice(), list.clone(), &blob)
                .read_to_end(&mut sink)
                .is_err()
        );
        let missing = ChunkList {
            chunk_size: chunk as u64,
            chunks: list.chunks[..1].to_vec(),
        };
        assert!(missing.validate(&blob).is_err());
        let tiny = ChunkList {
            chunk_size: 1,
            chunks: list.chunks.clone(),
        };
        assert!(tiny.validate(&blob).is_err());
    }
}
