use std::io::{self, Read, Write};
use std::net::Shutdown;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::mpsc::{self, Receiver, SyncSender};
use std::sync::{Arc, Mutex, Weak};
use std::thread::{self, JoinHandle};
#[cfg(target_os = "linux")]
use std::time::Duration;

use crate::memory::GuestMemory;
use crate::pci::PCI_DEVICE_VIRTIO_METAL;
use crate::virtio::pci::VirtioPciDevice;
use crate::virtio::queue::{QueueError, VirtQueue};
use crate::virtio::{
    QueueOutcome, VirtioDevice, VIRTIO_F_VERSION_1, VIRTIO_RING_F_INDIRECT_DESC,
};

pub const KMTL_MAGIC: u32 = 0x4B4D_544C;
pub const KMTL_PROTO_VERSION: u32 = 1;
pub const KMTL_VIRTIO_DEVICE_ID: u16 = 0x0050;
pub const KMTL_PCI_CLASS_CODE: u32 = 0x030200;

pub const KMTL_OP_GET_DEVICE_INFO: u32 = 1;
pub const KMTL_OP_COMMIT_COMMAND_BUFFER: u32 = 40;

pub const KMTL_FEAT_DMA_COHERENT: u32 = 1 << 4;

pub const KMTL_FRAME_GUEST_RPC: u8 = 0;
pub const KMTL_FRAME_VMM_RESET: u8 = 1;
pub const KMTL_FRAME_GUEST_RPC_PREFLUSHED: u8 = 2;

pub trait ShmDirtySync: Send + Sync {
    /// Clean and invalidate all guest-dirtied BAR4 SHM pages to the ARM64 Point
    /// of Coherency and re-arm dirty tracking. Returns true iff dirty tracking
    /// is active and every guest write to SHM is now coherent at PoC.
    fn sync_dirty_to_poc(&self) -> bool;
}

pub const KMTL_ERR_NOMEM: i32 = -12;
pub const KMTL_ERR_INVAL: i32 = -22;
pub const KMTL_ERR_IO: i32 = -5;
pub const KMTL_ERR_UNSUPP: i32 = -95;

pub const KMTL_REQ_HDR_LEN: usize = 40;
pub const KMTL_RESP_HDR_LEN: usize = 32;
pub const KMTL_DEV_CFG_LEN: usize = 64;
pub const KMTL_DEV_INFO_LEN: usize = 256;

pub const KMTL_MAX_IN_PAYLOAD: usize = 64 * 1024 * 1024;
pub const KMTL_MAX_OUT_PAYLOAD: usize = 1024 * 1024;
pub const KMTL_MAX_INFLIGHT_BYTES: usize = 64 * 1024 * 1024;
pub const DEFAULT_LINUX_GPU_MIB: usize = 512;
pub const MIN_GPU_MIB: usize = 64;
pub const MAX_GPU_MIB: usize = 131_072;

const QUEUE_MAX_SIZE: u16 = 128;
const MAX_QUEUED_JOBS: usize = 64;

pub struct ShmBacking {
    ptr: usize,
    size: u64,
    #[cfg(target_os = "linux")]
    fd: i32,
}

impl ShmBacking {
    pub fn host_ptr(&self) -> *mut u8 {
        self.ptr as *mut u8
    }

    pub fn size(&self) -> u64 {
        self.size
    }
}

impl Drop for ShmBacking {
    fn drop(&mut self) {
        if self.ptr != 0 && self.size > 0 {
            unsafe {
                libc::munmap(self.ptr as *mut libc::c_void, self.size as usize);
            }
            self.ptr = 0;
        }
        #[cfg(target_os = "linux")]
        if self.fd >= 0 {
            unsafe {
                libc::close(self.fd);
            }
            self.fd = -1;
        }
    }
}

struct InflightBudgetGuard {
    counter: Arc<AtomicUsize>,
    bytes: usize,
}

impl Drop for InflightBudgetGuard {
    fn drop(&mut self) {
        self.counter.fetch_sub(self.bytes, Ordering::AcqRel);
    }
}

enum MetalJob {
    Rpc {
        head_index: u16,
        expected_gen: u64,
        req_hdr: [u8; KMTL_REQ_HDR_LEN],
        in_payload: Vec<u8>,
        out_segs: Vec<(u64, u32)>,
        out_cap: u32,
        _budget: InflightBudgetGuard,
    },
    WakeReset,
}

