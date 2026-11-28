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
    kvm_create_device, kvm_device_attr, kvm_device_type_KVM_DEV_TYPE_ARM_VGIC_V2,
    kvm_device_type_KVM_DEV_TYPE_ARM_VGIC_V3, kvm_regs, kvm_userspace_memory_region,
    kvm_vcpu_init, user_pt_regs, KVM_ARM_IRQ_NUM_MASK, KVM_ARM_IRQ_TYPE_SHIFT,
    KVM_ARM_IRQ_TYPE_SPI, KVM_ARM_VCPU_POWER_OFF, KVM_ARM_VCPU_PSCI_0_2,
    KVM_DEV_ARM_VGIC_CTRL_INIT, KVM_DEV_ARM_VGIC_GRP_ADDR, KVM_DEV_ARM_VGIC_GRP_CTRL,
    KVM_DEV_ARM_VGIC_GRP_NR_IRQS, KVM_REG_ARM64, KVM_REG_ARM64_SYSREG,
    KVM_REG_ARM64_SYSREG_CRM_MASK, KVM_REG_ARM64_SYSREG_CRM_SHIFT, KVM_REG_ARM64_SYSREG_CRN_MASK,
    KVM_REG_ARM64_SYSREG_CRN_SHIFT, KVM_REG_ARM64_SYSREG_OP0_MASK, KVM_REG_ARM64_SYSREG_OP0_SHIFT,
    KVM_REG_ARM64_SYSREG_OP1_MASK, KVM_REG_ARM64_SYSREG_OP1_SHIFT, KVM_REG_ARM64_SYSREG_OP2_MASK,
    KVM_REG_ARM64_SYSREG_OP2_SHIFT, KVM_REG_ARM_CORE, KVM_REG_SIZE_U64, KVM_SYSTEM_EVENT_RESET,
    KVM_SYSTEM_EVENT_SHUTDOWN, KVM_VGIC_V2_ADDR_TYPE_CPU, KVM_VGIC_V2_ADDR_TYPE_DIST,
    KVM_VGIC_V3_ADDR_TYPE_DIST, KVM_VGIC_V3_ADDR_TYPE_REDIST,
};
#[cfg(target_os = "linux")]
use kvm_ioctls::{DeviceFd, Kvm, VcpuExit, VcpuFd, VmFd};

#[cfg(target_os = "linux")]
use crate::affinity::pin_current_thread;
#[cfg(target_os = "linux")]
use crate::boot::{GicVersion, FDT_GPA, GICC_BASE, GICD_BASE, GICR_BASE, KERNEL_GPA};
#[cfg(target_os = "linux")]
use crate::irq::SpiBackend;
#[cfg(target_os = "linux")]
use crate::memory::GuestMemory;
#[cfg(target_os = "linux")]
use crate::mmio::MmioBus;
#[cfg(target_os = "linux")]
use crate::pci::ShmRegionMapper;
#[cfg(target_os = "linux")]
use crate::virtio::metal::{ShmBacking, ShmDirtySync};

#[cfg(target_os = "linux")]
const KVM_API_VERSION: i32 = 12;
#[cfg(target_os = "linux")]
const VGIC_NR_IRQS: u32 = 128;
#[cfg(target_os = "linux")]
const PSTATE_EL1H_MASKED: u64 = 0x3c5;
#[cfg(target_os = "linux")]
const KVM_SET_SIGNAL_MASK: libc::c_ulong = 0x4004_ae8b;
#[cfg(target_os = "linux")]
const KVM_MEM_LOG_DIRTY_PAGES: u32 = 1 << 0;
#[cfg(target_os = "linux")]
const KVM_CAP_MANUAL_DIRTY_LOG_PROTECT2: libc::c_ulong = 168;
#[cfg(target_os = "linux")]
const KVM_DIRTY_LOG_MANUAL_PROTECT_ENABLE: u64 = 1 << 0;
#[cfg(target_os = "linux")]
const KVM_CHECK_EXTENSION: libc::Ioctl = 0x0000_ae03u32 as libc::Ioctl;
#[cfg(target_os = "linux")]
const KVM_GET_DIRTY_LOG: libc::Ioctl = 0x4010_ae42u32 as libc::Ioctl;
#[cfg(target_os = "linux")]
const KVM_ENABLE_CAP: libc::Ioctl = 0x4068_aea3u32 as libc::Ioctl;
#[cfg(target_os = "linux")]
const KVM_CLEAR_DIRTY_LOG: libc::Ioctl = 0xc018_aec0u32 as libc::Ioctl;

