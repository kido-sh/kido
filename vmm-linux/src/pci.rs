use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use crate::boot::{PCIE_MMIO32_BASE, PCIE_MMIO32_SIZE, PCIE_MMIO64_BASE, PCIE_MMIO64_SIZE};
use crate::virtio::pci::VirtioPciDevice;

pub const PCI_VENDOR_VIRTIO: u16 = 0x1af4;
pub const PCI_DEVICE_VIRTIO_NET: u16 = 0x1041;
pub const PCI_DEVICE_VIRTIO_BLK: u16 = 0x1042;
pub const PCI_DEVICE_VIRTIO_FS: u16 = 0x105a;
pub const PCI_DEVICE_VIRTIO_METAL: u16 = 0x1090;

const HOST_BRIDGE_VENDOR: u16 = 0x1b36;
const HOST_BRIDGE_DEVICE: u16 = 0x0008;

pub const BAR0_SIZE: u64 = 0x4000;
pub const BAR0_SIZE_MASK: u32 = !(BAR0_SIZE as u32 - 1);

const PCI_COMMAND_IO: u16 = 1 << 0;
const PCI_COMMAND_MEMORY: u16 = 1 << 1;
const PCI_COMMAND_BUS_MASTER: u16 = 1 << 2;
pub const PCI_COMMAND_INTX_DISABLE: u16 = 1 << 10;
const PCI_COMMAND_WRITABLE: u16 =
    PCI_COMMAND_IO | PCI_COMMAND_MEMORY | PCI_COMMAND_BUS_MASTER | PCI_COMMAND_INTX_DISABLE;

const PCI_STATUS_INT_STATUS: u16 = 1 << 3;
const PCI_STATUS_CAP_LIST: u16 = 1 << 4;

const PCI_CAP_ID_VNDR: u8 = 0x09;
const VIRTIO_PCI_CAP_COMMON_CFG: u8 = 1;
const VIRTIO_PCI_CAP_NOTIFY_CFG: u8 = 2;
const VIRTIO_PCI_CAP_ISR_CFG: u8 = 3;
const VIRTIO_PCI_CAP_DEVICE_CFG: u8 = 4;
const VIRTIO_PCI_CAP_SHARED_MEMORY_CFG: u8 = 8;

const PCI_BAR_MEM64_PREFETCH: u32 = 0x0c;

pub trait ShmRegionMapper: Send + Sync {
    fn map(&self, gpa: u64) -> Result<(), String>;
    fn unmap(&self);
}

pub struct ShmBarConfig {
    pub size: u64,
    pub bar_size: u64,
    pub mapper: Arc<dyn ShmRegionMapper>,
}

impl ShmBarConfig {
    pub fn new(size: u64, mapper: Arc<dyn ShmRegionMapper>) -> Result<Self, String> {
        if size == 0 || (size & 0x3fff) != 0 {
            return Err(format!("SHM size {size:#x} must be non-zero and 16 KiB aligned"));
        }
        let bar_size = size
            .checked_next_power_of_two()
            .ok_or_else(|| format!("SHM size {size:#x} overflows power-of-two BAR"))?
            .max(64 * 1024 * 1024);
        if bar_size > PCIE_MMIO64_SIZE {
            return Err(format!(
                "SHM BAR size {bar_size:#x} exceeds 64-bit PCIe window {PCIE_MMIO64_SIZE:#x}"
            ));
        }
        Ok(Self {
            size,
            bar_size,
            mapper,
        })
    }
}

pub const VIRTIO_COMMON_OFF: u32 = 0x0000;
pub const VIRTIO_COMMON_LEN: u32 = 0x0040;
pub const VIRTIO_ISR_OFF: u32 = 0x1000;
pub const VIRTIO_ISR_LEN: u32 = 0x0004;
pub const VIRTIO_DEVICE_OFF: u32 = 0x2000;
pub const VIRTIO_DEVICE_LEN: u32 = 0x0100;
pub const VIRTIO_NOTIFY_OFF: u32 = 0x3000;
pub const VIRTIO_NOTIFY_LEN: u32 = 0x1000;
pub const VIRTIO_NOTIFY_MULT: u32 = 4;

