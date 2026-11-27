use std::sync::{Arc, Mutex};

use crate::irq::IrqLine;
use crate::memory::GuestMemory;
use crate::pci::{
    VIRTIO_COMMON_LEN, VIRTIO_COMMON_OFF, VIRTIO_DEVICE_LEN, VIRTIO_DEVICE_OFF, VIRTIO_ISR_LEN,
    VIRTIO_ISR_OFF, VIRTIO_NOTIFY_LEN, VIRTIO_NOTIFY_MULT, VIRTIO_NOTIFY_OFF,
};
use crate::virtio::queue::VirtQueue;
use crate::virtio::{
    VirtioDevice, VIRTIO_F_VERSION_1, VIRTIO_STATUS_DEVICE_NEEDS_RESET, VIRTIO_STATUS_DRIVER_OK,
    VIRTIO_STATUS_FEATURES_OK,
};

const COMMON_DEVICE_FEATURE_SELECT: u64 = 0x00;
const COMMON_DEVICE_FEATURE: u64 = 0x04;
const COMMON_DRIVER_FEATURE_SELECT: u64 = 0x08;
const COMMON_DRIVER_FEATURE: u64 = 0x0c;
const COMMON_MSIX_CONFIG: u64 = 0x10;
const COMMON_NUM_QUEUES: u64 = 0x12;
const COMMON_DEVICE_STATUS: u64 = 0x14;
const COMMON_CONFIG_GENERATION: u64 = 0x15;
const COMMON_QUEUE_SELECT: u64 = 0x16;
const COMMON_QUEUE_SIZE: u64 = 0x18;
const COMMON_QUEUE_MSIX_VECTOR: u64 = 0x1a;
const COMMON_QUEUE_ENABLE: u64 = 0x1c;
const COMMON_QUEUE_NOTIFY_OFF: u64 = 0x1e;
const COMMON_QUEUE_DESC: u64 = 0x20;
const COMMON_QUEUE_DESC_HI: u64 = 0x24;
const COMMON_QUEUE_DRIVER: u64 = 0x28;
const COMMON_QUEUE_DRIVER_HI: u64 = 0x2c;
const COMMON_QUEUE_DEVICE: u64 = 0x30;
const COMMON_QUEUE_DEVICE_HI: u64 = 0x34;

const MSIX_NO_VECTOR: u16 = 0xffff;

struct TransportState {
    device_feature_select: u32,
    driver_feature_select: u32,
    driver_features: u64,
    device_status: u8,
    queue_select: u16,
    isr_status: u8,
    intx_disabled: bool,
    bus_master: bool,
    queues: Vec<VirtQueue>,
}

impl TransportState {
    fn sync_irq(&self, irq: &IrqLine) {
        irq.set_level(self.isr_status != 0 && !self.intx_disabled);
    }

    fn reset(&mut self, irq: &IrqLine) {
        self.device_feature_select = 0;
        self.driver_feature_select = 0;
        self.driver_features = 0;
        self.device_status = 0;
        self.queue_select = 0;
        self.isr_status = 0;
        for q in &mut self.queues {
            q.reset();
        }
        self.sync_irq(irq);
    }
}

pub struct VirtioPciDevice {
    device: Box<dyn VirtioDevice>,
    mem: Arc<GuestMemory>,
    irq: IrqLine,
    state: Mutex<TransportState>,
}

impl VirtioPciDevice {
    pub fn new(device: Box<dyn VirtioDevice>, mem: Arc<GuestMemory>, irq: IrqLine) -> Self {
        let queues = device
            .queue_max_sizes()
            .iter()
            .map(|&max| VirtQueue::new(max))
            .collect();
        Self {
            device,
            mem,
            irq,
            state: Mutex::new(TransportState {
                device_feature_select: 0,
                driver_feature_select: 0,
                driver_features: 0,
                device_status: 0,
                queue_select: 0,
                isr_status: 0,
                intx_disabled: false,
                bus_master: true,
                queues,
            }),
        }
    }

    pub fn pci_device_id(&self) -> u16 {
        self.device.pci_device_id()
    }

