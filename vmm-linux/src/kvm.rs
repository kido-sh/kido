#[cfg(target_os = "linux")]
use std::mem::offset_of;
#[cfg(target_os = "linux")]
use std::os::unix::io::{AsRawFd, FromRawFd, OwnedFd};
#[cfg(target_os = "linux")]
use std::sync::atomic::{AtomicBool, Ordering};
#[cfg(target_os = "linux")]
use std::sync::{Arc, Condvar, Mutex};
#[cfg(target_os = "linux")]
use std::thread;
#[cfg(target_os = "linux")]
use std::time::Duration;

#[cfg(target_os = "linux")]
use kvm_bindings::{
    kvm_create_device, kvm_device_attr, kvm_device_type_KVM_DEV_TYPE_ARM_VGIC_V3, kvm_regs,
    kvm_userspace_memory_region, kvm_vcpu_init, user_pt_regs, KVM_ARM_IRQ_NUM_MASK,
    KVM_ARM_IRQ_TYPE_SHIFT, KVM_ARM_IRQ_TYPE_SPI, KVM_ARM_VCPU_POWER_OFF, KVM_ARM_VCPU_PSCI_0_2,
    KVM_DEV_ARM_VGIC_CTRL_INIT, KVM_DEV_ARM_VGIC_GRP_ADDR, KVM_DEV_ARM_VGIC_GRP_CTRL,
    KVM_DEV_ARM_VGIC_GRP_NR_IRQS, KVM_REG_ARM64, KVM_REG_ARM64_SYSREG,
    KVM_REG_ARM64_SYSREG_CRM_MASK, KVM_REG_ARM64_SYSREG_CRM_SHIFT, KVM_REG_ARM64_SYSREG_CRN_MASK,
    KVM_REG_ARM64_SYSREG_CRN_SHIFT, KVM_REG_ARM64_SYSREG_OP0_MASK, KVM_REG_ARM64_SYSREG_OP0_SHIFT,
    KVM_REG_ARM64_SYSREG_OP1_MASK, KVM_REG_ARM64_SYSREG_OP1_SHIFT, KVM_REG_ARM64_SYSREG_OP2_MASK,
    KVM_REG_ARM64_SYSREG_OP2_SHIFT, KVM_REG_ARM_CORE, KVM_REG_SIZE_U64, KVM_SYSTEM_EVENT_RESET,
    KVM_SYSTEM_EVENT_SHUTDOWN, KVM_VGIC_V3_ADDR_TYPE_DIST, KVM_VGIC_V3_ADDR_TYPE_REDIST,
};
#[cfg(target_os = "linux")]
use kvm_ioctls::{DeviceFd, Kvm, VcpuExit, VcpuFd, VmFd};

#[cfg(target_os = "linux")]
use crate::affinity::pin_current_thread;
#[cfg(target_os = "linux")]
use crate::boot::{FDT_GPA, GICD_BASE, GICR_BASE, KERNEL_GPA};
#[cfg(target_os = "linux")]
use crate::irq::SpiBackend;
#[cfg(target_os = "linux")]
use crate::memory::GuestMemory;
#[cfg(target_os = "linux")]
use crate::mmio::MmioBus;

#[cfg(target_os = "linux")]
const KVM_API_VERSION: i32 = 12;
#[cfg(target_os = "linux")]
const VGIC_NR_IRQS: u32 = 128;
#[cfg(target_os = "linux")]
const PSTATE_EL1H_MASKED: u64 = 0x3c5;
#[cfg(target_os = "linux")]
const KVM_SET_SIGNAL_MASK: libc::c_ulong = 0x4004_ae8b;

#[cfg(target_os = "linux")]
#[repr(C)]
struct KvmSignalMask {
    len: u32,
    sigset: [u8; 8],
}

#[cfg(target_os = "linux")]
fn vcpu_kick_sig() -> libc::c_int {
    let candidate = libc::SIGRTMIN() + 1;
    if candidate <= libc::SIGRTMAX() && candidate <= 64 {
        candidate
    } else {
        libc::SIGALRM
    }
}