fn read_u32_le(b: &[u8], off: usize) -> u32 {
    u32::from_le_bytes(b[off..off + 4].try_into().unwrap())
}

fn read_u64_le(b: &[u8], off: usize) -> u64 {
    u64::from_le_bytes(b[off..off + 8].try_into().unwrap())
}

pub fn make_device_config_from_info(info: &[u8], shm_size: u64) -> Result<[u8; KMTL_DEV_CFG_LEN], String> {
    if info.len() < KMTL_DEV_INFO_LEN {
        return Err(format!(
            "worker device info too short ({} bytes, expected {KMTL_DEV_INFO_LEN})",
            info.len()
        ));
    }
    let registry_id = read_u64_le(info, 128);
    let max_buffer_length = read_u64_le(info, 136).min(shm_size);
    let rec_ws = read_u64_le(info, 144).min(shm_size);
    let max_tg_x = read_u32_le(info, 168);
    let max_tg_y = read_u32_le(info, 172);
    let max_tg_z = read_u32_le(info, 176);
    let max_tg_mem = read_u32_le(info, 180);
    let host_page_size = read_u32_le(info, 184).max(16384);
    let flags = read_u32_le(info, 188);

    let mut cfg = [0u8; KMTL_DEV_CFG_LEN];
    cfg[0..4].copy_from_slice(&KMTL_MAGIC.to_le_bytes());
    cfg[4..8].copy_from_slice(&KMTL_PROTO_VERSION.to_le_bytes());
    cfg[8..12].copy_from_slice(&host_page_size.to_le_bytes());
    cfg[12..16].copy_from_slice(&flags.to_le_bytes());
    cfg[16..24].copy_from_slice(&max_buffer_length.to_le_bytes());
    cfg[24..32].copy_from_slice(&rec_ws.to_le_bytes());
    cfg[32..40].copy_from_slice(&shm_size.to_le_bytes());
    cfg[40..44].copy_from_slice(&max_tg_x.to_le_bytes());
    cfg[44..48].copy_from_slice(&max_tg_y.to_le_bytes());
    cfg[48..52].copy_from_slice(&max_tg_z.to_le_bytes());
    cfg[52..56].copy_from_slice(&max_tg_mem.to_le_bytes());
    cfg[56..64].copy_from_slice(&registry_id.to_le_bytes());
    Ok(cfg)
}

fn make_error_resp(status: i32) -> [u8; KMTL_RESP_HDR_LEN] {
    let mut resp = [0u8; KMTL_RESP_HDR_LEN];
    resp[0..4].copy_from_slice(&status.to_le_bytes());
    resp
}

fn exchange_frame(
    sock: &mut UnixStream,
    kind: u8,
    req_hdr: &[u8; KMTL_REQ_HDR_LEN],
    in_payload: &[u8],
    out_cap: u32,
) -> io::Result<([u8; KMTL_RESP_HDR_LEN], Vec<u8>)> {
    let mut frame_hdr = [0u8; 1 + KMTL_REQ_HDR_LEN + 8];
    frame_hdr[0] = kind;
    frame_hdr[1..1 + KMTL_REQ_HDR_LEN].copy_from_slice(req_hdr);
    frame_hdr[1 + KMTL_REQ_HDR_LEN..1 + KMTL_REQ_HDR_LEN + 4]
        .copy_from_slice(&(in_payload.len() as u32).to_le_bytes());
    frame_hdr[1 + KMTL_REQ_HDR_LEN + 4..1 + KMTL_REQ_HDR_LEN + 8]
        .copy_from_slice(&out_cap.to_le_bytes());
    sock.write_all(&frame_hdr)?;
    if !in_payload.is_empty() {
        sock.write_all(in_payload)?;
    }

    let mut resp_hdr = [0u8; KMTL_RESP_HDR_LEN];
    sock.read_exact(&mut resp_hdr)?;
    let extra_len = u32::from_le_bytes(resp_hdr[4..8].try_into().unwrap());
    if extra_len > out_cap || extra_len as usize > KMTL_MAX_OUT_PAYLOAD {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!("worker extra_len {extra_len} exceeds cap {out_cap}"),
        ));
    }
    let mut extra = vec![0u8; extra_len as usize];
    if extra_len > 0 {
        sock.read_exact(&mut extra)?;
    }
    Ok((resp_hdr, extra))
}

