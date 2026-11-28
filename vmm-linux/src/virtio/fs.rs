#![allow(clippy::unnecessary_cast)]

use std::collections::{HashMap, VecDeque};
use std::ffi::{CStr, CString};
use std::fs::File;
use std::io;
use std::os::unix::io::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Condvar, Mutex, OnceLock, RwLock, Weak};
use std::thread::{self, JoinHandle};

use crate::memory::GuestMemory;
use crate::pci::PCI_DEVICE_VIRTIO_FS;
use crate::virtio::pci::VirtioPciDevice;
use crate::virtio::queue::{QueueError, VirtQueue};
use crate::virtio::{
    QueueOutcome, VirtioDevice, VIRTIO_F_VERSION_1, VIRTIO_RING_F_INDIRECT_DESC,
};

pub const VIRTIO_FS_SUBSYSTEM_ID: u16 = 0x001a;
pub const VIRTIO_FS_PCI_CLASS: u32 = 0x018000;
pub const DEFAULT_FS_TAG: &str = "kido";

const TAG_LEN: usize = 36;
const DEV_CONFIG_LEN: usize = 40;
const HIPRIO_QUEUE_SIZE: u16 = 64;
const REQ_QUEUE_SIZE: u16 = 256;
const NUM_WORKERS: usize = 4;

// FUSE 7.x protocol constants matching Darwin VirtioFS.kext (fuse.h).
const FUSE_KERNEL_VERSION: u32 = 7;
const FUSE_KERNEL_MINOR_VERSION: u32 = 38;
const FUSE_ROOT_ID: u64 = 1;

const FUSE_ASYNC_READ: u32 = 1 << 0;
const FUSE_BIG_WRITES: u32 = 1 << 5;
const FUSE_PARALLEL_DIROPS: u32 = 1 << 18;
const FUSE_MAX_PAGES: u32 = 1 << 22;
const FUSE_SETXATTR_EXT: u32 = 1 << 29;

const FUSE_OP_LOOKUP: u32 = 1;
const FUSE_OP_FORGET: u32 = 2;
const FUSE_OP_GETATTR: u32 = 3;
const FUSE_OP_SETATTR: u32 = 4;
const FUSE_OP_READLINK: u32 = 5;
const FUSE_OP_SYMLINK: u32 = 6;
const FUSE_OP_MKNOD: u32 = 8;
const FUSE_OP_MKDIR: u32 = 9;
const FUSE_OP_UNLINK: u32 = 10;
const FUSE_OP_RMDIR: u32 = 11;
const FUSE_OP_RENAME: u32 = 12;
const FUSE_OP_LINK: u32 = 13;
const FUSE_OP_OPEN: u32 = 14;
const FUSE_OP_READ: u32 = 15;
const FUSE_OP_WRITE: u32 = 16;
const FUSE_OP_STATFS: u32 = 17;
const FUSE_OP_RELEASE: u32 = 18;
const FUSE_OP_FSYNC: u32 = 20;
const FUSE_OP_SETXATTR: u32 = 21;
const FUSE_OP_GETXATTR: u32 = 22;
const FUSE_OP_LISTXATTR: u32 = 23;
const FUSE_OP_REMOVEXATTR: u32 = 24;
const FUSE_OP_FLUSH: u32 = 25;
const FUSE_OP_INIT: u32 = 26;
const FUSE_OP_OPENDIR: u32 = 27;
const FUSE_OP_READDIR: u32 = 28;
const FUSE_OP_RELEASEDIR: u32 = 29;
const FUSE_OP_FSYNCDIR: u32 = 30;
const FUSE_OP_ACCESS: u32 = 34;
const FUSE_OP_CREATE: u32 = 35;
const FUSE_OP_DESTROY: u32 = 38;
const FUSE_OP_BATCH_FORGET: u32 = 42;
const FUSE_OP_FALLOCATE: u32 = 43;
const FUSE_OP_READDIRPLUS: u32 = 44;
const FUSE_OP_RENAME2: u32 = 45;
const FUSE_OP_LSEEK: u32 = 46;
const FUSE_OP_SYNCFS: u32 = 50;

const FUSE_IN_HEADER_LEN: usize = 40;
const FUSE_OUT_HEADER_LEN: usize = 16;
const FUSE_ATTR_LEN: usize = 88;
const FUSE_ENTRY_OUT_LEN: usize = 128;
const FUSE_ATTR_OUT_LEN: usize = 104;
const FUSE_OPEN_OUT_LEN: usize = 16;
const FUSE_WRITE_OUT_LEN: usize = 8;
const FUSE_STATFS_OUT_LEN: usize = 80;
const FUSE_INIT_OUT_LEN: usize = 64;
const FUSE_GETXATTR_OUT_LEN: usize = 8;
const FUSE_LSEEK_OUT_LEN: usize = 8;
const FUSE_DIRENT_HDR_LEN: usize = 24;

const FATTR_MODE: u32 = 1 << 0;
const FATTR_UID: u32 = 1 << 1;
const FATTR_GID: u32 = 1 << 2;
const FATTR_SIZE: u32 = 1 << 3;
const FATTR_ATIME: u32 = 1 << 4;
const FATTR_MTIME: u32 = 1 << 5;
const FATTR_FH: u32 = 1 << 6;
const FATTR_ATIME_NOW: u32 = 1 << 7;
const FATTR_MTIME_NOW: u32 = 1 << 8;

const FUSE_GETATTR_FH: u32 = 1 << 0;
const FUSE_FSYNC_FDATASYNC: u32 = 1 << 0;

// Canonical Linux open flags sent on the FUSE wire by VirtioFS.kext.
const FUSE_O_ACCMODE: u32 = 0x3;
const FUSE_O_WRONLY: u32 = 0x1;
const FUSE_O_RDWR: u32 = 0x2;
const FUSE_O_CREAT: u32 = 0x40;
const FUSE_O_EXCL: u32 = 0x80;
const FUSE_O_TRUNC: u32 = 0x200;
const FUSE_O_APPEND: u32 = 0x400;

const FUSE_XATTR_CREATE: u32 = 0x1;
const FUSE_XATTR_REPLACE: u32 = 0x2;

const FUSE_RENAME_NOREPLACE: u32 = 1 << 0;
const FUSE_RENAME_EXCHANGE: u32 = 1 << 1;

// Resource bounds and protocol quotas.
const MAX_IO_BYTES: u32 = 1 << 17; // 128 KiB, matches VirtioFSDevice kMaxIO
const MAX_REQ_BYTES: usize = 256 * 1024;
const MAX_INFLIGHT_BYTES: usize = 32 * 1024 * 1024;
const MAX_HIPRIO_QUEUED: usize = 64;
const MAX_REQ_QUEUED: usize = 256;
const MAX_INODES: usize = 65_536;
const MAX_OPEN_FILES: usize = 2048;
const MAX_OPEN_DIRS: usize = 512;
const MAX_XATTR_VALUE: usize = 64 * 1024;
const MAX_SYMLINK_LEN: usize = 4096;
const MAX_NAME_LEN: usize = 255;
const VIRTIOFS_SIZE_MAX: u64 = (i64::MAX as u64) - (1u64 << 20);
const DEFAULT_ENTRY_VALID_SEC: u64 = 1;
const DEFAULT_ATTR_VALID_SEC: u64 = 1;

// Guest xattrs are confined under a dedicated user.* prefix on Linux so
// Darwin-style xattr names work on ext4/btrfs/xfs while never touching host
// security.*, trusted.*, or system.* xattrs.
#[cfg(target_os = "linux")]
const LINUX_XATTR_PREFIX: &str = "user.kido.xattr.";
#[cfg(target_os = "linux")]
const LINUX_XATTR_NAME_MAX: usize = 255;

// Canonical Linux errno numbers expected by Darwin VirtioFS.kext virtiofs_errno().
pub const LINUX_EPERM: i32 = 1;
pub const LINUX_ENOENT: i32 = 2;
pub const LINUX_EINTR: i32 = 4;
pub const LINUX_EIO: i32 = 5;
pub const LINUX_ENXIO: i32 = 6;
pub const LINUX_E2BIG: i32 = 7;
pub const LINUX_EBADF: i32 = 9;
pub const LINUX_EAGAIN: i32 = 11;
pub const LINUX_ENOMEM: i32 = 12;
pub const LINUX_EACCES: i32 = 13;
pub const LINUX_EFAULT: i32 = 14;
pub const LINUX_EBUSY: i32 = 16;
pub const LINUX_EEXIST: i32 = 17;
pub const LINUX_EXDEV: i32 = 18;
pub const LINUX_ENODEV: i32 = 19;
pub const LINUX_ENOTDIR: i32 = 20;
pub const LINUX_EISDIR: i32 = 21;
pub const LINUX_EINVAL: i32 = 22;
pub const LINUX_ENFILE: i32 = 23;
pub const LINUX_EMFILE: i32 = 24;
pub const LINUX_ENOTTY: i32 = 25;
pub const LINUX_ETXTBSY: i32 = 26;
pub const LINUX_EFBIG: i32 = 27;
pub const LINUX_ENOSPC: i32 = 28;
pub const LINUX_ESPIPE: i32 = 29;
pub const LINUX_EROFS: i32 = 30;
pub const LINUX_EMLINK: i32 = 31;
pub const LINUX_EPIPE: i32 = 32;
pub const LINUX_ERANGE: i32 = 34;
pub const LINUX_EDEADLK: i32 = 35;
pub const LINUX_ENAMETOOLONG: i32 = 36;
pub const LINUX_ENOLCK: i32 = 37;
pub const LINUX_ENOSYS: i32 = 38;
pub const LINUX_ENOTEMPTY: i32 = 39;
pub const LINUX_ELOOP: i32 = 40;
pub const LINUX_ENODATA: i32 = 61;
pub const LINUX_EOVERFLOW: i32 = 75;
pub const LINUX_EILSEQ: i32 = 84;
pub const LINUX_EOPNOTSUPP: i32 = 95;
pub const LINUX_ESTALE: i32 = 116;
pub const LINUX_EDQUOT: i32 = 122;
pub const LINUX_ECANCELED: i32 = 125;

/// Maps a host OS errno value to a negative canonical Linux errno for the FUSE
/// wire protocol. VirtioFS.kext converts these back into Darwin BSD errnos via
/// its `from_linux[]` table in `fuse_proto.c`.
pub fn host_errno_to_fuse_error(host_errno: i32) -> i32 {
    if host_errno <= 0 {
        return -LINUX_EIO;
    }
    let linux_code = match host_errno {
        libc::EPERM => LINUX_EPERM,
        libc::ENOENT => LINUX_ENOENT,
        libc::EINTR => LINUX_EINTR,
        libc::EIO => LINUX_EIO,
        libc::ENXIO => LINUX_ENXIO,
        libc::E2BIG => LINUX_E2BIG,
        libc::EBADF => LINUX_EBADF,
        libc::EAGAIN => LINUX_EAGAIN,
        libc::ENOMEM => LINUX_ENOMEM,
        libc::EACCES => LINUX_EACCES,
        libc::EFAULT => LINUX_EFAULT,
        libc::EBUSY => LINUX_EBUSY,
        libc::EEXIST => LINUX_EEXIST,
        libc::EXDEV => LINUX_EXDEV,
        libc::ENODEV => LINUX_ENODEV,
        libc::ENOTDIR => LINUX_ENOTDIR,
        libc::EISDIR => LINUX_EISDIR,
        libc::EINVAL => LINUX_EINVAL,
        libc::ENFILE => LINUX_ENFILE,
        libc::EMFILE => LINUX_EMFILE,
        libc::ENOTTY => LINUX_ENOTTY,
        libc::ETXTBSY => LINUX_ETXTBSY,
        libc::EFBIG => LINUX_EFBIG,
        libc::ENOSPC => LINUX_ENOSPC,
        libc::ESPIPE => LINUX_ESPIPE,
        libc::EROFS => LINUX_EROFS,
        libc::EMLINK => LINUX_EMLINK,
        libc::EPIPE => LINUX_EPIPE,
        libc::ERANGE => LINUX_ERANGE,
        libc::EDEADLK => LINUX_EDEADLK,
        libc::ENAMETOOLONG => LINUX_ENAMETOOLONG,
        libc::ENOLCK => LINUX_ENOLCK,
        libc::ENOSYS => LINUX_ENOSYS,
        libc::ENOTEMPTY => LINUX_ENOTEMPTY,
        libc::ELOOP => LINUX_ELOOP,
        libc::EOVERFLOW => LINUX_EOVERFLOW,
        libc::EILSEQ => LINUX_EILSEQ,
        libc::EOPNOTSUPP => LINUX_EOPNOTSUPP,
        #[cfg(not(target_os = "linux"))]
        libc::ENOTSUP => LINUX_EOPNOTSUPP,
        libc::ESTALE => LINUX_ESTALE,
        libc::EDQUOT => LINUX_EDQUOT,
        libc::ECANCELED => LINUX_ECANCELED,
        #[cfg(target_os = "linux")]
        libc::ENODATA => LINUX_ENODATA,
        #[cfg(target_os = "macos")]
        libc::ENOATTR => LINUX_ENODATA,
        #[cfg(target_os = "linux")]
        other if (1..=131).contains(&other) => other,
        _ => LINUX_EIO,
    };
    -linux_code
}

