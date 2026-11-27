use std::collections::{HashMap, HashSet};
use std::ffi::CString;
use std::io::{self, BufReader};
use std::mem::size_of;
use std::net::{Shutdown, TcpStream};
use std::os::fd::AsRawFd;
use std::ptr;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use crate::auth::RateLog;
use crate::proto::{
    ChannelReader, ChannelWriter, DeadlineStream, FRAME_NOTIFY, FRAME_RESPONSE, Request, Response,
};

pub const MAX_NAME: usize = 1023;
pub const MAX_DEPTH: usize = 64;
pub const MAX_PATH: usize = 4096;
pub const MAX_BATCH: usize = 32 << 10;
pub const RECORD_HEADER: usize = 48;

const RECORD_PATH: u8 = 1;
const RECORD_FLUSH: u8 = 2;
const FLAG_NAME: u8 = 1;
const FLAG_REPLACED: u8 = 2;

const NOTIFY_IDLE: Duration = Duration::from_secs(90);
const NOTIFY_RECHECK: Duration = Duration::from_secs(1);
const VIRTIOFS_IOC_NOTIFY: libc::c_ulong = 0x8010_5601;

const O_EVTONLY: libc::c_int = 0x0000_8000;
const O_NOFOLLOW_ANY: libc::c_int = 0x2000_0000;
const TCP_KEEPALIVE: libc::c_int = 0x10;
const TCP_KEEPINTVL: libc::c_int = 0x101;

pub const ERR_UNSUPPORTED: &str = "the virtio-fs driver takes no notifications";
pub const ERR_NO_MOUNTS: &str = "no share of the session is mounted yet";

unsafe extern "C" {
    fn ffsctl(
        fd: libc::c_int,
        request: libc::c_ulong,
        data: *mut libc::c_void,
        options: libc::c_uint,
    ) -> libc::c_int;
}

pub fn valid_share(name: &str) -> bool {
    let b = name.as_bytes();
    if b.is_empty() || b.len() > 64 || name == "." || name == ".." {
        return false;
    }
    b.iter()
        .all(|&c| c.is_ascii_alphanumeric() || matches!(c, b'_' | b'.' | b'-'))
}

pub fn valid_path_bytes(p: &[u8]) -> bool {
    if p.is_empty() || p.len() > MAX_PATH || p.contains(&0) {
        return false;
    }
    let mut depth = 0usize;
    for comp in p.split(|&b| b == b'/') {
        depth += 1;
        if depth > MAX_DEPTH
            || comp.is_empty()
            || comp == b"."
            || comp == b".."
            || comp.len() > MAX_NAME
        {
            return false;
        }
    }
    true
}

pub fn flush_batch() -> [u8; RECORD_HEADER] {
    let mut b = [0u8; RECORD_HEADER];
    b[0..2].copy_from_slice(&(RECORD_HEADER as u16).to_le_bytes());
    b[2] = RECORD_FLUSH;
    b
}

pub fn decode_frame(p: &[u8]) -> Result<(&str, &[u8]), &'static str> {
    if p.is_empty() {
        return Ok(("", &[]));
    }
    let n = p[0] as usize;
    if 1 + n >= p.len() || p.len() - 1 - n > MAX_BATCH {
        return Err("notify: malformed record");
    }
    let share = std::str::from_utf8(&p[1..1 + n]).map_err(|_| "notify: malformed record")?;
    if !valid_share(share) {
        return Err("notify: malformed record");
    }
    Ok((share, &p[1 + n..]))
}