#[cfg(target_os = "linux")]
const fn arm64_core_reg(byte_offset: usize) -> u64 {
    KVM_REG_ARM64 | KVM_REG_SIZE_U64 | (KVM_REG_ARM_CORE as u64) | ((byte_offset / 4) as u64)
}

#[cfg(target_os = "linux")]
const fn arm64_sys_reg(op0: u64, op1: u64, crn: u64, crm: u64, op2: u64) -> u64 {
    KVM_REG_ARM64
        | KVM_REG_SIZE_U64
        | (KVM_REG_ARM64_SYSREG as u64)
        | ((op0 << KVM_REG_ARM64_SYSREG_OP0_SHIFT) & (KVM_REG_ARM64_SYSREG_OP0_MASK as u64))
        | ((op1 << KVM_REG_ARM64_SYSREG_OP1_SHIFT) & (KVM_REG_ARM64_SYSREG_OP1_MASK as u64))
        | ((crn << KVM_REG_ARM64_SYSREG_CRN_SHIFT) & (KVM_REG_ARM64_SYSREG_CRN_MASK as u64))
        | ((crm << KVM_REG_ARM64_SYSREG_CRM_SHIFT) & (KVM_REG_ARM64_SYSREG_CRM_MASK as u64))
        | ((op2 << KVM_REG_ARM64_SYSREG_OP2_SHIFT) & (KVM_REG_ARM64_SYSREG_OP2_MASK as u64))
}

#[cfg(target_os = "linux")]
const REG_X0: u64 = arm64_core_reg(offset_of!(kvm_regs, regs) + offset_of!(user_pt_regs, regs));
#[cfg(target_os = "linux")]
const REG_X1: u64 =
    arm64_core_reg(offset_of!(kvm_regs, regs) + offset_of!(user_pt_regs, regs) + 8);
#[cfg(target_os = "linux")]
const REG_X2: u64 =
    arm64_core_reg(offset_of!(kvm_regs, regs) + offset_of!(user_pt_regs, regs) + 16);
#[cfg(target_os = "linux")]
const REG_X3: u64 =
    arm64_core_reg(offset_of!(kvm_regs, regs) + offset_of!(user_pt_regs, regs) + 24);
#[cfg(target_os = "linux")]
const REG_PC: u64 = arm64_core_reg(offset_of!(kvm_regs, regs) + offset_of!(user_pt_regs, pc));
#[cfg(target_os = "linux")]
const REG_PSTATE: u64 =
    arm64_core_reg(offset_of!(kvm_regs, regs) + offset_of!(user_pt_regs, pstate));

#[cfg(target_os = "linux")]
const SYSREG_MPIDR_EL1: u64 = arm64_sys_reg(3, 0, 0, 0, 5);
#[cfg(target_os = "linux")]
const SYSREG_ID_AA64MMFR0_EL1: u64 = arm64_sys_reg(3, 0, 0, 7, 0);

#[cfg(target_os = "linux")]
extern "C" fn noop_sig_handler(_sig: libc::c_int) {}

#[cfg(target_os = "linux")]
pub struct KvmSpiBackend {
    vm: Arc<VmFd>,
}

#[cfg(target_os = "linux")]
impl SpiBackend for KvmSpiBackend {
    fn set_spi_level(&self, spi: u32, active: bool) {
        let irq = (KVM_ARM_IRQ_TYPE_SPI << KVM_ARM_IRQ_TYPE_SHIFT)
            | ((spi + 32) & KVM_ARM_IRQ_NUM_MASK);
        loop {
            match self.vm.set_irq_line(irq, active) {
                Ok(()) => break,
                Err(e) if e.errno() == libc::EINTR => continue,
                Err(_) => break,
            }
        }
    }
}

#[cfg(target_os = "linux")]
struct PauseState {
    paused: bool,
    parked: usize,
}

