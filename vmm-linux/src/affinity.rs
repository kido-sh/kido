use std::collections::{BTreeMap, BTreeSet};

const MAX_CPU_ID: usize = 1023;
const MAX_CPU_RANGE: usize = 1024;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CpuInfo {
    pub id: usize,
    pub midr: Option<u64>,
    pub capacity: u32,
    pub max_freq_khz: u32,
}

pub fn parse_cpu_list(spec: &str) -> Result<Vec<usize>, String> {
    let trimmed = spec.trim();
    if trimmed.is_empty() {
        return Err("empty CPU list".to_string());
    }
    let mut set = BTreeSet::new();
    for part in trimmed.split(',') {
        let p = part.trim();
        if p.is_empty() {
            return Err(format!("empty entry in CPU list '{spec}'"));
        }
        if let Some((lo_s, hi_s)) = p.split_once('-') {
            let lo: usize = lo_s
                .trim()
                .parse()
                .map_err(|_| format!("invalid CPU range '{p}'"))?;
            let hi: usize = hi_s
                .trim()
                .parse()
                .map_err(|_| format!("invalid CPU range '{p}'"))?;
            if hi < lo || hi > MAX_CPU_ID || hi - lo >= MAX_CPU_RANGE {
                return Err(format!("invalid CPU range '{p}'"));
            }
            for id in lo..=hi {
                set.insert(id);
            }
        } else {
            let id: usize = p.parse().map_err(|_| format!("invalid CPU id '{p}'"))?;
            if id > MAX_CPU_ID {
                return Err(format!("CPU id {id} exceeds max {MAX_CPU_ID}"));
            }
            set.insert(id);
        }
    }
    Ok(set.into_iter().collect())
}

fn arm_part_tier(midr: Option<u64>) -> u32 {
    let Some(m) = midr else {
        return 0;
    };
    if ((m >> 24) & 0xff) != 0x41 {
        return 0;
    }
    match ((m >> 4) & 0xfff) as u16 {
        0xd03 | 0xd04 | 0xd05 | 0xd46 | 0xd4a | 0xd80 => 1,
        0xd07 | 0xd08 | 0xd09 | 0xd0a | 0xd0b | 0xd0c | 0xd0d | 0xd41 | 0xd42 | 0xd47 | 0xd49
        | 0xd4b | 0xd4d | 0xd81 | 0xd87 | 0xd8e => 2,
        0xd40 | 0xd44 | 0xd48 | 0xd4e | 0xd4f | 0xd82 | 0xd84 | 0xd85 => 3,
        _ => 0,
    }
}

pub fn select_best_homogeneous_cluster(cpus: &[CpuInfo]) -> Vec<usize> {
    if cpus.is_empty() {
        return Vec::new();
    }
    let mut clusters: BTreeMap<(Option<u64>, u32, u32), Vec<&CpuInfo>> = BTreeMap::new();
    for cpu in cpus {
        let key = match cpu.midr {
            Some(m) => (Some(m), 0, 0),
            None => (None, cpu.capacity, cpu.max_freq_khz),
        };
        clusters.entry(key).or_default().push(cpu);
    }
    if clusters.len() <= 1 {
        let mut ids: Vec<usize> = cpus.iter().map(|c| c.id).collect();
        ids.sort_unstable();
        ids.dedup();
        return ids;
    }

    let mut ranked: Vec<(u32, u32, u32, usize, Vec<usize>)> = clusters
        .into_iter()
        .map(|((midr, _, _), members)| {
            let cap = members.iter().map(|c| c.capacity).max().unwrap_or(0);
            let freq = members.iter().map(|c| c.max_freq_khz).max().unwrap_or(0);
            let tier = arm_part_tier(midr);
            let mut ids: Vec<usize> = members.iter().map(|c| c.id).collect();
            ids.sort_unstable();
            ids.dedup();
            (cap, freq, tier, ids.len(), ids)
        })
        .collect();

    ranked.sort_by(|a, b| {
        b.0.cmp(&a.0)
            .then_with(|| b.1.cmp(&a.1))
            .then_with(|| b.2.cmp(&a.2))
            .then_with(|| b.3.cmp(&a.3))
            .then_with(|| a.4.first().cmp(&b.4.first()))
    });

    ranked.into_iter().next().map(|r| r.4).unwrap_or_default()
}

