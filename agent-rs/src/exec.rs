use std::ffi::{CStr, CString};
use std::fs;
use std::io::{self, BufReader};
use std::net::{Shutdown, TcpStream};
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::unix::fs::MetadataExt;
use std::os::unix::process::{CommandExt, ExitStatusExt};
use std::path::Path;
use std::process::{Command, ExitStatus, Stdio};
use std::ptr;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{self, SyncSender};
use std::sync::{Arc, Condvar, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use crate::proto::{
    self, ChannelReader, ChannelWriter, DeadlineStream, FRAME_CLOSE_STDIN, FRAME_EXIT,
    FRAME_RESIZE, FRAME_RESPONSE, FRAME_SIGNAL, FRAME_STDERR, FRAME_STDIN, FRAME_STDOUT,
    FrameError, Request, Response,
};
use crate::user;

pub const DEFAULT_PATH: &str = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin";
pub const ROOT_HOME: &str = "/var/root";
const DRAIN_TIMEOUT: Duration = Duration::from_millis(500);
const DRAIN_POLL_GRACE: Duration = Duration::from_millis(50);
const FINISH_TIMEOUT: Duration = Duration::from_secs(2);
const HANGUP_GRACE: Duration = Duration::from_secs(1);
const MAX_SIGNAL: u8 = 31;

static PTSNAME_LOCK: Mutex<()> = Mutex::new(());

struct Cred {
    uid: u32,
    gid: u32,
    groups: Vec<libc::gid_t>,
}

struct ExecSpec {
    path: String,
    env: Vec<(String, String)>,
    dir: String,
    cred: Option<Cred>,
}

fn prepare(req: &Request) -> Result<ExecSpec, String> {
    if req.args.is_empty() || req.args[0].is_empty() {
        return Err("no command given".into());
    }
    for a in &req.args {
        if a.as_bytes().contains(&0) {
            return Err("argument contains a NUL byte".into());
        }
    }

    let mut dir = "/".to_string();
    let mut home = ROOT_HOME.to_string();
    let mut cred = None;
    if !req.user.is_empty() {
        let a = user::lookup_user(&req.user)?;
        home = a.home;
        let euid = unsafe { libc::geteuid() };
        let egid = unsafe { libc::getegid() };
        if a.uid != euid || a.gid != egid || euid == 0 {
            cred = Some(Cred {
                uid: a.uid,
                gid: a.gid,
                groups: a.groups.into_iter().map(|g| g as libc::gid_t).collect(),
            });
        }
    }

    let mut base = vec![format!("PATH={DEFAULT_PATH}"), format!("HOME={home}")];
    if req.tty {
        base.push("TERM=xterm".to_string());
    }
    let merged = merge_env(&base, &req.env)?;
    let path_env = env_value(&merged, "PATH").unwrap_or("").to_string();
    let env: Vec<(String, String)> = merged
        .into_iter()
        .filter_map(|kv| kv.split_once('=').map(|(k, v)| (k.to_string(), v.to_string())))
        .collect();

    if !req.dir.is_empty() {
        if !req.dir.starts_with('/') || req.dir.as_bytes().contains(&0) {
            return Err(format!(
                "working directory {:?} is not an absolute path",
                req.dir
            ));
        }
        dir = clean_abs_path(&req.dir);
    }
    match fs::metadata(&dir) {
        Err(e) => return Err(format!("working directory: {e}")),
        Ok(st) if !st.is_dir() => return Err(format!("working directory {dir}: not a directory")),
        Ok(_) => {}
    }

    let path = look_path(&req.args[0], &path_env, &dir)?;
    Ok(ExecSpec {
        path,
        env,
        dir,
        cred,
    })
}

pub fn merge_env(base: &[String], extra: &[String]) -> Result<Vec<String>, String> {
    let mut out = base.to_vec();
    for kv in extra {
        let Some((k, _)) = kv.split_once('=') else {
            return Err(format!("invalid environment entry for {:?}", kv));
        };
        if k.is_empty() || kv.as_bytes().contains(&0) {
            return Err(format!("invalid environment entry for {:?}", k));
        }
        let prefix = format!("{k}=");
        if let Some(slot) = out.iter_mut().find(|old| old.starts_with(&prefix)) {
            *slot = kv.clone();
        } else {
            out.push(kv.clone());
        }
    }
    Ok(out)
}

fn env_value<'a>(env: &'a [String], key: &str) -> Option<&'a str> {
    let prefix = format!("{key}=");
    env.iter().find_map(|kv| kv.strip_prefix(&prefix))
}

fn clean_abs_path(p: &str) -> String {
    let mut parts: Vec<&str> = Vec::new();
    for comp in p.split('/') {
        match comp {
            "" | "." => {}
            ".." => {
                parts.pop();
            }
            other => parts.push(other),
        }
    }
    if parts.is_empty() {
        "/".to_string()
    } else {
        format!("/{}", parts.join("/"))
    }
}

