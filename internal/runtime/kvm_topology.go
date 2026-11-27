package runtime

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	tcgReadyTimeout = 120 * time.Second
	maxCPUID        = 65535
	maxCPURange     = 4096

	KVMModeKVM = "kvm"
	KVMModeTCG = "tcg"
)

func ParseKVMMode(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto", KVMModeKVM:
		return KVMModeKVM, nil
	case KVMModeTCG:
		return KVMModeTCG, nil
	default:
		return "", fmt.Errorf("KIDO_ACCEL=%q: must be auto, kvm, or tcg", raw)
	}
}

func KVMModeFromEnv() (string, error) {
	return ParseKVMMode(os.Getenv("KIDO_ACCEL"))
}

func linuxQEMUAccelConfig(mode string) ([]string, time.Duration) {
	if mode == KVMModeTCG {
		return []string{"-machine", "virt,gic-version=3", "-accel", "tcg,thread=multi", "-cpu", "max"}, tcgReadyTimeout
	}
	return []string{"-machine", "virt,gic-version=host", "-accel", "kvm", "-cpu", "host"}, readyTimeout
}

type CPUInfo struct {
	ID         int
	MIDR       uint64
	HasMIDR    bool
	Capacity   int
	MaxFreqKHz int
}

type CPUCluster struct {
	MIDR       uint64
	HasMIDR    bool
	Name       string
	CPUs       []int
	Capacity   int
	MaxFreqKHz int
}

type KVMAffinityPlan struct {
	AllowedCPUs   []int
	Clusters      []CPUCluster
	Selected      CPUCluster
	PinCPUs       []int
	Heterogeneous bool
	Manual        bool
	MaxCPUs       int
}

func (p KVMAffinityPlan) Summary() string {
	if len(p.Clusters) == 0 {
		if len(p.AllowedCPUs) == 0 {
			return ""
		}
		return fmt.Sprintf("%d CPUs", len(p.AllowedCPUs))
	}
	if len(p.Clusters) == 1 {
		c := p.Clusters[0]
		if !c.HasMIDR {
			return fmt.Sprintf("%d CPUs", len(c.CPUs))
		}
		return fmt.Sprintf("%dx %s", len(c.CPUs), c.Name)
	}
	parts := make([]string, len(p.Clusters))
	for i, c := range p.Clusters {
		parts[i] = fmt.Sprintf("%dx %s", len(c.CPUs), c.Name)
	}
	return strings.Join(parts, " + ")
}

type KVMDiag struct {
	Available  bool
	APIVersion int
	Err        error
	Plan       KVMAffinityPlan
	PlanErr    error
}

type armPartInfo struct {
	name string
	tier int
}

var armParts = map[uint16]armPartInfo{
	0xd03: {"Cortex-A53", 1},
	0xd04: {"Cortex-A35", 1},
	0xd05: {"Cortex-A55", 1},
	0xd46: {"Cortex-A510", 1},
	0xd4a: {"Neoverse-E1", 1},
	0xd80: {"Cortex-A520", 1},
	0xd07: {"Cortex-A57", 2},
	0xd08: {"Cortex-A72", 2},
	0xd09: {"Cortex-A73", 2},
	0xd0a: {"Cortex-A75", 2},
	0xd0b: {"Cortex-A76", 2},
	0xd0c: {"Neoverse-N1", 2},
	0xd0d: {"Cortex-A77", 2},
	0xd41: {"Cortex-A78", 2},
	0xd42: {"Cortex-A78AE", 2},
	0xd47: {"Cortex-A710", 2},
	0xd49: {"Neoverse-N2", 2},
	0xd4b: {"Cortex-A78C", 2},
	0xd4d: {"Cortex-A715", 2},
	0xd81: {"Cortex-A720", 2},
	0xd87: {"Cortex-A725", 2},
	0xd8e: {"Neoverse-N3", 2},
	0xd40: {"Neoverse-V1", 3},
	0xd44: {"Cortex-X1", 3},
	0xd48: {"Cortex-X2", 3},
	0xd4e: {"Cortex-X3", 3},
	0xd4f: {"Neoverse-V2", 3},
	0xd82: {"Cortex-X4", 3},
	0xd84: {"Neoverse-V3", 3},
	0xd85: {"Cortex-X925", 3},
}

func armCoreName(midr uint64, hasMIDR bool) string {
	if !hasMIDR {
		return "unknown"
	}
	impl := (midr >> 24) & 0xff
	part := uint16((midr >> 4) & 0xfff)
	switch impl {
	case 0x41:
		if info, ok := armParts[part]; ok {
			return info.name
		}
	case 0xc0:
		switch part {
		case 0xac3:
			return "Ampere-1"
		case 0xac4:
			return "Ampere-1A"
		}
	}
	return fmt.Sprintf("0x%08x", midr)
}

func armPartTier(midr uint64, hasMIDR bool) int {
	if !hasMIDR {
		return 0
	}
	if ((midr >> 24) & 0xff) != 0x41 {
		return 0
	}
	if info, ok := armParts[uint16((midr>>4)&0xfff)]; ok {
		return info.tier
	}
	return 0
}

