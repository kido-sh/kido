use std::fmt;
use std::io::{self, BufRead, BufReader, Read, Write};
use std::net::TcpStream;
use std::sync::Arc;
use std::time::{Duration, Instant};

use base64::Engine as _;
use base64::engine::general_purpose::{STANDARD as B64_STD, URL_SAFE_NO_PAD as B64_URL};
use ed25519_dalek::{Signature, Signer, SigningKey, VerifyingKey};
use hkdf::Hkdf;
use hmac::{Hmac, Mac};
use kido_aead::Cipher;
use rand_core::{OsRng, RngCore};
use serde::{Deserialize, Deserializer, Serialize, Serializer};
use serde_json::value::RawValue;
use sha2::{Digest, Sha256};
use x25519_dalek::{EphemeralSecret, PublicKey as X25519PublicKey};
use zeroize::{Zeroize, Zeroizing};

pub const VERSION: &str = "2";
pub const DEFAULT_PORT: u16 = 1023;
pub const MAX_LINE: usize = 1 << 16;
pub const MAX_FRAME: usize = 1 << 20;
pub const NONCE_SIZE: usize = 32;
pub const FRAME_HEADER_SIZE: usize = 5;
pub const GCM_TAG_SIZE: usize = 16;

pub const OP_HELLO: &str = "hello";
pub const OP_PING: &str = "ping";
pub const OP_EXEC: &str = "exec";
pub const OP_SHUTDOWN: &str = "shutdown";
pub const OP_NOTIFY: &str = "notify";

pub const AGENT_DOMAIN: &[u8] = b"kido-auth-v2 agent\x00";
pub const REQUEST_DOMAIN: &[u8] = b"kido-auth-v2 request\x00";
pub const TRANSCRIPT_DOMAIN: &[u8] = b"kido-aead-v1 transcript\x00";
pub const KEY_TO_AGENT: &[u8] = b"kido-aead-v1 client to agent";
pub const KEY_TO_CLIENT: &[u8] = b"kido-aead-v1 agent to client";
#[cfg(test)]
pub const BOOT_SECRET_DOMAIN: &[u8] = b"kido-boot-secret-v1\x00";
pub const BOOT_PROOF_DOMAIN: &[u8] = b"kido-boot-proof-v1\x00";

pub const NEEDS_NEWER_CLI: &str = "this kido-agent needs a newer kido; upgrade the kido CLI";
pub const ERR_UNAUTHORIZED: &str = "unauthorized";
pub const ERR_BAD_REQUEST: &str = "bad request";

pub const FRAME_STDIN: u8 = 0;
pub const FRAME_STDOUT: u8 = 1;
pub const FRAME_STDERR: u8 = 2;
pub const FRAME_EXIT: u8 = 3;
pub const FRAME_CLOSE_STDIN: u8 = 4;
pub const FRAME_SIGNAL: u8 = 5;
pub const FRAME_RESIZE: u8 = 6;
pub const FRAME_REQUEST: u8 = 7;
pub const FRAME_RESPONSE: u8 = 8;
pub const FRAME_NOTIFY: u8 = 9;

type HmacSha256 = Hmac<Sha256>;

fn de_null_default<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

fn de_opt_b64<'de, D>(d: D) -> Result<Vec<u8>, D::Error>
where
    D: Deserializer<'de>,
{
    match Option::<String>::deserialize(d)? {
        None => Ok(Vec::new()),
        Some(s) => B64_STD.decode(s.as_bytes()).map_err(serde::de::Error::custom),
    }
}

fn ser_opt_b64<S>(v: &Option<Vec<u8>>, s: S) -> Result<S::Ok, S::Error>
where
    S: Serializer,
{
    match v {
        Some(b) => s.serialize_str(&B64_STD.encode(b)),
        None => s.serialize_none(),
    }
}

fn is_zero_i32(v: &i32) -> bool {
    *v == 0
}

fn is_zero_u16(v: &u16) -> bool {
    *v == 0
}

#[derive(Debug, Deserialize)]
pub struct Hello {
    #[serde(default, deserialize_with = "de_null_default")]
    pub op: String,
    #[serde(default, deserialize_with = "de_opt_b64")]
    pub nonce: Vec<u8>,
    #[serde(default, deserialize_with = "de_opt_b64")]
    pub eph: Vec<u8>,
}

