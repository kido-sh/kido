use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

use crate::memory::GuestMemory;
use crate::pci::PCI_DEVICE_VIRTIO_NET;
use crate::virtio::queue::{QueueError, VirtQueue};
use crate::virtio::{QueueOutcome, VirtioDevice, VIRTIO_F_VERSION_1};

const VIRTIO_NET_F_MTU: u64 = 1 << 3;
const VIRTIO_NET_F_MAC: u64 = 1 << 5;

pub const VIRTIO_NET_HDR_LEN: usize = 12;
pub const VIRTIO_NET_MAX_FRAME: usize = 2048;
const DEFAULT_MTU: u16 = 1500;
const LINK_STATUS_UP: u16 = 1;
const QUEUE_MAX_SIZES: [u16; 2] = [256, 256];
const MAX_PENDING_FRAMES: usize = 512;

type WakeCb = Arc<dyn Fn() + Send + Sync>;

pub struct NetChannel {
    tx_to_net: Mutex<VecDeque<Vec<u8>>>,
    rx_to_guest: Mutex<VecDeque<Vec<u8>>>,
    wake_net: Mutex<Option<WakeCb>>,
    wake_guest_rx: Mutex<Option<WakeCb>>,
}

impl NetChannel {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            tx_to_net: Mutex::new(VecDeque::with_capacity(64)),
            rx_to_guest: Mutex::new(VecDeque::with_capacity(64)),
            wake_net: Mutex::new(None),
            wake_guest_rx: Mutex::new(None),
        })
    }

    pub fn set_wake_net(&self, cb: WakeCb) {
        *self.wake_net.lock().unwrap() = Some(cb);
    }

    pub fn clear_wake_net(&self) {
        *self.wake_net.lock().unwrap() = None;
    }

    pub fn set_wake_guest_rx(&self, cb: WakeCb) {
        *self.wake_guest_rx.lock().unwrap() = Some(cb);
    }

    pub fn push_tx_from_guest(&self, frame: Vec<u8>) {
        let mut q = self.tx_to_net.lock().unwrap();
        if q.len() >= MAX_PENDING_FRAMES {
            q.pop_front();
        }
        q.push_back(frame);
        drop(q);
        let cb = self.wake_net.lock().unwrap().clone();
        if let Some(cb) = cb {
            cb();
        }
    }

    pub fn drain_tx_to_net(&self, out: &mut Vec<Vec<u8>>) {
        let mut q = self.tx_to_net.lock().unwrap();
        while let Some(frame) = q.pop_front() {
            out.push(frame);
        }
    }

    pub fn push_rx_to_guest(&self, frame: Vec<u8>) {
        let mut q = self.rx_to_guest.lock().unwrap();
        if q.len() >= MAX_PENDING_FRAMES {
            q.pop_front();
        }
        q.push_back(frame);
        drop(q);
        let cb = self.wake_guest_rx.lock().unwrap().clone();
        if let Some(cb) = cb {
            cb();
        }
    }

    pub fn pop_rx_for_guest(&self) -> Option<Vec<u8>> {
        self.rx_to_guest.lock().unwrap().pop_front()
    }

    pub fn has_rx_for_guest(&self) -> bool {
        !self.rx_to_guest.lock().unwrap().is_empty()
    }

    pub fn clear(&self) {
        self.tx_to_net.lock().unwrap().clear();
        self.rx_to_guest.lock().unwrap().clear();
    }
}

pub struct VirtioNet {
    channel: Arc<NetChannel>,
    config: [u8; 16],
}

impl VirtioNet {
    pub fn new(mac: [u8; 6], channel: Arc<NetChannel>) -> Self {
        let mut config = [0u8; 16];
        config[0..6].copy_from_slice(&mac);
        config[6..8].copy_from_slice(&LINK_STATUS_UP.to_le_bytes());
        config[8..10].copy_from_slice(&1u16.to_le_bytes());
        config[10..12].copy_from_slice(&DEFAULT_MTU.to_le_bytes());
        Self { channel, config }
    }

