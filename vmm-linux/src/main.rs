#![cfg_attr(
    not(all(target_os = "linux", target_arch = "aarch64")),
    allow(dead_code)
)]

mod affinity;
mod boot;
mod irq;
#[cfg(all(target_os = "linux", target_arch = "aarch64"))]
mod kvm;
mod memory;
mod mmio;
mod net;
mod notify;
mod pci;
mod pl011;
mod pl031;
mod virtio;

use std::fs::{self, OpenOptions};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::os::unix::io::RawFd;
use std::path::{Path, PathBuf};

use crate::memory::{MAX_RAM_SIZE, MIN_RAM_SIZE};
use crate::net::{parse_forward, parse_mac, ForwardSpec, DEFAULT_GUEST_MAC};
use crate::notify::validate_notify_fd;
use crate::pl011::DEFAULT_CONSOLE_MAX_BYTES;
use crate::virtio::blk::DiskSyncMode;
use crate::virtio::fs::{parse_share_spec, ShareConfig};
use crate::virtio::metal::{DEFAULT_LINUX_GPU_MIB, MAX_GPU_MIB, MIN_GPU_MIB};

#[derive(Debug, Clone)]
struct DiskSpec {
    path: PathBuf,
    read_only: bool,
}

#[derive(Debug, Clone)]
struct CliConfig {
    kernel: PathBuf,
    initrd: PathBuf,
    root_disk: Option<PathBuf>,
    root_disk_base: Option<PathBuf>,
    extra_disks: Vec<DiskSpec>,
    shares: Vec<ShareConfig>,
    notify_fd: Option<RawFd>,
    watch_paths: Vec<PathBuf>,
    memory_mib: usize,
    cpus: usize,
    boot_args: String,
    cmdline_file: Option<PathBuf>,
    console_log: Option<PathBuf>,
    console_max: u64,
    pin_cpus: Option<String>,
    state_file: Option<PathBuf>,
    disk_sync: DiskSyncMode,
    mac: [u8; 6],
    forwards: Vec<ForwardSpec>,
    restrict: bool,
    gpu: bool,
    gpu_memory_mib: usize,
    vk_worker: Option<PathBuf>,
}

fn default_linux_gpu_mib(memory_mib: usize) -> usize {
    if memory_mib >= 2048 {
        (memory_mib / 2).min(1024)
    } else {
        DEFAULT_LINUX_GPU_MIB
    }
}

fn usage() -> ! {
    eprintln!(
        "usage: kido-vmm --kernel PATH --initrd PATH [--disk PATH [--disk-base PATH]] \
         [--extra-disk PATH] [--extra-disk-ro PATH] [--share NAME=DIR] [--share-ro NAME=DIR] \
         [--notify-fd N --watch DIR ...] [--memory MIB] [--cpus N] \
         [--boot-args STR | --cmdline-file PATH] [--console-log PATH | --console PATH] \
         [--console-max BYTES] [--pin-cpus LIST] [--state-file PATH] [--disk-sync full|none] \
         [--mac AA:BB:CC:DD:EE:FF] [--forward tcp|udp:HOST_IP:HOST_PORT:[GUEST_IP:]GUEST_PORT] \
         [--restrict] [--gpu] [--gpu-memory MIB] [--vk-worker PATH] [--version]"
    );
    std::process::exit(2);
}