fn scatter_write(mem: &GuestMemory, segs: &[(u64, u32)], data: &[u8]) -> u32 {
    let mut written = 0usize;
    for &(gpa, len) in segs {
        if written >= data.len() {
            break;
        }
        let chunk = (len as usize).min(data.len() - written);
        if chunk > 0 {
            if mem.write_slice(gpa, &data[written..written + chunk]).is_err() {
                break;
            }
            written += chunk;
        }
    }
    written as u32
}

fn is_executable_file(p: &Path) -> bool {
    std::fs::metadata(p).is_ok_and(|m| m.is_file() && (m.permissions().mode() & 0o111) != 0)
}

pub fn resolve_vk_worker_path(explicit: Option<&Path>) -> Result<PathBuf, String> {
    if let Some(p) = explicit {
        if is_executable_file(p) {
            return Ok(p.to_path_buf());
        }
        return Err(format!("--vk-worker '{}' is not an executable file", p.display()));
    }
    if let Ok(env_p) = std::env::var("KIDO_VK_WORKER") {
        let p = PathBuf::from(&env_p);
        if is_executable_file(&p) {
            return Ok(p);
        }
        return Err(format!("KIDO_VK_WORKER '{}' is not an executable file", p.display()));
    }
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            let candidates = [
                dir.join("kido-vk-worker"),
                dir.join("..").join("libexec").join("kido").join("kido-vk-worker"),
            ];
            for c in candidates {
                if is_executable_file(&c) {
                    return Ok(c);
                }
            }
        }
    }
    if let Ok(path_var) = std::env::var("PATH") {
        for dir in std::env::split_paths(&path_var) {
            let c = dir.join("kido-vk-worker");
            if is_executable_file(&c) {
                return Ok(c);
            }
        }
    }
    Err("kido-vk-worker not found (install next to kido-vmm, in PATH, or pass --vk-worker)".to_string())
}

pub struct VirtioMetal {
    dev_config: [u8; KMTL_DEV_CFG_LEN],
    shm: Arc<ShmBacking>,
    #[cfg(target_os = "linux")]
    child: Mutex<Option<std::process::Child>>,
    sock: Mutex<Option<UnixStream>>,
    sock_shutdown: Mutex<Option<UnixStream>>,
    shutdown_flag: Arc<AtomicBool>,
    desired_reset_gen: Arc<AtomicU64>,
    inflight_bytes: Arc<AtomicUsize>,
    vpci: Mutex<Option<Weak<VirtioPciDevice>>>,
    tx: Mutex<Option<SyncSender<MetalJob>>>,
    worker: Mutex<Option<JoinHandle<()>>>,
}