fn last_fuse_error() -> i32 {
    host_errno_to_fuse_error(
        io::Error::last_os_error()
            .raw_os_error()
            .unwrap_or(libc::EIO),
    )
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ShareConfig {
    pub name: String,
    pub path: PathBuf,
    pub read_only: bool,
}

pub fn validate_share_name(name: &str) -> Result<(), String> {
    if name.is_empty() || name.len() > MAX_NAME_LEN || name == "." || name == ".." {
        return Err(format!("invalid share name '{name}'"));
    }
    if name.bytes().any(|b| b == b'/' || b == 0) {
        return Err(format!("invalid share name '{name}'"));
    }
    Ok(())
}

pub fn parse_share_spec(spec: &str, read_only: bool) -> Result<ShareConfig, String> {
    let Some((name, dir)) = spec.split_once('=') else {
        return Err(format!("invalid share '{spec}' (expected NAME=DIR)"));
    };
    validate_share_name(name)?;
    if !dir.starts_with('/') {
        return Err(format!("share '{name}' path '{dir}' must be absolute"));
    }
    Ok(ShareConfig {
        name: name.to_string(),
        path: PathBuf::from(dir),
        read_only,
    })
}

fn validate_component(raw: &[u8]) -> Result<CString, i32> {
    if raw.is_empty() {
        return Err(-LINUX_EINVAL);
    }
    if raw.len() > MAX_NAME_LEN {
        return Err(-LINUX_ENAMETOOLONG);
    }
    if raw == b"." || raw == b".." {
        return Err(-LINUX_EINVAL);
    }
    if raw.iter().any(|&b| b == b'/' || b == 0) {
        return Err(-LINUX_EINVAL);
    }
    CString::new(raw).map_err(|_| -LINUX_EINVAL)
}

fn split_cstr(bytes: &[u8]) -> Result<(&[u8], &[u8]), i32> {
    let Some(nul) = bytes.iter().position(|&b| b == 0) else {
        return Err(-LINUX_EINVAL);
    };
    Ok((&bytes[..nul], &bytes[nul + 1..]))
}

#[inline]
fn read_u32_le(b: &[u8], off: usize) -> u32 {
    u32::from_le_bytes(b[off..off + 4].try_into().unwrap())
}

#[inline]
fn read_u64_le(b: &[u8], off: usize) -> u64 {
    u64::from_le_bytes(b[off..off + 8].try_into().unwrap())
}

#[inline]
fn fuse_align8(n: usize) -> usize {
    (n + 7) & !7
}

fn put_out_header(buf: &mut [u8], len: u32, error: i32, unique: u64) {
    buf[0..4].copy_from_slice(&len.to_le_bytes());
    buf[4..8].copy_from_slice(&error.to_le_bytes());
    buf[8..16].copy_from_slice(&unique.to_le_bytes());
}

fn finish_ok(buf: &mut [u8], unique: u64) {
    let len = buf.len() as u32;
    put_out_header(buf, len, 0, unique);
}

fn make_error_reply(unique: u64, error: i32) -> Vec<u8> {
    let mut out = vec![0u8; FUSE_OUT_HEADER_LEN];
    put_out_header(&mut out, FUSE_OUT_HEADER_LEN as u32, error, unique);
    out
}

fn make_ok_empty(unique: u64) -> Vec<u8> {
    make_error_reply(unique, 0)
}

#[derive(Clone, Copy)]
struct FuseAttr {
    ino: u64,
    size: u64,
    blocks: u64,
    atime: u64,
    mtime: u64,
    ctime: u64,
    atimensec: u32,
    mtimensec: u32,
    ctimensec: u32,
    mode: u32,
    nlink: u32,
    uid: u32,
    gid: u32,
    rdev: u32,
    blksize: u32,
}

impl FuseAttr {
    fn from_stat(st: &libc::stat) -> Self {
        let size = if st.st_size < 0 {
            0
        } else {
            (st.st_size as u64).min(VIRTIOFS_SIZE_MAX)
        };
        let blocks = if st.st_blocks < 0 {
            0
        } else {
            st.st_blocks as u64
        };
        let clamp_sec = |s: i64| if s < 0 { 0 } else { s as u64 };
        let clamp_nsec = |ns: i64| ns.clamp(0, 999_999_999) as u32;
        Self {
            ino: st.st_ino as u64,
            size,
            blocks,
            atime: clamp_sec(st.st_atime as i64),
            mtime: clamp_sec(st.st_mtime as i64),
            ctime: clamp_sec(st.st_ctime as i64),
            atimensec: clamp_nsec(st.st_atime_nsec as i64),
            mtimensec: clamp_nsec(st.st_mtime_nsec as i64),
            ctimensec: clamp_nsec(st.st_ctime_nsec as i64),
            mode: (st.st_mode as u32) & 0o17_7777,
            nlink: (st.st_nlink as u64).min(u32::MAX as u64) as u32,
            uid: st.st_uid,
            gid: st.st_gid,
            rdev: (st.st_rdev as u64).min(u32::MAX as u64) as u32,
            blksize: (st.st_blksize as u64).clamp(512, 65536) as u32,
        }
    }

    fn write_to(&self, dst: &mut [u8]) {
        dst[0..8].copy_from_slice(&self.ino.to_le_bytes());
        dst[8..16].copy_from_slice(&self.size.to_le_bytes());
        dst[16..24].copy_from_slice(&self.blocks.to_le_bytes());
        dst[24..32].copy_from_slice(&self.atime.to_le_bytes());
        dst[32..40].copy_from_slice(&self.mtime.to_le_bytes());
        dst[40..48].copy_from_slice(&self.ctime.to_le_bytes());
        dst[48..52].copy_from_slice(&self.atimensec.to_le_bytes());
        dst[52..56].copy_from_slice(&self.mtimensec.to_le_bytes());
        dst[56..60].copy_from_slice(&self.ctimensec.to_le_bytes());
        dst[60..64].copy_from_slice(&self.mode.to_le_bytes());
        dst[64..68].copy_from_slice(&self.nlink.to_le_bytes());
        dst[68..72].copy_from_slice(&self.uid.to_le_bytes());
        dst[72..76].copy_from_slice(&self.gid.to_le_bytes());
        dst[76..80].copy_from_slice(&self.rdev.to_le_bytes());
        dst[80..84].copy_from_slice(&self.blksize.to_le_bytes());
        dst[84..88].copy_from_slice(&0u32.to_le_bytes());
    }
}

fn write_entry_out(dst: &mut [u8], nodeid: u64, generation: u64, attr: &FuseAttr) {
    dst[0..8].copy_from_slice(&nodeid.to_le_bytes());
    dst[8..16].copy_from_slice(&generation.to_le_bytes());
    dst[16..24].copy_from_slice(&DEFAULT_ENTRY_VALID_SEC.to_le_bytes());
    dst[24..32].copy_from_slice(&DEFAULT_ATTR_VALID_SEC.to_le_bytes());
    dst[32..36].copy_from_slice(&0u32.to_le_bytes());
    dst[36..40].copy_from_slice(&0u32.to_le_bytes());
    attr.write_to(&mut dst[40..40 + FUSE_ATTR_LEN]);
}

fn write_attr_out(dst: &mut [u8], attr: &FuseAttr) {
    dst[0..8].copy_from_slice(&DEFAULT_ATTR_VALID_SEC.to_le_bytes());
    dst[8..12].copy_from_slice(&0u32.to_le_bytes());
    dst[12..16].copy_from_slice(&0u32.to_le_bytes());
    attr.write_to(&mut dst[16..16 + FUSE_ATTR_LEN]);
}

fn write_open_out(dst: &mut [u8], fh: u64) {
    dst[0..8].copy_from_slice(&fh.to_le_bytes());
    dst[8..12].copy_from_slice(&0u32.to_le_bytes());
    dst[12..16].copy_from_slice(&0i32.to_le_bytes());
}

fn mode_to_dt(mode: u32) -> u32 {
    match (mode & (libc::S_IFMT as u32)) as libc::mode_t {
        libc::S_IFIFO => libc::DT_FIFO as u32,
        libc::S_IFCHR => libc::DT_CHR as u32,
        libc::S_IFDIR => libc::DT_DIR as u32,
        libc::S_IFBLK => libc::DT_BLK as u32,
        libc::S_IFREG => libc::DT_REG as u32,
        libc::S_IFLNK => libc::DT_LNK as u32,
        libc::S_IFSOCK => libc::DT_SOCK as u32,
        _ => libc::DT_UNKNOWN as u32,
    }
}

// ---------- Low-level confined host filesystem helpers ----------

#[cfg(target_os = "linux")]
#[repr(C)]
struct OpenHow {
    flags: u64,
    mode: u64,
    resolve: u64,
}

#[cfg(target_os = "linux")]
const RESOLVE_NO_MAGICLINKS: u64 = 0x02;
#[cfg(target_os = "linux")]
const RESOLVE_NO_SYMLINKS: u64 = 0x04;
#[cfg(target_os = "linux")]
const RESOLVE_BENEATH: u64 = 0x08;

/// Opens a single validated child component beneath `dir_fd` using Linux
/// `openat2(RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS)`
/// when available, with safe `openat(O_NOFOLLOW)` fallback on ENOSYS or non-Linux.
fn openat_confined(
    dir_fd: RawFd,
    name: &CStr,
    flags: i32,
    mode: libc::mode_t,
) -> Result<OwnedFd, i32> {
    #[cfg(target_os = "linux")]
    {
        let open_flags = flags | libc::O_CLOEXEC | libc::O_NOFOLLOW;
        let how = OpenHow {
            flags: (open_flags as u32) as u64,
            mode: (mode & 0o1777) as u64,
            resolve: RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS,
        };
        loop {
            let rc = unsafe {
                libc::syscall(
                    libc::SYS_openat2,
                    dir_fd,
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
            return Err(host_errno_to_fuse_error(err));
        }
    }
    loop {
        let rc = unsafe {
            libc::openat(
                dir_fd,
                name.as_ptr(),
                flags | libc::O_CLOEXEC | libc::O_NOFOLLOW,
                (mode & 0o1777) as libc::c_uint,
            )
        };
        if rc >= 0 {
            return Ok(unsafe { OwnedFd::from_raw_fd(rc) });
        }
        let err = io::Error::last_os_error()
            .raw_os_error()
            .unwrap_or(libc::EIO);
        if err == libc::EINTR {
            continue;
        }
        return Err(host_errno_to_fuse_error(err));
    }
}

/// Opens a pinned inode handle for `name` beneath `dir_fd` without following
/// symlinks and without blocking on FIFOs or device nodes.
/// Caller MUST pass a validated single path component (no '/', no NUL, not "."/"..").
fn open_inode_handle(dir_fd: RawFd, name: &CStr) -> Result<OwnedFd, i32> {
    #[cfg(target_os = "linux")]
    {
        // Note: RESOLVE_NO_SYMLINKS rejects even a trailing symlink with ELOOP,
        // which would break lookup/readlink of symlinks inside a share. Because
        // `name` is strictly a single component and O_PATH | O_NOFOLLOW opens
        // the symlink inode itself without dereferencing it, RESOLVE_BENEATH |
        // RESOLVE_NO_MAGICLINKS enforces full confinement while allowing S_IFLNK lookup.
        let open_flags = libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC;
        let how = OpenHow {
            flags: (open_flags as u32) as u64,
            mode: 0,
            resolve: RESOLVE_BENEATH | RESOLVE_NO_MAGICLINKS,
        };
        loop {
            let rc = unsafe {
                libc::syscall(
                    libc::SYS_openat2,
                    dir_fd,
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
            return Err(host_errno_to_fuse_error(err));
        }
        loop {
            let rc = unsafe {
                libc::openat(
                    dir_fd,
                    name.as_ptr(),
                    libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC,
                )
            };
            if rc >= 0 {
                return Ok(unsafe { OwnedFd::from_raw_fd(rc) });
            }
            let err = io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO);
            if err == libc::EINTR {
                continue;
            }
            return Err(host_errno_to_fuse_error(err));
        }
    }
    #[cfg(target_os = "macos")]
    {
        match openat_confined(dir_fd, name, libc::O_RDONLY | libc::O_NONBLOCK, 0) {
            Ok(fd) => Ok(fd),
            Err(e) if e == -LINUX_ELOOP => loop {
                // On macOS, O_SYMLINK opens the symlink itself without following it
                // (combining O_SYMLINK with O_NOFOLLOW returns ELOOP).
                let rc = unsafe {
                    libc::openat(dir_fd, name.as_ptr(), libc::O_SYMLINK | libc::O_CLOEXEC)
                };
                if rc >= 0 {
                    return Ok(unsafe { OwnedFd::from_raw_fd(rc) });
                }
                let err = io::Error::last_os_error()
                    .raw_os_error()
                    .unwrap_or(libc::EIO);
                if err == libc::EINTR {
                    continue;
                }
                return Err(host_errno_to_fuse_error(err));
            },
            Err(e) => Err(e),
        }
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    {
        openat_confined(dir_fd, name, libc::O_RDONLY | libc::O_NONBLOCK, 0)
    }
}

fn open_share_root(path: &Path) -> Result<(OwnedFd, libc::stat), String> {
    let bytes = path.as_os_str().as_encoded_bytes();
    if bytes.contains(&0) {
        return Err(format!("share path '{}' contains NUL", path.display()));
    }
    #[cfg(target_os = "linux")]
    let fd = {
        if !bytes.starts_with(b"/") {
            return Err(format!(
                "share root '{}' must be an absolute path",
                path.display()
            ));
        }
        let dir_flags = libc::O_RDONLY | libc::O_DIRECTORY | libc::O_CLOEXEC;
        let slash_fd = loop {
            let rc = unsafe { libc::open(c"/".as_ptr(), dir_flags) };
            if rc >= 0 {
                break unsafe { OwnedFd::from_raw_fd(rc) };
            }
            let err = io::Error::last_os_error();
            if err.raw_os_error() == Some(libc::EINTR) {
                continue;
            }
            return Err(format!("open '/' for share '{}': {err}", path.display()));
        };
        let trimmed = bytes.iter().position(|&b| b != b'/').map(|i| &bytes[i..]);
        if let Some(rel_bytes) = trimmed {
            let crel = CString::new(rel_bytes)
                .map_err(|_| format!("share path '{}' contains NUL", path.display()))?;
            let how = OpenHow {
                flags: (dir_flags as u32) as u64,
                mode: 0,
                resolve: RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS,
            };
            let mut opened: Option<OwnedFd> = None;
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
                    opened = Some(unsafe { OwnedFd::from_raw_fd(rc as RawFd) });
                    break;
                }
                let err = io::Error::last_os_error();
                if err.raw_os_error() == Some(libc::EINTR) {
                    continue;
                }
                if err.raw_os_error() == Some(libc::ENOSYS) {
                    break;
                }
                return Err(format!("open share root '{}': {err}", path.display()));
            }
            if let Some(fd) = opened {
                fd
            } else {
                // ENOSYS fallback: step component-by-component with O_NOFOLLOW.
                let mut cur = slash_fd;
                for comp in rel_bytes.split(|&b| b == b'/') {
                    if comp.is_empty() {
                        continue;
                    }
                    let ccomp = validate_component(comp)
                        .map_err(|_| format!("invalid component in share path '{}'", path.display()))?;
                    cur = openat_confined(cur.as_raw_fd(), &ccomp, libc::O_RDONLY | libc::O_DIRECTORY, 0)
                        .map_err(|e| format!("open share root '{}': fuse errno {e}", path.display()))?;
                }
                cur
            }
        } else {
            slash_fd
        }
    };
    #[cfg(not(target_os = "linux"))]
    let fd = {
        let cpath = CString::new(bytes)
            .map_err(|_| format!("share path '{}' contains NUL", path.display()))?;
        loop {
            let rc = unsafe {
                libc::open(
                    cpath.as_ptr(),
                    libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC,
                )
            };
            if rc >= 0 {
                break unsafe { OwnedFd::from_raw_fd(rc) };
            }
            let err = io::Error::last_os_error();
            if err.raw_os_error() == Some(libc::EINTR) {
                continue;
            }
            return Err(format!("open share root '{}': {err}", path.display()));
        }
    };
    let st = fstat_fd(fd.as_raw_fd())
        .map_err(|e| format!("stat share root '{}': fuse errno {e}", path.display()))?;
    if (st.st_mode & libc::S_IFMT) != libc::S_IFDIR {
        return Err(format!("share root '{}' is not a directory", path.display()));
    }
    Ok((fd, st))
}

fn fstat_fd(fd: RawFd) -> Result<libc::stat, i32> {
    let mut st: libc::stat = unsafe { std::mem::zeroed() };
    #[cfg(target_os = "linux")]
    {
        let rc = unsafe { libc::fstatat(fd, c"".as_ptr(), &mut st, libc::AT_EMPTY_PATH) };
        if rc == 0 {
            Ok(st)
        } else {
            Err(last_fuse_error())
        }
    }
    #[cfg(not(target_os = "linux"))]
    {
        let rc = unsafe { libc::fstat(fd, &mut st) };
        if rc == 0 {
            Ok(st)
        } else {
            Err(last_fuse_error())
        }
    }
}

#[cfg(target_os = "linux")]
fn proc_self_fd_cstr(fd: RawFd) -> CString {
    CString::new(format!("/proc/self/fd/{fd}")).unwrap()
}

/// Re-opens a pinned `InodeEntry` handle for regular-file I/O or directory reading.
/// Never follows symlinks: caller must pass verified `file_type` (`S_IFREG` or `S_IFDIR`).
fn reopen_inode_fd(
    pinned_fd: RawFd,
    file_type: u32,
    open_flags: i32,
    is_dir: bool,
) -> Result<OwnedFd, i32> {
    if is_dir {
        if file_type != (libc::S_IFDIR as u32) {
            return Err(-LINUX_ENOTDIR);
        }
    } else if file_type != (libc::S_IFREG as u32) {
        return Err(match file_type as libc::mode_t {
            libc::S_IFDIR => -LINUX_EISDIR,
            libc::S_IFLNK => -LINUX_ELOOP,
            libc::S_IFCHR | libc::S_IFBLK => -LINUX_ENXIO,
            _ => -LINUX_EOPNOTSUPP,
        });
    }

    #[cfg(target_os = "linux")]
    {
        let proc_path = proc_self_fd_cstr(pinned_fd);
        let dir_flag = if is_dir { libc::O_DIRECTORY } else { 0 };
        loop {
            let rc = unsafe {
                libc::open(
                    proc_path.as_ptr(),
                    open_flags | dir_flag | libc::O_CLOEXEC | libc::O_NOCTTY,
                )
            };
            if rc >= 0 {
                return Ok(unsafe { OwnedFd::from_raw_fd(rc) });
            }
            let err = io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO);
            if err == libc::EINTR {
                continue;
            }
            return Err(host_errno_to_fuse_error(err));
        }
    }

    #[cfg(not(target_os = "linux"))]
    {
        if is_dir {
            let rc = unsafe { libc::fcntl(pinned_fd, libc::F_DUPFD_CLOEXEC, 0) };
            if rc < 0 {
                return Err(last_fuse_error());
            }
            return Ok(unsafe { OwnedFd::from_raw_fd(rc) });
        }
        // macOS fallback for local unit tests: resolve the pinned FD path via F_GETPATH
        // and re-open with O_NOFOLLOW + verify (dev, ino) still matches.
        let orig_st = fstat_fd(pinned_fd)?;
        let mut path_buf = [0u8; libc::PATH_MAX as usize];
        let rc = unsafe { libc::fcntl(pinned_fd, libc::F_GETPATH, path_buf.as_mut_ptr()) };
        if rc < 0 {
            return Err(last_fuse_error());
        }
        let nul = path_buf
            .iter()
            .position(|&b| b == 0)
            .unwrap_or(path_buf.len());
        let cpath = CString::new(&path_buf[..nul]).map_err(|_| -LINUX_EIO)?;
        let fd = loop {
            let r = unsafe {
                libc::open(
                    cpath.as_ptr(),
                    open_flags | libc::O_NOFOLLOW | libc::O_CLOEXEC | libc::O_NOCTTY,
                )
            };
            if r >= 0 {
                break unsafe { OwnedFd::from_raw_fd(r) };
            }
            let err = io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO);
            if err == libc::EINTR {
                continue;
            }
            return Err(host_errno_to_fuse_error(err));
        };
        let new_st = fstat_fd(fd.as_raw_fd())?;
        if new_st.st_dev != orig_st.st_dev || new_st.st_ino != orig_st.st_ino {
            return Err(-LINUX_ESTALE);
        }
        Ok(fd)
    }
}

fn readlink_pinned(pinned_fd: RawFd, file_type: u32) -> Result<Vec<u8>, i32> {
    if file_type != (libc::S_IFLNK as u32) {
        return Err(-LINUX_EINVAL);
    }
    let mut buf = vec![0u8; MAX_SYMLINK_LEN];
    #[cfg(target_os = "linux")]
    {
        let n = unsafe {
            libc::readlinkat(
                pinned_fd,
                c"".as_ptr(),
                buf.as_mut_ptr().cast::<libc::c_char>(),
                buf.len(),
            )
        };
        if n < 0 {
            return Err(last_fuse_error());
        }
        buf.truncate(n as usize);
        Ok(buf)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let mut path_buf = [0u8; libc::PATH_MAX as usize];
        let rc = unsafe { libc::fcntl(pinned_fd, libc::F_GETPATH, path_buf.as_mut_ptr()) };
        if rc < 0 {
            return Err(last_fuse_error());
        }
        let nul = path_buf
            .iter()
            .position(|&b| b == 0)
            .unwrap_or(path_buf.len());
        let cpath = CString::new(&path_buf[..nul]).map_err(|_| -LINUX_EIO)?;
        let n = unsafe {
            libc::readlink(
                cpath.as_ptr(),
                buf.as_mut_ptr().cast::<libc::c_char>(),
                buf.len(),
            )
        };
        if n < 0 {
            return Err(last_fuse_error());
        }
        buf.truncate(n as usize);
        Ok(buf)
    }
}

fn fchmod_inode(
    pinned_fd: RawFd,
    file_type: u32,
    fh_fd: Option<RawFd>,
    mode: u32,
) -> Result<(), i32> {
    if file_type == (libc::S_IFLNK as u32) {
        return Err(-LINUX_EOPNOTSUPP);
    }
    // Strip SUID/SGID so guest cannot plant privileged host binaries.
    let safe_mode = (mode & 0o1777) as libc::mode_t;
    if let Some(fd) = fh_fd {
        let rc = unsafe { libc::fchmod(fd, safe_mode) };
        return if rc == 0 { Ok(()) } else { Err(last_fuse_error()) };
    }
    #[cfg(target_os = "linux")]
    {
        let proc_path = proc_self_fd_cstr(pinned_fd);
        let rc = unsafe { libc::chmod(proc_path.as_ptr(), safe_mode) };
        if rc == 0 {
            Ok(())
        } else {
            Err(last_fuse_error())
        }
    }
    #[cfg(not(target_os = "linux"))]
    {
        let rc = unsafe { libc::fchmod(pinned_fd, safe_mode) };
        if rc == 0 {
            Ok(())
        } else {
            Err(last_fuse_error())
        }
    }
}

fn ftruncate_inode(
    pinned_fd: RawFd,
    file_type: u32,
    fh_fd: Option<RawFd>,
    size: u64,
) -> Result<(), i32> {
    if size > VIRTIOFS_SIZE_MAX {
        return Err(-LINUX_EFBIG);
    }
    let off = libc::off_t::try_from(size).map_err(|_| -LINUX_EFBIG)?;
    if let Some(fd) = fh_fd {
        loop {
            let rc = unsafe { libc::ftruncate(fd, off) };
            if rc == 0 {
                return Ok(());
            }
            let err = io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO);
            if err == libc::EINTR {
                continue;
            }
            return Err(host_errno_to_fuse_error(err));
        }
    }
    let tmp = reopen_inode_fd(pinned_fd, file_type, libc::O_WRONLY, false)?;
    loop {
        let rc = unsafe { libc::ftruncate(tmp.as_raw_fd(), off) };
        if rc == 0 {
            return Ok(());
        }
        let err = io::Error::last_os_error()
            .raw_os_error()
            .unwrap_or(libc::EIO);
        if err == libc::EINTR {
            continue;
        }
        return Err(host_errno_to_fuse_error(err));
    }
}

fn utimens_inode(
    pinned_fd: RawFd,
    file_type: u32,
    fh_fd: Option<RawFd>,
    times: &[libc::timespec; 2],
) -> Result<(), i32> {
    if let Some(fd) = fh_fd {
        let rc = unsafe { libc::futimens(fd, times.as_ptr()) };
        return if rc == 0 { Ok(()) } else { Err(last_fuse_error()) };
    }
    if file_type == (libc::S_IFLNK as u32) {
        return Err(-LINUX_EOPNOTSUPP);
    }
    #[cfg(target_os = "linux")]
    {
        let proc_path = proc_self_fd_cstr(pinned_fd);
        let rc = unsafe { libc::utimensat(libc::AT_FDCWD, proc_path.as_ptr(), times.as_ptr(), 0) };
        if rc == 0 {
            Ok(())
        } else {
            Err(last_fuse_error())
        }
    }
    #[cfg(not(target_os = "linux"))]
    {
        let rc = unsafe { libc::futimens(pinned_fd, times.as_ptr()) };
        if rc == 0 {
            Ok(())
        } else {
            Err(last_fuse_error())
        }
    }
}

fn link_inode_into(
    old_pinned_fd: RawFd,
    old_file_type: u32,
    newdir_fd: RawFd,
    newname: &CStr,
) -> Result<(), i32> {
    if old_file_type == (libc::S_IFDIR as u32) {
        return Err(-LINUX_EPERM);
    }
    #[cfg(target_os = "linux")]
    {
        // Prefer /proc/self/fd/<fd> with AT_SYMLINK_FOLLOW for non-symlinks so
        // unprivileged hosts (e.g. RPi5 without CAP_DAC_READ_SEARCH) succeed.
        if old_file_type != (libc::S_IFLNK as u32) {
            let proc_path = proc_self_fd_cstr(old_pinned_fd);
            let rc = unsafe {
                libc::linkat(
                    libc::AT_FDCWD,
                    proc_path.as_ptr(),
                    newdir_fd,
                    newname.as_ptr(),
                    libc::AT_SYMLINK_FOLLOW,
                )
            };
            if rc == 0 {
                return Ok(());
            }
            let err = io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO);
            if err != libc::ENOENT && err != libc::EACCES {
                return Err(host_errno_to_fuse_error(err));
            }
        }
        let rc = unsafe {
            libc::linkat(
                old_pinned_fd,
                c"".as_ptr(),
                newdir_fd,
                newname.as_ptr(),
                libc::AT_EMPTY_PATH,
            )
        };
        if rc == 0 {
            Ok(())
        } else {
            Err(last_fuse_error())
        }
    }
    #[cfg(not(target_os = "linux"))]
    {
        let mut path_buf = [0u8; libc::PATH_MAX as usize];
        let rc = unsafe { libc::fcntl(old_pinned_fd, libc::F_GETPATH, path_buf.as_mut_ptr()) };
        if rc < 0 {
            return Err(last_fuse_error());
        }
        let nul = path_buf
            .iter()
            .position(|&b| b == 0)
            .unwrap_or(path_buf.len());
        let cpath = CString::new(&path_buf[..nul]).map_err(|_| -LINUX_EIO)?;
        let rc = unsafe {
            libc::linkat(
                libc::AT_FDCWD,
                cpath.as_ptr(),
                newdir_fd,
                newname.as_ptr(),
                0,
            )
        };
        if rc == 0 {
            Ok(())
        } else {
            Err(last_fuse_error())
        }
    }
}

fn statfs_fd(fd: RawFd) -> Result<libc::statvfs, i32> {
    let mut sv: libc::statvfs = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::fstatvfs(fd, &mut sv) };
    if rc == 0 {
        Ok(sv)
    } else {
        Err(last_fuse_error())
    }
}

// ---------- Xattr helpers (sandboxed under user.kido.xattr.* on Linux) ----------

#[cfg(target_os = "linux")]
fn host_xattr_name(guest_name: &[u8]) -> Result<CString, i32> {
    if guest_name.is_empty() || guest_name.contains(&0) {
        return Err(-LINUX_EINVAL);
    }
    let total = LINUX_XATTR_PREFIX.len() + guest_name.len();
    if total > LINUX_XATTR_NAME_MAX {
        return Err(-LINUX_ENAMETOOLONG);
    }
    let mut out = Vec::with_capacity(total + 1);
    out.extend_from_slice(LINUX_XATTR_PREFIX.as_bytes());
    out.extend_from_slice(guest_name);
    CString::new(out).map_err(|_| -LINUX_EINVAL)
}

#[cfg(not(target_os = "linux"))]
fn host_xattr_name(guest_name: &[u8]) -> Result<CString, i32> {
    if guest_name.is_empty() || guest_name.len() > 127 || guest_name.contains(&0) {
        return Err(-LINUX_EINVAL);
    }
    CString::new(guest_name).map_err(|_| -LINUX_EINVAL)
}

