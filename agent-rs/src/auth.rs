use std::io::BufReader;
use std::net::{Shutdown, TcpStream};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use ed25519_dalek::{SigningKey, VerifyingKey};

use crate::proto::{
    self, ChannelReader, ChannelWriter, DeadlineStream, ERR_BAD_REQUEST, ERR_UNAUTHORIZED,
    FRAME_RESPONSE, Hello, NEEDS_NEWER_CLI, NONCE_SIZE, OP_HELLO, ReadRequestError, Request,
    Response,
};

pub const PRE_AUTH_TIMEOUT: Duration = Duration::from_secs(2);
pub const MAX_PRE_AUTH: usize = 256;

struct PendingConn {
    id: u64,
    stream: Arc<TcpStream>,
    hello: bool,
}

#[derive(Default)]
pub struct Pending {
    conns: Mutex<Vec<PendingConn>>,
}

impl Pending {
    pub fn add(&self, id: u64, stream: Arc<TcpStream>) -> Option<Arc<TcpStream>> {
        let mut conns = self.conns.lock().unwrap_or_else(|e| e.into_inner());
        let old = if conns.len() >= MAX_PRE_AUTH {
            let idx = conns.iter().position(|x| !x.hello).unwrap_or(0);
            Some(conns.remove(idx).stream)
        } else {
            None
        };
        conns.push(PendingConn {
            id,
            stream,
            hello: false,
        });
        old
    }

    pub fn hello_seen(&self, id: u64) {
        let mut conns = self.conns.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(c) = conns.iter_mut().find(|x| x.id == id) {
            c.hello = true;
        }
    }

    pub fn remove(&self, id: u64) {
        let mut conns = self.conns.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(idx) = conns.iter().position(|x| x.id == id) {
            conns.remove(idx);
        }
    }
}

#[derive(Default)]
struct RateLogState {
    next: Option<Instant>,
    dropped: u64,
}

#[derive(Default)]
pub struct RateLog {
    state: Mutex<RateLogState>,
}

impl RateLog {
    pub fn log(&self, msg: &str) {
        let mut st = self.state.lock().unwrap_or_else(|e| e.into_inner());
        let now = Instant::now();
        if let Some(next) = st.next
            && now < next
        {
            st.dropped += 1;
            return;
        }
        st.next = Some(now + Duration::from_secs(1));
        if st.dropped > 0 {
            let dropped = st.dropped;
            st.dropped = 0;
            crate::log_line(&format!("{msg} (and {dropped} more like it)"));
        } else {
            crate::log_line(msg);
        }
    }
}

pub type AuthResult = Result<
    (
        Request,
        ChannelWriter<DeadlineStream>,
        ChannelReader<BufReader<DeadlineStream>>,
    ),
    String,
>;

pub fn authenticate(
    identity: &SigningKey,
    boot: Option<&[u8; 32]>,
    host_key: &VerifyingKey,
    pending: &Pending,
    conn_id: u64,
    stream: Arc<TcpStream>,
    deadline: Instant,
) -> AuthResult {
    let mut br = BufReader::with_capacity(
        4 << 10,
        DeadlineStream::new(Arc::clone(&stream), Some(deadline)),
    );
    let line = proto::read_line_bounded(&mut br).map_err(|e| format!("bad hello: {e}"))?;
    let h: Hello = serde_json::from_slice(&line).map_err(|e| format!("bad hello: {e}"))?;
    if h.op != OP_HELLO || h.nonce.len() != NONCE_SIZE || h.eph.is_empty() {
        let _ = proto::write_line(
            DeadlineStream::new(Arc::clone(&stream), Some(deadline)),
            &Response::err(NEEDS_NEWER_CLI),
        );
        return Err("client did not start with a hello carrying an ephemeral key".into());
    }
    pending.hello_seen(conn_id);
    let (challenge, mut cw, mut cr) = proto::agent_challenge(
        identity,
        boot,
        &h,
        DeadlineStream::new(Arc::clone(&stream), Some(deadline)),
        br,
    )?;
    proto::write_line(
        DeadlineStream::new(Arc::clone(&stream), Some(deadline)),
        &challenge,
    )
    .map_err(|e| e.to_string())?;
    let req = match cr.read_request(host_key) {
        Ok(req) => {
            pending.remove(conn_id);
            req
        }
        Err(ReadRequestError::Unauthorized) => {
            let _ = cw.write_message(FRAME_RESPONSE, &Response::err(ERR_UNAUTHORIZED));
            return Err(ERR_UNAUTHORIZED.into());
        }
        Err(e @ ReadRequestError::BadRequest(_)) => {
            let _ = cw.write_message(FRAME_RESPONSE, &Response::err(ERR_BAD_REQUEST));
            return Err(e.to_string());
        }
        Err(e) => return Err(e.to_string()),
    };
    let mut cr = proto::upgrade_reader_if_empty(cr, 64 << 10);
    cr.reader_mut().get_mut().set_deadline(None);
    Ok((req, cw, cr))
}

pub fn evict_stream(stream: &TcpStream) {
    let _ = stream.shutdown(Shutdown::Both);
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use std::net::TcpListener;
    use std::thread;

    use rand_core::OsRng;

    #[test]
    fn slowloris_pre_auth_hits_wall_clock_deadline() {
        let ln = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = ln.local_addr().unwrap();
        let identity = SigningKey::generate(&mut OsRng);
        let host = SigningKey::generate(&mut OsRng);
        let pending = Pending::default();

        let client = thread::spawn(move || {
            let mut s = TcpStream::connect(addr).unwrap();
            let _ = s.set_nodelay(true);
            for _ in 0..20 {
                if s.write_all(b"{").is_err() {
                    break;
                }
                thread::sleep(Duration::from_millis(120));
            }
        });

        let (stream, _) = ln.accept().unwrap();
        let stream = Arc::new(stream);
        pending.add(1, Arc::clone(&stream));
        let start = Instant::now();
        let deadline = start + Duration::from_millis(600);
        let res = authenticate(
            &identity,
            None,
            &host.verifying_key(),
            &pending,
            1,
            stream,
            deadline,
        );
        let elapsed = start.elapsed();
        assert!(res.is_err());
        assert!(
            elapsed >= Duration::from_millis(550) && elapsed < Duration::from_millis(1200),
            "expected wall-clock deadline ~600ms, got {elapsed:?}"
        );
        let _ = client.join();
    }
}
