use std::fmt;
use std::fs::File;
use std::os::unix::io::AsRawFd;
use std::ptr::{self, NonNull};
#[cfg(test)]
use std::sync::atomic::AtomicU64 as TestAtomicU64;
use std::sync::atomic::{AtomicU16, AtomicU32, AtomicU64, AtomicU8, Ordering};

pub const RAM_BASE: u64 = 0x4000_0000;
pub const MIN_RAM_SIZE: usize = 256 * 1024 * 1024;
pub const MAX_RAM_SIZE: usize = (crate::boot::PCIE_ECAM_HIGH_BASE - RAM_BASE) as usize;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum MemError {
    OutOfBounds { gpa: u64, len: usize },
    Misaligned { gpa: u64, align: usize },
    AllocFailed(i32),
    Io(i32),
}

impl fmt::Display for MemError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            MemError::OutOfBounds { gpa, len } => {
                write!(f, "guest memory out of bounds: gpa={gpa:#x} len={len:#x}")
            }
            MemError::Misaligned { gpa, align } => {
                write!(f, "guest memory misaligned: gpa={gpa:#x} align={align}")
            }
            MemError::AllocFailed(errno) => {
                write!(f, "guest memory mmap failed: errno={errno}")
            }
            MemError::Io(errno) => {
                write!(f, "guest memory I/O failed: errno={errno}")
            }
        }
    }
}

impl std::error::Error for MemError {}

pub struct GuestMemory {
    base: u64,
    ptr: NonNull<u8>,
    len: usize,
}

unsafe impl Send for GuestMemory {}
unsafe impl Sync for GuestMemory {}

impl GuestMemory {
    pub fn allocate(base: u64, size: usize) -> Result<Self, MemError> {
        if (base & 0xfff) != 0 {
            return Err(MemError::Misaligned {
                gpa: base,
                align: 4096,
            });
        }
        if size == 0 || (size & 0xfff) != 0 || base.checked_add(size as u64).is_none() {
            return Err(MemError::OutOfBounds { gpa: base, len: size });
        }

        #[cfg(target_os = "linux")]
        let flags = libc::MAP_PRIVATE | libc::MAP_ANONYMOUS | libc::MAP_NORESERVE;
        #[cfg(not(target_os = "linux"))]
        let flags = libc::MAP_PRIVATE | libc::MAP_ANONYMOUS;

        let raw = unsafe {
            libc::mmap(
                ptr::null_mut(),
                size,
                libc::PROT_READ | libc::PROT_WRITE,
                flags,
                -1,
                0,
            )
        };
        if raw == libc::MAP_FAILED {
            let errno = std::io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::ENOMEM);
            return Err(MemError::AllocFailed(errno));
        }

        #[cfg(target_os = "linux")]
        unsafe {
            libc::madvise(raw, size, libc::MADV_HUGEPAGE);
        }

