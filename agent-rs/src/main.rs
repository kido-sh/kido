mod auth;
mod exec;
mod notify;
mod proto;
mod user;

use std::env;
use std::ffi::CString;
use std::io::{self, Write};
use std::net::{Ipv4Addr, Shutdown, TcpListener, TcpStream};
use std::os::fd::{AsRawFd, FromRawFd, RawFd};
use std::process::{Command, Stdio};
use std::ptr;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, Once, RwLock};
use std::thread;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use ed25519_dalek::{SigningKey, VerifyingKey};
use rand_core::OsRng;
use zeroize::{Zeroize, Zeroizing};

use crate::auth::{MAX_PRE_AUTH, PRE_AUTH_TIMEOUT, Pending, RateLog};
use crate::notify::{MountTable, SysMounts};
use crate::proto::{
    DEFAULT_PORT, FRAME_RESPONSE, OP_EXEC, OP_NOTIFY, OP_PING, OP_SHUTDOWN, Response, VERSION,
};

const MAX_CONNS: usize = 256;
const MAX_PRE_AUTH_WORKERS: usize = MAX_PRE_AUTH * 2;
const RB_HALT: libc::c_int = 0x08;

pub static FORK_LOCK: RwLock<()> = RwLock::new(());

unsafe extern "C" {
    fn reboot(howto: libc::c_int) -> libc::c_int;
}

pub fn log_line(msg: &str) {
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default();
    let mut err = io::stderr().lock();
    let _ = writeln!(
        err,
        "kido-agent: {}.{:06} {msg}",
        now.as_secs(),
        now.subsec_micros()
    );
    let _ = err.flush();
}

fn fatal(msg: &str) -> ! {
    log_line(msg);
    std::process::exit(1);
}

struct BootConfig {
    key: String,
    secret: Zeroizing<String>,
    ip: String,
    port: u16,
}

fn read_bootargs_sysctl() -> Option<Zeroizing<String>> {
    let name = CString::new("kern.bootargs").ok()?;
    let mut len: libc::size_t = 0;
    let rc = unsafe {
        libc::sysctlbyname(
            name.as_ptr(),
            ptr::null_mut(),
            &mut len,
            ptr::null_mut(),
            0,
        )
    };
    if rc != 0 || len == 0 {
        return None;
    }
    let mut buf = Zeroizing::new(vec![0u8; len]);
    let rc = unsafe {
        libc::sysctlbyname(
            name.as_ptr(),
            buf.as_mut_ptr().cast::<libc::c_void>(),
            &mut len,
            ptr::null_mut(),
            0,
        )
    };
    if rc != 0 {
        return None;
    }
    buf.truncate(len);
    if buf.last() == Some(&0) {
        buf.pop();
    }
    let s = std::str::from_utf8(&buf).ok()?.to_string();
    Some(Zeroizing::new(s))
}

fn boot_args() -> BootConfig {
    let mut c = BootConfig {
        key: String::new(),
        secret: Zeroizing::new(String::new()),
        ip: String::new(),
        port: DEFAULT_PORT,
    };
    let Some(args) = read_bootargs_sysctl() else {
        return c;
    };
    for field in args.split_whitespace() {
        let Some((k, v)) = field.split_once('=') else {
            continue;
        };
        match k {
            "kido_key" => c.key = v.to_string(),
            "kido_secret" => *c.secret = v.to_string(),
            "kido_ip" => c.ip = v.to_string(),
            "kido_port" => {
                if let Ok(p) = v.parse::<u16>()
                    && p > 0
                {
                    c.port = p;
                }
            }
            _ => {}
        }
    }
    c
}

fn listen_host(ip: &str) -> Result<String, String> {
    if ip.is_empty() {
        return Ok("0.0.0.0".into());
    }
    ip.parse::<Ipv4Addr>()
        .map(|_| ip.to_string())
        .map_err(|_| format!("kido_ip={ip:?} is not an IPv4 address"))
}

fn listen_when_up(addr: &str) -> io::Result<TcpListener> {
    let deadline = Instant::now() + Duration::from_secs(60);
    loop {
        match TcpListener::bind(addr) {
            Ok(ln) => return Ok(ln),
            Err(e)
                if e.raw_os_error() == Some(libc::EADDRNOTAVAIL) && Instant::now() < deadline =>
            {
                thread::sleep(Duration::from_millis(50));
            }
            Err(e) => return Err(e),
        }
    }
}