#[cfg(target_os = "linux")]
#[repr(C)]
struct KvmEnableCap {
    cap: u32,
    flags: u32,
    args: [u64; 4],
    pad: [u8; 64],
}

#[cfg(target_os = "linux")]
#[repr(C)]
struct KvmDirtyLog {
    slot: u32,
    padding1: u32,
    dirty_bitmap: *mut libc::c_void,
}

#[cfg(target_os = "linux")]
#[repr(C)]
struct KvmClearDirtyLog {
    slot: u32,
    num_pages: u32,
    first_page: u64,
    dirty_bitmap: *mut libc::c_void,
}

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
struct ShmMapState {
    gpa: Option<u64>,
    dirty_log_enabled: bool,
    dirty_bitmap: Vec<u64>,
}

#[cfg(target_os = "linux")]
pub struct KvmShmMapper {
    vm: Arc<VmFd>,
    slot: u32,
    backing: Arc<ShmBacking>,
    host_page_size: usize,
    manual_dirty_clear: bool,
    allow_dirty_log: bool,
    dma_coherent: bool,
    state: Mutex<ShmMapState>,
}

#[cfg(all(target_os = "linux", target_arch = "aarch64"))]
#[inline]
fn arm64_dcache_line_size() -> usize {
    static LINE: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
    let cached = LINE.load(Ordering::Relaxed);
    if cached != 0 {
        return cached;
    }
    let ctr: u64;
    unsafe {
        std::arch::asm!("mrs {}, ctr_el0", out(reg) ctr, options(nomem, nostack, preserves_flags));
    }
    let dmin = (ctr >> 16) & 0xf;
    let mut line = 4usize << dmin;
    if !(16..=256).contains(&line) {
        line = 64;
    }
    LINE.store(line, Ordering::Relaxed);
    line
}

#[cfg(all(target_os = "linux", target_arch = "aarch64"))]
#[inline]
fn arm64_clean_inv_dcache_range_no_dsb(ptr: *const u8, len: usize, line: usize) {
    if len == 0 {
        return;
    }
    let mut cur = (ptr as usize) & !(line - 1);
    let end = (ptr as usize).saturating_add(len);
    while cur < end {
        unsafe {
            std::arch::asm!("dc civac, {}", in(reg) cur, options(nostack, preserves_flags));
        }
        cur += line;
    }
}

#[cfg(all(target_os = "linux", target_arch = "aarch64"))]
#[inline]
fn arm64_dsb_sy() {
    unsafe {
        std::arch::asm!("dsb sy", options(nostack, preserves_flags));
    }
}

#[cfg(target_os = "linux")]
impl KvmShmMapper {
    fn flush_full_shm_to_poc(&self) {
        if self.dma_coherent {
            return;
        }
        #[cfg(target_arch = "aarch64")]
        {
            let size = self.backing.size() as usize;
            if size > 0 {
                let line = arm64_dcache_line_size();
                arm64_clean_inv_dcache_range_no_dsb(self.backing.host_ptr(), size, line);
                arm64_dsb_sy();
            }
        }
    }
    fn set_region_retry(&self, region: kvm_userspace_memory_region) -> Result<(), String> {
        loop {
            match unsafe { self.vm.set_user_memory_region(region) } {
                Ok(()) => return Ok(()),
                Err(e) if e.errno() == libc::EINTR => continue,
                Err(e) => {
                    return Err(format!(
                        "KVM_SET_USER_MEMORY_REGION(slot={}, gpa={:#x}, size={:#x}): {e}",
                        region.slot, region.guest_phys_addr, region.memory_size
                    ));
                }
            }
        }
    }

    fn ioctl_get_dirty_log(&self, bitmap: &mut [u64]) -> bool {
        let log = KvmDirtyLog {
            slot: self.slot,
            padding1: 0,
            dirty_bitmap: bitmap.as_mut_ptr() as *mut libc::c_void,
        };
        loop {
            let rc = unsafe { libc::ioctl(self.vm.as_raw_fd(), KVM_GET_DIRTY_LOG, &log) };
            if rc == 0 {
                return true;
            }
            if std::io::Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
                continue;
            }
            return false;
        }
    }

    fn ioctl_clear_dirty_log(&self, first_page: u64, num_pages: u32, sub_bitmap: &mut [u64]) -> bool {
        let mut clear = KvmClearDirtyLog {
            slot: self.slot,
            num_pages,
            first_page,
            dirty_bitmap: sub_bitmap.as_mut_ptr() as *mut libc::c_void,
        };
        loop {
            let rc = unsafe { libc::ioctl(self.vm.as_raw_fd(), KVM_CLEAR_DIRTY_LOG, &mut clear) };
            if rc == 0 {
                return true;
            }
            if std::io::Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
                continue;
            }
            return false;
        }
    }
}

