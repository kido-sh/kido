use std::collections::VecDeque;
use std::fs::{self, File, OpenOptions};
use std::io::{self, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use crate::irq::IrqLine;

const UARTDR: u64 = 0x000;
const UARTRSR: u64 = 0x004;
const UARTFR: u64 = 0x018;
const UARTILPR: u64 = 0x020;
const UARTIBRD: u64 = 0x024;
const UARTFBRD: u64 = 0x028;
const UARTLCR_H: u64 = 0x02c;
const UARTCR: u64 = 0x030;
const UARTIFLS: u64 = 0x034;
const UARTIMSC: u64 = 0x038;
const UARTRIS: u64 = 0x03c;
const UARTMIS: u64 = 0x040;
const UARTICR: u64 = 0x044;
const UARTDMACR: u64 = 0x048;

const FR_RXFE: u32 = 1 << 4;
const FR_RXFF: u32 = 1 << 6;
const FR_TXFE: u32 = 1 << 7;

const INT_RX: u32 = 1 << 4;
const INT_TX: u32 = 1 << 5;

const RX_FIFO_CAP: usize = 256;
const TX_BUF_CAP: usize = 512;
pub const DEFAULT_CONSOLE_MAX_BYTES: u64 = 2 * 1024 * 1024;

const PRIMECELL_ID: [u8; 8] = [0x11, 0x10, 0x14, 0x00, 0x0d, 0xf0, 0x05, 0xb1];

fn open_console_file_retry(path: &Path) -> io::Result<File> {
    loop {
        match OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .mode(0o600)
            .open(path)
        {
            Ok(f) => return Ok(f),
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        }
    }
}

fn write_all_retry<W: Write>(w: &mut W, mut buf: &[u8]) -> io::Result<()> {
    while !buf.is_empty() {
        match w.write(buf) {
            Ok(0) => return Err(io::Error::new(io::ErrorKind::WriteZero, "short write")),
            Ok(n) => buf = &buf[n..],
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        }
    }
    Ok(())
}

pub enum ConsoleSink {
    Stdout {
        buf: Vec<u8>,
    },
    RotatingFile {
        path: PathBuf,
        backup_path: PathBuf,
        max_bytes: u64,
        written: u64,
        file: Option<File>,
        buf: Vec<u8>,
    },
    #[cfg(test)]
    Buffer(Vec<u8>),
}

impl ConsoleSink {
    pub fn stdout() -> Self {
        Self::Stdout {
            buf: Vec::with_capacity(TX_BUF_CAP),
        }
    }

    pub fn rotating_file(path: PathBuf, max_bytes: u64) -> io::Result<Self> {
        let cap = max_bytes.max(4096);
        let backup_path = PathBuf::from(format!("{}.1", path.display()));
        let file = open_console_file_retry(&path)?;
        Ok(Self::RotatingFile {
            path,
            backup_path,
            max_bytes: cap,
            written: 0,
            file: Some(file),
            buf: Vec::with_capacity(TX_BUF_CAP),
        })
    }

    fn write_byte(&mut self, byte: u8) {
        match self {
            ConsoleSink::Stdout { buf } => {
                buf.push(byte);
                if byte == b'\n' || buf.len() >= TX_BUF_CAP {
                    self.flush();
                }
            }
            ConsoleSink::RotatingFile { buf, .. } => {
                buf.push(byte);
                if byte == b'\n' || buf.len() >= TX_BUF_CAP {
                    self.flush();
                }
            }
            #[cfg(test)]
            ConsoleSink::Buffer(buf) => buf.push(byte),
        }
    }

    fn flush(&mut self) {
        match self {
            ConsoleSink::Stdout { buf } => {
                if !buf.is_empty() {
                    let mut out = io::stdout().lock();
                    let _ = write_all_retry(&mut out, buf);
                    let _ = out.flush();
                    buf.clear();
                }
            }
            ConsoleSink::RotatingFile {
                path,
                backup_path,
                max_bytes,
                written,
                file,
                buf,
            } => {
                let mut pos = 0usize;
                while pos < buf.len() {
                    if file.is_none() {
                        *file = open_console_file_retry(path).ok();
                        if file.is_none() {
                            break;
                        }
                    }
                    if *written >= *max_bytes {
                        *file = None;
                        let _ = fs::rename(&*path, &*backup_path);
                        *file = open_console_file_retry(path).ok();
                        *written = 0;
                        if file.is_none() {
                            break;
                        }
                    }
                    let space = (*max_bytes - *written).min((buf.len() - pos) as u64) as usize;
                    if let Some(f) = file.as_mut() {
                        if write_all_retry(f, &buf[pos..pos + space]).is_ok() {
                            *written += space as u64;
                            pos += space;
                        } else {
                            break;
                        }
                    }
                }
                buf.clear();
                if let Some(f) = file.as_mut() {
                    let _ = f.flush();
                }
            }
            #[cfg(test)]
            ConsoleSink::Buffer(_) => {}
        }
    }
}

struct Pl011State {
    rx_fifo: VecDeque<u8>,
    rsr: u32,
    ilpr: u32,
    ibrd: u32,
    fbrd: u32,
    lcr_h: u32,
    cr: u32,
    ifls: u32,
    imsc: u32,
    ris: u32,
    dmacr: u32,
    sink: ConsoleSink,
}

impl Pl011State {
    fn update_irq(&self, irq: &IrqLine) {
        irq.set_level((self.ris & self.imsc) != 0);
    }
}

pub struct Pl011 {
    state: Mutex<Pl011State>,
    irq: IrqLine,
}

impl Pl011 {
    pub fn new(sink: ConsoleSink, irq: IrqLine) -> Self {
        Self {
            state: Mutex::new(Pl011State {
                rx_fifo: VecDeque::with_capacity(RX_FIFO_CAP),
                rsr: 0,
                ilpr: 0,
                ibrd: 0,
                fbrd: 0,
                lcr_h: 0,
                cr: 0x0300,
                ifls: 0x12,
                imsc: 0,
                ris: INT_TX,
                dmacr: 0,
                sink,
            }),
            irq,
        }
    }

    #[allow(dead_code)]
    pub fn push_rx(&self, bytes: &[u8]) {
        let mut s = self.state.lock().unwrap();
        for &b in bytes {
            if s.rx_fifo.len() < RX_FIFO_CAP {
                s.rx_fifo.push_back(b);
            }
        }
        if !s.rx_fifo.is_empty() {
            s.ris |= INT_RX;
        }
        s.update_irq(&self.irq);
    }

    pub fn flush(&self) {
        self.state.lock().unwrap().sink.flush();
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
        let val = u32::from_le_bytes(buf);
        self.write_reg(offset, val);
    }

    fn read_reg(&self, offset: u64) -> u32 {
        if (0xfe0..=0xffc).contains(&offset) && (offset & 3) == 0 {
            let idx = ((offset - 0xfe0) >> 2) as usize;
            return PRIMECELL_ID[idx] as u32;
        }
        let mut s = self.state.lock().unwrap();
        match offset {
            UARTDR => {
                let b = s.rx_fifo.pop_front().unwrap_or(0) as u32;
                if s.rx_fifo.is_empty() {
                    s.ris &= !INT_RX;
                    s.update_irq(&self.irq);
                }
                b
            }
            UARTRSR => s.rsr,
            UARTFR => {
                let mut fr = FR_TXFE;
                if s.rx_fifo.is_empty() {
                    fr |= FR_RXFE;
                } else if s.rx_fifo.len() >= RX_FIFO_CAP {
                    fr |= FR_RXFF;
                }
                fr
            }
            UARTILPR => s.ilpr,
            UARTIBRD => s.ibrd,
            UARTFBRD => s.fbrd,
            UARTLCR_H => s.lcr_h,
            UARTCR => s.cr,
            UARTIFLS => s.ifls,
            UARTIMSC => s.imsc,
            UARTRIS => s.ris,
            UARTMIS => s.ris & s.imsc,
            UARTDMACR => s.dmacr,
            _ => 0,
        }
    }

    fn write_reg(&self, offset: u64, val: u32) {
        let mut s = self.state.lock().unwrap();
        match offset {
            UARTDR => {
                s.sink.write_byte((val & 0xff) as u8);
                s.ris |= INT_TX;
                s.update_irq(&self.irq);
            }
            UARTRSR => {
                s.rsr = 0;
            }
            UARTILPR => s.ilpr = val & 0xff,
            UARTIBRD => s.ibrd = val & 0xffff,
            UARTFBRD => s.fbrd = val & 0x3f,
            UARTLCR_H => s.lcr_h = val & 0xff,
            UARTCR => s.cr = val & 0xffff,
            UARTIFLS => s.ifls = val & 0x3f,
            UARTIMSC => {
                s.imsc = val & 0x7ff;
                s.update_irq(&self.irq);
            }
            UARTICR => {
                s.ris &= !(val & 0x7ff);
                if !s.rx_fifo.is_empty() {
                    s.ris |= INT_RX;
                }
                s.update_irq(&self.irq);
            }
            UARTDMACR => s.dmacr = val & 0x7,
            _ => {}
        }
    }

    #[cfg(test)]
    pub fn take_buffer(&self) -> Vec<u8> {
        let mut s = self.state.lock().unwrap();
        match &mut s.sink {
            ConsoleSink::Buffer(b) => std::mem::take(b),
            _ => Vec::new(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::boot::PL011_SPI;
    use crate::irq::{test_util::MockSpiBackend, SpiMux};

    #[test]
    fn tx_rx_irq_and_primecell_ids() {
        let backend = MockSpiBackend::new();
        let mux = SpiMux::new(backend.clone());
        let uart = Pl011::new(ConsoleSink::Buffer(Vec::new()), mux.line(PL011_SPI, 0));

        let mut buf = [0u8; 4];
        uart.read(UARTFR, &mut buf);
        assert_eq!(u32::from_le_bytes(buf), 0x90);

        for (i, &expected) in PRIMECELL_ID.iter().enumerate() {
            uart.read(0xfe0 + (i as u64) * 4, &mut buf);
            assert_eq!(u32::from_le_bytes(buf), expected as u32);
        }

        uart.write(UARTDR, &u32::to_le_bytes(b'H' as u32));
        uart.write(UARTDR, &u32::to_le_bytes(b'i' as u32));
        assert_eq!(uart.take_buffer(), b"Hi");

        uart.write(UARTIMSC, &INT_RX.to_le_bytes());
        assert!(!backend.level(PL011_SPI));
        uart.push_rx(b"A");
        assert!(backend.level(PL011_SPI));

        uart.read(UARTFR, &mut buf);
        assert_eq!(u32::from_le_bytes(buf) & FR_RXFE, 0);

        uart.read(UARTDR, &mut buf);
        assert_eq!(u32::from_le_bytes(buf), b'A' as u32);
        assert!(!backend.level(PL011_SPI));
    }

    #[test]
    fn rotating_console_log_enforces_size_cap() {
        let base = std::env::temp_dir().join(format!(
            "kido-vmm-console-rot-{}.log",
            std::process::id()
        ));
        let backup = PathBuf::from(format!("{}.1", base.display()));
        let _ = fs::remove_file(&base);
        let _ = fs::remove_file(&backup);

        let backend = MockSpiBackend::new();
        let mux = SpiMux::new(backend);
        let sink = ConsoleSink::rotating_file(base.clone(), 4096).unwrap();
        let uart = Pl011::new(sink, mux.line(PL011_SPI, 0));

        for _ in 0..5000 {
            uart.write(UARTDR, &u32::to_le_bytes(b'X' as u32));
        }
        uart.flush();

        assert_eq!(fs::metadata(&backup).unwrap().len(), 4096);
        assert_eq!(fs::metadata(&base).unwrap().len(), 5000 - 4096);

        let _ = fs::remove_file(&base);
        let _ = fs::remove_file(&backup);
    }
}