fn accept_cloexec(ln_fd: RawFd) -> io::Result<Option<TcpStream>> {
    let _fork_guard = FORK_LOCK.read().unwrap_or_else(|e| e.into_inner());
    let fd = unsafe { libc::accept(ln_fd, ptr::null_mut(), ptr::null_mut()) };
    if fd < 0 {
        let e = io::Error::last_os_error();
        if matches!(
            e.kind(),
            io::ErrorKind::WouldBlock | io::ErrorKind::Interrupted
        ) {
            return Ok(None);
        }
        return Err(e);
    }
    if unsafe { libc::fcntl(fd, libc::F_SETFD, libc::FD_CLOEXEC) } < 0 {
        let e = io::Error::last_os_error();
        unsafe {
            libc::close(fd);
        }
        return Err(e);
    }
    let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
    if flags < 0 || unsafe { libc::fcntl(fd, libc::F_SETFL, flags & !libc::O_NONBLOCK) } < 0 {
        let e = io::Error::last_os_error();
        unsafe {
            libc::close(fd);
        }
        return Err(e);
    }
    Ok(Some(unsafe { TcpStream::from_raw_fd(fd) }))
}

struct CliFlags {
    listen: String,
    key: String,
    secret: Zeroizing<String>,
}

fn parse_flags() -> CliFlags {
    let mut flags = CliFlags {
        listen: String::new(),
        key: String::new(),
        secret: Zeroizing::new(String::new()),
    };
    let mut args = env::args().skip(1);
    while let Some(arg) = args.next() {
        let stripped = arg
            .strip_prefix("--")
            .or_else(|| arg.strip_prefix('-'))
            .unwrap_or("");
        if stripped.is_empty() {
            continue;
        }
        let (name, val) = if let Some((n, v)) = stripped.split_once('=') {
            (n, v.to_string())
        } else {
            let v = args
                .next()
                .unwrap_or_else(|| fatal(&format!("flag -{stripped} needs an argument")));
            (stripped, v)
        };
        match name {
            "listen" => flags.listen = val,
            "key" => flags.key = val,
            "secret" => *flags.secret = val,
            _ => fatal(&format!("unknown flag: {arg}")),
        }
    }
    flags
}

struct Server {
    host_key: VerifyingKey,
    identity: SigningKey,
    boot: Option<Zeroizing<[u8; 32]>>,
    pending: Pending,
    live_pre_auth: AtomicUsize,
    active_conns: AtomicUsize,
    refusals: RateLog,
    halt: Once,
    mounts: Arc<dyn MountTable>,
    notify_conn: Mutex<Option<Arc<TcpStream>>>,
    notify_send: Mutex<()>,
    notify_log: RateLog,
}

struct CounterPermit<'a>(&'a AtomicUsize);

impl<'a> CounterPermit<'a> {
    fn try_acquire(counter: &'a AtomicUsize, limit: usize) -> Option<Self> {
        let mut cur = counter.load(Ordering::Relaxed);
        loop {
            if cur >= limit {
                return None;
            }
            match counter.compare_exchange_weak(
                cur,
                cur + 1,
                Ordering::Acquire,
                Ordering::Relaxed,
            ) {
                Ok(_) => return Some(Self(counter)),
                Err(next) => cur = next,
            }
        }
    }
}

impl Drop for CounterPermit<'_> {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::Release);
    }
}

struct OwnedPreAuthPermit(Arc<Server>);

impl Drop for OwnedPreAuthPermit {
    fn drop(&mut self) {
        self.0.live_pre_auth.fetch_sub(1, Ordering::Release);
    }
}

impl Server {
    fn acquire_pre_auth_worker(self: &Arc<Self>) -> Option<OwnedPreAuthPermit> {
        for attempt in 0..20 {
            let mut cur = self.live_pre_auth.load(Ordering::Relaxed);
            loop {
                if cur >= MAX_PRE_AUTH_WORKERS {
                    break;
                }
                match self.live_pre_auth.compare_exchange_weak(
                    cur,
                    cur + 1,
                    Ordering::Acquire,
                    Ordering::Relaxed,
                ) {
                    Ok(_) => return Some(OwnedPreAuthPermit(Arc::clone(self))),
                    Err(next) => cur = next,
                }
            }
            if attempt < 19 {
                thread::sleep(Duration::from_millis(1));
            }
        }
        None
    }