    fn process_rx(&self, queue: &mut VirtQueue, mem: &GuestMemory) -> QueueOutcome {
        let mut outcome = QueueOutcome::default();
        let budget = queue.size() as usize;
        for _ in 0..budget {
            if !self.channel.has_rx_for_guest() {
                break;
            }
            let chain = match queue.pop_avail(mem) {
                Ok(Some(c)) => c,
                Ok(None) => break,
                Err(QueueError::NotReady) => break,
                Err(_) => {
                    outcome.needs_reset = true;
                    break;
                }
            };
            let Some(frame) = self.channel.pop_rx_for_guest() else {
                break;
            };
            if frame.is_empty() || frame.len() > VIRTIO_NET_MAX_FRAME {
                if queue.push_used(mem, chain.head_index, 0) {
                    outcome.raise_irq = true;
                }
                continue;
            }

            let total_needed = VIRTIO_NET_HDR_LEN + frame.len();
            let mut written = 0usize;
            let hdr = [0u8; VIRTIO_NET_HDR_LEN];
            let mut ok = true;

            for d in &chain.descriptors {
                if !d.is_write() {
                    ok = false;
                    break;
                }
                if written >= total_needed {
                    break;
                }
                let mut desc_off = 0usize;
                let desc_len = d.len as usize;

                if written < VIRTIO_NET_HDR_LEN && desc_off < desc_len {
                    let hdr_rem = VIRTIO_NET_HDR_LEN - written;
                    let chunk = hdr_rem.min(desc_len - desc_off);
                    if mem
                        .write_slice(d.addr + desc_off as u64, &hdr[written..written + chunk])
                        .is_err()
                    {
                        ok = false;
                        break;
                    }
                    written += chunk;
                    desc_off += chunk;
                }

                if written >= VIRTIO_NET_HDR_LEN && written < total_needed && desc_off < desc_len {
                    let frame_off = written - VIRTIO_NET_HDR_LEN;
                    let frame_rem = frame.len() - frame_off;
                    let chunk = frame_rem.min(desc_len - desc_off);
                    if mem
                        .write_slice(
                            d.addr + desc_off as u64,
                            &frame[frame_off..frame_off + chunk],
                        )
                        .is_err()
                    {
                        ok = false;
                        break;
                    }
                    written += chunk;
                }
            }

            let used_len = if ok && written == total_needed {
                total_needed as u32
            } else {
                0
            };
            if queue.push_used(mem, chain.head_index, used_len) {
                outcome.raise_irq = true;
            }
        }
        outcome
    }

    fn process_tx(&self, queue: &mut VirtQueue, mem: &GuestMemory) -> QueueOutcome {
        let mut outcome = QueueOutcome::default();
        let mut scratch = Vec::new();
        let budget = queue.size() as usize;
        for _ in 0..budget {
            let chain = match queue.pop_avail(mem) {
                Ok(Some(c)) => c,
                Ok(None) => break,
                Err(QueueError::NotReady) => break,
                Err(_) => {
                    outcome.needs_reset = true;
                    break;
                }
            };

            let mut total_len = 0usize;
            let mut valid = true;
            for d in &chain.descriptors {
                if d.is_write() {
                    valid = false;
                    break;
                }
                match total_len.checked_add(d.len as usize) {
                    Some(n) if n <= VIRTIO_NET_HDR_LEN + VIRTIO_NET_MAX_FRAME => total_len = n,
                    _ => {
                        valid = false;
                        break;
                    }
                }
            }

            if valid && total_len > VIRTIO_NET_HDR_LEN {
                scratch.clear();
                scratch.resize(total_len, 0);
                let mut pos = 0usize;
                for d in &chain.descriptors {
                    let len = d.len as usize;
                    if mem.read_slice(d.addr, &mut scratch[pos..pos + len]).is_err() {
                        valid = false;
                        break;
                    }
                    pos += len;
                }
                if valid {
                    self.channel
                        .push_tx_from_guest(scratch[VIRTIO_NET_HDR_LEN..].to_vec());
                }
            }

            if queue.push_used(mem, chain.head_index, 0) {
                outcome.raise_irq = true;
            }
        }
        outcome
    }
}

impl VirtioDevice for VirtioNet {
    fn pci_device_id(&self) -> u16 {
        PCI_DEVICE_VIRTIO_NET
    }

    fn pci_subsystem_id(&self) -> u16 {
        0x0001
    }

    fn pci_class_code(&self) -> u32 {
        0x020000
    }

    fn features(&self) -> u64 {
        VIRTIO_F_VERSION_1 | VIRTIO_NET_F_MAC | VIRTIO_NET_F_MTU
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &QUEUE_MAX_SIZES
    }

    fn read_config(&self, offset: u64, data: &mut [u8]) {
        for (i, byte) in data.iter_mut().enumerate() {
            let idx = (offset as usize).saturating_add(i);
            *byte = self.config.get(idx).copied().unwrap_or(0);
        }
    }

    fn process_queue(
        &self,
        queue_index: u16,
        queue: &mut VirtQueue,
        mem: &GuestMemory,
    ) -> QueueOutcome {
        match queue_index {
            0 => self.process_rx(queue, mem),
            1 => self.process_tx(queue, mem),
            _ => QueueOutcome::default(),
        }
    }

