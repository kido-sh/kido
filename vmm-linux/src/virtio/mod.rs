pub mod blk;
pub mod net;
pub mod pci;
pub mod queue;

use crate::memory::GuestMemory;
use crate::virtio::queue::VirtQueue;

#[allow(dead_code)]
pub const VIRTIO_STATUS_ACKNOWLEDGE: u8 = 1;
#[allow(dead_code)]
pub const VIRTIO_STATUS_DRIVER: u8 = 2;
pub const VIRTIO_STATUS_DRIVER_OK: u8 = 4;
pub const VIRTIO_STATUS_FEATURES_OK: u8 = 8;
pub const VIRTIO_STATUS_DEVICE_NEEDS_RESET: u8 = 64;

pub const VIRTIO_F_VERSION_1: u64 = 1u64 << 32;

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct QueueOutcome {
    pub raise_irq: bool,
    pub needs_reset: bool,
}

pub trait VirtioDevice: Send + Sync {
    fn pci_device_id(&self) -> u16;
    fn pci_subsystem_id(&self) -> u16;
    fn pci_class_code(&self) -> u32;
    fn features(&self) -> u64;
    fn queue_max_sizes(&self) -> &[u16];
    fn read_config(&self, offset: u64, data: &mut [u8]);
    fn process_queue(
        &self,
        queue_index: u16,
        queue: &mut VirtQueue,
        mem: &GuestMemory,
    ) -> QueueOutcome;
    fn reset(&self) {}
    fn flush(&self) {}
}