#[cfg(target_os = "linux")]
impl ShmRegionMapper for KvmShmMapper {
    fn map(&self, gpa: u64) -> Result<(), String> {
        let mut st = self.state.lock().unwrap();
        if st.gpa == Some(gpa) {
            return Ok(());
        }
        if let Some(old_gpa) = st.gpa {
            let unmap = kvm_userspace_memory_region {
                slot: self.slot,
                flags: 0,
                guest_phys_addr: old_gpa,
                memory_size: 0,
                userspace_addr: 0,
            };
            self.set_region_retry(unmap)?;
            // Deleting the memslot destroys its KVM dirty bitmap; flush any guest-dirtied
            // cache lines to PoC now that Stage-2 mappings are torn down.
            self.flush_full_shm_to_poc();
            st.gpa = None;
            st.dirty_log_enabled = false;
        }
        let shm_size = self.backing.size();
        let can_track = self.allow_dirty_log
            && self.host_page_size > 0
            && (shm_size as usize).is_multiple_of(self.host_page_size);
        let mut dirty_enabled = false;
        if can_track {
            let region = kvm_userspace_memory_region {
                slot: self.slot,
                flags: KVM_MEM_LOG_DIRTY_PAGES,
                guest_phys_addr: gpa,
                memory_size: shm_size,
                userspace_addr: self.backing.host_ptr() as u64,
            };
            if self.set_region_retry(region).is_ok() {
                dirty_enabled = true;
            }
        }
        if !dirty_enabled {
            let region = kvm_userspace_memory_region {
                slot: self.slot,
                flags: 0,
                guest_phys_addr: gpa,
                memory_size: shm_size,
                userspace_addr: self.backing.host_ptr() as u64,
            };
            self.set_region_retry(region)?;
        }
        if dirty_enabled {
            let total_pages = (shm_size as usize) / self.host_page_size;
            let n_words = total_pages.div_ceil(64);
            st.dirty_bitmap.clear();
            st.dirty_bitmap.resize(n_words, 0);
        } else {
            st.dirty_bitmap.clear();
        }
        st.dirty_log_enabled = dirty_enabled;
        st.gpa = Some(gpa);
        Ok(())
    }

    fn unmap(&self) {
        let mut st = self.state.lock().unwrap();
        if let Some(old_gpa) = st.gpa {
            let unmap = kvm_userspace_memory_region {
                slot: self.slot,
                flags: 0,
                guest_phys_addr: old_gpa,
                memory_size: 0,
                userspace_addr: 0,
            };
            match self.set_region_retry(unmap) {
                Ok(()) => {
                    self.flush_full_shm_to_poc();
                    st.gpa = None;
                    st.dirty_log_enabled = false;
                }
                Err(e) => eprintln!("kido-vmm: {e}"),
            }
        }
    }
}