fn xattr_get_inode(
    pinned_fd: RawFd,
    file_type: u32,
    guest_name: &[u8],
    max_size: u32,
) -> Result<(u32, Vec<u8>), i32> {
    if file_type == (libc::S_IFLNK as u32) {
        return Err(-LINUX_ENODATA);
    }
    let cname = host_xattr_name(guest_name)?;
    #[cfg(target_os = "linux")]
    {
        let proc_path = proc_self_fd_cstr(pinned_fd);
        if max_size == 0 {
            let n = unsafe {
                libc::getxattr(proc_path.as_ptr(), cname.as_ptr(), std::ptr::null_mut(), 0)
            };
            if n < 0 {
                return Err(last_fuse_error());
            }
            return Ok((n as u32, Vec::new()));
        }
        let cap = (max_size as usize).min(MAX_XATTR_VALUE);
        let mut buf = vec![0u8; cap];
        let n = unsafe {
            libc::getxattr(
                proc_path.as_ptr(),
                cname.as_ptr(),
                buf.as_mut_ptr().cast::<libc::c_void>(),
                buf.len(),
            )
        };
        if n < 0 {
            return Err(last_fuse_error());
        }
        buf.truncate(n as usize);
        Ok((n as u32, buf))
    }
    #[cfg(target_os = "macos")]
    {
        if max_size == 0 {
            let n = unsafe {
                libc::fgetxattr(pinned_fd, cname.as_ptr(), std::ptr::null_mut(), 0, 0, 0)
            };
            if n < 0 {
                return Err(last_fuse_error());
            }
            return Ok((n as u32, Vec::new()));
        }
        let cap = (max_size as usize).min(MAX_XATTR_VALUE);
        let mut buf = vec![0u8; cap];
        let n = unsafe {
            libc::fgetxattr(
                pinned_fd,
                cname.as_ptr(),
                buf.as_mut_ptr().cast::<libc::c_void>(),
                buf.len(),
                0,
                0,
            )
        };
        if n < 0 {
            return Err(last_fuse_error());
        }
        buf.truncate(n as usize);
        Ok((n as u32, buf))
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    {
        let _ = (pinned_fd, cname, max_size);
        Err(-LINUX_EOPNOTSUPP)
    }
}

fn xattr_list_inode(pinned_fd: RawFd, file_type: u32, max_size: u32) -> Result<(u32, Vec<u8>), i32> {
    if file_type == (libc::S_IFLNK as u32) {
        return Ok((0, Vec::new()));
    }
    #[cfg(target_os = "linux")]
    {
        let proc_path = proc_self_fd_cstr(pinned_fd);
        let raw_len = unsafe { libc::listxattr(proc_path.as_ptr(), std::ptr::null_mut(), 0) };
        if raw_len < 0 {
            let err = last_fuse_error();
            if err == -LINUX_EOPNOTSUPP || err == -LINUX_ENOSYS {
                return Ok((0, Vec::new()));
            }
            return Err(err);
        }
        if raw_len == 0 {
            return Ok((0, Vec::new()));
        }
        let mut raw = vec![0u8; (raw_len as usize).min(MAX_XATTR_VALUE)];
        let got = unsafe {
            libc::listxattr(
                proc_path.as_ptr(),
                raw.as_mut_ptr().cast::<libc::c_char>(),
                raw.len(),
            )
        };
        if got < 0 {
            return Err(last_fuse_error());
        }
        raw.truncate(got as usize);
        let prefix = LINUX_XATTR_PREFIX.as_bytes();
        let mut filtered = Vec::new();
        for entry in raw.split(|&b| b == 0) {
            if let Some(stripped) = entry.strip_prefix(prefix) {
                if !stripped.is_empty() {
                    filtered.extend_from_slice(stripped);
                    filtered.push(0);
                }
            }
        }
        if max_size == 0 {
            return Ok((filtered.len() as u32, Vec::new()));
        }
        if filtered.len() > max_size as usize {
            return Err(-LINUX_ERANGE);
        }
        Ok((filtered.len() as u32, filtered))
    }
    #[cfg(target_os = "macos")]
    {
        let raw_len = unsafe { libc::flistxattr(pinned_fd, std::ptr::null_mut(), 0, 0) };
        if raw_len < 0 {
            let err = last_fuse_error();
            if err == -LINUX_EOPNOTSUPP || err == -LINUX_ENOSYS {
                return Ok((0, Vec::new()));
            }
            return Err(err);
        }
        if raw_len == 0 {
            return Ok((0, Vec::new()));
        }
        let mut raw = vec![0u8; (raw_len as usize).min(MAX_XATTR_VALUE)];
        let got = unsafe {
            libc::flistxattr(
                pinned_fd,
                raw.as_mut_ptr().cast::<libc::c_char>(),
                raw.len(),
                0,
            )
        };
        if got < 0 {
            return Err(last_fuse_error());
        }
        raw.truncate(got as usize);
        if max_size == 0 {
            return Ok((raw.len() as u32, Vec::new()));
        }
        if raw.len() > max_size as usize {
            return Err(-LINUX_ERANGE);
        }
        Ok((raw.len() as u32, raw))
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    {
        let _ = (pinned_fd, max_size);
        Ok((0, Vec::new()))
    }
}

fn xattr_set_inode(
    pinned_fd: RawFd,
    file_type: u32,
    guest_name: &[u8],
    value: &[u8],
    fuse_flags: u32,
) -> Result<(), i32> {
    if file_type == (libc::S_IFLNK as u32) {
        return Err(-LINUX_EOPNOTSUPP);
    }
    if value.len() > MAX_XATTR_VALUE {
        return Err(-LINUX_E2BIG);
    }
    if (fuse_flags & !(FUSE_XATTR_CREATE | FUSE_XATTR_REPLACE)) != 0 {
        return Err(-LINUX_EINVAL);
    }
    let cname = host_xattr_name(guest_name)?;
    #[cfg(target_os = "linux")]
    {
        let mut host_flags = 0i32;
        if (fuse_flags & FUSE_XATTR_CREATE) != 0 {
            host_flags |= libc::XATTR_CREATE;
        }
        if (fuse_flags & FUSE_XATTR_REPLACE) != 0 {
            host_flags |= libc::XATTR_REPLACE;
        }
        let proc_path = proc_self_fd_cstr(pinned_fd);
        let rc = unsafe {
            libc::setxattr(
                proc_path.as_ptr(),
                cname.as_ptr(),
                value.as_ptr().cast::<libc::c_void>(),
                value.len(),
                host_flags,
            )
        };
        if rc == 0 {
            Ok(())
        } else {
            Err(last_fuse_error())
        }
    }
    #[cfg(target_os = "macos")]
    {
        let mut host_flags = 0i32;
        if (fuse_flags & FUSE_XATTR_CREATE) != 0 {
            host_flags |= libc::XATTR_CREATE;
        }
        if (fuse_flags & FUSE_XATTR_REPLACE) != 0 {
            host_flags |= libc::XATTR_REPLACE;
        }
        let rc = unsafe {
            libc::fsetxattr(
                pinned_fd,
                cname.as_ptr(),
                value.as_ptr().cast::<libc::c_void>(),
                value.len(),
                0,
                host_flags,
            )
        };
        if rc == 0 {
            Ok(())
        } else {
            Err(last_fuse_error())
        }
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    {
        let _ = (pinned_fd, cname, value, fuse_flags);
        Err(-LINUX_EOPNOTSUPP)
    }
}

fn xattr_remove_inode(pinned_fd: RawFd, file_type: u32, guest_name: &[u8]) -> Result<(), i32> {
    if file_type == (libc::S_IFLNK as u32) {
        return Err(-LINUX_ENODATA);
    }
    let cname = host_xattr_name(guest_name)?;
    #[cfg(target_os = "linux")]
    {
        let proc_path = proc_self_fd_cstr(pinned_fd);
        let rc = unsafe { libc::removexattr(proc_path.as_ptr(), cname.as_ptr()) };
        if rc == 0 {
            Ok(())
        } else {
            Err(last_fuse_error())
        }
    }
    #[cfg(target_os = "macos")]
    {
        let rc = unsafe { libc::fremovexattr(pinned_fd, cname.as_ptr(), 0) };
        if rc == 0 {
            Ok(())
        } else {
            Err(last_fuse_error())
        }
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    {
        let _ = (pinned_fd, cname);
        Err(-LINUX_EOPNOTSUPP)
    }
}

// ---------- Directory streaming cursor ----------

#[derive(Clone)]
struct RawDirEntry {
    ino: u64,
    off: u64,
    d_type: u32,
    name: Vec<u8>,
}

#[cfg(target_os = "linux")]
struct LinuxDirStream {
    dir: *mut libc::DIR,
    last_emitted_off: u64,
    peeked: Option<RawDirEntry>,
    eof: bool,
}

#[cfg(target_os = "linux")]
unsafe impl Send for LinuxDirStream {}

#[cfg(target_os = "linux")]
impl LinuxDirStream {
    fn from_fd(fd: OwnedFd) -> Result<Self, i32> {
        use std::os::unix::io::IntoRawFd;
        let raw = fd.into_raw_fd();
        let dir = unsafe { libc::fdopendir(raw) };
        if dir.is_null() {
            let err = last_fuse_error();
            unsafe { libc::close(raw) };
            return Err(err);
        }
        Ok(Self {
            dir,
            last_emitted_off: 0,
            peeked: None,
            eof: false,
        })
    }

    fn next_entry(&mut self) -> Result<Option<RawDirEntry>, i32> {
        if let Some(e) = self.peeked.take() {
            return Ok(Some(e));
        }
        if self.eof {
            return Ok(None);
        }
        unsafe {
            *libc::__errno_location() = 0;
            let ent = libc::readdir(self.dir);
            if ent.is_null() {
                let err = *libc::__errno_location();
                if err != 0 {
                    return Err(host_errno_to_fuse_error(err));
                }
                self.eof = true;
                return Ok(None);
            }
            let c_name = CStr::from_ptr((*ent).d_name.as_ptr());
            let name = c_name.to_bytes().to_vec();
            let ino = (*ent).d_ino as u64;
            let off = (*ent).d_off as u64;
            let d_type = (*ent).d_type as u32;
            Ok(Some(RawDirEntry {
                ino,
                off,
                d_type,
                name,
            }))
        }
    }

    fn seek_if_needed(&mut self, offset: u64) {
        if offset == 0 {
            unsafe { libc::rewinddir(self.dir) };
            self.last_emitted_off = 0;
            self.peeked = None;
            self.eof = false;
        } else if offset != self.last_emitted_off {
            unsafe { libc::seekdir(self.dir, offset as libc::c_long) };
            self.last_emitted_off = offset;
            self.peeked = None;
            self.eof = false;
        }
    }
}

#[cfg(target_os = "linux")]
impl Drop for LinuxDirStream {
    fn drop(&mut self) {
        if !self.dir.is_null() {
            unsafe { libc::closedir(self.dir) };
            self.dir = std::ptr::null_mut();
        }
    }
}

#[cfg(not(target_os = "linux"))]
struct SnapshotDirStream {
    dir_fd: OwnedFd,
    entries: Vec<RawDirEntry>,
    loaded: bool,
}

#[cfg(not(target_os = "linux"))]
impl SnapshotDirStream {
    fn from_fd(dir_fd: OwnedFd) -> Result<Self, i32> {
        Ok(Self {
            dir_fd,
            entries: Vec::new(),
            loaded: false,
        })
    }

    fn reload(&mut self) -> Result<(), i32> {
        let dup_fd = unsafe { libc::fcntl(self.dir_fd.as_raw_fd(), libc::F_DUPFD_CLOEXEC, 0) };
        if dup_fd < 0 {
            return Err(last_fuse_error());
        }
        unsafe { libc::lseek(dup_fd, 0, libc::SEEK_SET) };
        let dir = unsafe { libc::fdopendir(dup_fd) };
        if dir.is_null() {
            let err = last_fuse_error();
            unsafe { libc::close(dup_fd) };
            return Err(err);
        }
        self.entries.clear();
        let mut idx = 0u64;
        loop {
            let ent = unsafe { libc::readdir(dir) };
            if ent.is_null() {
                break;
            }
            let c_name = unsafe { CStr::from_ptr((*ent).d_name.as_ptr()) };
            idx += 1;
            self.entries.push(RawDirEntry {
                ino: unsafe { (*ent).d_ino as u64 },
                off: idx,
                d_type: unsafe { (*ent).d_type as u32 },
                name: c_name.to_bytes().to_vec(),
            });
            if self.entries.len() >= 200_000 {
                break;
            }
        }
        unsafe { libc::closedir(dir) };
        self.loaded = true;
        Ok(())
    }
}

enum DirCursor {
    VirtualRoot {
        entries: Vec<RawDirEntry>,
    },
    #[cfg(target_os = "linux")]
    Host(LinuxDirStream),
    #[cfg(not(target_os = "linux"))]
    Host(SnapshotDirStream),
}

// ---------- Inode & handle tables ----------

struct ShareState {
    name: String,
    read_only: bool,
    root_nodeid: u64,
    root_fd: Arc<OwnedFd>,
    root_dev: u64,
    root_ino: u64,
}

#[derive(Clone)]
struct InodeEntry {
    nodeid: u64,
    generation: u64,
    share_idx: usize,
    is_share_root: bool,
    parent_nodeid: u64,
    dev: u64,
    ino: u64,
    file_type: u32,
    fd: Arc<OwnedFd>,
    nlookup: u64,
}

#[derive(Clone)]
struct FileHandle {
    nodeid: u64,
    share_idx: usize,
    writable: bool,
    file: Arc<File>,
}

#[derive(Clone)]
struct DirHandle {
    nodeid: u64,
    share_idx: Option<usize>,
    cursor: Arc<Mutex<DirCursor>>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct FsLimits {
    pub max_inodes: usize,
    pub max_open_files: usize,
    pub max_open_dirs: usize,
}

impl FsLimits {
    pub fn from_nofile(nofile: usize) -> Self {
        let reserved = if nofile >= 1024 { 512 } else { nofile / 2 };
        let usable = nofile.saturating_sub(reserved).max(64);
        let max_open_files = (usable / 8).clamp(16, MAX_OPEN_FILES);
        let max_open_dirs = (usable / 32).clamp(8, MAX_OPEN_DIRS);
        let max_inodes = usable
            .saturating_sub(max_open_files)
            .saturating_sub(max_open_dirs)
            .clamp(32, MAX_INODES);
        Self {
            max_inodes,
            max_open_files,
            max_open_dirs,
        }
    }

    pub fn from_current_rlimit() -> Self {
        let mut rl = libc::rlimit {
            rlim_cur: 0,
            rlim_max: 0,
        };
        if unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut rl) } == 0
            && rl.rlim_cur != libc::RLIM_INFINITY
        {
            Self::from_nofile(rl.rlim_cur as usize)
        } else {
            Self {
                max_inodes: MAX_INODES,
                max_open_files: MAX_OPEN_FILES,
                max_open_dirs: MAX_OPEN_DIRS,
            }
        }
    }
}

struct DroppedTables {
    _fds: Vec<Arc<OwnedFd>>,
    _files: Vec<Arc<File>>,
    _dirs: Vec<Arc<Mutex<DirCursor>>>,
}

struct FsTables {
    session_epoch: u64,
    session_flags: u32,
    next_nodeid: u64,
    next_generation: u64,
    next_fh: u64,
    max_inodes: usize,
    inodes: HashMap<u64, InodeEntry>,
    inode_by_key: HashMap<(usize, u64, u64), u64>,
    files: HashMap<u64, FileHandle>,
    dirs: HashMap<u64, DirHandle>,
}

impl FsTables {
    fn new(shares: &[ShareState], limits: FsLimits) -> Self {
        let mut tables = Self {
            session_epoch: 1,
            session_flags: 0,
            next_nodeid: FUSE_ROOT_ID + 1,
            next_generation: 1,
            next_fh: 1,
            max_inodes: limits.max_inodes,
            inodes: HashMap::new(),
            inode_by_key: HashMap::new(),
            files: HashMap::new(),
            dirs: HashMap::new(),
        };
        tables.install_share_roots(shares);
        tables
    }

    fn install_share_roots(&mut self, shares: &[ShareState]) {
        for (idx, sh) in shares.iter().enumerate() {
            let entry = InodeEntry {
                nodeid: sh.root_nodeid,
                generation: 1,
                share_idx: idx,
                is_share_root: true,
                parent_nodeid: sh.root_nodeid,
                dev: sh.root_dev,
                ino: sh.root_ino,
                file_type: libc::S_IFDIR as u32,
                fd: Arc::clone(&sh.root_fd),
                nlookup: 0,
            };
            self.inode_by_key
                .insert((idx, sh.root_dev, sh.root_ino), sh.root_nodeid);
            self.inodes.insert(sh.root_nodeid, entry);
            if self.next_nodeid <= sh.root_nodeid {
                self.next_nodeid = sh.root_nodeid + 1;
            }
        }
    }

    fn reset(&mut self, shares: &[ShareState]) -> DroppedTables {
        let dropped_fds: Vec<Arc<OwnedFd>> = self.inodes.drain().map(|(_, e)| e.fd).collect();
        let dropped_files: Vec<Arc<File>> = self.files.drain().map(|(_, f)| f.file).collect();
        let dropped_dirs: Vec<Arc<Mutex<DirCursor>>> =
            self.dirs.drain().map(|(_, d)| d.cursor).collect();
        self.inode_by_key.clear();
        self.session_epoch = self.session_epoch.wrapping_add(1).max(1);
        self.session_flags = 0;
        self.next_nodeid = FUSE_ROOT_ID + 1;
        self.next_generation = self.next_generation.wrapping_add(1).max(1);
        self.next_fh = 1;
        self.install_share_roots(shares);
        DroppedTables {
            _fds: dropped_fds,
            _files: dropped_files,
            _dirs: dropped_dirs,
        }
    }

    fn get_or_insert_inode(
        &mut self,
        share_idx: usize,
        parent_nodeid: u64,
        st: &libc::stat,
        fd: OwnedFd,
        bump_lookup: u64,
    ) -> Result<(u64, u64, Option<OwnedFd>), (i32, OwnedFd)> {
        let key = (share_idx, st.st_dev as u64, st.st_ino as u64);
        let file_type = (st.st_mode as u32) & (libc::S_IFMT as u32);
        if let Some(&existing_id) = self.inode_by_key.get(&key) {
            if let Some(existing) = self.inodes.get_mut(&existing_id) {
                if existing.file_type == file_type {
                    existing.nlookup = existing.nlookup.saturating_add(bump_lookup);
                    if !existing.is_share_root {
                        existing.parent_nodeid = parent_nodeid;
                    }
                    return Ok((existing.nodeid, existing.generation, Some(fd)));
                }
            }
        }
        if self.inodes.len() >= self.max_inodes {
            return Err((-LINUX_ENOMEM, fd));
        }
        let nodeid = self.next_nodeid;
        let Some(next) = self.next_nodeid.checked_add(1) else {
            return Err((-LINUX_ENOMEM, fd));
        };
        self.next_nodeid = next;
        let generation = self.next_generation;
        self.next_generation = self.next_generation.wrapping_add(1).max(1);
        let entry = InodeEntry {
            nodeid,
            generation,
            share_idx,
            is_share_root: false,
            parent_nodeid,
            dev: st.st_dev as u64,
            ino: st.st_ino as u64,
            file_type,
            fd: Arc::new(fd),
            nlookup: bump_lookup,
        };
        self.inode_by_key.insert(key, nodeid);
        self.inodes.insert(nodeid, entry);
        Ok((nodeid, generation, None))
    }

    fn forget_one(&mut self, nodeid: u64, nlookup: u64) -> Option<Arc<OwnedFd>> {
        if nodeid == FUSE_ROOT_ID || nlookup == 0 {
            return None;
        }
        let remove = if let Some(entry) = self.inodes.get_mut(&nodeid) {
            entry.nlookup = entry.nlookup.saturating_sub(nlookup);
            entry.nlookup == 0 && !entry.is_share_root
        } else {
            false
        };
        if remove {
            if let Some(entry) = self.inodes.remove(&nodeid) {
                let key = (entry.share_idx, entry.dev, entry.ino);
                if self.inode_by_key.get(&key) == Some(&nodeid) {
                    self.inode_by_key.remove(&key);
                }
                return Some(entry.fd);
            }
        }
        None
    }
}

// ---------- Worker queue & in-flight budget ----------

struct BudgetGuard {
    counter: Option<Arc<AtomicUsize>>,
    bytes: usize,
}

impl BudgetGuard {
    fn untracked() -> Self {
        Self {
            counter: None,
            bytes: 0,
        }
    }
}

impl Drop for BudgetGuard {
    fn drop(&mut self) {
        if let Some(ref counter) = self.counter {
            counter.fetch_sub(self.bytes, Ordering::AcqRel);
        }
    }
}

struct FsJob {
    queue_index: u16,
    head_index: u16,
    expected_gen: u64,
    in_buf: Vec<u8>,
    out_segs: Vec<(u64, u32)>,
    out_cap: usize,
    _budget: BudgetGuard,
}

struct JobQueueState {
    hiprio: VecDeque<FsJob>,
    reqs: VecDeque<FsJob>,
    closed: bool,
}

struct JobQueue {
    mu: Mutex<JobQueueState>,
    cv: Condvar,
}

impl JobQueue {
    fn new() -> Self {
        Self {
            mu: Mutex::new(JobQueueState {
                hiprio: VecDeque::with_capacity(MAX_HIPRIO_QUEUED),
                reqs: VecDeque::with_capacity(MAX_REQ_QUEUED),
                closed: false,
            }),
            cv: Condvar::new(),
        }
    }

    fn try_push(&self, job: FsJob) -> Result<(), FsJob> {
        let mut q = self.mu.lock().unwrap();
        if q.closed {
            return Err(job);
        }
        if job.queue_index == 0 {
            if q.hiprio.len() >= MAX_HIPRIO_QUEUED {
                return Err(job);
            }
            q.hiprio.push_back(job);
        } else {
            if q.reqs.len() >= MAX_REQ_QUEUED {
                return Err(job);
            }
            q.reqs.push_back(job);
        }
        self.cv.notify_one();
        Ok(())
    }

    fn pop(&self) -> Option<FsJob> {
        let mut q = self.mu.lock().unwrap();
        loop {
            if let Some(job) = q.hiprio.pop_front() {
                return Some(job);
            }
            if let Some(job) = q.reqs.pop_front() {
                return Some(job);
            }
            if q.closed {
                return None;
            }
            q = self.cv.wait(q).unwrap();
        }
    }

    fn clear_pending(&self) {
        let (dropped_hi, dropped_req) = {
            let mut q = self.mu.lock().unwrap();
            (
                std::mem::take(&mut q.hiprio),
                std::mem::take(&mut q.reqs),
            )
        };
        drop(dropped_hi);
        drop(dropped_req);
    }

    fn close(&self) {
        let (dropped_hi, dropped_req) = {
            let mut q = self.mu.lock().unwrap();
            q.closed = true;
            let hi = std::mem::take(&mut q.hiprio);
            let req = std::mem::take(&mut q.reqs);
            self.cv.notify_all();
            (hi, req)
        };
        drop(dropped_hi);
        drop(dropped_req);
    }
}

// ---------- VirtioFs device ----------

pub struct VirtioFs {
    dev_config: [u8; DEV_CONFIG_LEN],
    shares: Vec<ShareState>,
    share_by_name: HashMap<String, usize>,
    limits: FsLimits,
    tables: RwLock<FsTables>,
    jobs: Arc<JobQueue>,
    inflight_bytes: Arc<AtomicUsize>,
    shutdown_flag: Arc<AtomicBool>,
    vpci: OnceLock<Weak<VirtioPciDevice>>,
    workers_started: AtomicBool,
    workers: Mutex<Vec<JoinHandle<()>>>,
}

impl VirtioFs {
    pub fn new(tag: &str, configs: &[ShareConfig]) -> Result<Arc<Self>, String> {
        Self::new_with_limits(tag, configs, FsLimits::from_current_rlimit())
    }

    pub fn new_with_limits(
        tag: &str,
        configs: &[ShareConfig],
        limits: FsLimits,
    ) -> Result<Arc<Self>, String> {
        if tag.is_empty() || tag.len() > TAG_LEN || tag.as_bytes().contains(&0) {
            return Err(format!("invalid virtio-fs tag '{tag}'"));
        }
        let mut dev_config = [0u8; DEV_CONFIG_LEN];
        dev_config[..tag.len()].copy_from_slice(tag.as_bytes());
        dev_config[36..40].copy_from_slice(&1u32.to_le_bytes());

        let mut shares = Vec::with_capacity(configs.len());
        let mut share_by_name = HashMap::with_capacity(configs.len());
        for (i, cfg) in configs.iter().enumerate() {
            validate_share_name(&cfg.name)?;
            if share_by_name.contains_key(&cfg.name) {
                return Err(format!("duplicate share name '{}'", cfg.name));
            }
            let (fd, st) = open_share_root(&cfg.path)?;
            let root_nodeid = FUSE_ROOT_ID + 1 + (i as u64);
            share_by_name.insert(cfg.name.clone(), i);
            shares.push(ShareState {
                name: cfg.name.clone(),
                read_only: cfg.read_only,
                root_nodeid,
                root_fd: Arc::new(fd),
                root_dev: st.st_dev as u64,
                root_ino: st.st_ino as u64,
            });
        }

        let tables = FsTables::new(&shares, limits);
        Ok(Arc::new(Self {
            dev_config,
            shares,
            share_by_name,
            limits,
            tables: RwLock::new(tables),
            jobs: Arc::new(JobQueue::new()),
            inflight_bytes: Arc::new(AtomicUsize::new(0)),
            shutdown_flag: Arc::new(AtomicBool::new(false)),
            vpci: OnceLock::new(),
            workers_started: AtomicBool::new(false),
            workers: Mutex::new(Vec::new()),
        }))
    }

    pub fn start_workers(
        self: &Arc<Self>,
        vpci: Arc<VirtioPciDevice>,
        _mem: Arc<GuestMemory>,
    ) -> Result<(), String> {
        let mut workers = self.workers.lock().unwrap();
        if !workers.is_empty() {
            return Err("VirtioFs workers already started".to_string());
        }
        let _ = self.vpci.set(Arc::downgrade(&vpci));
        for idx in 0..NUM_WORKERS {
            let fs_weak = Arc::downgrade(self);
            let jobs = Arc::clone(&self.jobs);
            let shutdown_flag = Arc::clone(&self.shutdown_flag);
            let vpci_weak = Arc::downgrade(&vpci);
            let handle = thread::Builder::new()
                .name(format!("virtiofs-{idx}"))
                .spawn(move || Self::worker_loop(fs_weak, jobs, shutdown_flag, vpci_weak))
                .map_err(|e| format!("spawn virtiofs worker {idx}: {e}"))?;
            workers.push(handle);
        }
        self.workers_started.store(true, Ordering::Release);
        Ok(())
    }

    pub fn shutdown(&self) {
        self.shutdown_flag.store(true, Ordering::Release);
        self.workers_started.store(false, Ordering::Release);
        self.jobs.close();
        let handles: Vec<JoinHandle<()>> = std::mem::take(&mut *self.workers.lock().unwrap());
        let me = thread::current().id();
        for h in handles {
            if h.thread().id() != me {
                let _ = h.join();
            }
        }
        let dropped = self.tables.write().unwrap().reset(&self.shares);
        drop(dropped);
    }

    fn worker_loop(
        fs_weak: Weak<Self>,
        jobs: Arc<JobQueue>,
        shutdown_flag: Arc<AtomicBool>,
        vpci_weak: Weak<VirtioPciDevice>,
    ) {
        while let Some(job) = jobs.pop() {
            if shutdown_flag.load(Ordering::Acquire) {
                break;
            }
            let Some(fs) = fs_weak.upgrade() else {
                break;
            };
            let Some(vpci) = vpci_weak.upgrade() else {
                break;
            };
            if vpci.reset_generation() != job.expected_gen {
                continue;
            }
            let reply = fs.dispatch_fuse_bytes(&job.in_buf, job.out_cap);
            if shutdown_flag.load(Ordering::Acquire) {
                break;
            }
            let payload = reply.as_deref().unwrap_or(&[]);
            vpci.complete_used_with_write(
                job.queue_index,
                job.head_index,
                job.expected_gen,
                &job.out_segs,
                payload,
            );
        }
    }

    fn reserve_budget(&self, bytes: usize) -> Option<BudgetGuard> {
        let reserved = bytes.max(1);
        loop {
            let cur = self.inflight_bytes.load(Ordering::Acquire);
            if cur.saturating_add(reserved) > MAX_INFLIGHT_BYTES {
                return None;
            }
            if self
                .inflight_bytes
                .compare_exchange_weak(cur, cur + reserved, Ordering::AcqRel, Ordering::Acquire)
                .is_ok()
            {
                return Some(BudgetGuard {
                    counter: Some(Arc::clone(&self.inflight_bytes)),
                    bytes: reserved,
                });
            }
        }
    }

    fn current_epoch(&self) -> u64 {
        self.tables.read().unwrap().session_epoch
    }

    fn insert_inode_for_epoch(
        &self,
        epoch: u64,
        share_idx: usize,
        parent_nodeid: u64,
        st: &libc::stat,
        fd: OwnedFd,
        bump_lookup: u64,
    ) -> Result<(u64, u64), i32> {
        let res = {
            let mut t = self.tables.write().unwrap();
            if t.session_epoch != epoch {
                Err((-LINUX_EIO, fd))
            } else {
                t.get_or_insert_inode(share_idx, parent_nodeid, st, fd, bump_lookup)
            }
        };
        match res {
            Ok((cid, gen, unused_fd)) => {
                drop(unused_fd);
                Ok((cid, gen))
            }
            Err((code, unused_fd)) => {
                drop(unused_fd);
                Err(code)
            }
        }
    }

    fn virtual_root_attr(&self) -> FuseAttr {
        FuseAttr {
            ino: FUSE_ROOT_ID,
            size: 0,
            blocks: 0,
            atime: 0,
            mtime: 0,
            ctime: 0,
            atimensec: 0,
            mtimensec: 0,
            ctimensec: 0,
            mode: (libc::S_IFDIR as u32) | 0o555,
            nlink: 2 + self.shares.len() as u32,
            uid: 0,
            gid: 0,
            rdev: 0,
            blksize: 4096,
        }
    }

    fn lookup_inode(&self, nodeid: u64) -> Result<InodeEntry, i32> {
        self.tables
            .read()
            .unwrap()
            .inodes
            .get(&nodeid)
            .cloned()
            .ok_or(-LINUX_ENOENT)
    }

    fn check_writable_share(&self, share_idx: usize) -> Result<(), i32> {
        if self.shares.get(share_idx).is_none_or(|s| s.read_only) {
            Err(-LINUX_EROFS)
        } else {
            Ok(())
        }
    }

    /// Executes one FUSE request buffer and returns `Some(reply_bytes)` or
    /// `None` for no-reply opcodes (`FUSE_FORGET`, `FUSE_BATCH_FORGET`).
    pub fn dispatch_fuse_bytes(&self, req: &[u8], out_cap: usize) -> Option<Vec<u8>> {
        if req.len() < FUSE_IN_HEADER_LEN {
            return if out_cap >= FUSE_OUT_HEADER_LEN {
                Some(make_error_reply(0, -LINUX_EINVAL))
            } else {
                None
            };
        }
        let hdr_len = read_u32_le(req, 0) as usize;
        let opcode = read_u32_le(req, 4);
        let unique = read_u64_le(req, 8);
        let nodeid = read_u64_le(req, 16);

        if hdr_len < FUSE_IN_HEADER_LEN || hdr_len > req.len() {
            return if opcode == FUSE_OP_FORGET || opcode == FUSE_OP_BATCH_FORGET {
                None
            } else if out_cap >= FUSE_OUT_HEADER_LEN {
                Some(make_error_reply(unique, -LINUX_EINVAL))
            } else {
                None
            };
        }
        let body = &req[FUSE_IN_HEADER_LEN..hdr_len];

        match opcode {
            FUSE_OP_FORGET => {
                self.op_forget(nodeid, body);
                return None;
            }
            FUSE_OP_BATCH_FORGET => {
                self.op_batch_forget(body);
                return None;
            }
            _ => {}
        }

        if out_cap < FUSE_OUT_HEADER_LEN {
            return None;
        }
        let max_payload = out_cap - FUSE_OUT_HEADER_LEN;
        let res = match opcode {
            FUSE_OP_INIT => self.op_init(unique, body),
            FUSE_OP_DESTROY => self.op_destroy(unique),
            FUSE_OP_LOOKUP => self.op_lookup(unique, nodeid, body),
            FUSE_OP_GETATTR => self.op_getattr(unique, nodeid, body),
            FUSE_OP_SETATTR => self.op_setattr(unique, nodeid, body),
            FUSE_OP_READLINK => self.op_readlink(unique, nodeid, max_payload),
            FUSE_OP_SYMLINK => self.op_symlink(unique, nodeid, body),
            FUSE_OP_MKNOD => self.op_mknod(unique, nodeid, body),
            FUSE_OP_MKDIR => self.op_mkdir(unique, nodeid, body),
            FUSE_OP_UNLINK => self.op_unlink(unique, nodeid, body, false),
            FUSE_OP_RMDIR => self.op_unlink(unique, nodeid, body, true),
            FUSE_OP_RENAME => self.op_rename(unique, nodeid, body, false),
            FUSE_OP_RENAME2 => self.op_rename(unique, nodeid, body, true),
            FUSE_OP_LINK => self.op_link(unique, nodeid, body),
            FUSE_OP_OPEN => self.op_open(unique, nodeid, body),
            FUSE_OP_CREATE => self.op_create(unique, nodeid, body),
            FUSE_OP_READ => self.op_read(unique, nodeid, body, max_payload),
            FUSE_OP_WRITE => self.op_write(unique, nodeid, body),
            FUSE_OP_STATFS => self.op_statfs(unique, nodeid),
            FUSE_OP_RELEASE => self.op_release(unique, nodeid, body),
            FUSE_OP_FLUSH => self.op_flush(unique, nodeid, body),
            FUSE_OP_FSYNC => self.op_fsync(unique, nodeid, body, false),
            FUSE_OP_FSYNCDIR => self.op_fsync(unique, nodeid, body, true),
            FUSE_OP_OPENDIR => self.op_opendir(unique, nodeid),
            FUSE_OP_READDIR => self.op_readdir(unique, nodeid, body, max_payload, false),
            FUSE_OP_READDIRPLUS => self.op_readdir(unique, nodeid, body, max_payload, true),
            FUSE_OP_RELEASEDIR => self.op_releasedir(unique, nodeid, body),
            FUSE_OP_GETXATTR => self.op_getxattr(unique, nodeid, body, max_payload),
            FUSE_OP_LISTXATTR => self.op_listxattr(unique, nodeid, body, max_payload),
            FUSE_OP_SETXATTR => self.op_setxattr(unique, nodeid, body),
            FUSE_OP_REMOVEXATTR => self.op_removexattr(unique, nodeid, body),
            FUSE_OP_ACCESS => self.op_access(unique, nodeid, body),
            FUSE_OP_FALLOCATE => self.op_fallocate(unique, nodeid, body),
            FUSE_OP_LSEEK => self.op_lseek(unique, nodeid, body),
            FUSE_OP_SYNCFS => self.op_syncfs(unique, nodeid),
            _ => Err(-LINUX_ENOSYS),
        };
        let reply = match res {
            Ok(bytes) if bytes.len() <= out_cap => bytes,
            Ok(_) => make_error_reply(unique, -LINUX_EIO),
            Err(code) => make_error_reply(unique, code),
        };
        Some(reply)
    }

    fn op_init(&self, unique: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if body.len() < 16 {
            return Err(-LINUX_EINVAL);
        }
        let major = read_u32_le(body, 0);
        let minor = read_u32_le(body, 4);
        let max_readahead = read_u32_le(body, 8);
        let flags = read_u32_le(body, 12);

        if major != FUSE_KERNEL_VERSION {
            let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_INIT_OUT_LEN];
            finish_ok(&mut out, unique);
            out[16..20].copy_from_slice(&FUSE_KERNEL_VERSION.to_le_bytes());
            out[20..24].copy_from_slice(&FUSE_KERNEL_MINOR_VERSION.to_le_bytes());
            return Ok(out);
        }

        let supported =
            FUSE_ASYNC_READ | FUSE_BIG_WRITES | FUSE_PARALLEL_DIROPS | FUSE_MAX_PAGES | FUSE_SETXATTR_EXT;
        let negotiated = flags & supported;
        {
            let mut t = self.tables.write().unwrap();
            t.session_flags = negotiated;
        }
        let ra = if max_readahead == 0 {
            MAX_IO_BYTES
        } else {
            max_readahead.min(MAX_IO_BYTES)
        };
        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_INIT_OUT_LEN];
        finish_ok(&mut out, unique);
        let p = &mut out[FUSE_OUT_HEADER_LEN..];
        p[0..4].copy_from_slice(&FUSE_KERNEL_VERSION.to_le_bytes());
        p[4..8].copy_from_slice(&minor.min(FUSE_KERNEL_MINOR_VERSION).to_le_bytes());
        p[8..12].copy_from_slice(&ra.to_le_bytes());
        p[12..16].copy_from_slice(&negotiated.to_le_bytes());
        p[16..18].copy_from_slice(&64u16.to_le_bytes()); // max_background
        p[18..20].copy_from_slice(&48u16.to_le_bytes()); // congestion_threshold
        p[20..24].copy_from_slice(&MAX_IO_BYTES.to_le_bytes()); // max_write
        p[24..28].copy_from_slice(&1u32.to_le_bytes()); // time_gran = 1 ns
        p[28..30].copy_from_slice(&32u16.to_le_bytes()); // max_pages = 32 (128 KiB)
        Ok(out)
    }

    fn op_destroy(&self, unique: u64) -> Result<Vec<u8>, i32> {
        self.jobs.clear_pending();
        let dropped = self.tables.write().unwrap().reset(&self.shares);
        drop(dropped);
        Ok(make_ok_empty(unique))
    }

    fn op_forget(&self, nodeid: u64, body: &[u8]) {
        if body.len() < 8 {
            return;
        }
        let nlookup = read_u64_le(body, 0);
        let dropped = self.tables.write().unwrap().forget_one(nodeid, nlookup);
        drop(dropped);
    }

    fn op_batch_forget(&self, body: &[u8]) {
        if body.len() < 8 {
            return;
        }
        let count = read_u32_le(body, 0) as usize;
        let max_items = (body.len() - 8) / 16;
        let n = count.min(max_items);
        let mut dropped = Vec::new();
        {
            let mut t = self.tables.write().unwrap();
            for i in 0..n {
                let off = 8 + i * 16;
                let nid = read_u64_le(body, off);
                let nlookup = read_u64_le(body, off + 8);
                if let Some(fd) = t.forget_one(nid, nlookup) {
                    dropped.push(fd);
                }
            }
        }
        drop(dropped);
    }

    fn op_lookup(&self, unique: u64, parent_id: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        let (name_bytes, _) = split_cstr(body)?;

        // Virtual root (nodeid = 1): exposes each configured share as a top-level dir.
        if parent_id == FUSE_ROOT_ID {
            if name_bytes == b"." || name_bytes == b".." {
                let attr = self.virtual_root_attr();
                let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_ENTRY_OUT_LEN];
                finish_ok(&mut out, unique);
                write_entry_out(&mut out[FUSE_OUT_HEADER_LEN..], FUSE_ROOT_ID, 1, &attr);
                return Ok(out);
            }
            let name = std::str::from_utf8(name_bytes).map_err(|_| -LINUX_ENOENT)?;
            let &share_idx = self.share_by_name.get(name).ok_or(-LINUX_ENOENT)?;
            let sh = &self.shares[share_idx];
            let st = fstat_fd(sh.root_fd.as_raw_fd())?;
            let attr = FuseAttr::from_stat(&st);
            let generation = {
                let mut t = self.tables.write().unwrap();
                let entry = t.inodes.get_mut(&sh.root_nodeid).ok_or(-LINUX_ENOENT)?;
                entry.nlookup = entry.nlookup.saturating_add(1);
                entry.generation
            };
            let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_ENTRY_OUT_LEN];
            finish_ok(&mut out, unique);
            write_entry_out(
                &mut out[FUSE_OUT_HEADER_LEN..],
                sh.root_nodeid,
                generation,
                &attr,
            );
            return Ok(out);
        }

        let epoch = self.current_epoch();
        let parent = self.lookup_inode(parent_id)?;
        if parent.file_type != (libc::S_IFDIR as u32) {
            return Err(-LINUX_ENOTDIR);
        }

        // Handle "." and ".." strictly in-memory without passing them to host FS.
        if name_bytes == b"." || name_bytes == b".." {
            let target_id = if name_bytes == b"." {
                parent.nodeid
            } else {
                parent.parent_nodeid
            };
            let target = self.lookup_inode(target_id)?;
            let st = fstat_fd(target.fd.as_raw_fd())?;
            let attr = FuseAttr::from_stat(&st);
            let generation = {
                let mut t = self.tables.write().unwrap();
                if t.session_epoch != epoch {
                    return Err(-LINUX_EIO);
                }
                let entry = t.inodes.get_mut(&target_id).ok_or(-LINUX_ENOENT)?;
                entry.nlookup = entry.nlookup.saturating_add(1);
                entry.generation
            };
            let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_ENTRY_OUT_LEN];
            finish_ok(&mut out, unique);
            write_entry_out(&mut out[FUSE_OUT_HEADER_LEN..], target_id, generation, &attr);
            return Ok(out);
        }

        let cname = validate_component(name_bytes)?;
        let child_fd = open_inode_handle(parent.fd.as_raw_fd(), &cname)?;
        let st = fstat_fd(child_fd.as_raw_fd())?;
        let attr = FuseAttr::from_stat(&st);
        let (child_id, generation) = self.insert_inode_for_epoch(
            epoch,
            parent.share_idx,
            parent.nodeid,
            &st,
            child_fd,
            1,
        )?;

        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_ENTRY_OUT_LEN];
        finish_ok(&mut out, unique);
        write_entry_out(&mut out[FUSE_OUT_HEADER_LEN..], child_id, generation, &attr);
        Ok(out)
    }

    fn op_getattr(&self, unique: u64, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if nodeid == FUSE_ROOT_ID {
            let attr = self.virtual_root_attr();
            let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_ATTR_OUT_LEN];
            finish_ok(&mut out, unique);
            write_attr_out(&mut out[FUSE_OUT_HEADER_LEN..], &attr);
            return Ok(out);
        }

        let fh_opt = if body.len() >= 16 && (read_u32_le(body, 0) & FUSE_GETATTR_FH) != 0 {
            Some(read_u64_le(body, 8))
        } else {
            None
        };

        let st = if let Some(fh) = fh_opt {
            let file_opt = self
                .tables
                .read()
                .unwrap()
                .files
                .get(&fh)
                .filter(|h| h.nodeid == nodeid)
                .map(|h| Arc::clone(&h.file));
            if let Some(f) = file_opt {
                fstat_fd(f.as_raw_fd())?
            } else {
                let inode = self.lookup_inode(nodeid)?;
                fstat_fd(inode.fd.as_raw_fd())?
            }
        } else {
            let inode = self.lookup_inode(nodeid)?;
            fstat_fd(inode.fd.as_raw_fd())?
        };
        let attr = FuseAttr::from_stat(&st);
        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_ATTR_OUT_LEN];
        finish_ok(&mut out, unique);
        write_attr_out(&mut out[FUSE_OUT_HEADER_LEN..], &attr);
        Ok(out)
    }

    fn op_setattr(&self, unique: u64, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if nodeid == FUSE_ROOT_ID {
            return Err(-LINUX_EROFS);
        }
        if body.len() < 88 {
            return Err(-LINUX_EINVAL);
        }
        let valid = read_u32_le(body, 0);
        let fh = read_u64_le(body, 8);
        let size = read_u64_le(body, 16);
        let atime = read_u64_le(body, 32);
        let mtime = read_u64_le(body, 40);
        let atimensec = read_u32_le(body, 56);
        let mtimensec = read_u32_le(body, 60);
        let mode = read_u32_le(body, 68);

        let inode = self.lookup_inode(nodeid)?;
        self.check_writable_share(inode.share_idx)?;

        let fh_file = if (valid & FATTR_FH) != 0 {
            let h = self
                .tables
                .read()
                .unwrap()
                .files
                .get(&fh)
                .filter(|h| h.nodeid == nodeid)
                .cloned()
                .ok_or(-LINUX_EBADF)?;
            Some(h)
        } else {
            None
        };
        let fh_raw = fh_file.as_ref().map(|h| h.file.as_raw_fd());

        if (valid & FATTR_MODE) != 0 {
            fchmod_inode(inode.fd.as_raw_fd(), inode.file_type, fh_raw, mode)?;
        }
        // FATTR_UID / FATTR_GID are intentionally ignored on host: VirtioFS.kext
        // presents the guest caller's own uid/gid in getattr, and ignoring host
        // chown prevents both privilege issues as root and EPERM failures as non-root.
        let _ = valid & (FATTR_UID | FATTR_GID);

        if (valid & FATTR_SIZE) != 0 {
            if inode.file_type == (libc::S_IFDIR as u32) {
                return Err(-LINUX_EISDIR);
            }
            let write_fh = fh_file
                .as_ref()
                .filter(|h| h.writable)
                .map(|h| h.file.as_raw_fd());
            ftruncate_inode(inode.fd.as_raw_fd(), inode.file_type, write_fh, size)?;
        }

        if (valid & (FATTR_ATIME | FATTR_MTIME | FATTR_ATIME_NOW | FATTR_MTIME_NOW)) != 0 {
            let ts_atime = if (valid & FATTR_ATIME_NOW) != 0 {
                libc::timespec {
                    tv_sec: 0,
                    tv_nsec: libc::UTIME_NOW,
                }
            } else if (valid & FATTR_ATIME) != 0 {
                if atimensec >= 1_000_000_000 {
                    return Err(-LINUX_EINVAL);
                }
                libc::timespec {
                    tv_sec: atime as i64,
                    tv_nsec: atimensec as libc::c_long,
                }
            } else {
                libc::timespec {
                    tv_sec: 0,
                    tv_nsec: libc::UTIME_OMIT,
                }
            };
            let ts_mtime = if (valid & FATTR_MTIME_NOW) != 0 {
                libc::timespec {
                    tv_sec: 0,
                    tv_nsec: libc::UTIME_NOW,
                }
            } else if (valid & FATTR_MTIME) != 0 {
                if mtimensec >= 1_000_000_000 {
                    return Err(-LINUX_EINVAL);
                }
                libc::timespec {
                    tv_sec: mtime as i64,
                    tv_nsec: mtimensec as libc::c_long,
                }
            } else {
                libc::timespec {
                    tv_sec: 0,
                    tv_nsec: libc::UTIME_OMIT,
                }
            };
            utimens_inode(
                inode.fd.as_raw_fd(),
                inode.file_type,
                fh_raw,
                &[ts_atime, ts_mtime],
            )?;
        }

        let st = match fh_raw {
            Some(fd) => fstat_fd(fd)?,
            None => fstat_fd(inode.fd.as_raw_fd())?,
        };
        let attr = FuseAttr::from_stat(&st);
        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_ATTR_OUT_LEN];
        finish_ok(&mut out, unique);
        write_attr_out(&mut out[FUSE_OUT_HEADER_LEN..], &attr);
        Ok(out)
    }

    fn op_readlink(&self, unique: u64, nodeid: u64, max_payload: usize) -> Result<Vec<u8>, i32> {
        if nodeid == FUSE_ROOT_ID {
            return Err(-LINUX_EINVAL);
        }
        let inode = self.lookup_inode(nodeid)?;
        let target = readlink_pinned(inode.fd.as_raw_fd(), inode.file_type)?;
        if target.len() > max_payload {
            return Err(-LINUX_ENAMETOOLONG);
        }
        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + target.len()];
        finish_ok(&mut out, unique);
        out[FUSE_OUT_HEADER_LEN..].copy_from_slice(&target);
        Ok(out)
    }

    fn op_symlink(&self, unique: u64, parent_id: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if parent_id == FUSE_ROOT_ID {
            return Err(-LINUX_EROFS);
        }
        let epoch = self.current_epoch();
        let parent = self.lookup_inode(parent_id)?;
        self.check_writable_share(parent.share_idx)?;
        if parent.file_type != (libc::S_IFDIR as u32) {
            return Err(-LINUX_ENOTDIR);
        }
        let (name_bytes, rest) = split_cstr(body)?;
        let (target_bytes, _) = split_cstr(rest)?;
        if target_bytes.is_empty() || target_bytes.len() >= MAX_SYMLINK_LEN {
            return Err(if target_bytes.is_empty() {
                -LINUX_ENOENT
            } else {
                -LINUX_ENAMETOOLONG
            });
        }
        let cname = validate_component(name_bytes)?;
        let ctarget = CString::new(target_bytes).map_err(|_| -LINUX_EINVAL)?;
        let rc = unsafe { libc::symlinkat(ctarget.as_ptr(), parent.fd.as_raw_fd(), cname.as_ptr()) };
        if rc != 0 {
            return Err(last_fuse_error());
        }
        let child_fd = open_inode_handle(parent.fd.as_raw_fd(), &cname)?;
        let st = fstat_fd(child_fd.as_raw_fd())?;
        if (st.st_mode & libc::S_IFMT) != libc::S_IFLNK {
            return Err(-LINUX_ESTALE);
        }
        let attr = FuseAttr::from_stat(&st);
        let (child_id, generation) = self.insert_inode_for_epoch(
            epoch,
            parent.share_idx,
            parent.nodeid,
            &st,
            child_fd,
            1,
        )?;

        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_ENTRY_OUT_LEN];
        finish_ok(&mut out, unique);
        write_entry_out(&mut out[FUSE_OUT_HEADER_LEN..], child_id, generation, &attr);
        Ok(out)
    }

    fn op_mknod(&self, unique: u64, parent_id: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if parent_id == FUSE_ROOT_ID {
            return Err(-LINUX_EROFS);
        }
        if body.len() < 16 {
            return Err(-LINUX_EINVAL);
        }
        let epoch = self.current_epoch();
        let parent = self.lookup_inode(parent_id)?;
        self.check_writable_share(parent.share_idx)?;
        if parent.file_type != (libc::S_IFDIR as u32) {
            return Err(-LINUX_ENOTDIR);
        }
        let raw_mode = read_u32_le(body, 0);
        let umask = read_u32_le(body, 8);
        let (name_bytes, _) = split_cstr(&body[16..])?;
        let cname = validate_component(name_bytes)?;
        let ftype = (raw_mode & (libc::S_IFMT as u32)) as libc::mode_t;
        let perms = ((raw_mode & 0o1777) & !umask) as libc::mode_t;

        let (expected_ftype, child_fd) = match ftype {
            libc::S_IFREG | 0 => {
                let created_fd = openat_confined(
                    parent.fd.as_raw_fd(),
                    &cname,
                    libc::O_WRONLY | libc::O_CREAT | libc::O_EXCL,
                    perms,
                )?;
                #[cfg(target_os = "linux")]
                let pinned = {
                    let proc_path = proc_self_fd_cstr(created_fd.as_raw_fd());
                    let rc =
                        unsafe { libc::open(proc_path.as_ptr(), libc::O_PATH | libc::O_CLOEXEC) };
                    if rc >= 0 {
                        unsafe { OwnedFd::from_raw_fd(rc) }
                    } else {
                        open_inode_handle(parent.fd.as_raw_fd(), &cname)?
                    }
                };
                #[cfg(not(target_os = "linux"))]
                let pinned = open_inode_handle(parent.fd.as_raw_fd(), &cname)?;
                drop(created_fd);
                (libc::S_IFREG, pinned)
            }
            libc::S_IFIFO => {
                let rc = unsafe { libc::mkfifoat(parent.fd.as_raw_fd(), cname.as_ptr(), perms) };
                if rc != 0 {
                    return Err(last_fuse_error());
                }
                (libc::S_IFIFO, open_inode_handle(parent.fd.as_raw_fd(), &cname)?)
            }
            #[cfg(target_os = "linux")]
            libc::S_IFSOCK => {
                let rc = unsafe {
                    libc::mknodat(parent.fd.as_raw_fd(), cname.as_ptr(), libc::S_IFSOCK | perms, 0)
                };
                if rc != 0 {
                    return Err(last_fuse_error());
                }
                (libc::S_IFSOCK, open_inode_handle(parent.fd.as_raw_fd(), &cname)?)
            }
            _ => return Err(-LINUX_EPERM),
        };

        let st = fstat_fd(child_fd.as_raw_fd())?;
        if (st.st_mode & libc::S_IFMT) != expected_ftype {
            return Err(-LINUX_ESTALE);
        }
        let attr = FuseAttr::from_stat(&st);
        let (child_id, generation) = self.insert_inode_for_epoch(
            epoch,
            parent.share_idx,
            parent.nodeid,
            &st,
            child_fd,
            1,
        )?;

        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_ENTRY_OUT_LEN];
        finish_ok(&mut out, unique);
        write_entry_out(&mut out[FUSE_OUT_HEADER_LEN..], child_id, generation, &attr);
        Ok(out)
    }

    fn op_mkdir(&self, unique: u64, parent_id: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if parent_id == FUSE_ROOT_ID {
            return Err(-LINUX_EROFS);
        }
        if body.len() < 8 {
            return Err(-LINUX_EINVAL);
        }
        let epoch = self.current_epoch();
        let parent = self.lookup_inode(parent_id)?;
        self.check_writable_share(parent.share_idx)?;
        if parent.file_type != (libc::S_IFDIR as u32) {
            return Err(-LINUX_ENOTDIR);
        }
        let raw_mode = read_u32_le(body, 0);
        let umask = read_u32_le(body, 4);
        let perms = ((raw_mode & 0o1777) & !umask) as libc::mode_t;
        let (name_bytes, _) = split_cstr(&body[8..])?;
        let cname = validate_component(name_bytes)?;

        let rc = unsafe { libc::mkdirat(parent.fd.as_raw_fd(), cname.as_ptr(), perms) };
        if rc != 0 {
            return Err(last_fuse_error());
        }
        let child_fd = open_inode_handle(parent.fd.as_raw_fd(), &cname)?;
        let st = fstat_fd(child_fd.as_raw_fd())?;
        if (st.st_mode & libc::S_IFMT) != libc::S_IFDIR {
            return Err(-LINUX_ESTALE);
        }
        let attr = FuseAttr::from_stat(&st);
        let (child_id, generation) = self.insert_inode_for_epoch(
            epoch,
            parent.share_idx,
            parent.nodeid,
            &st,
            child_fd,
            1,
        )?;

        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_ENTRY_OUT_LEN];
        finish_ok(&mut out, unique);
        write_entry_out(&mut out[FUSE_OUT_HEADER_LEN..], child_id, generation, &attr);
        Ok(out)
    }

    fn op_unlink(
        &self,
        unique: u64,
        parent_id: u64,
        body: &[u8],
        rmdir: bool,
    ) -> Result<Vec<u8>, i32> {
        if parent_id == FUSE_ROOT_ID {
            return Err(-LINUX_EROFS);
        }
        let parent = self.lookup_inode(parent_id)?;
        self.check_writable_share(parent.share_idx)?;
        if parent.file_type != (libc::S_IFDIR as u32) {
            return Err(-LINUX_ENOTDIR);
        }
        let (name_bytes, _) = split_cstr(body)?;
        let cname = validate_component(name_bytes)?;
        let flags = if rmdir { libc::AT_REMOVEDIR } else { 0 };
        let rc = unsafe { libc::unlinkat(parent.fd.as_raw_fd(), cname.as_ptr(), flags) };
        if rc != 0 {
            return Err(last_fuse_error());
        }
        Ok(make_ok_empty(unique))
    }

    fn op_rename(
        &self,
        unique: u64,
        old_parent_id: u64,
        body: &[u8],
        v2: bool,
    ) -> Result<Vec<u8>, i32> {
        if old_parent_id == FUSE_ROOT_ID {
            return Err(-LINUX_EROFS);
        }
        let hdr_sz = if v2 { 16 } else { 8 };
        if body.len() < hdr_sz {
            return Err(-LINUX_EINVAL);
        }
        let new_parent_id = read_u64_le(body, 0);
        let flags = if v2 { read_u32_le(body, 8) } else { 0 };
        if new_parent_id == FUSE_ROOT_ID {
            return Err(-LINUX_EROFS);
        }
        if (flags & !(FUSE_RENAME_NOREPLACE | FUSE_RENAME_EXCHANGE)) != 0 {
            return Err(-LINUX_EINVAL);
        }
        if (flags & FUSE_RENAME_NOREPLACE) != 0 && (flags & FUSE_RENAME_EXCHANGE) != 0 {
            return Err(-LINUX_EINVAL);
        }
        let epoch = self.current_epoch();
        let old_parent = self.lookup_inode(old_parent_id)?;
        let new_parent = self.lookup_inode(new_parent_id)?;
        if old_parent.share_idx != new_parent.share_idx {
            return Err(-LINUX_EXDEV);
        }
        self.check_writable_share(old_parent.share_idx)?;
        if old_parent.file_type != (libc::S_IFDIR as u32)
            || new_parent.file_type != (libc::S_IFDIR as u32)
        {
            return Err(-LINUX_ENOTDIR);
        }
        let (old_bytes, rest) = split_cstr(&body[hdr_sz..])?;
        let (new_bytes, _) = split_cstr(rest)?;
        let cold = validate_component(old_bytes)?;
        let cnew = validate_component(new_bytes)?;

        if flags == 0 {
            let rc = unsafe {
                libc::renameat(
                    old_parent.fd.as_raw_fd(),
                    cold.as_ptr(),
                    new_parent.fd.as_raw_fd(),
                    cnew.as_ptr(),
                )
            };
            if rc != 0 {
                return Err(last_fuse_error());
            }
        } else {
            #[cfg(target_os = "linux")]
            {
                let rc = unsafe {
                    libc::syscall(
                        libc::SYS_renameat2,
                        old_parent.fd.as_raw_fd(),
                        cold.as_ptr(),
                        new_parent.fd.as_raw_fd(),
                        cnew.as_ptr(),
                        flags,
                    )
                };
                if rc != 0 {
                    return Err(last_fuse_error());
                }
            }
            #[cfg(target_os = "macos")]
            {
                let mut mflags = 0u32;
                if (flags & FUSE_RENAME_NOREPLACE) != 0 {
                    mflags |= libc::RENAME_EXCL as u32;
                }
                if (flags & FUSE_RENAME_EXCHANGE) != 0 {
                    mflags |= libc::RENAME_SWAP as u32;
                }
                let rc = unsafe {
                    libc::renameatx_np(
                        old_parent.fd.as_raw_fd(),
                        cold.as_ptr(),
                        new_parent.fd.as_raw_fd(),
                        cnew.as_ptr(),
                        mflags,
                    )
                };
                if rc != 0 {
                    return Err(last_fuse_error());
                }
            }
            #[cfg(not(any(target_os = "linux", target_os = "macos")))]
            {
                return Err(-LINUX_ENOSYS);
            }
        }

        // Best-effort refresh of tracked parent_nodeid for moved directory/file
        // (and both sides when RENAME_EXCHANGE swapped entries across directories).
        let refresh_parent = |parent: &InodeEntry, cname: &CStr| {
            if let Ok(moved_fd) = open_inode_handle(parent.fd.as_raw_fd(), cname) {
                if let Ok(st) = fstat_fd(moved_fd.as_raw_fd()) {
                    let key = (parent.share_idx, st.st_dev as u64, st.st_ino as u64);
                    let mut t = self.tables.write().unwrap();
                    if t.session_epoch == epoch {
                        if let Some(&nid) = t.inode_by_key.get(&key) {
                            if let Some(ent) = t.inodes.get_mut(&nid) {
                                if !ent.is_share_root {
                                    ent.parent_nodeid = parent.nodeid;
                                }
                            }
                        }
                    }
                }
            }
        };
        refresh_parent(&new_parent, &cnew);
        if (flags & FUSE_RENAME_EXCHANGE) != 0 {
            refresh_parent(&old_parent, &cold);
        }
        Ok(make_ok_empty(unique))
    }

    fn op_link(&self, unique: u64, new_parent_id: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if new_parent_id == FUSE_ROOT_ID {
            return Err(-LINUX_EROFS);
        }
        if body.len() < 8 {
            return Err(-LINUX_EINVAL);
        }
        let old_nodeid = read_u64_le(body, 0);
        if old_nodeid == FUSE_ROOT_ID {
            return Err(-LINUX_EPERM);
        }
        let epoch = self.current_epoch();
        let old_inode = self.lookup_inode(old_nodeid)?;
        let new_parent = self.lookup_inode(new_parent_id)?;
        if old_inode.share_idx != new_parent.share_idx {
            return Err(-LINUX_EXDEV);
        }
        self.check_writable_share(new_parent.share_idx)?;
        if new_parent.file_type != (libc::S_IFDIR as u32) {
            return Err(-LINUX_ENOTDIR);
        }
        let (name_bytes, _) = split_cstr(&body[8..])?;
        let cname = validate_component(name_bytes)?;

        link_inode_into(
            old_inode.fd.as_raw_fd(),
            old_inode.file_type,
            new_parent.fd.as_raw_fd(),
            &cname,
        )?;
        // Stat the already-pinned source inode directly (no reopen TOCTOU) and
        // bump its lookup count under the matching session epoch.
        let st = fstat_fd(old_inode.fd.as_raw_fd())?;
        let attr = FuseAttr::from_stat(&st);
        let (child_id, generation) = {
            let mut t = self.tables.write().unwrap();
            if t.session_epoch != epoch {
                return Err(-LINUX_EIO);
            }
            let entry = t.inodes.get_mut(&old_inode.nodeid).ok_or(-LINUX_ENOENT)?;
            entry.nlookup = entry.nlookup.saturating_add(1);
            (entry.nodeid, entry.generation)
        };

        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_ENTRY_OUT_LEN];
        finish_ok(&mut out, unique);
        write_entry_out(&mut out[FUSE_OUT_HEADER_LEN..], child_id, generation, &attr);
        Ok(out)
    }

    fn op_open(&self, unique: u64, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if nodeid == FUSE_ROOT_ID {
            return Err(-LINUX_EISDIR);
        }
        if body.len() < 8 {
            return Err(-LINUX_EINVAL);
        }
        let epoch = self.current_epoch();
        let fuse_flags = read_u32_le(body, 0);
        let inode = self.lookup_inode(nodeid)?;
        let acc = fuse_flags & FUSE_O_ACCMODE;
        let is_writable_mode = acc == FUSE_O_WRONLY || acc == FUSE_O_RDWR;
        let wants_write =
            is_writable_mode || (fuse_flags & (FUSE_O_CREAT | FUSE_O_TRUNC | FUSE_O_APPEND)) != 0;
        if wants_write {
            self.check_writable_share(inode.share_idx)?;
        }
        if (fuse_flags & FUSE_O_TRUNC) != 0 && !is_writable_mode {
            return Err(-LINUX_EINVAL);
        }

        let mut host_flags = match acc {
            FUSE_O_WRONLY => libc::O_WRONLY,
            FUSE_O_RDWR => libc::O_RDWR,
            _ => libc::O_RDONLY,
        };
        if (fuse_flags & FUSE_O_TRUNC) != 0 {
            host_flags |= libc::O_TRUNC;
        }
        // Note: VirtioFS.kext passes explicit offsets to FUSE_WRITE even for
        // O_APPEND files, so we do NOT pass O_APPEND to host open (which would
        // cause pwrite to ignore offset on Linux!).
        let fd = reopen_inode_fd(inode.fd.as_raw_fd(), inode.file_type, host_flags, false)?;
        let file = Arc::new(File::from(fd));

        let fh = {
            let mut t = self.tables.write().unwrap();
            if t.session_epoch != epoch {
                drop(t);
                drop(file);
                return Err(-LINUX_EIO);
            }
            if t.files.len() >= self.limits.max_open_files {
                drop(t);
                drop(file);
                return Err(-LINUX_EMFILE);
            }
            let fh = t.next_fh;
            t.next_fh = t.next_fh.wrapping_add(1).max(1);
            t.files.insert(
                fh,
                FileHandle {
                    nodeid,
                    share_idx: inode.share_idx,
                    writable: is_writable_mode,
                    file,
                },
            );
            fh
        };

        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_OPEN_OUT_LEN];
        finish_ok(&mut out, unique);
        write_open_out(&mut out[FUSE_OUT_HEADER_LEN..], fh);
        Ok(out)
    }

    fn op_create(&self, unique: u64, parent_id: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if parent_id == FUSE_ROOT_ID {
            return Err(-LINUX_EROFS);
        }
        if body.len() < 16 {
            return Err(-LINUX_EINVAL);
        }
        let epoch = self.current_epoch();
        let parent = self.lookup_inode(parent_id)?;
        self.check_writable_share(parent.share_idx)?;
        if parent.file_type != (libc::S_IFDIR as u32) {
            return Err(-LINUX_ENOTDIR);
        }
        let fuse_flags = read_u32_le(body, 0);
        let raw_mode = read_u32_le(body, 4);
        let umask = read_u32_le(body, 8);
        let (name_bytes, _) = split_cstr(&body[16..])?;
        let cname = validate_component(name_bytes)?;

        let perms = ((raw_mode & 0o1777) & !umask) as libc::mode_t;
        let acc = fuse_flags & FUSE_O_ACCMODE;
        let is_writable_mode = acc == FUSE_O_WRONLY || acc == FUSE_O_RDWR;
        let want_excl = (fuse_flags & FUSE_O_EXCL) != 0;
        let want_trunc = (fuse_flags & FUSE_O_TRUNC) != 0;
        if want_trunc && !is_writable_mode {
            return Err(-LINUX_EINVAL);
        }
        let base_acc_flags = match acc {
            FUSE_O_WRONLY => libc::O_WRONLY,
            FUSE_O_RDWR => libc::O_RDWR,
            _ => libc::O_RDONLY,
        };

        // Check handle quota before creating file on disk.
        if self.tables.read().unwrap().files.len() >= self.limits.max_open_files {
            return Err(-LINUX_EMFILE);
        }

        // Always attempt atomic O_CREAT | O_EXCL first so an existing FIFO or
        // device node is never opened directly (preventing worker deadlock and
        // host device-driver open() side effects). When O_EXCL was not requested
        // and the entry already exists, inspect via O_PATH | O_NOFOLLOW first
        // and only reopen verified regular files.
        let mut attempt = 0u32;
        let (open_fd, pinned_fd, st) = loop {
            match openat_confined(
                parent.fd.as_raw_fd(),
                &cname,
                base_acc_flags | libc::O_CREAT | libc::O_EXCL,
                perms,
            ) {
                Ok(fd) => {
                    let st = fstat_fd(fd.as_raw_fd())?;
                    if (st.st_mode & libc::S_IFMT) != libc::S_IFREG {
                        return Err(-LINUX_EIO);
                    }
                    #[cfg(target_os = "linux")]
                    let pinned = {
                        let proc_path = proc_self_fd_cstr(fd.as_raw_fd());
                        let rc = unsafe {
                            libc::open(proc_path.as_ptr(), libc::O_PATH | libc::O_CLOEXEC)
                        };
                        if rc >= 0 {
                            unsafe { OwnedFd::from_raw_fd(rc) }
                        } else {
                            open_inode_handle(parent.fd.as_raw_fd(), &cname)?
                        }
                    };
                    #[cfg(not(target_os = "linux"))]
                    let pinned = open_inode_handle(parent.fd.as_raw_fd(), &cname)?;
                    break (fd, pinned, st);
                }
                Err(e) if e == -LINUX_EEXIST && !want_excl => {
                    let pinned = match open_inode_handle(parent.fd.as_raw_fd(), &cname) {
                        Ok(p) => p,
                        Err(e2) if e2 == -LINUX_ENOENT && attempt < 4 => {
                            attempt += 1;
                            continue;
                        }
                        Err(e2) => return Err(e2),
                    };
                    let st = fstat_fd(pinned.as_raw_fd())?;
                    let ftype = st.st_mode & libc::S_IFMT;
                    if ftype != libc::S_IFREG {
                        return Err(if ftype == libc::S_IFDIR {
                            -LINUX_EISDIR
                        } else if ftype == libc::S_IFLNK {
                            -LINUX_ELOOP
                        } else {
                            -LINUX_ENXIO
                        });
                    }
                    let mut reopen_flags = base_acc_flags;
                    if want_trunc {
                        reopen_flags |= libc::O_TRUNC;
                    }
                    let fd = reopen_inode_fd(
                        pinned.as_raw_fd(),
                        libc::S_IFREG as u32,
                        reopen_flags,
                        false,
                    )?;
                    let st_after = fstat_fd(fd.as_raw_fd())?;
                    break (fd, pinned, st_after);
                }
                Err(e) => return Err(e),
            }
        };

        let attr = FuseAttr::from_stat(&st);
        let file = Arc::new(File::from(open_fd));

        let (child_id, generation, fh, unused_fd) = {
            let mut t = self.tables.write().unwrap();
            if t.session_epoch != epoch {
                drop(t);
                drop(pinned_fd);
                drop(file);
                return Err(-LINUX_EIO);
            }
            if t.files.len() >= self.limits.max_open_files {
                drop(t);
                drop(pinned_fd);
                drop(file);
                return Err(-LINUX_EMFILE);
            }
            let (cid, gen, unused) = match t.get_or_insert_inode(
                parent.share_idx,
                parent.nodeid,
                &st,
                pinned_fd,
                1,
            ) {
                Ok(v) => v,
                Err((code, fd)) => {
                    drop(t);
                    drop(fd);
                    drop(file);
                    return Err(code);
                }
            };
            let fh = t.next_fh;
            t.next_fh = t.next_fh.wrapping_add(1).max(1);
            t.files.insert(
                fh,
                FileHandle {
                    nodeid: cid,
                    share_idx: parent.share_idx,
                    writable: is_writable_mode,
                    file,
                },
            );
            (cid, gen, fh, unused)
        };
        drop(unused_fd);

        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_ENTRY_OUT_LEN + FUSE_OPEN_OUT_LEN];
        finish_ok(&mut out, unique);
        write_entry_out(
            &mut out[FUSE_OUT_HEADER_LEN..FUSE_OUT_HEADER_LEN + FUSE_ENTRY_OUT_LEN],
            child_id,
            generation,
            &attr,
        );
        write_open_out(
            &mut out[FUSE_OUT_HEADER_LEN + FUSE_ENTRY_OUT_LEN..],
            fh,
        );
        Ok(out)
    }

    fn op_read(
        &self,
        unique: u64,
        nodeid: u64,
        body: &[u8],
        max_payload: usize,
    ) -> Result<Vec<u8>, i32> {
        if body.len() < 24 {
            return Err(-LINUX_EINVAL);
        }
        let fh = read_u64_le(body, 0);
        let offset = read_u64_le(body, 8);
        let size = read_u32_le(body, 16) as usize;
        if size > MAX_IO_BYTES as usize || size > max_payload {
            return Err(-LINUX_EINVAL);
        }
        if offset
            .checked_add(size as u64)
            .is_none_or(|end| end > VIRTIOFS_SIZE_MAX)
        {
            return Err(-LINUX_EINVAL);
        }
        let off_t = libc::off_t::try_from(offset).map_err(|_| -LINUX_EINVAL)?;
        let file = {
            let t = self.tables.read().unwrap();
            let h = t.files.get(&fh).ok_or(-LINUX_EBADF)?;
            if h.nodeid != nodeid {
                return Err(-LINUX_EBADF);
            }
            Arc::clone(&h.file)
        };

        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + size];
        let mut got = 0usize;
        while got < size {
            let n = unsafe {
                libc::pread(
                    file.as_raw_fd(),
                    out.as_mut_ptr()
                        .add(FUSE_OUT_HEADER_LEN + got)
                        .cast::<libc::c_void>(),
                    size - got,
                    off_t + got as libc::off_t,
                )
            };
            if n < 0 {
                let err = io::Error::last_os_error()
                    .raw_os_error()
                    .unwrap_or(libc::EIO);
                if err == libc::EINTR {
                    continue;
                }
                return Err(host_errno_to_fuse_error(err));
            }
            if n == 0 {
                break;
            }
            got += n as usize;
        }
        out.truncate(FUSE_OUT_HEADER_LEN + got);
        finish_ok(&mut out, unique);
        Ok(out)
    }

    fn op_write(&self, unique: u64, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        // Support full 40-byte fuse_write_in (VirtioFS.kext) and 24-byte compat.
        if body.len() < 24 {
            return Err(-LINUX_EINVAL);
        }
        let fh = read_u64_le(body, 0);
        let offset = read_u64_le(body, 8);
        let size = read_u32_le(body, 16) as usize;
        if size > MAX_IO_BYTES as usize {
            return Err(-LINUX_EINVAL);
        }
        let data_off = if 40usize.checked_add(size).is_some_and(|need| body.len() >= need) {
            40
        } else if 24usize.checked_add(size) == Some(body.len()) {
            24
        } else {
            return Err(-LINUX_EINVAL);
        };
        let off_t = libc::off_t::try_from(offset).map_err(|_| -LINUX_EFBIG)?;
        if offset.saturating_add(size as u64) > VIRTIOFS_SIZE_MAX {
            return Err(-LINUX_EFBIG);
        }

        let (file, share_idx, writable) = {
            let t = self.tables.read().unwrap();
            let h = t.files.get(&fh).ok_or(-LINUX_EBADF)?;
            if h.nodeid != nodeid {
                return Err(-LINUX_EBADF);
            }
            (Arc::clone(&h.file), h.share_idx, h.writable)
        };
        self.check_writable_share(share_idx)?;
        if !writable {
            return Err(-LINUX_EBADF);
        }

        let data = &body[data_off..data_off + size];
        let mut written = 0usize;
        while written < data.len() {
            let n = unsafe {
                libc::pwrite(
                    file.as_raw_fd(),
                    data.as_ptr().add(written).cast::<libc::c_void>(),
                    data.len() - written,
                    off_t + written as libc::off_t,
                )
            };
            if n < 0 {
                let err = io::Error::last_os_error()
                    .raw_os_error()
                    .unwrap_or(libc::EIO);
                if err == libc::EINTR {
                    continue;
                }
                if written > 0 {
                    break;
                }
                return Err(host_errno_to_fuse_error(err));
            }
            if n == 0 {
                break;
            }
            written += n as usize;
        }

        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_WRITE_OUT_LEN];
        finish_ok(&mut out, unique);
        out[16..20].copy_from_slice(&(written as u32).to_le_bytes());
        Ok(out)
    }

    fn op_statfs(&self, unique: u64, nodeid: u64) -> Result<Vec<u8>, i32> {
        let fd = if nodeid == FUSE_ROOT_ID {
            self.shares.first().map(|s| Arc::clone(&s.root_fd))
        } else {
            let inode = self.lookup_inode(nodeid)?;
            self.shares
                .get(inode.share_idx)
                .map(|s| Arc::clone(&s.root_fd))
        };

        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_STATFS_OUT_LEN];
        finish_ok(&mut out, unique);
        let p = &mut out[FUSE_OUT_HEADER_LEN..];
        if let Some(root_fd) = fd {
            let sv = statfs_fd(root_fd.as_raw_fd())?;
            let bsize = (sv.f_bsize as u64).clamp(512, 1 << 20) as u32;
            let frsize = if sv.f_frsize > 0 {
                (sv.f_frsize as u64).clamp(512, 1 << 20) as u32
            } else {
                bsize
            };
            let namelen = if sv.f_namemax > 0 {
                (sv.f_namemax as u64).min(MAX_NAME_LEN as u64) as u32
            } else {
                MAX_NAME_LEN as u32
            };
            p[0..8].copy_from_slice(&(sv.f_blocks as u64).to_le_bytes());
            p[8..16].copy_from_slice(&(sv.f_bfree as u64).to_le_bytes());
            p[16..24].copy_from_slice(&(sv.f_bavail as u64).to_le_bytes());
            p[24..32].copy_from_slice(&(sv.f_files as u64).to_le_bytes());
            p[32..40].copy_from_slice(&(sv.f_ffree as u64).to_le_bytes());
            p[40..44].copy_from_slice(&bsize.to_le_bytes());
            p[44..48].copy_from_slice(&namelen.to_le_bytes());
            p[48..52].copy_from_slice(&frsize.to_le_bytes());
        } else {
            p[40..44].copy_from_slice(&4096u32.to_le_bytes());
            p[44..48].copy_from_slice(&(MAX_NAME_LEN as u32).to_le_bytes());
            p[48..52].copy_from_slice(&4096u32.to_le_bytes());
        }
        Ok(out)
    }

    fn op_release(&self, unique: u64, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if body.len() < 8 {
            return Err(-LINUX_EINVAL);
        }
        let fh = read_u64_le(body, 0);
        let removed = {
            let mut t = self.tables.write().unwrap();
            if t.files.get(&fh).is_some_and(|h| h.nodeid == nodeid) {
                t.files.remove(&fh)
            } else {
                None
            }
        };
        drop(removed);
        Ok(make_ok_empty(unique))
    }

    fn op_flush(&self, unique: u64, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if body.len() < 8 {
            return Err(-LINUX_EINVAL);
        }
        let fh = read_u64_le(body, 0);
        let exists = self
            .tables
            .read()
            .unwrap()
            .files
            .get(&fh)
            .is_some_and(|h| h.nodeid == nodeid);
        if !exists {
            return Err(-LINUX_EBADF);
        }
        Ok(make_ok_empty(unique))
    }

    fn op_fsync(
        &self,
        unique: u64,
        nodeid: u64,
        body: &[u8],
        is_dir: bool,
    ) -> Result<Vec<u8>, i32> {
        if body.len() < 12 {
            return Err(-LINUX_EINVAL);
        }
        let fh = read_u64_le(body, 0);
        let flags = read_u32_le(body, 8);
        let data_only = (flags & FUSE_FSYNC_FDATASYNC) != 0;

        if is_dir {
            {
                let t = self.tables.read().unwrap();
                let h = t.dirs.get(&fh).ok_or(-LINUX_EBADF)?;
                if h.nodeid != nodeid {
                    return Err(-LINUX_EBADF);
                }
            }
            if nodeid == FUSE_ROOT_ID {
                return Ok(make_ok_empty(unique));
            }
            let inode = self.lookup_inode(nodeid)?;
            let dir_fd = reopen_inode_fd(inode.fd.as_raw_fd(), inode.file_type, libc::O_RDONLY, true)?;
            loop {
                let rc = unsafe { libc::fsync(dir_fd.as_raw_fd()) };
                if rc == 0 {
                    return Ok(make_ok_empty(unique));
                }
                let err = io::Error::last_os_error()
                    .raw_os_error()
                    .unwrap_or(libc::EIO);
                if err == libc::EINTR {
                    continue;
                }
                return Err(host_errno_to_fuse_error(err));
            }
        }

        let file = {
            let t = self.tables.read().unwrap();
            let h = t.files.get(&fh).ok_or(-LINUX_EBADF)?;
            if h.nodeid != nodeid {
                return Err(-LINUX_EBADF);
            }
            Arc::clone(&h.file)
        };
        loop {
            let res = if data_only {
                file.sync_data()
            } else {
                file.sync_all()
            };
            match res {
                Ok(()) => return Ok(make_ok_empty(unique)),
                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                Err(e) => {
                    return Err(host_errno_to_fuse_error(
                        e.raw_os_error().unwrap_or(libc::EIO),
                    ));
                }
            }
        }
    }

    fn op_opendir(&self, unique: u64, nodeid: u64) -> Result<Vec<u8>, i32> {
        let epoch = self.current_epoch();
        let (share_idx, cursor) = if nodeid == FUSE_ROOT_ID {
            let mut entries = Vec::with_capacity(self.shares.len() + 2);
            entries.push(RawDirEntry {
                ino: FUSE_ROOT_ID,
                off: 1,
                d_type: libc::DT_DIR as u32,
                name: b".".to_vec(),
            });
            entries.push(RawDirEntry {
                ino: FUSE_ROOT_ID,
                off: 2,
                d_type: libc::DT_DIR as u32,
                name: b"..".to_vec(),
            });
            for (i, sh) in self.shares.iter().enumerate() {
                entries.push(RawDirEntry {
                    ino: sh.root_ino,
                    off: (i + 3) as u64,
                    d_type: libc::DT_DIR as u32,
                    name: sh.name.as_bytes().to_vec(),
                });
            }
            (None, DirCursor::VirtualRoot { entries })
        } else {
            let inode = self.lookup_inode(nodeid)?;
            let dir_fd =
                reopen_inode_fd(inode.fd.as_raw_fd(), inode.file_type, libc::O_RDONLY, true)?;
            #[cfg(target_os = "linux")]
            let stream = LinuxDirStream::from_fd(dir_fd)?;
            #[cfg(not(target_os = "linux"))]
            let stream = SnapshotDirStream::from_fd(dir_fd)?;
            (Some(inode.share_idx), DirCursor::Host(stream))
        };

        let cursor = Arc::new(Mutex::new(cursor));
        let fh = {
            let mut t = self.tables.write().unwrap();
            if t.session_epoch != epoch {
                drop(t);
                drop(cursor);
                return Err(-LINUX_EIO);
            }
            if t.dirs.len() >= self.limits.max_open_dirs {
                drop(t);
                drop(cursor);
                return Err(-LINUX_EMFILE);
            }
            let fh = t.next_fh;
            t.next_fh = t.next_fh.wrapping_add(1).max(1);
            t.dirs.insert(
                fh,
                DirHandle {
                    nodeid,
                    share_idx,
                    cursor,
                },
            );
            fh
        };

        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_OPEN_OUT_LEN];
        finish_ok(&mut out, unique);
        write_open_out(&mut out[FUSE_OUT_HEADER_LEN..], fh);
        Ok(out)
    }

    fn op_readdir(
        &self,
        unique: u64,
        nodeid: u64,
        body: &[u8],
        max_payload: usize,
        plus: bool,
    ) -> Result<Vec<u8>, i32> {
        if body.len() < 24 {
            return Err(-LINUX_EINVAL);
        }
        let epoch = self.current_epoch();
        let fh = read_u64_le(body, 0);
        let offset = read_u64_le(body, 8);
        let req_size = read_u32_le(body, 16) as usize;
        let limit = req_size.min(MAX_IO_BYTES as usize).min(max_payload);

        let dir_handle = {
            let t = self.tables.read().unwrap();
            let h = t.dirs.get(&fh).ok_or(-LINUX_EBADF)?;
            if h.nodeid != nodeid {
                return Err(-LINUX_EBADF);
            }
            h.clone()
        };

        let parent_inode = if nodeid == FUSE_ROOT_ID {
            None
        } else {
            Some(self.lookup_inode(nodeid)?)
        };

        let mut out = Vec::with_capacity(FUSE_OUT_HEADER_LEN + limit.min(4096));
        out.resize(FUSE_OUT_HEADER_LEN, 0);
        let mut cursor = dir_handle.cursor.lock().unwrap();

        match &mut *cursor {
            DirCursor::VirtualRoot { entries } => {
                let mut produced = 0usize;
                for ent in entries.iter() {
                    if ent.off <= offset {
                        continue;
                    }
                    let rec_len = if plus {
                        fuse_align8(FUSE_ENTRY_OUT_LEN + FUSE_DIRENT_HDR_LEN + ent.name.len())
                    } else {
                        fuse_align8(FUSE_DIRENT_HDR_LEN + ent.name.len())
                    };
                    if out.len() - FUSE_OUT_HEADER_LEN + rec_len > limit {
                        if produced == 0 {
                            return Err(-LINUX_EINVAL);
                        }
                        break;
                    }
                    if plus {
                        self.append_virtual_direntplus(&mut out, ent)?;
                    } else {
                        append_dirent(&mut out, ent);
                    }
                    produced += 1;
                }
            }
            #[cfg(target_os = "linux")]
            DirCursor::Host(stream) => {
                stream.seek_if_needed(offset);
                let mut produced = 0usize;
                while let Some(mut ent) = stream.next_entry()? {
                    if ent.name.is_empty()
                        || ent.name.len() > MAX_NAME_LEN
                        || ent.name.iter().any(|&b| b == b'/' || b == 0)
                    {
                        stream.last_emitted_off = ent.off;
                        continue;
                    }
                    if ent.name == b".." {
                        if let Some(ref parent) = parent_inode {
                            if parent.is_share_root {
                                ent.ino = parent.ino;
                            }
                        }
                    }
                    let rec_len = if plus {
                        fuse_align8(FUSE_ENTRY_OUT_LEN + FUSE_DIRENT_HDR_LEN + ent.name.len())
                    } else {
                        fuse_align8(FUSE_DIRENT_HDR_LEN + ent.name.len())
                    };
                    if out.len() - FUSE_OUT_HEADER_LEN + rec_len > limit {
                        stream.peeked = Some(ent);
                        if produced == 0 {
                            return Err(-LINUX_EINVAL);
                        }
                        break;
                    }
                    if plus {
                        let parent = parent_inode.as_ref().ok_or(-LINUX_EIO)?;
                        self.append_host_direntplus(&mut out, epoch, parent, &ent)?;
                    } else {
                        append_dirent(&mut out, &ent);
                    }
                    stream.last_emitted_off = ent.off;
                    produced += 1;
                }
            }
            #[cfg(not(target_os = "linux"))]
            DirCursor::Host(stream) => {
                if !stream.loaded || offset == 0 {
                    stream.reload()?;
                }
                let mut produced = 0usize;
                for raw_ent in &stream.entries {
                    if raw_ent.off <= offset {
                        continue;
                    }
                    if raw_ent.name.is_empty()
                        || raw_ent.name.len() > MAX_NAME_LEN
                        || raw_ent.name.iter().any(|&b| b == b'/' || b == 0)
                    {
                        continue;
                    }
                    let mut ent = raw_ent.clone();
                    if ent.name == b".." {
                        if let Some(ref parent) = parent_inode {
                            if parent.is_share_root {
                                ent.ino = parent.ino;
                            }
                        }
                    }
                    let rec_len = if plus {
                        fuse_align8(FUSE_ENTRY_OUT_LEN + FUSE_DIRENT_HDR_LEN + ent.name.len())
                    } else {
                        fuse_align8(FUSE_DIRENT_HDR_LEN + ent.name.len())
                    };
                    if out.len() - FUSE_OUT_HEADER_LEN + rec_len > limit {
                        if produced == 0 {
                            return Err(-LINUX_EINVAL);
                        }
                        break;
                    }
                    if plus {
                        let parent = parent_inode.as_ref().ok_or(-LINUX_EIO)?;
                        self.append_host_direntplus(&mut out, epoch, parent, &ent)?;
                    } else {
                        append_dirent(&mut out, &ent);
                    }
                    produced += 1;
                }
            }
        }

        let _ = dir_handle.share_idx;
        finish_ok(&mut out, unique);
        Ok(out)
    }

    fn append_virtual_direntplus(&self, out: &mut Vec<u8>, ent: &RawDirEntry) -> Result<(), i32> {
        let start = out.len();
        let rec_len = fuse_align8(FUSE_ENTRY_OUT_LEN + FUSE_DIRENT_HDR_LEN + ent.name.len());
        out.resize(start + rec_len, 0);
        if ent.name != b"." && ent.name != b".." {
            if let Ok(name) = std::str::from_utf8(&ent.name) {
                if let Some(&share_idx) = self.share_by_name.get(name) {
                    let sh = &self.shares[share_idx];
                    if let Ok(st) = fstat_fd(sh.root_fd.as_raw_fd()) {
                        let attr = FuseAttr::from_stat(&st);
                        let gen = {
                            let mut t = self.tables.write().unwrap();
                            if let Some(e) = t.inodes.get_mut(&sh.root_nodeid) {
                                e.nlookup = e.nlookup.saturating_add(1);
                                e.generation
                            } else {
                                1
                            }
                        };
                        write_entry_out(
                            &mut out[start..start + FUSE_ENTRY_OUT_LEN],
                            sh.root_nodeid,
                            gen,
                            &attr,
                        );
                    }
                }
            }
        }
        write_dirent_at(&mut out[start + FUSE_ENTRY_OUT_LEN..], ent, ent.d_type);
        Ok(())
    }

    fn append_host_direntplus(
        &self,
        out: &mut Vec<u8>,
        epoch: u64,
        parent: &InodeEntry,
        ent: &RawDirEntry,
    ) -> Result<(), i32> {
        let start = out.len();
        let rec_len = fuse_align8(FUSE_ENTRY_OUT_LEN + FUSE_DIRENT_HDR_LEN + ent.name.len());
        out.resize(start + rec_len, 0);
        let mut d_type = ent.d_type;
        if ent.name != b"." && ent.name != b".." {
            if let Ok(cname) = validate_component(&ent.name) {
                if let Ok(child_fd) = open_inode_handle(parent.fd.as_raw_fd(), &cname) {
                    if let Ok(st) = fstat_fd(child_fd.as_raw_fd()) {
                        let attr = FuseAttr::from_stat(&st);
                        d_type = mode_to_dt(attr.mode);
                        if let Ok((cid, gen)) = self.insert_inode_for_epoch(
                            epoch,
                            parent.share_idx,
                            parent.nodeid,
                            &st,
                            child_fd,
                            1,
                        ) {
                            write_entry_out(
                                &mut out[start..start + FUSE_ENTRY_OUT_LEN],
                                cid,
                                gen,
                                &attr,
                            );
                        }
                    }
                }
            }
        }
        write_dirent_at(&mut out[start + FUSE_ENTRY_OUT_LEN..], ent, d_type);
        Ok(())
    }

    fn op_releasedir(&self, unique: u64, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if body.len() < 8 {
            return Err(-LINUX_EINVAL);
        }
        let fh = read_u64_le(body, 0);
        let removed = {
            let mut t = self.tables.write().unwrap();
            if t.dirs.get(&fh).is_some_and(|h| h.nodeid == nodeid) {
                t.dirs.remove(&fh)
            } else {
                None
            }
        };
        drop(removed);
        Ok(make_ok_empty(unique))
    }

    fn op_getxattr(
        &self,
        unique: u64,
        nodeid: u64,
        body: &[u8],
        max_payload: usize,
    ) -> Result<Vec<u8>, i32> {
        if nodeid == FUSE_ROOT_ID {
            return Err(-LINUX_ENODATA);
        }
        if body.len() < 8 {
            return Err(-LINUX_EINVAL);
        }
        let size = read_u32_le(body, 0);
        let (name_bytes, _) = split_cstr(&body[8..])?;
        let inode = self.lookup_inode(nodeid)?;
        if size > 0 && max_payload == 0 {
            return Err(-LINUX_ERANGE);
        }
        let cap = if size == 0 {
            0
        } else {
            (size as usize).min(max_payload).min(MAX_XATTR_VALUE) as u32
        };
        let (actual_len, val) =
            xattr_get_inode(inode.fd.as_raw_fd(), inode.file_type, name_bytes, cap)?;
        if size == 0 {
            let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_GETXATTR_OUT_LEN];
            finish_ok(&mut out, unique);
            out[16..20].copy_from_slice(&actual_len.to_le_bytes());
            return Ok(out);
        }
        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + val.len()];
        finish_ok(&mut out, unique);
        out[FUSE_OUT_HEADER_LEN..].copy_from_slice(&val);
        Ok(out)
    }

    fn op_listxattr(
        &self,
        unique: u64,
        nodeid: u64,
        body: &[u8],
        max_payload: usize,
    ) -> Result<Vec<u8>, i32> {
        if body.len() < 8 {
            return Err(-LINUX_EINVAL);
        }
        let size = read_u32_le(body, 0);
        if nodeid == FUSE_ROOT_ID {
            if size == 0 {
                let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_GETXATTR_OUT_LEN];
                finish_ok(&mut out, unique);
                return Ok(out);
            }
            return Ok(make_ok_empty(unique));
        }
        let inode = self.lookup_inode(nodeid)?;
        if size > 0 && max_payload == 0 {
            return Err(-LINUX_ERANGE);
        }
        let cap = if size == 0 {
            0
        } else {
            (size as usize).min(max_payload).min(MAX_XATTR_VALUE) as u32
        };
        let (actual_len, list) = xattr_list_inode(inode.fd.as_raw_fd(), inode.file_type, cap)?;
        if size == 0 {
            let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_GETXATTR_OUT_LEN];
            finish_ok(&mut out, unique);
            out[16..20].copy_from_slice(&actual_len.to_le_bytes());
            return Ok(out);
        }
        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + list.len()];
        finish_ok(&mut out, unique);
        out[FUSE_OUT_HEADER_LEN..].copy_from_slice(&list);
        Ok(out)
    }

    fn op_setxattr(&self, unique: u64, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if nodeid == FUSE_ROOT_ID {
            return Err(-LINUX_EROFS);
        }
        let ext = (self.tables.read().unwrap().session_flags & FUSE_SETXATTR_EXT) != 0;
        let hdr_len = if ext { 16 } else { 8 };
        if body.len() < hdr_len {
            return Err(-LINUX_EINVAL);
        }
        let size = read_u32_le(body, 0) as usize;
        let flags = read_u32_le(body, 4);
        let (name_bytes, rest) = split_cstr(&body[hdr_len..])?;
        if rest.len() < size {
            return Err(-LINUX_EINVAL);
        }
        let value = &rest[..size];
        let inode = self.lookup_inode(nodeid)?;
        self.check_writable_share(inode.share_idx)?;
        xattr_set_inode(inode.fd.as_raw_fd(), inode.file_type, name_bytes, value, flags)?;
        Ok(make_ok_empty(unique))
    }

    fn op_removexattr(&self, unique: u64, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if nodeid == FUSE_ROOT_ID {
            return Err(-LINUX_EROFS);
        }
        let (name_bytes, _) = split_cstr(body)?;
        let inode = self.lookup_inode(nodeid)?;
        self.check_writable_share(inode.share_idx)?;
        xattr_remove_inode(inode.fd.as_raw_fd(), inode.file_type, name_bytes)?;
        Ok(make_ok_empty(unique))
    }

    fn op_access(&self, unique: u64, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if body.len() < 4 {
            return Err(-LINUX_EINVAL);
        }
        let mask = read_u32_le(body, 0);
        if nodeid == FUSE_ROOT_ID {
            if (mask & (libc::W_OK as u32)) != 0 {
                return Err(-LINUX_EROFS);
            }
            return Ok(make_ok_empty(unique));
        }
        let inode = self.lookup_inode(nodeid)?;
        if (mask & (libc::W_OK as u32)) != 0 {
            self.check_writable_share(inode.share_idx)?;
        }
        Ok(make_ok_empty(unique))
    }

    fn op_fallocate(&self, unique: u64, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if nodeid == FUSE_ROOT_ID {
            return Err(-LINUX_EROFS);
        }
        if body.len() < 28 {
            return Err(-LINUX_EINVAL);
        }
        let fh = read_u64_le(body, 0);
        let offset = read_u64_le(body, 8);
        let length = read_u64_le(body, 16);
        let mode = read_u32_le(body, 24);
        if offset.saturating_add(length) > VIRTIOFS_SIZE_MAX {
            return Err(-LINUX_EFBIG);
        }
        let (file, share_idx, writable) = {
            let t = self.tables.read().unwrap();
            let h = t.files.get(&fh).ok_or(-LINUX_EBADF)?;
            if h.nodeid != nodeid {
                return Err(-LINUX_EBADF);
            }
            (Arc::clone(&h.file), h.share_idx, h.writable)
        };
        self.check_writable_share(share_idx)?;
        if !writable {
            return Err(-LINUX_EBADF);
        }
        #[cfg(target_os = "linux")]
        {
            let off_t = libc::off_t::try_from(offset).map_err(|_| -LINUX_EINVAL)?;
            let len_t = libc::off_t::try_from(length).map_err(|_| -LINUX_EINVAL)?;
            loop {
                let rc = unsafe { libc::fallocate(file.as_raw_fd(), mode as i32, off_t, len_t) };
                if rc == 0 {
                    return Ok(make_ok_empty(unique));
                }
                let err = io::Error::last_os_error()
                    .raw_os_error()
                    .unwrap_or(libc::EIO);
                if err == libc::EINTR {
                    continue;
                }
                return Err(host_errno_to_fuse_error(err));
            }
        }
        #[cfg(not(target_os = "linux"))]
        {
            let _ = (unique, file, mode);
            Err(-LINUX_EOPNOTSUPP)
        }
    }

    fn op_lseek(&self, unique: u64, nodeid: u64, body: &[u8]) -> Result<Vec<u8>, i32> {
        if body.len() < 20 {
            return Err(-LINUX_EINVAL);
        }
        let fh = read_u64_le(body, 0);
        let offset = read_u64_le(body, 8);
        let whence = read_u32_le(body, 16);
        let host_whence = match whence {
            3 => libc::SEEK_DATA,
            4 => libc::SEEK_HOLE,
            _ => return Err(-LINUX_EINVAL),
        };
        let off_t = libc::off_t::try_from(offset).map_err(|_| -LINUX_EINVAL)?;
        let file = {
            let t = self.tables.read().unwrap();
            let h = t.files.get(&fh).ok_or(-LINUX_EBADF)?;
            if h.nodeid != nodeid {
                return Err(-LINUX_EBADF);
            }
            Arc::clone(&h.file)
        };
        let rc = unsafe { libc::lseek(file.as_raw_fd(), off_t, host_whence) };
        if rc < 0 {
            return Err(last_fuse_error());
        }
        let mut out = vec![0u8; FUSE_OUT_HEADER_LEN + FUSE_LSEEK_OUT_LEN];
        finish_ok(&mut out, unique);
        out[16..24].copy_from_slice(&(rc as u64).to_le_bytes());
        Ok(out)
    }

    fn op_syncfs(&self, unique: u64, nodeid: u64) -> Result<Vec<u8>, i32> {
        let target_share = if nodeid == FUSE_ROOT_ID {
            None
        } else {
            let inode = self.lookup_inode(nodeid)?;
            if self.shares.get(inode.share_idx).is_none_or(|s| s.read_only) {
                return Ok(make_ok_empty(unique));
            }
            Some(inode.share_idx)
        };
        // Flush open writable file handles for the target share (or all writable
        // shares for the virtual root) instead of triggering host-wide syncfs(2)
        // writeback amplification.
        let files: Vec<Arc<File>> = self
            .tables
            .read()
            .unwrap()
            .files
            .values()
            .filter(|h| {
                h.writable
                    && target_share.is_none_or(|s| h.share_idx == s)
                    && self.shares.get(h.share_idx).is_some_and(|sh| !sh.read_only)
            })
            .map(|h| Arc::clone(&h.file))
            .collect();
        for f in files {
            let _ = f.sync_data();
        }
        Ok(make_ok_empty(unique))
    }
}