func readSysfsUint64(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 0, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func readSysfsInt(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || v < 0 {
		return 0
	}
	return v
}

func readCPUInfosFrom(allowed []int, sysCPU, procCPUInfo string) []CPUInfo {
	infos := make([]CPUInfo, 0, len(allowed))
	var cpuinfoFallback map[int]uint64
	readFallback := false
	for _, id := range allowed {
		base := filepath.Join(sysCPU, fmt.Sprintf("cpu%d", id))
		midr, ok := readSysfsUint64(filepath.Join(base, "regs", "identification", "midr_el1"))
		if !ok {
			if !readFallback {
				readFallback = true
				if b, err := os.ReadFile(procCPUInfo); err == nil {
					cpuinfoFallback = parseProcCPUInfoMIDR(string(b))
				}
			}
			midr, ok = cpuinfoFallback[id]
		}
		infos = append(infos, CPUInfo{
			ID:         id,
			MIDR:       midr,
			HasMIDR:    ok,
			Capacity:   readSysfsInt(filepath.Join(base, "cpu_capacity")),
			MaxFreqKHz: readSysfsInt(filepath.Join(base, "cpufreq", "cpuinfo_max_freq")),
		})
	}
	return infos
}

func parseProcCPUInfoMIDR(data string) map[int]uint64 {
	out := map[int]uint64{}
	var (
		hasProc, hasImpl, hasPart bool
		proc                      int
		impl, variant, part, rev  uint64
	)
	flush := func() {
		if hasProc && hasImpl && hasPart {
			out[proc] = ((impl & 0xff) << 24) | ((variant & 0xf) << 20) | (0xf << 16) | ((part & 0xfff) << 4) | (rev & 0xf)
		}
		hasProc, hasImpl, hasPart = false, false, false
		proc, impl, variant, part, rev = 0, 0, 0, 0, 0
	}
	for _, rawLine := range strings.Split(data, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			flush()
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "processor":
			if hasProc {
				flush()
			}
			if n, err := strconv.Atoi(val); err == nil && n >= 0 {
				proc = n
				hasProc = true
			}
		case "CPU implementer":
			if v, err := strconv.ParseUint(val, 0, 64); err == nil {
				impl = v
				hasImpl = true
			}
		case "CPU variant":
			if v, err := strconv.ParseUint(val, 0, 64); err == nil {
				variant = v
			}
		case "CPU part":
			if v, err := strconv.ParseUint(val, 0, 64); err == nil {
				part = v
				hasPart = true
			}
		case "CPU revision":
			if v, err := strconv.ParseUint(val, 0, 64); err == nil {
				rev = v
			}
		}
	}
	flush()
	return out
}

