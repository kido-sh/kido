use std::io;
use std::os::unix::io::{FromRawFd, OwnedFd, RawFd};
use std::path::PathBuf;

pub const NOTIFY_EVENT: u32 = 1;
pub const NOTIFY_FLUSH: u32 = 2;
pub const NOTIFY_MAX_PATH: usize = 4096;

pub const FLAG_ROOT_CHANGED: u32 = 0x20;
pub const FLAG_ITEM_CREATED: u32 = 0x100;
pub const FLAG_ITEM_REMOVED: u32 = 0x200;
pub const FLAG_ITEM_RENAMED: u32 = 0x800;

/// Sentinel file ID attached to IN_MOVED_TO / IN_DELETE so the notify relay
/// recognizes a first-ever replace of a pre-existing file even before its
/// inode was recorded in relayPending.known.
pub const SYNTHETIC_REPLACE_ID: u64 = u64::MAX;

const HEADER_LEN: usize = 20;
const LARGEST_RECORD: usize = HEADER_LEN + NOTIFY_MAX_PATH;
const DEFAULT_QUEUE_CAP: usize = 1 << 20;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DrainResult {
    Empty,
    Pending,
    Broken,
}

/// Framed non-blocking pipe writer matching `vmm/notify.c` and `internal/notify/notify.go`.
/// Every record is `[u32 body_len][u32 kind][u32 flags][u64 fileid][path bytes]`.
pub struct NotifyQueue {
    buf: Vec<u8>,
    cap: usize,
    off: usize,
    broken: bool,
}

impl NotifyQueue {
    pub fn new(cap: usize) -> Self {
        let cap = cap.max(2 * LARGEST_RECORD);
        Self {
            buf: Vec::with_capacity(cap.min(64 * 1024)),
            cap,
            off: 0,
            broken: false,
        }
    }

    pub fn is_empty(&self) -> bool {
        self.off >= self.buf.len()
    }

    #[cfg(test)]
    pub fn is_broken(&self) -> bool {
        self.broken
    }

    fn record_total_at(&self, pos: usize) -> usize {
        let body = u32::from_le_bytes(self.buf[pos..pos + 4].try_into().unwrap()) as usize;
        4 + body
    }

    fn compact(&mut self) {
        let mut start = 0usize;
        while start < self.buf.len() {
            let end = start + self.record_total_at(start);
            if end > self.off {
                break;
            }
            start = end;
        }
        if start > 0 {
            self.buf.drain(..start);
            self.off -= start;
        }
    }

    fn append_raw(&mut self, kind: u32, flags: u32, fileid: u64, path: &[u8]) -> bool {
        let rec_len = HEADER_LEN + path.len();
        if self.buf.len() + rec_len > self.cap {
            self.compact();
        }
        if self.buf.len() + rec_len > self.cap {
            return false;
        }
        let body_len = (HEADER_LEN - 4 + path.len()) as u32;
        self.buf.extend_from_slice(&body_len.to_le_bytes());
        self.buf.extend_from_slice(&kind.to_le_bytes());
        self.buf.extend_from_slice(&flags.to_le_bytes());
        self.buf.extend_from_slice(&fileid.to_le_bytes());
        self.buf.extend_from_slice(path);
        true
    }

    pub fn push_flush(&mut self) {
        if self.broken {
            return;
        }
        self.compact();
        let keep = if self.off > 0 && !self.buf.is_empty() {
            self.record_total_at(0)
        } else {
            0
        };
        self.buf.truncate(keep);
        let _ = self.append_raw(NOTIFY_FLUSH, 0, 0, &[]);
    }

    pub fn push_event(&mut self, flags: u32, fileid: u64, path: &[u8]) {
        if self.broken {
            return;
        }
        if path.len() > NOTIFY_MAX_PATH || !self.append_raw(NOTIFY_EVENT, flags, fileid, path) {
            self.push_flush();
        }
    }

    pub fn drain_fd(&mut self, fd: RawFd) -> DrainResult {
        if self.broken {
            return DrainResult::Broken;
        }
        while self.off < self.buf.len() {
            let rem = &self.buf[self.off..];
            let n = unsafe { libc::write(fd, rem.as_ptr().cast::<libc::c_void>(), rem.len()) };
            if n > 0 {
                self.off += n as usize;
                continue;
            }
            if n < 0 {
                let err = io::Error::last_os_error()
                    .raw_os_error()
                    .unwrap_or(libc::EIO);
                if err == libc::EINTR {
                    continue;
                }
                if err == libc::EAGAIN || err == libc::EWOULDBLOCK {
                    return DrainResult::Pending;
                }
            }
            self.broken = true;
            self.buf.clear();
            self.off = 0;
            return DrainResult::Broken;
        }
        self.buf.clear();
        self.off = 0;
        DrainResult::Empty
    }
}

pub fn validate_notify_fd(fd: RawFd) -> Result<(), String> {
    if fd < 3 {
        return Err(format!("bad --notify-fd {fd}: must be >= 3"));
    }
    let mut st: libc::stat = unsafe { std::mem::zeroed() };
    if unsafe { libc::fstat(fd, &mut st) } != 0 || (st.st_mode & libc::S_IFMT) != libc::S_IFIFO {
        return Err(format!("bad --notify-fd {fd}: not a pipe"));
    }
    Ok(())
}

pub fn close_raw_fd(fd: RawFd) {
    if fd >= 0 {
        let _ = unsafe { OwnedFd::from_raw_fd(fd) };
    }
}