#[derive(Debug, Serialize)]
pub struct Challenge {
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none", serialize_with = "ser_opt_b64")]
    pub nonce: Option<Vec<u8>>,
    #[serde(skip_serializing_if = "Option::is_none", serialize_with = "ser_opt_b64")]
    pub key: Option<Vec<u8>>,
    #[serde(skip_serializing_if = "Option::is_none", serialize_with = "ser_opt_b64")]
    pub eph: Option<Vec<u8>>,
    #[serde(skip_serializing_if = "Option::is_none", serialize_with = "ser_opt_b64")]
    pub sig: Option<Vec<u8>>,
    #[serde(skip_serializing_if = "Option::is_none", serialize_with = "ser_opt_b64")]
    pub proof: Option<Vec<u8>>,
}

#[derive(Debug, Deserialize)]
struct SignedRequest<'a> {
    #[serde(default, deserialize_with = "de_opt_b64")]
    sig: Vec<u8>,
    #[serde(borrow, default)]
    req: Option<&'a RawValue>,
}

#[derive(Clone, Default, Deserialize, Serialize)]
pub struct Request {
    #[serde(default, deserialize_with = "de_null_default")]
    pub op: String,
    #[serde(default, deserialize_with = "de_null_default", skip_serializing_if = "Vec::is_empty")]
    pub args: Vec<String>,
    #[serde(default, deserialize_with = "de_null_default", skip_serializing_if = "Vec::is_empty")]
    pub env: Vec<String>,
    #[serde(default, deserialize_with = "de_null_default", skip_serializing_if = "String::is_empty")]
    pub dir: String,
    #[serde(default, deserialize_with = "de_null_default", skip_serializing_if = "String::is_empty")]
    pub user: String,
    #[serde(default, deserialize_with = "de_null_default", skip_serializing_if = "std::ops::Not::not")]
    pub tty: bool,
    #[serde(default, deserialize_with = "de_null_default", skip_serializing_if = "is_zero_u16")]
    pub rows: u16,
    #[serde(default, deserialize_with = "de_null_default", skip_serializing_if = "is_zero_u16")]
    pub cols: u16,
}

impl fmt::Debug for Request {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Request")
            .field("op", &self.op)
            .field("args", &self.args)
            .field("env_count", &self.env.len())
            .field("dir", &self.dir)
            .field("user", &self.user)
            .field("tty", &self.tty)
            .field("rows", &self.rows)
            .field("cols", &self.cols)
            .finish()
    }
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct Response {
    pub ok: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub version: String,
    #[serde(skip_serializing_if = "is_zero_i32")]
    pub pid: i32,
}

impl Response {
    pub fn ok_version() -> Self {
        Self {
            ok: true,
            version: VERSION.to_string(),
            ..Default::default()
        }
    }

    pub fn ok_pid(pid: i32) -> Self {
        Self {
            ok: true,
            pid,
            ..Default::default()
        }
    }

    pub fn err(msg: impl Into<String>) -> Self {
        Self {
            ok: false,
            error: msg.into(),
            ..Default::default()
        }
    }
}

pub fn parse_key(s: &str) -> Result<VerifyingKey, String> {
    let raw = B64_URL
        .decode(s.as_bytes())
        .map_err(|_| "not a base64url ed25519 public key".to_string())?;
    let arr: [u8; 32] = raw
        .try_into()
        .map_err(|_| "not a base64url ed25519 public key".to_string())?;
    VerifyingKey::from_bytes(&arr).map_err(|_| "not a base64url ed25519 public key".to_string())
}

pub fn parse_secret(s: &str) -> Result<[u8; 32], String> {
    let raw = Zeroizing::new(
        B64_URL
            .decode(s.as_bytes())
            .map_err(|_| "not a base64url boot secret".to_string())?,
    );
    raw.as_slice()
        .try_into()
        .map_err(|_| "not a base64url boot secret".to_string())
}