pub fn pci_intx_spi(dev: u8, pin: u8) -> u32 {
    3 + (((dev as u32) + (pin as u32).saturating_sub(1)) % 4)
}

fn write_le16(buf: &mut [u8; 256], off: usize, val: u16) {
    buf[off..off + 2].copy_from_slice(&val.to_le_bytes());
}

fn write_le32(buf: &mut [u8; 256], off: usize, val: u32) {
    buf[off..off + 4].copy_from_slice(&val.to_le_bytes());
}

fn read_le16(buf: &[u8; 256], off: usize) -> u16 {
    u16::from_le_bytes(buf[off..off + 2].try_into().unwrap())
}

fn read_le32(buf: &[u8; 256], off: usize) -> u32 {
    u32::from_le_bytes(buf[off..off + 4].try_into().unwrap())
}

struct HostBridge {
    config: Mutex<[u8; 256]>,
}

impl HostBridge {
    fn new() -> Self {
        let mut cfg = [0u8; 256];
        write_le16(&mut cfg, 0x00, HOST_BRIDGE_VENDOR);
        write_le16(&mut cfg, 0x02, HOST_BRIDGE_DEVICE);
        cfg[0x0b] = 0x06; // Bridge / Host bridge (0x060000)
        cfg[0x0e] = 0x00; // Type 0 header
        Self {
            config: Mutex::new(cfg),
        }
    }

    fn read(&self, reg: usize, data: &mut [u8]) {
        if reg >= 256 {
            data.fill(0);
            return;
        }
        let cfg = self.config.lock().unwrap();
        for (i, byte) in data.iter_mut().enumerate() {
            let idx = reg + i;
            *byte = if idx < 256 { cfg[idx] } else { 0 };
        }
    }

    fn write(&self, reg: usize, data: &[u8]) {
        if reg >= 256 {
            return;
        }
        let mut cfg = self.config.lock().unwrap();
        let mut cmd = read_le16(&cfg, 0x04);
        let mut cmd_bytes = cmd.to_le_bytes();
        let mut cmd_touched = false;
        for (i, &byte) in data.iter().enumerate() {
            let idx = reg + i;
            if idx == 0x04 || idx == 0x05 {
                cmd_bytes[idx - 0x04] = byte;
                cmd_touched = true;
            }
        }
        if cmd_touched {
            cmd = u16::from_le_bytes(cmd_bytes) & PCI_COMMAND_WRITABLE;
            write_le16(&mut cfg, 0x04, cmd);
        }
    }
}

struct PciSlot {
    dev_num: u8,
    config: Mutex<[u8; 256]>,
    active_bar0: AtomicU64,
    active_bar4: AtomicU64,
    bar4_probing: Mutex<bool>,
    shm: Option<ShmBarConfig>,
    virtio: Arc<VirtioPciDevice>,
}

