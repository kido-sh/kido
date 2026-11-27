use std::mem::{ManuallyDrop, size_of};
use std::ptr;
use std::sync::atomic::{Ordering, compiler_fence};

use aes_gcm::aead::AeadInPlace;
use aes_gcm::{Aes256Gcm, KeyInit, Nonce, Tag};

pub struct Cipher(ManuallyDrop<Aes256Gcm>);

impl Cipher {
    pub fn new(key: &[u8; 32]) -> Self {
        Self(ManuallyDrop::new(
            Aes256Gcm::new_from_slice(key).expect("32-byte AES-256 key"),
        ))
    }

    #[inline(never)]
    pub fn encrypt_in_place_detached(
        &self,
        nonce: &[u8; 12],
        aad: &[u8],
        buf: &mut [u8],
    ) -> Result<[u8; 16], ()> {
        let tag = self
            .0
            .encrypt_in_place_detached(Nonce::from_slice(nonce), aad, buf)
            .map_err(|_| ())?;
        Ok(tag.into())
    }

    #[inline(never)]
    pub fn decrypt_in_place_detached(
        &self,
        nonce: &[u8; 12],
        aad: &[u8],
        buf: &mut [u8],
        tag: &[u8; 16],
    ) -> Result<(), ()> {
        self.0
            .decrypt_in_place_detached(Nonce::from_slice(nonce), aad, buf, Tag::from_slice(tag))
            .map_err(|_| ())
    }
}

impl Drop for Cipher {
    fn drop(&mut self) {
        unsafe {
            ManuallyDrop::drop(&mut self.0);
            let p = (&mut self.0 as *mut ManuallyDrop<Aes256Gcm>).cast::<u8>();
            for i in 0..size_of::<Aes256Gcm>() {
                ptr::write_volatile(p.add(i), 0);
            }
            compiler_fence(Ordering::SeqCst);
        }
    }
}