fn parse_args<I: IntoIterator<Item = String>>(args: I) -> Result<CliConfig, String> {
    let mut iter = args.into_iter();
    let _prog = iter.next();

    let mut kernel: Option<PathBuf> = None;
    let mut initrd: Option<PathBuf> = None;
    let mut root_disk: Option<PathBuf> = None;
    let mut root_disk_base: Option<PathBuf> = None;
    let mut extra_disks: Vec<DiskSpec> = Vec::new();
    let mut shares: Vec<ShareConfig> = Vec::new();
    let mut notify_fd: Option<RawFd> = None;
    let mut watch_paths: Vec<PathBuf> = Vec::new();
    let mut memory_mib: usize = 1024;
    let mut cpus: usize = 1;
    let mut boot_args: Option<String> = None;
    let mut cmdline_file: Option<PathBuf> = None;
    let mut console_log: Option<PathBuf> = None;
    let mut console_max: u64 = DEFAULT_CONSOLE_MAX_BYTES;
    let mut pin_cpus: Option<String> = None;
    let mut state_file: Option<PathBuf> = None;
    let mut disk_sync = DiskSyncMode::Full;
    let mut mac = DEFAULT_GUEST_MAC;
    let mut forwards: Vec<ForwardSpec> = Vec::new();
    let mut restrict = false;
    let mut gpu = false;
    let mut gpu_memory_mib: usize = 0;
    let mut vk_worker: Option<PathBuf> = None;

    while let Some(flag) = iter.next() {
        if flag == "--restrict" {
            restrict = true;
            continue;
        }
        if flag == "--gpu" {
            gpu = true;
            continue;
        }
        let val = iter
            .next()
            .ok_or_else(|| format!("missing value for {flag}"))?;
        match flag.as_str() {
            "--kernel" => kernel = Some(PathBuf::from(val)),
            "--initrd" => initrd = Some(PathBuf::from(val)),
            "--disk" => root_disk = Some(PathBuf::from(val)),
            "--disk-base" => root_disk_base = Some(PathBuf::from(val)),
            "--extra-disk" => extra_disks.push(DiskSpec {
                path: PathBuf::from(val),
                read_only: false,
            }),
            "--extra-disk-ro" => extra_disks.push(DiskSpec {
                path: PathBuf::from(val),
                read_only: true,
            }),
            "--share" => shares.push(parse_share_spec(&val, false)?),
            "--share-ro" => shares.push(parse_share_spec(&val, true)?),
            "--notify-fd" => {
                if notify_fd.is_some() {
                    return Err("duplicate --notify-fd".to_string());
                }
                let fd = val
                    .parse::<RawFd>()
                    .map_err(|_| format!("invalid --notify-fd '{val}'"))?;
                validate_notify_fd(fd)?;
                notify_fd = Some(fd);
            }
            "--watch" => {
                if !val.starts_with('/') {
                    return Err(format!("bad --watch '{val}': not an absolute path"));
                }
                watch_paths.push(PathBuf::from(val));
            }
            "--memory" => {
                memory_mib = val
                    .parse::<usize>()
                    .map_err(|_| format!("invalid --memory '{val}'"))?;
            }
            "--cpus" => {
                cpus = val
                    .parse::<usize>()
                    .map_err(|_| format!("invalid --cpus '{val}'"))?;
            }
            "--boot-args" => boot_args = Some(val),
            "--cmdline-file" => cmdline_file = Some(PathBuf::from(val)),
            "--console-log" | "--console" => {
                if val == "-" {
                    console_log = None;
                } else {
                    console_log = Some(PathBuf::from(val));
                }
            }
            "--console-max" => {
                console_max = val
                    .parse::<u64>()
                    .map_err(|_| format!("invalid --console-max '{val}'"))?;
            }
            "--pin-cpus" => pin_cpus = Some(val),
            "--state-file" => state_file = Some(PathBuf::from(val)),
            "--disk-sync" => match val.as_str() {
                "full" | "fsync" => disk_sync = DiskSyncMode::Full,
                "none" => disk_sync = DiskSyncMode::None,
                _ => return Err(format!("invalid --disk-sync '{val}' (expected full|none)")),
            },
            "--mac" => mac = parse_mac(&val)?,
            "--forward" => forwards.push(parse_forward(&val)?),
            "--gpu-memory" => {
                gpu_memory_mib = val
                    .parse::<usize>()
                    .map_err(|_| format!("invalid --gpu-memory '{val}'"))?;
            }
            "--vk-worker" => vk_worker = Some(PathBuf::from(val)),
            other => return Err(format!("unknown flag '{other}'")),
        }
    }

    let kernel = kernel.ok_or_else(|| "--kernel is required".to_string())?;
    let initrd = initrd.ok_or_else(|| "--initrd is required".to_string())?;
    if cpus == 0 || cpus > 32 {
        return Err(format!("--cpus {cpus} out of range 1..=32"));
    }
    let ram_bytes = memory_mib
        .checked_mul(1024 * 1024)
        .ok_or_else(|| format!("--memory {memory_mib} overflows"))?;
    if !(MIN_RAM_SIZE..=MAX_RAM_SIZE).contains(&ram_bytes) {
        return Err(format!(
            "--memory {memory_mib} MiB out of range ({}..={} MiB)",
            MIN_RAM_SIZE >> 20,
            MAX_RAM_SIZE >> 20
        ));
    }
    if gpu_memory_mib != 0 && !gpu {
        return Err("--gpu-memory requires --gpu".to_string());
    }
    if gpu {
        if gpu_memory_mib == 0 {
            gpu_memory_mib = default_linux_gpu_mib(memory_mib);
        }
        if !(MIN_GPU_MIB..=MAX_GPU_MIB).contains(&gpu_memory_mib) {
            return Err(format!(
                "--gpu-memory {gpu_memory_mib} MiB out of range ({MIN_GPU_MIB}..={MAX_GPU_MIB} MiB)"
            ));
        }
    }
    let mut seen_shares = std::collections::HashSet::new();
    for sh in &shares {
        if !seen_shares.insert(sh.name.as_str()) {
            return Err(format!("duplicate share name '{}'", sh.name));
        }
    }
    let max_extra_disks = 29 - usize::from(gpu) - usize::from(!shares.is_empty());
    if extra_disks.len() > max_extra_disks {
        return Err(format!(
            "too many extra disks ({}, max {max_extra_disks})",
            extra_disks.len()
        ));
    }
    if root_disk_base.is_some() && root_disk.is_none() {
        return Err("--disk-base requires --disk".to_string());
    }

    Ok(CliConfig {
        kernel,
        initrd,
        root_disk,
        root_disk_base,
        extra_disks,
        shares,
        notify_fd,
        watch_paths,
        memory_mib,
        cpus,
        boot_args: boot_args.unwrap_or_default(),
        cmdline_file,
        console_log,
        console_max,
        pin_cpus,
        state_file,
        disk_sync,
        mac,
        forwards,
        restrict,
        gpu,
        gpu_memory_mib,
        vk_worker,
    })
}