        let ptr = NonNull::new(raw.cast::<u8>()).ok_or(MemError::AllocFailed(libc::ENOMEM))?;
        Ok(Self {
            base,
            ptr,
            len: size,
        })
    }

    #[cfg(test)]
    pub fn from_vec(base: u64, buf: Vec<u8>) -> Self {
        let alloc_len = buf.len().max(4096).next_multiple_of(4096);
        let mem = Self::allocate(base, alloc_len).expect("allocate test GuestMemory");
        mem.write_slice(base, &buf).unwrap();
        mem
    }

    #[inline]
    pub fn base(&self) -> u64 {
        self.base
    }

    #[inline]
    pub fn len(&self) -> usize {
        self.len
    }

    #[inline]
    pub fn end(&self) -> u64 {
        self.base + self.len as u64
    }

    #[inline]
    pub fn host_ptr(&self) -> *mut u8 {
        self.ptr.as_ptr()
    }

    #[inline]
    fn offset_of(&self, gpa: u64, len: usize) -> Result<usize, MemError> {
        let off_u64 = gpa
            .checked_sub(self.base)
            .ok_or(MemError::OutOfBounds { gpa, len })?;
        let off = usize::try_from(off_u64).map_err(|_| MemError::OutOfBounds { gpa, len })?;
        let end = off
            .checked_add(len)
            .ok_or(MemError::OutOfBounds { gpa, len })?;
        if end > self.len {
            return Err(MemError::OutOfBounds { gpa, len });
        }
        Ok(off)
    }

    #[inline]
    pub fn validate_range(&self, gpa: u64, len: usize) -> Result<(), MemError> {
        self.offset_of(gpa, len).map(|_| ())
    }

    #[inline]
    fn ptr_at(&self, gpa: u64, len: usize) -> Result<*mut u8, MemError> {
        let off = self.offset_of(gpa, len)?;
        Ok(unsafe { self.ptr.as_ptr().add(off) })
    }

    pub fn write_slice(&self, gpa: u64, src: &[u8]) -> Result<(), MemError> {
        let dst = self.ptr_at(gpa, src.len())?;
        let mut i = 0usize;
        let len = src.len();
        while i < len && (((dst as usize) + i) & 7) != 0 {
            unsafe {
                (*(dst.add(i) as *const AtomicU8)).store(src[i], Ordering::Relaxed);
            }
            i += 1;
        }
        while i + 8 <= len {
            let word = u64::from_ne_bytes(src[i..i + 8].try_into().unwrap());
            unsafe {
                (*(dst.add(i) as *const AtomicU64)).store(word, Ordering::Relaxed);
            }
            i += 8;
        }
        while i < len {
            unsafe {
                (*(dst.add(i) as *const AtomicU8)).store(src[i], Ordering::Relaxed);
            }
            i += 1;
        }
        Ok(())
    }

    pub fn read_slice(&self, gpa: u64, dst: &mut [u8]) -> Result<(), MemError> {
        let src = self.ptr_at(gpa, dst.len())?;
        let mut i = 0usize;
        let len = dst.len();
        while i < len && (((src as usize) + i) & 7) != 0 {
            dst[i] = unsafe { (*(src.add(i) as *const AtomicU8)).load(Ordering::Relaxed) };
            i += 1;
        }
        while i + 8 <= len {
            let word = unsafe { (*(src.add(i) as *const AtomicU64)).load(Ordering::Relaxed) };
            dst[i..i + 8].copy_from_slice(&word.to_ne_bytes());
            i += 8;
        }
        while i < len {
            dst[i] = unsafe { (*(src.add(i) as *const AtomicU8)).load(Ordering::Relaxed) };
            i += 1;
        }
        Ok(())
    }

    pub fn zero_range(&self, gpa: u64, len: usize) -> Result<(), MemError> {
        let dst = self.ptr_at(gpa, len)?;
        let mut i = 0usize;
        while i < len && (((dst as usize) + i) & 7) != 0 {
            unsafe {
                (*(dst.add(i) as *const AtomicU8)).store(0, Ordering::Relaxed);
            }
            i += 1;
        }
        while i + 8 <= len {
            unsafe {
                (*(dst.add(i) as *const AtomicU64)).store(0, Ordering::Relaxed);
            }
            i += 8;
        }
        while i < len {
            unsafe {
                (*(dst.add(i) as *const AtomicU8)).store(0, Ordering::Relaxed);
            }
            i += 1;
        }
        Ok(())
    }

    #[cfg(test)]
    #[inline]
    pub fn read_u8(&self, gpa: u64) -> Result<u8, MemError> {
        let p = self.ptr_at(gpa, 1)? as *const AtomicU8;
        Ok(unsafe { (*p).load(Ordering::Relaxed) })
    }

    #[inline]
    pub fn write_u8(&self, gpa: u64, val: u8) -> Result<(), MemError> {
        let p = self.ptr_at(gpa, 1)? as *const AtomicU8;
        unsafe { (*p).store(val, Ordering::Relaxed) };
        Ok(())
    }

    #[inline]
    pub fn load_u16_le(&self, gpa: u64, order: Ordering) -> Result<u16, MemError> {
        if (gpa & 1) != 0 {
            return Err(MemError::Misaligned { gpa, align: 2 });
        }
        let raw = self.ptr_at(gpa, 2)?;
        if ((raw as usize) & 1) != 0 {
            return Err(MemError::Misaligned { gpa, align: 2 });
        }
        let p = raw as *const AtomicU16;
        Ok(u16::from_le(unsafe { (*p).load(order) }))
    }

    #[inline]
    pub fn store_u16_le(&self, gpa: u64, val: u16, order: Ordering) -> Result<(), MemError> {
        if (gpa & 1) != 0 {
            return Err(MemError::Misaligned { gpa, align: 2 });
        }
        let raw = self.ptr_at(gpa, 2)?;
        if ((raw as usize) & 1) != 0 {
            return Err(MemError::Misaligned { gpa, align: 2 });
        }
        let p = raw as *const AtomicU16;
        unsafe { (*p).store(val.to_le(), order) };
        Ok(())
    }

    #[cfg(test)]
    #[inline]
    pub fn load_u32_le(&self, gpa: u64, order: Ordering) -> Result<u32, MemError> {
        if (gpa & 3) != 0 {
            return Err(MemError::Misaligned { gpa, align: 4 });
        }
        let raw = self.ptr_at(gpa, 4)?;
        if ((raw as usize) & 3) != 0 {
            return Err(MemError::Misaligned { gpa, align: 4 });
        }
        let p = raw as *const AtomicU32;
        Ok(u32::from_le(unsafe { (*p).load(order) }))
    }

    #[inline]
    pub fn store_u32_le(&self, gpa: u64, val: u32, order: Ordering) -> Result<(), MemError> {
        if (gpa & 3) != 0 {
            return Err(MemError::Misaligned { gpa, align: 4 });
        }
        let raw = self.ptr_at(gpa, 4)?;
        if ((raw as usize) & 3) != 0 {
            return Err(MemError::Misaligned { gpa, align: 4 });
        }
        let p = raw as *const AtomicU32;
        unsafe { (*p).store(val.to_le(), order) };
        Ok(())
    }

    #[cfg(test)]
    #[inline]
    pub fn load_u64_le(&self, gpa: u64, order: Ordering) -> Result<u64, MemError> {
        if (gpa & 7) != 0 {
            return Err(MemError::Misaligned { gpa, align: 8 });
        }
        let raw = self.ptr_at(gpa, 8)?;
        if ((raw as usize) & 7) != 0 {
            return Err(MemError::Misaligned { gpa, align: 8 });
        }
        let p = raw as *const TestAtomicU64;
        Ok(u64::from_le(unsafe { (*p).load(order) }))
    }

    pub fn pread_into_guest(
        &self,
        file: &File,
        file_offset: u64,
        gpa: u64,
        len: usize,
    ) -> Result<usize, MemError> {
        let dst = self.ptr_at(gpa, len)?;
        if len == 0 {
            return Ok(0);
        }
        let fd = file.as_raw_fd();
        let mut done = 0usize;
        while done < len {
            let off = file_offset
                .checked_add(done as u64)
                .and_then(|v| libc::off_t::try_from(v).ok())
                .ok_or(MemError::OutOfBounds { gpa, len })?;
            let n = unsafe {
                libc::pread(
                    fd,
                    dst.add(done).cast::<libc::c_void>(),
                    len - done,
                    off,
                )
            };
            if n < 0 {
                let err = std::io::Error::last_os_error()
                    .raw_os_error()
                    .unwrap_or(libc::EIO);
                if err == libc::EINTR {
                    continue;
                }
                return Err(MemError::Io(err));
            }
            if n == 0 {
                return Err(MemError::Io(libc::EIO));
            }
            done += n as usize;
        }
        Ok(done)
    }

    pub fn pwrite_from_guest(
        &self,
        file: &File,
        file_offset: u64,
        gpa: u64,
        len: usize,
    ) -> Result<usize, MemError> {
        let src = self.ptr_at(gpa, len)?;
        if len == 0 {
            return Ok(0);
        }
        let fd = file.as_raw_fd();
        let mut done = 0usize;
        while done < len {
            let off = file_offset
                .checked_add(done as u64)
                .and_then(|v| libc::off_t::try_from(v).ok())
                .ok_or(MemError::OutOfBounds { gpa, len })?;
            let n = unsafe {
                libc::pwrite(
                    fd,
                    src.add(done).cast::<libc::c_void>(),
                    len - done,
                    off,
                )
            };
            if n < 0 {
                let err = std::io::Error::last_os_error()
                    .raw_os_error()
                    .unwrap_or(libc::EIO);
                if err == libc::EINTR {
                    continue;
                }
                return Err(MemError::Io(err));
            }
            if n == 0 {
                return Err(MemError::Io(libc::EIO));
            }
            done += n as usize;
        }
        Ok(done)
    }
}

