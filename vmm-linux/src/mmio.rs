use std::sync::Arc;

use crate::boot::{
    EcamLayout, PCIE_MMIO32_BASE, PCIE_MMIO32_SIZE, PL011_BASE, PL011_SIZE, PL031_BASE, PL031_SIZE,
};
use crate::pci::PciBus;
use crate::pl011::Pl011;
use crate::pl031::Pl031;

pub struct MmioBus {
    pl011: Arc<Pl011>,
    pl031: Arc<Pl031>,
    pci: Arc<PciBus>,
    ecam: EcamLayout,
}

impl MmioBus {
    pub fn new(pl011: Arc<Pl011>, pl031: Arc<Pl031>, pci: Arc<PciBus>, ecam: EcamLayout) -> Self {
        Self {
            pl011,
            pl031,
            pci,
            ecam,
        }
    }

    #[inline]
    fn in_range(addr: u64, len: usize, base: u64, size: u64) -> Option<u64> {
        let end = addr.checked_add(len as u64)?;
        let region_end = base.checked_add(size)?;
        if addr >= base && end <= region_end {
            Some(addr - base)
        } else {
            None
        }
    }

    #[inline]
    fn valid_width(len: usize) -> bool {
        matches!(len, 1 | 2 | 4 | 8)
    }

    pub fn read(&self, addr: u64, data: &mut [u8]) {
        if !Self::valid_width(data.len()) {
            data.fill(0);
            return;
        }
        if let Some(off) = Self::in_range(addr, data.len(), PL011_BASE, PL011_SIZE) {
            self.pl011.read(off, data);
        } else if let Some(off) = Self::in_range(addr, data.len(), PL031_BASE, PL031_SIZE) {
            self.pl031.read(off, data);
        } else if let Some(off) = Self::in_range(addr, data.len(), self.ecam.base, self.ecam.size) {
            self.pci.ecam_read(off, data);
        } else if Self::in_range(addr, data.len(), PCIE_MMIO32_BASE, PCIE_MMIO32_SIZE).is_some() {
            self.pci.bar_read(addr, data);
        } else {
            data.fill(0);
        }
    }