fn write_state(path: Option<&Path>, state: &str) {
    let Some(path) = path else {
        return;
    };
    let tmp = PathBuf::from(format!("{}.tmp", path.display()));
    if let Ok(mut f) = OpenOptions::new()
        .create(true)
        .write(true)
        .truncate(true)
        .mode(0o600)
        .open(&tmp)
    {
        if writeln!(f, "{state}").is_ok() {
            let _ = fs::rename(&tmp, path);
        }
    }
}

fn resolve_boot_args(cfg: &CliConfig) -> Result<String, String> {
    if let Some(ref path) = cfg.cmdline_file {
        let mut raw = fs::read_to_string(path)
            .map_err(|e| format!("read --cmdline-file '{}': {e}", path.display()))?;
        let _ = fs::remove_file(path);
        let trimmed = raw.trim().to_string();
        unsafe {
            crate::boot::zeroize_bytes(raw.as_bytes_mut());
        }
        return Ok(trimmed);
    }
    Ok(cfg.boot_args.trim().to_string())
}

#[cfg(all(target_os = "linux", target_arch = "aarch64"))]
fn raise_nofile_limit() {
    let mut rl = libc::rlimit {
        rlim_cur: 0,
        rlim_max: 0,
    };
    if unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut rl) } == 0 {
        let target: libc::rlim_t = 131_072;
        let desired = if rl.rlim_max == libc::RLIM_INFINITY {
            target
        } else {
            rl.rlim_max.min(target)
        };
        if desired > rl.rlim_cur {
            rl.rlim_cur = desired;
            let _ = unsafe { libc::setrlimit(libc::RLIMIT_NOFILE, &rl) };
        }
    }
}

#[cfg(all(target_os = "linux", target_arch = "aarch64"))]
struct FsShutdownGuard(Option<std::sync::Arc<crate::virtio::fs::VirtioFs>>);

#[cfg(all(target_os = "linux", target_arch = "aarch64"))]
impl Drop for FsShutdownGuard {
    fn drop(&mut self) {
        if let Some(fs) = self.0.take() {
            fs.shutdown();
        }
    }
}