impl Drop for GuestMemory {
    fn drop(&mut self) {
        unsafe {
            libc::munmap(self.ptr.as_ptr().cast::<libc::c_void>(), self.len);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bounds_and_overflow_rejected() {
        let mem = GuestMemory::from_vec(RAM_BASE, vec![0u8; 4096]);
        assert!(mem.validate_range(RAM_BASE, 4096).is_ok());
        assert!(mem.validate_range(RAM_BASE - 1, 1).is_err());
        assert!(mem.validate_range(RAM_BASE + 4096, 1).is_err());
        assert!(mem.validate_range(RAM_BASE + 4000, 100).is_err());
        assert!(mem.validate_range(u64::MAX, 16).is_err());
        assert!(GuestMemory::allocate(RAM_BASE + 1, 4096).is_err());
        assert!(RAM_BASE + MAX_RAM_SIZE as u64 <= crate::boot::PCIE_ECAM_HIGH_BASE);
    }

    #[test]
    fn alignment_and_rw_roundtrip() {
        let mem = GuestMemory::from_vec(RAM_BASE, vec![0u8; 4096]);
        assert!(mem.load_u16_le(RAM_BASE + 1, Ordering::Acquire).is_err());
        assert!(mem.load_u32_le(RAM_BASE + 2, Ordering::Acquire).is_err());
        assert!(mem.load_u64_le(RAM_BASE + 4, Ordering::Acquire).is_err());

        mem.store_u16_le(RAM_BASE + 8, 0x1234, Ordering::Release)
            .unwrap();
        assert_eq!(
            mem.load_u16_le(RAM_BASE + 8, Ordering::Acquire).unwrap(),
            0x1234
        );

        mem.store_u32_le(RAM_BASE + 16, 0xdead_beef, Ordering::Release)
            .unwrap();
        assert_eq!(
            mem.load_u32_le(RAM_BASE + 16, Ordering::Acquire).unwrap(),
            0xdead_beef
        );

        let payload: Vec<u8> = (0u8..37).collect();
        mem.write_slice(RAM_BASE + 3, &payload).unwrap();
        let mut out = vec![0u8; 37];
        mem.read_slice(RAM_BASE + 3, &mut out).unwrap();
        assert_eq!(out, payload);
        mem.zero_range(RAM_BASE + 3, 37).unwrap();
        mem.read_slice(RAM_BASE + 3, &mut out).unwrap();
        assert!(out.iter().all(|&b| b == 0));
    }
}