impl VirtioMetal {
    #[cfg(target_os = "linux")]
    pub fn launch(shm_bytes: u64, vk_worker_hint: Option<&Path>) -> Result<Arc<Self>, String> {
        use std::os::unix::io::FromRawFd;
        use std::os::unix::process::CommandExt;
        use std::process::{Command, Stdio};

        if shm_bytes < (MIN_GPU_MIB as u64) << 20 || (shm_bytes & 0x3fff) != 0 {
            return Err(format!("invalid GPU SHM size {shm_bytes:#x}"));
        }
        let worker_path = resolve_vk_worker_path(vk_worker_hint)?;

        let shm_fd = unsafe {
            libc::syscall(
                libc::SYS_memfd_create,
                c"kido-gpu-shm".as_ptr(),
                libc::MFD_CLOEXEC | libc::MFD_ALLOW_SEALING,
            ) as i32
        };
        if shm_fd < 0 {
            return Err(format!(
                "memfd_create(kido-gpu-shm) failed: {}",
                io::Error::last_os_error()
            ));
        }
        if unsafe { libc::ftruncate(shm_fd, shm_bytes as libc::off_t) } != 0 {
            let err = io::Error::last_os_error();
            unsafe { libc::close(shm_fd) };
            return Err(format!("ftruncate GPU SHM ({shm_bytes} B) failed: {err}"));
        }
        let _ = unsafe { libc::fcntl(shm_fd, libc::F_ADD_SEALS, libc::F_SEAL_SHRINK) };

        let mapped = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                shm_bytes as usize,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_SHARED,
                shm_fd,
                0,
            )
        };
        if mapped == libc::MAP_FAILED {
            let err = io::Error::last_os_error();
            unsafe { libc::close(shm_fd) };
            return Err(format!("mmap GPU SHM ({shm_bytes} B) failed: {err}"));
        }
        let shm = Arc::new(ShmBacking {
            ptr: mapped as usize,
            size: shm_bytes,
            fd: shm_fd,
        });

        let mut sv = [-1i32; 2];
        if unsafe {
            libc::socketpair(
                libc::AF_UNIX,
                libc::SOCK_STREAM | libc::SOCK_CLOEXEC,
                0,
                sv.as_mut_ptr(),
            )
        } != 0
        {
            return Err(format!("socketpair failed: {}", io::Error::last_os_error()));
        }
        let parent_sock_fd = sv[0];
        let child_sock_fd = sv[1];

        let parent_pid = unsafe { libc::getpid() };
        let mut cmd = Command::new(&worker_path);
        cmd.stdin(Stdio::null());
        cmd.args([
            "--sock-fd",
            &child_sock_fd.to_string(),
            "--shm-fd",
            &shm_fd.to_string(),
            "--shm-bytes",
            &shm_bytes.to_string(),
        ]);
        unsafe {
            cmd.pre_exec(move || {
                // Restore default signal mask and SIGPIPE so worker & glslangValidator are killable.
                let mut empty: libc::sigset_t = std::mem::zeroed();
                libc::sigemptyset(&mut empty);
                libc::pthread_sigmask(libc::SIG_SETMASK, &empty, std::ptr::null_mut());
                libc::signal(libc::SIGPIPE, libc::SIG_DFL);
                if libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL, 0, 0, 0) != 0 {
                    return Err(io::Error::last_os_error());
                }
                if libc::getppid() != parent_pid {
                    libc::_exit(1);
                }
                // Clear CLOEXEC directly on the two passed FDs without clobbering low FDs.
                if libc::fcntl(child_sock_fd, libc::F_SETFD, 0) < 0 {
                    return Err(io::Error::last_os_error());
                }
                if libc::fcntl(shm_fd, libc::F_SETFD, 0) < 0 {
                    return Err(io::Error::last_os_error());
                }
                Ok(())
            });
        }

        let child_res = cmd.spawn();
        unsafe { libc::close(child_sock_fd) };
        let mut child = match child_res {
            Ok(c) => c,
            Err(e) => {
                unsafe { libc::close(parent_sock_fd) };
                return Err(format!("spawn '{}': {e}", worker_path.display()));
            }
        };

        let mut parent_sock = unsafe { UnixStream::from_raw_fd(parent_sock_fd) };
        let _ = parent_sock.set_read_timeout(Some(Duration::from_secs(10)));
        let _ = parent_sock.set_write_timeout(Some(Duration::from_secs(10)));

        let mut info_req = [0u8; KMTL_REQ_HDR_LEN];
        info_req[0..4].copy_from_slice(&KMTL_MAGIC.to_le_bytes());
        info_req[4..8].copy_from_slice(&KMTL_PROTO_VERSION.to_le_bytes());
        info_req[8..12].copy_from_slice(&KMTL_OP_GET_DEVICE_INFO.to_le_bytes());

        let dev_config = match exchange_frame(
            &mut parent_sock,
            KMTL_FRAME_GUEST_RPC,
            &info_req,
            &[],
            KMTL_DEV_INFO_LEN as u32,
        ) {
            Ok((resp_hdr, extra)) => {
                let status = i32::from_le_bytes(resp_hdr[0..4].try_into().unwrap());
                if status != 0 {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Err(format!("kido-vk-worker GET_DEVICE_INFO failed (status {status})"));
                }
                match make_device_config_from_info(&extra, shm_bytes) {
                    Ok(cfg) => {
                        let name_end = extra
                            .iter()
                            .take(128)
                            .position(|&b| b == 0)
                            .unwrap_or(128.min(extra.len()));
                        let name = String::from_utf8_lossy(&extra[..name_end]);
                        let coherent = (read_u32_le(&cfg, 12) & KMTL_FEAT_DMA_COHERENT) != 0;
                        eprintln!(
                            "kido-vmm: virtio-metal ready: {} (shm {} MiB, dma-coherent={})",
                            name,
                            shm_bytes >> 20,
                            if coherent { "yes" } else { "no" }
                        );
                        cfg
                    }
                    Err(e) => {
                        let _ = child.kill();
                        let _ = child.wait();
                        return Err(e);
                    }
                }
            }
            Err(e) => {
                let _ = child.kill();
                let _ = child.wait();
                return Err(format!("kido-vk-worker handshake failed: {e}"));
            }
        };

        let _ = parent_sock.set_read_timeout(None);
        let _ = parent_sock.set_write_timeout(None);
        let sock_shutdown = parent_sock.try_clone().ok();

        Ok(Arc::new(Self {
            dev_config,
            shm,
            child: Mutex::new(Some(child)),
            sock: Mutex::new(Some(parent_sock)),
            sock_shutdown: Mutex::new(sock_shutdown),
            shutdown_flag: Arc::new(AtomicBool::new(false)),
            desired_reset_gen: Arc::new(AtomicU64::new(0)),
            inflight_bytes: Arc::new(AtomicUsize::new(0)),
            vpci: Mutex::new(None),
            tx: Mutex::new(None),
            worker: Mutex::new(None),
        }))
    }

    pub fn shm_backing(&self) -> Arc<ShmBacking> {
        Arc::clone(&self.shm)
    }

    pub fn shm_size(&self) -> u64 {
        self.shm.size()
    }

    pub fn is_dma_coherent(&self) -> bool {
        (read_u32_le(&self.dev_config, 12) & KMTL_FEAT_DMA_COHERENT) != 0
    }

    pub fn start_worker(
        self: &Arc<Self>,
        vpci: Arc<VirtioPciDevice>,
        _mem: Arc<GuestMemory>,
        dirty_sync: Option<Arc<dyn ShmDirtySync>>,
    ) -> Result<(), String> {
        let sock = self
            .sock
            .lock()
            .unwrap()
            .take()
            .ok_or_else(|| "VirtioMetal worker already started".to_string())?;
        *self.vpci.lock().unwrap() = Some(Arc::downgrade(&vpci));
        let (tx, rx) = mpsc::sync_channel::<MetalJob>(MAX_QUEUED_JOBS);
        *self.tx.lock().unwrap() = Some(tx);

        let vpci_weak = Arc::downgrade(&vpci);
        let shutdown_flag = Arc::clone(&self.shutdown_flag);
        let desired_reset_gen = Arc::clone(&self.desired_reset_gen);
        let handle = thread::Builder::new()
            .name("metal-rpc".to_string())
            .spawn(move || {
                metal_worker_loop(
                    sock,
                    rx,
                    vpci_weak,
                    shutdown_flag,
                    desired_reset_gen,
                    dirty_sync,
                );
            })
            .map_err(|e| format!("spawn metal-rpc thread: {e}"))?;
        *self.worker.lock().unwrap() = Some(handle);
        Ok(())
    }

    pub fn shutdown(&self) {
        self.shutdown_flag.store(true, Ordering::Release);
        drop(self.tx.lock().unwrap().take());
        if let Some(s) = self.sock_shutdown.lock().unwrap().take() {
            let _ = s.shutdown(Shutdown::Both);
        }
        #[cfg(target_os = "linux")]
        if let Some(mut child) = self.child.lock().unwrap().take() {
            let _ = child.kill();
            let _ = child.wait();
        }
        if let Some(h) = self.worker.lock().unwrap().take() {
            let _ = h.join();
        }
    }
}