#[cfg(all(target_os = "linux", target_arch = "aarch64"))]
pub struct KvmVm {
    vcpus: Vec<VcpuFd>,
    _gic: DeviceFd,
    vm: Arc<VmFd>,
    _kvm: Kvm,
    _mem: Arc<GuestMemory>,
    mpidrs: Vec<u64>,
    id_aa64mmfr0_el1: u64,
    ipa_bits: u32,
}

#[cfg(target_os = "linux")]
impl KvmVm {
    pub fn create(mem: Arc<GuestMemory>, num_cpus: usize) -> Result<Self, String> {
        if num_cpus == 0 || num_cpus > 32 {
            return Err(format!("invalid vCPU count {num_cpus} (must be 1..=32)"));
        }

        let kvm = Kvm::new().map_err(|e| format!("open /dev/kvm: {e}"))?;
        let api = kvm.get_api_version();
        if api != KVM_API_VERSION {
            return Err(format!(
                "unsupported KVM API version {api} (expected {KVM_API_VERSION})"
            ));
        }

        let host_ipa = kvm.get_host_ipa_limit();
        let (vm, ipa_bits) = if host_ipa > 0 {
            let ipa = (host_ipa as u32).min(40);
            let vm = kvm
                .create_vm_with_ipa_size(ipa)
                .map_err(|e| format!("KVM_CREATE_VM (ipa={ipa}): {e}"))?;
            (vm, ipa)
        } else {
            let vm = kvm
                .create_vm()
                .map_err(|e| format!("KVM_CREATE_VM: {e}"))?;
            (vm, 40u32)
        };
        let max_ipa = 1u64.checked_shl(ipa_bits).unwrap_or(u64::MAX);
        if mem.end() > max_ipa {
            return Err(format!(
                "guest RAM end {:#x} exceeds KVM IPA limit {max_ipa:#x} ({ipa_bits}-bit)",
                mem.end()
            ));
        }
        let vm = Arc::new(vm);

        let mem_region = kvm_userspace_memory_region {
            slot: 0,
            flags: 0,
            guest_phys_addr: mem.base(),
            memory_size: mem.len() as u64,
            userspace_addr: mem.host_ptr() as u64,
        };
        unsafe {
            vm.set_user_memory_region(mem_region)
                .map_err(|e| format!("KVM_SET_USER_MEMORY_REGION: {e}"))?;
        }

        let mut preferred = kvm_vcpu_init::default();
        vm.get_preferred_target(&mut preferred)
            .map_err(|e| format!("KVM_ARM_PREFERRED_TARGET: {e}"))?;

        let mut vcpus = Vec::with_capacity(num_cpus);
        let mut mpidrs = Vec::with_capacity(num_cpus);
        let mut id_aa64mmfr0_el1 = 0u64;

        for idx in 0..num_cpus {
            let vcpu = vm
                .create_vcpu(idx as u64)
                .map_err(|e| format!("KVM_CREATE_VCPU({idx}): {e}"))?;
            let mut kvi = preferred;
            kvi.features[0] |= 1 << KVM_ARM_VCPU_PSCI_0_2;
            if idx > 0 {
                kvi.features[0] |= 1 << KVM_ARM_VCPU_POWER_OFF;
            }
            vcpu.vcpu_init(&kvi)
                .map_err(|e| format!("KVM_ARM_VCPU_INIT({idx}): {e}"))?;

            let mpidr = get_reg_u64(&vcpu, SYSREG_MPIDR_EL1)
                .map_err(|e| format!("read vcpu{idx} MPIDR_EL1: {e}"))?;
            mpidrs.push(mpidr);

            if idx == 0 {
                id_aa64mmfr0_el1 = get_reg_u64(&vcpu, SYSREG_ID_AA64MMFR0_EL1)
                    .map_err(|e| format!("read vcpu0 ID_AA64MMFR0_EL1: {e}"))?;
            }
            vcpus.push(vcpu);
        }

        let gic = Self::init_vgic_v3(&vm)?;

        Ok(Self {
            vcpus,
            _gic: gic,
            vm,
            _kvm: kvm,
            _mem: mem,
            mpidrs,
            id_aa64mmfr0_el1,
            ipa_bits,
        })
    }

