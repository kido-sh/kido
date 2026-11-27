use std::fs::{File, OpenOptions};
use std::io;
use std::os::unix::fs::{FileExt, OpenOptionsExt};
use std::os::unix::io::AsRawFd;
use std::path::Path;
use std::sync::Mutex;

use crate::memory::GuestMemory;
use crate::pci::PCI_DEVICE_VIRTIO_BLK;
use crate::virtio::queue::{DescriptorChain, QueueError, VirtQueue};
use crate::virtio::{QueueOutcome, VirtioDevice, VIRTIO_F_VERSION_1};

const VIRTIO_BLK_F_SIZE_MAX: u64 = 1 << 1;
const VIRTIO_BLK_F_SEG_MAX: u64 = 1 << 2;
const VIRTIO_BLK_F_RO: u64 = 1 << 5;
const VIRTIO_BLK_F_BLK_SIZE: u64 = 1 << 6;
const VIRTIO_BLK_F_FLUSH: u64 = 1 << 9;
const VIRTIO_BLK_F_DISCARD: u64 = 1 << 13;

const VIRTIO_BLK_T_IN: u32 = 0;
const VIRTIO_BLK_T_OUT: u32 = 1;
const VIRTIO_BLK_T_FLUSH: u32 = 4;
const VIRTIO_BLK_T_GET_ID: u32 = 8;
const VIRTIO_BLK_T_DISCARD: u32 = 11;

const VIRTIO_BLK_S_OK: u8 = 0;
const VIRTIO_BLK_S_IOERR: u8 = 1;
const VIRTIO_BLK_S_UNSUPP: u8 = 2;

const SECTOR_SIZE: u64 = 512;
const DEFAULT_BLK_SIZE: u32 = 512;
const DEFAULT_SIZE_MAX: u32 = 1024 * 1024;
const DEFAULT_SEG_MAX: u32 = 126;
const QUEUE_MAX_SIZE: u16 = 128;

const MAX_DISCARD_SECTORS: u32 = 65_536;
const MAX_DISCARD_SEGS: usize = 16;
const DISCARD_SECTOR_ALIGN: u32 = 8;

pub const COW_MAGIC: [u8; 8] = *b"KIDOCOW1";
pub const COW_VERSION: u32 = 1;
pub const COW_BLOCK_SIZE: u64 = 4096;
pub const COW_HEADER_SIZE: u64 = 32;
pub const MAX_COW_DISK_BYTES: u64 = 1u64 << 40;
const BITMAP_PAGE_SIZE: usize = 4096;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DiskSyncMode {
    Full,
    None,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum DiscardErr {
    Unsupported,
    Io,
}

fn open_regular_nofollow(path: &Path, writable: bool, label: &str) -> Result<(File, u64), String> {
    let file = OpenOptions::new()
        .read(true)
        .write(writable)
        .custom_flags(libc::O_NOFOLLOW)
        .open(path)
        .map_err(|e| format!("open {label} '{}': {e}", path.display()))?;
    let meta = file
        .metadata()
        .map_err(|e| format!("stat {label} '{}': {e}", path.display()))?;
    if !meta.is_file() {
        return Err(format!("{label} '{}' is not a regular file", path.display()));
    }
    let len = meta.len();
    if len > i64::MAX as u64 {
        return Err(format!("{label} '{}' size exceeds off_t limit", path.display()));
    }
    if writable {
        acquire_exclusive_flock(&file, path, label)?;
    }
    Ok((file, len))
}

fn acquire_exclusive_flock(file: &File, path: &Path, label: &str) -> Result<(), String> {
    loop {
        let rc = unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) };
        if rc == 0 {
            return Ok(());
        }
        let err = io::Error::last_os_error();
        if err.raw_os_error() == Some(libc::EINTR) {
            continue;
        }
        return Err(format!("lock {label} '{}': {err}", path.display()));
    }
}

fn sync_data_retry(file: &File) -> Result<(), ()> {
    loop {
        match file.sync_data() {
            Ok(()) => return Ok(()),
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(_) => return Err(()),
        }
    }
}

fn write_all_at_retry(file: &File, mut buf: &[u8], mut offset: u64) -> Result<(), ()> {
    while !buf.is_empty() {
        match file.write_at(buf, offset) {
            Ok(0) => return Err(()),
            Ok(n) => {
                buf = &buf[n..];
                offset += n as u64;
            }
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(_) => return Err(()),
        }
    }
    Ok(())
}

fn read_exact_at_retry(file: &File, offset: u64, dst: &mut [u8]) -> Result<(), ()> {
    let mut done = 0usize;
    while done < dst.len() {
        match file.read_at(&mut dst[done..], offset + done as u64) {
            Ok(0) => return Err(()),
            Ok(n) => done += n,
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(_) => return Err(()),
        }
    }
    Ok(())
}

fn punch_hole_range(file: &File, offset: u64, len: u64) -> Result<(), DiscardErr> {
    if len == 0 {
        return Ok(());
    }
    let off_i64 = libc::off_t::try_from(offset).map_err(|_| DiscardErr::Io)?;
    let len_i64 = libc::off_t::try_from(len).map_err(|_| DiscardErr::Io)?;
    #[cfg(target_os = "linux")]
    {
        loop {
            let rc = unsafe {
                libc::fallocate(
                    file.as_raw_fd(),
                    libc::FALLOC_FL_PUNCH_HOLE | libc::FALLOC_FL_KEEP_SIZE,
                    off_i64,
                    len_i64,
                )
            };
            if rc == 0 {
                return Ok(());
            }
            let err = io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO);
            if err == libc::EINTR {
                continue;
            }
            if err == libc::EOPNOTSUPP || err == libc::ENOSYS || err == libc::ENOTTY {
                return Err(DiscardErr::Unsupported);
            }
            return Err(DiscardErr::Io);
        }
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = (off_i64, len_i64);
        let zeros = [0u8; COW_BLOCK_SIZE as usize];
        let mut done = 0u64;
        while done < len {
            let chunk = ((len - done) as usize).min(zeros.len());
            write_all_at_retry(file, &zeros[..chunk], offset + done).map_err(|_| DiscardErr::Io)?;
            done += chunk as u64;
        }
        Ok(())
    }
}