#[cfg(all(target_os = "linux", target_arch = "aarch64"))]
fn run_vm(cfg: CliConfig) -> Result<(), String> {
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::Arc;

    use crate::affinity::resolve_and_pin_process;
    use crate::boot::{
        build_fdt, decode_pa_bits, ecam_layout_for_pa_bits, load_kernel_and_initrd,
        read_urandom_seed, write_fdt_to_ram, zeroize_bytes, FdtConfig, PL011_SPI,
    };
    use crate::irq::SpiMux;
    use crate::kvm::{setup_control_signalfd, KvmVm};
    use crate::memory::{GuestMemory, RAM_BASE};
    use crate::mmio::MmioBus;
    use crate::net::NetEngine;
    use crate::notify::{close_raw_fd, NotifyHandle};
    use crate::pci::{pci_intx_spi, PciBus, ShmBarConfig};
    use crate::pl011::{ConsoleSink, Pl011};
    use crate::pl031::Pl031;
    use crate::virtio::blk::VirtioBlk;
    use crate::virtio::fs::{VirtioFs, VirtioFsAdapter, DEFAULT_FS_TAG};
    use crate::virtio::metal::{VirtioMetal, VirtioMetalAdapter};
    use crate::virtio::net::{NetChannel, VirtioNet};
    use crate::virtio::pci::VirtioPciDevice;

    raise_nofile_limit();
    unsafe {
        libc::signal(libc::SIGPIPE, libc::SIG_IGN);
        libc::prctl(libc::PR_SET_NAME, c"kido-vmm".as_ptr(), 0, 0, 0);
    }

    let mut notify_handle: Option<NotifyHandle> = match cfg.notify_fd {
        Some(fd) if cfg.watch_paths.is_empty() => {
            close_raw_fd(fd);
            None
        }
        Some(fd) => match NotifyHandle::start(fd, cfg.watch_paths.clone()) {
            Ok(h) => Some(h),
            Err(e) => {
                eprintln!("kido-vmm: notifications off: {e}");
                None
            }
        },
        None => None,
    };

    write_state(cfg.state_file.as_deref(), "starting");
    let mut boot_args = resolve_boot_args(&cfg)?;

    let sig_fd = setup_control_signalfd()?;
    let pin_cpus = resolve_and_pin_process(cfg.pin_cpus.as_deref())?;

    let net_channel = NetChannel::new();
    let net_engine = NetEngine::bind(
        Arc::clone(&net_channel),
        cfg.mac,
        &cfg.forwards,
        cfg.restrict,
    )?;

    let ram_size = cfg.memory_mib * 1024 * 1024;
    let mem = Arc::new(
        GuestMemory::allocate(RAM_BASE, ram_size)
            .map_err(|e| format!("allocate {ram_size} bytes guest RAM: {e}"))?,
    );

    let loaded = load_kernel_and_initrd(&mem, &cfg.kernel, &cfg.initrd)?;

    let vm = KvmVm::create(Arc::clone(&mem), cfg.cpus)?;
    let pa_bits = decode_pa_bits(vm.id_aa64mmfr0_el1()).min(vm.ipa_bits());
    let ecam = ecam_layout_for_pa_bits(pa_bits);
    let ecam_end = ecam
        .base
        .checked_add(ecam.size)
        .ok_or_else(|| "ECAM address overflow".to_string())?;
    if mem.base() < ecam_end && mem.end() > ecam.base {
        return Err(format!(
            "guest RAM {:#x}..{:#x} overlaps PCIe ECAM {:#x}..{:#x}",
            mem.base(),
            mem.end(),
            ecam.base,
            ecam_end
        ));
    }

    let rng_seed = read_urandom_seed()?;
    let mut dtb = build_fdt(&FdtConfig {
        ram_base: RAM_BASE,
        ram_size: ram_size as u64,
        cpu_mpidrs: vm.mpidrs(),
        gic_version: vm.gic_version(),
        ecam,
        boot_args: &boot_args,
        initrd_start: loaded.initrd_gpa,
        initrd_end: loaded.initrd_end,
        rng_seed,
    })?;
    let fdt_res = write_fdt_to_ram(&mem, &dtb);
    unsafe {
        zeroize_bytes(boot_args.as_bytes_mut());
    }
    drop(boot_args);
    zeroize_bytes(&mut dtb);
    drop(dtb);
    fdt_res?;

    vm.configure_vcpu0_boot()?;

    let spi_mux = SpiMux::new(vm.spi_backend());

    let sink = match cfg.console_log {
        Some(ref path) => ConsoleSink::rotating_file(path.clone(), cfg.console_max)
            .map_err(|e| format!("open console log '{}': {e}", path.display()))?,
        None => ConsoleSink::stdout(),
    };
    let pl011 = Arc::new(Pl011::new(sink, spi_mux.line(PL011_SPI, 0)));
    let pl031 = Arc::new(Pl031::new());

    let mut pci_bus = PciBus::new();

    let mut disk_idx = 0usize;
    if let Some(ref root_path) = cfg.root_disk {
        let blk = match cfg.root_disk_base {
            Some(ref base_path) => {
                VirtioBlk::open_cow(base_path, root_path, false, cfg.disk_sync, disk_idx)?
            }
            None => VirtioBlk::open(root_path, false, cfg.disk_sync, disk_idx)?,
        };
        disk_idx += 1;
        let spi = pci_intx_spi(1, 1);
        let vpci = Arc::new(VirtioPciDevice::new(
            Box::new(blk),
            Arc::clone(&mem),
            spi_mux.line(spi, 1),
        ));
        pci_bus.add_virtio_device(1, vpci)?;
    }

    let vnet = VirtioNet::new(cfg.mac, Arc::clone(&net_channel));
    let net_spi = pci_intx_spi(2, 1);
    let net_vpci = Arc::new(VirtioPciDevice::new(
        Box::new(vnet),
        Arc::clone(&mem),
        spi_mux.line(net_spi, 2),
    ));
    let net_vpci_rx = Arc::clone(&net_vpci);
    net_channel.set_wake_guest_rx(Arc::new(move || {
        net_vpci_rx.inject_queue(0);
    }));
    pci_bus.add_virtio_device(2, net_vpci)?;

    let mut metal_dev: Option<Arc<VirtioMetal>> = None;
    let extra_disk_slot_base: usize = if cfg.gpu {
        if pa_bits < 40 {
            return Err(format!(
                "--gpu requires at least 40-bit KVM IPA for 64-bit GPU shared memory BAR (got {pa_bits}-bit)"
            ));
        }
        let shm_bytes = (cfg.gpu_memory_mib as u64) << 20;
        let metal = VirtioMetal::launch(shm_bytes, cfg.vk_worker.as_deref())?;
        let metal_spi = pci_intx_spi(3, 1);
        let metal_vpci = Arc::new(VirtioPciDevice::new(
            Box::new(VirtioMetalAdapter(Arc::clone(&metal))),
            Arc::clone(&mem),
            spi_mux.line(metal_spi, 3),
        ));
        let dma_coherent = metal.is_dma_coherent();
        let shm_mapper = vm.shm_mapper(1, metal.shm_backing(), dma_coherent);
        let dirty_sync = if dma_coherent {
            None
        } else {
            Some(Arc::clone(&shm_mapper) as Arc<dyn crate::virtio::metal::ShmDirtySync>)
        };
        metal.start_worker(Arc::clone(&metal_vpci), Arc::clone(&mem), dirty_sync)?;
        let shm_cfg = ShmBarConfig::new(
            metal.shm_size(),
            shm_mapper as Arc<dyn crate::pci::ShmRegionMapper>,
        )?;
        pci_bus.add_virtio_device_with_shm(3, metal_vpci, Some(shm_cfg))?;
        metal_dev = Some(metal);
        4
    } else {
        3
    };

    let mut fs_dev = FsShutdownGuard(None);
    let mut next_slot = extra_disk_slot_base;
    if !cfg.shares.is_empty() {
        let fs = VirtioFs::new(DEFAULT_FS_TAG, &cfg.shares)?;
        let fs_slot = next_slot as u8;
        let fs_spi = pci_intx_spi(fs_slot, 1);
        let fs_vpci = Arc::new(VirtioPciDevice::new(
            Box::new(VirtioFsAdapter(Arc::clone(&fs))),
            Arc::clone(&mem),
            spi_mux.line(fs_spi, fs_slot as u32),
        ));
        fs.start_workers(Arc::clone(&fs_vpci), Arc::clone(&mem))?;
        fs_dev.0 = Some(fs);
        pci_bus.add_virtio_device(fs_slot, fs_vpci)?;
        next_slot += 1;
    }

    for (i, spec) in cfg.extra_disks.iter().enumerate() {
        let dev_num = (next_slot + i) as u8;
        let blk = VirtioBlk::open(&spec.path, spec.read_only, cfg.disk_sync, disk_idx)?;
        disk_idx += 1;
        let spi = pci_intx_spi(dev_num, 1);
        let vpci = Arc::new(VirtioPciDevice::new(
            Box::new(blk),
            Arc::clone(&mem),
            spi_mux.line(spi, dev_num as u32),
        ));
        pci_bus.add_virtio_device(dev_num, vpci)?;
    }

    let pci_bus = Arc::new(pci_bus);
    let mmio = Arc::new(MmioBus::new(
        Arc::clone(&pl011),
        pl031,
        Arc::clone(&pci_bus),
        ecam,
    ));

    let net_shutdown = Arc::new(AtomicBool::new(false));
    let (net_pause, net_handle) = net_engine.spawn(Arc::clone(&net_shutdown))?;

    write_state(cfg.state_file.as_deref(), "running");
    let state_pause = cfg.state_file.clone();
    let state_resume = cfg.state_file.clone();
    let pci_pause = Arc::clone(&pci_bus);
    let pl011_pause = Arc::clone(&pl011);
    let net_on_pause = Arc::clone(&net_pause);
    let net_on_resume = Arc::clone(&net_pause);

    let res = vm.run(
        mmio,
        &pin_cpus,
        &sig_fd,
        move || {
            net_on_pause.pause();
            pci_pause.flush_all();
            pl011_pause.flush();
            write_state(state_pause.as_deref(), "paused");
        },
        move || {
            net_on_resume.resume();
            write_state(state_resume.as_deref(), "running");
        },
    );

    net_shutdown.store(true, Ordering::Release);
    net_pause.wake_shutdown();
    let _ = net_handle.join();
    if let Some(fs) = fs_dev.0.take() {
        fs.shutdown();
    }
    if let Some(nh) = notify_handle.take() {
        nh.shutdown();
    }
    if let Some(metal) = metal_dev.take() {
        metal.shutdown();
    }
    drop(sig_fd);

    pci_bus.flush_all();
    pl011.flush();

    match res {
        Ok(()) => {
            write_state(cfg.state_file.as_deref(), "stopped");
            Ok(())
        }
        Err(e) => {
            write_state(cfg.state_file.as_deref(), "error");
            Err(e)
        }
    }
}