fn look_path(file: &str, path_env: &str, dir: &str) -> Result<String, String> {
    if file.contains('/') {
        let p = if file.starts_with('/') {
            file.to_string()
        } else {
            format!("{}/{}", dir.trim_end_matches('/'), file)
        };
        return match check_executable(&p) {
            Ok(()) => Ok(p),
            Err(e) if e.kind() == io::ErrorKind::NotFound => {
                Err(format!("executable file not found: {file:?}"))
            }
            Err(e) if e.kind() == io::ErrorKind::PermissionDenied => {
                Err(format!("permission denied: {file:?}"))
            }
            Err(e) => Err(e.to_string()),
        };
    }

    let mut denied = false;
    for d in path_env.split(':') {
        if !d.starts_with('/') {
            continue;
        }
        let p = format!("{}/{}", d.trim_end_matches('/'), file);
        match check_executable(&p) {
            Ok(()) => return Ok(p),
            Err(e) if e.kind() == io::ErrorKind::PermissionDenied => denied = true,
            Err(_) => {}
        }
    }
    if denied {
        Err(format!("permission denied: {file:?}"))
    } else {
        Err(format!("executable file not found in $PATH: {file:?}"))
    }
}

fn check_executable(p: &str) -> io::Result<()> {
    let st = fs::metadata(Path::new(p))?;
    if st.is_dir() || (st.mode() & 0o111) == 0 {
        return Err(io::Error::from(io::ErrorKind::PermissionDenied));
    }
    Ok(())
}

fn start_error(err: &io::Error, file: &str, chdir_failed: bool) -> String {
    if chdir_failed {
        return format!("working directory: {err}");
    }
    match err.kind() {
        io::ErrorKind::NotFound => format!("executable file not found: {file:?}: {err}"),
        io::ErrorKind::PermissionDenied => format!("permission denied: {file:?}: {err}"),
        _ => err.to_string(),
    }
}

fn set_cloexec(fd: RawFd) -> io::Result<()> {
    let rc = unsafe { libc::fcntl(fd, libc::F_SETFD, libc::FD_CLOEXEC) };
    if rc < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(())
    }
}

fn set_nonblock(fd: RawFd) -> io::Result<()> {
    let flags = unsafe { libc::fcntl(fd, libc::F_GETFL) };
    if flags < 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK) } < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(())
    }
}