#[cfg(test)]
pub fn boot_secret(seed: &[u8; 32]) -> [u8; 32] {
    let mut mac = <HmacSha256 as Mac>::new_from_slice(seed).expect("hmac accepts any key size");
    mac.update(BOOT_SECRET_DOMAIN);
    mac.finalize().into_bytes().into()
}

pub fn boot_proof(secret: &[u8; 32], transcript: &[u8; 32]) -> [u8; 32] {
    let mut mac = <HmacSha256 as Mac>::new_from_slice(secret).expect("hmac accepts any key size");
    mac.update(BOOT_PROOF_DOMAIN);
    mac.update(transcript);
    mac.finalize().into_bytes().into()
}

pub fn transcript_hash(
    client_nonce: &[u8],
    client_eph: &[u8],
    agent_nonce: &[u8],
    agent_eph: &[u8],
    agent_key: &[u8],
) -> [u8; 32] {
    let mut h = Sha256::new();
    h.update(TRANSCRIPT_DOMAIN);
    h.update(client_nonce);
    h.update(client_eph);
    h.update(agent_nonce);
    h.update(agent_eph);
    h.update(agent_key);
    h.finalize().into()
}

pub fn exit_payload(status: i32) -> [u8; 4] {
    (status as u32).to_be_bytes()
}

pub fn parse_resize(p: &[u8]) -> Option<(u16, u16)> {
    if p.len() != 4 {
        return None;
    }
    Some((
        u16::from_be_bytes([p[0], p[1]]),
        u16::from_be_bytes([p[2], p[3]]),
    ))
}

pub fn write_line<W: Write, T: Serialize>(mut w: W, v: &T) -> io::Result<()> {
    let mut buf = serde_json::to_vec(v)
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
    if buf.len() >= MAX_LINE {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!("proto: line of {} bytes exceeds {}", buf.len(), MAX_LINE),
        ));
    }
    buf.push(b'\n');
    w.write_all(&buf)
}

pub fn read_line_bounded<R: BufRead>(r: &mut R) -> io::Result<Vec<u8>> {
    let mut line = Vec::new();
    loop {
        let buf = match r.fill_buf() {
            Ok(b) => b,
            Err(ref e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        };
        if buf.is_empty() {
            if line.is_empty() {
                return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "eof"));
            }
            if line.last() == Some(&b'\r') {
                line.pop();
            }
            if line.len() >= MAX_LINE {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    format!("proto: line exceeds {} bytes", MAX_LINE),
                ));
            }
            return Ok(line);
        }
        if let Some(pos) = buf.iter().position(|&b| b == b'\n') {
            let chunk = &buf[..pos];
            let has_cr = if let Some(&last) = chunk.last() {
                last == b'\r'
            } else {
                line.last() == Some(&b'\r')
            };
            let content_len = line.len() + pos - usize::from(has_cr);
            if content_len >= MAX_LINE {
                r.consume(pos + 1);
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    format!("proto: line exceeds {} bytes", MAX_LINE),
                ));
            }
            line.extend_from_slice(chunk);
            r.consume(pos + 1);
            if has_cr {
                line.pop();
            }
            return Ok(line);
        }
        let n = buf.len();
        let new_len = line.len() + n;
        if new_len > MAX_LINE || (new_len == MAX_LINE && buf.last() != Some(&b'\r')) {
            r.consume(n);
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("proto: line exceeds {} bytes", MAX_LINE),
            ));
        }
        line.extend_from_slice(buf);
        r.consume(n);
    }
}

#[derive(Debug)]
pub enum FrameError {
    Eof,
    TooLarge,
    Auth,
    Sequence,
    Protocol(String),
    Io(io::Error),
}

impl FrameError {
    fn clone_sticky(&self) -> Self {
        match self {
            FrameError::Eof => FrameError::Eof,
            FrameError::TooLarge => FrameError::TooLarge,
            FrameError::Auth => FrameError::Auth,
            FrameError::Sequence => FrameError::Sequence,
            FrameError::Protocol(s) => FrameError::Protocol(s.clone()),
            FrameError::Io(e) => FrameError::Io(io::Error::new(e.kind(), e.to_string())),
        }
    }
}