impl Drop for VirtioMetal {
    fn drop(&mut self) {
        self.shutdown();
    }
}

fn metal_worker_loop(
    mut sock: UnixStream,
    rx: Receiver<MetalJob>,
    vpci_weak: Weak<VirtioPciDevice>,
    shutdown_flag: Arc<AtomicBool>,
    desired_reset_gen: Arc<AtomicU64>,
    dirty_sync: Option<Arc<dyn ShmDirtySync>>,
) {
    let mut sock_dead = false;
    let mut applied_reset_gen = 0u64;

    let sync_reset = |sock: &mut UnixStream, sock_dead: &mut bool, applied: &mut u64| {
        let want = desired_reset_gen.load(Ordering::Acquire);
        if want > *applied {
            *applied = want;
            if !*sock_dead {
                let mut reset_req = [0u8; KMTL_REQ_HDR_LEN];
                reset_req[0..4].copy_from_slice(&KMTL_MAGIC.to_le_bytes());
                reset_req[4..8].copy_from_slice(&KMTL_PROTO_VERSION.to_le_bytes());
                if exchange_frame(sock, KMTL_FRAME_VMM_RESET, &reset_req, &[], 0).is_err() {
                    *sock_dead = true;
                }
            }
        }
    };

    while let Ok(job) = rx.recv() {
        if shutdown_flag.load(Ordering::Acquire) {
            break;
        }
        sync_reset(&mut sock, &mut sock_dead, &mut applied_reset_gen);
        match job {
            MetalJob::WakeReset => {}
            MetalJob::Rpc {
                head_index,
                expected_gen,
                req_hdr,
                in_payload,
                out_segs,
                out_cap,
                _budget,
            } => {
                let Some(vpci) = vpci_weak.upgrade() else {
                    break;
                };
                if vpci.reset_generation() != expected_gen || shutdown_flag.load(Ordering::Acquire) {
                    continue;
                }
                let (resp_hdr, extra) = if sock_dead {
                    (make_error_resp(KMTL_ERR_IO), Vec::new())
                } else {
                    let opcode = read_u32_le(&req_hdr, 8);
                    let frame_kind = if opcode == KMTL_OP_COMMIT_COMMAND_BUFFER
                        && dirty_sync.as_ref().is_some_and(|ds| ds.sync_dirty_to_poc())
                    {
                        KMTL_FRAME_GUEST_RPC_PREFLUSHED
                    } else {
                        KMTL_FRAME_GUEST_RPC
                    };
                    match exchange_frame(&mut sock, frame_kind, &req_hdr, &in_payload, out_cap) {
                        Ok(r) => r,
                        Err(e) => {
                            if !shutdown_flag.load(Ordering::Acquire) {
                                eprintln!("kido-vmm: kido-vk-worker RPC I/O error: {e}");
                            }
                            sock_dead = true;
                            (make_error_resp(KMTL_ERR_IO), Vec::new())
                        }
                    }
                };
                if shutdown_flag.load(Ordering::Acquire) {
                    break;
                }
                let mut combined = Vec::with_capacity(KMTL_RESP_HDR_LEN + extra.len());
                combined.extend_from_slice(&resp_hdr);
                combined.extend_from_slice(&extra);
                vpci.complete_used_with_write(0, head_index, expected_gen, &out_segs, &combined);
            }
        }
    }
}