pub fn validate_batch(mut b: &[u8]) -> Result<(), &'static str> {
    if b.is_empty() || b.len() > MAX_BATCH {
        return Err("notify: malformed record");
    }
    while !b.is_empty() {
        if b.len() < RECORD_HEADER {
            return Err("notify: malformed record");
        }
        let n = u16::from_le_bytes([b[0], b[1]]) as usize;
        let plen = u16::from_le_bytes([b[6], b[7]]) as usize;
        if n != RECORD_HEADER + plen
            || n > b.len()
            || (b[4] & !(FLAG_NAME | FLAG_REPLACED)) != 0
            || b[5] != 0
        {
            return Err("notify: malformed record");
        }
        let kind = b[2];
        let state = b[3];
        let mtime_nsec = u32::from_le_bytes([b[40], b[41], b[42], b[43]]);
        let ctime_nsec = u32::from_le_bytes([b[44], b[45], b[46], b[47]]);
        if state > 2 || mtime_nsec >= 1_000_000_000 || ctime_nsec >= 1_000_000_000 {
            return Err("notify: malformed record");
        }
        match kind {
            RECORD_PATH => {
                if !valid_path_bytes(&b[RECORD_HEADER..n]) {
                    return Err("notify: malformed record");
                }
            }
            RECORD_FLUSH => {
                if plen != 0 || b[3..RECORD_HEADER].iter().any(|&x| x != 0) {
                    return Err("notify: malformed record");
                }
            }
            _ => return Err("notify: malformed record"),
        }
        b = &b[n..];
    }
    Ok(())
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MountInfo {
    pub from: String,
    pub typ: String,
    pub on: String,
    pub fsid: [i32; 2],
}

pub trait MountTable: Send + Sync {
    fn list(&self) -> io::Result<Vec<MountInfo>>;
    fn open(&self, path: &str) -> io::Result<libc::c_int>;
    fn info(&self, fd: libc::c_int) -> io::Result<MountInfo>;
    fn fsctl(&self, fd: libc::c_int, batch: Option<&[u8]>) -> io::Result<()>;
    fn close(&self, fd: libc::c_int);
}

pub struct SysMounts;

fn c_char_buf_to_string(buf: &[libc::c_char]) -> String {
    let bytes: &[u8] =
        unsafe { std::slice::from_raw_parts(buf.as_ptr().cast::<u8>(), buf.len()) };
    let len = bytes.iter().position(|&b| b == 0).unwrap_or(bytes.len());
    String::from_utf8_lossy(&bytes[..len]).into_owned()
}

const _: () = assert!(size_of::<libc::fsid_t>() == size_of::<[i32; 2]>());

fn statfs_info(st: &libc::statfs) -> MountInfo {
    let fsid: [i32; 2] = unsafe { std::mem::transmute(st.f_fsid) };
    MountInfo {
        from: c_char_buf_to_string(&st.f_mntfromname),
        typ: c_char_buf_to_string(&st.f_fstypename),
        on: c_char_buf_to_string(&st.f_mntonname),
        fsid,
    }
}

#[repr(C)]
struct VirtiofsNotifyArg {
    ptr: u64,
    len: u32,
    flags: u32,
}

impl MountTable for SysMounts {
    fn list(&self) -> io::Result<Vec<MountInfo>> {
        let count = unsafe { libc::getfsstat(ptr::null_mut(), 0, libc::MNT_NOWAIT) };
        if count < 0 {
            return Err(io::Error::last_os_error());
        }
        let cap = (count as usize).saturating_add(8);
        let byte_len = cap
            .checked_mul(size_of::<libc::statfs>())
            .and_then(|v| libc::c_int::try_from(v).ok())
            .ok_or_else(|| io::Error::from_raw_os_error(libc::ENOMEM))?;
        let mut buf: Vec<libc::statfs> = vec![unsafe { std::mem::zeroed() }; cap];
        let got = unsafe { libc::getfsstat(buf.as_mut_ptr(), byte_len, libc::MNT_NOWAIT) };
        if got < 0 {
            return Err(io::Error::last_os_error());
        }
        let n = (got as usize).min(cap);
        Ok(buf[..n].iter().map(statfs_info).collect())
    }