    fn serve(self: Arc<Self>, conn_id: u64, stream: Arc<TcpStream>, pre_auth_permit: OwnedPreAuthPermit) {
        let _ = stream.set_nodelay(true);
        let deadline = Instant::now() + PRE_AUTH_TIMEOUT;

        let peer = stream
            .peer_addr()
            .map(|a| a.to_string())
            .unwrap_or_else(|_| "unknown".into());
        let auth_res = auth::authenticate(
            &self.identity,
            self.boot.as_deref(),
            &self.host_key,
            &self.pending,
            conn_id,
            Arc::clone(&stream),
            deadline,
        );
        self.pending.remove(conn_id);
        drop(pre_auth_permit);

        let (req, mut cw, cr) = match auth_res {
            Ok(v) => v,
            Err(e) => {
                self.refusals.log(&format!("{peer}: {e}"));
                let _ = stream.shutdown(Shutdown::Both);
                return;
            }
        };

        let Some(_permit) = CounterPermit::try_acquire(&self.active_conns, MAX_CONNS) else {
            self.refusals
                .log(&format!("refusing {peer}: {MAX_CONNS} sessions open"));
            let _ = cw.write_message(FRAME_RESPONSE, &Response::err("too many sessions"));
            let _ = stream.shutdown(Shutdown::Both);
            return;
        };
        cw.writer_mut().set_deadline(None);

        match req.op.as_str() {
            OP_PING => {
                let _ = cw.write_message(FRAME_RESPONSE, &Response::ok_version());
            }
            OP_EXEC => {
                exec::handle_exec(Arc::clone(&stream), cw, cr, &req);
            }
            OP_NOTIFY => {
                notify::handle_notify(
                    Arc::clone(&stream),
                    cw,
                    cr,
                    &req,
                    Arc::clone(&self.mounts),
                    &self.notify_conn,
                    &self.notify_send,
                    &self.notify_log,
                );
            }
            OP_SHUTDOWN => {
                log_line("shutdown requested");
                let _ = cw.write_message(FRAME_RESPONSE, &Response::ok_version());
                let _ = stream.shutdown(Shutdown::Both);
                self.halt.call_once(|| {
                    let _ = thread::Builder::new()
                        .stack_size(256 * 1024)
                        .spawn(poweroff);
                });
            }
            other => {
                let _ = cw.write_message(
                    FRAME_RESPONSE,
                    &Response::err(format!("unknown op {other:?}")),
                );
            }
        }
        let _ = stream.shutdown(Shutdown::Both);
    }
}

fn run_with_timeout(prog: &str) {
    let spawn_res = {
        let _fork_guard = FORK_LOCK.write().unwrap_or_else(|e| e.into_inner());
        Command::new(prog)
            .stdin(Stdio::null())
            .stdout(Stdio::from(io::stderr()))
            .stderr(Stdio::from(io::stderr()))
            .spawn()
    };
    let mut child = match spawn_res {
        Ok(c) => c,
        Err(e) => {
            log_line(&format!("{prog}: {e}"));
            return;
        }
    };
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        match child.try_wait() {
            Ok(Some(st)) => {
                if !st.success() {
                    log_line(&format!("{prog}: exit status {st}"));
                }
                return;
            }
            Ok(None) => {
                if Instant::now() >= deadline {
                    log_line(&format!("{prog}: still running after 10 s"));
                    let _ = child.kill();
                    let _ = child.wait();
                    return;
                }
                thread::sleep(Duration::from_millis(50));
            }
            Err(e) => {
                log_line(&format!("{prog}: {e}"));
                return;
            }
        }
    }
}

fn poweroff() {
    thread::sleep(Duration::from_millis(100));
    run_with_timeout("/bin/sync");
    run_with_timeout("/bin/poweroff");
    thread::sleep(Duration::from_secs(5));
    log_line("still running 5 s after poweroff; halting");
    unsafe {
        libc::sync();
        reboot(RB_HALT);
    }
}

fn read_u32_sysctl(key: &str) -> Option<u32> {
    let name = CString::new(key).ok()?;
    let mut val: u32 = 0;
    let mut len: libc::size_t = std::mem::size_of::<u32>();
    let rc = unsafe {
        libc::sysctlbyname(
            name.as_ptr(),
            (&mut val as *mut u32).cast::<libc::c_void>(),
            &mut len,
            ptr::null_mut(),
            0,
        )
    };
    if rc == 0 && len == std::mem::size_of::<u32>() {
        Some(val)
    } else {
        None
    }
}

fn raise_nofile_limit() {
    let mut rl = libc::rlimit {
        rlim_cur: 0,
        rlim_max: 0,
    };
    if unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut rl) } != 0 {
        return;
    }
    let max_per_proc = read_u32_sysctl("kern.maxfilesperproc")
        .map(|v| v as libc::rlim_t)
        .unwrap_or(10240);
    let target = rl.rlim_max.min(max_per_proc);
    if target > rl.rlim_cur {
        rl.rlim_cur = target;
        unsafe {
            libc::setrlimit(libc::RLIMIT_NOFILE, &rl);
        }
    }
}