#[cfg(target_os = "linux")]
impl ShmDirtySync for KvmShmMapper {
    fn sync_dirty_to_poc(&self) -> bool {
        #[cfg(not(target_arch = "aarch64"))]
        {
            return false;
        }
        #[cfg(target_arch = "aarch64")]
        {
            let mut st = self.state.lock().unwrap();
            if st.gpa.is_none() || !st.dirty_log_enabled {
                return false;
            }
            let total_pages = (self.backing.size() as usize) / self.host_page_size;
            let n_words = total_pages.div_ceil(64);
            if total_pages == 0 || st.dirty_bitmap.len() != n_words {
                return false;
            }
            if !self.ioctl_get_dirty_log(&mut st.dirty_bitmap) {
                return false;
            }

            let mut first_nz: Option<usize> = None;
            let mut last_nz: usize = 0;
            for (idx, &word) in st.dirty_bitmap.iter().enumerate() {
                if word != 0 {
                    if first_nz.is_none() {
                        first_nz = Some(idx);
                    }
                    last_nz = idx;
                }
            }
            let Some(first_w) = first_nz else {
                return true;
            };

            // Clear dirty log & write-protect dirty Stage-2 PTEs BEFORE dc civac so any
            // concurrent guest store after Stage-2 TLBI sets a fresh dirty bit for the next commit.
            if self.manual_dirty_clear {
                let first_page = (first_w * 64) as u64;
                let max_pages = ((last_nz + 1 - first_w) * 64).min(total_pages - first_w * 64);
                let Ok(max_pages_u32) = u32::try_from(max_pages) else {
                    return false;
                };
                if !self.ioctl_clear_dirty_log(
                    first_page,
                    max_pages_u32,
                    &mut st.dirty_bitmap[first_w..=last_nz],
                ) {
                    return false;
                }
            }

            let base = self.backing.host_ptr();
            let page_sz = self.host_page_size;
            let line = arm64_dcache_line_size();
            let mut span_start: Option<usize> = None;
            let mut span_end: usize = 0;

            for (rel_idx, &bitmap_word) in st.dirty_bitmap[first_w..=last_nz].iter().enumerate() {
                let w_idx = first_w + rel_idx;
                let mut word = bitmap_word;
                if word == 0 {
                    if let Some(s) = span_start.take() {
                        let off = s * page_sz;
                        let len = (span_end - s) * page_sz;
                        arm64_clean_inv_dcache_range_no_dsb(unsafe { base.add(off) }, len, line);
                    }
                    continue;
                }
                while word != 0 {
                    let tz = word.trailing_zeros() as usize;
                    let page = w_idx * 64 + tz;
                    if page >= total_pages {
                        break;
                    }
                    let shifted = word >> tz;
                    let run = (shifted.trailing_ones() as usize).min(total_pages - page);
                    let run_end = page + run;
                    match span_start {
                        Some(_) if span_end == page => {
                            span_end = run_end;
                        }
                        Some(s) => {
                            let off = s * page_sz;
                            let len = (span_end - s) * page_sz;
                            arm64_clean_inv_dcache_range_no_dsb(unsafe { base.add(off) }, len, line);
                            span_start = Some(page);
                            span_end = run_end;
                        }
                        None => {
                            span_start = Some(page);
                            span_end = run_end;
                        }
                    }
                    if tz + run >= 64 {
                        word = 0;
                    } else {
                        word &= !(((1u64 << run) - 1) << tz);
                    }
                }
            }
            if let Some(s) = span_start {
                let off = s * page_sz;
                let len = (span_end - s) * page_sz;
                arm64_clean_inv_dcache_range_no_dsb(unsafe { base.add(off) }, len, line);
            }
            arm64_dsb_sy();
            true
        }
    }
}

#[cfg(target_os = "linux")]
impl Drop for KvmShmMapper {
    fn drop(&mut self) {
        self.unmap();
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
    gic_version: GicVersion,
    vm: Arc<VmFd>,
    _kvm: Kvm,
    _mem: Arc<GuestMemory>,
    mpidrs: Vec<u64>,
    id_aa64mmfr0_el1: u64,
    ipa_bits: u32,
    manual_dirty_clear: bool,
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

        let mut manual_dirty_clear = false;
        let cap_bits = unsafe {
            libc::ioctl(
                vm.as_raw_fd(),
                KVM_CHECK_EXTENSION,
                KVM_CAP_MANUAL_DIRTY_LOG_PROTECT2,
            )
        };
        if cap_bits > 0 && ((cap_bits as u64) & KVM_DIRTY_LOG_MANUAL_PROTECT_ENABLE) != 0 {
            let enable = KvmEnableCap {
                cap: KVM_CAP_MANUAL_DIRTY_LOG_PROTECT2 as u32,
                flags: 0,
                args: [KVM_DIRTY_LOG_MANUAL_PROTECT_ENABLE, 0, 0, 0],
                pad: [0u8; 64],
            };
            if unsafe { libc::ioctl(vm.as_raw_fd(), KVM_ENABLE_CAP, &enable) } == 0 {
                manual_dirty_clear = true;
            }
        }

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

        let (gic, gic_version) = Self::init_vgic(&vm, num_cpus)?;

        Ok(Self {
            vcpus,
            _gic: gic,
            gic_version,
            vm,
            _kvm: kvm,
            _mem: mem,
            mpidrs,
            id_aa64mmfr0_el1,
            ipa_bits,
            manual_dirty_clear,
        })
    }