impl fmt::Display for FrameError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            FrameError::Eof => write!(f, "EOF"),
            FrameError::TooLarge => write!(f, "proto: frame too large"),
            FrameError::Auth => write!(
                f,
                "proto: a frame failed authentication; the connection was tampered with"
            ),
            FrameError::Sequence => write!(f, "proto: frame sequence numbers exhausted"),
            FrameError::Protocol(s) => write!(f, "{s}"),
            FrameError::Io(e) => write!(f, "{e}"),
        }
    }
}

#[derive(Debug)]
pub enum ReadRequestError {
    Unauthorized,
    BadRequest(String),
    Channel(FrameError),
}

impl fmt::Display for ReadRequestError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            ReadRequestError::Unauthorized => write!(f, "{ERR_UNAUTHORIZED}"),
            ReadRequestError::BadRequest(s) => write!(f, "{ERR_BAD_REQUEST}: {s}"),
            ReadRequestError::Channel(e) => write!(f, "bad request: {e}"),
        }
    }
}

fn seq_nonce(seq: u64) -> [u8; 12] {
    let mut n = [0u8; 12];
    n[4..].copy_from_slice(&seq.to_be_bytes());
    n
}

fn direction_aead(secret: &[u8], transcript: &[u8; 32], info: &[u8]) -> Cipher {
    let hk = Hkdf::<Sha256>::new(Some(transcript), secret);
    let mut key = [0u8; 32];
    hk.expand(info, &mut key)
        .expect("32-byte HKDF-SHA256 output is valid");
    let cipher = Cipher::new(&key);
    key.zeroize();
    cipher
}

pub struct ChannelWriter<W> {
    w: W,
    seal: Cipher,
    wseq: u64,
    wbuf: Vec<u8>,
    werr: Option<FrameError>,
}

impl<W> ChannelWriter<W> {
    pub fn writer_mut(&mut self) -> &mut W {
        &mut self.w
    }
}

impl<W: Write> ChannelWriter<W> {
    pub fn write_frame(&mut self, typ: u8, payload: &[u8]) -> Result<(), FrameError> {
        if payload.len() > MAX_FRAME {
            return Err(FrameError::TooLarge);
        }
        if let Some(ref err) = self.werr {
            return Err(err.clone_sticky());
        }
        if self.wseq == u64::MAX {
            self.werr = Some(FrameError::Sequence);
            return Err(FrameError::Sequence);
        }
        let mut hdr = [0u8; FRAME_HEADER_SIZE];
        hdr[0] = typ;
        hdr[1..5].copy_from_slice(&(payload.len() as u32).to_be_bytes());

        let total = FRAME_HEADER_SIZE + payload.len() + GCM_TAG_SIZE;
        self.wbuf.clear();
        self.wbuf.reserve(total);
        self.wbuf.extend_from_slice(&hdr);
        self.wbuf.extend_from_slice(payload);

        let nonce = seq_nonce(self.wseq);
        let tag = self
            .seal
            .encrypt_in_place_detached(&nonce, &hdr, &mut self.wbuf[FRAME_HEADER_SIZE..])
            .map_err(|_| {
                self.werr = Some(FrameError::Auth);
                FrameError::Auth
            })?;
        self.wbuf.extend_from_slice(&tag);
        self.wseq += 1;

        if let Err(e) = self.w.write_all(&self.wbuf) {
            let fe = FrameError::Io(e);
            self.werr = Some(fe.clone_sticky());
            return Err(fe);
        }
        Ok(())
    }

    pub fn write_message<T: Serialize>(&mut self, typ: u8, v: &T) -> Result<(), FrameError> {
        let b = serde_json::to_vec(v)
            .map_err(|e| FrameError::Protocol(format!("marshal: {e}")))?;
        if b.len() >= MAX_LINE {
            return Err(FrameError::Protocol(format!(
                "proto: message of {} bytes exceeds {}",
                b.len(),
                MAX_LINE
            )));
        }
        self.write_frame(typ, &b)
    }
}

pub struct ChannelReader<R> {
    r: R,
    open: Cipher,
    rseq: u64,
    rerr: Option<FrameError>,
    transcript: [u8; 32],
}

impl<R> ChannelReader<R> {
    pub fn reader_mut(&mut self) -> &mut R {
        &mut self.r
    }
}

