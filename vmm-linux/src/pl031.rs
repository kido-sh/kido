use std::sync::Mutex;
use std::time::{SystemTime, UNIX_EPOCH};

const RTCDR: u64 = 0x000;
const RTCMR: u64 = 0x004;
const RTCLR: u64 = 0x008;
const RTCCR: u64 = 0x00c;
const RTCIMSC: u64 = 0x010;
const RTCRIS: u64 = 0x014;
const RTCMIS: u64 = 0x018;
const RTCICR: u64 = 0x01c;

const PRIMECELL_ID: [u8; 8] = [0x31, 0x10, 0x04, 0x00, 0x0d, 0xf0, 0x05, 0xb1];

struct Pl031State {
    offset_secs: i64,
    mr: u32,
    lr: u32,
    cr: u32,
    imsc: u32,
    ris: u32,
}

pub struct Pl031 {
    state: Mutex<Pl031State>,
}

impl Pl031 {
    pub fn new() -> Self {
        Self {
            state: Mutex::new(Pl031State {
                offset_secs: 0,
                mr: 0,
                lr: 0,
                cr: 1,
                imsc: 0,
                ris: 0,
            }),
        }
    }

    fn realtime_secs() -> i64 {
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_secs() as i64)
            .unwrap_or(0)
    }

    pub fn read(&self, offset: u64, data: &mut [u8]) {
        let val = self.read_reg(offset);
        let bytes = val.to_le_bytes();
        let n = data.len().min(4);
        data[..n].copy_from_slice(&bytes[..n]);
        for b in &mut data[n..] {
            *b = 0;
        }
    }

    pub fn write(&self, offset: u64, data: &[u8]) {
        let mut buf = [0u8; 4];
        let n = data.len().min(4);
        buf[..n].copy_from_slice(&data[..n]);
        self.write_reg(offset, u32::from_le_bytes(buf));
    }

    fn read_reg(&self, offset: u64) -> u32 {
        if (0xfe0..=0xffc).contains(&offset) && (offset & 3) == 0 {
            let idx = ((offset - 0xfe0) >> 2) as usize;
            return PRIMECELL_ID[idx] as u32;
        }
        let s = self.state.lock().unwrap();
        match offset {
            RTCDR => Self::realtime_secs().wrapping_add(s.offset_secs) as u32,
            RTCMR => s.mr,
            RTCLR => s.lr,
            RTCCR => s.cr,
            RTCIMSC => s.imsc,
            RTCRIS => s.ris,
            RTCMIS => s.ris & s.imsc,
            _ => 0,
        }
    }

    fn write_reg(&self, offset: u64, val: u32) {
        let mut s = self.state.lock().unwrap();
        match offset {
            RTCMR => s.mr = val,
            RTCLR => {
                s.lr = val;
                s.offset_secs = (val as i64).wrapping_sub(Self::realtime_secs());
            }
            RTCCR => s.cr = val & 1,
            RTCIMSC => s.imsc = val & 1,
            RTCICR => s.ris &= !(val & 1),
            _ => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn realtime_offset_and_primecell_ids() {
        let rtc = Pl031::new();
        let mut buf = [0u8; 4];
        rtc.read(RTCDR, &mut buf);
        let now = u32::from_le_bytes(buf);
        assert!(now > 1_700_000_000);

        let target: u32 = 1_800_000_123;
        rtc.write(RTCLR, &target.to_le_bytes());
        rtc.read(RTCDR, &mut buf);
        let readback = u32::from_le_bytes(buf);
        assert!(readback >= target && readback <= target + 2);

        for (i, &expected) in PRIMECELL_ID.iter().enumerate() {
            rtc.read(0xfe0 + (i as u64) * 4, &mut buf);
            assert_eq!(u32::from_le_bytes(buf), expected as u32);
        }
    }
}