    fn init_vgic_v3(vm: &VmFd) -> Result<DeviceFd, String> {
        let mut dev = kvm_create_device {
            type_: kvm_device_type_KVM_DEV_TYPE_ARM_VGIC_V3,
            fd: 0,
            flags: 0,
        };
        let gic = vm
            .create_device(&mut dev)
            .map_err(|e| format!("KVM_CREATE_DEVICE(VGIC_V3): {e}"))?;

        let nr_irqs = VGIC_NR_IRQS;
        gic.set_device_attr(&kvm_device_attr {
            flags: 0,
            group: KVM_DEV_ARM_VGIC_GRP_NR_IRQS,
            attr: 0,
            addr: (&nr_irqs as *const u32) as u64,
        })
        .map_err(|e| format!("VGIC_V3 NR_IRQS={nr_irqs}: {e}"))?;

        let dist_addr = GICD_BASE;
        gic.set_device_attr(&kvm_device_attr {
            flags: 0,
            group: KVM_DEV_ARM_VGIC_GRP_ADDR,
            attr: KVM_VGIC_V3_ADDR_TYPE_DIST as u64,
            addr: (&dist_addr as *const u64) as u64,
        })
        .map_err(|e| format!("VGIC_V3 DIST={dist_addr:#x}: {e}"))?;

        let redist_addr = GICR_BASE;
        gic.set_device_attr(&kvm_device_attr {
            flags: 0,
            group: KVM_DEV_ARM_VGIC_GRP_ADDR,
            attr: KVM_VGIC_V3_ADDR_TYPE_REDIST as u64,
            addr: (&redist_addr as *const u64) as u64,
        })
        .map_err(|e| format!("VGIC_V3 REDIST={redist_addr:#x}: {e}"))?;

        gic.set_device_attr(&kvm_device_attr {
            flags: 0,
            group: KVM_DEV_ARM_VGIC_GRP_CTRL,
            attr: KVM_DEV_ARM_VGIC_CTRL_INIT as u64,
            addr: 0,
        })
        .map_err(|e| format!("VGIC_V3 CTRL_INIT: {e}"))?;

        Ok(gic)
    }

    pub fn spi_backend(&self) -> Arc<dyn SpiBackend> {
        Arc::new(KvmSpiBackend {
            vm: Arc::clone(&self.vm),
        })
    }

    pub fn mpidrs(&self) -> &[u64] {
        &self.mpidrs
    }

    pub fn id_aa64mmfr0_el1(&self) -> u64 {
        self.id_aa64mmfr0_el1
    }

    pub fn ipa_bits(&self) -> u32 {
        self.ipa_bits
    }

    pub fn configure_vcpu0_boot(&self) -> Result<(), String> {
        let vcpu0 = self
            .vcpus
            .first()
            .ok_or_else(|| "no vcpu0 available".to_string())?;
        set_reg_u64(vcpu0, REG_PSTATE, PSTATE_EL1H_MASKED)
            .map_err(|e| format!("set vcpu0 PSTATE: {e}"))?;
        set_reg_u64(vcpu0, REG_PC, KERNEL_GPA).map_err(|e| format!("set vcpu0 PC: {e}"))?;
        set_reg_u64(vcpu0, REG_X0, FDT_GPA).map_err(|e| format!("set vcpu0 x0: {e}"))?;
        set_reg_u64(vcpu0, REG_X1, 0).map_err(|e| format!("set vcpu0 x1: {e}"))?;
        set_reg_u64(vcpu0, REG_X2, 0).map_err(|e| format!("set vcpu0 x2: {e}"))?;
        set_reg_u64(vcpu0, REG_X3, 0).map_err(|e| format!("set vcpu0 x3: {e}"))?;
        Ok(())
    }