    fn open(&self, path: &str) -> io::Result<libc::c_int> {
        let cpath = CString::new(path).map_err(|_| io::Error::from_raw_os_error(libc::EINVAL))?;
        let flags = O_EVTONLY | libc::O_DIRECTORY | O_NOFOLLOW_ANY | libc::O_CLOEXEC;
        let fd = unsafe { libc::open(cpath.as_ptr(), flags, 0) };
        if fd < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(fd)
        }
    }

    fn info(&self, fd: libc::c_int) -> io::Result<MountInfo> {
        let mut st: libc::statfs = unsafe { std::mem::zeroed() };
        if unsafe { libc::fstatfs(fd, &mut st) } != 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(statfs_info(&st))
    }

    fn fsctl(&self, fd: libc::c_int, batch: Option<&[u8]>) -> io::Result<()> {
        let mut arg = VirtiofsNotifyArg {
            ptr: 0,
            len: 0,
            flags: 0,
        };
        if let Some(b) = batch
            && !b.is_empty()
        {
            arg.len = u32::try_from(b.len())
                .map_err(|_| io::Error::from_raw_os_error(libc::EINVAL))?;
            arg.ptr = b.as_ptr() as usize as u64;
        }
        let rc = unsafe {
            ffsctl(
                fd,
                VIRTIOFS_IOC_NOTIFY,
                (&mut arg as *mut VirtiofsNotifyArg).cast::<libc::c_void>(),
                0,
            )
        };
        if rc != 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(())
        }
    }

    fn close(&self, fd: libc::c_int) {
        unsafe {
            libc::close(fd);
        }
    }
}

struct OpenMount {
    fd: libc::c_int,
    info: MountInfo,
}

#[derive(Default)]
struct ShareMounts {
    open: Vec<OpenMount>,
    has_open: bool,
    listed: Option<Instant>,
}

struct NotifyMounts {
    table: Arc<dyn MountTable>,
    shares: HashMap<String, ShareMounts>,
    order: Vec<String>,
}

impl NotifyMounts {
    fn new(table: Arc<dyn MountTable>, names: &[String]) -> Self {
        let mut shares = HashMap::new();
        let mut order = Vec::new();
        for name in names {
            if !shares.contains_key(name) {
                shares.insert(name.clone(), ShareMounts::default());
                order.push(name.clone());
            }
        }
        Self {
            table,
            shares,
            order,
        }
    }

    fn find(&mut self, share: &str, now: Instant) -> Result<&[OpenMount], String> {
        let Some(sm) = self.shares.get(share) else {
            return Err(format!("share {share:?} is not part of this session"));
        };
        if sm.has_open
            && let Some(listed) = sm.listed
            && now.saturating_duration_since(listed) < NOTIFY_RECHECK
        {
            return Ok(&self.shares[share].open);
        }
        let list = self.table.list().map_err(|e| e.to_string())?;
        let want_from = format!("kido:{share}");
        let listing: Vec<MountInfo> = list
            .into_iter()
            .filter(|e| e.typ == "virtiofs" && e.from == want_from)
            .collect();

        let same = {
            let sm = self.shares.get_mut(share).unwrap();
            sm.listed = Some(now);
            sm.has_open
                && listing.len() == sm.open.len()
                && listing.iter().zip(sm.open.iter()).all(|(a, b)| a == &b.info)
        };
        if same {
            return Ok(&self.shares[share].open);
        }
        self.forget(share);
        let mut opened = Vec::new();
        for e in listing {
            let Ok(fd) = self.table.open(&e.on) else {
                continue;
            };
            match self.table.info(fd) {
                Ok(got) if got.typ == e.typ && got.from == e.from && got.fsid == e.fsid => {
                    opened.push(OpenMount { fd, info: e });
                }
                _ => self.table.close(fd),
            }
        }
        if opened.is_empty() {
            return Err(format!("share {share:?} is not mounted"));
        }
        let sm = self.shares.get_mut(share).unwrap();
        sm.open = opened;
        sm.has_open = true;
        Ok(&sm.open)
    }

    fn forget(&mut self, share: &str) {
        if let Some(sm) = self.shares.get_mut(share) {
            for o in sm.open.drain(..) {
                self.table.close(o.fd);
            }
            sm.has_open = false;
        }
    }