fn pipe_cloexec() -> io::Result<(OwnedFd, OwnedFd)> {
    let _fork_guard = crate::FORK_LOCK.read().unwrap_or_else(|e| e.into_inner());
    let mut fds = [-1 as libc::c_int; 2];
    if unsafe { libc::pipe(fds.as_mut_ptr()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    let r = unsafe { OwnedFd::from_raw_fd(fds[0]) };
    let w = unsafe { OwnedFd::from_raw_fd(fds[1]) };
    set_cloexec(r.as_raw_fd())?;
    set_cloexec(w.as_raw_fd())?;
    Ok((r, w))
}

fn dup_cloexec(fd: RawFd) -> io::Result<OwnedFd> {
    let n = unsafe { libc::fcntl(fd, libc::F_DUPFD_CLOEXEC, 0) };
    if n < 0 {
        Err(io::Error::last_os_error())
    } else {
        Ok(unsafe { OwnedFd::from_raw_fd(n) })
    }
}

fn open_pty() -> io::Result<(OwnedFd, OwnedFd)> {
    let master_raw =
        unsafe { libc::posix_openpt(libc::O_RDWR | libc::O_NOCTTY | libc::O_CLOEXEC) };
    if master_raw < 0 {
        return Err(io::Error::last_os_error());
    }
    let master = unsafe { OwnedFd::from_raw_fd(master_raw) };
    if unsafe { libc::grantpt(master.as_raw_fd()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::unlockpt(master.as_raw_fd()) } != 0 {
        return Err(io::Error::last_os_error());
    }
    let slave_path = {
        let _g = PTSNAME_LOCK.lock().unwrap_or_else(|e| e.into_inner());
        let p = unsafe { libc::ptsname(master.as_raw_fd()) };
        if p.is_null() {
            return Err(io::Error::last_os_error());
        }
        unsafe { CStr::from_ptr(p) }.to_owned()
    };
    let slave_raw = unsafe {
        libc::open(
            slave_path.as_ptr(),
            libc::O_RDWR | libc::O_NOCTTY | libc::O_CLOEXEC,
        )
    };
    if slave_raw < 0 {
        return Err(io::Error::last_os_error());
    }
    let slave = unsafe { OwnedFd::from_raw_fd(slave_raw) };
    Ok((master, slave))
}

fn set_winsize(fd: RawFd, rows: u16, cols: u16) {
    if rows == 0 || cols == 0 {
        return;
    }
    let ws = libc::winsize {
        ws_row: rows,
        ws_col: cols,
        ws_xpixel: 0,
        ws_ypixel: 0,
    };
    unsafe {
        libc::ioctl(fd, libc::TIOCSWINSZ as libc::c_ulong, &ws);
    }
}

struct FrameWriterInner {
    cw: ChannelWriter<DeadlineStream>,
    closed: bool,
}

struct FrameWriter {
    closed: AtomicBool,
    inner: Mutex<FrameWriterInner>,
}

impl FrameWriter {
    fn new(cw: ChannelWriter<DeadlineStream>) -> Self {
        Self {
            closed: AtomicBool::new(false),
            inner: Mutex::new(FrameWriterInner { cw, closed: false }),
        }
    }

    fn mark_closed(&self) {
        self.closed.store(true, Ordering::SeqCst);
    }

    fn frame(&self, typ: u8, payload: &[u8]) -> Result<(), FrameError> {
        if self.closed.load(Ordering::SeqCst) {
            return Err(FrameError::Protocol("closed".into()));
        }
        let mut g = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        if g.closed || self.closed.load(Ordering::SeqCst) {
            g.closed = true;
            return Err(FrameError::Protocol("closed".into()));
        }
        let res = g.cw.write_frame(typ, payload);
        if res.is_err() {
            g.closed = true;
            self.closed.store(true, Ordering::SeqCst);
        }
        res
    }

    fn close(&self) {
        self.closed.store(true, Ordering::SeqCst);
        let mut g = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        g.closed = true;
    }

    fn finish(&self, status: i32) -> Result<(), FrameError> {
        if self.closed.load(Ordering::SeqCst) {
            return Err(FrameError::Protocol("closed".into()));
        }
        let mut g = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        if g.closed || self.closed.load(Ordering::SeqCst) {
            g.closed = true;
            return Err(FrameError::Protocol("closed".into()));
        }
        g.closed = true;
        self.closed.store(true, Ordering::SeqCst);
        g.cw.writer_mut()
            .set_deadline(Some(Instant::now() + FINISH_TIMEOUT));
        g.cw.write_frame(FRAME_EXIT, &proto::exit_payload(status))
    }

    fn response(&self, resp: &Response) -> Result<(), FrameError> {
        if self.closed.load(Ordering::SeqCst) {
            return Err(FrameError::Protocol("closed".into()));
        }
        let mut g = self.inner.lock().unwrap_or_else(|e| e.into_inner());
        if g.closed || self.closed.load(Ordering::SeqCst) {
            g.closed = true;
            return Err(FrameError::Protocol("closed".into()));
        }
        g.cw.write_message(FRAME_RESPONSE, resp)
    }
}

struct SessionState {
    ptmx: Option<Arc<OwnedFd>>,
    stop_w: Option<OwnedFd>,
    exited: bool,
    hung_up: bool,
}

struct Session {
    stream: Arc<TcpStream>,
    pid: i32,
    tty: bool,
    out: Arc<FrameWriter>,
    state: Mutex<SessionState>,
}

impl Session {
    fn signal(&self, sig: libc::c_int) {
        let g = self.state.lock().unwrap_or_else(|e| e.into_inner());
        if g.exited {
            return;
        }
        if let Some(ref ptmx) = g.ptmx {
            let mut pgrp: libc::c_int = 0;
            let rc = unsafe {
                libc::ioctl(ptmx.as_raw_fd(), libc::TIOCGPGRP as libc::c_ulong, &mut pgrp)
            };
            if rc == 0 && pgrp > 0 && unsafe { libc::kill(-pgrp, sig) } == 0 {
                return;
            }
        }
        if unsafe { libc::kill(-self.pid, sig) } != 0 {
            unsafe {
                libc::kill(self.pid, sig);
            }
        }
    }

    fn resize(&self, rows: u16, cols: u16) {
        let g = self.state.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(ref ptmx) = g.ptmx {
            set_winsize(ptmx.as_raw_fd(), rows, cols);
        }
    }

    fn stop_io(&self) {
        let mut g = self.state.lock().unwrap_or_else(|e| e.into_inner());
        g.stop_w.take();
    }

    fn clear_ptmx(&self) {
        let mut g = self.state.lock().unwrap_or_else(|e| e.into_inner());
        g.ptmx = None;
    }

    fn hangup(&self) {
        let first = {
            let mut g = self.state.lock().unwrap_or_else(|e| e.into_inner());
            let was = g.hung_up;
            g.hung_up = true;
            !was
        };
        if first {
            self.out.mark_closed();
            let _ = self.stream.shutdown(Shutdown::Both);
            self.signal(libc::SIGHUP);
            if self.tty {
                self.stop_io();
                self.clear_ptmx();
            }
            self.out.close();
        }
    }

    fn kill_after(&self, d: Duration) {
        let deadline = Instant::now() + d;
        while Instant::now() < deadline {
            if self.state.lock().unwrap_or_else(|e| e.into_inner()).exited {
                return;
            }
            thread::sleep(Duration::from_millis(10));
        }
        self.signal(libc::SIGKILL);
    }
}

struct PumpTracker {
    remaining: Mutex<usize>,
    cvar: Condvar,
}

impl PumpTracker {
    fn new(count: usize) -> Self {
        Self {
            remaining: Mutex::new(count),
            cvar: Condvar::new(),
        }
    }

    fn done(&self) {
        let mut rem = self.remaining.lock().unwrap_or_else(|e| e.into_inner());
        *rem = rem.saturating_sub(1);
        if *rem == 0 {
            self.cvar.notify_all();
        }
    }

    fn wait_timeout(&self, timeout: Duration) -> bool {
        let mut rem = self.remaining.lock().unwrap_or_else(|e| e.into_inner());
        let deadline = Instant::now() + timeout;
        while *rem > 0 {
            let now = Instant::now();
            if now >= deadline {
                return false;
            }
            let (guard, res) = self
                .cvar
                .wait_timeout(rem, deadline - now)
                .unwrap_or_else(|e| e.into_inner());
            rem = guard;
            if res.timed_out() && *rem > 0 {
                return false;
            }
        }
        true
    }
}

enum StdinWriteErr {
    Stopped,
    Broken,
}

fn write_chunk_nonblock(fd: RawFd, stop_r: RawFd, mut buf: &[u8]) -> Result<(), StdinWriteErr> {
    let mut fds = [
        libc::pollfd {
            fd,
            events: libc::POLLOUT,
            revents: 0,
        },
        libc::pollfd {
            fd: stop_r,
            events: libc::POLLIN,
            revents: 0,
        },
    ];
    while !buf.is_empty() {
        fds[0].revents = 0;
        fds[1].revents = 0;
        let rc = unsafe { libc::poll(fds.as_mut_ptr(), 2, -1) };
        if rc < 0 {
            if io::Error::last_os_error().kind() == io::ErrorKind::Interrupted {
                continue;
            }
            return Err(StdinWriteErr::Broken);
        }
        if fds[1].revents != 0 {
            return Err(StdinWriteErr::Stopped);
        }
        if fds[0].revents & (libc::POLLOUT | libc::POLLHUP | libc::POLLERR | libc::POLLNVAL) != 0 {
            let n = unsafe { libc::write(fd, buf.as_ptr().cast::<libc::c_void>(), buf.len()) };
            if n > 0 {
                buf = &buf[n as usize..];
            } else if n == 0 {
                return Err(StdinWriteErr::Broken);
            } else {
                let e = io::Error::last_os_error();
                if matches!(
                    e.kind(),
                    io::ErrorKind::Interrupted | io::ErrorKind::WouldBlock
                ) {
                    continue;
                }
                return Err(StdinWriteErr::Broken);
            }
        }
    }
    Ok(())
}

enum StdinTarget {
    Pipe(OwnedFd),
    Pty(Arc<OwnedFd>),
}

impl StdinTarget {
    fn as_raw_fd(&self) -> RawFd {
        match self {
            StdinTarget::Pipe(f) => f.as_raw_fd(),
            StdinTarget::Pty(f) => f.as_raw_fd(),
        }
    }
}

fn stdin_writer_loop(
    rx: mpsc::Receiver<Vec<u8>>,
    mut target: Option<StdinTarget>,
    stop_r: Arc<OwnedFd>,
) {
    for chunk in rx {
        if let Some(ref t) = target {
            match write_chunk_nonblock(t.as_raw_fd(), stop_r.as_raw_fd(), &chunk) {
                Ok(()) => {}
                Err(StdinWriteErr::Stopped) => {
                    return;
                }
                Err(StdinWriteErr::Broken) => {
                    target = None;
                }
            }
        }
    }
    drop(target);
}

fn pump_fd(
    sess: Arc<Session>,
    out_fd: Arc<OwnedFd>,
    stop_r: Arc<OwnedFd>,
    typ: u8,
    tracker: Arc<PumpTracker>,
) {
    struct DoneGuard(Arc<PumpTracker>);
    impl Drop for DoneGuard {
        fn drop(&mut self) {
            self.0.done();
        }
    }
    let _guard = DoneGuard(tracker);

    let mut buf = vec![0u8; 32 << 10];
    let mut fds = [
        libc::pollfd {
            fd: out_fd.as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        },
        libc::pollfd {
            fd: stop_r.as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        },
    ];
    loop {
        fds[0].revents = 0;
        fds[1].revents = 0;
        let rc = unsafe { libc::poll(fds.as_mut_ptr(), 2, -1) };
        if rc < 0 {
            if io::Error::last_os_error().kind() == io::ErrorKind::Interrupted {
                continue;
            }
            return;
        }
        if fds[1].revents != 0 {
            return;
        }
        if fds[0].revents & (libc::POLLIN | libc::POLLHUP | libc::POLLERR | libc::POLLNVAL) != 0 {
            let n = unsafe {
                libc::read(
                    out_fd.as_raw_fd(),
                    buf.as_mut_ptr().cast::<libc::c_void>(),
                    buf.len(),
                )
            };
            if n > 0 {
                if sess.out.frame(typ, &buf[..n as usize]).is_err() {
                    sess.hangup();
                    return;
                }
            } else if n == 0 {
                return;
            } else {
                let e = io::Error::last_os_error();
                if matches!(
                    e.kind(),
                    io::ErrorKind::Interrupted | io::ErrorKind::WouldBlock
                ) {
                    continue;
                }
                return;
            }
        }
    }
}

fn read_frames_loop(
    sess: Arc<Session>,
    mut cr: ChannelReader<BufReader<DeadlineStream>>,
    tty: bool,
    stdin_tx: SyncSender<Vec<u8>>,
) {
    let mut stdin_tx = Some(stdin_tx);
    loop {
        match cr.read_frame() {
            Ok((typ, payload)) => match typ {
                FRAME_STDIN => {
                    if !payload.is_empty()
                        && let Some(ref tx) = stdin_tx
                        && tx.send(payload).is_err()
                    {
                        stdin_tx = None;
                    }
                }
                FRAME_CLOSE_STDIN => {
                    stdin_tx = None;
                }
                FRAME_SIGNAL => {
                    if payload.len() == 1 && (1..=MAX_SIGNAL).contains(&payload[0]) {
                        sess.signal(payload[0] as libc::c_int);
                    }
                }
                FRAME_RESIZE => {
                    if let Some((rows, cols)) = proto::parse_resize(&payload) {
                        sess.resize(rows, cols);
                    }
                }
                other => {
                    crate::log_line(&format!("pid {}: ignoring frame type {}", sess.pid, other));
                }
            },
            Err(err) => {
                let stdin_open = stdin_tx.is_some();
                let clean_eof = matches!(err, FrameError::Eof);
                if stdin_open || tty || !clean_eof {
                    sess.hangup();
                }
                if stdin_open && !tty {
                    sess.kill_after(HANGUP_GRACE);
                }
                drop(stdin_tx);
                return;
            }
        }
    }
}

fn exit_status(status: io::Result<ExitStatus>) -> i32 {
    let Ok(st) = status else {
        return 255;
    };
    if let Some(sig) = st.signal() {
        return 128 + sig;
    }
    st.code().unwrap_or(255)
}

fn spawn_helper<F: FnOnce() + Send + 'static>(f: F) -> io::Result<thread::JoinHandle<()>> {
    thread::Builder::new().stack_size(256 * 1024).spawn(f)
}

fn spawn_spec_command(
    spec: &ExecSpec,
    args: &[String],
    tty: bool,
    stdin: Stdio,
    stdout: Stdio,
    stderr: Stdio,
) -> Result<std::process::Child, String> {
    let c_dir = CString::new(spec.dir.as_str())
        .map_err(|_| format!("working directory {:?} contains a NUL byte", spec.dir))?;
    let (chdir_err_r, chdir_err_w) = pipe_cloexec().map_err(|e| format!("pipe: {e}"))?;
    let chdir_err_w_fd = chdir_err_w.as_raw_fd();

    let mut cmd = Command::new(&spec.path);
    cmd.arg0(&args[0]);
    if args.len() > 1 {
        cmd.args(&args[1..]);
    }
    cmd.env_clear();
    cmd.envs(spec.env.iter().map(|(k, v)| (k, v)));
    cmd.stdin(stdin).stdout(stdout).stderr(stderr);

    let cred = spec.cred.as_ref().map(|c| (c.uid, c.gid, c.groups.clone()));
    unsafe {
        cmd.pre_exec(move || {
            let mut empty: libc::sigset_t = std::mem::zeroed();
            libc::sigemptyset(&mut empty);
            libc::sigprocmask(libc::SIG_SETMASK, &empty, ptr::null_mut());
            for sig in [
                libc::SIGHUP,
                libc::SIGINT,
                libc::SIGQUIT,
                libc::SIGPIPE,
                libc::SIGTERM,
                libc::SIGTSTP,
                libc::SIGTTIN,
                libc::SIGTTOU,
            ] {
                libc::signal(sig, libc::SIG_DFL);
            }
            if tty {
                if libc::setsid() < 0 {
                    return Err(io::Error::last_os_error());
                }
                if libc::ioctl(0, libc::TIOCSCTTY as libc::c_ulong, 0 as libc::c_int) < 0 {
                    return Err(io::Error::last_os_error());
                }
            } else if libc::setpgid(0, 0) < 0 {
                return Err(io::Error::last_os_error());
            }
            if let Some((uid, gid, ref groups)) = cred {
                if libc::setgroups(groups.len() as libc::c_int, groups.as_ptr()) != 0 {
                    return Err(io::Error::last_os_error());
                }
                if libc::setgid(gid as libc::gid_t) != 0 {
                    return Err(io::Error::last_os_error());
                }
                if libc::setuid(uid as libc::uid_t) != 0 {
                    return Err(io::Error::last_os_error());
                }
            }
            if libc::chdir(c_dir.as_ptr()) != 0 {
                let err = io::Error::last_os_error();
                let marker: u8 = 1;
                let _ = libc::write(
                    chdir_err_w_fd,
                    (&marker as *const u8).cast::<libc::c_void>(),
                    1,
                );
                return Err(err);
            }
            Ok(())
        });
    }

    let spawn_res = {
        let _fork_guard = crate::FORK_LOCK.write().unwrap_or_else(|e| e.into_inner());
        cmd.spawn()
    };
    drop(cmd);
    drop(chdir_err_w);

    match spawn_res {
        Ok(child) => Ok(child),
        Err(e) => {
            let mut marker = [0u8; 1];
            let chdir_failed = loop {
                let n = unsafe {
                    libc::read(
                        chdir_err_r.as_raw_fd(),
                        marker.as_mut_ptr().cast::<libc::c_void>(),
                        1,
                    )
                };
                if n < 0 && io::Error::last_os_error().kind() == io::ErrorKind::Interrupted {
                    continue;
                }
                break n == 1;
            };
            Err(start_error(&e, &args[0], chdir_failed))
        }
    }
}

pub fn handle_exec(
    stream: Arc<TcpStream>,
    cw: ChannelWriter<DeadlineStream>,
    cr: ChannelReader<BufReader<DeadlineStream>>,
    req: &Request,
) {
    let out = Arc::new(FrameWriter::new(cw));
    let spec = match prepare(req) {
        Ok(s) => s,
        Err(e) => {
            let arg0 = req.args.first().map(String::as_str).unwrap_or("");
            crate::log_line(&format!("exec {arg0:?}: {e}"));
            let _ = out.response(&Response::err(e));
            return;
        }
    };

    let tty = req.tty;
    let (outputs, stdin_target, ptmx_opt, stdin_stdio, stdout_stdio, stderr_stdio) = if tty {
        let (ptmx, slave) = match open_pty() {
            Ok(p) => p,
            Err(e) => {
                let _ = out.response(&Response::err(format!("pty: {e}")));
                return;
            }
        };
        if let Err(e) = set_nonblock(ptmx.as_raw_fd()) {
            let _ = out.response(&Response::err(format!("pty: {e}")));
            return;
        }
        if req.rows > 0 && req.cols > 0 {
            set_winsize(ptmx.as_raw_fd(), req.rows, req.cols);
        }
        if let Some(ref c) = spec.cred {
            unsafe {
                libc::fchown(
                    slave.as_raw_fd(),
                    c.uid as libc::uid_t,
                    u32::MAX as libc::gid_t,
                );
            }
        }
        let s_out = match dup_cloexec(slave.as_raw_fd()) {
            Ok(f) => f,
            Err(e) => {
                let _ = out.response(&Response::err(format!("pty: {e}")));
                return;
            }
        };
        let s_err = match dup_cloexec(slave.as_raw_fd()) {
            Ok(f) => f,
            Err(e) => {
                let _ = out.response(&Response::err(format!("pty: {e}")));
                return;
            }
        };
        let ptmx = Arc::new(ptmx);
        (
            vec![(FRAME_STDOUT, Arc::clone(&ptmx))],
            StdinTarget::Pty(Arc::clone(&ptmx)),
            Some(ptmx),
            Stdio::from(slave),
            Stdio::from(s_out),
            Stdio::from(s_err),
        )
    } else {
        let (in_r, in_w) = match pipe_cloexec() {
            Ok(p) => p,
            Err(e) => {
                let _ = out.response(&Response::err(format!("pipe: {e}")));
                return;
            }
        };
        if let Err(e) = set_nonblock(in_w.as_raw_fd()) {
            let _ = out.response(&Response::err(format!("pipe: {e}")));
            return;
        }
        let (out_r, out_w) = match pipe_cloexec() {
            Ok(p) => p,
            Err(e) => {
                let _ = out.response(&Response::err(format!("pipe: {e}")));
                return;
            }
        };
        let (err_r, err_w) = match pipe_cloexec() {
            Ok(p) => p,
            Err(e) => {
                let _ = out.response(&Response::err(format!("pipe: {e}")));
                return;
            }
        };
        (
            vec![
                (FRAME_STDOUT, Arc::new(out_r)),
                (FRAME_STDERR, Arc::new(err_r)),
            ],
            StdinTarget::Pipe(in_w),
            None,
            Stdio::from(in_r),
            Stdio::from(out_w),
            Stdio::from(err_w),
        )
    };

    let mut child = match spawn_spec_command(
        &spec,
        &req.args,
        tty,
        stdin_stdio,
        stdout_stdio,
        stderr_stdio,
    ) {
        Ok(c) => c,
        Err(msg) => {
            let _ = out.response(&Response::err(msg));
            return;
        }
    };

    let pid = child.id() as i32;
    crate::log_line(&format!(
        "exec pid {}: {} (tty {}, user {:?})",
        pid, req.args[0], tty, req.user
    ));

    let (stop_r, stop_w) = match pipe_cloexec() {
        Ok((r, w)) => (Arc::new(r), w),
        Err(e) => {
            crate::log_line(&format!("exec pid {pid}: stop pipe: {e}"));
            unsafe {
                libc::kill(-pid, libc::SIGKILL);
            }
            let _ = child.wait();
            return;
        }
    };

    let sess = Arc::new(Session {
        stream: Arc::clone(&stream),
        pid,
        tty,
        out: Arc::clone(&out),
        state: Mutex::new(SessionState {
            ptmx: ptmx_opt,
            stop_w: Some(stop_w),
            exited: false,
            hung_up: false,
        }),
    });

    if out.response(&Response::ok_pid(pid)).is_err() {
        sess.signal(libc::SIGKILL);
    }

    let abort_spawn = |sess: &Session,
                       stream: &TcpStream,
                       pump_handles: Vec<thread::JoinHandle<()>>,
                       child: &mut std::process::Child| {
        sess.out.mark_closed();
        let _ = stream.shutdown(Shutdown::Both);
        sess.signal(libc::SIGKILL);
        sess.stop_io();
        sess.clear_ptmx();
        sess.out.close();
        for h in pump_handles {
            let _ = h.join();
        }
        let _ = child.wait();
    };

    let tracker = Arc::new(PumpTracker::new(outputs.len()));
    let mut pump_handles = Vec::with_capacity(outputs.len());
    for (typ, fd) in outputs {
        let s = Arc::clone(&sess);
        let sr = Arc::clone(&stop_r);
        let tr = Arc::clone(&tracker);
        match spawn_helper(move || pump_fd(s, fd, sr, typ, tr)) {
            Ok(h) => pump_handles.push(h),
            Err(e) => {
                crate::log_line(&format!("exec pid {pid}: spawn pump: {e}"));
                abort_spawn(&sess, &stream, pump_handles, &mut child);
                return;
            }
        }
    }

    let (stdin_tx, stdin_rx) = mpsc::sync_channel::<Vec<u8>>(16);
    let stdin_stop = Arc::clone(&stop_r);
    if let Err(e) = spawn_helper(move || stdin_writer_loop(stdin_rx, Some(stdin_target), stdin_stop))
    {
        crate::log_line(&format!("exec pid {pid}: spawn stdin writer: {e}"));
        abort_spawn(&sess, &stream, pump_handles, &mut child);
        return;
    }

    {
        let s = Arc::clone(&sess);
        if let Err(e) = spawn_helper(move || read_frames_loop(s, cr, tty, stdin_tx)) {
            crate::log_line(&format!("exec pid {pid}: spawn frame reader: {e}"));
            abort_spawn(&sess, &stream, pump_handles, &mut child);
            return;
        }
    }

    let status = exit_status(child.wait());
    {
        let mut g = sess.state.lock().unwrap_or_else(|e| e.into_inner());
        g.exited = true;
    }

    if !tracker.wait_timeout(DRAIN_TIMEOUT) {
        sess.stop_io();
        if !tracker.wait_timeout(DRAIN_POLL_GRACE) {
            let _ = stream.shutdown(Shutdown::Both);
        }
        if !tty {
            for h in pump_handles.drain(..) {
                let _ = h.join();
            }
        }
    }

    let _ = out.finish(status);
    let _ = stream.shutdown(Shutdown::Both);
    sess.stop_io();
    if tty {
        sess.clear_ptmx();
    }
    for h in pump_handles {
        let _ = h.join();
    }
    crate::log_line(&format!("exit pid {pid}: status {status}"));
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt;

    #[test]
    fn merge_env_hides_values() {
        let err = merge_env(&[], &["API_TOKEN=hunter2\0".to_string()]).unwrap_err();
        assert!(!err.contains("hunter2"));
        assert!(err.contains("API_TOKEN"));
    }

    #[test]
    fn clean_path_works() {
        assert_eq!(clean_abs_path("/"), "/");
        assert_eq!(clean_abs_path("/a/b/../c//d/."), "/a/c/d");
        assert_eq!(clean_abs_path("/../../x"), "/x");
    }

    #[test]
    fn pre_exec_chdir_reports_working_directory_error() {
        if unsafe { libc::geteuid() } == 0 {
            return;
        }
        let dir =
            std::env::temp_dir().join(format!("kido-agent-chdir-test-{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        let locked = dir.join("locked");
        fs::create_dir(&locked).unwrap();
        fs::set_permissions(&locked, fs::Permissions::from_mode(0o000)).unwrap();

        let req = Request {
            op: proto::OP_EXEC.into(),
            args: vec!["/usr/bin/true".into()],
            dir: locked.to_string_lossy().into_owned(),
            ..Default::default()
        };
        let spec = prepare(&req).unwrap();
        let err = spawn_spec_command(
            &spec,
            &req.args,
            false,
            Stdio::null(),
            Stdio::null(),
            Stdio::null(),
        )
        .unwrap_err();

        let _ = fs::set_permissions(&locked, fs::Permissions::from_mode(0o700));
        let _ = fs::remove_dir_all(&dir);
        assert!(
            err.starts_with("working directory:"),
            "expected working directory prefix, got {err:?}"
        );
    }

    #[test]
    fn hangup_does_not_deadlock_when_pump_blocked_in_write_all() {
        let ln = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = ln.local_addr().unwrap();
        let client = TcpStream::connect(addr).unwrap();
        let (server, _) = ln.accept().unwrap();
        let small: libc::c_int = 4096;
        unsafe {
            libc::setsockopt(
                server.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_SNDBUF,
                (&small as *const libc::c_int).cast::<libc::c_void>(),
                std::mem::size_of::<libc::c_int>() as libc::socklen_t,
            );
            libc::setsockopt(
                client.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_RCVBUF,
                (&small as *const libc::c_int).cast::<libc::c_void>(),
                std::mem::size_of::<libc::c_int>() as libc::socklen_t,
            );
        }

        let server = Arc::new(server);
        let (cw, _) = proto::new_channel_pair(
            &[0x11u8; 32],
            [0x22u8; 32],
            false,
            DeadlineStream::new(Arc::clone(&server), None),
            io::empty(),
        );
        let out = Arc::new(FrameWriter::new(cw));
        let sess = Arc::new(Session {
            stream: Arc::clone(&server),
            pid: 0,
            tty: false,
            out: Arc::clone(&out),
            state: Mutex::new(SessionState {
                ptmx: None,
                stop_w: None,
                exited: true,
                hung_up: false,
            }),
        });

        let out_writer = Arc::clone(&out);
        let writer_handle = thread::spawn(move || {
            let chunk = vec![0x55u8; 32 << 10];
            while out_writer.frame(FRAME_STDOUT, &chunk).is_ok() {}
        });

        let wait_blocked = Instant::now();
        let mut observed_locked = false;
        while wait_blocked.elapsed() < Duration::from_secs(3) {
            if out.inner.try_lock().is_err() {
                thread::sleep(Duration::from_millis(50));
                if out.inner.try_lock().is_err() {
                    observed_locked = true;
                    break;
                }
            }
            thread::sleep(Duration::from_millis(5));
        }
        assert!(observed_locked, "pump writer never blocked in write_all");

        let hangup_start = Instant::now();
        let sess_h = Arc::clone(&sess);
        let hangup_handle = thread::spawn(move || sess_h.hangup());

        let (done_tx, done_rx) = mpsc::channel();
        thread::spawn(move || {
            let _ = hangup_handle.join();
            let _ = writer_handle.join();
            let _ = done_tx.send(());
        });
        done_rx
            .recv_timeout(Duration::from_secs(2))
            .expect("Session::hangup deadlocked against blocked write_all");
        assert!(hangup_start.elapsed() < Duration::from_secs(2));
        drop(client);
    }

    #[test]
    fn tty_hangup_signals_foreground_pgrp() {
        let dir =
            std::env::temp_dir().join(format!("kido-agent-tty-fg-hup-{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();
        let marker = dir.join("fg_hup");
        let pgid_file = dir.join("pgids");

        let ln = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = ln.local_addr().unwrap();
        let client = Arc::new(TcpStream::connect(addr).unwrap());
        let (server, _) = ln.accept().unwrap();
        let server = Arc::new(server);

        let secret = [0x33u8; 32];
        let tr = [0x44u8; 32];
        let (srv_w, srv_r) = proto::new_channel_pair(
            &secret,
            tr,
            false,
            DeadlineStream::new(Arc::clone(&server), None),
            BufReader::new(DeadlineStream::new(Arc::clone(&server), None)),
        );
        let (mut cli_w, mut cli_r) = proto::new_channel_pair(
            &secret,
            tr,
            true,
            DeadlineStream::new(Arc::clone(&client), None),
            BufReader::new(DeadlineStream::new(Arc::clone(&client), None)),
        );

        let req = Request {
            op: proto::OP_EXEC.into(),
            args: vec!["/bin/sh".into()],
            env: vec!["PS1=$ ".into()],
            tty: true,
            rows: 24,
            cols: 80,
            ..Default::default()
        };

        let srv_handle = thread::spawn(move || handle_exec(server, srv_w, srv_r, &req));

        let (typ, resp_bytes) = cli_r.read_frame().unwrap();
        assert_eq!(typ, FRAME_RESPONSE);
        let resp: serde_json::Value = serde_json::from_slice(&resp_bytes).unwrap();
        assert_eq!(resp["ok"], true);
        let leader_pid = resp["pid"].as_i64().unwrap();

        let cmd_line = format!(
            "trap ':' HUP\n/bin/sh -c 'trap \"echo hup > {m}; exit 0\" HUP; echo $$ $(ps -o pgid= -p $$) > {p}; echo FG_READY; while :; do sleep 0.05; done'\n",
            m = marker.display(),
            p = pgid_file.display(),
        );
        cli_w
            .write_frame(FRAME_STDIN, cmd_line.as_bytes())
            .unwrap();

        let mut acc = String::new();
        let wait_ready = Instant::now();
        while !acc.contains("\nFG_READY") && wait_ready.elapsed() < Duration::from_secs(5) {
            let (t, data) = cli_r.read_frame().unwrap();
            if t == FRAME_STDOUT {
                acc.push_str(&String::from_utf8_lossy(&data));
            }
        }
        assert!(acc.contains("\nFG_READY"), "never saw FG_READY output: {acc:?}");

        let pgids = fs::read_to_string(&pgid_file).unwrap();
        let mut parts = pgids.split_whitespace();
        let _child_pid: i64 = parts.next().unwrap().parse().unwrap();
        let fg_pgid: i64 = parts.next().unwrap().parse().unwrap();
        assert_ne!(
            fg_pgid, leader_pid,
            "foreground child must run in a distinct process group from session leader"
        );

        let _ = client.shutdown(Shutdown::Both);

        let wait_marker = Instant::now();
        while !marker.exists() && wait_marker.elapsed() < Duration::from_secs(5) {
            thread::sleep(Duration::from_millis(20));
        }
        assert!(
            marker.exists(),
            "TTY hangup failed to deliver SIGHUP to foreground process group {fg_pgid} (leader {leader_pid})"
        );

        let _ = srv_handle.join();
        let _ = fs::remove_dir_all(&dir);
    }
}