fn read_header_exact<R: Read>(r: &mut R, hdr: &mut [u8; FRAME_HEADER_SIZE]) -> Result<(), FrameError> {
    let mut off = 0;
    while off < FRAME_HEADER_SIZE {
        match r.read(&mut hdr[off..]) {
            Ok(0) => {
                if off == 0 {
                    return Err(FrameError::Eof);
                }
                return Err(FrameError::Io(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "unexpected EOF in frame header",
                )));
            }
            Ok(n) => off += n,
            Err(ref e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(FrameError::Io(e)),
        }
    }
    Ok(())
}

impl<R: Read> ChannelReader<R> {
    pub fn read_frame(&mut self) -> Result<(u8, Vec<u8>), FrameError> {
        self.read_frame_max(MAX_FRAME)
    }

    pub fn read_frame_max(&mut self, max: usize) -> Result<(u8, Vec<u8>), FrameError> {
        if let Some(ref err) = self.rerr {
            return Err(err.clone_sticky());
        }
        match self.open_next(max) {
            Ok(v) => Ok(v),
            Err(e) => {
                self.rerr = Some(e.clone_sticky());
                Err(e)
            }
        }
    }

    fn open_next(&mut self, max: usize) -> Result<(u8, Vec<u8>), FrameError> {
        let mut hdr = [0u8; FRAME_HEADER_SIZE];
        read_header_exact(&mut self.r, &mut hdr)?;
        let n = u32::from_be_bytes([hdr[1], hdr[2], hdr[3], hdr[4]]) as usize;
        if n > max {
            return Err(FrameError::TooLarge);
        }
        if self.rseq == u64::MAX {
            return Err(FrameError::Sequence);
        }
        let mut buf = vec![0u8; n + GCM_TAG_SIZE];
        self.r.read_exact(&mut buf).map_err(FrameError::Io)?;
        let (ct, tag_slice) = buf.split_at_mut(n);
        let tag: &[u8; 16] = (&*tag_slice).try_into().expect("16-byte GCM tag");
        let nonce = seq_nonce(self.rseq);
        self.open
            .decrypt_in_place_detached(&nonce, &hdr, ct, tag)
            .map_err(|_| FrameError::Auth)?;
        buf.truncate(n);
        self.rseq += 1;
        Ok((hdr[0], buf))
    }

    pub fn read_request(&mut self, host: &VerifyingKey) -> Result<Request, ReadRequestError> {
        let (typ, payload) = self
            .read_frame_max(MAX_LINE - 1)
            .map_err(ReadRequestError::Channel)?;
        if typ != FRAME_REQUEST {
            let err = FrameError::Protocol(format!("proto: got frame type {}, want a request", typ));
            self.rerr = Some(err.clone_sticky());
            return Err(ReadRequestError::Channel(err));
        }
        let sr: SignedRequest<'_> = serde_json::from_slice(&payload)
            .map_err(|e| ReadRequestError::BadRequest(e.to_string()))?;
        let raw_req = match sr.req {
            Some(r) if !r.get().is_empty() => r.get().as_bytes(),
            _ => return Err(ReadRequestError::Unauthorized),
        };
        let sig_arr: [u8; 64] = match sr.sig.try_into() {
            Ok(a) => a,
            Err(_) => return Err(ReadRequestError::Unauthorized),
        };
        let sig = Signature::from_bytes(&sig_arr);
        let mut msg = Vec::with_capacity(REQUEST_DOMAIN.len() + 32 + raw_req.len());
        msg.extend_from_slice(REQUEST_DOMAIN);
        msg.extend_from_slice(&self.transcript);
        msg.extend_from_slice(raw_req);
        if host.verify_strict(&msg, &sig).is_err() {
            return Err(ReadRequestError::Unauthorized);
        }
        serde_json::from_slice::<Request>(raw_req)
            .map_err(|e| ReadRequestError::BadRequest(e.to_string()))
    }

    pub fn into_reader(self) -> (R, Cipher, u64, Option<FrameError>, [u8; 32]) {
        (self.r, self.open, self.rseq, self.rerr, self.transcript)
    }