pub struct VirtioMetalAdapter(pub Arc<VirtioMetal>);

impl VirtioDevice for VirtioMetalAdapter {
    fn pci_device_id(&self) -> u16 {
        PCI_DEVICE_VIRTIO_METAL
    }

    fn pci_subsystem_id(&self) -> u16 {
        KMTL_VIRTIO_DEVICE_ID
    }

    fn pci_class_code(&self) -> u32 {
        KMTL_PCI_CLASS_CODE
    }

    fn features(&self) -> u64 {
        VIRTIO_F_VERSION_1 | VIRTIO_RING_F_INDIRECT_DESC
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &[QUEUE_MAX_SIZE, QUEUE_MAX_SIZE]
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
        if queue_index != 0 {
            let mut raise_irq = false;
            while let Ok(Some(chain)) = queue.pop_avail(mem) {
                if queue.push_used(mem, chain.head_index, 0) {
                    raise_irq = true;
                }
            }
            return QueueOutcome {
                raise_irq,
                needs_reset: false,
            };
        }
        let expected_gen = self
            .0
            .vpci
            .lock()
            .unwrap()
            .as_ref()
            .and_then(Weak::upgrade)
            .map_or(0, |v| v.reset_generation());
        let mut raise_irq = false;

        loop {
            let chain = match queue.pop_avail(mem) {
                Ok(Some(c)) => c,
                Ok(None) => break,
                Err(QueueError::NotReady) => break,
                Err(e) => {
                    eprintln!("kido-vmm: virtio-metal queue pop error: {e:?}");
                    return QueueOutcome {
                        raise_irq,
                        needs_reset: true,
                    };
                }
            };

            let mut total_read: usize = 0;
            let mut total_write: usize = 0;
            let mut read_segs = Vec::new();
            let mut out_segs = Vec::new();
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
                    if total_read > KMTL_REQ_HDR_LEN + KMTL_MAX_IN_PAYLOAD {
                        bad_chain = true;
                        break;
                    }
                    read_segs.push((d.addr, d.len));
                }
            }