struct CowDisk {
    base: File,
    overlay: File,
    disk_bytes: u64,
    num_blocks: u64,
    data_offset: u64,
    bitmap: Vec<u8>,
    pending_bitmap_pages: Vec<bool>,
}

impl CowDisk {
    fn open(base_path: &Path, overlay_path: &Path, read_only: bool) -> Result<Self, String> {
        let (base, base_len) = open_regular_nofollow(base_path, false, "disk base")?;
        let (overlay, overlay_len) = open_regular_nofollow(overlay_path, !read_only, "disk overlay")?;

        if overlay_len < COW_HEADER_SIZE {
            return Err(format!(
                "disk overlay '{}' too small ({overlay_len} bytes)",
                overlay_path.display()
            ));
        }

        let mut hdr = [0u8; COW_HEADER_SIZE as usize];
        read_exact_at_retry(&overlay, 0, &mut hdr)
            .map_err(|_| format!("read CoW header '{}'", overlay_path.display()))?;

        if hdr[0..8] != COW_MAGIC {
            return Err(format!("invalid CoW magic in '{}'", overlay_path.display()));
        }
        let version = u32::from_le_bytes(hdr[8..12].try_into().unwrap());
        let block_size = u32::from_le_bytes(hdr[12..16].try_into().unwrap()) as u64;
        let disk_bytes = u64::from_le_bytes(hdr[16..24].try_into().unwrap());
        let data_offset = u64::from_le_bytes(hdr[24..32].try_into().unwrap());

        if version != COW_VERSION {
            return Err(format!("unsupported CoW version {version}"));
        }
        if block_size != COW_BLOCK_SIZE {
            return Err(format!(
                "unsupported CoW block size {block_size} (expected {COW_BLOCK_SIZE})"
            ));
        }
        if disk_bytes < SECTOR_SIZE || (disk_bytes % SECTOR_SIZE) != 0 || disk_bytes > MAX_COW_DISK_BYTES
        {
            return Err(format!("invalid CoW disk_size {disk_bytes}"));
        }
        if base_len < disk_bytes {
            return Err(format!(
                "base disk '{}' ({base_len} B) smaller than CoW disk_size ({disk_bytes} B)",
                base_path.display()
            ));
        }

        let num_blocks = disk_bytes.div_ceil(COW_BLOCK_SIZE);
        let bitmap_bytes = num_blocks.div_ceil(8);
        let min_data_off = COW_HEADER_SIZE
            .checked_add(bitmap_bytes)
            .and_then(|v| v.checked_add(COW_BLOCK_SIZE - 1))
            .map(|v| v & !(COW_BLOCK_SIZE - 1))
            .ok_or_else(|| "CoW data_offset overflow".to_string())?;
        if data_offset < min_data_off || (data_offset % COW_BLOCK_SIZE) != 0 {
            return Err(format!(
                "invalid CoW data_offset {data_offset:#x} (min {min_data_off:#x})"
            ));
        }
        let min_overlay_len = data_offset
            .checked_add(disk_bytes)
            .ok_or_else(|| "CoW overlay length overflow".to_string())?;
        if min_overlay_len > i64::MAX as u64 {
            return Err("CoW overlay length exceeds off_t limit".to_string());
        }
        if overlay_len < min_overlay_len {
            return Err(format!(
                "disk overlay '{}' truncated ({overlay_len} < {min_overlay_len})",
                overlay_path.display()
            ));
        }

        let bitmap_len =
            usize::try_from(bitmap_bytes).map_err(|_| "CoW bitmap size overflow".to_string())?;
        let mut bitmap = vec![0u8; bitmap_len];
        read_exact_at_retry(&overlay, COW_HEADER_SIZE, &mut bitmap)
            .map_err(|_| format!("read CoW bitmap '{}'", overlay_path.display()))?;

        let num_pages = bitmap_len.div_ceil(BITMAP_PAGE_SIZE);
        Ok(Self {
            base,
            overlay,
            disk_bytes,
            num_blocks,
            data_offset,
            bitmap,
            pending_bitmap_pages: vec![false; num_pages],
        })
    }

    #[inline]
    fn is_dirty(&self, block: u64) -> bool {
        let byte_idx = (block / 8) as usize;
        let bit = (block % 8) as u8;
        (self.bitmap[byte_idx] & (1u8 << bit)) != 0
    }

    fn mark_dirty_range(&mut self, start_blk: u64, count: u64) {
        for blk in start_blk..(start_blk + count) {
            let byte_idx = (blk / 8) as usize;
            let mask = 1u8 << ((blk % 8) as u8);
            if (self.bitmap[byte_idx] & mask) == 0 {
                self.bitmap[byte_idx] |= mask;
                self.pending_bitmap_pages[byte_idx / BITMAP_PAGE_SIZE] = true;
            }
        }
    }

    fn has_pending_bitmap(&self) -> bool {
        self.pending_bitmap_pages.iter().any(|&d| d)
    }