    pub fn run<FP, FR>(
        self,
        mmio: Arc<MmioBus>,
        pin_cpus: &[usize],
        sig_fd: &OwnedFd,
        on_pause: FP,
        on_resume: FR,
    ) -> Result<(), String>
    where
        FP: Fn(),
        FR: Fn(),
    {
        install_kick_signal_handler();
        let wake_fd = Arc::new(create_eventfd()?);

        let num_vcpus = self.vcpus.len();
        let shutdown = Arc::new(AtomicBool::new(false));
        let paused = Arc::new(AtomicBool::new(false));
        let pause_sync = Arc::new((
            Mutex::new(PauseState {
                paused: false,
                parked: 0,
            }),
            Condvar::new(),
        ));
        let tids: Arc<Mutex<Vec<usize>>> = Arc::new(Mutex::new(Vec::with_capacity(num_vcpus)));
        let alive = Arc::new((Mutex::new(0usize), Condvar::new()));
        let first_err: Arc<Mutex<Option<String>>> = Arc::new(Mutex::new(None));

        struct TidGuard {
            tid: usize,
            tids: Arc<Mutex<Vec<usize>>>,
            alive: Arc<(Mutex<usize>, Condvar)>,
        }
        impl Drop for TidGuard {
            fn drop(&mut self) {
                self.tids.lock().unwrap().retain(|&t| t != self.tid);
                let mut count = self.alive.0.lock().unwrap();
                *count = count.saturating_sub(1);
                if *count == 0 {
                    self.alive.1.notify_all();
                }
            }
        }

        let mut handles = Vec::with_capacity(num_vcpus);
        for (cpu_idx, mut vcpu) in self.vcpus.into_iter().enumerate() {
            let t_mmio = Arc::clone(&mmio);
            let t_shutdown = Arc::clone(&shutdown);
            let t_paused = Arc::clone(&paused);
            let t_pause_sync = Arc::clone(&pause_sync);
            let t_tids = Arc::clone(&tids);
            let t_alive = Arc::clone(&alive);
            let t_first_err = Arc::clone(&first_err);
            let wake_fd_thread = Arc::clone(&wake_fd);
            let pin_target = if pin_cpus.is_empty() {
                None
            } else {
                Some(pin_cpus[cpu_idx % pin_cpus.len()])
            };

            *alive.0.lock().unwrap() += 1;
            let spawn_res = thread::Builder::new()
                .name(format!("vcpu-{cpu_idx}"))
                .spawn(move || {
                    if let Some(host_cpu) = pin_target {
                        let _ = pin_current_thread(&[host_cpu]);
                    }
                    let my_tid = unsafe { libc::pthread_self() as usize };
                    t_tids.lock().unwrap().push(my_tid);
                    let _guard = TidGuard {
                        tid: my_tid,
                        tids: Arc::clone(&t_tids),
                        alive: Arc::clone(&t_alive),
                    };

                    if let Err(msg) = configure_vcpu_signal_mask(&vcpu) {
                        let mut slot = t_first_err.lock().unwrap();
                        if slot.is_none() {
                            *slot = Some(format!("vcpu{cpu_idx}: {msg}"));
                        }
                        signal_shutdown(&t_shutdown, &t_tids, wake_fd_thread.as_raw_fd(), my_tid);
                        return;
                    }

                    while !t_shutdown.load(Ordering::Acquire) {
                        if t_paused.load(Ordering::Acquire) {
                            let (mu, cv) = &*t_pause_sync;
                            let mut st = mu.lock().unwrap();
                            st.parked += 1;
                            cv.notify_all();
                            while st.paused && !t_shutdown.load(Ordering::Acquire) {
                                st = cv.wait(st).unwrap();
                            }
                            st.parked = st.parked.saturating_sub(1);
                            cv.notify_all();
                            drop(st);
                            drain_pending_kick_sig();
                            continue;
                        }

                        match vcpu.run() {
                            Ok(VcpuExit::MmioRead(addr, data)) => {
                                t_mmio.read(addr, data);
                            }
                            Ok(VcpuExit::MmioWrite(addr, data)) => {
                                t_mmio.write(addr, data);
                            }
                            Ok(VcpuExit::SystemEvent(ev_type, _)) => {
                                if ev_type != KVM_SYSTEM_EVENT_SHUTDOWN
                                    && ev_type != KVM_SYSTEM_EVENT_RESET
                                {
                                    let msg =
                                        format!("vcpu{cpu_idx} unexpected system event {ev_type}");
                                    let mut slot = t_first_err.lock().unwrap();
                                    if slot.is_none() {
                                        *slot = Some(msg);
                                    }
                                }
                                signal_shutdown(
                                    &t_shutdown,
                                    &t_tids,
                                    wake_fd_thread.as_raw_fd(),
                                    my_tid,
                                );
                                break;
                            }
                            Ok(VcpuExit::Shutdown) | Ok(VcpuExit::Hlt) => {
                                signal_shutdown(
                                    &t_shutdown,
                                    &t_tids,
                                    wake_fd_thread.as_raw_fd(),
                                    my_tid,
                                );
                                break;
                            }
                            Ok(VcpuExit::Intr) => {
                                drain_pending_kick_sig();
                            }
                            Ok(other) => {
                                let msg = format!("vcpu{cpu_idx} unexpected KVM exit: {other:?}");
                                let mut slot = t_first_err.lock().unwrap();
                                if slot.is_none() {
                                    *slot = Some(msg);
                                }
                                signal_shutdown(
                                    &t_shutdown,
                                    &t_tids,
                                    wake_fd_thread.as_raw_fd(),
                                    my_tid,
                                );
                                break;
                            }
                            Err(e) => {
                                let errno = e.errno();
                                if errno == libc::EINTR {
                                    drain_pending_kick_sig();
                                    continue;
                                }
                                if errno == libc::EAGAIN {
                                    continue;
                                }
                                let msg = format!("vcpu{cpu_idx} KVM_RUN failed: {e}");
                                let mut slot = t_first_err.lock().unwrap();
                                if slot.is_none() {
                                    *slot = Some(msg);
                                }
                                signal_shutdown(
                                    &t_shutdown,
                                    &t_tids,
                                    wake_fd_thread.as_raw_fd(),
                                    my_tid,
                                );
                                break;
                            }
                        }
                    }
                });

            match spawn_res {
                Ok(h) => handles.push(h),
                Err(e) => {
                    *alive.0.lock().unwrap() -= 1;
                    shutdown.store(true, Ordering::Release);
                    let mut count = alive.0.lock().unwrap();
                    while *count > 0 {
                        kick_all_vcpus(&tids, None);
                        let (guard, _) = alive
                            .1
                            .wait_timeout(count, Duration::from_millis(10))
                            .unwrap();
                        count = guard;
                    }
                    drop(count);
                    for h in handles {
                        let _ = h.join();
                    }
                    return Err(format!("spawn vcpu-{cpu_idx}: {e}"));
                }
            }
        }

        let mut fds = [
            libc::pollfd {
                fd: sig_fd.as_raw_fd(),
                events: libc::POLLIN,
                revents: 0,
            },
            libc::pollfd {
                fd: wake_fd.as_raw_fd(),
                events: libc::POLLIN,
                revents: 0,
            },
        ];
        let err_mask = libc::POLLERR | libc::POLLHUP | libc::POLLNVAL;
        loop {
            let rc = unsafe { libc::poll(fds.as_mut_ptr(), fds.len() as libc::nfds_t, -1) };
            if rc < 0 {
                let err = std::io::Error::last_os_error()
                    .raw_os_error()
                    .unwrap_or(libc::EINTR);
                if err == libc::EINTR {
                    continue;
                }
                break;
            }
            if ((fds[0].revents | fds[1].revents) & err_mask) != 0 {
                shutdown.store(true, Ordering::Release);
                break;
            }
            if (fds[1].revents & libc::POLLIN) != 0 {
                shutdown.store(true, Ordering::Release);
                break;
            }
            if (fds[0].revents & libc::POLLIN) != 0 {
                let mut should_stop = false;
                while let Some(sig) = read_signalfd_sig(sig_fd.as_raw_fd()) {
                    match sig {
                        libc::SIGUSR1 => {
                            paused.store(true, Ordering::Release);
                            let (mu, cv) = &*pause_sync;
                            let mut st = mu.lock().unwrap();
                            st.paused = true;
                            while st.parked < *alive.0.lock().unwrap()
                                && !shutdown.load(Ordering::Acquire)
                            {
                                kick_all_vcpus(&tids, None);
                                let (g, _) =
                                    cv.wait_timeout(st, Duration::from_millis(10)).unwrap();
                                st = g;
                            }
                            drop(st);
                            if !shutdown.load(Ordering::Acquire) {
                                on_pause();
                            }
                        }
                        libc::SIGUSR2 => {
                            if !shutdown.load(Ordering::Acquire) {
                                on_resume();
                            }
                            paused.store(false, Ordering::Release);
                            let (mu, cv) = &*pause_sync;
                            let mut st = mu.lock().unwrap();
                            st.paused = false;
                            cv.notify_all();
                            drop(st);
                        }
                        _ => {
                            should_stop = true;
                        }
                    }
                }
                if should_stop {
                    shutdown.store(true, Ordering::Release);
                    break;
                }
            }
        }

        paused.store(false, Ordering::Release);
        {
            let (mu, cv) = &*pause_sync;
            let mut st = mu.lock().unwrap();
            st.paused = false;
            cv.notify_all();
        }
        let mut count = alive.0.lock().unwrap();
        while *count > 0 {
            kick_all_vcpus(&tids, None);
            let (guard, _) = alive
                .1
                .wait_timeout(count, Duration::from_millis(10))
                .unwrap();
            count = guard;
        }
        drop(count);

        for h in handles {
            let _ = h.join();
        }

        if let Some(err) = first_err.lock().unwrap().take() {
            return Err(err);
        }
        Ok(())
    }
}