#[cfg(target_os = "linux")]
mod linux_impl {
    use super::*;
    use std::collections::HashMap;
    use std::ffi::{CStr, CString};
    use std::os::unix::io::AsRawFd;
    use std::path::Path;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::{Arc, Mutex};
    use std::thread::{self, JoinHandle};

    const MAX_WATCHES: usize = 8192;
    const MAX_DEPTH: usize = 64;
    const INOTIFY_BUF_BYTES: usize = 64 * 1024;
    const MAX_READS_PER_POLL: usize = 16;
    const MAX_PENDING_MOVES: usize = 1024;

    const SUBDIR_WATCH_MASK: u32 = libc::IN_MODIFY
        | libc::IN_ATTRIB
        | libc::IN_CLOSE_WRITE
        | libc::IN_MOVED_FROM
        | libc::IN_MOVED_TO
        | libc::IN_CREATE
        | libc::IN_DELETE
        | libc::IN_DELETE_SELF
        | libc::IN_ONLYDIR
        | libc::IN_EXCL_UNLINK;

    const ROOT_WATCH_MASK: u32 = SUBDIR_WATCH_MASK | libc::IN_MOVE_SELF;

    #[repr(C)]
    struct OpenHow {
        flags: u64,
        mode: u64,
        resolve: u64,
    }

    const RESOLVE_NO_MAGICLINKS: u64 = 0x02;
    const RESOLVE_NO_SYMLINKS: u64 = 0x04;
    const RESOLVE_BENEATH: u64 = 0x08;