    fn persist_bitmap(&mut self) -> Result<(), ()> {
        for page_idx in 0..self.pending_bitmap_pages.len() {
            if !self.pending_bitmap_pages[page_idx] {
                continue;
            }
            let start = page_idx * BITMAP_PAGE_SIZE;
            let end = (start + BITMAP_PAGE_SIZE).min(self.bitmap.len());
            write_all_at_retry(
                &self.overlay,
                &self.bitmap[start..end],
                COW_HEADER_SIZE + start as u64,
            )?;
            self.pending_bitmap_pages[page_idx] = false;
        }
        Ok(())
    }

    fn read_into_guest(
        &self,
        mut file_off: u64,
        mut gpa: u64,
        mut remain: usize,
        mem: &GuestMemory,
    ) -> Result<(), ()> {
        while remain > 0 {
            let blk = file_off / COW_BLOCK_SIZE;
            let dirty = self.is_dirty(blk);
            let mut run_end = ((blk + 1) * COW_BLOCK_SIZE).min(self.disk_bytes);
            let target_end = file_off + remain as u64;
            while run_end < target_end {
                let next_blk = run_end / COW_BLOCK_SIZE;
                if next_blk >= self.num_blocks || self.is_dirty(next_blk) != dirty {
                    break;
                }
                run_end = ((next_blk + 1) * COW_BLOCK_SIZE).min(self.disk_bytes);
            }
            let chunk = (run_end.min(target_end) - file_off) as usize;
            let res = if dirty {
                mem.pread_into_guest(&self.overlay, self.data_offset + file_off, gpa, chunk)
            } else {
                mem.pread_into_guest(&self.base, file_off, gpa, chunk)
            };
            if res.is_err() {
                return Err(());
            }
            file_off += chunk as u64;
            gpa += chunk as u64;
            remain -= chunk;
        }
        Ok(())
    }

    fn write_from_guest(
        &mut self,
        mut file_off: u64,
        mut gpa: u64,
        mut remain: usize,
        mem: &GuestMemory,
    ) -> Result<(), ()> {
        let mut blk_buf = [0u8; COW_BLOCK_SIZE as usize];
        while remain > 0 {
            let blk = file_off / COW_BLOCK_SIZE;
            let blk_off = blk * COW_BLOCK_SIZE;
            let blk_len = (self.disk_bytes - blk_off).min(COW_BLOCK_SIZE) as usize;
            let in_blk = (file_off - blk_off) as usize;

            if in_blk == 0 && remain >= COW_BLOCK_SIZE as usize {
                let full_blocks = (remain as u64) / COW_BLOCK_SIZE;
                let chunk = (full_blocks * COW_BLOCK_SIZE) as usize;
                if mem
                    .pwrite_from_guest(&self.overlay, self.data_offset + file_off, gpa, chunk)
                    .is_err()
                {
                    return Err(());
                }
                self.mark_dirty_range(blk, full_blocks);
                file_off += chunk as u64;
                gpa += chunk as u64;
                remain -= chunk;
            } else {
                let chunk = remain.min(blk_len - in_blk);
                if self.is_dirty(blk) || (in_blk == 0 && chunk == blk_len) {
                    if mem
                        .pwrite_from_guest(&self.overlay, self.data_offset + file_off, gpa, chunk)
                        .is_err()
                    {
                        return Err(());
                    }
                    self.mark_dirty_range(blk, 1);
                } else {
                    read_exact_at_retry(&self.base, blk_off, &mut blk_buf[..blk_len])?;
                    if mem
                        .read_slice(gpa, &mut blk_buf[in_blk..in_blk + chunk])
                        .is_err()
                    {
                        return Err(());
                    }
                    write_all_at_retry(
                        &self.overlay,
                        &blk_buf[..blk_len],
                        self.data_offset + blk_off,
                    )?;
                    self.mark_dirty_range(blk, 1);
                }
                file_off += chunk as u64;
                gpa += chunk as u64;
                remain -= chunk;
            }
        }
        Ok(())
    }

    fn discard_range(&mut self, mut off: u64, mut remain: u64) -> Result<(), DiscardErr> {
        let mut blk_buf = [0u8; COW_BLOCK_SIZE as usize];
        while remain > 0 {
            let blk = off / COW_BLOCK_SIZE;
            let blk_off = blk * COW_BLOCK_SIZE;
            let blk_len = (self.disk_bytes - blk_off).min(COW_BLOCK_SIZE);
            let in_blk = off - blk_off;

            if in_blk == 0 && remain >= blk_len {
                let full_blocks = if remain >= COW_BLOCK_SIZE {
                    let max_full = (self.disk_bytes - off) / COW_BLOCK_SIZE;
                    (remain / COW_BLOCK_SIZE).min(max_full)
                } else {
                    0
                };
                if full_blocks > 0 {
                    let span = full_blocks * COW_BLOCK_SIZE;
                    punch_hole_range(&self.overlay, self.data_offset + off, span)?;
                    self.mark_dirty_range(blk, full_blocks);
                    off += span;
                    remain -= span;
                    continue;
                }
                punch_hole_range(&self.overlay, self.data_offset + blk_off, blk_len)?;
                self.mark_dirty_range(blk, 1);
                off += blk_len;
                remain -= blk_len;
            } else {
                let chunk = remain.min(blk_len - in_blk) as usize;
                let blen = blk_len as usize;
                let ioff = in_blk as usize;
                if self.is_dirty(blk) {
                    read_exact_at_retry(
                        &self.overlay,
                        self.data_offset + blk_off,
                        &mut blk_buf[..blen],
                    )
                    .map_err(|_| DiscardErr::Io)?;
                } else {
                    read_exact_at_retry(&self.base, blk_off, &mut blk_buf[..blen])
                        .map_err(|_| DiscardErr::Io)?;
                }
                blk_buf[ioff..ioff + chunk].fill(0);
                write_all_at_retry(
                    &self.overlay,
                    &blk_buf[..blen],
                    self.data_offset + blk_off,
                )
                .map_err(|_| DiscardErr::Io)?;
                self.mark_dirty_range(blk, 1);
                off += chunk as u64;
                remain -= chunk as u64;
            }
        }
        Ok(())
    }