#[cfg(target_os = "linux")]
pub fn setup_control_signalfd() -> Result<OwnedFd, String> {
    unsafe {
        let mut sfd_mask: libc::sigset_t = std::mem::zeroed();
        libc::sigemptyset(&mut sfd_mask);
        libc::sigaddset(&mut sfd_mask, libc::SIGTERM);
        libc::sigaddset(&mut sfd_mask, libc::SIGINT);
        libc::sigaddset(&mut sfd_mask, libc::SIGUSR1);
        libc::sigaddset(&mut sfd_mask, libc::SIGUSR2);

        let mut block_mask = sfd_mask;
        libc::sigaddset(&mut block_mask, vcpu_kick_sig());
        if libc::pthread_sigmask(libc::SIG_BLOCK, &block_mask, std::ptr::null_mut()) != 0 {
            return Err(format!(
                "pthread_sigmask failed: {}",
                std::io::Error::last_os_error()
            ));
        }
        let fd = libc::signalfd(-1, &sfd_mask, libc::SFD_NONBLOCK | libc::SFD_CLOEXEC);
        if fd < 0 {
            return Err(format!(
                "signalfd failed: {}",
                std::io::Error::last_os_error()
            ));
        }
        Ok(OwnedFd::from_raw_fd(fd))
    }
}