    fn send(&mut self, share: &str, batch: &[u8], now: Instant) -> Result<(), String> {
        let mut took: HashSet<[i32; 2]> = HashSet::new();
        let mut last_err: Option<String> = None;
        for _ in 0..2 {
            let mounts: Vec<(libc::c_int, [i32; 2])> = self
                .find(share, now)?
                .iter()
                .map(|o| (o.fd, o.info.fsid))
                .collect();
            last_err = None;
            for (fd, fsid) in mounts {
                if took.contains(&fsid) {
                    continue;
                }
                match self.table.fsctl(fd, Some(batch)) {
                    Ok(()) => {
                        took.insert(fsid);
                    }
                    Err(e) => {
                        last_err = Some(e.to_string());
                    }
                }
            }
            if last_err.is_none() {
                return Ok(());
            }
            self.forget(share);
        }
        Err(last_err.unwrap_or_else(|| "fsctl failed".into()))
    }

    fn probe(&mut self, now: Instant) -> Result<(), String> {
        let names = self.order.clone();
        for share in names {
            let (fd, info) = match self.find(&share, now) {
                Ok(m) => (m[0].fd, m[0].info.clone()),
                Err(_) => continue,
            };
            match self.table.fsctl(fd, None) {
                Ok(()) => return Ok(()),
                Err(e) => {
                    let code = e.raw_os_error().unwrap_or(0);
                    if matches!(code, libc::ENOTTY | libc::EINVAL | libc::ENOTSUP) {
                        match self.table.info(fd) {
                            Ok(got) if got == info => return Err(ERR_UNSUPPORTED.into()),
                            _ => {
                                self.forget(&share);
                                continue;
                            }
                        }
                    }
                    return Err(e.to_string());
                }
            }
        }
        Err(ERR_NO_MOUNTS.into())
    }

    fn close_all(&mut self) {
        let names = self.order.clone();
        for share in names {
            self.forget(&share);
        }
    }
}

impl Drop for NotifyMounts {
    fn drop(&mut self) {
        self.close_all();
    }
}

fn enable_keepalive(stream: &TcpStream, secs: libc::c_int) {
    let fd = stream.as_raw_fd();
    let one: libc::c_int = 1;
    unsafe {
        libc::setsockopt(
            fd,
            libc::SOL_SOCKET,
            libc::SO_KEEPALIVE,
            (&one as *const libc::c_int).cast::<libc::c_void>(),
            size_of::<libc::c_int>() as libc::socklen_t,
        );
        libc::setsockopt(
            fd,
            libc::IPPROTO_TCP,
            TCP_KEEPALIVE,
            (&secs as *const libc::c_int).cast::<libc::c_void>(),
            size_of::<libc::c_int>() as libc::socklen_t,
        );
        libc::setsockopt(
            fd,
            libc::IPPROTO_TCP,
            TCP_KEEPINTVL,
            (&secs as *const libc::c_int).cast::<libc::c_void>(),
            size_of::<libc::c_int>() as libc::socklen_t,
        );
    }
}