    pub fn pci_subsystem_id(&self) -> u16 {
        self.device.pci_subsystem_id()
    }

    pub fn pci_class_code(&self) -> u32 {
        self.device.pci_class_code()
    }

    pub fn flush(&self) {
        self.device.flush();
    }

    pub fn inject_queue(&self, queue_index: u16) {
        self.handle_notify(queue_index);
    }

    pub fn isr_pending(&self) -> bool {
        self.state.lock().unwrap().isr_status != 0
    }

    pub fn set_intx_disabled(&self, disabled: bool) {
        let mut s = self.state.lock().unwrap();
        if s.intx_disabled != disabled {
            s.intx_disabled = disabled;
            s.sync_irq(&self.irq);
        }
    }

    pub fn set_bus_master(&self, enabled: bool) {
        self.state.lock().unwrap().bus_master = enabled;
    }

    #[cfg(test)]
    pub fn raise_irq_for_test(&self) {
        let mut s = self.state.lock().unwrap();
        s.isr_status |= 1;
        s.sync_irq(&self.irq);
    }

    pub fn bar_read(&self, offset: u64, data: &mut [u8]) {
        let Some(end) = offset.checked_add(data.len() as u64) else {
            data.fill(0);
            return;
        };
        if offset >= VIRTIO_COMMON_OFF as u64 && end <= (VIRTIO_COMMON_OFF + VIRTIO_COMMON_LEN) as u64
        {
            self.read_common_cfg(offset - VIRTIO_COMMON_OFF as u64, data);
        } else if offset >= VIRTIO_ISR_OFF as u64 && end <= (VIRTIO_ISR_OFF + VIRTIO_ISR_LEN) as u64
        {
            data.fill(0);
            if offset == VIRTIO_ISR_OFF as u64 && !data.is_empty() {
                let mut s = self.state.lock().unwrap();
                let old = s.isr_status;
                s.isr_status = 0;
                s.sync_irq(&self.irq);
                data[0] = old;
            }
        } else if offset >= VIRTIO_DEVICE_OFF as u64
            && end <= (VIRTIO_DEVICE_OFF + VIRTIO_DEVICE_LEN) as u64
        {
            self.device.read_config(offset - VIRTIO_DEVICE_OFF as u64, data);
        } else {
            data.fill(0);
        }
    }

    pub fn bar_write(&self, offset: u64, data: &[u8]) {
        let Some(end) = offset.checked_add(data.len() as u64) else {
            return;
        };
        if offset >= VIRTIO_COMMON_OFF as u64 && end <= (VIRTIO_COMMON_OFF + VIRTIO_COMMON_LEN) as u64
        {
            self.write_common_cfg(offset - VIRTIO_COMMON_OFF as u64, data);
        } else if offset >= VIRTIO_NOTIFY_OFF as u64
            && end <= (VIRTIO_NOTIFY_OFF + VIRTIO_NOTIFY_LEN) as u64
        {
            let rel = offset - VIRTIO_NOTIFY_OFF as u64;
            let q_idx = (rel / VIRTIO_NOTIFY_MULT as u64) as u16;
            self.handle_notify(q_idx);
        }
    }