impl Drop for VirtioFs {
    fn drop(&mut self) {
        self.shutdown();
    }
}

fn append_dirent(out: &mut Vec<u8>, ent: &RawDirEntry) {
    let start = out.len();
    let rec_len = fuse_align8(FUSE_DIRENT_HDR_LEN + ent.name.len());
    out.resize(start + rec_len, 0);
    write_dirent_at(&mut out[start..], ent, ent.d_type);
}

fn write_dirent_at(dst: &mut [u8], ent: &RawDirEntry, d_type: u32) {
    dst[0..8].copy_from_slice(&ent.ino.to_le_bytes());
    dst[8..16].copy_from_slice(&ent.off.to_le_bytes());
    dst[16..20].copy_from_slice(&(ent.name.len() as u32).to_le_bytes());
    dst[20..24].copy_from_slice(&d_type.to_le_bytes());
    dst[24..24 + ent.name.len()].copy_from_slice(&ent.name);
}

fn scatter_write(mem: &GuestMemory, segs: &[(u64, u32)], data: &[u8]) -> u32 {
    let mut written = 0usize;
    for &(gpa, len) in segs {
        if written >= data.len() {
            break;
        }
        let chunk = (len as usize).min(data.len() - written);
        if chunk > 0 {
            if mem
                .write_slice(gpa, &data[written..written + chunk])
                .is_err()
            {
                break;
            }
            written += chunk;
        }
    }
    written as u32
}