    fn reset(&self) {
        self.channel.clear();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::memory::RAM_BASE;
    use crate::virtio::queue::{VRING_AVAIL_F_NO_INTERRUPT, VRING_DESC_F_WRITE};
    use std::sync::atomic::Ordering;

    fn make_queue(mem: &GuestMemory, base: u64) -> VirtQueue {
        let mut q = VirtQueue::new(256);
        q.set_size(16);
        q.set_desc_table(base);
        q.set_avail_ring(base + 0x1000);
        q.set_used_ring(base + 0x2000);
        assert!(q.enable(mem));
        q
    }

    fn write_desc(mem: &GuestMemory, q: &VirtQueue, idx: u16, addr: u64, len: u32, flags: u16) {
        let mut raw = [0u8; 16];
        raw[0..8].copy_from_slice(&addr.to_le_bytes());
        raw[8..12].copy_from_slice(&len.to_le_bytes());
        raw[12..14].copy_from_slice(&flags.to_le_bytes());
        mem.write_slice(q.desc_table() + (idx as u64) * 16, &raw)
            .unwrap();
    }

    #[test]
    fn virtio_net_rx_tx_and_no_interrupt_honored() {
        let mem = GuestMemory::from_vec(RAM_BASE, vec![0u8; 256 * 1024]);
        let ch = NetChannel::new();
        let mac = [0x02, 0xdc, 0x72, 0x11, 0x22, 0x33];
        let dev = VirtioNet::new(mac, Arc::clone(&ch));

        let mut cfg = [0u8; 12];
        dev.read_config(0, &mut cfg);
        assert_eq!(&cfg[0..6], &mac);
        assert_eq!(u16::from_le_bytes(cfg[6..8].try_into().unwrap()), 1);
        assert_eq!(u16::from_le_bytes(cfg[10..12].try_into().unwrap()), 1500);

        let mut rxq = make_queue(&mem, RAM_BASE + 0x1000);
        let mut txq = make_queue(&mem, RAM_BASE + 0x5000);

        mem.store_u16_le(txq.avail_ring(), VRING_AVAIL_F_NO_INTERRUPT, Ordering::Release)
            .unwrap();
        let tx_buf = RAM_BASE + 0x10000;
        let mut tx_pkt = vec![0u8; VIRTIO_NET_HDR_LEN];
        tx_pkt.extend_from_slice(b"ethernet-tx-frame");
        mem.write_slice(tx_buf, &tx_pkt).unwrap();
        write_desc(&mem, &txq, 0, tx_buf, tx_pkt.len() as u32, 0);
        mem.store_u16_le(txq.avail_ring() + 4, 0, Ordering::Release)
            .unwrap();
        mem.store_u16_le(txq.avail_ring() + 2, 1, Ordering::Release)
            .unwrap();

        let out = dev.process_queue(1, &mut txq, &mem);
        assert!(!out.raise_irq);
        assert!(!out.needs_reset);
        let mut drained = Vec::new();
        ch.drain_tx_to_net(&mut drained);
        assert_eq!(drained, vec![b"ethernet-tx-frame".to_vec()]);

        let rx_buf = RAM_BASE + 0x12000;
        write_desc(
            &mem,
            &rxq,
            0,
            rx_buf,
            (VIRTIO_NET_HDR_LEN + VIRTIO_NET_MAX_FRAME) as u32,
            VRING_DESC_F_WRITE,
        );
        mem.store_u16_le(rxq.avail_ring() + 4, 0, Ordering::Release)
            .unwrap();
        mem.store_u16_le(rxq.avail_ring() + 2, 1, Ordering::Release)
            .unwrap();

        ch.push_rx_to_guest(b"ethernet-rx-frame".to_vec());
        let out = dev.process_queue(0, &mut rxq, &mem);
        assert!(out.raise_irq);
        assert!(!out.needs_reset);

        let used_len = mem.load_u32_le(rxq.used_ring() + 8, Ordering::Acquire).unwrap();
        assert_eq!(
            used_len as usize,
            VIRTIO_NET_HDR_LEN + b"ethernet-rx-frame".len()
        );
        let mut got = vec![0u8; used_len as usize];
        mem.read_slice(rx_buf, &mut got).unwrap();
        assert_eq!(&got[..VIRTIO_NET_HDR_LEN], &[0u8; VIRTIO_NET_HDR_LEN]);
        assert_eq!(&got[VIRTIO_NET_HDR_LEN..], b"ethernet-rx-frame");
    }
}
