use std::sync::atomic::{fence, Ordering};

use crate::memory::GuestMemory;

pub const VRING_DESC_F_NEXT: u16 = 1;
pub const VRING_DESC_F_WRITE: u16 = 2;
pub const VRING_DESC_F_INDIRECT: u16 = 4;

pub const VRING_AVAIL_F_NO_INTERRUPT: u16 = 1;

const DESC_SIZE: u64 = 16;
const AVAIL_HDR_SIZE: u64 = 4;
const AVAIL_ELEM_SIZE: u64 = 2;
const USED_HDR_SIZE: u64 = 4;
const USED_ELEM_SIZE: u64 = 8;

pub const MAX_CHAIN_BYTES: u64 = 64 * 1024 * 1024;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Descriptor {
    pub addr: u64,
    pub len: u32,
    pub flags: u16,
    pub next: u16,
}

impl Descriptor {
    #[inline]
    pub fn is_write(&self) -> bool {
        (self.flags & VRING_DESC_F_WRITE) != 0
    }

    #[inline]
    pub fn has_next(&self) -> bool {
        (self.flags & VRING_DESC_F_NEXT) != 0
    }

    #[inline]
    pub fn is_indirect(&self) -> bool {
        (self.flags & VRING_DESC_F_INDIRECT) != 0
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DescriptorChain {
    pub head_index: u16,
    pub descriptors: Vec<Descriptor>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum QueueError {
    NotReady,
    InvalidAvailIdx,
    InvalidHead(u16),
    CycleDetected,
    ChainTooLong,
    InvalidIndirect,
    OutOfBounds(u64, u32),
    TotalBytesExceeded,
}

pub struct VirtQueue {
    max_size: u16,
    size: u16,
    enabled: bool,
    desc_table: u64,
    avail_ring: u64,
    used_ring: u64,
    next_avail: u16,
    next_used: u16,
}

impl VirtQueue {
    pub fn new(max_size: u16) -> Self {
        Self {
            max_size,
            size: max_size,
            enabled: false,
            desc_table: 0,
            avail_ring: 0,
            used_ring: 0,
            next_avail: 0,
            next_used: 0,
        }
    }

    pub fn reset(&mut self) {
        self.size = self.max_size;
        self.enabled = false;
        self.desc_table = 0;
        self.avail_ring = 0;
        self.used_ring = 0;
        self.next_avail = 0;
        self.next_used = 0;
    }

    pub fn size(&self) -> u16 {
        self.size
    }

    pub fn set_size(&mut self, size: u16) {
        if self.enabled {
            return;
        }
        if size > 0 && size <= self.max_size && size.is_power_of_two() {
            self.size = size;
        }
    }

    pub fn enabled(&self) -> bool {
        self.enabled
    }

    pub fn desc_table(&self) -> u64 {
        self.desc_table
    }

    pub fn avail_ring(&self) -> u64 {
        self.avail_ring
    }

    pub fn used_ring(&self) -> u64 {
        self.used_ring
    }

    pub fn set_desc_table(&mut self, addr: u64) {
        if self.enabled {
            return;
        }
        self.desc_table = addr;
    }

    pub fn set_desc_table_low(&mut self, lo: u32) {
        if self.enabled {
            return;
        }
        self.desc_table = (self.desc_table & 0xffff_ffff_0000_0000) | (lo as u64);
    }

    pub fn set_desc_table_high(&mut self, hi: u32) {
        if self.enabled {
            return;
        }
        self.desc_table = (self.desc_table & 0x0000_0000_ffff_ffff) | ((hi as u64) << 32);
    }

    pub fn set_avail_ring(&mut self, addr: u64) {
        if self.enabled {
            return;
        }
        self.avail_ring = addr;
    }

    pub fn set_avail_ring_low(&mut self, lo: u32) {
        if self.enabled {
            return;
        }
        self.avail_ring = (self.avail_ring & 0xffff_ffff_0000_0000) | (lo as u64);
    }

    pub fn set_avail_ring_high(&mut self, hi: u32) {
        if self.enabled {
            return;
        }
        self.avail_ring = (self.avail_ring & 0x0000_0000_ffff_ffff) | ((hi as u64) << 32);
    }

    pub fn set_used_ring(&mut self, addr: u64) {
        if self.enabled {
            return;
        }
        self.used_ring = addr;
    }

    pub fn set_used_ring_low(&mut self, lo: u32) {
        if self.enabled {
            return;
        }
        self.used_ring = (self.used_ring & 0xffff_ffff_0000_0000) | (lo as u64);
    }

    pub fn set_used_ring_high(&mut self, hi: u32) {
        if self.enabled {
            return;
        }
        self.used_ring = (self.used_ring & 0x0000_0000_ffff_ffff) | ((hi as u64) << 32);
    }

    pub fn enable(&mut self, mem: &GuestMemory) -> bool {
        if self.enabled {
            return true;
        }
        if self.size == 0 || !self.size.is_power_of_two() || self.size > self.max_size {
            return false;
        }
        if self.desc_table == 0 || self.avail_ring == 0 || self.used_ring == 0 {
            return false;
        }
        if (self.desc_table & 0xf) != 0 || (self.avail_ring & 0x1) != 0 || (self.used_ring & 0x3) != 0
        {
            return false;
        }
        let qsz = self.size as usize;
        let desc_bytes = (DESC_SIZE as usize) * qsz;
        let avail_bytes = (AVAIL_HDR_SIZE as usize) + (AVAIL_ELEM_SIZE as usize) * qsz;
        let used_bytes = (USED_HDR_SIZE as usize) + (USED_ELEM_SIZE as usize) * qsz;

        if mem.validate_range(self.desc_table, desc_bytes).is_err()
            || mem.validate_range(self.avail_ring, avail_bytes).is_err()
            || mem.validate_range(self.used_ring, used_bytes).is_err()
        {
            return false;
        }
        self.enabled = true;
        true
    }

    fn read_desc(mem: &GuestMemory, table_gpa: u64, index: u16) -> Result<Descriptor, QueueError> {
        let gpa = table_gpa
            .checked_add((index as u64) * DESC_SIZE)
            .ok_or(QueueError::OutOfBounds(table_gpa, DESC_SIZE as u32))?;
        let mut raw = [0u8; 16];
        mem.read_slice(gpa, &mut raw)
            .map_err(|_| QueueError::OutOfBounds(gpa, 16))?;
        Ok(Descriptor {
            addr: u64::from_le_bytes(raw[0..8].try_into().unwrap()),
            len: u32::from_le_bytes(raw[8..12].try_into().unwrap()),
            flags: u16::from_le_bytes(raw[12..14].try_into().unwrap()),
            next: u16::from_le_bytes(raw[14..16].try_into().unwrap()),
        })
    }

    pub fn pop_avail(&mut self, mem: &GuestMemory) -> Result<Option<DescriptorChain>, QueueError> {
        if !self.enabled {
            return Err(QueueError::NotReady);
        }
        let avail_idx_gpa = self
            .avail_ring
            .checked_add(2)
            .ok_or(QueueError::OutOfBounds(self.avail_ring, 2))?;
        let avail_idx = mem
            .load_u16_le(avail_idx_gpa, Ordering::Acquire)
            .map_err(|_| QueueError::OutOfBounds(avail_idx_gpa, 2))?;
        let pending = avail_idx.wrapping_sub(self.next_avail);
        if pending == 0 {
            return Ok(None);
        }
        if pending > self.size {
            return Err(QueueError::InvalidAvailIdx);
        }

        let ring_slot = (self.next_avail % self.size) as u64;
        let head_gpa = self
            .avail_ring
            .checked_add(AVAIL_HDR_SIZE)
            .and_then(|v| v.checked_add(ring_slot * AVAIL_ELEM_SIZE))
            .ok_or(QueueError::OutOfBounds(self.avail_ring, 2))?;
        let head = mem
            .load_u16_le(head_gpa, Ordering::Acquire)
            .map_err(|_| QueueError::OutOfBounds(head_gpa, 2))?;
        self.next_avail = self.next_avail.wrapping_add(1);

        if head >= self.size {
            return Err(QueueError::InvalidHead(head));
        }

        let mut descriptors = Vec::with_capacity(8);
        let mut total_bytes: u64 = 0;
        let mut idx = head;
        let mut steps: u16 = 0;

        loop {
            if steps >= self.size {
                return Err(QueueError::CycleDetected);
            }
            if idx >= self.size {
                return Err(QueueError::InvalidHead(idx));
            }
            let desc = Self::read_desc(mem, self.desc_table, idx)?;
            steps += 1;

            if desc.is_indirect() {
                if steps > 1
                    || desc.is_write()
                    || desc.has_next()
                    || desc.len == 0
                    || (desc.len % 16) != 0
                {
                    return Err(QueueError::InvalidIndirect);
                }
                let ind_count = desc.len / 16;
                if ind_count > self.size as u32 {
                    return Err(QueueError::ChainTooLong);
                }
                if mem.validate_range(desc.addr, desc.len as usize).is_err() {
                    return Err(QueueError::OutOfBounds(desc.addr, desc.len));
                }
                let mut ind_idx: u16 = 0;
                let mut ind_steps: u32 = 0;
                loop {
                    if ind_steps >= ind_count {
                        return Err(QueueError::CycleDetected);
                    }
                    if (ind_idx as u32) >= ind_count {
                        return Err(QueueError::InvalidIndirect);
                    }
                    let idesc = Self::read_desc(mem, desc.addr, ind_idx)?;
                    if idesc.is_indirect() {
                        return Err(QueueError::InvalidIndirect);
                    }
                    if mem.validate_range(idesc.addr, idesc.len as usize).is_err() {
                        return Err(QueueError::OutOfBounds(idesc.addr, idesc.len));
                    }
                    total_bytes = total_bytes
                        .checked_add(idesc.len as u64)
                        .ok_or(QueueError::TotalBytesExceeded)?;
                    if total_bytes > MAX_CHAIN_BYTES {
                        return Err(QueueError::TotalBytesExceeded);
                    }
                    descriptors.push(idesc);
                    ind_steps += 1;
                    if !idesc.has_next() {
                        break;
                    }
                    ind_idx = idesc.next;
                }
                break;
            }

            if mem.validate_range(desc.addr, desc.len as usize).is_err() {
                return Err(QueueError::OutOfBounds(desc.addr, desc.len));
            }
            total_bytes = total_bytes
                .checked_add(desc.len as u64)
                .ok_or(QueueError::TotalBytesExceeded)?;
            if total_bytes > MAX_CHAIN_BYTES {
                return Err(QueueError::TotalBytesExceeded);
            }
            descriptors.push(desc);

            if !desc.has_next() {
                break;
            }
            idx = desc.next;
        }

        Ok(Some(DescriptorChain {
            head_index: head,
            descriptors,
        }))
    }

    pub fn push_used(&mut self, mem: &GuestMemory, head_index: u16, written_len: u32) -> bool {
        if !self.enabled {
            return false;
        }
        let slot = (self.next_used % self.size) as u64;
        let Some(elem_gpa) = self
            .used_ring
            .checked_add(USED_HDR_SIZE)
            .and_then(|v| v.checked_add(slot * USED_ELEM_SIZE))
        else {
            return false;
        };
        let Some(len_gpa) = elem_gpa.checked_add(4) else {
            return false;
        };
        let Some(used_idx_gpa) = self.used_ring.checked_add(2) else {
            return false;
        };
        let _ = mem.store_u32_le(elem_gpa, head_index as u32, Ordering::Relaxed);
        let _ = mem.store_u32_le(len_gpa, written_len, Ordering::Relaxed);
        self.next_used = self.next_used.wrapping_add(1);
        let _ = mem.store_u16_le(used_idx_gpa, self.next_used, Ordering::Release);
        fence(Ordering::SeqCst);
        let avail_flags = mem
            .load_u16_le(self.avail_ring, Ordering::Acquire)
            .unwrap_or(0);
        (avail_flags & VRING_AVAIL_F_NO_INTERRUPT) == 0
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::memory::RAM_BASE;

    fn write_desc(
        mem: &GuestMemory,
        table: u64,
        idx: u16,
        addr: u64,
        len: u32,
        flags: u16,
        next: u16,
    ) {
        let mut raw = [0u8; 16];
        raw[0..8].copy_from_slice(&addr.to_le_bytes());
        raw[8..12].copy_from_slice(&len.to_le_bytes());
        raw[12..14].copy_from_slice(&flags.to_le_bytes());
        raw[14..16].copy_from_slice(&next.to_le_bytes());
        mem.write_slice(table + (idx as u64) * 16, &raw).unwrap();
    }

    fn setup_queue(mem: &GuestMemory, size: u16) -> (VirtQueue, u64, u64, u64) {
        let desc = RAM_BASE + 0x1000;
        let avail = RAM_BASE + 0x2000;
        let used = RAM_BASE + 0x3000;
        let mut q = VirtQueue::new(128);
        q.set_size(size);
        q.set_desc_table(desc);
        q.set_avail_ring(avail);
        q.set_used_ring(used);
        assert!(q.enable(mem));
        (q, desc, avail, used)
    }

    #[test]
    fn rejects_cycle_in_descriptor_chain() {
        let mem = GuestMemory::from_vec(RAM_BASE, vec![0u8; 64 * 1024]);
        let (mut q, desc, avail, _) = setup_queue(&mem, 8);
        write_desc(&mem, desc, 0, RAM_BASE + 0x4000, 16, VRING_DESC_F_NEXT, 1);
        write_desc(&mem, desc, 1, RAM_BASE + 0x4010, 16, VRING_DESC_F_NEXT, 0);
        mem.store_u16_le(avail + 4, 0, Ordering::Release).unwrap();
        mem.store_u16_le(avail + 2, 1, Ordering::Release).unwrap();

        assert_eq!(q.pop_avail(&mem), Err(QueueError::CycleDetected));
    }

    #[test]
    fn rejects_out_of_bounds_descriptor_gpa() {
        let mem = GuestMemory::from_vec(RAM_BASE, vec![0u8; 64 * 1024]);
        let (mut q, desc, avail, _) = setup_queue(&mem, 8);
        write_desc(&mem, desc, 0, RAM_BASE + 0x10000, 16, 0, 0);
        mem.store_u16_le(avail + 4, 0, Ordering::Release).unwrap();
        mem.store_u16_le(avail + 2, 1, Ordering::Release).unwrap();

        assert_eq!(
            q.pop_avail(&mem),
            Err(QueueError::OutOfBounds(RAM_BASE + 0x10000, 16))
        );
    }

    #[test]
    fn rejects_out_of_range_head_and_honors_no_interrupt_flag() {
        let mem = GuestMemory::from_vec(RAM_BASE, vec![0u8; 64 * 1024]);
        let (mut q, desc, avail, used) = setup_queue(&mem, 8);

        mem.store_u16_le(avail + 4, 99, Ordering::Release).unwrap();
        mem.store_u16_le(avail + 2, 1, Ordering::Release).unwrap();
        assert_eq!(q.pop_avail(&mem), Err(QueueError::InvalidHead(99)));

        write_desc(&mem, desc, 2, RAM_BASE + 0x4000, 32, VRING_DESC_F_WRITE, 0);
        mem.store_u16_le(avail, VRING_AVAIL_F_NO_INTERRUPT, Ordering::Release)
            .unwrap();
        mem.store_u16_le(avail + 6, 2, Ordering::Release).unwrap();
        mem.store_u16_le(avail + 2, 2, Ordering::Release).unwrap();

        let chain = q.pop_avail(&mem).unwrap().unwrap();
        assert_eq!(chain.head_index, 2);
        assert_eq!(chain.descriptors.len(), 1);
        let should_irq = q.push_used(&mem, 2, 32);
        assert!(!should_irq);
        assert_eq!(mem.load_u16_le(used + 2, Ordering::Acquire).unwrap(), 1);
    }

    #[test]
    fn rejects_non_power_of_two_oob_and_post_enable_mutation() {
        let mem = GuestMemory::from_vec(RAM_BASE, vec![0u8; 64 * 1024]);
        let mut q = VirtQueue::new(128);
        q.set_size(12);
        assert_eq!(q.size(), 128);
        q.set_desc_table(RAM_BASE + 0x1000);
        q.set_avail_ring(RAM_BASE + 0x2000);
        q.set_used_ring(RAM_BASE + 0x20000);
        assert!(!q.enable(&mem));

        q.set_size(8);
        q.set_used_ring(RAM_BASE + 0x3000);
        assert!(q.enable(&mem));
        q.set_size(64);
        q.set_desc_table(u64::MAX - 4);
        q.set_avail_ring(u64::MAX - 2);
        q.set_used_ring(u64::MAX - 4);
        assert_eq!(q.size(), 8);
        assert_eq!(q.desc_table(), RAM_BASE + 0x1000);
        assert_eq!(q.avail_ring(), RAM_BASE + 0x2000);
        assert_eq!(q.used_ring(), RAM_BASE + 0x3000);
    }

    #[test]
    fn rejects_avail_idx_overflow_beyond_queue_size() {
        let mem = GuestMemory::from_vec(RAM_BASE, vec![0u8; 64 * 1024]);
        let (mut q, _, avail, _) = setup_queue(&mem, 8);
        mem.store_u16_le(avail + 2, 9, Ordering::Release).unwrap();
        assert_eq!(q.pop_avail(&mem), Err(QueueError::InvalidAvailIdx));
    }

    #[test]
    fn indirect_descriptor_table_works_and_rejects_invalid() {
        let mem = GuestMemory::from_vec(RAM_BASE, vec![0u8; 64 * 1024]);
        let (mut q, desc, avail, _) = setup_queue(&mem, 8);
        let ind_table = RAM_BASE + 0x5000;

        write_desc(&mem, ind_table, 0, RAM_BASE + 0x6000, 16, VRING_DESC_F_NEXT, 1);
        write_desc(&mem, ind_table, 1, RAM_BASE + 0x6010, 1, VRING_DESC_F_WRITE, 0);
        write_desc(&mem, desc, 0, ind_table, 32, VRING_DESC_F_INDIRECT, 0);
        mem.store_u16_le(avail + 4, 0, Ordering::Release).unwrap();
        mem.store_u16_le(avail + 2, 1, Ordering::Release).unwrap();

        let chain = q.pop_avail(&mem).unwrap().unwrap();
        assert_eq!(chain.descriptors.len(), 2);

        write_desc(&mem, ind_table, 0, RAM_BASE + 0x6000, 16, VRING_DESC_F_NEXT, 0);
        mem.store_u16_le(avail + 6, 0, Ordering::Release).unwrap();
        mem.store_u16_le(avail + 2, 2, Ordering::Release).unwrap();
        assert_eq!(q.pop_avail(&mem), Err(QueueError::CycleDetected));

        write_desc(
            &mem,
            desc,
            1,
            ind_table,
            32,
            VRING_DESC_F_INDIRECT | VRING_DESC_F_WRITE,
            0,
        );
        mem.store_u16_le(avail + 8, 1, Ordering::Release).unwrap();
        mem.store_u16_le(avail + 2, 3, Ordering::Release).unwrap();
        assert_eq!(q.pop_avail(&mem), Err(QueueError::InvalidIndirect));

        write_desc(&mem, desc, 2, RAM_BASE + 0x6000, 16, VRING_DESC_F_NEXT, 3);
        write_desc(&mem, desc, 3, ind_table, 32, VRING_DESC_F_INDIRECT, 0);
        mem.store_u16_le(avail + 10, 2, Ordering::Release).unwrap();
        mem.store_u16_le(avail + 2, 4, Ordering::Release).unwrap();
        assert_eq!(q.pop_avail(&mem), Err(QueueError::InvalidIndirect));
    }
}
