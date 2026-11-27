#[cfg(not(target_os = "linux"))]
use std::fs::File;
#[cfg(not(target_os = "linux"))]
use std::io::Read;
use std::path::Path;

use vm_fdt::FdtWriter;

use crate::memory::{GuestMemory, RAM_BASE};

pub const FDT_GPA: u64 = RAM_BASE;
pub const FDT_MAX_SIZE: usize = 2 * 1024 * 1024;
pub const KERNEL_GPA: u64 = RAM_BASE + 0x0020_0000;
pub const KERNEL_ALIGN: u64 = 0x0020_0000;
pub const INITRD_ALIGN: u64 = 0x1000;

pub const GICD_BASE: u64 = 0x0800_0000;
pub const GICD_SIZE: u64 = 0x0001_0000;
pub const GICR_BASE: u64 = 0x080a_0000;
pub const GICR_SIZE: u64 = 0x00f6_0000;

pub const PL011_BASE: u64 = 0x0900_0000;
pub const PL011_SIZE: u64 = 0x1000;
pub const PL011_SPI: u32 = 1;

pub const PL031_BASE: u64 = 0x0901_0000;
pub const PL031_SIZE: u64 = 0x1000;
pub const PL031_SPI: u32 = 2;

pub const PCIE_MMIO32_BASE: u64 = 0x1000_0000;
pub const PCIE_MMIO32_SIZE: u64 = 0x2eff_0000;

pub const PCIE_MMIO64_BASE: u64 = 0x80_0000_0000;
pub const PCIE_MMIO64_SIZE: u64 = 0x40_0000_0000;

pub const PCIE_ECAM_HIGH_BASE: u64 = 0x40_1000_0000;
pub const PCIE_ECAM_HIGH_SIZE: u64 = 0x1000_0000;
pub const PCIE_ECAM_LOW_BASE: u64 = 0x3f00_0000;
pub const PCIE_ECAM_LOW_SIZE: u64 = 0x0100_0000;

pub const GIC_PHANDLE: u32 = 0x8001;
const CLK_PHANDLE: u32 = 0x8000;