pub struct VirtioFsAdapter(pub Arc<VirtioFs>);

impl VirtioDevice for VirtioFsAdapter {
    fn pci_device_id(&self) -> u16 {
        PCI_DEVICE_VIRTIO_FS
    }

    fn pci_subsystem_id(&self) -> u16 {
        VIRTIO_FS_SUBSYSTEM_ID
    }

    fn pci_class_code(&self) -> u32 {
        VIRTIO_FS_PCI_CLASS
    }

    fn features(&self) -> u64 {
        VIRTIO_F_VERSION_1 | VIRTIO_RING_F_INDIRECT_DESC
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &[HIPRIO_QUEUE_SIZE, REQ_QUEUE_SIZE]
    }

    fn read_config(&self, offset: u64, data: &mut [u8]) {
        for (i, b) in data.iter_mut().enumerate() {
            let idx = offset as usize + i;
            *b = self.0.dev_config.get(idx).copied().unwrap_or(0);
        }
    }

    fn process_queue(
        &self,
        queue_index: u16,
        queue: &mut VirtQueue,
        mem: &GuestMemory,
    ) -> QueueOutcome {
        if queue_index > 1 {
            return QueueOutcome::default();
        }
        let vpci_opt = self.0.vpci.get().and_then(Weak::upgrade);
        let expected_gen = vpci_opt.as_ref().map_or(0, |v| v.reset_generation());
        let has_workers =
            vpci_opt.is_some() && self.0.workers_started.load(Ordering::Acquire);
        let mut raise_irq = false;

        loop {
            let chain = match queue.pop_avail(mem) {
                Ok(Some(c)) => c,
                Ok(None) => break,
                Err(QueueError::NotReady) => break,
                Err(_) => {
                    return QueueOutcome {
                        raise_irq,
                        needs_reset: true,
                    };
                }
            };

            let mut read_segs = Vec::new();
            let mut out_segs = Vec::new();
            let mut total_read = 0usize;
            let mut total_write = 0usize;
            let mut bad_chain = false;

            for d in &chain.descriptors {
                if d.is_write() {
                    out_segs.push((d.addr, d.len));
                    total_write = total_write.saturating_add(d.len as usize);
                } else {
                    if !out_segs.is_empty() {
                        bad_chain = true;
                        break;
                    }
                    total_read = total_read.saturating_add(d.len as usize);
                    if total_read > MAX_REQ_BYTES {
                        bad_chain = true;
                        break;
                    }
                    read_segs.push((d.addr, d.len));
                }
            }

            if bad_chain || total_read < FUSE_IN_HEADER_LEN {
                let written = if total_write >= FUSE_OUT_HEADER_LEN {
                    let err = make_error_reply(0, -LINUX_EINVAL);
                    scatter_write(mem, &out_segs, &err)
                } else {
                    0
                };
                if queue.push_used(mem, chain.head_index, written) {
                    raise_irq = true;
                }
                continue;
            }

            let mut in_buf = vec![0u8; total_read];
            let mut off = 0usize;
            let mut read_ok = true;
            for (gpa, len) in read_segs {
                let seg_len = len as usize;
                if seg_len > 0 {
                    if mem.read_slice(gpa, &mut in_buf[off..off + seg_len]).is_err() {
                        read_ok = false;
                        break;
                    }
                    off += seg_len;
                }
            }
            if !read_ok {
                let written = if total_write >= FUSE_OUT_HEADER_LEN {
                    let err = make_error_reply(0, -LINUX_EIO);
                    scatter_write(mem, &out_segs, &err)
                } else {
                    0
                };
                if queue.push_used(mem, chain.head_index, written) {
                    raise_irq = true;
                }
                continue;
            }

            let unique = read_u64_le(&in_buf, 8);
            let out_cap = total_write.min(FUSE_OUT_HEADER_LEN + MAX_IO_BYTES as usize + 4096);

            if !has_workers {
                // Synchronous fallback (used by direct VirtQueue unit tests without worker threads).
                let reply = self.0.dispatch_fuse_bytes(&in_buf, out_cap);
                let written = match reply {
                    Some(bytes) => scatter_write(mem, &out_segs, &bytes),
                    None => 0,
                };
                if queue.push_used(mem, chain.head_index, written) {
                    raise_irq = true;
                }
                continue;
            }

            // Exempt Q0 hiprio (FUSE_FORGET / FUSE_BATCH_FORGET) from Q1's
            // inflight byte budget so write saturation never leaks host inodes.
            let budget = if queue_index == 0 {
                BudgetGuard::untracked()
            } else {
                let Some(b) = self.0.reserve_budget(in_buf.len()) else {
                    let written = if total_write >= FUSE_OUT_HEADER_LEN {
                        let err = make_error_reply(unique, -LINUX_ENOMEM);
                        scatter_write(mem, &out_segs, &err)
                    } else {
                        0
                    };
                    if queue.push_used(mem, chain.head_index, written) {
                        raise_irq = true;
                    }
                    continue;
                };
                b
            };

            let job = FsJob {
                queue_index,
                head_index: chain.head_index,
                expected_gen,
                in_buf,
                out_segs,
                out_cap,
                _budget: budget,
            };

            if let Err(rejected) = self.0.jobs.try_push(job) {
                let written = if rejected.out_cap >= FUSE_OUT_HEADER_LEN {
                    let err = make_error_reply(unique, -LINUX_ENOMEM);
                    scatter_write(mem, &rejected.out_segs, &err)
                } else {
                    0
                };
                if queue.push_used(mem, rejected.head_index, written) {
                    raise_irq = true;
                }
            }
        }

        QueueOutcome {
            raise_irq,
            needs_reset: false,
        }
    }

