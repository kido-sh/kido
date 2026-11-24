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
}