const ARM64_IMAGE_MAGIC: u32 = 0x644d_5241;
const MAX_BOOTARGS_LEN: usize = 1023;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Arm64ImageHeader {
    pub text_offset: u64,
    pub image_size: u64,
    pub flags: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct LoadedImages {
    pub kernel_gpa: u64,
    pub kernel_reserved_span: u64,
    pub initrd_gpa: u64,
    pub initrd_end: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct EcamLayout {
    pub base: u64,
    pub size: u64,
    pub max_bus: u32,
}

pub fn decode_pa_bits(id_aa64mmfr0_el1: u64) -> u32 {
    const PA_BITS: [u32; 7] = [32, 36, 40, 42, 44, 48, 52];
    let idx = (id_aa64mmfr0_el1 & 0xf) as usize;
    PA_BITS.get(idx).copied().unwrap_or(52)
}

pub fn ecam_layout_for_pa_bits(pa_bits: u32) -> EcamLayout {
    if pa_bits >= 40 {
        EcamLayout {
            base: PCIE_ECAM_HIGH_BASE,
            size: PCIE_ECAM_HIGH_SIZE,
            max_bus: 255,
        }
    } else {
        EcamLayout {
            base: PCIE_ECAM_LOW_BASE,
            size: PCIE_ECAM_LOW_SIZE,
            max_bus: 15,
        }
    }
}

pub fn mpidr_to_dt_affinity(mpidr: u64) -> u64 {
    mpidr & 0x00ff_00ff_ffff
}

pub fn parse_arm64_image_header(bytes: &[u8]) -> Result<Arm64ImageHeader, String> {
    if bytes.len() < 64 {
        return Err(format!(
            "ARM64 Image too small: {} bytes (need >= 64)",
            bytes.len()
        ));
    }
    let magic = u32::from_le_bytes(bytes[0x38..0x3c].try_into().unwrap());
    if magic != ARM64_IMAGE_MAGIC {
        return Err(format!(
            "invalid ARM64 Image magic: {magic:#010x} (expected {ARM64_IMAGE_MAGIC:#010x})"
        ));
    }
    let text_offset = u64::from_le_bytes(bytes[0x08..0x10].try_into().unwrap());
    let image_size = u64::from_le_bytes(bytes[0x10..0x18].try_into().unwrap());
    let flags = u64::from_le_bytes(bytes[0x18..0x20].try_into().unwrap());
    Ok(Arm64ImageHeader {
        text_offset,
        image_size,
        flags,
    })
}

#[inline]
fn align_up(val: u64, align: u64) -> Result<u64, String> {
    val.checked_add(align - 1)
        .map(|v| v & !(align - 1))
        .ok_or_else(|| format!("alignment overflow for {val:#x}"))
}

pub fn load_kernel_and_initrd_bytes(
    mem: &GuestMemory,
    kernel_bytes: &[u8],
    initrd_bytes: &[u8],
) -> Result<LoadedImages, String> {
    let hdr = parse_arm64_image_header(kernel_bytes)?;
    if hdr.text_offset != 0 {
        return Err(format!(
            "unsupported non-zero ARM64 Image text_offset {:#x}",
            hdr.text_offset
        ));
    }
    let file_len = kernel_bytes.len() as u64;
    let effective_len = file_len.max(hdr.image_size);
    let reserved_span = align_up(effective_len.max(KERNEL_ALIGN), KERNEL_ALIGN)?;

    let kernel_end = KERNEL_GPA
        .checked_add(reserved_span)
        .ok_or_else(|| "kernel GPA overflow".to_string())?;
    if kernel_end > mem.end() {
        return Err(format!(
            "kernel image ({effective_len} bytes, span {reserved_span:#x}) exceeds guest RAM"
        ));
    }

    mem.write_slice(KERNEL_GPA, kernel_bytes)
        .map_err(|e| format!("failed to copy kernel into guest RAM: {e}"))?;

    if effective_len > file_len {
        let bss_len = usize::try_from(effective_len - file_len)
            .map_err(|_| "kernel .bss size overflow".to_string())?;
        mem.zero_range(KERNEL_GPA + file_len, bss_len)
            .map_err(|e| format!("failed to zero kernel .bss: {e}"))?;
    }

    let initrd_gpa = align_up(KERNEL_GPA + reserved_span, INITRD_ALIGN)?;
    let initrd_end = initrd_gpa
        .checked_add(initrd_bytes.len() as u64)
        .ok_or_else(|| "initrd GPA overflow".to_string())?;
    if initrd_end > mem.end() {
        return Err(format!(
            "initrd ({} bytes at {initrd_gpa:#x}) exceeds guest RAM end {:#x}",
            initrd_bytes.len(),
            mem.end()
        ));
    }
    if !initrd_bytes.is_empty() {
        mem.write_slice(initrd_gpa, initrd_bytes)
            .map_err(|e| format!("failed to copy initrd into guest RAM: {e}"))?;
    }

    Ok(LoadedImages {
        kernel_gpa: KERNEL_GPA,
        kernel_reserved_span: reserved_span,
        initrd_gpa,
        initrd_end,
    })
}

pub fn load_kernel_and_initrd(
    mem: &GuestMemory,
    kernel_path: &Path,
    initrd_path: &Path,
) -> Result<LoadedImages, String> {
    let kernel_bytes = std::fs::read(kernel_path)
        .map_err(|e| format!("read kernel '{}': {e}", kernel_path.display()))?;
    let initrd_bytes = std::fs::read(initrd_path)
        .map_err(|e| format!("read initrd '{}': {e}", initrd_path.display()))?;
    load_kernel_and_initrd_bytes(mem, &kernel_bytes, &initrd_bytes)
}

pub struct FdtConfig<'a> {
    pub ram_base: u64,
    pub ram_size: u64,
    pub cpu_mpidrs: &'a [u64],
    pub ecam: EcamLayout,
    pub boot_args: &'a str,
    pub initrd_start: u64,
    pub initrd_end: u64,
    pub rng_seed: [u8; 32],
}

pub fn zeroize_bytes(buf: &mut [u8]) {
    for b in buf.iter_mut() {
        unsafe {
            std::ptr::write_volatile(b, 0);
        }
    }
    std::sync::atomic::compiler_fence(std::sync::atomic::Ordering::SeqCst);
}

pub fn read_urandom_seed() -> Result<[u8; 32], String> {
    let mut seed = [0u8; 32];
    #[cfg(target_os = "linux")]
    {
        let mut done = 0usize;
        while done < seed.len() {
            let n = unsafe {
                libc::getrandom(
                    seed.as_mut_ptr().add(done).cast::<libc::c_void>(),
                    seed.len() - done,
                    0,
                )
            };
            if n < 0 {
                let err = std::io::Error::last_os_error();
                if err.raw_os_error() == Some(libc::EINTR) {
                    continue;
                }
                return Err(format!("getrandom failed: {err}"));
            }
            if n == 0 {
                return Err("getrandom returned 0 bytes".to_string());
            }
            done += n as usize;
        }
        Ok(seed)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let mut f = File::open("/dev/urandom").map_err(|e| format!("open /dev/urandom: {e}"))?;
        f.read_exact(&mut seed)
            .map_err(|e| format!("read /dev/urandom: {e}"))?;
        Ok(seed)
    }
}

pub fn build_fdt(cfg: &FdtConfig<'_>) -> Result<Vec<u8>, String> {
    if cfg.cpu_mpidrs.is_empty() {
        return Err("cannot build FDT with zero CPUs".to_string());
    }
    if cfg.boot_args.len() > MAX_BOOTARGS_LEN {
        return Err(format!(
            "bootargs too long: {} bytes (max {MAX_BOOTARGS_LEN})",
            cfg.boot_args.len()
        ));
    }

    let mut fdt = FdtWriter::new().map_err(|e| format!("FdtWriter::new: {e}"))?;
    let root = fdt.begin_node("").map_err(|e| format!("begin root: {e}"))?;
    fdt.property_u32("#address-cells", 2)
        .map_err(|e| format!("root #address-cells: {e}"))?;
    fdt.property_u32("#size-cells", 2)
        .map_err(|e| format!("root #size-cells: {e}"))?;
    fdt.property_u32("interrupt-parent", GIC_PHANDLE)
        .map_err(|e| format!("root interrupt-parent: {e}"))?;
    fdt.property_string("compatible", "linux,dummy-virt")
        .map_err(|e| format!("root compatible: {e}"))?;

    // /chosen
    let chosen = fdt
        .begin_node("chosen")
        .map_err(|e| format!("begin chosen: {e}"))?;
    fdt.property_string("stdout-path", "/pl011@9000000")
        .map_err(|e| format!("chosen stdout-path: {e}"))?;
    fdt.property_string("bootargs", cfg.boot_args)
        .map_err(|e| format!("chosen bootargs: {e}"))?;
    if cfg.initrd_end > cfg.initrd_start {
        fdt.property_u64("linux,initrd-start", cfg.initrd_start)
            .map_err(|e| format!("chosen initrd-start: {e}"))?;
        fdt.property_u64("linux,initrd-end", cfg.initrd_end)
            .map_err(|e| format!("chosen initrd-end: {e}"))?;
    }
    fdt.property("rng-seed", &cfg.rng_seed)
        .map_err(|e| format!("chosen rng-seed: {e}"))?;
    fdt.end_node(chosen)
        .map_err(|e| format!("end chosen: {e}"))?;

    // /aliases
    let aliases = fdt
        .begin_node("aliases")
        .map_err(|e| format!("begin aliases: {e}"))?;
    fdt.property_string("serial0", "/pl011@9000000")
        .map_err(|e| format!("aliases serial0: {e}"))?;
    fdt.end_node(aliases)
        .map_err(|e| format!("end aliases: {e}"))?;

    // /memory@40000000
    let mem_name = format!("memory@{:x}", cfg.ram_base);
    let memory = fdt
        .begin_node(&mem_name)
        .map_err(|e| format!("begin memory: {e}"))?;
    fdt.property_string("device_type", "memory")
        .map_err(|e| format!("memory device_type: {e}"))?;
    fdt.property_array_u64("reg", &[cfg.ram_base, cfg.ram_size])
        .map_err(|e| format!("memory reg: {e}"))?;
    fdt.end_node(memory)
        .map_err(|e| format!("end memory: {e}"))?;

    // /cpus
    let need_64bit_mpidr = cfg
        .cpu_mpidrs
        .iter()
        .any(|&m| mpidr_to_dt_affinity(m) > u32::MAX as u64);
    let cpus = fdt
        .begin_node("cpus")
        .map_err(|e| format!("begin cpus: {e}"))?;
    fdt.property_u32("#address-cells", if need_64bit_mpidr { 2 } else { 1 })
        .map_err(|e| format!("cpus #address-cells: {e}"))?;
    fdt.property_u32("#size-cells", 0)
        .map_err(|e| format!("cpus #size-cells: {e}"))?;
    for &raw_mpidr in cfg.cpu_mpidrs.iter() {
        let aff = mpidr_to_dt_affinity(raw_mpidr);
        let cpu_name = format!("cpu@{aff:x}");
        let cpu = fdt
            .begin_node(&cpu_name)
            .map_err(|e| format!("begin {cpu_name}: {e}"))?;
        fdt.property_string("device_type", "cpu")
            .map_err(|e| format!("cpu device_type: {e}"))?;
        fdt.property_string("compatible", "arm,arm-v8")
            .map_err(|e| format!("cpu compatible: {e}"))?;
        fdt.property_string("enable-method", "psci")
            .map_err(|e| format!("cpu enable-method: {e}"))?;
        if need_64bit_mpidr {
            fdt.property_u64("reg", aff)
                .map_err(|e| format!("cpu reg: {e}"))?;
        } else {
            fdt.property_u32("reg", aff as u32)
                .map_err(|e| format!("cpu reg: {e}"))?;
        }
        fdt.end_node(cpu)
            .map_err(|e| format!("end {cpu_name}: {e}"))?;
    }
    fdt.end_node(cpus).map_err(|e| format!("end cpus: {e}"))?;

    // /psci
    let psci = fdt
        .begin_node("psci")
        .map_err(|e| format!("begin psci: {e}"))?;
    fdt.property_string_list(
        "compatible",
        vec!["arm,psci-1.0".to_string(), "arm,psci-0.2".to_string()],
    )
    .map_err(|e| format!("psci compatible: {e}"))?;
    fdt.property_string("method", "hvc")
        .map_err(|e| format!("psci method: {e}"))?;
    fdt.end_node(psci).map_err(|e| format!("end psci: {e}"))?;

    // /timer
    let timer = fdt
        .begin_node("timer")
        .map_err(|e| format!("begin timer: {e}"))?;
    fdt.property_string("compatible", "arm,armv8-timer")
        .map_err(|e| format!("timer compatible: {e}"))?;
    fdt.property_null("always-on")
        .map_err(|e| format!("timer always-on: {e}"))?;
    fdt.property_array_u32(
        "interrupts",
        &[
            1, 13, 0x104, // Secure physical timer (PPI 13 -> INTID 29)
            1, 14, 0x104, // Non-secure physical timer (PPI 14 -> INTID 30)
            1, 11, 0x104, // Virtual timer (PPI 11 -> INTID 27)
            1, 10, 0x104, // Hypervisor timer (PPI 10 -> INTID 26)
        ],
    )
    .map_err(|e| format!("timer interrupts: {e}"))?;
    fdt.end_node(timer).map_err(|e| format!("end timer: {e}"))?;

    // /apb-pclk
    let clk = fdt
        .begin_node("apb-pclk")
        .map_err(|e| format!("begin apb-pclk: {e}"))?;
    fdt.property_string("compatible", "fixed-clock")
        .map_err(|e| format!("clk compatible: {e}"))?;
    fdt.property_u32("#clock-cells", 0)
        .map_err(|e| format!("clk #clock-cells: {e}"))?;
    fdt.property_u32("clock-frequency", 24_000_000)
        .map_err(|e| format!("clk clock-frequency: {e}"))?;
    fdt.property_string("clock-output-names", "clk24mhz")
        .map_err(|e| format!("clk clock-output-names: {e}"))?;
    fdt.property_phandle(CLK_PHANDLE)
        .map_err(|e| format!("clk phandle: {e}"))?;
    fdt.end_node(clk).map_err(|e| format!("end apb-pclk: {e}"))?;

    // /intc@8000000
    let intc = fdt
        .begin_node("intc@8000000")
        .map_err(|e| format!("begin intc: {e}"))?;
    fdt.property_string("compatible", "arm,gic-v3")
        .map_err(|e| format!("intc compatible: {e}"))?;
    fdt.property_null("interrupt-controller")
        .map_err(|e| format!("intc interrupt-controller: {e}"))?;
    fdt.property_u32("#interrupt-cells", 3)
        .map_err(|e| format!("intc #interrupt-cells: {e}"))?;
    fdt.property_u32("#address-cells", 2)
        .map_err(|e| format!("intc #address-cells: {e}"))?;
    fdt.property_u32("#size-cells", 2)
        .map_err(|e| format!("intc #size-cells: {e}"))?;
    fdt.property_null("ranges")
        .map_err(|e| format!("intc ranges: {e}"))?;
    fdt.property_phandle(GIC_PHANDLE)
        .map_err(|e| format!("intc phandle: {e}"))?;
    fdt.property_array_u64("reg", &[GICD_BASE, GICD_SIZE, GICR_BASE, GICR_SIZE])
        .map_err(|e| format!("intc reg: {e}"))?;
    fdt.end_node(intc).map_err(|e| format!("end intc: {e}"))?;

    // /pl011@9000000
    let pl011 = fdt
        .begin_node("pl011@9000000")
        .map_err(|e| format!("begin pl011: {e}"))?;
    fdt.property_string_list(
        "compatible",
        vec!["arm,pl011".to_string(), "arm,primecell".to_string()],
    )
    .map_err(|e| format!("pl011 compatible: {e}"))?;
    fdt.property_array_u64("reg", &[PL011_BASE, PL011_SIZE])
        .map_err(|e| format!("pl011 reg: {e}"))?;
    fdt.property_array_u32("interrupts", &[0, PL011_SPI, 4])
        .map_err(|e| format!("pl011 interrupts: {e}"))?;
    fdt.property_array_u32("clocks", &[CLK_PHANDLE, CLK_PHANDLE])
        .map_err(|e| format!("pl011 clocks: {e}"))?;
    fdt.property_string_list(
        "clock-names",
        vec!["uartclk".to_string(), "apb_pclk".to_string()],
    )
    .map_err(|e| format!("pl011 clock-names: {e}"))?;
    fdt.end_node(pl011).map_err(|e| format!("end pl011: {e}"))?;

    // /pl031@9010000
    let pl031 = fdt
        .begin_node("pl031@9010000")
        .map_err(|e| format!("begin pl031: {e}"))?;
    fdt.property_string_list(
        "compatible",
        vec!["arm,pl031".to_string(), "arm,primecell".to_string()],
    )
    .map_err(|e| format!("pl031 compatible: {e}"))?;
    fdt.property_array_u64("reg", &[PL031_BASE, PL031_SIZE])
        .map_err(|e| format!("pl031 reg: {e}"))?;
    fdt.property_array_u32("interrupts", &[0, PL031_SPI, 4])
        .map_err(|e| format!("pl031 interrupts: {e}"))?;
    fdt.property_u32("clocks", CLK_PHANDLE)
        .map_err(|e| format!("pl031 clocks: {e}"))?;
    fdt.property_string("clock-names", "apb_pclk")
        .map_err(|e| format!("pl031 clock-names: {e}"))?;
    fdt.end_node(pl031).map_err(|e| format!("end pl031: {e}"))?;

    // /pcie@10000000
    let pcie = fdt
        .begin_node("pcie@10000000")
        .map_err(|e| format!("begin pcie: {e}"))?;
    fdt.property_string("compatible", "pci-host-ecam-generic")
        .map_err(|e| format!("pcie compatible: {e}"))?;
    fdt.property_string("device_type", "pci")
        .map_err(|e| format!("pcie device_type: {e}"))?;
    fdt.property_u32("#address-cells", 3)
        .map_err(|e| format!("pcie #address-cells: {e}"))?;
    fdt.property_u32("#size-cells", 2)
        .map_err(|e| format!("pcie #size-cells: {e}"))?;
    fdt.property_u32("#interrupt-cells", 1)
        .map_err(|e| format!("pcie #interrupt-cells: {e}"))?;
    fdt.property_u32("linux,pci-domain", 0)
        .map_err(|e| format!("pcie domain: {e}"))?;
    fdt.property_array_u32("bus-range", &[0, cfg.ecam.max_bus])
        .map_err(|e| format!("pcie bus-range: {e}"))?;
    fdt.property_array_u64("reg", &[cfg.ecam.base, cfg.ecam.size])
        .map_err(|e| format!("pcie reg: {e}"))?;
    // 32-bit non-prefetchable MMIO window: 0x1000_0000..0x3efe_ffff
    // When PA >= 40 bits (high ECAM), also advertise the 64-bit prefetchable window
    // at 0x80_0000_0000..0xbf_ffff_ffff for large VirtIO shared memory BARs.
    let mut ranges = vec![
        0x0200_0000,
        (PCIE_MMIO32_BASE >> 32) as u32,
        PCIE_MMIO32_BASE as u32,
        (PCIE_MMIO32_BASE >> 32) as u32,
        PCIE_MMIO32_BASE as u32,
        (PCIE_MMIO32_SIZE >> 32) as u32,
        PCIE_MMIO32_SIZE as u32,
    ];
    if cfg.ecam.base >= 0x1_0000_0000 {
        ranges.extend_from_slice(&[
            0x4300_0000,
            (PCIE_MMIO64_BASE >> 32) as u32,
            PCIE_MMIO64_BASE as u32,
            (PCIE_MMIO64_BASE >> 32) as u32,
            PCIE_MMIO64_BASE as u32,
            (PCIE_MMIO64_SIZE >> 32) as u32,
            PCIE_MMIO64_SIZE as u32,
        ]);
    }
    fdt.property_array_u32("ranges", &ranges)
        .map_err(|e| format!("pcie ranges: {e}"))?;
    fdt.property_array_u32("interrupt-map-mask", &[0x1800, 0, 0, 0x7])
        .map_err(|e| format!("pcie interrupt-map-mask: {e}"))?;
    let mut irq_map = Vec::with_capacity(16 * 10);
    for dev in 0u32..4 {
        for pin in 1u32..=4 {
            let spi = 3 + ((dev + pin - 1) % 4);
            irq_map.extend_from_slice(&[
                dev << 11,
                0,
                0,
                pin,
                GIC_PHANDLE,
                0,
                0,
                0,
                spi,
                4,
            ]);
        }
    }
    fdt.property_array_u32("interrupt-map", &irq_map)
        .map_err(|e| format!("pcie interrupt-map: {e}"))?;
    fdt.end_node(pcie).map_err(|e| format!("end pcie: {e}"))?;

    fdt.end_node(root).map_err(|e| format!("end root: {e}"))?;
    let dtb = fdt.finish().map_err(|e| format!("FdtWriter::finish: {e}"))?;
    if dtb.len() > FDT_MAX_SIZE {
        return Err(format!(
            "FDT blob size {} exceeds {FDT_MAX_SIZE} byte limit",
            dtb.len()
        ));
    }
    Ok(dtb)
}

pub fn write_fdt_to_ram(mem: &GuestMemory, dtb: &[u8]) -> Result<(), String> {
    if dtb.len() > FDT_MAX_SIZE {
        return Err(format!("FDT blob too large: {} bytes", dtb.len()));
    }
    mem.write_slice(FDT_GPA, dtb)
        .map_err(|e| format!("failed to write FDT to guest RAM: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn be32(buf: &[u8], off: usize) -> u32 {
        u32::from_be_bytes(buf[off..off + 4].try_into().unwrap())
    }

    fn collect_fdt_paths_and_props(dtb: &[u8]) -> Vec<(String, String, Vec<u8>)> {
        assert_eq!(be32(dtb, 0), 0xd00d_feed);
        let off_struct = be32(dtb, 8) as usize;
        let off_strings = be32(dtb, 12) as usize;
        let mut pos = off_struct;
        let mut stack: Vec<String> = Vec::new();
        let mut out = Vec::new();
        loop {
            let tok = be32(dtb, pos);
            pos += 4;
            match tok {
                1 => {
                    let end = dtb[pos..].iter().position(|&b| b == 0).unwrap();
                    let name = std::str::from_utf8(&dtb[pos..pos + end]).unwrap().to_string();
                    pos += (end + 4) & !3;
                    stack.push(name);
                }
                2 => {
                    stack.pop();
                }
                3 => {
                    let len = be32(dtb, pos) as usize;
                    let nameoff = be32(dtb, pos + 4) as usize;
                    pos += 8;
                    let val = dtb[pos..pos + len].to_vec();
                    pos += (len + 3) & !3;
                    let s_start = off_strings + nameoff;
                    let s_end = dtb[s_start..].iter().position(|&b| b == 0).unwrap();
                    let pname = std::str::from_utf8(&dtb[s_start..s_start + s_end])
                        .unwrap()
                        .to_string();
                    let path = if stack.len() <= 1 {
                        "/".to_string()
                    } else {
                        format!("/{}", stack[1..].join("/"))
                    };
                    out.push((path, pname, val));
                }
                4 => {}
                9 => break,
                other => panic!("unexpected FDT token {other}"),
            }
        }
        out
    }

    #[test]
    fn arm64_image_loader_verifies_magic_and_zeroes_bss() {
        let mut kernel = vec![0xAAu8; 4096];
        kernel[0x08..0x10].copy_from_slice(&0u64.to_le_bytes());
        kernel[0x10..0x18].copy_from_slice(&8192u64.to_le_bytes());
        kernel[0x38..0x3c].copy_from_slice(&ARM64_IMAGE_MAGIC.to_le_bytes());
        let initrd = vec![0x55u8; 2048];

        let mem = GuestMemory::from_vec(RAM_BASE, vec![0xCCu8; 8 * 1024 * 1024]);
        let loaded = load_kernel_and_initrd_bytes(&mem, &kernel, &initrd).unwrap();
        assert_eq!(loaded.kernel_gpa, KERNEL_GPA);
        assert_eq!(loaded.initrd_gpa, KERNEL_GPA + 0x0020_0000);
        assert_eq!(loaded.initrd_end, loaded.initrd_gpa + 2048);

        let mut bss_check = vec![0xFFu8; 4096];
        mem.read_slice(KERNEL_GPA + 4096, &mut bss_check).unwrap();
        assert!(bss_check.iter().all(|&b| b == 0));

        let mut initrd_check = vec![0u8; 2048];
        mem.read_slice(loaded.initrd_gpa, &mut initrd_check).unwrap();
        assert_eq!(initrd_check, initrd);
    }

    #[test]
    fn arm64_image_rejects_bad_magic_and_nonzero_text_offset() {
        let bad = vec![0u8; 128];
        assert!(parse_arm64_image_header(&bad).is_err());

        let mut nz = vec![0u8; 4096];
        nz[0x08..0x10].copy_from_slice(&0x80000u64.to_le_bytes());
        nz[0x38..0x3c].copy_from_slice(&ARM64_IMAGE_MAGIC.to_le_bytes());
        let mem = GuestMemory::from_vec(RAM_BASE, vec![0u8; 8 * 1024 * 1024]);
        assert!(load_kernel_and_initrd_bytes(&mem, &nz, &[]).is_err());
    }

    #[test]
    fn fdt_matches_guest_hw_contract() {
        let mpidrs = [0x8000_0000u64, 0x8000_0001, 0x8000_0002, 0x8000_0003];
        let dtb = build_fdt(&FdtConfig {
            ram_base: RAM_BASE,
            ram_size: 1024 * 1024 * 1024,
            cpu_mpidrs: &mpidrs,
            ecam: ecam_layout_for_pa_bits(40),
            boot_args: "serial=3 ext4-uuid=abc",
            initrd_start: 0x4040_0000,
            initrd_end: 0x43b0_0000,
            rng_seed: [0x42; 32],
        })
        .unwrap();

        let props = collect_fdt_paths_and_props(&dtb);
        let find = |path: &str, name: &str| -> Option<Vec<u8>> {
            props
                .iter()
                .find(|(p, n, _)| p == path && n == name)
                .map(|(_, _, v)| v.clone())
        };

        assert_eq!(find("/", "compatible").unwrap(), b"linux,dummy-virt\0");
        assert_eq!(find("/chosen", "stdout-path").unwrap(), b"/pl011@9000000\0");
        assert_eq!(find("/chosen", "bootargs").unwrap(), b"serial=3 ext4-uuid=abc\0");
        assert_eq!(find("/chosen", "rng-seed").unwrap(), vec![0x42; 32]);
        assert_eq!(
            u64::from_be_bytes(find("/chosen", "linux,initrd-start").unwrap().try_into().unwrap()),
            0x4040_0000
        );
        assert_eq!(
            u64::from_be_bytes(find("/chosen", "linux,initrd-end").unwrap().try_into().unwrap()),
            0x43b0_0000
        );
        // Verify MPIDR bit 31 RES1 was stripped for PSCI CPU_ON compatibility
        for i in 0..4u32 {
            let reg = find(&format!("/cpus/cpu@{i:x}"), "reg").unwrap();
            assert_eq!(u32::from_be_bytes(reg.try_into().unwrap()), i);
        }
        assert_eq!(find("/psci", "method").unwrap(), b"hvc\0");
        let timer_irqs = find("/timer", "interrupts").unwrap();
        assert_eq!(timer_irqs.len(), 48);
        assert_eq!(be32(&timer_irqs, 24), 1);
        assert_eq!(be32(&timer_irqs, 28), 11);

        let pcie_reg = find("/pcie@10000000", "reg").unwrap();
        assert_eq!(
            u64::from_be_bytes(pcie_reg[0..8].try_into().unwrap()),
            PCIE_ECAM_HIGH_BASE
        );
        assert_eq!(
            u64::from_be_bytes(pcie_reg[8..16].try_into().unwrap()),
            PCIE_ECAM_HIGH_SIZE
        );
        let pcie_ranges = find("/pcie@10000000", "ranges").unwrap();
        assert_eq!(pcie_ranges.len(), 56);
        assert_eq!(be32(&pcie_ranges, 28), 0x4300_0000);
        assert_eq!(
            u64::from_be_bytes(pcie_ranges[32..40].try_into().unwrap()),
            PCIE_MMIO64_BASE
        );

        // Low PA < 40 bits switches ECAM to 0x3f00_0000 and omits 64-bit MMIO range
        let dtb_low = build_fdt(&FdtConfig {
            ram_base: RAM_BASE,
            ram_size: 512 * 1024 * 1024,
            cpu_mpidrs: &[0x8000_0000],
            ecam: ecam_layout_for_pa_bits(36),
            boot_args: "serial=3",
            initrd_start: 0x4040_0000,
            initrd_end: 0x4050_0000,
            rng_seed: [0x11; 32],
        })
        .unwrap();
        let props_low = collect_fdt_paths_and_props(&dtb_low);
        let reg_low = props_low
            .iter()
            .find(|(p, n, _)| p == "/pcie@10000000" && n == "reg")
            .unwrap()
            .2
            .clone();
        assert_eq!(
            u64::from_be_bytes(reg_low[0..8].try_into().unwrap()),
            PCIE_ECAM_LOW_BASE
        );
        let ranges_low = props_low
            .iter()
            .find(|(p, n, _)| p == "/pcie@10000000" && n == "ranges")
            .unwrap()
            .2
            .clone();
        assert_eq!(ranges_low.len(), 28);
    }
}