#[allow(clippy::too_many_arguments)]
pub fn handle_notify(
    stream: Arc<TcpStream>,
    mut cw: ChannelWriter<DeadlineStream>,
    mut cr: ChannelReader<BufReader<DeadlineStream>>,
    req: &Request,
    mounts: Arc<dyn MountTable>,
    notify_conn: &Mutex<Option<Arc<TcpStream>>>,
    notify_send: &Mutex<()>,
    notify_log: &RateLog,
) {
    cw.writer_mut()
        .set_deadline(Some(Instant::now() + Duration::from_secs(10)));
    for name in &req.args {
        if !valid_share(name) {
            let _ = cw.write_message(
                FRAME_RESPONSE,
                &Response::err(format!("bad share name {name:?}")),
            );
            return;
        }
    }

    let mut m = NotifyMounts::new(mounts, &req.args);
    {
        let mut active = notify_conn.lock().unwrap_or_else(|e| e.into_inner());
        if let Err(e) = m.probe(Instant::now()) {
            drop(active);
            let _ = cw.write_message(FRAME_RESPONSE, &Response::err(e));
            return;
        }
        if let Some(old) = active.replace(Arc::clone(&stream)) {
            let _ = old.shutdown(Shutdown::Both);
        }
    }

    struct ActiveGuard<'a> {
        slot: &'a Mutex<Option<Arc<TcpStream>>>,
        stream: Arc<TcpStream>,
    }
    impl Drop for ActiveGuard<'_> {
        fn drop(&mut self) {
            let mut active = self.slot.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(ref cur) = *active
                && Arc::ptr_eq(cur, &self.stream)
            {
                *active = None;
            }
        }
    }
    let _guard = ActiveGuard {
        slot: notify_conn,
        stream: Arc::clone(&stream),
    };

    let _ = cw.write_message(FRAME_RESPONSE, &Response::ok_version());
    cw.writer_mut().set_deadline(None);
    enable_keepalive(&stream, 15);

    loop {
        cr.reader_mut()
            .get_mut()
            .set_deadline(Some(Instant::now() + NOTIFY_IDLE));
        let (typ, payload) = match cr.read_frame() {
            Ok(f) => f,
            Err(_) => return,
        };
        if typ != FRAME_NOTIFY {
            return;
        }
        let (share, batch) = match decode_frame(&payload) {
            Ok(v) => v,
            Err(e) => {
                notify_log.log(&format!("notify: {e}"));
                return;
            }
        };
        if share.is_empty() {
            continue;
        }
        let flush = flush_batch();
        let to_send: &[u8] = if validate_batch(batch).is_err() {
            notify_log.log(&format!(
                "notify {share}: a malformed batch; flushing the share"
            ));
            &flush
        } else {
            batch
        };
        let _send_guard = notify_send.lock().unwrap_or_else(|e| e.into_inner());
        if let Err(e) = m.send(share, to_send, Instant::now()) {
            notify_log.log(&format!("notify {share}: {e}"));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hex_decode(s: &str) -> Vec<u8> {
        (0..s.len())
            .step_by(2)
            .map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap())
            .collect()
    }

    #[test]
    fn golden_batch_validates() {
        let golden = hex_decode(
            "3800010200000800a4810000010000002a00000000000000c086fd6a00000000c186fd6a0000000015cd5b07b168de3a6469722f66696c65340001010100040000000000000000000000000000000000000000000000000000000000000000000000000000000000676f6e65300002000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
        );
        validate_batch(&golden).unwrap();
        validate_batch(&flush_batch()).unwrap();

        let mut bad = golden.clone();
        bad[2] = 9;
        assert!(validate_batch(&bad).is_err());

        let mut non_utf8 = vec![0u8; RECORD_HEADER + 3];
        non_utf8[0..2].copy_from_slice(&((RECORD_HEADER + 3) as u16).to_le_bytes());
        non_utf8[2] = RECORD_PATH;
        non_utf8[3] = 1;
        non_utf8[6..8].copy_from_slice(&3u16.to_le_bytes());
        non_utf8[RECORD_HEADER..].copy_from_slice(&[b'a', 0xff, b'b']);
        validate_batch(&non_utf8).unwrap();
    }

    #[test]
    fn bounded_c_char_conversion() {
        let no_nul = [b'a' as libc::c_char; 4];
        assert_eq!(c_char_buf_to_string(&no_nul), "aaaa");
        let with_nul = [b'x' as libc::c_char, 0, b'y' as libc::c_char];
        assert_eq!(c_char_buf_to_string(&with_nul), "x");
    }

    #[test]
    fn share_and_frame_validation() {
        assert!(valid_share("m0"));
        assert!(valid_share("A_b.c-9"));
        assert!(valid_share("..."));
        assert!(!valid_share(""));
        assert!(!valid_share("."));
        assert!(!valid_share(".."));
        assert!(!valid_share("a/b"));
        assert!(!valid_share(&"x".repeat(65)));

        assert_eq!(decode_frame(&[]).unwrap(), ("", &[][..]));
        let mut f = vec![2, b'm', b'0'];
        f.extend_from_slice(&flush_batch());
        let (s, b) = decode_frame(&f).unwrap();
        assert_eq!(s, "m0");
        assert_eq!(b, &flush_batch());
    }

    struct FakeMounts {
        state: Mutex<FakeState>,
    }

    #[derive(Default)]
    struct FakeState {
        entries: Vec<MountInfo>,
        seen: HashMap<String, MountInfo>,
        fds: HashMap<libc::c_int, String>,
        dead: HashSet<libc::c_int>,
        next: libc::c_int,
        probe_err: Option<i32>,
        calls: Vec<String>,
    }

    impl MountTable for FakeMounts {
        fn list(&self) -> io::Result<Vec<MountInfo>> {
            Ok(self.state.lock().unwrap().entries.clone())
        }
        fn open(&self, path: &str) -> io::Result<libc::c_int> {
            let mut st = self.state.lock().unwrap();
            st.next += 1;
            let fd = st.next;
            st.fds.insert(fd, path.to_string());
            Ok(fd)
        }
        fn info(&self, fd: libc::c_int) -> io::Result<MountInfo> {
            let st = self.state.lock().unwrap();
            let Some(path) = st.fds.get(&fd) else {
                return Err(io::Error::from_raw_os_error(libc::EBADF));
            };
            if st.dead.contains(&fd) {
                return Err(io::Error::from_raw_os_error(libc::EBADF));
            }
            if let Some(m) = st.seen.get(path) {
                return Ok(m.clone());
            }
            st.entries
                .iter()
                .find(|e| &e.on == path)
                .cloned()
                .ok_or_else(|| io::Error::from_raw_os_error(libc::ENOENT))
        }
        fn fsctl(&self, fd: libc::c_int, batch: Option<&[u8]>) -> io::Result<()> {
            let mut st = self.state.lock().unwrap();
            if batch.is_none() {
                if st.dead.contains(&fd) {
                    return Err(io::Error::from_raw_os_error(libc::ENOTTY));
                }
                if let Some(code) = st.probe_err {
                    return Err(io::Error::from_raw_os_error(code));
                }
                return Ok(());
            }
            let path = st.fds.get(&fd).cloned().unwrap_or_default();
            st.calls.push(path);
            if st.dead.contains(&fd) {
                return Err(io::Error::from_raw_os_error(libc::ENOTTY));
            }
            Ok(())
        }
        fn close(&self, fd: libc::c_int) {
            self.state.lock().unwrap().fds.remove(&fd);
        }
    }

    #[test]
    fn notify_mounts_filters_and_retries() {
        let vfs = |from: &str, on: &str, id: i32| MountInfo {
            from: from.into(),
            typ: "virtiofs".into(),
            on: on.into(),
            fsid: [id, id],
        };
        let fake = Arc::new(FakeMounts {
            state: Mutex::new(FakeState {
                entries: vec![
                    MountInfo {
                        from: "kido:m0".into(),
                        typ: "apfs".into(),
                        on: "/wrong".into(),
                        fsid: [0, 0],
                    },
                    vfs("kido:m0", "/swapped", 1),
                    vfs("kido:m0", "/data", 3),
                    vfs("kido:m0", "/again", 4),
                ],
                seen: HashMap::from([("/swapped".into(), vfs("kido:m0", "/swapped", 9))]),
                ..Default::default()
            }),
        });
        let mut m = NotifyMounts::new(fake.clone(), &["m0".into()]);
        let t0 = Instant::now();
        m.probe(t0).unwrap();
        m.send("m0", &flush_batch(), t0).unwrap();
        assert_eq!(fake.state.lock().unwrap().calls, vec!["/data", "/again"]);

        let dead_fd = m.shares["m0"].open[1].fd;
        {
            let mut st = fake.state.lock().unwrap();
            st.dead.insert(dead_fd);
            st.entries[3] = vfs("kido:m0", "/again", 5);
            st.calls.clear();
        }
        m.send("m0", &flush_batch(), t0).unwrap();
        assert_eq!(
            fake.state.lock().unwrap().calls,
            vec!["/data", "/again", "/again"]
        );
        m.close_all();
        assert!(fake.state.lock().unwrap().fds.is_empty());
    }
}