#[cfg(not(all(target_os = "linux", target_arch = "aarch64")))]
fn run_vm(_cfg: CliConfig) -> Result<(), String> {
    Err("kido-vmm Linux KVM execution requires Linux aarch64".to_string())
}

fn main() {
    let raw_args: Vec<String> = std::env::args().collect();
    if raw_args.len() == 2 && raw_args[1] == "--version" {
        println!("kido-vmm {}", env!("CARGO_PKG_VERSION"));
        return;
    }
    if raw_args.len() <= 1 || raw_args.iter().any(|a| a == "-h" || a == "--help") {
        usage();
    }
    let cfg = match parse_args(raw_args) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("kido-vmm: {e}");
            usage();
        }
    };
    if let Err(e) = run_vm(cfg) {
        eprintln!("kido-vmm: {e}");
        std::process::exit(1);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::net::ForwardProto;

    #[test]
    fn cli_parses_all_m0_m1_m2_flags() {
        let cfg = parse_args(
            [
                "kido-vmm",
                "--kernel",
                "/boot/kernel",
                "--initrd",
                "/boot/initrd.cpio",
                "--disk",
                "/var/root.img",
                "--extra-disk",
                "/var/rw.img",
                "--extra-disk-ro",
                "/var/ro.img",
                "--memory",
                "1024",
                "--cpus",
                "4",
                "--boot-args",
                "serial=3 ext4-uuid=test",
                "--console-log",
                "/tmp/console.log",
                "--pin-cpus",
                "4-7",
                "--mac",
                "02:dc:72:01:02:03",
                "--forward",
                "tcp:127.0.0.1:12345:1023",
                "--forward",
                "udp:127.0.0.1:9999:10.0.2.15:9999",
                "--restrict",
            ]
            .into_iter()
            .map(String::from),
        )
        .unwrap();

        assert_eq!(cfg.kernel, PathBuf::from("/boot/kernel"));
        assert_eq!(cfg.initrd, PathBuf::from("/boot/initrd.cpio"));
        assert_eq!(cfg.root_disk, Some(PathBuf::from("/var/root.img")));
        assert_eq!(cfg.extra_disks.len(), 2);
        assert!(!cfg.extra_disks[0].read_only);
        assert!(cfg.extra_disks[1].read_only);
        assert_eq!(cfg.memory_mib, 1024);
        assert_eq!(cfg.cpus, 4);
        assert_eq!(cfg.mac, [0x02, 0xdc, 0x72, 0x01, 0x02, 0x03]);
        assert_eq!(cfg.forwards.len(), 2);
        assert_eq!(cfg.forwards[0].proto, ForwardProto::Tcp);
        assert_eq!(cfg.forwards[1].proto, ForwardProto::Udp);
        assert!(cfg.restrict);
    }

    #[test]
    fn cmdline_file_is_read_and_immediately_unlinked() {
        let path = std::env::temp_dir().join(format!(
            "kido-vmm-cmdline-test-{}.txt",
            std::process::id()
        ));
        fs::write(&path, "serial=3 kido_secret=supersecret\n").unwrap();
        let cfg = parse_args(
            [
                "kido-vmm",
                "--kernel",
                "/k",
                "--initrd",
                "/i",
                "--cmdline-file",
                path.to_str().unwrap(),
            ]
            .into_iter()
            .map(String::from),
        )
        .unwrap();
        let args = resolve_boot_args(&cfg).unwrap();
        assert_eq!(args, "serial=3 kido_secret=supersecret");
        assert!(!path.exists());
    }

    #[test]
    fn cli_parses_shares_and_rejects_invalid_or_duplicate() {
        let cfg = parse_args(
            [
                "kido-vmm",
                "--kernel",
                "/k",
                "--initrd",
                "/i",
                "--share",
                "m0=/tmp/rw",
                "--share-ro",
                "m1=/tmp/ro",
                "--watch",
                "/tmp/rw",
            ]
            .into_iter()
            .map(String::from),
        )
        .unwrap();
        assert_eq!(cfg.shares.len(), 2);
        assert_eq!(cfg.shares[0].name, "m0");
        assert!(!cfg.shares[0].read_only);
        assert_eq!(cfg.shares[1].name, "m1");
        assert!(cfg.shares[1].read_only);
        assert_eq!(cfg.watch_paths, vec![PathBuf::from("/tmp/rw")]);

        // Duplicate share name rejected
        assert!(parse_args(
            [
                "kido-vmm",
                "--kernel",
                "/k",
                "--initrd",
                "/i",
                "--share",
                "m0=/tmp/a",
                "--share-ro",
                "m0=/tmp/b",
            ]
            .into_iter()
            .map(String::from),
        )
        .is_err());

        // Relative share path or traversal share name rejected
        assert!(parse_args(
            [
                "kido-vmm",
                "--kernel",
                "/k",
                "--initrd",
                "/i",
                "--share",
                "../evil=/tmp/a",
            ]
            .into_iter()
            .map(String::from),
        )
        .is_err());
        assert!(parse_args(
            [
                "kido-vmm",
                "--kernel",
                "/k",
                "--initrd",
                "/i",
                "--share",
                "m0=relative/dir",
            ]
            .into_iter()
            .map(String::from),
        )
        .is_err());
    }

    #[test]
    fn gpu_memory_default_scales_with_ram_and_allows_explicit_override() {
        let small = parse_args(
            ["kido-vmm", "--kernel", "/k", "--initrd", "/i", "--memory", "1024", "--gpu"]
                .into_iter()
                .map(String::from),
        )
        .unwrap();
        assert_eq!(small.gpu_memory_mib, 512);

        let medium = parse_args(
            ["kido-vmm", "--kernel", "/k", "--initrd", "/i", "--memory", "2048", "--gpu"]
                .into_iter()
                .map(String::from),
        )
        .unwrap();
        assert_eq!(medium.gpu_memory_mib, 1024);

        let large = parse_args(
            ["kido-vmm", "--kernel", "/k", "--initrd", "/i", "--memory", "4096", "--gpu"]
                .into_iter()
                .map(String::from),
        )
        .unwrap();
        assert_eq!(large.gpu_memory_mib, 1024);

        let explicit = parse_args(
            [
                "kido-vmm",
                "--kernel",
                "/k",
                "--initrd",
                "/i",
                "--memory",
                "4096",
                "--gpu",
                "--gpu-memory",
                "1536",
            ]
            .into_iter()
            .map(String::from),
        )
        .unwrap();
        assert_eq!(explicit.gpu_memory_mib, 1536);
    }
}