    fn read_common_cfg(&self, off: u64, data: &mut [u8]) {
        let s = self.state.lock().unwrap();
        let q = s.queues.get(s.queue_select as usize);
        match (off, data.len()) {
            (COMMON_DEVICE_FEATURE_SELECT, 4) => {
                data.copy_from_slice(&s.device_feature_select.to_le_bytes());
            }
            (COMMON_DEVICE_FEATURE, 4) => {
                let feats = self.device.features();
                let word = match s.device_feature_select {
                    0 => feats as u32,
                    1 => (feats >> 32) as u32,
                    _ => 0,
                };
                data.copy_from_slice(&word.to_le_bytes());
            }
            (COMMON_DRIVER_FEATURE_SELECT, 4) => {
                data.copy_from_slice(&s.driver_feature_select.to_le_bytes());
            }
            (COMMON_DRIVER_FEATURE, 4) => {
                let word = match s.driver_feature_select {
                    0 => s.driver_features as u32,
                    1 => (s.driver_features >> 32) as u32,
                    _ => 0,
                };
                data.copy_from_slice(&word.to_le_bytes());
            }
            (COMMON_MSIX_CONFIG, 2) => {
                data.copy_from_slice(&MSIX_NO_VECTOR.to_le_bytes());
            }
            (COMMON_NUM_QUEUES, 2) => {
                data.copy_from_slice(&(s.queues.len() as u16).to_le_bytes());
            }
            (COMMON_DEVICE_STATUS, 1) => {
                data[0] = s.device_status;
            }
            (COMMON_CONFIG_GENERATION, 1) => {
                data[0] = 0;
            }
            (COMMON_QUEUE_SELECT, 2) => {
                data.copy_from_slice(&s.queue_select.to_le_bytes());
            }
            (COMMON_QUEUE_SIZE, 2) => {
                let sz = q.map_or(0u16, VirtQueue::size);
                data.copy_from_slice(&sz.to_le_bytes());
            }
            (COMMON_QUEUE_MSIX_VECTOR, 2) => {
                data.copy_from_slice(&MSIX_NO_VECTOR.to_le_bytes());
            }
            (COMMON_QUEUE_ENABLE, 2) => {
                let en = u16::from(q.is_some_and(VirtQueue::enabled));
                data.copy_from_slice(&en.to_le_bytes());
            }
            (COMMON_QUEUE_NOTIFY_OFF, 2) => {
                let noff = if q.is_some() { s.queue_select } else { 0 };
                data.copy_from_slice(&noff.to_le_bytes());
            }
            (COMMON_QUEUE_DESC, 8) => {
                let addr = q.map_or(0u64, VirtQueue::desc_table);
                data.copy_from_slice(&addr.to_le_bytes());
            }
            (COMMON_QUEUE_DESC, 4) => {
                let lo = q.map_or(0u32, |vq| vq.desc_table() as u32);
                data.copy_from_slice(&lo.to_le_bytes());
            }
            (COMMON_QUEUE_DESC_HI, 4) => {
                let hi = q.map_or(0u32, |vq| (vq.desc_table() >> 32) as u32);
                data.copy_from_slice(&hi.to_le_bytes());
            }
            (COMMON_QUEUE_DRIVER, 8) => {
                let addr = q.map_or(0u64, VirtQueue::avail_ring);
                data.copy_from_slice(&addr.to_le_bytes());
            }
            (COMMON_QUEUE_DRIVER, 4) => {
                let lo = q.map_or(0u32, |vq| vq.avail_ring() as u32);
                data.copy_from_slice(&lo.to_le_bytes());
            }
            (COMMON_QUEUE_DRIVER_HI, 4) => {
                let hi = q.map_or(0u32, |vq| (vq.avail_ring() >> 32) as u32);
                data.copy_from_slice(&hi.to_le_bytes());
            }
            (COMMON_QUEUE_DEVICE, 8) => {
                let addr = q.map_or(0u64, VirtQueue::used_ring);
                data.copy_from_slice(&addr.to_le_bytes());
            }
            (COMMON_QUEUE_DEVICE, 4) => {
                let lo = q.map_or(0u32, |vq| vq.used_ring() as u32);
                data.copy_from_slice(&lo.to_le_bytes());
            }
            (COMMON_QUEUE_DEVICE_HI, 4) => {
                let hi = q.map_or(0u32, |vq| (vq.used_ring() >> 32) as u32);
                data.copy_from_slice(&hi.to_le_bytes());
            }
            _ => data.fill(0),
        }
    }