#[cfg(target_os = "linux")]
fn configure_vcpu_signal_mask(vcpu: &VcpuFd) -> Result<(), String> {
    let kick = vcpu_kick_sig();
    unsafe {
        let mut cur: libc::sigset_t = std::mem::zeroed();
        if libc::pthread_sigmask(libc::SIG_SETMASK, std::ptr::null(), &mut cur) != 0 {
            return Err(format!(
                "pthread_sigmask query failed: {}",
                std::io::Error::last_os_error()
            ));
        }
        libc::sigdelset(&mut cur, kick);
        let mut kvm_mask = KvmSignalMask {
            len: 8,
            sigset: [0u8; 8],
        };
        std::ptr::copy_nonoverlapping(
            (&cur as *const libc::sigset_t).cast::<u8>(),
            kvm_mask.sigset.as_mut_ptr(),
            8,
        );
        let rc = libc::ioctl(
            vcpu.as_raw_fd(),
            KVM_SET_SIGNAL_MASK as _,
            &kvm_mask as *const KvmSignalMask,
        );
        if rc != 0 {
            return Err(format!(
                "KVM_SET_SIGNAL_MASK failed: {}",
                std::io::Error::last_os_error()
            ));
        }
    }
    Ok(())
}

#[cfg(target_os = "linux")]
fn drain_pending_kick_sig() {
    let kick = vcpu_kick_sig();
    unsafe {
        let mut set: libc::sigset_t = std::mem::zeroed();
        libc::sigemptyset(&mut set);
        libc::sigaddset(&mut set, kick);
        let ts = libc::timespec {
            tv_sec: 0,
            tv_nsec: 0,
        };
        while libc::sigtimedwait(&set, std::ptr::null_mut(), &ts) > 0 {}
    }
}