    pub fn from_parts(
        r: R,
        open: Cipher,
        rseq: u64,
        rerr: Option<FrameError>,
        transcript: [u8; 32],
    ) -> Self {
        Self {
            r,
            open,
            rseq,
            rerr,
            transcript,
        }
    }
}

pub fn new_channel_pair<W, R>(
    secret: &[u8],
    transcript: [u8; 32],
    is_client: bool,
    w: W,
    r: R,
) -> (ChannelWriter<W>, ChannelReader<R>) {
    let to_agent = direction_aead(secret, &transcript, KEY_TO_AGENT);
    let to_client = direction_aead(secret, &transcript, KEY_TO_CLIENT);
    let (seal, open) = if is_client {
        (to_agent, to_client)
    } else {
        (to_client, to_agent)
    };
    (
        ChannelWriter {
            w,
            seal,
            wseq: 0,
            wbuf: Vec::new(),
            werr: None,
        },
        ChannelReader {
            r,
            open,
            rseq: 0,
            rerr: None,
            transcript,
        },
    )
}

pub fn agent_challenge<W, R>(
    identity: &SigningKey,
    boot: Option<&[u8; 32]>,
    h: &Hello,
    w: W,
    r: R,
) -> Result<(Challenge, ChannelWriter<W>, ChannelReader<R>), String> {
    if h.nonce.len() != NONCE_SIZE {
        return Err("hello without a nonce".into());
    }
    let peer_bytes: [u8; 32] = h
        .eph
        .as_slice()
        .try_into()
        .map_err(|_| "hello: invalid ephemeral public key length".to_string())?;
    let peer = X25519PublicKey::from(peer_bytes);
    let eph = EphemeralSecret::random_from_rng(OsRng);
    let ea = X25519PublicKey::from(&eph);
    let shared = eph.diffie_hellman(&peer);
    if !shared.was_contributory() {
        return Err("hello: low-order X25519 public key".into());
    }
    let mut an = [0u8; NONCE_SIZE];
    OsRng.fill_bytes(&mut an);
    let agent_pub = identity.verifying_key().to_bytes();
    let t = transcript_hash(&h.nonce, &h.eph, &an, ea.as_bytes(), &agent_pub);

    let mut shared_bytes = *shared.as_bytes();
    let (cw, cr) = new_channel_pair(&shared_bytes, t, false, w, r);
    shared_bytes.zeroize();

    let mut sign_msg = [0u8; AGENT_DOMAIN.len() + 32];
    sign_msg[..AGENT_DOMAIN.len()].copy_from_slice(AGENT_DOMAIN);
    sign_msg[AGENT_DOMAIN.len()..].copy_from_slice(&t);
    let sig = identity.sign(&sign_msg);

    let proof = boot.map(|s| boot_proof(s, &t).to_vec());
    let challenge = Challenge {
        ok: false,
        error: None,
        nonce: Some(an.to_vec()),
        key: Some(agent_pub.to_vec()),
        eph: Some(ea.as_bytes().to_vec()),
        sig: Some(sig.to_bytes().to_vec()),
        proof,
    };
    Ok((challenge, cw, cr))
}

#[derive(Clone)]
pub struct DeadlineStream {
    stream: Arc<TcpStream>,
    deadline: Option<Instant>,
}

impl DeadlineStream {
    pub fn new(stream: Arc<TcpStream>, deadline: Option<Instant>) -> Self {
        Self { stream, deadline }
    }

    pub fn set_deadline(&mut self, deadline: Option<Instant>) {
        self.deadline = deadline;
        if deadline.is_none() {
            let _ = self.stream.set_read_timeout(None);
            let _ = self.stream.set_write_timeout(None);
        }
    }

    fn remaining_timeout(&self) -> io::Result<Option<Duration>> {
        let Some(deadline) = self.deadline else {
            return Ok(None);
        };
        let rem = deadline.saturating_duration_since(Instant::now());
        if rem.is_zero() {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "i/o deadline exceeded",
            ));
        }
        Ok(Some(rem.max(Duration::from_millis(1))))
    }
}

impl Read for DeadlineStream {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        loop {
            if let Some(t) = self.remaining_timeout()? {
                self.stream.set_read_timeout(Some(t))?;
            }
            match (&*self.stream).read(buf) {
                Err(ref e) if e.kind() == io::ErrorKind::Interrupted => continue,
                res => return res,
            }
        }
    }
}