    fn write_common_cfg(&self, off: u64, data: &[u8]) {
        let mut s = self.state.lock().unwrap();
        let q_sel = s.queue_select as usize;
        match (off, data.len()) {
            (COMMON_DEVICE_FEATURE_SELECT, 4) => {
                s.device_feature_select = u32::from_le_bytes(data.try_into().unwrap());
            }
            (COMMON_DRIVER_FEATURE_SELECT, 4) => {
                s.driver_feature_select = u32::from_le_bytes(data.try_into().unwrap());
            }
            (COMMON_DRIVER_FEATURE, 4) => {
                if (s.device_status & VIRTIO_STATUS_FEATURES_OK) != 0 {
                    return;
                }
                let val = u32::from_le_bytes(data.try_into().unwrap()) as u64;
                match s.driver_feature_select {
                    0 => {
                        s.driver_features = (s.driver_features & 0xffff_ffff_0000_0000) | val;
                    }
                    1 => {
                        s.driver_features =
                            (s.driver_features & 0x0000_0000_ffff_ffff) | (val << 32);
                    }
                    _ => {}
                }
            }
            (COMMON_DEVICE_STATUS, 1) => {
                let mut status = data[0];
                if status == 0 {
                    s.reset(&self.irq);
                    self.device.reset();
                    return;
                }
                let needs_reset = s.device_status & VIRTIO_STATUS_DEVICE_NEEDS_RESET;
                let was_features_ok = (s.device_status & VIRTIO_STATUS_FEATURES_OK) != 0;
                if (status & VIRTIO_STATUS_FEATURES_OK) != 0 && !was_features_ok {
                    let offered = self.device.features();
                    let valid = (s.driver_features & !offered) == 0
                        && (s.driver_features & VIRTIO_F_VERSION_1) != 0;
                    if !valid {
                        status &= !VIRTIO_STATUS_FEATURES_OK;
                    }
                }
                if (status & VIRTIO_STATUS_DRIVER_OK) != 0
                    && (status & VIRTIO_STATUS_FEATURES_OK) == 0
                {
                    status &= !VIRTIO_STATUS_DRIVER_OK;
                }
                s.device_status = status | needs_reset;
            }
            (COMMON_QUEUE_SELECT, 2) => {
                s.queue_select = u16::from_le_bytes(data.try_into().unwrap());
            }
            (COMMON_QUEUE_SIZE, 2) => {
                let sz = u16::from_le_bytes(data.try_into().unwrap());
                if let Some(q) = s.queues.get_mut(q_sel) {
                    q.set_size(sz);
                }
            }
            (COMMON_QUEUE_ENABLE, 2) => {
                let val = u16::from_le_bytes(data.try_into().unwrap());
                if val == 1 {
                    if let Some(q) = s.queues.get_mut(q_sel) {
                        q.enable(&self.mem);
                    }
                }
            }
            (COMMON_QUEUE_DESC, 8) => {
                let addr = u64::from_le_bytes(data.try_into().unwrap());
                if let Some(q) = s.queues.get_mut(q_sel) {
                    q.set_desc_table(addr);
                }
            }
            (COMMON_QUEUE_DESC, 4) => {
                let lo = u32::from_le_bytes(data.try_into().unwrap());
                if let Some(q) = s.queues.get_mut(q_sel) {
                    q.set_desc_table_low(lo);
                }
            }
            (COMMON_QUEUE_DESC_HI, 4) => {
                let hi = u32::from_le_bytes(data.try_into().unwrap());
                if let Some(q) = s.queues.get_mut(q_sel) {
                    q.set_desc_table_high(hi);
                }
            }
            (COMMON_QUEUE_DRIVER, 8) => {
                let addr = u64::from_le_bytes(data.try_into().unwrap());
                if let Some(q) = s.queues.get_mut(q_sel) {
                    q.set_avail_ring(addr);
                }
            }
            (COMMON_QUEUE_DRIVER, 4) => {
                let lo = u32::from_le_bytes(data.try_into().unwrap());
                if let Some(q) = s.queues.get_mut(q_sel) {
                    q.set_avail_ring_low(lo);
                }
            }
            (COMMON_QUEUE_DRIVER_HI, 4) => {
                let hi = u32::from_le_bytes(data.try_into().unwrap());
                if let Some(q) = s.queues.get_mut(q_sel) {
                    q.set_avail_ring_high(hi);
                }
            }
            (COMMON_QUEUE_DEVICE, 8) => {
                let addr = u64::from_le_bytes(data.try_into().unwrap());
                if let Some(q) = s.queues.get_mut(q_sel) {
                    q.set_used_ring(addr);
                }
            }
            (COMMON_QUEUE_DEVICE, 4) => {
                let lo = u32::from_le_bytes(data.try_into().unwrap());
                if let Some(q) = s.queues.get_mut(q_sel) {
                    q.set_used_ring_low(lo);
                }
            }
            (COMMON_QUEUE_DEVICE_HI, 4) => {
                let hi = u32::from_le_bytes(data.try_into().unwrap());
                if let Some(q) = s.queues.get_mut(q_sel) {
                    q.set_used_ring_high(hi);
                }
            }
            _ => {}
        }
    }