    fn init_vgic(vm: &VmFd, num_cpus: usize) -> Result<(DeviceFd, GicVersion), String> {
        let mut dev_v3 = kvm_create_device {
            type_: kvm_device_type_KVM_DEV_TYPE_ARM_VGIC_V3,
            fd: 0,
            flags: 0,
        };
        match vm.create_device(&mut dev_v3) {
            Ok(gic) => {
                Self::configure_vgic_v3(&gic)?;
                Ok((gic, GicVersion::V3))
            }
            Err(e)
                if matches!(
                    e.errno(),
                    libc::ENODEV | libc::ENXIO | libc::EOPNOTSUPP
                ) =>
            {
                if num_cpus > 8 {
                    return Err(format!(
                        "GICv2 host supports at most 8 vCPUs (requested {num_cpus})"
                    ));
                }
                let mut dev_v2 = kvm_create_device {
                    type_: kvm_device_type_KVM_DEV_TYPE_ARM_VGIC_V2,
                    fd: 0,
                    flags: 0,
                };
                let gic = vm
                    .create_device(&mut dev_v2)
                    .map_err(|e2| format!("KVM_CREATE_DEVICE(VGIC_V2): {e2}"))?;
                Self::configure_vgic_v2(&gic)?;
                Ok((gic, GicVersion::V2))
            }
            Err(e) => Err(format!("KVM_CREATE_DEVICE(VGIC_V3): {e}")),
        }
    }

    fn configure_vgic_v3(gic: &DeviceFd) -> Result<(), String> {
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

        Ok(())
    }

    fn configure_vgic_v2(gic: &DeviceFd) -> Result<(), String> {
        let nr_irqs = VGIC_NR_IRQS;
        gic.set_device_attr(&kvm_device_attr {
            flags: 0,
            group: KVM_DEV_ARM_VGIC_GRP_NR_IRQS,
            attr: 0,
            addr: (&nr_irqs as *const u32) as u64,
        })
        .map_err(|e| format!("VGIC_V2 NR_IRQS={nr_irqs}: {e}"))?;

        let dist_addr = GICD_BASE;
        gic.set_device_attr(&kvm_device_attr {
            flags: 0,
            group: KVM_DEV_ARM_VGIC_GRP_ADDR,
            attr: KVM_VGIC_V2_ADDR_TYPE_DIST as u64,
            addr: (&dist_addr as *const u64) as u64,
        })
        .map_err(|e| format!("VGIC_V2 DIST={dist_addr:#x}: {e}"))?;

        let cpu_addr = GICC_BASE;
        gic.set_device_attr(&kvm_device_attr {
            flags: 0,
            group: KVM_DEV_ARM_VGIC_GRP_ADDR,
            attr: KVM_VGIC_V2_ADDR_TYPE_CPU as u64,
            addr: (&cpu_addr as *const u64) as u64,
        })
        .map_err(|e| format!("VGIC_V2 CPU={cpu_addr:#x}: {e}"))?;

        gic.set_device_attr(&kvm_device_attr {
            flags: 0,
            group: KVM_DEV_ARM_VGIC_GRP_CTRL,
            attr: KVM_DEV_ARM_VGIC_CTRL_INIT as u64,
            addr: 0,
        })
        .map_err(|e| format!("VGIC_V2 CTRL_INIT: {e}"))?;

        Ok(())
    }

    pub fn gic_version(&self) -> GicVersion {
        self.gic_version
    }

    pub fn spi_backend(&self) -> Arc<dyn SpiBackend> {
        Arc::new(KvmSpiBackend {
            vm: Arc::clone(&self.vm),
        })
    }

    pub fn shm_mapper(
        &self,
        slot: u32,
        backing: Arc<ShmBacking>,
        dma_coherent: bool,
    ) -> Arc<KvmShmMapper> {
        let ps = unsafe { libc::sysconf(libc::_SC_PAGESIZE) };
        let host_page_size = if ps > 0 { ps as usize } else { 4096 };
        let allow_dirty_log =
            !dma_coherent && std::env::var_os("KIDO_VMM_NO_DIRTY_LOG").is_none();
        Arc::new(KvmShmMapper {
            vm: Arc::clone(&self.vm),
            slot,
            backing,
            host_page_size,
            manual_dirty_clear: self.manual_dirty_clear,
            allow_dirty_log,
            dma_coherent,
            state: Mutex::new(ShmMapState {
                gpa: None,
                dirty_log_enabled: false,
                dirty_bitmap: Vec::new(),
            }),
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
        sa.sa_sigaction = noop_sig_handler as *const () as usize;
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