    fn flush(&mut self, sync: bool) -> Result<(), ()> {
        if self.has_pending_bitmap() {
            if sync {
                sync_data_retry(&self.overlay)?;
            }
            self.persist_bitmap()?;
            if sync {
                sync_data_retry(&self.overlay)?;
            }
        } else if sync {
            sync_data_retry(&self.overlay)?;
        }
        Ok(())
    }
}

impl Drop for CowDisk {
    fn drop(&mut self) {
        if self.has_pending_bitmap() {
            let _ = self.flush(true);
        }
    }
}

enum DiskBackend {
    Raw(File),
    Cow(Mutex<CowDisk>),
}

pub struct VirtioBlk {
    backend: DiskBackend,
    disk_bytes: u64,
    read_only: bool,
    sync_mode: DiskSyncMode,
    disk_id: [u8; 20],
    config: [u8; 64],
}

impl VirtioBlk {
    pub fn open(
        path: &Path,
        read_only: bool,
        sync_mode: DiskSyncMode,
        index: usize,
    ) -> Result<Self, String> {
        let (file, disk_bytes) = open_regular_nofollow(path, !read_only, "disk")?;
        let capacity_sectors = disk_bytes / SECTOR_SIZE;
        if capacity_sectors == 0 {
            return Err(format!(
                "disk '{}' is too small ({disk_bytes} bytes, need >= 512)",
                path.display()
            ));
        }

        Ok(Self::from_backend(
            DiskBackend::Raw(file),
            capacity_sectors * SECTOR_SIZE,
            read_only,
            sync_mode,
            index,
        ))
    }

    pub fn open_cow(
        base_path: &Path,
        overlay_path: &Path,
        read_only: bool,
        sync_mode: DiskSyncMode,
        index: usize,
    ) -> Result<Self, String> {
        let cow = CowDisk::open(base_path, overlay_path, read_only)?;
        let disk_bytes = cow.disk_bytes;
        Ok(Self::from_backend(
            DiskBackend::Cow(Mutex::new(cow)),
            disk_bytes,
            read_only,
            sync_mode,
            index,
        ))
    }

    fn from_backend(
        backend: DiskBackend,
        disk_bytes: u64,
        read_only: bool,
        sync_mode: DiskSyncMode,
        index: usize,
    ) -> Self {
        let capacity_sectors = disk_bytes / SECTOR_SIZE;
        let mut disk_id = [0u8; 20];
        let label = format!("kido-blk{index}");
        let copy_len = label.len().min(disk_id.len());
        disk_id[..copy_len].copy_from_slice(&label.as_bytes()[..copy_len]);

        let mut config = [0u8; 64];
        config[0..8].copy_from_slice(&capacity_sectors.to_le_bytes());
        config[8..12].copy_from_slice(&DEFAULT_SIZE_MAX.to_le_bytes());
        config[12..16].copy_from_slice(&DEFAULT_SEG_MAX.to_le_bytes());
        config[20..24].copy_from_slice(&DEFAULT_BLK_SIZE.to_le_bytes());
        config[36..40].copy_from_slice(&MAX_DISCARD_SECTORS.to_le_bytes());
        config[40..44].copy_from_slice(&(MAX_DISCARD_SEGS as u32).to_le_bytes());
        config[44..48].copy_from_slice(&DISCARD_SECTOR_ALIGN.to_le_bytes());

        Self {
            backend,
            disk_bytes,
            read_only,
            sync_mode,
            disk_id,
            config,
        }
    }

    #[cfg(test)]
    pub fn capacity_sectors(&self) -> u64 {
        self.disk_bytes / SECTOR_SIZE
    }

    fn execute_chain(&self, chain: &DescriptorChain, mem: &GuestMemory) -> (u8, u32) {
        if chain.descriptors.len() < 2 {
            return (VIRTIO_BLK_S_IOERR, 0);
        }
        let hdr_desc = chain.descriptors[0];
        let status_desc = *chain.descriptors.last().unwrap();
        if hdr_desc.is_write() || hdr_desc.len < 16 {
            return (VIRTIO_BLK_S_IOERR, 0);
        }
        if !status_desc.is_write() || status_desc.len < 1 {
            return (VIRTIO_BLK_S_IOERR, 0);
        }

        let mut hdr = [0u8; 16];
        if mem.read_slice(hdr_desc.addr, &mut hdr).is_err() {
            return (VIRTIO_BLK_S_IOERR, 0);
        }
        let req_type = u32::from_le_bytes(hdr[0..4].try_into().unwrap());
        let sector = u64::from_le_bytes(hdr[8..16].try_into().unwrap());
        let data_descs = &chain.descriptors[1..chain.descriptors.len() - 1];

        let (status, data_written) = match req_type {
            VIRTIO_BLK_T_IN => self.handle_read(sector, data_descs, mem),
            VIRTIO_BLK_T_OUT => self.handle_write(sector, data_descs, mem),
            VIRTIO_BLK_T_FLUSH => self.handle_flush(),
            VIRTIO_BLK_T_GET_ID => self.handle_get_id(data_descs, mem),
            VIRTIO_BLK_T_DISCARD => self.handle_discard(data_descs, mem),
            _ => (VIRTIO_BLK_S_UNSUPP, 0),
        };

        let _ = mem.write_u8(status_desc.addr, status);
        (status, data_written.saturating_add(1))
    }