    fn handle_notify(&self, queue_index: u16) {
        let mut s = self.state.lock().unwrap();
        let ready_mask = VIRTIO_STATUS_FEATURES_OK | VIRTIO_STATUS_DRIVER_OK;
        if (s.device_status & ready_mask) != ready_mask
            || (s.device_status & VIRTIO_STATUS_DEVICE_NEEDS_RESET) != 0
            || !s.bus_master
        {
            return;
        }
        let Some(q) = s.queues.get_mut(queue_index as usize) else {
            return;
        };
        if !q.enabled() {
            return;
        }
        let outcome = self.device.process_queue(queue_index, q, &self.mem);
        if outcome.needs_reset {
            s.device_status |= VIRTIO_STATUS_DEVICE_NEEDS_RESET;
        }
        if outcome.raise_irq {
            s.isr_status |= 1;
            s.sync_irq(&self.irq);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::irq::{test_util::MockSpiBackend, SpiMux};
    use crate::memory::RAM_BASE;
    use crate::pci::PCI_DEVICE_VIRTIO_BLK;
    use crate::virtio::{QueueOutcome, VIRTIO_STATUS_ACKNOWLEDGE, VIRTIO_STATUS_DRIVER};

    struct StubDev {
        fail_queue: bool,
    }
    impl VirtioDevice for StubDev {
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
            VIRTIO_F_VERSION_1 | (1 << 6)
        }
        fn queue_max_sizes(&self) -> &[u16] {
            &[128]
        }
        fn read_config(&self, _offset: u64, data: &mut [u8]) {
            data.fill(0);
        }
        fn process_queue(
            &self,
            _q: u16,
            _queue: &mut VirtQueue,
            _mem: &GuestMemory,
        ) -> QueueOutcome {
            QueueOutcome {
                raise_irq: !self.fail_queue,
                needs_reset: self.fail_queue,
            }
        }
    }

    #[test]
    fn common_cfg_accepts_both_64bit_and_32bit_queue_address_writes() {
        let mem = Arc::new(GuestMemory::from_vec(RAM_BASE, vec![0u8; 128 * 1024]));
        let backend = MockSpiBackend::new();
        let mux = SpiMux::new(backend);
        let vpci = VirtioPciDevice::new(Box::new(StubDev { fail_queue: false }), mem, mux.line(4, 0));

        let desc_gpa = RAM_BASE + 0x1000;
        let avail_gpa = RAM_BASE + 0x2000;
        let used_gpa = RAM_BASE + 0x3000;
        vpci.bar_write(COMMON_QUEUE_DESC, &desc_gpa.to_le_bytes());
        vpci.bar_write(COMMON_QUEUE_DRIVER, &avail_gpa.to_le_bytes());
        vpci.bar_write(COMMON_QUEUE_DEVICE, &used_gpa.to_le_bytes());

        let mut readback = [0u8; 8];
        vpci.bar_read(COMMON_QUEUE_DESC, &mut readback);
        assert_eq!(u64::from_le_bytes(readback), desc_gpa);

        let alt = RAM_BASE + 0x4000;
        vpci.bar_write(COMMON_QUEUE_DESC, &(alt as u32).to_le_bytes());
        vpci.bar_write(COMMON_QUEUE_DESC_HI, &((alt >> 32) as u32).to_le_bytes());
        vpci.bar_read(COMMON_QUEUE_DESC, &mut readback);
        assert_eq!(u64::from_le_bytes(readback), alt);

        vpci.bar_write(COMMON_QUEUE_ENABLE, &1u16.to_le_bytes());
        let mut en = [0u8; 2];
        vpci.bar_read(COMMON_QUEUE_ENABLE, &mut en);
        assert_eq!(u16::from_le_bytes(en), 1);

        vpci.bar_write(COMMON_QUEUE_DESC, &0xffff_ffff_ffff_fff0u64.to_le_bytes());
        vpci.bar_read(COMMON_QUEUE_DESC, &mut readback);
        assert_eq!(u64::from_le_bytes(readback), alt);

        vpci.bar_write(COMMON_DEVICE_STATUS, &[0]);
        vpci.bar_read(COMMON_QUEUE_ENABLE, &mut en);
        assert_eq!(u16::from_le_bytes(en), 0);
    }

    #[test]
    fn enforces_features_ok_before_driver_ok_and_isr_suboffset() {
        let mem = Arc::new(GuestMemory::from_vec(RAM_BASE, vec![0u8; 128 * 1024]));
        let backend = MockSpiBackend::new();
        let mux = SpiMux::new(backend.clone());
        let vpci = VirtioPciDevice::new(Box::new(StubDev { fail_queue: true }), mem, mux.line(4, 0));

        vpci.bar_write(
            COMMON_DEVICE_STATUS,
            &[VIRTIO_STATUS_ACKNOWLEDGE | VIRTIO_STATUS_DRIVER | VIRTIO_STATUS_DRIVER_OK],
        );
        let mut st = [0u8; 1];
        vpci.bar_read(COMMON_DEVICE_STATUS, &mut st);
        assert_eq!(st[0] & VIRTIO_STATUS_DRIVER_OK, 0);

        vpci.bar_write(COMMON_DRIVER_FEATURE_SELECT, &1u32.to_le_bytes());
        vpci.bar_write(COMMON_DRIVER_FEATURE, &1u32.to_le_bytes());
        vpci.bar_write(
            COMMON_DEVICE_STATUS,
            &[VIRTIO_STATUS_ACKNOWLEDGE | VIRTIO_STATUS_DRIVER | VIRTIO_STATUS_FEATURES_OK],
        );
        vpci.bar_write(COMMON_QUEUE_DESC, &(RAM_BASE + 0x1000).to_le_bytes());
        vpci.bar_write(COMMON_QUEUE_DRIVER, &(RAM_BASE + 0x2000).to_le_bytes());
        vpci.bar_write(COMMON_QUEUE_DEVICE, &(RAM_BASE + 0x3000).to_le_bytes());
        vpci.bar_write(COMMON_QUEUE_ENABLE, &1u16.to_le_bytes());
        vpci.bar_write(
            COMMON_DEVICE_STATUS,
            &[VIRTIO_STATUS_ACKNOWLEDGE
                | VIRTIO_STATUS_DRIVER
                | VIRTIO_STATUS_FEATURES_OK
                | VIRTIO_STATUS_DRIVER_OK],
        );
        vpci.bar_read(COMMON_DEVICE_STATUS, &mut st);
        assert_ne!(st[0] & VIRTIO_STATUS_DRIVER_OK, 0);

        vpci.bar_write(VIRTIO_NOTIFY_OFF as u64, &0u16.to_le_bytes());
        vpci.bar_read(COMMON_DEVICE_STATUS, &mut st);
        assert_ne!(st[0] & VIRTIO_STATUS_DEVICE_NEEDS_RESET, 0);

        vpci.raise_irq_for_test();
        assert!(backend.level(4));
        let mut sub = [0u8; 1];
        vpci.bar_read(VIRTIO_ISR_OFF as u64 + 1, &mut sub);
        assert_eq!(sub[0], 0);
        assert!(backend.level(4));
        vpci.bar_read(VIRTIO_ISR_OFF as u64, &mut sub);
        assert_eq!(sub[0], 1);
        assert!(!backend.level(4));
    }
}