    fn reset(&self) {
        self.0.jobs.clear_pending();
        let dropped = self.0.tables.write().unwrap().reset(&self.0.shares);
        drop(dropped);
    }

    fn flush(&self) {
        let files: Vec<Arc<File>> = self
            .0
            .tables
            .read()
            .unwrap()
            .files
            .values()
            .filter(|h| h.writable)
            .map(|h| Arc::clone(&h.file))
            .collect();
        for f in files {
            let _ = f.sync_data();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::irq::{test_util::MockSpiBackend, SpiMux};
    use crate::memory::RAM_BASE;
    use crate::virtio::queue::{VRING_DESC_F_NEXT, VRING_DESC_F_WRITE};
    use std::fs;
    use std::os::unix::fs::symlink;
    use std::sync::atomic::AtomicU64;

    static TEST_SEQ: AtomicU64 = AtomicU64::new(1);

    struct TempDir(PathBuf);
    impl TempDir {
        fn new(prefix: &str) -> Self {
            let seq = TEST_SEQ.fetch_add(1, Ordering::Relaxed);
            let p = std::env::temp_dir().join(format!(
                "kido-virtiofs-test-{prefix}-{}-{seq}",
                std::process::id()
            ));
            fs::create_dir_all(&p).unwrap();
            Self(p)
        }
        fn path(&self) -> &Path {
            &self.0
        }
    }
    impl Drop for TempDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn build_req(opcode: u32, unique: u64, nodeid: u64, body: &[u8]) -> Vec<u8> {
        let total = FUSE_IN_HEADER_LEN + body.len();
        let mut req = vec![0u8; total];
        req[0..4].copy_from_slice(&(total as u32).to_le_bytes());
        req[4..8].copy_from_slice(&opcode.to_le_bytes());
        req[8..16].copy_from_slice(&unique.to_le_bytes());
        req[16..24].copy_from_slice(&nodeid.to_le_bytes());
        req[FUSE_IN_HEADER_LEN..].copy_from_slice(body);
        req
    }

    fn reply_header(reply: &[u8]) -> (u32, i32, u64) {
        (
            read_u32_le(reply, 0),
            i32::from_le_bytes(reply[4..8].try_into().unwrap()),
            read_u64_le(reply, 8),
        )
    }

    fn fuse_init(fs: &VirtioFs) {
        let mut body = vec![0u8; 64];
        body[0..4].copy_from_slice(&FUSE_KERNEL_VERSION.to_le_bytes());
        body[4..8].copy_from_slice(&46u32.to_le_bytes());
        body[8..12].copy_from_slice(&MAX_IO_BYTES.to_le_bytes());
        body[12..16].copy_from_slice(
            &(FUSE_ASYNC_READ | FUSE_PARALLEL_DIROPS | FUSE_SETXATTR_EXT).to_le_bytes(),
        );
        let req = build_req(FUSE_OP_INIT, 2, 0, &body);
        let rep = fs.dispatch_fuse_bytes(&req, 4096).unwrap();
        let (len, err, uniq) = reply_header(&rep);
        assert_eq!(err, 0);
        assert_eq!(uniq, 2);
        assert_eq!(len as usize, FUSE_OUT_HEADER_LEN + FUSE_INIT_OUT_LEN);
        assert_eq!(read_u32_le(&rep, 16), 7);
        assert_eq!(read_u32_le(&rep, 36), MAX_IO_BYTES);
    }

    fn fuse_lookup(fs: &VirtioFs, parent: u64, name: &str) -> Result<(u64, u32, u64), i32> {
        let mut body = name.as_bytes().to_vec();
        body.push(0);
        let req = build_req(FUSE_OP_LOOKUP, 10, parent, &body);
        let rep = fs.dispatch_fuse_bytes(&req, 4096).unwrap();
        let (_, err, _) = reply_header(&rep);
        if err != 0 {
            return Err(err);
        }
        let nodeid = read_u64_le(&rep, 16);
        let size = read_u64_le(&rep, 16 + 40 + 8);
        let mode = read_u32_le(&rep, 16 + 40 + 60);
        Ok((nodeid, mode, size))
    }

    #[test]
    fn config_space_and_errno_mapping_match_darwin_kext() {
        let dir = TempDir::new("cfg");
        let fs = VirtioFs::new(
            DEFAULT_FS_TAG,
            &[ShareConfig {
                name: "m0".to_string(),
                path: dir.path().to_path_buf(),
                read_only: false,
            }],
        )
        .unwrap();
        let adapter = VirtioFsAdapter(Arc::clone(&fs));
        assert_eq!(adapter.pci_device_id(), PCI_DEVICE_VIRTIO_FS);
        assert_eq!(adapter.pci_subsystem_id(), 26);
        assert_eq!(adapter.pci_class_code(), 0x018000);
        assert_eq!(adapter.queue_max_sizes(), &[64, 256]);
        let mut cfg = [0u8; 40];
        adapter.read_config(0, &mut cfg);
        assert_eq!(&cfg[0..5], b"kido\0");
        assert_eq!(read_u32_le(&cfg, 36), 1);

        // Verify canonical Linux errno mapping expected by VirtioFS.kext fuse_proto.c
        assert_eq!(host_errno_to_fuse_error(libc::ENOENT), -LINUX_ENOENT);
        assert_eq!(host_errno_to_fuse_error(libc::EROFS), -LINUX_EROFS);
        assert_eq!(host_errno_to_fuse_error(libc::EAGAIN), -11);
        assert_eq!(host_errno_to_fuse_error(libc::ENAMETOOLONG), -36);
        assert_eq!(host_errno_to_fuse_error(libc::ENOSYS), -38);
        assert_eq!(host_errno_to_fuse_error(libc::ENOTEMPTY), -39);
        assert_eq!(host_errno_to_fuse_error(libc::ELOOP), -40);
        assert_eq!(host_errno_to_fuse_error(libc::EOVERFLOW), -75);
        assert_eq!(host_errno_to_fuse_error(libc::EOPNOTSUPP), -95);
        assert_eq!(host_errno_to_fuse_error(libc::ESTALE), -116);
        assert_eq!(host_errno_to_fuse_error(libc::EDQUOT), -122);
        #[cfg(target_os = "linux")]
        assert_eq!(host_errno_to_fuse_error(libc::ENODATA), -61);
        #[cfg(target_os = "macos")]
        assert_eq!(host_errno_to_fuse_error(libc::ENOATTR), -61);
        assert_eq!(host_errno_to_fuse_error(99999), -LINUX_EIO);
    }

    #[test]
    fn symlink_escape_and_path_traversal_are_blocked() {
        let base = TempDir::new("sec");
        let share = base.path().join("share");
        let outside = base.path().join("outside");
        fs::create_dir_all(&share).unwrap();
        fs::create_dir_all(&outside).unwrap();
        fs::write(outside.join("secret.txt"), b"TOP_SECRET").unwrap();
        symlink(outside.join("secret.txt"), share.join("escape_file")).unwrap();
        symlink(&outside, share.join("escape_dir")).unwrap();
        symlink("/etc/passwd", share.join("abs_escape")).unwrap();

        let fs_dev = VirtioFs::new(
            DEFAULT_FS_TAG,
            &[ShareConfig {
                name: "m0".to_string(),
                path: share.clone(),
                read_only: false,
            }],
        )
        .unwrap();
        fuse_init(&fs_dev);
        let (m0, _, _) = fuse_lookup(&fs_dev, FUSE_ROOT_ID, "m0").unwrap();

        // Component names with slash or traversal are rejected.
        assert_eq!(fuse_lookup(&fs_dev, m0, "../outside/secret.txt"), Err(-LINUX_EINVAL));
        assert_eq!(fuse_lookup(&fs_dev, m0, "a/b"), Err(-LINUX_EINVAL));
        assert_eq!(fuse_lookup(&fs_dev, m0, ""), Err(-LINUX_EINVAL));

        // ".." on share root stays confined at share root (never escapes to parent!).
        let (dotdot_id, _, _) = fuse_lookup(&fs_dev, m0, "..").unwrap();
        assert_eq!(dotdot_id, m0);

        // Lookup of symlink returns S_IFLNK (NOT S_IFREG/S_IFDIR) and cannot be opened or traversed.
        let (esc_file_id, mode, _) = fuse_lookup(&fs_dev, m0, "escape_file").unwrap();
        assert_eq!((mode & (libc::S_IFMT as u32)) as libc::mode_t, libc::S_IFLNK);
        let open_req = build_req(FUSE_OP_OPEN, 20, esc_file_id, &[0u8; 8]);
        let open_rep = fs_dev.dispatch_fuse_bytes(&open_req, 4096).unwrap();
        assert_eq!(reply_header(&open_rep).1, -LINUX_ELOOP);

        let (esc_dir_id, dmode, _) = fuse_lookup(&fs_dev, m0, "escape_dir").unwrap();
        assert_eq!((dmode & (libc::S_IFMT as u32)) as libc::mode_t, libc::S_IFLNK);
        assert_eq!(fuse_lookup(&fs_dev, esc_dir_id, "secret.txt"), Err(-LINUX_ENOTDIR));
        let opendir_req = build_req(FUSE_OP_OPENDIR, 21, esc_dir_id, &[0u8; 8]);
        let opendir_rep = fs_dev.dispatch_fuse_bytes(&opendir_req, 4096).unwrap();
        assert_eq!(reply_header(&opendir_rep).1, -LINUX_ENOTDIR);

        // Readlink returns raw target string without resolving on host.
        let rl_req = build_req(FUSE_OP_READLINK, 22, esc_file_id, &[]);
        let rl_rep = fs_dev.dispatch_fuse_bytes(&rl_req, 4096).unwrap();
        assert_eq!(reply_header(&rl_rep).1, 0);
        assert_eq!(
            &rl_rep[FUSE_OUT_HEADER_LEN..],
            outside.join("secret.txt").as_os_str().as_encoded_bytes()
        );
    }

    #[test]
    fn read_only_share_rejects_every_mutating_opcode() {
        let dir = TempDir::new("ro");
        fs::write(dir.path().join("existing.txt"), b"hello").unwrap();
        fs::create_dir(dir.path().join("sub")).unwrap();

        let fs_dev = VirtioFs::new(
            DEFAULT_FS_TAG,
            &[
                ShareConfig {
                    name: "rw".to_string(),
                    path: dir.path().to_path_buf(),
                    read_only: false,
                },
                ShareConfig {
                    name: "ro".to_string(),
                    path: dir.path().to_path_buf(),
                    read_only: true,
                },
            ],
        )
        .unwrap();
        fuse_init(&fs_dev);

        let (rw_id, _, _) = fuse_lookup(&fs_dev, FUSE_ROOT_ID, "rw").unwrap();
        let (ro_id, _, _) = fuse_lookup(&fs_dev, FUSE_ROOT_ID, "ro").unwrap();
        assert_ne!(rw_id, ro_id); // Same host directory mounted rw + ro must NOT alias nodeids!

        let (ro_file, _, _) = fuse_lookup(&fs_dev, ro_id, "existing.txt").unwrap();

        // Reading on RO works.
        let open_ro = build_req(FUSE_OP_OPEN, 30, ro_file, &[0u8; 8]);
        let open_ro_rep = fs_dev.dispatch_fuse_bytes(&open_ro, 4096).unwrap();
        assert_eq!(reply_header(&open_ro_rep).1, 0);
        let ro_fh = read_u64_le(&open_ro_rep, 16);

        // Open for write on RO -> EROFS.
        let mut wopen = [0u8; 8];
        wopen[0..4].copy_from_slice(&FUSE_O_WRONLY.to_le_bytes());
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_OPEN, 31, ro_file, &wopen), 4096).unwrap()).1,
            -LINUX_EROFS
        );

        // CREATE -> EROFS.
        let mut create_body = vec![0u8; 16];
        create_body[0..4].copy_from_slice(&(FUSE_O_WRONLY | FUSE_O_CREAT | FUSE_O_EXCL).to_le_bytes());
        create_body[4..8].copy_from_slice(&(libc::S_IFREG as u32 | 0o644).to_le_bytes());
        create_body.extend_from_slice(b"newfile\0");
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_CREATE, 32, ro_id, &create_body), 4096).unwrap()).1,
            -LINUX_EROFS
        );

        // MKDIR -> EROFS.
        let mut mkdir_body = vec![0u8; 8];
        mkdir_body[0..4].copy_from_slice(&0o755u32.to_le_bytes());
        mkdir_body.extend_from_slice(b"newdir\0");
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_MKDIR, 33, ro_id, &mkdir_body), 4096).unwrap()).1,
            -LINUX_EROFS
        );

        // MKNOD -> EROFS.
        let mut mknod_body = vec![0u8; 16];
        mknod_body[0..4].copy_from_slice(&(libc::S_IFIFO as u32 | 0o644).to_le_bytes());
        mknod_body.extend_from_slice(b"fifo\0");
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_MKNOD, 34, ro_id, &mknod_body), 4096).unwrap()).1,
            -LINUX_EROFS
        );

        // SYMLINK -> EROFS.
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_SYMLINK, 35, ro_id, b"l\0target\0"), 4096).unwrap()).1,
            -LINUX_EROFS
        );

        // UNLINK & RMDIR -> EROFS.
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_UNLINK, 36, ro_id, b"existing.txt\0"), 4096).unwrap()).1,
            -LINUX_EROFS
        );
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_RMDIR, 37, ro_id, b"sub\0"), 4096).unwrap()).1,
            -LINUX_EROFS
        );

        // RENAME -> EROFS.
        let mut ren_body = ro_id.to_le_bytes().to_vec();
        ren_body.extend_from_slice(b"existing.txt\0moved.txt\0");
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_RENAME, 38, ro_id, &ren_body), 4096).unwrap()).1,
            -LINUX_EROFS
        );

        // LINK -> EROFS.
        let mut link_body = ro_file.to_le_bytes().to_vec();
        link_body.extend_from_slice(b"hl\0");
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_LINK, 39, ro_id, &link_body), 4096).unwrap()).1,
            -LINUX_EROFS
        );

        // SETATTR -> EROFS.
        let mut sa_body = vec![0u8; 88];
        sa_body[0..4].copy_from_slice(&FATTR_MODE.to_le_bytes());
        sa_body[68..72].copy_from_slice(&0o777u32.to_le_bytes());
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_SETATTR, 40, ro_file, &sa_body), 4096).unwrap()).1,
            -LINUX_EROFS
        );

        // WRITE on RO fh -> EROFS.
        let mut w_body = vec![0u8; 40];
        w_body[0..8].copy_from_slice(&ro_fh.to_le_bytes());
        w_body[16..20].copy_from_slice(&3u32.to_le_bytes());
        w_body.extend_from_slice(b"abc");
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_WRITE, 41, ro_file, &w_body), 4096).unwrap()).1,
            -LINUX_EROFS
        );

        // SETXATTR / REMOVEXATTR -> EROFS.
        let mut sx = vec![0u8; 16];
        sx[0..4].copy_from_slice(&1u32.to_le_bytes());
        sx.extend_from_slice(b"k\0v");
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_SETXATTR, 42, ro_file, &sx), 4096).unwrap()).1,
            -LINUX_EROFS
        );
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_REMOVEXATTR, 43, ro_file, b"k\0"), 4096).unwrap()).1,
            -LINUX_EROFS
        );

        // Cross-share rename (rw -> ro) -> EXDEV.
        let mut xren = ro_id.to_le_bytes().to_vec();
        xren.extend_from_slice(b"existing.txt\0moved.txt\0");
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_RENAME, 44, rw_id, &xren), 4096).unwrap()).1,
            -LINUX_EXDEV
        );
    }

    #[test]
    fn full_crud_unlinked_open_rename2_hardlink_and_forget_refcounts() {
        let dir = TempDir::new("crud");
        let fs_dev = VirtioFs::new(
            DEFAULT_FS_TAG,
            &[ShareConfig {
                name: "m0".to_string(),
                path: dir.path().to_path_buf(),
                read_only: false,
            }],
        )
        .unwrap();
        fuse_init(&fs_dev);
        let (m0, _, _) = fuse_lookup(&fs_dev, FUSE_ROOT_ID, "m0").unwrap();

        // CREATE file "a"
        let mut cbody = vec![0u8; 16];
        cbody[0..4].copy_from_slice(&(FUSE_O_RDWR | FUSE_O_CREAT | FUSE_O_EXCL).to_le_bytes());
        cbody[4..8].copy_from_slice(&(libc::S_IFREG as u32 | 0o4644).to_le_bytes()); // includes SUID bit!
        cbody.extend_from_slice(b"a\0");
        let crep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_CREATE, 100, m0, &cbody), 4096)
            .unwrap();
        assert_eq!(reply_header(&crep).1, 0);
        let a_id = read_u64_le(&crep, 16);
        let a_mode = read_u32_le(&crep, 16 + 40 + 60);
        let a_fh = read_u64_le(&crep, 16 + FUSE_ENTRY_OUT_LEN);
        // SUID bit must have been stripped!
        assert_eq!(a_mode & 0o6000, 0);

        // WRITE "hello world"
        let mut wbody = vec![0u8; 40];
        wbody[0..8].copy_from_slice(&a_fh.to_le_bytes());
        wbody[16..20].copy_from_slice(&11u32.to_le_bytes());
        wbody.extend_from_slice(b"hello world");
        let wrep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_WRITE, 101, a_id, &wbody), 4096)
            .unwrap();
        assert_eq!(reply_header(&wrep).1, 0);
        assert_eq!(read_u32_le(&wrep, 16), 11);

        // Hardlink "a" -> "a_hard"
        let mut lbody = a_id.to_le_bytes().to_vec();
        lbody.extend_from_slice(b"a_hard\0");
        let lrep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_LINK, 102, m0, &lbody), 4096)
            .unwrap();
        assert_eq!(reply_header(&lrep).1, 0);
        assert_eq!(read_u64_le(&lrep, 16), a_id); // same inode!

        // Unlink BOTH names while a_fh is still open -> unlinked-while-open I/O must still work!
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_UNLINK, 103, m0, b"a\0"), 4096).unwrap()).1,
            0
        );
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_UNLINK, 104, m0, b"a_hard\0"), 4096).unwrap()).1,
            0
        );
        let mut rbody = vec![0u8; 40];
        rbody[0..8].copy_from_slice(&a_fh.to_le_bytes());
        rbody[16..20].copy_from_slice(&11u32.to_le_bytes());
        let rrep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_READ, 105, a_id, &rbody), 4096)
            .unwrap();
        assert_eq!(reply_header(&rrep).1, 0);
        assert_eq!(&rrep[FUSE_OUT_HEADER_LEN..], b"hello world");

        // Truncate unlinked open file via SETATTR(FATTR_SIZE | FATTR_FH)
        let mut sabody = vec![0u8; 88];
        sabody[0..4].copy_from_slice(&(FATTR_SIZE | FATTR_FH).to_le_bytes());
        sabody[8..16].copy_from_slice(&a_fh.to_le_bytes());
        sabody[16..24].copy_from_slice(&5u64.to_le_bytes());
        let sarep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_SETATTR, 106, a_id, &sabody), 4096)
            .unwrap();
        assert_eq!(reply_header(&sarep).1, 0);
        assert_eq!(read_u64_le(&sarep, 16 + 16 + 8), 5);

        // Release and FORGET(2) (create=1 + link=1 -> 2 lookups)
        let mut rel = vec![0u8; 24];
        rel[0..8].copy_from_slice(&a_fh.to_le_bytes());
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_RELEASE, 107, a_id, &rel), 4096).unwrap()).1,
            0
        );
        // FORGET(1) keeps inode alive (nlookup=1), FORGET(1) removes it.
        assert!(fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_FORGET, 108, a_id, &1u64.to_le_bytes()), 0)
            .is_none());
        assert!(fs_dev.tables.read().unwrap().inodes.contains_key(&a_id));
        assert!(fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_FORGET, 109, a_id, &1u64.to_le_bytes()), 0)
            .is_none());
        assert!(!fs_dev.tables.read().unwrap().inodes.contains_key(&a_id));

        // RENAME2: NOREPLACE and EXCHANGE
        fs::write(dir.path().join("x"), b"X").unwrap();
        fs::write(dir.path().join("y"), b"Y").unwrap();
        let mut r2 = vec![0u8; 16];
        r2[0..8].copy_from_slice(&m0.to_le_bytes());
        r2[8..12].copy_from_slice(&FUSE_RENAME_NOREPLACE.to_le_bytes());
        r2.extend_from_slice(b"x\0y\0");
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_RENAME2, 110, m0, &r2), 4096).unwrap()).1,
            -LINUX_EEXIST
        );
        r2[8..12].copy_from_slice(&FUSE_RENAME_EXCHANGE.to_le_bytes());
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_RENAME2, 111, m0, &r2), 4096).unwrap()).1,
            0
        );
        assert_eq!(fs::read(dir.path().join("x")).unwrap(), b"Y");
        assert_eq!(fs::read(dir.path().join("y")).unwrap(), b"X");

        // BATCH_FORGET on two looked-up files
        let (x_id, _, _) = fuse_lookup(&fs_dev, m0, "x").unwrap();
        let (y_id, _, _) = fuse_lookup(&fs_dev, m0, "y").unwrap();
        let mut bf = Vec::new();
        bf.extend_from_slice(&2u32.to_le_bytes());
        bf.extend_from_slice(&0u32.to_le_bytes());
        bf.extend_from_slice(&x_id.to_le_bytes());
        bf.extend_from_slice(&1u64.to_le_bytes());
        bf.extend_from_slice(&y_id.to_le_bytes());
        bf.extend_from_slice(&1u64.to_le_bytes());
        assert!(fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_BATCH_FORGET, 112, 0, &bf), 0)
            .is_none());
        assert!(!fs_dev.tables.read().unwrap().inodes.contains_key(&x_id));
        assert!(!fs_dev.tables.read().unwrap().inodes.contains_key(&y_id));

        // Share root survives FORGET(m0, 1) and can be looked up again (unmount+remount).
        assert!(fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_FORGET, 113, m0, &1u64.to_le_bytes()), 0)
            .is_none());
        let (m0_again, _, _) = fuse_lookup(&fs_dev, FUSE_ROOT_ID, "m0").unwrap();
        assert_eq!(m0_again, m0);
    }

    #[test]
    fn malformed_headers_and_quotas_do_not_panic_or_leak() {
        let dir = TempDir::new("malformed");
        fs::write(dir.path().join("f"), b"1").unwrap();
        let fs_dev = VirtioFs::new(
            DEFAULT_FS_TAG,
            &[ShareConfig {
                name: "m0".to_string(),
                path: dir.path().to_path_buf(),
                read_only: false,
            }],
        )
        .unwrap();
        fuse_init(&fs_dev);

        // Short header -> EINVAL
        let rep = fs_dev.dispatch_fuse_bytes(&[0u8; 10], 4096).unwrap();
        assert_eq!(reply_header(&rep).1, -LINUX_EINVAL);

        // Declared len > actual bytes -> EINVAL
        let mut bad = vec![0u8; FUSE_IN_HEADER_LEN];
        bad[0..4].copy_from_slice(&9999u32.to_le_bytes());
        bad[4..8].copy_from_slice(&FUSE_OP_GETATTR.to_le_bytes());
        bad[8..16].copy_from_slice(&77u64.to_le_bytes());
        let rep = fs_dev.dispatch_fuse_bytes(&bad, 4096).unwrap();
        assert_eq!(reply_header(&rep), (16, -LINUX_EINVAL, 77));

        // Unknown opcode -> ENOSYS
        let rep = fs_dev
            .dispatch_fuse_bytes(&build_req(9999, 88, FUSE_ROOT_ID, &[]), 4096)
            .unwrap();
        assert_eq!(reply_header(&rep), (16, -LINUX_ENOSYS, 88));

        // MKNOD device node -> EPERM
        let (m0, _, _) = fuse_lookup(&fs_dev, FUSE_ROOT_ID, "m0").unwrap();
        let mut dev_body = vec![0u8; 16];
        dev_body[0..4].copy_from_slice(&(libc::S_IFCHR as u32 | 0o600).to_le_bytes());
        dev_body.extend_from_slice(b"null\0");
        assert_eq!(
            reply_header(&fs_dev.dispatch_fuse_bytes(&build_req(FUSE_OP_MKNOD, 89, m0, &dev_body), 4096).unwrap()).1,
            -LINUX_EPERM
        );

        // Verify handle quota enforcement without actually leaking 2048 OS fds:
        // fill table near limit using cloned Arc<File>, then open 1 more -> EMFILE.
        let (f_id, _, _) = fuse_lookup(&fs_dev, m0, "f").unwrap();
        let open_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_OPEN, 90, f_id, &[0u8; 8]), 4096)
            .unwrap();
        let real_fh = read_u64_le(&open_rep, 16);
        {
            let mut t = fs_dev.tables.write().unwrap();
            let proto = t.files.get(&real_fh).unwrap().clone();
            for i in 1..fs_dev.limits.max_open_files {
                t.files.insert(10_000 + i as u64, proto.clone());
            }
        }
        let quota_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_OPEN, 91, f_id, &[0u8; 8]), 4096)
            .unwrap();
        assert_eq!(reply_header(&quota_rep).1, -LINUX_EMFILE);
    }

    #[test]
    fn multithreaded_concurrent_fuse_and_virtqueue_worker_pool() {
        let dir = TempDir::new("concurrent");
        let fs_dev = VirtioFs::new(
            DEFAULT_FS_TAG,
            &[ShareConfig {
                name: "m0".to_string(),
                path: dir.path().to_path_buf(),
                read_only: false,
            }],
        )
        .unwrap();
        fuse_init(&fs_dev);
        let (m0, _, _) = fuse_lookup(&fs_dev, FUSE_ROOT_ID, "m0").unwrap();

        // Spawn 8 host threads hammering concurrent create/write/read/readdir/unlink/forget.
        let mut handles = Vec::new();
        for tid in 0..8usize {
            let fs_c = Arc::clone(&fs_dev);
            handles.push(thread::spawn(move || {
                for iter in 0..40usize {
                    let name = format!("t{tid}_f{iter}");
                    let mut cbody = vec![0u8; 16];
                    cbody[0..4]
                        .copy_from_slice(&(FUSE_O_RDWR | FUSE_O_CREAT | FUSE_O_EXCL).to_le_bytes());
                    cbody[4..8].copy_from_slice(&(libc::S_IFREG as u32 | 0o644).to_le_bytes());
                    cbody.extend_from_slice(name.as_bytes());
                    cbody.push(0);
                    let crep = fs_c
                        .dispatch_fuse_bytes(&build_req(FUSE_OP_CREATE, (tid * 1000 + iter) as u64, m0, &cbody), 4096)
                        .unwrap();
                    assert_eq!(reply_header(&crep).1, 0);
                    let nid = read_u64_le(&crep, 16);
                    let fh = read_u64_le(&crep, 16 + FUSE_ENTRY_OUT_LEN);

                    let payload = format!("data-{tid}-{iter}");
                    let mut wbody = vec![0u8; 40];
                    wbody[0..8].copy_from_slice(&fh.to_le_bytes());
                    wbody[16..20].copy_from_slice(&(payload.len() as u32).to_le_bytes());
                    wbody.extend_from_slice(payload.as_bytes());
                    let wrep = fs_c
                        .dispatch_fuse_bytes(&build_req(FUSE_OP_WRITE, 1, nid, &wbody), 4096)
                        .unwrap();
                    assert_eq!(reply_header(&wrep).1, 0);

                    let mut rbody = vec![0u8; 40];
                    rbody[0..8].copy_from_slice(&fh.to_le_bytes());
                    rbody[16..20].copy_from_slice(&(payload.len() as u32).to_le_bytes());
                    let rrep = fs_c
                        .dispatch_fuse_bytes(&build_req(FUSE_OP_READ, 2, nid, &rbody), 4096)
                        .unwrap();
                    assert_eq!(reply_header(&rrep).1, 0);
                    assert_eq!(&rrep[FUSE_OUT_HEADER_LEN..], payload.as_bytes());

                    let mut rel = vec![0u8; 24];
                    rel[0..8].copy_from_slice(&fh.to_le_bytes());
                    fs_c.dispatch_fuse_bytes(&build_req(FUSE_OP_RELEASE, 3, nid, &rel), 4096)
                        .unwrap();
                    let mut unl = name.into_bytes();
                    unl.push(0);
                    let urep = fs_c
                        .dispatch_fuse_bytes(&build_req(FUSE_OP_UNLINK, 4, m0, &unl), 4096)
                        .unwrap();
                    assert_eq!(reply_header(&urep).1, 0);
                    fs_c.dispatch_fuse_bytes(
                        &build_req(FUSE_OP_FORGET, 5, nid, &1u64.to_le_bytes()),
                        0,
                    );
                }
            }));
        }
        for h in handles {
            h.join().unwrap();
        }
        // Zero leaked file handles or child inodes!
        assert!(fs_dev.tables.read().unwrap().files.is_empty());
        assert_eq!(fs_dev.tables.read().unwrap().inodes.len(), 1); // only share root m0

        // Also test async worker pool round-trip through VirtioPciDevice + VirtQueue!
        let mem = Arc::new(GuestMemory::from_vec(RAM_BASE, vec![0u8; 256 * 1024]));
        let backend = MockSpiBackend::new();
        let mux = SpiMux::new(backend.clone());
        let vpci = Arc::new(VirtioPciDevice::new(
            Box::new(VirtioFsAdapter(Arc::clone(&fs_dev))),
            Arc::clone(&mem),
            mux.line(5, 3),
        ));
        fs_dev.start_workers(Arc::clone(&vpci), Arc::clone(&mem)).unwrap();

        // Negotiate VirtIO 1.0 + enable queue 1
        vpci.bar_write(0x08, &1u32.to_le_bytes()); // driver_feature_select = 1
        vpci.bar_write(0x0c, &1u32.to_le_bytes()); // VIRTIO_F_VERSION_1
        vpci.bar_write(0x14, &[1 | 2 | 8]); // ACK | DRIVER | FEATURES_OK
        vpci.bar_write(0x16, &1u16.to_le_bytes()); // queue_select = 1
        vpci.bar_write(0x18, &16u16.to_le_bytes()); // queue_size = 16
        let desc = RAM_BASE + 0x1000;
        let avail = RAM_BASE + 0x2000;
        let used = RAM_BASE + 0x3000;
        vpci.bar_write(0x20, &desc.to_le_bytes());
        vpci.bar_write(0x28, &avail.to_le_bytes());
        vpci.bar_write(0x30, &used.to_le_bytes());
        vpci.bar_write(0x1c, &1u16.to_le_bytes()); // queue_enable = 1
        vpci.bar_write(0x14, &[1 | 2 | 8 | 4]); // DRIVER_OK

        // Submit FUSE_STATFS on queue 1
        let req_gpa = RAM_BASE + 0x4000;
        let rep_gpa = RAM_BASE + 0x5000;
        let req = build_req(FUSE_OP_STATFS, 555, FUSE_ROOT_ID, &[]);
        mem.write_slice(req_gpa, &req).unwrap();
        let mut d0 = [0u8; 16];
        d0[0..8].copy_from_slice(&req_gpa.to_le_bytes());
        d0[8..12].copy_from_slice(&(req.len() as u32).to_le_bytes());
        d0[12..14].copy_from_slice(&VRING_DESC_F_NEXT.to_le_bytes());
        d0[14..16].copy_from_slice(&1u16.to_le_bytes());
        mem.write_slice(desc, &d0).unwrap();
        let mut d1 = [0u8; 16];
        d1[0..8].copy_from_slice(&rep_gpa.to_le_bytes());
        d1[8..12].copy_from_slice(&256u32.to_le_bytes());
        d1[12..14].copy_from_slice(&VRING_DESC_F_WRITE.to_le_bytes());
        mem.write_slice(desc + 16, &d1).unwrap();
        mem.store_u16_le(avail + 4, 0, Ordering::Release).unwrap();
        mem.store_u16_le(avail + 2, 1, Ordering::Release).unwrap();

        // Kick queue 1 (NOTIFY_OFF + 4 * 1)
        vpci.bar_write(0x3004, &1u16.to_le_bytes());

        // Wait for worker completion
        let mut completed = false;
        for _ in 0..500 {
            if mem.load_u16_le(used + 2, Ordering::Acquire).unwrap() == 1 {
                completed = true;
                break;
            }
            thread::sleep(std::time::Duration::from_millis(2));
        }
        assert!(completed, "async virtio-fs worker did not complete STATFS");
        assert!(backend.level(5));
        let mut rep = [0u8; FUSE_OUT_HEADER_LEN + FUSE_STATFS_OUT_LEN];
        mem.read_slice(rep_gpa, &mut rep).unwrap();
        assert_eq!(
            reply_header(&rep),
            ((FUSE_OUT_HEADER_LEN + FUSE_STATFS_OUT_LEN) as u32, 0, 555)
        );
        fs_dev.shutdown();
    }

    #[test]
    fn readdir_readdirplus_xattr_and_rename_symlink_race() {
        let base = TempDir::new("dir_xattr_race");
        let share = base.path().join("share");
        let outside = base.path().join("outside");
        fs::create_dir_all(share.join("sub")).unwrap();
        fs::create_dir_all(&outside).unwrap();
        fs::write(share.join("sub").join("inside.txt"), b"ok").unwrap();
        fs::write(outside.join("inside.txt"), b"SECRET_OUTSIDE_DATA").unwrap();
        for i in 0..20 {
            fs::write(share.join(format!("item_{i:02}.txt")), format!("v{i}")).unwrap();
        }

        let fs_dev = VirtioFs::new(
            DEFAULT_FS_TAG,
            &[ShareConfig {
                name: "m0".to_string(),
                path: share.clone(),
                read_only: false,
            }],
        )
        .unwrap();
        fuse_init(&fs_dev);

        // Virtual root READDIR lists m0
        let od = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_OPENDIR, 1, FUSE_ROOT_ID, &[0u8; 8]), 4096)
            .unwrap();
        assert_eq!(reply_header(&od).1, 0);
        let root_dh = read_u64_le(&od, 16);
        let mut rd_in = vec![0u8; 40];
        rd_in[0..8].copy_from_slice(&root_dh.to_le_bytes());
        rd_in[16..20].copy_from_slice(&4096u32.to_le_bytes());
        let rd = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_READDIR, 2, FUSE_ROOT_ID, &rd_in), 4096)
            .unwrap();
        assert_eq!(reply_header(&rd).1, 0);
        assert!(rd.len() > FUSE_OUT_HEADER_LEN);
        let mut names = Vec::new();
        let mut pos = FUSE_OUT_HEADER_LEN;
        while pos + FUSE_DIRENT_HDR_LEN <= rd.len() {
            let namelen = read_u32_le(&rd, pos + 16) as usize;
            let name = std::str::from_utf8(&rd[pos + 24..pos + 24 + namelen]).unwrap();
            names.push(name.to_string());
            pos += fuse_align8(FUSE_DIRENT_HDR_LEN + namelen);
        }
        assert!(names.contains(&"m0".to_string()));
        let mut rel = vec![0u8; 24];
        rel[0..8].copy_from_slice(&root_dh.to_le_bytes());
        fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_RELEASEDIR, 3, FUSE_ROOT_ID, &rel), 4096)
            .unwrap();

        // Host share READDIR pagination with small buffer
        let (m0, _, _) = fuse_lookup(&fs_dev, FUSE_ROOT_ID, "m0").unwrap();
        let od = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_OPENDIR, 4, m0, &[0u8; 8]), 4096)
            .unwrap();
        let dh = read_u64_le(&od, 16);
        let mut seen = Vec::new();
        let mut off = 0u64;
        loop {
            let mut body = vec![0u8; 40];
            body[0..8].copy_from_slice(&dh.to_le_bytes());
            body[8..16].copy_from_slice(&off.to_le_bytes());
            body[16..20].copy_from_slice(&128u32.to_le_bytes());
            let rep = fs_dev
                .dispatch_fuse_bytes(&build_req(FUSE_OP_READDIR, 5, m0, &body), 4096)
                .unwrap();
            assert_eq!(reply_header(&rep).1, 0);
            if rep.len() == FUSE_OUT_HEADER_LEN {
                break;
            }
            let mut p = FUSE_OUT_HEADER_LEN;
            while p + FUSE_DIRENT_HDR_LEN <= rep.len() {
                off = read_u64_le(&rep, p + 8);
                let nlen = read_u32_le(&rep, p + 16) as usize;
                seen.push(String::from_utf8(rep[p + 24..p + 24 + nlen].to_vec()).unwrap());
                p += fuse_align8(FUSE_DIRENT_HDR_LEN + nlen);
            }
        }
        for i in 0..20 {
            assert!(seen.contains(&format!("item_{i:02}.txt")));
        }
        rel[0..8].copy_from_slice(&dh.to_le_bytes());
        fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_RELEASEDIR, 6, m0, &rel), 4096)
            .unwrap();

        // Xattr round-trip (skip cleanly if host tmpfs lacks user.* xattr support)
        let (f0, _, _) = fuse_lookup(&fs_dev, m0, "item_00.txt").unwrap();
        let mut sx = vec![0u8; 16];
        sx[0..4].copy_from_slice(&5u32.to_le_bytes());
        sx[4..8].copy_from_slice(&FUSE_XATTR_CREATE.to_le_bytes());
        sx.extend_from_slice(b"com.kido.test\0hello");
        let sx_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_SETXATTR, 7, f0, &sx), 4096)
            .unwrap();
        let sx_err = reply_header(&sx_rep).1;
        if sx_err == 0 {
            // Duplicate CREATE fails with EEXIST
            let dup = fs_dev
                .dispatch_fuse_bytes(&build_req(FUSE_OP_SETXATTR, 8, f0, &sx), 4096)
                .unwrap();
            assert_eq!(reply_header(&dup).1, -LINUX_EEXIST);

            // Size probe
            let mut gx = vec![0u8; 8];
            gx.extend_from_slice(b"com.kido.test\0");
            let gsz = fs_dev
                .dispatch_fuse_bytes(&build_req(FUSE_OP_GETXATTR, 9, f0, &gx), 4096)
                .unwrap();
            assert_eq!(reply_header(&gsz).1, 0);
            assert_eq!(read_u32_le(&gsz, 16), 5);

            // Value read
            gx[0..4].copy_from_slice(&64u32.to_le_bytes());
            let gval = fs_dev
                .dispatch_fuse_bytes(&build_req(FUSE_OP_GETXATTR, 10, f0, &gx), 4096)
                .unwrap();
            assert_eq!(reply_header(&gval).1, 0);
            assert_eq!(&gval[FUSE_OUT_HEADER_LEN..], b"hello");

            // LISTXATTR contains com.kido.test\0
            let mut lx = vec![0u8; 8];
            lx[0..4].copy_from_slice(&1024u32.to_le_bytes());
            let lval = fs_dev
                .dispatch_fuse_bytes(&build_req(FUSE_OP_LISTXATTR, 11, f0, &lx), 4096)
                .unwrap();
            assert_eq!(reply_header(&lval).1, 0);
            assert!(lval[FUSE_OUT_HEADER_LEN..]
                .split(|&b| b == 0)
                .any(|e| e == b"com.kido.test"));

            // REMOVEXATTR
            let rm = fs_dev
                .dispatch_fuse_bytes(&build_req(FUSE_OP_REMOVEXATTR, 12, f0, b"com.kido.test\0"), 4096)
                .unwrap();
            assert_eq!(reply_header(&rm).1, 0);
            let gone = fs_dev
                .dispatch_fuse_bytes(&build_req(FUSE_OP_GETXATTR, 13, f0, &gx), 4096)
                .unwrap();
            assert_eq!(reply_header(&gone).1, -LINUX_ENODATA);
        } else {
            assert_eq!(sx_err, -LINUX_EOPNOTSUPP);
        }

        // Symlink-swap race test: host repeatedly swaps share/sub <-> symlink(outside).
        // Guest lookup+read must NEVER observe SECRET_OUTSIDE_DATA (size 19).
        let stop = Arc::new(AtomicBool::new(false));
        let stop_bg = Arc::clone(&stop);
        let sub_path = share.join("sub");
        let saved_path = share.join("sub_saved");
        let outside_bg = outside.clone();
        let racer = thread::spawn(move || {
            while !stop_bg.load(Ordering::Relaxed) {
                let _ = fs::rename(&sub_path, &saved_path);
                let _ = symlink(&outside_bg, &sub_path);
                let _ = fs::remove_file(&sub_path);
                let _ = fs::rename(&saved_path, &sub_path);
            }
        });
        for _ in 0..2000 {
            if let Ok((sub_id, mode, _)) = fuse_lookup(&fs_dev, m0, "sub") {
                if (mode & (libc::S_IFMT as u32)) as libc::mode_t == libc::S_IFDIR {
                    if let Ok((fid, _, size)) = fuse_lookup(&fs_dev, sub_id, "inside.txt") {
                        assert_eq!(size, 2, "escaped to outside file during symlink race!");
                        fs_dev.dispatch_fuse_bytes(
                            &build_req(FUSE_OP_FORGET, 0, fid, &1u64.to_le_bytes()),
                            0,
                        );
                    }
                }
                fs_dev.dispatch_fuse_bytes(
                    &build_req(FUSE_OP_FORGET, 0, sub_id, &1u64.to_le_bytes()),
                    0,
                );
            }
        }
        stop.store(true, Ordering::Relaxed);
        racer.join().unwrap();
    }

    #[test]
    fn audit_regressions_fifo_create_limits_reset_xattr_and_confinement() {
        let dir = TempDir::new("audit_reg");
        let share = dir.path().join("share");
        let ro_share = dir.path().join("ro");
        fs::create_dir(&share).unwrap();
        fs::create_dir(&ro_share).unwrap();
        fs::write(share.join("existing.txt"), b"abc").unwrap();

        // H2: dynamic FD quota always reserves headroom for non-FS subsystems.
        let lim1024 = FsLimits::from_nofile(1024);
        assert!(lim1024.max_inodes + lim1024.max_open_files + lim1024.max_open_dirs <= 512);
        let lim_big = FsLimits::from_nofile(131_072);
        assert_eq!(lim_big.max_inodes, MAX_INODES);
        assert_eq!(lim_big.max_open_files, MAX_OPEN_FILES);
        assert_eq!(lim_big.max_open_dirs, MAX_OPEN_DIRS);

        // H3 (Linux): open_share_root rejects paths with an intermediate symlink.
        #[cfg(target_os = "linux")]
        {
            let real_parent = dir.path().join("real_parent");
            let real_target = real_parent.join("target");
            fs::create_dir_all(&real_target).unwrap();
            let sym_parent = dir.path().join("sym_parent");
            symlink(&real_parent, &sym_parent).unwrap();
            let via_sym = sym_parent.join("target");
            assert!(
                open_share_root(&via_sym).is_err(),
                "open_share_root followed an intermediate symlink"
            );
        }

        let fs_dev = VirtioFs::new(
            DEFAULT_FS_TAG,
            &[
                ShareConfig {
                    name: "m0".to_string(),
                    path: share.clone(),
                    read_only: false,
                },
                ShareConfig {
                    name: "m1".to_string(),
                    path: ro_share.clone(),
                    read_only: true,
                },
            ],
        )
        .unwrap();
        fuse_init(&fs_dev);
        let (m0, _, _) = fuse_lookup(&fs_dev, FUSE_ROOT_ID, "m0").unwrap();
        let (m1, _, _) = fuse_lookup(&fs_dev, FUSE_ROOT_ID, "m1").unwrap();

        // C1: FUSE_MKNOD(S_IFIFO) + FUSE_CREATE(O_WRONLY without O_EXCL) must NOT block!
        let mut fifo_mknod = vec![0u8; 16];
        fifo_mknod[0..4].copy_from_slice(&((libc::S_IFIFO as u32) | 0o600).to_le_bytes());
        fifo_mknod.extend_from_slice(b"myfifo\0");
        let mk_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_MKNOD, 1, m0, &fifo_mknod), 4096)
            .unwrap();
        assert_eq!(reply_header(&mk_rep).1, 0);

        let mut fifo_create = vec![0u8; 16];
        fifo_create[0..4].copy_from_slice(&(FUSE_O_WRONLY | FUSE_O_CREAT).to_le_bytes());
        fifo_create[4..8].copy_from_slice(&0o644u32.to_le_bytes());
        fifo_create.extend_from_slice(b"myfifo\0");
        let cr_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_CREATE, 2, m0, &fifo_create), 4096)
            .unwrap();
        assert_eq!(reply_header(&cr_rep).1, -LINUX_ENXIO);

        // M3: O_RDONLY | O_TRUNC -> EINVAL; O_RDONLY | O_APPEND -> writable=false.
        let (ex_id, _, _) = fuse_lookup(&fs_dev, m0, "existing.txt").unwrap();
        let mut open_rd_trunc = [0u8; 8];
        open_rd_trunc[0..4].copy_from_slice(&FUSE_O_TRUNC.to_le_bytes());
        let rd_trunc_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_OPEN, 3, ex_id, &open_rd_trunc), 4096)
            .unwrap();
        assert_eq!(reply_header(&rd_trunc_rep).1, -LINUX_EINVAL);

        let mut open_rd_app = [0u8; 8];
        open_rd_app[0..4].copy_from_slice(&FUSE_O_APPEND.to_le_bytes());
        let rd_app_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_OPEN, 4, ex_id, &open_rd_app), 4096)
            .unwrap();
        assert_eq!(reply_header(&rd_app_rep).1, 0);
        let rd_app_fh = read_u64_le(&rd_app_rep, 16);
        assert!(!fs_dev.tables.read().unwrap().files.get(&rd_app_fh).unwrap().writable);

        // M4: GETXATTR / LISTXATTR with size > 0 and max_payload == 0 -> ERANGE.
        let mut gx = vec![0u8; 8];
        gx[0..4].copy_from_slice(&64u32.to_le_bytes());
        gx.extend_from_slice(b"com.kido.a\0");
        let gx_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_GETXATTR, 5, ex_id, &gx), FUSE_OUT_HEADER_LEN)
            .unwrap();
        assert_eq!(reply_header(&gx_rep).1, -LINUX_ERANGE);
        let mut lx = vec![0u8; 8];
        lx[0..4].copy_from_slice(&64u32.to_le_bytes());
        let lx_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_LISTXATTR, 6, ex_id, &lx), FUSE_OUT_HEADER_LEN)
            .unwrap();
        assert_eq!(reply_header(&lx_rep).1, -LINUX_ERANGE);

        // M6: FUSE_READ offset + size overflow -> EINVAL.
        let mut rd_ovf = [0u8; 24];
        rd_ovf[0..8].copy_from_slice(&rd_app_fh.to_le_bytes());
        rd_ovf[8..16].copy_from_slice(&((i64::MAX as u64) - 4).to_le_bytes());
        rd_ovf[16..20].copy_from_slice(&16u32.to_le_bytes());
        let ovf_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_READ, 7, ex_id, &rd_ovf), 4096)
            .unwrap();
        assert_eq!(reply_header(&ovf_rep).1, -LINUX_EINVAL);

        // M8: SYNCFS on RO share returns Ok immediately.
        let sync_ro = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_SYNCFS, 8, m1, &[]), 4096)
            .unwrap();
        assert_eq!(reply_header(&sync_ro).1, 0);

        // L4: SETATTR with FATTR_FH and bogus fh -> EBADF.
        let mut bad_sa = [0u8; 88];
        bad_sa[0..4].copy_from_slice(&(FATTR_FH | FATTR_MODE).to_le_bytes());
        bad_sa[8..16].copy_from_slice(&999_999u64.to_le_bytes());
        bad_sa[68..72].copy_from_slice(&0o644u32.to_le_bytes());
        let bad_sa_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_SETATTR, 9, ex_id, &bad_sa), 4096)
            .unwrap();
        assert_eq!(reply_header(&bad_sa_rep).1, -LINUX_EBADF);

        // L5: Virtual root READDIR with too-small buffer -> EINVAL.
        let od_root = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_OPENDIR, 10, FUSE_ROOT_ID, &[0u8; 8]), 4096)
            .unwrap();
        let root_dh = read_u64_le(&od_root, 16);
        let mut tiny_rd = [0u8; 24];
        tiny_rd[0..8].copy_from_slice(&root_dh.to_le_bytes());
        tiny_rd[16..20].copy_from_slice(&8u32.to_le_bytes());
        let tiny_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_READDIR, 11, FUSE_ROOT_ID, &tiny_rd), 4096)
            .unwrap();
        assert_eq!(reply_header(&tiny_rep).1, -LINUX_EINVAL);

        // L2: Share root READDIR rewrites ".." d_ino to m0's own inode (not host parent's).
        let od_m0 = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_OPENDIR, 12, m0, &[0u8; 8]), 4096)
            .unwrap();
        let m0_dh = read_u64_le(&od_m0, 16);
        let mut m0_rd = [0u8; 24];
        m0_rd[0..8].copy_from_slice(&m0_dh.to_le_bytes());
        m0_rd[16..20].copy_from_slice(&4096u32.to_le_bytes());
        let m0_rep = fs_dev
            .dispatch_fuse_bytes(&build_req(FUSE_OP_READDIR, 13, m0, &m0_rd), 4096)
            .unwrap();
        assert_eq!(reply_header(&m0_rep).1, 0);
        let mut pos = FUSE_OUT_HEADER_LEN;
        let mut dotdot_ino = None;
        while pos + FUSE_DIRENT_HDR_LEN <= m0_rep.len() {
            let ino = read_u64_le(&m0_rep, pos);
            let namelen = read_u32_le(&m0_rep, pos + 16) as usize;
            let name = &m0_rep[pos + 24..pos + 24 + namelen];
            if name == b".." {
                dotdot_ino = Some(ino);
            }
            pos += fuse_align8(FUSE_DIRENT_HDR_LEN + namelen);
        }
        assert_eq!(dotdot_ino, Some(fs_dev.shares[0].root_ino));

        // H4: VirtioFsAdapter::reset bumps session_epoch and drops pending jobs.
        let epoch_before = fs_dev.current_epoch();
        VirtioFsAdapter(Arc::clone(&fs_dev)).reset();
        assert_ne!(fs_dev.current_epoch(), epoch_before);
    }
}