            if bad_chain || total_read < KMTL_REQ_HDR_LEN || total_write < KMTL_RESP_HDR_LEN {
                let err_resp = make_error_resp(KMTL_ERR_INVAL);
                let written = scatter_write(mem, &out_segs, &err_resp);
                if queue.push_used(mem, chain.head_index, written) {
                    raise_irq = true;
                }
                continue;
            }

            let payload_len = total_read - KMTL_REQ_HDR_LEN;
            let reserved = payload_len.max(1);
            let mut budget_ok = false;
            loop {
                let cur = self.0.inflight_bytes.load(Ordering::Acquire);
                if cur.saturating_add(reserved) > KMTL_MAX_INFLIGHT_BYTES {
                    break;
                }
                if self
                    .0
                    .inflight_bytes
                    .compare_exchange_weak(cur, cur + reserved, Ordering::AcqRel, Ordering::Acquire)
                    .is_ok()
                {
                    budget_ok = true;
                    break;
                }
            }
            if !budget_ok {
                let err_resp = make_error_resp(KMTL_ERR_NOMEM);
                let written = scatter_write(mem, &out_segs, &err_resp);
                if queue.push_used(mem, chain.head_index, written) {
                    raise_irq = true;
                }
                continue;
            }
            let budget_guard = InflightBudgetGuard {
                counter: Arc::clone(&self.0.inflight_bytes),
                bytes: reserved,
            };

            let mut req_hdr = [0u8; KMTL_REQ_HDR_LEN];
            let mut in_payload = vec![0u8; payload_len];
            let mut off = 0usize;
            let mut read_ok = true;
            for (gpa, len) in read_segs {
                let mut seg_off = 0usize;
                let seg_len = len as usize;
                while seg_off < seg_len {
                    if off < KMTL_REQ_HDR_LEN {
                        let take = (KMTL_REQ_HDR_LEN - off).min(seg_len - seg_off);
                        if mem
                            .read_slice(gpa + seg_off as u64, &mut req_hdr[off..off + take])
                            .is_err()
                        {
                            read_ok = false;
                            break;
                        }
                        off += take;
                        seg_off += take;
                    } else {
                        let p_off = off - KMTL_REQ_HDR_LEN;
                        let take = (payload_len - p_off).min(seg_len - seg_off);
                        if mem
                            .read_slice(gpa + seg_off as u64, &mut in_payload[p_off..p_off + take])
                            .is_err()
                        {
                            read_ok = false;
                            break;
                        }
                        off += take;
                        seg_off += take;
                    }
                }
                if !read_ok {
                    break;
                }
            }
            if !read_ok {
                let err_resp = make_error_resp(KMTL_ERR_INVAL);
                let written = scatter_write(mem, &out_segs, &err_resp);
                if queue.push_used(mem, chain.head_index, written) {
                    raise_irq = true;
                }
                continue;
            }

            let opcode = read_u32_le(&req_hdr, 8);
            if opcode >= 0xFFFF_0000 {
                let err_resp = make_error_resp(KMTL_ERR_UNSUPP);
                let written = scatter_write(mem, &out_segs, &err_resp);
                if queue.push_used(mem, chain.head_index, written) {
                    raise_irq = true;
                }
                continue;
            }