impl PciSlot {
    fn new(dev_num: u8, virtio: Arc<VirtioPciDevice>, shm: Option<ShmBarConfig>) -> Self {
        let mut cfg = [0u8; 256];
        write_le16(&mut cfg, 0x00, PCI_VENDOR_VIRTIO);
        write_le16(&mut cfg, 0x02, virtio.pci_device_id());
        write_le16(&mut cfg, 0x04, 0x0000);
        write_le16(&mut cfg, 0x06, PCI_STATUS_CAP_LIST);
        cfg[0x08] = 0x01; // Revision ID >= 1 for Modern VirtIO 1.0
        let class = virtio.pci_class_code();
        cfg[0x09] = (class & 0xff) as u8;
        cfg[0x0a] = ((class >> 8) & 0xff) as u8;
        cfg[0x0b] = ((class >> 16) & 0xff) as u8;
        cfg[0x0e] = 0x00; // Single-function Type 0
        // BAR0..BAR5 initially 0 (probed & placed by IOPCIConfigurator); BAR4 marks 64-bit prefetchable when SHM is present.
        if shm.is_some() {
            write_le32(&mut cfg, 0x20, PCI_BAR_MEM64_PREFETCH);
        }
        write_le16(&mut cfg, 0x2c, PCI_VENDOR_VIRTIO);
        write_le16(&mut cfg, 0x2e, virtio.pci_subsystem_id());
        cfg[0x34] = 0x40; // Capabilities pointer
        cfg[0x3c] = 0x00; // Interrupt line (RW)
        cfg[0x3d] = 0x01; // INTA#

        // Cap 1 @ 0x40: COMMON_CFG (len 16, next 0x50)
        Self::write_virtio_cap(
            &mut cfg,
            0x40,
            0x50,
            16,
            VIRTIO_PCI_CAP_COMMON_CFG,
            0,
            VIRTIO_COMMON_OFF,
            VIRTIO_COMMON_LEN,
        );
        // Cap 2 @ 0x50: NOTIFY_CFG (len 20, next 0x64)
        Self::write_virtio_cap(
            &mut cfg,
            0x50,
            0x64,
            20,
            VIRTIO_PCI_CAP_NOTIFY_CFG,
            0,
            VIRTIO_NOTIFY_OFF,
            VIRTIO_NOTIFY_LEN,
        );
        write_le32(&mut cfg, 0x50 + 16, VIRTIO_NOTIFY_MULT);
        // Cap 3 @ 0x64: ISR_CFG (len 16, next 0x74)
        Self::write_virtio_cap(
            &mut cfg,
            0x64,
            0x74,
            16,
            VIRTIO_PCI_CAP_ISR_CFG,
            0,
            VIRTIO_ISR_OFF,
            VIRTIO_ISR_LEN,
        );
        // Cap 4 @ 0x74: DEVICE_CFG (len 16, next 0x84 if SHM else 0x00)
        Self::write_virtio_cap(
            &mut cfg,
            0x74,
            if shm.is_some() { 0x84 } else { 0x00 },
            16,
            VIRTIO_PCI_CAP_DEVICE_CFG,
            0,
            VIRTIO_DEVICE_OFF,
            VIRTIO_DEVICE_LEN,
        );
        if let Some(ref sc) = shm {
            // Cap 5 @ 0x84: SHARED_MEMORY_CFG (virtio_pci_cap64, len 24, bar=4, id=0)
            Self::write_virtio_cap(
                &mut cfg,
                0x84,
                0x00,
                24,
                VIRTIO_PCI_CAP_SHARED_MEMORY_CFG,
                4,
                0,
                sc.size as u32,
            );
            write_le32(&mut cfg, 0x84 + 16, 0);
            write_le32(&mut cfg, 0x84 + 20, (sc.size >> 32) as u32);
        }

        virtio.set_bus_master(false);
        Self {
            dev_num,
            config: Mutex::new(cfg),
            active_bar0: AtomicU64::new(0),
            active_bar4: AtomicU64::new(0),
            bar4_probing: Mutex::new(false),
            shm,
            virtio,
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn write_virtio_cap(
        cfg: &mut [u8; 256],
        off: usize,
        next: u8,
        len: u8,
        cfg_type: u8,
        bar: u8,
        bar_offset: u32,
        bar_len: u32,
    ) {
        cfg[off] = PCI_CAP_ID_VNDR;
        cfg[off + 1] = next;
        cfg[off + 2] = len;
        cfg[off + 3] = cfg_type;
        cfg[off + 4] = bar;
        cfg[off + 5] = 0;
        cfg[off + 6] = 0;
        cfg[off + 7] = 0;
        write_le32(cfg, off + 8, bar_offset);
        write_le32(cfg, off + 12, bar_len);
    }

    fn update_active_bar0(&self, cfg: &[u8; 256]) {
        let cmd = read_le16(cfg, 0x04);
        let mem_enabled = (cmd & PCI_COMMAND_MEMORY) != 0;
        let bar0 = read_le32(cfg, 0x10);
        let bar_u64 = bar0 as u64;
        let in_window = bar0 != BAR0_SIZE_MASK
            && bar_u64 >= PCIE_MMIO32_BASE
            && bar_u64
                .checked_add(BAR0_SIZE)
                .is_some_and(|end| end <= PCIE_MMIO32_BASE + PCIE_MMIO32_SIZE);
        let active = if mem_enabled && in_window { bar_u64 } else { 0 };
        self.active_bar0.store(active, Ordering::Release);
    }

    fn update_active_bar4(&self, cfg: &[u8; 256]) {
        let Some(ref sc) = self.shm else {
            return;
        };
        let cmd = read_le16(cfg, 0x04);
        let mem_enabled = (cmd & PCI_COMMAND_MEMORY) != 0;
        if mem_enabled && *self.bar4_probing.lock().unwrap() {
            return;
        }
        let lo = read_le32(cfg, 0x20);
        let hi = read_le32(cfg, 0x24);
        let mask = !(sc.bar_size - 1);
        let probe_lo = ((mask as u32) & !0x0f) | PCI_BAR_MEM64_PREFETCH;
        let probe_hi = (mask >> 32) as u32;
        let gpa = ((hi as u64) << 32) | ((lo & !0x0f) as u64);
        let not_probe = !(lo == probe_lo && hi == probe_hi);
        let in_window = not_probe
            && gpa >= PCIE_MMIO64_BASE
            && (gpa & (sc.bar_size - 1)) == 0
            && gpa
                .checked_add(sc.bar_size)
                .is_some_and(|end| end <= PCIE_MMIO64_BASE + PCIE_MMIO64_SIZE);
        let desired = if mem_enabled && in_window { gpa } else { 0 };
        let prev = self.active_bar4.load(Ordering::Acquire);
        if desired == prev {
            return;
        }
        if desired != 0 {
            match sc.mapper.map(desired) {
                Ok(()) => self.active_bar4.store(desired, Ordering::Release),
                Err(e) => {
                    eprintln!("kido-vmm: failed to map BAR4 SHM at {desired:#x}: {e}");
                    self.active_bar4.store(0, Ordering::Release);
                }
            }
        } else {
            sc.mapper.unmap();
            self.active_bar4.store(0, Ordering::Release);
        }
    }

    fn config_read(&self, reg: usize, data: &mut [u8]) {
        if reg >= 256 {
            data.fill(0);
            return;
        }
        let mut cfg = self.config.lock().unwrap();
        let mut status = PCI_STATUS_CAP_LIST;
        if self.virtio.isr_pending() {
            status |= PCI_STATUS_INT_STATUS;
        }
        write_le16(&mut cfg, 0x06, status);
        for (i, byte) in data.iter_mut().enumerate() {
            let idx = reg + i;
            *byte = if idx < 256 { cfg[idx] } else { 0 };
        }
    }

    fn config_write(&self, reg: usize, data: &[u8]) {
        if reg >= 256 {
            return;
        }
        let mut cfg = self.config.lock().unwrap();
        let mut cmd_bytes = read_le16(&cfg, 0x04).to_le_bytes();
        let mut bar0_bytes = read_le32(&cfg, 0x10).to_le_bytes();
        let mut bar4_bytes = read_le32(&cfg, 0x20).to_le_bytes();
        let mut bar5_bytes = read_le32(&cfg, 0x24).to_le_bytes();
        let mut cmd_touched = false;
        let mut bar0_touched = false;
        let mut bar4_touched = false;
        let mut bar5_touched = false;

        for (i, &byte) in data.iter().enumerate() {
            let idx = reg + i;
            match idx {
                0x04 | 0x05 => {
                    cmd_bytes[idx - 0x04] = byte;
                    cmd_touched = true;
                }
                0x0c => {
                    // Cache line size (RW)
                    cfg[idx] = byte;
                }
                0x10..=0x13 => {
                    bar0_bytes[idx - 0x10] = byte;
                    bar0_touched = true;
                }
                0x20..=0x23 if self.shm.is_some() => {
                    bar4_bytes[idx - 0x20] = byte;
                    bar4_touched = true;
                }
                0x24..=0x27 if self.shm.is_some() => {
                    bar5_bytes[idx - 0x24] = byte;
                    bar5_touched = true;
                }
                0x3c => {
                    // Interrupt line (RW)
                    cfg[idx] = byte;
                }
                _ => {
                    // Read-only config registers ignored
                }
            }
        }

        if cmd_touched {
            let cmd = u16::from_le_bytes(cmd_bytes) & PCI_COMMAND_WRITABLE;
            write_le16(&mut cfg, 0x04, cmd);
            let intx_disabled = (cmd & PCI_COMMAND_INTX_DISABLE) != 0;
            self.virtio.set_intx_disabled(intx_disabled);
            self.virtio
                .set_bus_master((cmd & PCI_COMMAND_BUS_MASTER) != 0);
            self.update_active_bar0(&cfg);
            self.update_active_bar4(&cfg);
        }

        if bar0_touched {
            let raw = u32::from_le_bytes(bar0_bytes);
            let masked = raw & BAR0_SIZE_MASK;
            write_le32(&mut cfg, 0x10, masked);
            self.update_active_bar0(&cfg);
        }

        if bar4_touched || bar5_touched {
            if let Some(ref sc) = self.shm {
                let mask = !(sc.bar_size - 1);
                if bar4_touched {
                    let raw = u32::from_le_bytes(bar4_bytes);
                    *self.bar4_probing.lock().unwrap() = (raw & !0x0f) == 0xffff_fff0;
                    let masked = ((raw & (mask as u32)) & !0x0f) | PCI_BAR_MEM64_PREFETCH;
                    write_le32(&mut cfg, 0x20, masked);
                }
                if bar5_touched {
                    let raw = u32::from_le_bytes(bar5_bytes);
                    if raw != 0xffff_ffff {
                        let lo = read_le32(&cfg, 0x20);
                        let probe_lo = ((mask as u32) & !0x0f) | PCI_BAR_MEM64_PREFETCH;
                        if lo != probe_lo {
                            *self.bar4_probing.lock().unwrap() = false;
                        }
                    }
                    let masked = raw & ((mask >> 32) as u32);
                    write_le32(&mut cfg, 0x24, masked);
                }
                self.update_active_bar4(&cfg);
            }
        }
    }
}

pub struct PciBus {
    host_bridge: HostBridge,
    slots: Vec<PciSlot>,
}

impl PciBus {
    pub fn new() -> Self {
        Self {
            host_bridge: HostBridge::new(),
            slots: Vec::new(),
        }
    }

    pub fn add_virtio_device(&mut self, dev_num: u8, virtio: Arc<VirtioPciDevice>) -> Result<(), String> {
        self.add_virtio_device_with_shm(dev_num, virtio, None)
    }

    pub fn add_virtio_device_with_shm(
        &mut self,
        dev_num: u8,
        virtio: Arc<VirtioPciDevice>,
        shm: Option<ShmBarConfig>,
    ) -> Result<(), String> {
        if dev_num == 0 || dev_num > 31 {
            return Err(format!("invalid PCI slot {dev_num} (must be 1..=31)"));
        }
        if self.slots.iter().any(|s| s.dev_num == dev_num) {
            return Err(format!("PCI slot {dev_num} already occupied"));
        }
        self.slots.push(PciSlot::new(dev_num, virtio, shm));
        Ok(())
    }

    pub fn flush_all(&self) {
        for slot in &self.slots {
            slot.virtio.flush();
        }
    }

    pub fn ecam_read(&self, offset: u64, data: &mut [u8]) {
        let bus = (offset >> 20) & 0xff;
        let dev = ((offset >> 15) & 0x1f) as u8;
        let func = (offset >> 12) & 0x07;
        let reg = (offset & 0xfff) as usize;
        if bus != 0 || func != 0 || reg + data.len() > 4096 {
            data.fill(0xff);
            return;
        }
        if dev == 0 {
            self.host_bridge.read(reg, data);
            return;
        }
        if let Some(slot) = self.slots.iter().find(|s| s.dev_num == dev) {
            slot.config_read(reg, data);
        } else {
            data.fill(0xff);
        }
    }

    pub fn ecam_write(&self, offset: u64, data: &[u8]) {
        let bus = (offset >> 20) & 0xff;
        let dev = ((offset >> 15) & 0x1f) as u8;
        let func = (offset >> 12) & 0x07;
        let reg = (offset & 0xfff) as usize;
        if bus != 0 || func != 0 || reg + data.len() > 4096 {
            return;
        }
        if dev == 0 {
            self.host_bridge.write(reg, data);
            return;
        }
        if let Some(slot) = self.slots.iter().find(|s| s.dev_num == dev) {
            slot.config_write(reg, data);
        }
    }

    pub fn bar_read(&self, addr: u64, data: &mut [u8]) {
        for slot in &self.slots {
            let base = slot.active_bar0.load(Ordering::Acquire);
            if base != 0 && addr >= base {
                if let Some(end) = addr.checked_add(data.len() as u64) {
                    if end <= base + BAR0_SIZE {
                        slot.virtio.bar_read(addr - base, data);
                        return;
                    }
                }
            }
        }
        data.fill(0);
    }

    pub fn bar_write(&self, addr: u64, data: &[u8]) {
        for slot in &self.slots {
            let base = slot.active_bar0.load(Ordering::Acquire);
            if base != 0 && addr >= base {
                if let Some(end) = addr.checked_add(data.len() as u64) {
                    if end <= base + BAR0_SIZE {
                        slot.virtio.bar_write(addr - base, data);
                        return;
                    }
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::irq::{test_util::MockSpiBackend, SpiMux};
    use crate::memory::{GuestMemory, RAM_BASE};
    use crate::virtio::{VirtioDevice, VIRTIO_F_VERSION_1};

    struct DummyVirtio;
    impl VirtioDevice for DummyVirtio {
        fn pci_device_id(&self) -> u16 {
            PCI_DEVICE_VIRTIO_BLK
        }
        fn pci_subsystem_id(&self) -> u16 {
            0x0002
        }
        fn pci_class_code(&self) -> u32 {
            0x010000
        }
        fn features(&self) -> u64 {
            VIRTIO_F_VERSION_1
        }
        fn queue_max_sizes(&self) -> &[u16] {
            &[128]
        }
        fn read_config(&self, _offset: u64, data: &mut [u8]) {
            data.fill(0x5a);
        }
        fn process_queue(
            &self,
            _queue_index: u16,
            _queue: &mut crate::virtio::queue::VirtQueue,
            _mem: &GuestMemory,
        ) -> crate::virtio::QueueOutcome {
            crate::virtio::QueueOutcome {
                raise_irq: true,
                needs_reset: false,
            }
        }
    }

    fn ecam_off(dev: u8, reg: u64) -> u64 {
        ((dev as u64) << 15) | reg
    }

    #[test]
    fn host_bridge_bar_probing_and_intx_disable_gating() {
        let mem = Arc::new(GuestMemory::from_vec(RAM_BASE, vec![0u8; 64 * 1024]));
        let backend = MockSpiBackend::new();
        let mux = SpiMux::new(backend.clone());
        let spi = pci_intx_spi(1, 1);
        assert_eq!(spi, 4); // INTID 36

        let vpci = Arc::new(VirtioPciDevice::new(
            Box::new(DummyVirtio),
            mem,
            mux.line(spi, 1),
        ));
        let mut bus = PciBus::new();
        bus.add_virtio_device(1, vpci.clone()).unwrap();

        // Host bridge at 00:00.0
        let mut id = [0u8; 4];
        bus.ecam_read(ecam_off(0, 0x00), &mut id);
        assert_eq!(u32::from_le_bytes(id), 0x0008_1b36);

        // Unpopulated slot returns 0xFF
        bus.ecam_read(ecam_off(2, 0x00), &mut id);
        assert_eq!(u32::from_le_bytes(id), 0xffff_ffff);

        // VirtIO device at 00:01.0: exact unaligned byte read of Header Type and Interrupt Pin
        let mut b = [0u8; 1];
        bus.ecam_read(ecam_off(1, 0x0e), &mut b);
        assert_eq!(b[0], 0x00);
        bus.ecam_read(ecam_off(1, 0x3d), &mut b);
        assert_eq!(b[0], 0x01);
        bus.ecam_read(ecam_off(1, 0x34), &mut b);
        assert_eq!(b[0], 0x40);

        // BAR0 size probing via 0xFFFF_FFFF write
        bus.ecam_write(ecam_off(1, 0x10), &0xffff_ffffu32.to_le_bytes());
        let mut bar = [0u8; 4];
        bus.ecam_read(ecam_off(1, 0x10), &mut bar);
        assert_eq!(u32::from_le_bytes(bar), BAR0_SIZE_MASK);
        // BAR1..5 hardwired 0
        bus.ecam_write(ecam_off(1, 0x14), &0xffff_ffffu32.to_le_bytes());
        bus.ecam_read(ecam_off(1, 0x14), &mut bar);
        assert_eq!(u32::from_le_bytes(bar), 0);

        // Malicious BAR0 pointing into RAM (0x4000_0000) is NOT mapped into MMIO routing
        bus.ecam_write(ecam_off(1, 0x10), &0x4000_0000u32.to_le_bytes());
        bus.ecam_write(ecam_off(1, 0x04), &PCI_COMMAND_MEMORY.to_le_bytes());
        let mut dev_cfg = [0u8; 4];
        bus.bar_read(0x4000_0000 + VIRTIO_DEVICE_OFF as u64, &mut dev_cfg);
        assert_eq!(dev_cfg, [0, 0, 0, 0]);

        // Valid BAR0 in 0x1000_0000..0x3efe_ffff routes cleanly
        bus.ecam_write(ecam_off(1, 0x10), &0x1000_0000u32.to_le_bytes());
        bus.bar_read(0x1000_0000 + VIRTIO_DEVICE_OFF as u64, &mut dev_cfg);
        assert_eq!(dev_cfg, [0x5a; 4]);

        // InterruptDisable (bit 10) gating INTx assertion & re-assertion!
        vpci.raise_irq_for_test();
        assert!(backend.level(spi));

        // Mask INTx via PCI_COMMAND bit 10 -> line must drop immediately
        bus.ecam_write(
            ecam_off(1, 0x04),
            &(PCI_COMMAND_MEMORY | PCI_COMMAND_INTX_DISABLE).to_le_bytes(),
        );
        assert!(!backend.level(spi));

        // Unmask INTx while ISR is still pending -> line must re-assert immediately
        bus.ecam_write(ecam_off(1, 0x04), &PCI_COMMAND_MEMORY.to_le_bytes());
        assert!(backend.level(spi));

        // Reading ISR_CFG clears ISR and drops INTx
        let mut isr = [0u8; 1];
        bus.bar_read(0x1000_0000 + VIRTIO_ISR_OFF as u64, &mut isr);
        assert_eq!(isr[0], 1);
        assert!(!backend.level(spi));
    }

    #[test]
    fn bar4_64bit_shm_probe_map_and_unmap() {
        struct MockShmMapper {
            mapped: Mutex<Option<u64>>,
        }
        impl ShmRegionMapper for MockShmMapper {
            fn map(&self, gpa: u64) -> Result<(), String> {
                *self.mapped.lock().unwrap() = Some(gpa);
                Ok(())
            }
            fn unmap(&self) {
                *self.mapped.lock().unwrap() = None;
            }
        }

        let mem = Arc::new(GuestMemory::from_vec(RAM_BASE, vec![0u8; 64 * 1024]));
        let backend = MockSpiBackend::new();
        let mux = SpiMux::new(backend);
        let vpci = Arc::new(VirtioPciDevice::new(Box::new(DummyVirtio), mem, mux.line(6, 3)));
        let mapper = Arc::new(MockShmMapper {
            mapped: Mutex::new(None),
        });
        let shm_size = 512 * 1024 * 1024u64;
        let shm_cfg = ShmBarConfig::new(shm_size, mapper.clone()).unwrap();

        let mut bus = PciBus::new();
        bus.add_virtio_device_with_shm(3, vpci, Some(shm_cfg)).unwrap();

        // Cap 4 next pointer chains to Cap 5 @ 0x84
        let mut next = [0u8; 1];
        bus.ecam_read(ecam_off(3, 0x75), &mut next);
        assert_eq!(next[0], 0x84);
        let mut cap5 = [0u8; 24];
        bus.ecam_read(ecam_off(3, 0x84), &mut cap5);
        assert_eq!(cap5[0], PCI_CAP_ID_VNDR);
        assert_eq!(cap5[2], 24);
        assert_eq!(cap5[3], VIRTIO_PCI_CAP_SHARED_MEMORY_CFG);
        assert_eq!(cap5[4], 4);
        assert_eq!(cap5[5], 0);
        assert_eq!(u32::from_le_bytes(cap5[12..16].try_into().unwrap()), shm_size as u32);
        assert_eq!(u32::from_le_bytes(cap5[20..24].try_into().unwrap()), 0);

        // Probe BAR4/BAR5 size
        bus.ecam_write(ecam_off(3, 0x20), &0xffff_ffffu32.to_le_bytes());
        bus.ecam_write(ecam_off(3, 0x24), &0xffff_ffffu32.to_le_bytes());
        let mut lo = [0u8; 4];
        let mut hi = [0u8; 4];
        bus.ecam_read(ecam_off(3, 0x20), &mut lo);
        bus.ecam_read(ecam_off(3, 0x24), &mut hi);
        assert_eq!(u32::from_le_bytes(lo), 0xe000_000c);
        assert_eq!(u32::from_le_bytes(hi), 0xffff_ffff);
        assert_eq!(*mapper.mapped.lock().unwrap(), None);

        // Program BAR4/BAR5 to PCIE_MMIO64_BASE and enable memory space
        bus.ecam_write(ecam_off(3, 0x20), &(PCIE_MMIO64_BASE as u32).to_le_bytes());
        bus.ecam_write(ecam_off(3, 0x24), &((PCIE_MMIO64_BASE >> 32) as u32).to_le_bytes());
        assert_eq!(*mapper.mapped.lock().unwrap(), None);
        bus.ecam_write(ecam_off(3, 0x04), &PCI_COMMAND_MEMORY.to_le_bytes());
        assert_eq!(*mapper.mapped.lock().unwrap(), Some(PCIE_MMIO64_BASE));

        // Torn BAR4 size probe while PCI_COMMAND_MEMORY is enabled must NOT spuriously remap
        bus.ecam_write(ecam_off(3, 0x20), &0xffff_ffffu32.to_le_bytes());
        assert_eq!(*mapper.mapped.lock().unwrap(), Some(PCIE_MMIO64_BASE));
        bus.ecam_write(ecam_off(3, 0x24), &0xffff_ffffu32.to_le_bytes());
        assert_eq!(*mapper.mapped.lock().unwrap(), Some(PCIE_MMIO64_BASE));
        // Restore original BAR4/BAR5
        bus.ecam_write(ecam_off(3, 0x20), &(PCIE_MMIO64_BASE as u32).to_le_bytes());
        bus.ecam_write(ecam_off(3, 0x24), &((PCIE_MMIO64_BASE >> 32) as u32).to_le_bytes());
        assert_eq!(*mapper.mapped.lock().unwrap(), Some(PCIE_MMIO64_BASE));

        // Disabling memory space unmaps SHM slot
        bus.ecam_write(ecam_off(3, 0x04), &0u16.to_le_bytes());
        assert_eq!(*mapper.mapped.lock().unwrap(), None);
    }
}