func ParseCPUList(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, errors.New("empty CPU list")
	}
	seen := map[int]bool{}
	var out []int
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty entry in %q", spec)
		}
		if loStr, hiStr, ok := strings.Cut(part, "-"); ok {
			lo, err1 := strconv.Atoi(strings.TrimSpace(loStr))
			hi, err2 := strconv.Atoi(strings.TrimSpace(hiStr))
			if err1 != nil || err2 != nil || lo < 0 || hi < lo || hi > maxCPUID || hi-lo > maxCPURange {
				return nil, fmt.Errorf("invalid CPU range %q", part)
			}
			for id := lo; id <= hi; id++ {
				if !seen[id] {
					seen[id] = true
					out = append(out, id)
				}
			}
			continue
		}
		id, err := strconv.Atoi(part)
		if err != nil || id < 0 || id > maxCPUID {
			return nil, fmt.Errorf("invalid CPU ID %q", part)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

func FormatCPUList(cpus []int) string {
	if len(cpus) == 0 {
		return ""
	}
	sorted := slices.Clone(cpus)
	slices.Sort(sorted)
	var parts []string
	start, prev := sorted[0], sorted[0]
	emit := func(lo, hi int) {
		if lo == hi {
			parts = append(parts, strconv.Itoa(lo))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", lo, hi))
		}
	}
	for _, id := range sorted[1:] {
		if id == prev {
			continue
		}
		if id == prev+1 {
			prev = id
			continue
		}
		emit(start, prev)
		start, prev = id, id
	}
	emit(start, prev)
	return strings.Join(parts, ",")
}

func FormatCPUClusters(clusters []CPUCluster) string {
	parts := make([]string, 0, len(clusters))
	for _, c := range clusters {
		parts = append(parts, fmt.Sprintf("%dx %s [%s]", len(c.CPUs), c.Name, FormatCPUList(c.CPUs)))
	}
	return strings.Join(parts, ", ")
}

type clusterKey struct {
	hasMIDR bool
	midr    uint64
}

func groupCPUClusters(cpus []CPUInfo) []CPUCluster {
	byKey := map[clusterKey]*CPUCluster{}
	for _, c := range cpus {
		k := clusterKey{hasMIDR: c.HasMIDR, midr: c.MIDR}
		cl, ok := byKey[k]
		if !ok {
			cl = &CPUCluster{
				MIDR:    c.MIDR,
				HasMIDR: c.HasMIDR,
				Name:    armCoreName(c.MIDR, c.HasMIDR),
			}
			byKey[k] = cl
		}
		cl.CPUs = append(cl.CPUs, c.ID)
		if c.Capacity > cl.Capacity {
			cl.Capacity = c.Capacity
		}
		if c.MaxFreqKHz > cl.MaxFreqKHz {
			cl.MaxFreqKHz = c.MaxFreqKHz
		}
	}
	clusters := make([]CPUCluster, 0, len(byKey))
	for _, cl := range byKey {
		slices.Sort(cl.CPUs)
		clusters = append(clusters, *cl)
	}
	slices.SortFunc(clusters, func(a, b CPUCluster) int {
		if d := cmp.Compare(b.Capacity, a.Capacity); d != 0 {
			return d
		}
		if d := cmp.Compare(b.MaxFreqKHz, a.MaxFreqKHz); d != 0 {
			return d
		}
		if d := cmp.Compare(armPartTier(b.MIDR, b.HasMIDR), armPartTier(a.MIDR, a.HasMIDR)); d != 0 {
			return d
		}
		if d := cmp.Compare(len(b.CPUs), len(a.CPUs)); d != 0 {
			return d
		}
		return cmp.Compare(a.CPUs[0], b.CPUs[0])
	})
	return clusters
}

func SelectKVMAffinity(cpus []CPUInfo, override string) (KVMAffinityPlan, error) {
	if len(cpus) == 0 {
		return KVMAffinityPlan{}, errors.New("no allowed CPUs available")
	}
	sorted := slices.Clone(cpus)
	slices.SortFunc(sorted, func(a, b CPUInfo) int { return cmp.Compare(a.ID, b.ID) })
	byID := make(map[int]CPUInfo, len(sorted))
	allowed := make([]int, 0, len(sorted))
	dedup := make([]CPUInfo, 0, len(sorted))
	for _, c := range sorted {
		if _, dup := byID[c.ID]; dup {
			continue
		}
		byID[c.ID] = c
		allowed = append(allowed, c.ID)
		dedup = append(dedup, c)
	}
	clusters := groupCPUClusters(dedup)
	plan := KVMAffinityPlan{
		AllowedCPUs:   allowed,
		Clusters:      clusters,
		Heterogeneous: len(clusters) > 1,
	}
	override = strings.TrimSpace(override)
	if override != "" {
		want, err := ParseCPUList(override)
		if err != nil {
			return plan, fmt.Errorf("KIDO_KVM_CPUS=%q: %w", override, err)
		}
		selectedInfos := make([]CPUInfo, 0, len(want))
		for _, id := range want {
			info, ok := byID[id]
			if !ok {
				return plan, fmt.Errorf("KIDO_KVM_CPUS=%q: CPU %d is not in this process's allowed CPUs (%s)", override, id, FormatCPUList(allowed))
			}
			selectedInfos = append(selectedInfos, info)
		}
		selectedClusters := groupCPUClusters(selectedInfos)
		if len(selectedClusters) > 1 {
			return plan, fmt.Errorf("KIDO_KVM_CPUS=%q: CPUs span multiple CPU types (%s), KVM requires a single homogeneous cluster", override, FormatCPUClusters(selectedClusters))
		}
		plan.Selected = selectedClusters[0]
		plan.PinCPUs = want
		plan.Manual = true
		plan.MaxCPUs = len(want)
		return plan, nil
	}
	plan.Selected = clusters[0]
	if plan.Heterogeneous {
		plan.PinCPUs = slices.Clone(clusters[0].CPUs)
		plan.MaxCPUs = len(clusters[0].CPUs)
	} else {
		plan.MaxCPUs = len(allowed)
	}
	return plan, nil
}

func resolvePlanCPUs(requested int, plan KVMAffinityPlan) (int, error) {
	maxCPUs := plan.MaxCPUs
	if maxCPUs < 1 {
		return 0, errors.New("no CPUs available for KVM")
	}
	if requested == 0 {
		return min(defaultCPUs, maxCPUs), nil
	}
	if requested < 1 {
		return 0, fmt.Errorf("--cpus %d: must be between 1 and %d", requested, maxCPUs)
	}
	if requested > maxCPUs {
		if plan.Manual {
			return 0, fmt.Errorf("--cpus %d: exceeds the %d CPUs selected by KIDO_KVM_CPUS (%s)", requested, maxCPUs, FormatCPUList(plan.PinCPUs))
		}
		if plan.Heterogeneous {
			return 0, fmt.Errorf("--cpus %d: exceeds the %d CPUs in KVM cluster %s (%s), heterogeneous ARM64 hosts pin KVM to one cluster (%s)",
				requested, maxCPUs, FormatCPUList(plan.Selected.CPUs), plan.Selected.Name, FormatCPUClusters(plan.Clusters))
		}
		return 0, fmt.Errorf("--cpus %d: must be between 1 and %d", requested, maxCPUs)
	}
	return requested, nil
}