            let out_cap = (total_write - KMTL_RESP_HDR_LEN).min(KMTL_MAX_OUT_PAYLOAD) as u32;
            let send_status = match self.0.tx.lock().unwrap().as_ref() {
                Some(tx) => match tx.try_send(MetalJob::Rpc {
                    head_index: chain.head_index,
                    expected_gen,
                    req_hdr,
                    in_payload,
                    out_segs: out_segs.clone(),
                    out_cap,
                    _budget: budget_guard,
                }) {
                    Ok(()) => Ok(()),
                    Err(mpsc::TrySendError::Full(_)) => Err(KMTL_ERR_NOMEM),
                    Err(mpsc::TrySendError::Disconnected(_)) => Err(KMTL_ERR_IO),
                },
                None => Err(KMTL_ERR_IO),
            };
            if let Err(err_code) = send_status {
                let err_resp = make_error_resp(err_code);
                let written = scatter_write(mem, &out_segs, &err_resp);
                if queue.push_used(mem, chain.head_index, written) {
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
        self.0.desired_reset_gen.fetch_add(1, Ordering::AcqRel);
        if let Some(tx) = self.0.tx.lock().unwrap().as_ref() {
            let _ = tx.try_send(MetalJob::WakeReset);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn device_config_builds_from_worker_info() {
        let mut info = [0u8; KMTL_DEV_INFO_LEN];
        info[0..13].copy_from_slice(b"Mali-G610 MC4");
        info[128..136].copy_from_slice(&0x1234_5678u64.to_le_bytes());
        info[136..144].copy_from_slice(&(1u64 << 30).to_le_bytes());
        info[144..152].copy_from_slice(&(1u64 << 30).to_le_bytes());
        info[168..172].copy_from_slice(&1024u32.to_le_bytes());
        info[172..176].copy_from_slice(&1024u32.to_le_bytes());
        info[176..180].copy_from_slice(&64u32.to_le_bytes());
        info[180..184].copy_from_slice(&32768u32.to_le_bytes());
        info[184..188].copy_from_slice(&4096u32.to_le_bytes());
        info[188..192].copy_from_slice(&1u32.to_le_bytes());

        let shm = 512 * 1024 * 1024u64;
        let cfg = make_device_config_from_info(&info, shm).unwrap();
        assert_eq!(read_u32_le(&cfg, 0), KMTL_MAGIC);
        assert_eq!(read_u32_le(&cfg, 4), KMTL_PROTO_VERSION);
        assert_eq!(read_u32_le(&cfg, 8), 16384);
        assert_eq!(read_u32_le(&cfg, 12), 1);
        assert_eq!(read_u64_le(&cfg, 16), shm);
        assert_eq!(read_u64_le(&cfg, 24), shm);
        assert_eq!(read_u64_le(&cfg, 32), shm);
        assert_eq!(read_u32_le(&cfg, 40), 1024);
        assert_eq!(read_u32_le(&cfg, 52), 32768);
        assert_eq!(read_u64_le(&cfg, 56), 0x1234_5678);

        // Verify KMTL_FEAT_DMA_COHERENT propagates through device config flags.
        info[188..192].copy_from_slice(&(1u32 | KMTL_FEAT_DMA_COHERENT).to_le_bytes());
        let cfg_coh = make_device_config_from_info(&info, shm).unwrap();
        assert_ne!(read_u32_le(&cfg_coh, 12) & KMTL_FEAT_DMA_COHERENT, 0);
    }

    #[test]
    fn rpc_framing_roundtrips_over_unix_socketpair() {
        let (mut a, mut b) = UnixStream::pair().unwrap();
        let t = thread::spawn(move || {
            let mut hdr = [0u8; 1 + KMTL_REQ_HDR_LEN + 8];
            b.read_exact(&mut hdr).unwrap();
            assert_eq!(hdr[0], KMTL_FRAME_GUEST_RPC);
            let in_len = u32::from_le_bytes(hdr[41..45].try_into().unwrap()) as usize;
            let mut payload = vec![0u8; in_len];
            b.read_exact(&mut payload).unwrap();
            assert_eq!(&payload, b"hello");

            let mut resp = [0u8; KMTL_RESP_HDR_LEN];
            resp[0..4].copy_from_slice(&0i32.to_le_bytes());
            resp[4..8].copy_from_slice(&5u32.to_le_bytes());
            resp[8..16].copy_from_slice(&42u64.to_le_bytes());
            b.write_all(&resp).unwrap();
            b.write_all(b"world").unwrap();
        });

        let mut req = [0u8; KMTL_REQ_HDR_LEN];
        req[0..4].copy_from_slice(&KMTL_MAGIC.to_le_bytes());
        req[4..8].copy_from_slice(&KMTL_PROTO_VERSION.to_le_bytes());
        req[8..12].copy_from_slice(&1u32.to_le_bytes());
        let (resp, extra) = exchange_frame(&mut a, KMTL_FRAME_GUEST_RPC, &req, b"hello", 64).unwrap();
        assert_eq!(read_u64_le(&resp, 8), 42);
        assert_eq!(&extra, b"world");
        t.join().unwrap();
    }
}