    fn open_dir_beneath(parent_fd: RawFd, name: &CStr) -> Result<OwnedFd, i32> {
        let flags = libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC;
        let how = OpenHow {
            flags: (flags as u32) as u64,
            mode: 0,
            resolve: RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS,
        };
        loop {
            let rc = unsafe {
                libc::syscall(
                    libc::SYS_openat2,
                    parent_fd,
                    name.as_ptr(),
                    &how as *const OpenHow,
                    std::mem::size_of::<OpenHow>(),
                )
            };
            if rc >= 0 {
                return Ok(unsafe { OwnedFd::from_raw_fd(rc as RawFd) });
            }
            let err = io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO);
            if err == libc::EINTR {
                continue;
            }
            if err == libc::ENOSYS {
                break;
            }
            return Err(err);
        }
        loop {
            let fallback = unsafe { libc::openat(parent_fd, name.as_ptr(), flags) };
            if fallback >= 0 {
                return Ok(unsafe { OwnedFd::from_raw_fd(fallback) });
            }
            let err = io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO);
            if err == libc::EINTR {
                continue;
            }
            return Err(err);
        }
    }

    /// Opens an absolute watch root without following any intermediate or final
    /// symbolic link or magiclink.
    fn open_root_dir(root: &Path) -> Result<OwnedFd, i32> {
        let bytes = root.as_os_str().as_encoded_bytes();
        if !bytes.starts_with(b"/") {
            return Err(libc::EINVAL);
        }
        let dir_flags = libc::O_RDONLY | libc::O_DIRECTORY | libc::O_CLOEXEC;
        let slash_fd = loop {
            let rc = unsafe { libc::open(c"/".as_ptr(), dir_flags) };
            if rc >= 0 {
                break unsafe { OwnedFd::from_raw_fd(rc) };
            }
            let err = io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO);
            if err == libc::EINTR {
                continue;
            }
            return Err(err);
        };
        let trimmed = bytes.iter().position(|&b| b != b'/').map(|i| &bytes[i..]);
        let Some(rel_bytes) = trimmed else {
            return Ok(slash_fd);
        };
        let crel = CString::new(rel_bytes).map_err(|_| libc::EINVAL)?;
        let how = OpenHow {
            flags: (dir_flags as u32) as u64,
            mode: 0,
            resolve: RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS,
        };
        loop {
            let rc = unsafe {
                libc::syscall(
                    libc::SYS_openat2,
                    slash_fd.as_raw_fd(),
                    crel.as_ptr(),
                    &how as *const OpenHow,
                    std::mem::size_of::<OpenHow>(),
                )
            };
            if rc >= 0 {
                return Ok(unsafe { OwnedFd::from_raw_fd(rc as RawFd) });
            }
            let err = io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO);
            if err == libc::EINTR {
                continue;
            }
            if err == libc::ENOSYS {
                break;
            }
            return Err(err);
        }
        // ENOSYS fallback: step component by component with O_NOFOLLOW.
        let mut cur = slash_fd;
        for comp in rel_bytes.split(|&b| b == b'/') {
            if comp.is_empty() {
                continue;
            }
            if comp == b"." || comp == b".." {
                return Err(libc::EINVAL);
            }
            let ccomp = CString::new(comp).map_err(|_| libc::EINVAL)?;
            cur = open_dir_beneath(cur.as_raw_fd(), &ccomp)?;
        }
        Ok(cur)
    }

    fn add_watch_for_fd(inotify_fd: RawFd, dir_fd: RawFd, is_root: bool) -> Result<i32, i32> {
        let proc_path = CString::new(format!("/proc/self/fd/{dir_fd}")).unwrap();
        let mask = if is_root {
            ROOT_WATCH_MASK
        } else {
            SUBDIR_WATCH_MASK
        } | libc::IN_MASK_ADD;
        let wd = unsafe { libc::inotify_add_watch(inotify_fd, proc_path.as_ptr(), mask) };
        if wd >= 0 {
            Ok(wd)
        } else {
            Err(io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO))
        }
    }

    fn normalize_root(p: &Path) -> String {
        let s = p.to_string_lossy();
        if s == "/" {
            "/".to_string()
        } else {
            s.trim_end_matches('/').to_string()
        }
    }

    fn join_root_rel(root: &str, rel: &str) -> String {
        if rel.is_empty() {
            root.to_string()
        } else if root == "/" {
            format!("/{rel}")
        } else {
            format!("{root}/{rel}")
        }
    }

    #[derive(Clone, PartialEq, Eq)]
    struct WatchEntry {
        root_idx: usize,
        rel: String,
        is_root: bool,
    }

    struct PendingMove {
        root_idx: usize,
        old_rel: String,
    }

    struct WatchTree {
        inotify_fd: OwnedFd,
        roots: Vec<String>,
        root_fds: Vec<Option<OwnedFd>>,
        wd_to_dirs: HashMap<i32, Vec<WatchEntry>>,
        dir_to_wd: HashMap<(usize, String), i32>,
        pending_moves: HashMap<u32, PendingMove>,
    }

    impl WatchTree {
        fn new(inotify_fd: OwnedFd, watch_paths: &[PathBuf]) -> Self {
            let mut roots = Vec::with_capacity(watch_paths.len());
            let mut root_fds = Vec::with_capacity(watch_paths.len());
            for p in watch_paths {
                roots.push(normalize_root(p));
                root_fds.push(open_root_dir(p).ok());
            }
            Self {
                inotify_fd,
                roots,
                root_fds,
                wd_to_dirs: HashMap::new(),
                dir_to_wd: HashMap::new(),
                pending_moves: HashMap::new(),
            }
        }

        fn init_all_watches(&mut self, stop: &AtomicBool, q: &mut NotifyQueue) {
            for root_idx in 0..self.roots.len() {
                if stop.load(Ordering::Acquire) {
                    return;
                }
                let Some(ref rfd) = self.root_fds[root_idx] else {
                    q.push_flush();
                    continue;
                };
                let dup_rc = unsafe { libc::fcntl(rfd.as_raw_fd(), libc::F_DUPFD_CLOEXEC, 0) };
                if dup_rc < 0 {
                    q.push_flush();
                    continue;
                }
                let dir_fd = unsafe { OwnedFd::from_raw_fd(dup_rc) };
                self.watch_subtree(root_idx, String::new(), dir_fd, 0, false, stop, q);
            }
        }

        fn register_wd(&mut self, wd: i32, root_idx: usize, rel: String, is_root: bool) {
            if let Some(prev_wd) = self.dir_to_wd.insert((root_idx, rel.clone()), wd) {
                if prev_wd != wd {
                    self.detach_entry_from_wd(prev_wd, root_idx, &rel, true);
                }
            }
            let list = self.wd_to_dirs.entry(wd).or_default();
            if let Some(existing) = list
                .iter_mut()
                .find(|e| e.root_idx == root_idx && e.rel == rel)
            {
                existing.is_root |= is_root;
            } else {
                list.push(WatchEntry {
                    root_idx,
                    rel,
                    is_root,
                });
            }
        }

        fn detach_entry_from_wd(&mut self, wd: i32, root_idx: usize, rel: &str, rm_kernel: bool) {
            let mut empty = false;
            if let Some(list) = self.wd_to_dirs.get_mut(&wd) {
                list.retain(|e| !(e.root_idx == root_idx && e.rel == rel));
                empty = list.is_empty();
            }
            if empty {
                self.wd_to_dirs.remove(&wd);
                if rm_kernel {
                    unsafe {
                        libc::inotify_rm_watch(self.inotify_fd.as_raw_fd(), wd);
                    }
                }
            }
        }

        fn remove_wd_ignored(&mut self, wd: i32) {
            if let Some(entries) = self.wd_to_dirs.remove(&wd) {
                for e in entries {
                    if self.dir_to_wd.get(&(e.root_idx, e.rel.clone())) == Some(&wd) {
                        self.dir_to_wd.remove(&(e.root_idx, e.rel));
                    }
                }
            }
        }

        fn remove_subtree_watches(&mut self, root_idx: usize, rel_prefix: &str) {
            let prefix = if rel_prefix.is_empty() {
                String::new()
            } else {
                format!("{rel_prefix}/")
            };
            let to_remove: Vec<(String, i32)> = self
                .dir_to_wd
                .iter()
                .filter(|((ridx, rel), _)| {
                    *ridx == root_idx
                        && (rel_prefix.is_empty()
                            || rel == rel_prefix
                            || rel.starts_with(&prefix))
                })
                .map(|((_, rel), &wd)| (rel.clone(), wd))
                .collect();
            for (rel, wd) in to_remove {
                if self.dir_to_wd.get(&(root_idx, rel.clone())) == Some(&wd) {
                    self.dir_to_wd.remove(&(root_idx, rel.clone()));
                }
                self.detach_entry_from_wd(wd, root_idx, &rel, true);
            }
        }

        fn flush_unmatched_moves(&mut self) {
            let leftover: Vec<PendingMove> = self.pending_moves.drain().map(|(_, pm)| pm).collect();
            for pm in leftover {
                self.remove_subtree_watches(pm.root_idx, &pm.old_rel);
            }
        }

        #[allow(clippy::too_many_arguments)]
        fn watch_subtree(
            &mut self,
            root_idx: usize,
            rel: String,
            dir_fd: OwnedFd,
            depth: usize,
            emit_existing: bool,
            stop: &AtomicBool,
            q: &mut NotifyQueue,
        ) {
            if stop.load(Ordering::Acquire) {
                return;
            }
            if depth > MAX_DEPTH {
                q.push_flush();
                return;
            }
            if self.wd_to_dirs.len() >= MAX_WATCHES {
                q.push_flush();
                return;
            }
            let is_root = depth == 0 && rel.is_empty();
            match add_watch_for_fd(self.inotify_fd.as_raw_fd(), dir_fd.as_raw_fd(), is_root) {
                Ok(wd) => self.register_wd(wd, root_idx, rel.clone(), is_root),
                Err(err) => {
                    if err != libc::ENOENT && err != libc::ENOTDIR {
                        q.push_flush();
                    }
                    return;
                }
            }

            // Duplicate dir_fd for fdopendir so we can close the DIR stream
            // immediately after scanning entries and keep peak open FDs O(MAX_DEPTH).
            let dup_rc = unsafe { libc::fcntl(dir_fd.as_raw_fd(), libc::F_DUPFD_CLOEXEC, 0) };
            if dup_rc < 0 {
                q.push_flush();
                return;
            }
            let dir = unsafe { libc::fdopendir(dup_rc) };
            if dir.is_null() {
                unsafe { libc::close(dup_rc) };
                q.push_flush();
                return;
            }
            let mut subdirs: Vec<(String, CString)> = Vec::new();
            loop {
                let ent = unsafe { libc::readdir(dir) };
                if ent.is_null() {
                    break;
                }
                let cname = unsafe { CStr::from_ptr((*ent).d_name.as_ptr()) };
                let name_bytes = cname.to_bytes();
                if name_bytes == b"." || name_bytes == b".." || name_bytes.is_empty() {
                    continue;
                }
                let Ok(name_str) = std::str::from_utf8(name_bytes) else {
                    q.push_flush();
                    continue;
                };
                let child_rel = if rel.is_empty() {
                    name_str.to_string()
                } else {
                    format!("{rel}/{name_str}")
                };
                if emit_existing {
                    let full = join_root_rel(&self.roots[root_idx], &child_rel);
                    q.push_event(FLAG_ITEM_CREATED, 0, full.as_bytes());
                }
                let d_type = unsafe { (*ent).d_type };
                if d_type == libc::DT_DIR || d_type == libc::DT_UNKNOWN {
                    subdirs.push((child_rel, cname.to_owned()));
                }
            }
            unsafe { libc::closedir(dir) };

            for (child_rel, cname) in subdirs {
                if stop.load(Ordering::Acquire) {
                    return;
                }
                match open_dir_beneath(dir_fd.as_raw_fd(), &cname) {
                    Ok(sub_fd) => {
                        self.watch_subtree(
                            root_idx,
                            child_rel,
                            sub_fd,
                            depth + 1,
                            emit_existing,
                            stop,
                            q,
                        );
                    }
                    Err(err) => {
                        if err != libc::ENOENT && err != libc::ENOTDIR && err != libc::ELOOP {
                            q.push_flush();
                        }
                    }
                }
            }
        }

        fn open_rel_dir(&self, root_idx: usize, rel: &str) -> Result<OwnedFd, i32> {
            let Some(rfd) = self.root_fds.get(root_idx).and_then(|o| o.as_ref()) else {
                return Err(libc::ENOENT);
            };
            let dup_rc = unsafe { libc::fcntl(rfd.as_raw_fd(), libc::F_DUPFD_CLOEXEC, 0) };
            if dup_rc < 0 {
                return Err(io::Error::last_os_error()
                    .raw_os_error()
                    .unwrap_or(libc::EIO));
            }
            let mut cur = unsafe { OwnedFd::from_raw_fd(dup_rc) };
            if rel.is_empty() {
                return Ok(cur);
            }
            for comp in rel.split('/') {
                if comp.is_empty() || comp == "." || comp == ".." {
                    return Err(libc::EINVAL);
                }
                let ccomp = CString::new(comp).map_err(|_| libc::EINVAL)?;
                cur = open_dir_beneath(cur.as_raw_fd(), &ccomp)?;
            }
            Ok(cur)
        }

        fn rewrite_moved_subtree(&mut self, root_idx: usize, old_rel: &str, new_rel: &str) {
            let prefix = format!("{old_rel}/");
            let mut updates: Vec<(i32, String, String)> = Vec::new();
            for (&wd, entries) in &self.wd_to_dirs {
                for entry in entries {
                    if entry.root_idx != root_idx {
                        continue;
                    }
                    if entry.rel == old_rel {
                        updates.push((wd, entry.rel.clone(), new_rel.to_string()));
                    } else if let Some(rest) = entry.rel.strip_prefix(&prefix) {
                        updates.push((wd, entry.rel.clone(), format!("{new_rel}/{rest}")));
                    }
                }
            }
            for (wd, old_r, new_r) in updates {
                if self.dir_to_wd.get(&(root_idx, old_r.clone())) == Some(&wd) {
                    self.dir_to_wd.remove(&(root_idx, old_r.clone()));
                }
                if let Some(entries) = self.wd_to_dirs.get_mut(&wd) {
                    for e in entries {
                        if e.root_idx == root_idx && e.rel == old_r {
                            e.rel = new_r.clone();
                        }
                    }
                }
                self.dir_to_wd.insert((root_idx, new_r), wd);
            }
        }

        fn handle_event(
            &mut self,
            wd: i32,
            mask: u32,
            cookie: u32,
            name: &[u8],
            stop: &AtomicBool,
            q: &mut NotifyQueue,
        ) {
            if (mask & libc::IN_Q_OVERFLOW) != 0 {
                self.pending_moves.clear();
                q.push_flush();
                return;
            }
            if (mask & libc::IN_IGNORED) != 0 {
                self.remove_wd_ignored(wd);
                return;
            }
            let Some(entries) = self.wd_to_dirs.get(&wd).cloned() else {
                return;
            };
            if (mask & (libc::IN_DELETE_SELF | libc::IN_MOVE_SELF)) != 0 {
                for entry in &entries {
                    if entry.is_root {
                        let root = &self.roots[entry.root_idx];
                        q.push_event(FLAG_ROOT_CHANGED, 0, root.as_bytes());
                        self.remove_subtree_watches(entry.root_idx, "");
                    }
                    // For non-root subdirectories, IN_DELETE_SELF is followed by
                    // IN_IGNORED from the kernel and IN_MOVE_SELF is handled via
                    // parent IN_MOVED_FROM / IN_MOVED_TO so renamed subtrees stay watched.
                }
                return;
            }
            if name.is_empty() {
                return;
            }
            let Ok(name_str) = std::str::from_utf8(name) else {
                q.push_flush();
                return;
            };
            if name_str == "." || name_str == ".." || name_str.contains('/') {
                q.push_flush();
                return;
            }

            let is_dir = (mask & libc::IN_ISDIR) != 0;
            let mut flags = 0u32;
            let mut fileid = 0u64;
            if (mask & libc::IN_CREATE) != 0 {
                flags |= FLAG_ITEM_CREATED;
            }
            if (mask & libc::IN_DELETE) != 0 {
                flags |= FLAG_ITEM_REMOVED;
                fileid = SYNTHETIC_REPLACE_ID;
            }
            if (mask & libc::IN_MOVED_FROM) != 0 {
                flags |= FLAG_ITEM_RENAMED;
            }
            if (mask & libc::IN_MOVED_TO) != 0 {
                flags |= FLAG_ITEM_RENAMED;
                fileid = SYNTHETIC_REPLACE_ID;
            }

            for entry in entries {
                let child_rel = if entry.rel.is_empty() {
                    name_str.to_string()
                } else {
                    format!("{}/{name_str}", entry.rel)
                };
                let full_path = join_root_rel(&self.roots[entry.root_idx], &child_rel);
                q.push_event(flags, fileid, full_path.as_bytes());

                if is_dir {
                    if (mask & libc::IN_CREATE) != 0 {
                        match self.open_rel_dir(entry.root_idx, &child_rel) {
                            Ok(sub_fd) => {
                                let depth = child_rel.split('/').count();
                                self.watch_subtree(
                                    entry.root_idx,
                                    child_rel,
                                    sub_fd,
                                    depth,
                                    true,
                                    stop,
                                    q,
                                );
                            }
                            Err(err) => {
                                if err != libc::ENOENT && err != libc::ENOTDIR && err != libc::ELOOP
                                {
                                    q.push_flush();
                                }
                            }
                        }
                    } else if (mask & libc::IN_MOVED_FROM) != 0 {
                        if cookie != 0 {
                            if self.pending_moves.len() >= MAX_PENDING_MOVES {
                                self.flush_unmatched_moves();
                            }
                            self.pending_moves.insert(
                                cookie,
                                PendingMove {
                                    root_idx: entry.root_idx,
                                    old_rel: child_rel,
                                },
                            );
                        } else {
                            self.remove_subtree_watches(entry.root_idx, &child_rel);
                        }
                    } else if (mask & libc::IN_MOVED_TO) != 0 {
                        let matched = self
                            .pending_moves
                            .get(&cookie)
                            .is_some_and(|pm| pm.root_idx == entry.root_idx);
                        if matched {
                            let pm = self.pending_moves.remove(&cookie).unwrap();
                            self.rewrite_moved_subtree(entry.root_idx, &pm.old_rel, &child_rel);
                        } else {
                            match self.open_rel_dir(entry.root_idx, &child_rel) {
                                Ok(sub_fd) => {
                                    let depth = child_rel.split('/').count();
                                    self.watch_subtree(
                                        entry.root_idx,
                                        child_rel,
                                        sub_fd,
                                        depth,
                                        true,
                                        stop,
                                        q,
                                    );
                                }
                                Err(err) => {
                                    if err != libc::ENOENT
                                        && err != libc::ENOTDIR
                                        && err != libc::ELOOP
                                    {
                                        q.push_flush();
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    pub struct NotifyHandle {
        shutdown_efd: OwnedFd,
        stop: Arc<AtomicBool>,
        worker: Mutex<Option<JoinHandle<()>>>,
    }

    impl NotifyHandle {
        pub fn start(notify_fd: RawFd, watch_paths: Vec<PathBuf>) -> Result<Self, String> {
            let pipe_fd = unsafe { OwnedFd::from_raw_fd(notify_fd) };
            let rc_cloexec =
                unsafe { libc::fcntl(pipe_fd.as_raw_fd(), libc::F_SETFD, libc::FD_CLOEXEC) };
            let fl = unsafe { libc::fcntl(pipe_fd.as_raw_fd(), libc::F_GETFL) };
            if rc_cloexec < 0
                || fl < 0
                || unsafe { libc::fcntl(pipe_fd.as_raw_fd(), libc::F_SETFL, fl | libc::O_NONBLOCK) }
                    < 0
            {
                return Err(format!("configure notify fd: {}", io::Error::last_os_error()));
            }

            let ino_rc = unsafe { libc::inotify_init1(libc::IN_NONBLOCK | libc::IN_CLOEXEC) };
            if ino_rc < 0 {
                return Err(format!("inotify_init1: {}", io::Error::last_os_error()));
            }
            let inotify_fd = unsafe { OwnedFd::from_raw_fd(ino_rc) };

            let efd_rc = unsafe { libc::eventfd(0, libc::EFD_NONBLOCK | libc::EFD_CLOEXEC) };
            if efd_rc < 0 {
                return Err(format!("eventfd: {}", io::Error::last_os_error()));
            }
            let shutdown_efd = unsafe { OwnedFd::from_raw_fd(efd_rc) };
            let dup_efd_rc =
                unsafe { libc::fcntl(shutdown_efd.as_raw_fd(), libc::F_DUPFD_CLOEXEC, 0) };
            if dup_efd_rc < 0 {
                return Err(format!("dup eventfd: {}", io::Error::last_os_error()));
            }
            let thread_efd = unsafe { OwnedFd::from_raw_fd(dup_efd_rc) };
            let stop = Arc::new(AtomicBool::new(false));
            let thread_stop = Arc::clone(&stop);

            let handle = thread::Builder::new()
                .name("kido-notify".to_string())
                .spawn(move || {
                    run_inotify_loop(pipe_fd, inotify_fd, thread_efd, thread_stop, watch_paths);
                })
                .map_err(|e| format!("spawn notify thread: {e}"))?;

            Ok(Self {
                shutdown_efd,
                stop,
                worker: Mutex::new(Some(handle)),
            })
        }

        pub fn shutdown(&self) {
            self.stop.store(true, Ordering::Release);
            let val: u64 = 1;
            let _ = unsafe {
                libc::write(
                    self.shutdown_efd.as_raw_fd(),
                    (&val as *const u64).cast::<libc::c_void>(),
                    std::mem::size_of::<u64>(),
                )
            };
            if let Some(h) = self.worker.lock().unwrap().take() {
                let _ = h.join();
            }
        }
    }

    impl Drop for NotifyHandle {
        fn drop(&mut self) {
            self.shutdown();
        }
    }

    fn run_inotify_loop(
        pipe_fd: OwnedFd,
        inotify_fd: OwnedFd,
        shutdown_efd: OwnedFd,
        stop: Arc<AtomicBool>,
        watch_paths: Vec<PathBuf>,
    ) {
        let mut tree = WatchTree::new(inotify_fd, &watch_paths);
        let mut queue = NotifyQueue::new(DEFAULT_QUEUE_CAP);
        tree.init_all_watches(&stop, &mut queue);

        let mut buf = vec![0u64; INOTIFY_BUF_BYTES / 8];
        let hdr_sz = std::mem::size_of::<libc::inotify_event>();

        loop {
            if stop.load(Ordering::Acquire) {
                break;
            }
            if !queue.is_empty() && queue.drain_fd(pipe_fd.as_raw_fd()) == DrainResult::Broken {
                eprintln!("kido-vmm: notifications stopped: the relay is gone");
                break;
            }

            let mut pfds = [
                libc::pollfd {
                    fd: tree.inotify_fd.as_raw_fd(),
                    events: libc::POLLIN,
                    revents: 0,
                },
                libc::pollfd {
                    fd: shutdown_efd.as_raw_fd(),
                    events: libc::POLLIN,
                    revents: 0,
                },
                libc::pollfd {
                    fd: pipe_fd.as_raw_fd(),
                    events: if queue.is_empty() {
                        libc::POLLERR | libc::POLLHUP
                    } else {
                        libc::POLLOUT
                    },
                    revents: 0,
                },
            ];
            let rc = unsafe { libc::poll(pfds.as_mut_ptr(), pfds.len() as libc::nfds_t, -1) };
            if rc < 0 {
                let err = io::Error::last_os_error()
                    .raw_os_error()
                    .unwrap_or(libc::EIO);
                if err == libc::EINTR {
                    continue;
                }
                break;
            }
            if stop.load(Ordering::Acquire) || (pfds[1].revents & libc::POLLIN) != 0 {
                break;
            }
            if (pfds[2].revents & (libc::POLLERR | libc::POLLHUP | libc::POLLNVAL)) != 0 {
                eprintln!("kido-vmm: notifications stopped: the relay is gone");
                break;
            }
            if (pfds[2].revents & libc::POLLOUT) != 0
                && queue.drain_fd(pipe_fd.as_raw_fd()) == DrainResult::Broken
            {
                eprintln!("kido-vmm: notifications stopped: the relay is gone");
                break;
            }
            if (pfds[0].revents & libc::POLLIN) != 0 {
                let mut drained_eagain = false;
                for _ in 0..MAX_READS_PER_POLL {
                    if stop.load(Ordering::Acquire) {
                        break;
                    }
                    let n = unsafe {
                        libc::read(
                            tree.inotify_fd.as_raw_fd(),
                            buf.as_mut_ptr().cast::<libc::c_void>(),
                            buf.len() * 8,
                        )
                    };
                    if n <= 0 {
                        if n < 0 {
                            let err = io::Error::last_os_error()
                                .raw_os_error()
                                .unwrap_or(libc::EIO);
                            if err == libc::EINTR {
                                continue;
                            }
                            if err == libc::EAGAIN {
                                drained_eagain = true;
                            }
                        } else {
                            drained_eagain = true;
                        }
                        break;
                    }
                    let bytes = unsafe {
                        std::slice::from_raw_parts(buf.as_ptr().cast::<u8>(), n as usize)
                    };
                    let mut pos = 0usize;
                    while pos + hdr_sz <= bytes.len() {
                        let ev = unsafe {
                            std::ptr::read_unaligned(
                                bytes.as_ptr().add(pos).cast::<libc::inotify_event>(),
                            )
                        };
                        let name_len = ev.len as usize;
                        let rec_end = pos.saturating_add(hdr_sz).saturating_add(name_len);
                        if rec_end > bytes.len() {
                            break;
                        }
                        let raw_name = &bytes[pos + hdr_sz..rec_end];
                        let nul = raw_name
                            .iter()
                            .position(|&b| b == 0)
                            .unwrap_or(raw_name.len());
                        tree.handle_event(
                            ev.wd,
                            ev.mask,
                            ev.cookie,
                            &raw_name[..nul],
                            &stop,
                            &mut queue,
                        );
                        pos = rec_end;
                    }
                }
                if drained_eagain {
                    tree.flush_unmatched_moves();
                }
                if queue.drain_fd(pipe_fd.as_raw_fd()) == DrainResult::Broken {
                    eprintln!("kido-vmm: notifications stopped: the relay is gone");
                    break;
                }
            }
        }
    }
}

#[cfg(target_os = "linux")]
pub use linux_impl::NotifyHandle;

#[cfg(not(target_os = "linux"))]
pub struct NotifyHandle;

#[cfg(not(target_os = "linux"))]
impl NotifyHandle {
    pub fn start(notify_fd: RawFd, _watch_paths: Vec<PathBuf>) -> Result<Self, String> {
        close_raw_fd(notify_fd);
        Ok(Self)
    }

    pub fn shutdown(&self) {}
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Read;
    use std::os::unix::io::AsRawFd;

    fn make_pipe() -> (std::fs::File, std::fs::File) {
        let mut fds = [-1i32; 2];
        assert_eq!(unsafe { libc::pipe(fds.as_mut_ptr()) }, 0);
        unsafe {
            (
                std::fs::File::from_raw_fd(fds[0]),
                std::fs::File::from_raw_fd(fds[1]),
            )
        }
    }

    #[test]
    fn queue_encodes_events_and_collapses_overflow_to_flush() {
        let (mut r, w) = make_pipe();
        let fl = unsafe { libc::fcntl(w.as_raw_fd(), libc::F_GETFL) };
        assert_eq!(unsafe { libc::fcntl(w.as_raw_fd(), libc::F_SETFL, fl | libc::O_NONBLOCK) }, 0);

        let mut q = NotifyQueue::new(2 * LARGEST_RECORD);
        q.push_event(FLAG_ITEM_CREATED, 42, b"/share/hello.txt");
        assert_eq!(q.drain_fd(w.as_raw_fd()), DrainResult::Empty);

        let mut got = vec![0u8; HEADER_LEN + 16];
        r.read_exact(&mut got).unwrap();
        assert_eq!(u32::from_le_bytes(got[0..4].try_into().unwrap()), 16 + 16);
        assert_eq!(u32::from_le_bytes(got[4..8].try_into().unwrap()), NOTIFY_EVENT);
        assert_eq!(u32::from_le_bytes(got[8..12].try_into().unwrap()), FLAG_ITEM_CREATED);
        assert_eq!(u64::from_le_bytes(got[12..20].try_into().unwrap()), 42);
        assert_eq!(&got[20..], b"/share/hello.txt");

        // Path exceeding NOTIFY_MAX_PATH collapses to NOTIFY_FLUSH.
        let huge = vec![b'a'; NOTIFY_MAX_PATH + 1];
        q.push_event(0, 0, &huge);
        assert_eq!(q.drain_fd(w.as_raw_fd()), DrainResult::Empty);
        let mut flush = [0u8; HEADER_LEN];
        r.read_exact(&mut flush).unwrap();
        assert_eq!(u32::from_le_bytes(flush[0..4].try_into().unwrap()), 16);
        assert_eq!(u32::from_le_bytes(flush[4..8].try_into().unwrap()), NOTIFY_FLUSH);
        assert_eq!(u32::from_le_bytes(flush[8..12].try_into().unwrap()), 0);
        assert_eq!(u64::from_le_bytes(flush[12..20].try_into().unwrap()), 0);
    }

    #[test]
    fn queue_marks_broken_when_reader_closes() {
        let (r, w) = make_pipe();
        drop(r);
        let fl = unsafe { libc::fcntl(w.as_raw_fd(), libc::F_GETFL) };
        assert_eq!(unsafe { libc::fcntl(w.as_raw_fd(), libc::F_SETFL, fl | libc::O_NONBLOCK) }, 0);
        unsafe { libc::signal(libc::SIGPIPE, libc::SIG_IGN) };

        let mut q = NotifyQueue::new(DEFAULT_QUEUE_CAP);
        q.push_event(0, 0, b"/share/x");
        assert_eq!(q.drain_fd(w.as_raw_fd()), DrainResult::Broken);
        assert!(q.is_broken());
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn linux_inotify_reports_create_modify_replace_subdir_and_clean_shutdown() {
        use std::os::unix::io::IntoRawFd;
        use std::time::{Duration, Instant};

        let root = std::env::temp_dir().join(format!(
            "kido-inotify-test-{}",
            std::process::id()
        ));
        let _ = std::fs::remove_dir_all(&root);
        std::fs::create_dir_all(&root).unwrap();

        let (mut r, w) = make_pipe();
        let fl = unsafe { libc::fcntl(r.as_raw_fd(), libc::F_GETFL) };
        assert_eq!(unsafe { libc::fcntl(r.as_raw_fd(), libc::F_SETFL, fl | libc::O_NONBLOCK) }, 0);

        let handle = NotifyHandle::start(w.into_raw_fd(), vec![root.clone()]).unwrap();
        std::thread::sleep(Duration::from_millis(50));

        std::fs::write(root.join("a.txt"), b"v1").unwrap();
        std::fs::write(root.join("a.tmp"), b"v2").unwrap();
        std::fs::rename(root.join("a.tmp"), root.join("a.txt")).unwrap();
        std::fs::create_dir(root.join("sub")).unwrap();
        std::thread::sleep(Duration::from_millis(30));
        std::fs::write(root.join("sub").join("b.txt"), b"nested").unwrap();

        let deadline = Instant::now() + Duration::from_secs(3);
        let mut raw = Vec::new();
        let mut seen_a_replace = false;
        let mut seen_b = false;
        let want_a = root.join("a.txt").to_string_lossy().into_owned();
        let want_b = root.join("sub").join("b.txt").to_string_lossy().into_owned();
        let mut chunk = [0u8; 4096];

        while Instant::now() < deadline && !(seen_a_replace && seen_b) {
            match r.read(&mut chunk) {
                Ok(n) if n > 0 => {
                    raw.extend_from_slice(&chunk[..n]);
                    let mut pos = 0usize;
                    while pos + 4 <= raw.len() {
                        let body = u32::from_le_bytes(raw[pos..pos + 4].try_into().unwrap()) as usize;
                        if pos + 4 + body > raw.len() {
                            break;
                        }
                        let kind = u32::from_le_bytes(raw[pos + 4..pos + 8].try_into().unwrap());
                        let flags = u32::from_le_bytes(raw[pos + 8..pos + 12].try_into().unwrap());
                        let fid = u64::from_le_bytes(raw[pos + 12..pos + 20].try_into().unwrap());
                        let path = String::from_utf8_lossy(&raw[pos + 20..pos + 4 + body]);
                        if kind == NOTIFY_EVENT {
                            if path == want_a
                                && (flags & FLAG_ITEM_RENAMED) != 0
                                && fid == SYNTHETIC_REPLACE_ID
                            {
                                seen_a_replace = true;
                            }
                            if path == want_b {
                                seen_b = true;
                            }
                        }
                        pos += 4 + body;
                    }
                    raw.drain(..pos);
                }
                _ => std::thread::sleep(Duration::from_millis(10)),
            }
        }
        assert!(seen_a_replace, "did not observe IN_MOVED_TO replace event for a.txt");
        assert!(seen_b, "did not observe nested subdir b.txt event");

        // Verify H1: renaming a watched subdir keeps watching it under its new path,
        // and moving a watched subdir outside the root cleans up its watches.
        let sub_renamed = root.join("sub_renamed");
        std::fs::rename(root.join("sub"), &sub_renamed).unwrap();
        std::thread::sleep(Duration::from_millis(50));
        std::fs::write(sub_renamed.join("c.txt"), b"after-rename").unwrap();

        let want_c = sub_renamed.join("c.txt").to_string_lossy().into_owned();
        let mut seen_c = false;
        let deadline2 = Instant::now() + Duration::from_secs(3);
        while Instant::now() < deadline2 && !seen_c {
            match r.read(&mut chunk) {
                Ok(n) if n > 0 => {
                    raw.extend_from_slice(&chunk[..n]);
                    let mut pos = 0usize;
                    while pos + 4 <= raw.len() {
                        let body =
                            u32::from_le_bytes(raw[pos..pos + 4].try_into().unwrap()) as usize;
                        if pos + 4 + body > raw.len() {
                            break;
                        }
                        let kind = u32::from_le_bytes(raw[pos + 4..pos + 8].try_into().unwrap());
                        let path = String::from_utf8_lossy(&raw[pos + 20..pos + 4 + body]);
                        if kind == NOTIFY_EVENT && path == want_c {
                            seen_c = true;
                        }
                        pos += 4 + body;
                    }
                    raw.drain(..pos);
                }
                _ => std::thread::sleep(Duration::from_millis(10)),
            }
        }
        assert!(seen_c, "did not observe event inside renamed subdirectory");

        // Move subdir outside watched root: edits there must NOT emit events.
        let outside = std::env::temp_dir().join(format!(
            "kido-inotify-outside-{}",
            std::process::id()
        ));
        let _ = std::fs::remove_dir_all(&outside);
        std::fs::rename(&sub_renamed, &outside).unwrap();
        std::thread::sleep(Duration::from_millis(80));
        // Drain any rename-out events first.
        while let Ok(n) = r.read(&mut chunk) {
            if n == 0 {
                break;
            }
        }
        raw.clear();
        std::fs::write(outside.join("leak.txt"), b"outside").unwrap();
        std::thread::sleep(Duration::from_millis(80));
        let mut saw_outside_leak = false;
        while let Ok(n) = r.read(&mut chunk) {
            if n == 0 {
                break;
            }
            if String::from_utf8_lossy(&chunk[..n]).contains("leak.txt") {
                saw_outside_leak = true;
            }
        }
        assert!(
            !saw_outside_leak,
            "moved-outside directory leaked inotify events"
        );

        handle.shutdown();
        let _ = std::fs::remove_dir_all(&root);
        let _ = std::fs::remove_dir_all(&outside);
    }
}