    pub fn write(&self, addr: u64, data: &[u8]) {
        if !Self::valid_width(data.len()) {
            return;
        }
        if let Some(off) = Self::in_range(addr, data.len(), PL011_BASE, PL011_SIZE) {
            self.pl011.write(off, data);
        } else if let Some(off) = Self::in_range(addr, data.len(), PL031_BASE, PL031_SIZE) {
            self.pl031.write(off, data);
        } else if let Some(off) = Self::in_range(addr, data.len(), self.ecam.base, self.ecam.size) {
            self.pci.ecam_write(off, data);
        } else if Self::in_range(addr, data.len(), PCIE_MMIO32_BASE, PCIE_MMIO32_SIZE).is_some() {
            self.pci.bar_write(addr, data);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::boot::{ecam_layout_for_pa_bits, PL011_SPI};
    use crate::irq::{test_util::MockSpiBackend, SpiMux};
    use crate::memory::{GuestMemory, RAM_BASE};
    use crate::pci::{
        pci_intx_spi, BAR0_SIZE_MASK, VIRTIO_COMMON_OFF, VIRTIO_DEVICE_OFF, VIRTIO_ISR_OFF,
        VIRTIO_NOTIFY_OFF,
    };
    use crate::pl011::ConsoleSink;
    use crate::virtio::blk::{DiskSyncMode, VirtioBlk};
    use crate::virtio::pci::VirtioPciDevice;
    use crate::virtio::queue::{VRING_DESC_F_NEXT, VRING_DESC_F_WRITE};
    use crate::virtio::{
        VIRTIO_STATUS_ACKNOWLEDGE, VIRTIO_STATUS_DRIVER, VIRTIO_STATUS_DRIVER_OK,
        VIRTIO_STATUS_FEATURES_OK,
    };
    use std::io::Write;
    use std::sync::atomic::Ordering;
    use std::time::{SystemTime, UNIX_EPOCH};

    #[test]
    fn full_mmio_ecam_bar0_virtio_blk_roundtrip() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let disk_path = std::env::temp_dir().join(format!(
            "kido-vmm-mmio-e2e-{}-{nanos}.img",
            std::process::id()
        ));
        {
            let mut f = std::fs::File::create(&disk_path).unwrap();
            let mut sector0 = vec![0u8; 4096];
            sector0[0..8].copy_from_slice(b"EXT4ROOT");
            f.write_all(&sector0).unwrap();
        }

        let mem = Arc::new(GuestMemory::from_vec(RAM_BASE, vec![0u8; 256 * 1024]));
        let backend = MockSpiBackend::new();
        let mux = SpiMux::new(backend.clone());
        let pl011 = Arc::new(Pl011::new(
            ConsoleSink::Buffer(Vec::new()),
            mux.line(PL011_SPI, 0),
        ));
        let pl031 = Arc::new(Pl031::new());

        let blk = VirtioBlk::open(&disk_path, false, DiskSyncMode::None, 0).unwrap();
        let spi = pci_intx_spi(1, 1);
        let vpci = Arc::new(VirtioPciDevice::new(
            Box::new(blk),
            Arc::clone(&mem),
            mux.line(spi, 1),
        ));
        let mut pci = PciBus::new();
        pci.add_virtio_device(1, vpci).unwrap();
        let ecam = ecam_layout_for_pa_bits(40);
        let mmio = MmioBus::new(pl011.clone(), pl031, Arc::new(pci), ecam);

        // 1. PL011 TX & PL031 RTC read through MmioBus
        mmio.write(PL011_BASE, &u32::to_le_bytes(b'!' as u32));
        assert_eq!(pl011.take_buffer(), b"!");
        let mut rtc_buf = [0u8; 4];
        mmio.read(PL031_BASE, &mut rtc_buf);
        assert!(u32::from_le_bytes(rtc_buf) > 1_700_000_000);

        // 2. ECAM probe + assign BAR0 @ 0x1000_0000 + enable Mem & BusMaster
        let slot1_ecam = ecam.base + (1u64 << 15);
        let mut bar_probe = [0u8; 4];
        mmio.write(slot1_ecam + 0x10, &0xffff_ffffu32.to_le_bytes());
        mmio.read(slot1_ecam + 0x10, &mut bar_probe);
        assert_eq!(u32::from_le_bytes(bar_probe), BAR0_SIZE_MASK);
        let bar0: u64 = 0x1000_0000;
        mmio.write(slot1_ecam + 0x10, &(bar0 as u32).to_le_bytes());
        mmio.write(slot1_ecam + 0x04, &0x0006u16.to_le_bytes());

        // 3. VirtIO 1.0 handshake via BAR0 COMMON_CFG + DEVICE_CFG
        let common = bar0 + VIRTIO_COMMON_OFF as u64;
        mmio.write(common + 0x14, &[0]); // reset
        mmio.write(common + 0x14, &[VIRTIO_STATUS_ACKNOWLEDGE | VIRTIO_STATUS_DRIVER]);
        mmio.write(common + 0x08, &0u32.to_le_bytes());
        mmio.write(common + 0x0c, &((1u32 << 6) | (1u32 << 9)).to_le_bytes());
        mmio.write(common + 0x08, &1u32.to_le_bytes());
        mmio.write(common + 0x0c, &1u32.to_le_bytes()); // VIRTIO_F_VERSION_1
        mmio.write(
            common + 0x14,
            &[VIRTIO_STATUS_ACKNOWLEDGE | VIRTIO_STATUS_DRIVER | VIRTIO_STATUS_FEATURES_OK],
        );
        let mut st = [0u8; 1];
        mmio.read(common + 0x14, &mut st);
        assert_ne!(st[0] & VIRTIO_STATUS_FEATURES_OK, 0);

        let mut cap = [0u8; 8];
        mmio.read(bar0 + VIRTIO_DEVICE_OFF as u64, &mut cap);
        assert_eq!(u64::from_le_bytes(cap), 8);

        // 4. Configure queue 0 using 64-bit MMIO stores (exact IOVirtIOTransport behavior)
        let desc = RAM_BASE + 0x1000;
        let avail = RAM_BASE + 0x2000;
        let used = RAM_BASE + 0x3000;
        mmio.write(common + 0x16, &0u16.to_le_bytes());
        mmio.write(common + 0x20, &desc.to_le_bytes());
        mmio.write(common + 0x28, &avail.to_le_bytes());
        mmio.write(common + 0x30, &used.to_le_bytes());
        mmio.write(common + 0x18, &16u16.to_le_bytes());
        mmio.write(common + 0x1c, &1u16.to_le_bytes());
        mmio.write(
            common + 0x14,
            &[VIRTIO_STATUS_ACKNOWLEDGE
                | VIRTIO_STATUS_DRIVER
                | VIRTIO_STATUS_FEATURES_OK
                | VIRTIO_STATUS_DRIVER_OK],
        );

        // 5. Submit sector-0 read, kick NOTIFY_CFG, verify guest RAM + SPI assertion + ISR clear
        let hdr_gpa = RAM_BASE + 0x5000;
        let dst_gpa = RAM_BASE + 0x6000;
        let status_gpa = RAM_BASE + 0x5100;
        mem.write_slice(hdr_gpa, &[0u8; 16]).unwrap(); // type=0 (IN), sector=0
        mem.write_u8(status_gpa, 0xff).unwrap();

        let write_d = |idx: u16, addr: u64, len: u32, flags: u16, next: u16| {
            let mut raw = [0u8; 16];
            raw[0..8].copy_from_slice(&addr.to_le_bytes());
            raw[8..12].copy_from_slice(&len.to_le_bytes());
            raw[12..14].copy_from_slice(&flags.to_le_bytes());
            raw[14..16].copy_from_slice(&next.to_le_bytes());
            mem.write_slice(desc + (idx as u64) * 16, &raw).unwrap();
        };
        write_d(0, hdr_gpa, 16, VRING_DESC_F_NEXT, 1);
        write_d(1, dst_gpa, 512, VRING_DESC_F_NEXT | VRING_DESC_F_WRITE, 2);
        write_d(2, status_gpa, 1, VRING_DESC_F_WRITE, 0);
        mem.store_u16_le(avail + 4, 0, Ordering::Release).unwrap();
        mem.store_u16_le(avail + 2, 1, Ordering::Release).unwrap();

        mmio.write(bar0 + VIRTIO_NOTIFY_OFF as u64, &0u16.to_le_bytes());

        assert_eq!(mem.read_u8(status_gpa).unwrap(), 0);
        let mut magic = [0u8; 8];
        mem.read_slice(dst_gpa, &mut magic).unwrap();
        assert_eq!(&magic, b"EXT4ROOT");
        assert!(backend.level(spi));

        let mut isr = [0u8; 1];
        mmio.read(bar0 + VIRTIO_ISR_OFF as u64, &mut isr);
        assert_eq!(isr[0], 1);
        assert!(!backend.level(spi));

        // Unknown MMIO reads return 0 safely without panicking
        let mut unk = [0xaa; 8];
        mmio.read(0x0a00_0000, &mut unk);
        assert_eq!(unk, [0; 8]);

        let _ = std::fs::remove_file(&disk_path);
    }
}