#[cfg(target_os = "linux")]
pub fn pin_current_thread(cpus: &[usize]) -> Result<(), String> {
    if cpus.is_empty() {
        return Ok(());
    }
    unsafe {
        let mut set: libc::cpu_set_t = std::mem::zeroed();
        libc::CPU_ZERO(&mut set);
        for &cpu in cpus {
            if cpu < libc::CPU_SETSIZE as usize {
                libc::CPU_SET(cpu, &mut set);
            }
        }
        let rc = libc::sched_setaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &set);
        if rc != 0 {
            let err = std::io::Error::last_os_error();
            return Err(format!("sched_setaffinity failed: {err}"));
        }
    }
    Ok(())
}

#[cfg(target_os = "linux")]
fn read_allowed_cpus() -> Vec<usize> {
    unsafe {
        let mut set: libc::cpu_set_t = std::mem::zeroed();
        libc::CPU_ZERO(&mut set);
        if libc::sched_getaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &mut set) != 0 {
            return Vec::new();
        }
        let mut out = Vec::new();
        for cpu in 0..(libc::CPU_SETSIZE as usize).min(MAX_CPU_ID + 1) {
            if libc::CPU_ISSET(cpu, &set) {
                out.push(cpu);
            }
        }
        out
    }
}

#[cfg(target_os = "linux")]
fn read_sysfs_u64(path: &str) -> Option<u64> {
    let raw = std::fs::read_to_string(path).ok()?;
    let s = raw.trim();
    if let Some(hex) = s.strip_prefix("0x").or_else(|| s.strip_prefix("0X")) {
        u64::from_str_radix(hex, 16).ok()
    } else {
        s.parse::<u64>().ok()
    }
}

#[cfg(target_os = "linux")]
pub fn resolve_and_pin_process(pin_spec: Option<&str>) -> Result<Vec<usize>, String> {
    let cpus = if let Some(spec) = pin_spec {
        parse_cpu_list(spec)?
    } else {
        let allowed = read_allowed_cpus();
        let infos: Vec<CpuInfo> = allowed
            .iter()
            .map(|&id| {
                let base = format!("/sys/devices/system/cpu/cpu{id}");
                let midr = read_sysfs_u64(&format!("{base}/regs/identification/midr_el1"));
                let capacity = read_sysfs_u64(&format!("{base}/cpu_capacity")).unwrap_or(0) as u32;
                let max_freq_khz =
                    read_sysfs_u64(&format!("{base}/cpufreq/cpuinfo_max_freq")).unwrap_or(0) as u32;
                CpuInfo {
                    id,
                    midr,
                    capacity,
                    max_freq_khz,
                }
            })
            .collect();
        let selected = select_best_homogeneous_cluster(&infos);
        if selected.len() == allowed.len() {
            return Ok(selected);
        }
        selected
    };
    if !cpus.is_empty() {
        pin_current_thread(&cpus)?;
    }
    Ok(cpus)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_ranges_and_dedup() {
        assert_eq!(parse_cpu_list("4-7").unwrap(), vec![4, 5, 6, 7]);
        assert_eq!(parse_cpu_list("7,4-6,5").unwrap(), vec![4, 5, 6, 7]);
        assert!(parse_cpu_list("").is_err());
        assert!(parse_cpu_list("7-4").is_err());
        assert!(parse_cpu_list("abc").is_err());
    }

    #[test]
    fn rk3588_selects_cortex_a76_cluster() {
        let mut cpus = Vec::new();
        for id in 0..4 {
            cpus.push(CpuInfo {
                id,
                midr: Some(0x412f_d050),
                capacity: 446,
                max_freq_khz: 1_800_000,
            });
        }
        for id in 4..8 {
            cpus.push(CpuInfo {
                id,
                midr: Some(0x414f_d0b0),
                capacity: 1024,
                max_freq_khz: 2_400_000,
            });
        }
        assert_eq!(select_best_homogeneous_cluster(&cpus), vec![4, 5, 6, 7]);

        let no_midr: Vec<CpuInfo> = cpus
            .into_iter()
            .map(|mut c| {
                c.midr = None;
                c
            })
            .collect();
        assert_eq!(select_best_homogeneous_cluster(&no_midr), vec![4, 5, 6, 7]);
    }
}