#[cfg(target_os = "linux")]
fn read_signalfd_sig(fd: libc::c_int) -> Option<libc::c_int> {
    let mut info: libc::signalfd_siginfo = unsafe { std::mem::zeroed() };
    let n = unsafe {
        libc::read(
            fd,
            (&mut info as *mut libc::signalfd_siginfo).cast::<libc::c_void>(),
            std::mem::size_of::<libc::signalfd_siginfo>(),
        )
    };
    if n as usize == std::mem::size_of::<libc::signalfd_siginfo>() {
        Some(info.ssi_signo as libc::c_int)
    } else {
        None
    }
}

#[cfg(target_os = "linux")]
fn set_reg_u64(vcpu: &VcpuFd, reg_id: u64, val: u64) -> Result<(), kvm_ioctls::Error> {
    vcpu.set_one_reg(reg_id, &val.to_ne_bytes()).map(|_| ())
}

#[cfg(target_os = "linux")]
fn get_reg_u64(vcpu: &VcpuFd, reg_id: u64) -> Result<u64, kvm_ioctls::Error> {
    let mut buf = [0u8; 8];
    vcpu.get_one_reg(reg_id, &mut buf)?;
    Ok(u64::from_ne_bytes(buf))
}

#[cfg(target_os = "linux")]
fn install_kick_signal_handler() {
    let kick = vcpu_kick_sig();
    unsafe {
        let mut sa: libc::sigaction = std::mem::zeroed();
        sa.sa_sigaction = noop_sig_handler as usize;
        sa.sa_flags = 0;
        libc::sigemptyset(&mut sa.sa_mask);
        libc::sigaction(kick, &sa, std::ptr::null_mut());
    }
}

#[cfg(target_os = "linux")]
fn create_eventfd() -> Result<OwnedFd, String> {
    let fd = unsafe { libc::eventfd(0, libc::EFD_NONBLOCK | libc::EFD_CLOEXEC) };
    if fd < 0 {
        return Err(format!(
            "eventfd failed: {}",
            std::io::Error::last_os_error()
        ));
    }
    Ok(unsafe { OwnedFd::from_raw_fd(fd) })
}

#[cfg(target_os = "linux")]
fn kick_all_vcpus(tids: &Mutex<Vec<usize>>, skip: Option<usize>) {
    let kick = vcpu_kick_sig();
    let list = tids.lock().unwrap();
    for &tid in list.iter() {
        if skip == Some(tid) {
            continue;
        }
        unsafe {
            libc::pthread_kill(tid as libc::pthread_t, kick);
        }
    }
}

#[cfg(target_os = "linux")]
fn signal_shutdown(
    shutdown: &AtomicBool,
    tids: &Mutex<Vec<usize>>,
    wake_fd: libc::c_int,
    self_tid: usize,
) {
    shutdown.store(true, Ordering::Release);
    kick_all_vcpus(tids, Some(self_tid));
    let val: u64 = 1;
    unsafe {
        libc::write(
            wake_fd,
            (&val as *const u64).cast::<libc::c_void>(),
            std::mem::size_of::<u64>(),
        );
    }
}