fn main() {
    raise_nofile_limit();
    let mut flags = parse_flags();
    let mut ba = boot_args();
    if flags.key.is_empty() {
        flags.key = ba.key;
    }
    if flags.secret.is_empty() {
        *flags.secret = std::mem::take(&mut *ba.secret);
    }
    ba.secret.zeroize();

    let host_key = proto::parse_key(&flags.key).unwrap_or_else(|e| {
        fatal(&format!(
            "kido_key: {e} (the host passes it in the boot-args; --key for tests)"
        ))
    });
    let boot = if flags.secret.is_empty() {
        log_line(
            "no kido_secret in the boot-args: kido will not accept this agent at its first contact",
        );
        None
    } else {
        let parsed = proto::parse_secret(&flags.secret)
            .unwrap_or_else(|e| fatal(&format!("kido_secret: {e}")));
        Some(Zeroizing::new(parsed))
    };
    flags.secret.zeroize();

    let identity = SigningKey::generate(&mut OsRng);
    if flags.listen.is_empty() {
        if ba.port >= 1024 {
            fatal(&format!(
                "kido_port={}: the agent port must be below 1024, so that guest users cannot bind it next to the agent",
                ba.port
            ));
        }
        let host = listen_host(&ba.ip).unwrap_or_else(|e| fatal(&e));
        flags.listen = format!("{host}:{}", ba.port);
    }

    let ln = listen_when_up(&flags.listen).unwrap_or_else(|e| fatal(&e.to_string()));
    ln.set_nonblocking(true)
        .unwrap_or_else(|e| fatal(&e.to_string()));
    let addr = ln.local_addr().unwrap_or_else(|e| fatal(&e.to_string()));
    log_line(&format!(
        "listening on {addr} (protocol {VERSION}, key authentication, sealed channel)"
    ));

    let server = Arc::new(Server {
        host_key,
        identity,
        boot,
        pending: Pending::default(),
        live_pre_auth: AtomicUsize::new(0),
        active_conns: AtomicUsize::new(0),
        refusals: RateLog::default(),
        halt: Once::new(),
        mounts: Arc::new(SysMounts),
        notify_conn: Mutex::new(None),
        notify_send: Mutex::new(()),
        notify_log: RateLog::default(),
    });

    let ln_fd = ln.as_raw_fd();
    let next_id = AtomicU64::new(1);
    let mut pfd = libc::pollfd {
        fd: ln_fd,
        events: libc::POLLIN,
        revents: 0,
    };
    loop {
        pfd.revents = 0;
        let rc = unsafe { libc::poll(&mut pfd, 1, -1) };
        if rc < 0 {
            let e = io::Error::last_os_error();
            if e.kind() == io::ErrorKind::Interrupted {
                continue;
            }
            log_line(&format!("poll: {e}"));
            thread::sleep(Duration::from_millis(100));
            continue;
        }
        loop {
            let stream = match accept_cloexec(ln_fd) {
                Ok(Some(s)) => Arc::new(s),
                Ok(None) => break,
                Err(e) => {
                    log_line(&format!("accept: {e}"));
                    thread::sleep(Duration::from_millis(100));
                    break;
                }
            };
            let id = next_id.fetch_add(1, Ordering::Relaxed);
            if let Some(old) = server.pending.add(id, Arc::clone(&stream)) {
                let old_peer = old
                    .peer_addr()
                    .map(|a| a.to_string())
                    .unwrap_or_else(|_| "unknown".into());
                server.refusals.log(&format!(
                    "closing {old_peer}: {MAX_PRE_AUTH} newer connections are authenticating"
                ));
                auth::evict_stream(&old);
            }
            let Some(pre_auth_permit) = server.acquire_pre_auth_worker() else {
                server.pending.remove(id);
                auth::evict_stream(&stream);
                server
                    .refusals
                    .log("refusing connection: pre-auth worker ceiling reached");
                continue;
            };
            let srv = Arc::clone(&server);
            let st = Arc::clone(&stream);
            if let Err(e) = thread::Builder::new()
                .stack_size(256 * 1024)
                .spawn(move || srv.serve(id, st, pre_auth_permit))
            {
                server.pending.remove(id);
                auth::evict_stream(&stream);
                server.refusals.log(&format!("spawn pre-auth thread: {e}"));
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn listen_host_checks() {
        assert_eq!(listen_host("10.0.2.15").unwrap(), "10.0.2.15");
        assert_eq!(listen_host("").unwrap(), "0.0.0.0");
        assert!(listen_host("not-an-ip").is_err());
        assert!(listen_host("::1").is_err());
    }
}