    fn handle_read(
        &self,
        sector: u64,
        descs: &[crate::virtio::queue::Descriptor],
        mem: &GuestMemory,
    ) -> (u8, u32) {
        let Some(mut file_off) = sector.checked_mul(SECTOR_SIZE) else {
            return (VIRTIO_BLK_S_IOERR, 0);
        };
        let mut written: u32 = 0;
        for d in descs {
            if !d.is_write() {
                return (VIRTIO_BLK_S_IOERR, written);
            }
            let len = d.len as u64;
            let Some(end_off) = file_off.checked_add(len) else {
                return (VIRTIO_BLK_S_IOERR, written);
            };
            if end_off > self.disk_bytes {
                return (VIRTIO_BLK_S_IOERR, written);
            }
            let ok = match &self.backend {
                DiskBackend::Raw(file) => mem
                    .pread_into_guest(file, file_off, d.addr, d.len as usize)
                    .is_ok(),
                DiskBackend::Cow(cow) => cow
                    .lock()
                    .unwrap()
                    .read_into_guest(file_off, d.addr, d.len as usize, mem)
                    .is_ok(),
            };
            if !ok {
                return (VIRTIO_BLK_S_IOERR, written);
            }
            file_off = end_off;
            written = written.saturating_add(d.len);
        }
        (VIRTIO_BLK_S_OK, written)
    }

    fn handle_write(
        &self,
        sector: u64,
        descs: &[crate::virtio::queue::Descriptor],
        mem: &GuestMemory,
    ) -> (u8, u32) {
        if self.read_only {
            return (VIRTIO_BLK_S_IOERR, 0);
        }
        let Some(mut file_off) = sector.checked_mul(SECTOR_SIZE) else {
            return (VIRTIO_BLK_S_IOERR, 0);
        };
        for d in descs {
            if d.is_write() {
                return (VIRTIO_BLK_S_IOERR, 0);
            }
            let len = d.len as u64;
            let Some(end_off) = file_off.checked_add(len) else {
                return (VIRTIO_BLK_S_IOERR, 0);
            };
            if end_off > self.disk_bytes {
                return (VIRTIO_BLK_S_IOERR, 0);
            }
            let ok = match &self.backend {
                DiskBackend::Raw(file) => mem
                    .pwrite_from_guest(file, file_off, d.addr, d.len as usize)
                    .is_ok(),
                DiskBackend::Cow(cow) => cow
                    .lock()
                    .unwrap()
                    .write_from_guest(file_off, d.addr, d.len as usize, mem)
                    .is_ok(),
            };
            if !ok {
                return (VIRTIO_BLK_S_IOERR, 0);
            }
            file_off = end_off;
        }
        (VIRTIO_BLK_S_OK, 0)
    }

    fn handle_flush(&self) -> (u8, u32) {
        if self.read_only {
            return (VIRTIO_BLK_S_OK, 0);
        }
        let sync = self.sync_mode != DiskSyncMode::None;
        let ok = match &self.backend {
            DiskBackend::Raw(file) => !sync || sync_data_retry(file).is_ok(),
            DiskBackend::Cow(cow) => cow.lock().unwrap().flush(sync).is_ok(),
        };
        if ok {
            (VIRTIO_BLK_S_OK, 0)
        } else {
            (VIRTIO_BLK_S_IOERR, 0)
        }
    }

    fn handle_get_id(
        &self,
        descs: &[crate::virtio::queue::Descriptor],
        mem: &GuestMemory,
    ) -> (u8, u32) {
        let mut offset = 0usize;
        for d in descs {
            if !d.is_write() {
                return (VIRTIO_BLK_S_IOERR, offset as u32);
            }
            let remain = self.disk_id.len().saturating_sub(offset);
            let n = (d.len as usize).min(remain);
            if n > 0 {
                if mem
                    .write_slice(d.addr, &self.disk_id[offset..offset + n])
                    .is_err()
                {
                    return (VIRTIO_BLK_S_IOERR, offset as u32);
                }
                offset += n;
            }
        }
        (VIRTIO_BLK_S_OK, offset as u32)
    }

    fn handle_discard(
        &self,
        descs: &[crate::virtio::queue::Descriptor],
        mem: &GuestMemory,
    ) -> (u8, u32) {
        if self.read_only {
            return (VIRTIO_BLK_S_IOERR, 0);
        }
        let mut total_segs = 0usize;
        for d in descs {
            if d.is_write() || d.len == 0 || (d.len % 16) != 0 {
                return (VIRTIO_BLK_S_IOERR, 0);
            }
            let mut pos = 0u64;
            while pos < d.len as u64 {
                total_segs += 1;
                if total_segs > MAX_DISCARD_SEGS {
                    return (VIRTIO_BLK_S_IOERR, 0);
                }
                let mut seg = [0u8; 16];
                if mem.read_slice(d.addr + pos, &mut seg).is_err() {
                    return (VIRTIO_BLK_S_IOERR, 0);
                }
                let sector = u64::from_le_bytes(seg[0..8].try_into().unwrap());
                let num_sectors = u32::from_le_bytes(seg[8..12].try_into().unwrap());
                if num_sectors > MAX_DISCARD_SECTORS {
                    return (VIRTIO_BLK_S_IOERR, 0);
                }
                let Some(off) = sector.checked_mul(SECTOR_SIZE) else {
                    return (VIRTIO_BLK_S_IOERR, 0);
                };
                let Some(bytes) = (num_sectors as u64).checked_mul(SECTOR_SIZE) else {
                    return (VIRTIO_BLK_S_IOERR, 0);
                };
                let Some(end) = off.checked_add(bytes) else {
                    return (VIRTIO_BLK_S_IOERR, 0);
                };
                if end > self.disk_bytes {
                    return (VIRTIO_BLK_S_IOERR, 0);
                }
                if bytes > 0 {
                    let res = match &self.backend {
                        DiskBackend::Raw(file) => punch_hole_range(file, off, bytes),
                        DiskBackend::Cow(cow) => cow.lock().unwrap().discard_range(off, bytes),
                    };
                    match res {
                        Ok(()) => {}
                        Err(DiscardErr::Unsupported) => return (VIRTIO_BLK_S_UNSUPP, 0),
                        Err(DiscardErr::Io) => return (VIRTIO_BLK_S_IOERR, 0),
                    }
                }
                pos += 16;
            }
        }
        (VIRTIO_BLK_S_OK, 0)
    }
}