impl Write for DeadlineStream {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        loop {
            if let Some(t) = self.remaining_timeout()? {
                self.stream.set_write_timeout(Some(t))?;
            }
            match (&*self.stream).write(buf) {
                Err(ref e) if e.kind() == io::ErrorKind::Interrupted => continue,
                res => return res,
            }
        }
    }

    fn flush(&mut self) -> io::Result<()> {
        (&*self.stream).flush()
    }
}

pub fn upgrade_reader_if_empty(
    cr: ChannelReader<BufReader<DeadlineStream>>,
    cap: usize,
) -> ChannelReader<BufReader<DeadlineStream>> {
    let (br, open, rseq, rerr, tr) = cr.into_reader();
    let br = if br.buffer().is_empty() {
        BufReader::with_capacity(cap, br.into_inner())
    } else {
        br
    };
    ChannelReader::from_parts(br, open, rseq, rerr, tr)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;

    #[test]
    fn channel_roundtrip_and_tamper_rejection() {
        let secret = [0x42u8; 32];
        let tr = [0x99u8; 32];
        let mut wire = Vec::new();
        let (mut cli_w, _) = new_channel_pair(&secret, tr, true, &mut wire, io::empty());
        cli_w.write_frame(FRAME_STDIN, b"PASSWORD=hunter2").unwrap();
        assert!(!wire.windows(7).any(|w| w == b"hunter2"));

        let (_, mut agt_r) =
            new_channel_pair(&secret, tr, false, io::sink(), Cursor::new(wire.clone()));
        let (typ, got) = agt_r.read_frame().unwrap();
        assert_eq!(typ, FRAME_STDIN);
        assert_eq!(got, b"PASSWORD=hunter2");

        for off in [0, 4, 5, 9, 5 + 16, 5 + 16 + 15] {
            let mut bad = wire.clone();
            bad[off] ^= 1;
            let (_, mut r) = new_channel_pair(&secret, tr, false, io::sink(), Cursor::new(bad));
            assert!(r.read_frame().is_err(), "bit flip at {off} accepted");
        }
    }

    #[test]
    fn channel_rejects_reflection_and_sticky_sequence_error() {
        let secret = [0x11u8; 32];
        let tr = [0x22u8; 32];
        let mut wire = Vec::new();
        let (mut agt_w, _) = new_channel_pair(&secret, tr, false, &mut wire, io::empty());
        agt_w.write_frame(FRAME_STDOUT, b"one").unwrap();

        let (_, mut agt_r) = new_channel_pair(&secret, tr, false, io::sink(), Cursor::new(wire));
        assert!(matches!(agt_r.read_frame(), Err(FrameError::Auth)));
        assert!(matches!(agt_r.read_frame(), Err(FrameError::Auth)));

        let mut out = Vec::new();
        let (mut cw, _) = new_channel_pair(&secret, tr, true, &mut out, io::empty());
        cw.wseq = u64::MAX - 1;
        cw.write_frame(FRAME_STDIN, b"last").unwrap();
        assert!(matches!(
            cw.write_frame(FRAME_STDIN, b"wrap"),
            Err(FrameError::Sequence)
        ));
        assert!(matches!(
            cw.write_frame(FRAME_STDIN, b""),
            Err(FrameError::Sequence)
        ));
    }

    #[test]
    fn read_line_crlf_boundary() {
        let mut max_ok = vec![b'a'; MAX_LINE - 1];
        max_ok.extend_from_slice(b"\r\n");
        let mut r = BufReader::new(Cursor::new(max_ok));
        let got = read_line_bounded(&mut r).unwrap();
        assert_eq!(got.len(), MAX_LINE - 1);

        let mut too_big = vec![b'a'; MAX_LINE];
        too_big.extend_from_slice(b"\r\n");
        let mut r = BufReader::new(Cursor::new(too_big));
        assert!(read_line_bounded(&mut r).is_err());
    }

    #[test]
    fn request_debug_redacts_env() {
        let req = Request {
            op: OP_EXEC.into(),
            args: vec!["env".into()],
            env: vec!["API_TOKEN=super-secret-value".into()],
            ..Default::default()
        };
        let dbg = format!("{req:?}");
        assert!(!dbg.contains("super-secret-value"));
        assert!(!dbg.contains("API_TOKEN"));
    }

    #[test]
    fn handshake_full_roundtrip() {
        let host = SigningKey::generate(&mut OsRng);
        let identity = SigningKey::generate(&mut OsRng);
        let boot = boot_secret(&host.to_bytes());

        let cli_eph = EphemeralSecret::random_from_rng(OsRng);
        let ec = X25519PublicKey::from(&cli_eph);
        let mut cn = [0u8; 32];
        OsRng.fill_bytes(&mut cn);
        let hello = Hello {
            op: OP_HELLO.into(),
            nonce: cn.to_vec(),
            eph: ec.as_bytes().to_vec(),
        };

        let mut to_cli: Vec<u8> = Vec::new();
        let mut to_agt: Vec<u8> = Vec::new();
        let (ch, _, _) =
            agent_challenge(&identity, Some(&boot), &hello, &mut to_cli, io::empty()).unwrap();
        let an = ch.nonce.as_ref().unwrap();
        let ea = ch.eph.as_ref().unwrap();
        let key = ch.key.as_ref().unwrap();
        let tr = transcript_hash(&cn, ec.as_bytes(), an, ea, key);

        let peer_ea: [u8; 32] = ea.as_slice().try_into().unwrap();
        let shared = cli_eph.diffie_hellman(&X25519PublicKey::from(peer_ea));
        assert!(shared.was_contributory());

        let proof_want = boot_proof(&boot, &tr);
        assert_eq!(ch.proof.as_deref(), Some(proof_want.as_slice()));

        let (mut cli_w, _) =
            new_channel_pair(shared.as_bytes(), tr, true, &mut to_agt, io::empty());
        let body = br#"{"op":"ping"}"#;
        let mut sign_msg = Vec::new();
        sign_msg.extend_from_slice(REQUEST_DOMAIN);
        sign_msg.extend_from_slice(&tr);
        sign_msg.extend_from_slice(body);
        let sig = host.sign(&sign_msg);
        let signed = format!(
            r#"{{"sig":"{}","req":{}}}"#,
            B64_STD.encode(sig.to_bytes()),
            std::str::from_utf8(body).unwrap()
        );
        cli_w.write_frame(FRAME_REQUEST, signed.as_bytes()).unwrap();

        let (_, mut agt_r) =
            new_channel_pair(shared.as_bytes(), tr, false, io::sink(), Cursor::new(to_agt));
        let req = agt_r.read_request(&host.verifying_key()).unwrap();
        assert_eq!(req.op, OP_PING);
    }

    #[test]
    fn bench_channel_32k() {
        let secret = [0x55u8; 32];
        let tr = [0xAAu8; 32];
        let payload = vec![0x33u8; 32 << 10];
        let (mut cli_w, _) = new_channel_pair(
            &secret,
            tr,
            true,
            Vec::<u8>::with_capacity((32 << 10) + 32),
            io::empty(),
        );
        let (_, mut agt_r) =
            new_channel_pair(&secret, tr, false, io::sink(), Cursor::new(Vec::<u8>::new()));
        let iters = 20000usize;
        let start = Instant::now();
        for _ in 0..iters {
            cli_w.w.clear();
            cli_w.write_frame(FRAME_STDIN, &payload).unwrap();
            std::mem::swap(&mut cli_w.w, agt_r.r.get_mut());
            agt_r.r.set_position(0);
            let (_, got) = agt_r.read_frame().unwrap();
            assert_eq!(got.len(), payload.len());
            std::mem::swap(&mut cli_w.w, agt_r.r.get_mut());
        }
        let elapsed = start.elapsed();
        let mb_s =
            (iters as f64 * payload.len() as f64) / (1024.0 * 1024.0) / elapsed.as_secs_f64();
        eprintln!(
            "rust 32K AEAD round-trip: {:.2} MB/s ({:.0} ns/op)",
            mb_s,
            elapsed.as_nanos() as f64 / iters as f64
        );
    }
}