impl VirtioDevice for VirtioBlk {
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
        let mut feats = VIRTIO_F_VERSION_1
            | VIRTIO_BLK_F_SIZE_MAX
            | VIRTIO_BLK_F_SEG_MAX
            | VIRTIO_BLK_F_BLK_SIZE
            | VIRTIO_BLK_F_FLUSH
            | VIRTIO_BLK_F_DISCARD;
        if self.read_only {
            feats |= VIRTIO_BLK_F_RO;
        }
        feats
    }

    fn queue_max_sizes(&self) -> &[u16] {
        &[QUEUE_MAX_SIZE]
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
        let mut outcome = QueueOutcome::default();
        if queue_index != 0 {
            return outcome;
        }
        let budget = queue.size() as usize;
        for _ in 0..budget {
            match queue.pop_avail(mem) {
                Ok(Some(chain)) => {
                    let (_, written_len) = self.execute_chain(&chain, mem);
                    if queue.push_used(mem, chain.head_index, written_len) {
                        outcome.raise_irq = true;
                    }
                }
                Ok(None) | Err(QueueError::NotReady) => break,
                Err(_) => {
                    outcome.needs_reset = true;
                    break;
                }
            }
        }
        outcome
    }

    fn flush(&self) {
        if self.read_only {
            return;
        }
        let sync = self.sync_mode != DiskSyncMode::None;
        match &self.backend {
            DiskBackend::Raw(file) => {
                if sync {
                    let _ = sync_data_retry(file);
                }
            }
            DiskBackend::Cow(cow) => {
                let _ = cow.lock().unwrap().flush(sync);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::memory::RAM_BASE;
    use crate::virtio::queue::{VRING_DESC_F_NEXT, VRING_DESC_F_WRITE};
    use std::io::Write;
    use std::sync::atomic::Ordering;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn unique_tmp(name: &str) -> std::path::PathBuf {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        std::env::temp_dir().join(format!("kido-vmm-{name}-{}-{nanos}.img", std::process::id()))
    }

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

    fn create_test_cow(path: &Path, disk_bytes: u64) {
        let num_blocks = disk_bytes.div_ceil(COW_BLOCK_SIZE);
        let bitmap_bytes = num_blocks.div_ceil(8);
        let data_offset = (COW_HEADER_SIZE + bitmap_bytes + COW_BLOCK_SIZE - 1) & !(COW_BLOCK_SIZE - 1);
        let mut f = File::create(path).unwrap();
        let mut hdr = [0u8; COW_HEADER_SIZE as usize];
        hdr[0..8].copy_from_slice(&COW_MAGIC);
        hdr[8..12].copy_from_slice(&COW_VERSION.to_le_bytes());
        hdr[12..16].copy_from_slice(&(COW_BLOCK_SIZE as u32).to_le_bytes());
        hdr[16..24].copy_from_slice(&disk_bytes.to_le_bytes());
        hdr[24..32].copy_from_slice(&data_offset.to_le_bytes());
        f.write_all(&hdr).unwrap();
        f.set_len(data_offset + disk_bytes).unwrap();
    }

    fn make_chain(
        mem: &GuestMemory,
        req_type: u32,
        sector: u64,
        data_gpa: u64,
        data_len: u32,
        data_write: bool,
        status_gpa: u64,
    ) -> DescriptorChain {
        let hdr_gpa = RAM_BASE + 0x1000;
        let mut hdr = [0u8; 16];
        hdr[0..4].copy_from_slice(&req_type.to_le_bytes());
        hdr[8..16].copy_from_slice(&sector.to_le_bytes());
        mem.write_slice(hdr_gpa, &hdr).unwrap();
        mem.write_u8(status_gpa, 0xff).unwrap();
        DescriptorChain {
            head_index: 0,
            descriptors: vec![
                crate::virtio::queue::Descriptor {
                    addr: hdr_gpa,
                    len: 16,
                    flags: VRING_DESC_F_NEXT,
                    next: 1,
                },
                crate::virtio::queue::Descriptor {
                    addr: data_gpa,
                    len: data_len,
                    flags: if data_write {
                        VRING_DESC_F_NEXT | VRING_DESC_F_WRITE
                    } else {
                        VRING_DESC_F_NEXT
                    },
                    next: 2,
                },
                crate::virtio::queue::Descriptor {
                    addr: status_gpa,
                    len: 1,
                    flags: VRING_DESC_F_WRITE,
                    next: 0,
                },
            ],
        }
    }

    #[test]
    fn rw_flush_get_id_and_readonly_enforcement() {
        let path = unique_tmp("blk");
        {
            let mut f = File::create(&path).unwrap();
            let mut data = vec![0u8; 8192];
            data[512..524].copy_from_slice(b"HELLO_SECTOR");
            f.write_all(&data).unwrap();
        }

        let mem = GuestMemory::from_vec(RAM_BASE, vec![0u8; 128 * 1024]);
        let blk = VirtioBlk::open(&path, false, DiskSyncMode::None, 0).unwrap();
        assert_eq!(blk.capacity_sectors(), 16);

        assert!(VirtioBlk::open(&path, false, DiskSyncMode::None, 0).is_err());

        let desc_gpa = RAM_BASE + 0x1000;
        let avail_gpa = RAM_BASE + 0x2000;
        let used_gpa = RAM_BASE + 0x3000;
        let mut q = VirtQueue::new(128);
        q.set_size(16);
        q.set_desc_table(desc_gpa);
        q.set_avail_ring(avail_gpa);
        q.set_used_ring(used_gpa);
        assert!(q.enable(&mem));

        let hdr_gpa = RAM_BASE + 0x4000;
        let data_gpa = RAM_BASE + 0x5000;
        let status_gpa = RAM_BASE + 0x6000;

        let mut hdr = [0u8; 16];
        hdr[0..4].copy_from_slice(&VIRTIO_BLK_T_IN.to_le_bytes());
        hdr[8..16].copy_from_slice(&1u64.to_le_bytes());
        mem.write_slice(hdr_gpa, &hdr).unwrap();
        mem.write_u8(status_gpa, 0xff).unwrap();

        write_desc(&mem, desc_gpa, 0, hdr_gpa, 16, VRING_DESC_F_NEXT, 1);
        write_desc(
            &mem,
            desc_gpa,
            1,
            data_gpa,
            512,
            VRING_DESC_F_NEXT | VRING_DESC_F_WRITE,
            2,
        );
        write_desc(&mem, desc_gpa, 2, status_gpa, 1, VRING_DESC_F_WRITE, 0);
        mem.store_u16_le(avail_gpa + 4, 0, Ordering::Release).unwrap();
        mem.store_u16_le(avail_gpa + 2, 1, Ordering::Release).unwrap();

        let out = blk.process_queue(0, &mut q, &mem);
        assert!(out.raise_irq && !out.needs_reset);
        assert_eq!(mem.read_u8(status_gpa).unwrap(), VIRTIO_BLK_S_OK);
        let mut readback = [0u8; 12];
        mem.read_slice(data_gpa, &mut readback).unwrap();
        assert_eq!(&readback, b"HELLO_SECTOR");

        hdr[0..4].copy_from_slice(&VIRTIO_BLK_T_OUT.to_le_bytes());
        hdr[8..16].copy_from_slice(&2u64.to_le_bytes());
        mem.write_slice(hdr_gpa, &hdr).unwrap();
        mem.write_slice(data_gpa, b"WRITTEN_BLK!").unwrap();
        write_desc(&mem, desc_gpa, 1, data_gpa, 512, VRING_DESC_F_NEXT, 2);
        mem.store_u16_le(avail_gpa + 6, 0, Ordering::Release).unwrap();
        mem.store_u16_le(avail_gpa + 2, 2, Ordering::Release).unwrap();
        assert!(blk.process_queue(0, &mut q, &mem).raise_irq);
        assert_eq!(mem.read_u8(status_gpa).unwrap(), VIRTIO_BLK_S_OK);

        let mut verify = vec![0u8; 12];
        let f = File::open(&path).unwrap();
        f.read_exact_at(&mut verify, 1024).unwrap();
        assert_eq!(&verify, b"WRITTEN_BLK!");

        hdr[0..4].copy_from_slice(&VIRTIO_BLK_T_GET_ID.to_le_bytes());
        mem.write_slice(hdr_gpa, &hdr).unwrap();
        write_desc(
            &mem,
            desc_gpa,
            1,
            data_gpa,
            20,
            VRING_DESC_F_NEXT | VRING_DESC_F_WRITE,
            2,
        );
        mem.store_u16_le(avail_gpa + 8, 0, Ordering::Release).unwrap();
        mem.store_u16_le(avail_gpa + 2, 3, Ordering::Release).unwrap();
        assert!(blk.process_queue(0, &mut q, &mem).raise_irq);
        let mut id = [0u8; 9];
        mem.read_slice(data_gpa, &mut id).unwrap();
        assert_eq!(&id, b"kido-blk0");

        drop(blk);

        let ro = VirtioBlk::open(&path, true, DiskSyncMode::None, 1).unwrap();
        hdr[0..4].copy_from_slice(&VIRTIO_BLK_T_OUT.to_le_bytes());
        mem.write_slice(hdr_gpa, &hdr).unwrap();
        write_desc(&mem, desc_gpa, 1, data_gpa, 512, VRING_DESC_F_NEXT, 2);
        mem.store_u16_le(avail_gpa + 10, 0, Ordering::Release).unwrap();
        mem.store_u16_le(avail_gpa + 2, 4, Ordering::Release).unwrap();
        assert!(ro.process_queue(0, &mut q, &mem).raise_irq);
        assert_eq!(mem.read_u8(status_gpa).unwrap(), VIRTIO_BLK_S_IOERR);

        let _ = std::fs::remove_file(&path);
    }

    #[test]
    fn cow_overlay_reads_base_copy_up_partial_writes_persists_and_discards() {
        let base_path = unique_tmp("cow-base");
        let cow_path = unique_tmp("cow-ovl");
        let link_path = unique_tmp("cow-link");
        let disk_size = 16 * 1024usize;
        let mut base_data = vec![0u8; disk_size];
        for (i, b) in base_data.iter_mut().enumerate() {
            *b = ((i / 512) as u8).wrapping_add(1);
        }
        std::fs::write(&base_path, &base_data).unwrap();
        create_test_cow(&cow_path, disk_size as u64);
        std::os::unix::fs::symlink(&cow_path, &link_path).unwrap();

        assert!(VirtioBlk::open_cow(&base_path, &link_path, false, DiskSyncMode::None, 0).is_err());

        let mem = GuestMemory::from_vec(RAM_BASE, vec![0u8; 128 * 1024]);
        let data_gpa = RAM_BASE + 0x4000;
        let status_gpa = RAM_BASE + 0x10000;

        {
            let blk = VirtioBlk::open_cow(&base_path, &cow_path, false, DiskSyncMode::None, 0).unwrap();
            assert_eq!(blk.capacity_sectors(), 32);

            let chain = make_chain(&mem, VIRTIO_BLK_T_IN, 0, data_gpa, disk_size as u32, true, status_gpa);
            let (status, written) = blk.execute_chain(&chain, &mem);
            assert_eq!(status, VIRTIO_BLK_S_OK);
            assert_eq!(written as usize, disk_size + 1);
            let mut got = vec![0u8; disk_size];
            mem.read_slice(data_gpa, &mut got).unwrap();
            assert_eq!(got, base_data);

            let patch = vec![0xAAu8; 512];
            mem.write_slice(data_gpa, &patch).unwrap();
            let chain = make_chain(&mem, VIRTIO_BLK_T_OUT, 1, data_gpa, 512, false, status_gpa);
            assert_eq!(blk.execute_chain(&chain, &mem).0, VIRTIO_BLK_S_OK);

            let block2 = vec![0x55u8; 4096];
            mem.write_slice(data_gpa, &block2).unwrap();
            let chain = make_chain(&mem, VIRTIO_BLK_T_OUT, 16, data_gpa, 4096, false, status_gpa);
            assert_eq!(blk.execute_chain(&chain, &mem).0, VIRTIO_BLK_S_OK);

            let mut seg = [0u8; 16];
            seg[0..8].copy_from_slice(&24u64.to_le_bytes());
            seg[8..12].copy_from_slice(&8u32.to_le_bytes());
            mem.write_slice(data_gpa, &seg).unwrap();
            let chain = make_chain(&mem, VIRTIO_BLK_T_DISCARD, 0, data_gpa, 16, false, status_gpa);
            assert_eq!(blk.execute_chain(&chain, &mem).0, VIRTIO_BLK_S_OK);

            let mut big_seg = [0u8; 16];
            big_seg[8..12].copy_from_slice(&(MAX_DISCARD_SECTORS + 1).to_le_bytes());
            mem.write_slice(data_gpa, &big_seg).unwrap();
            let chain = make_chain(&mem, VIRTIO_BLK_T_DISCARD, 0, data_gpa, 16, false, status_gpa);
            assert_eq!(blk.execute_chain(&chain, &mem).0, VIRTIO_BLK_S_IOERR);

            blk.flush();
        }

        assert_eq!(std::fs::read(&base_path).unwrap(), base_data);

        {
            let blk = VirtioBlk::open_cow(&base_path, &cow_path, false, DiskSyncMode::None, 0).unwrap();
            let chain = make_chain(&mem, VIRTIO_BLK_T_IN, 0, data_gpa, disk_size as u32, true, status_gpa);
            assert_eq!(blk.execute_chain(&chain, &mem).0, VIRTIO_BLK_S_OK);
            let mut got = vec![0u8; disk_size];
            mem.read_slice(data_gpa, &mut got).unwrap();

            let mut expected = base_data.clone();
            expected[512..1024].fill(0xAA);
            expected[8192..12288].fill(0x55);
            expected[12288..16384].fill(0x00);
            assert_eq!(got, expected);

            let chain = make_chain(&mem, VIRTIO_BLK_T_IN, 32, data_gpa, 512, true, status_gpa);
            assert_eq!(blk.execute_chain(&chain, &mem).0, VIRTIO_BLK_S_IOERR);
        }

        let ro = VirtioBlk::open_cow(&base_path, &cow_path, true, DiskSyncMode::None, 0).unwrap();
        let chain = make_chain(&mem, VIRTIO_BLK_T_OUT, 0, data_gpa, 512, false, status_gpa);
        assert_eq!(ro.execute_chain(&chain, &mem).0, VIRTIO_BLK_S_IOERR);

        let _ = std::fs::remove_file(&link_path);
        let _ = std::fs::remove_file(&base_path);
        let _ = std::fs::remove_file(&cow_path);
    }

    #[test]
    fn cow_pending_bitmap_preserved_on_persist_error() {
        let base_path = unique_tmp("cow-err-base");
        let cow_path = unique_tmp("cow-err-ovl");
        let disk_size = 8192u64;
        std::fs::write(&base_path, vec![0x11u8; disk_size as usize]).unwrap();
        create_test_cow(&cow_path, disk_size);

        let mut cow = CowDisk::open(&base_path, &cow_path, true).unwrap();
        cow.mark_dirty_range(0, 1);
        assert!(cow.has_pending_bitmap());
        assert!(cow.persist_bitmap().is_err());
        assert!(cow.has_pending_bitmap());

        let _ = std::fs::remove_file(&base_path);
        let _ = std::fs::remove_file(&cow_path);
    }
}
