package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	midrCortexA55  = 0x412fd050
	midrCortexA76  = 0x414fd0b0
	midrCortexA53  = 0x410fd034
	midrCortexA72  = 0x410fd082
	midrCortexX3   = 0x411fd4e0
	midrNeoverseN1 = 0x413fd0c1
)

func rk3588CPUs() []CPUInfo {
	return []CPUInfo{
		{ID: 0, MIDR: midrCortexA55, HasMIDR: true, Capacity: 446, MaxFreqKHz: 1800000},
		{ID: 1, MIDR: midrCortexA55, HasMIDR: true, Capacity: 446, MaxFreqKHz: 1800000},
		{ID: 2, MIDR: midrCortexA55, HasMIDR: true, Capacity: 446, MaxFreqKHz: 1800000},
		{ID: 3, MIDR: midrCortexA55, HasMIDR: true, Capacity: 446, MaxFreqKHz: 1800000},
		{ID: 4, MIDR: midrCortexA76, HasMIDR: true, Capacity: 1024, MaxFreqKHz: 2400000},
		{ID: 5, MIDR: midrCortexA76, HasMIDR: true, Capacity: 1024, MaxFreqKHz: 2400000},
		{ID: 6, MIDR: midrCortexA76, HasMIDR: true, Capacity: 1024, MaxFreqKHz: 2400000},
		{ID: 7, MIDR: midrCortexA76, HasMIDR: true, Capacity: 1024, MaxFreqKHz: 2400000},
	}
}

func TestParseKVMMode(t *testing.T) {
	cases := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{"", KVMModeKVM, false},
		{"   ", KVMModeKVM, false},
		{"auto", KVMModeKVM, false},
		{"AUTO", KVMModeKVM, false},
		{"kvm", KVMModeKVM, false},
		{" KVM ", KVMModeKVM, false},
		{"tcg", KVMModeTCG, false},
		{" tcg ", KVMModeTCG, false},
		{" TCG ", KVMModeTCG, false},
		{"hvf", "", true},
		{"unknown", "", true},
	}
	for _, tc := range cases {
		got, err := ParseKVMMode(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseKVMMode(%q) succeeded, want error", tc.raw)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseKVMMode(%q) = %q, %v; want %q, nil", tc.raw, got, err, tc.want)
		}
	}

	t.Setenv("KIDO_ACCEL", " tcg ")
	if got, err := KVMModeFromEnv(); err != nil || got != KVMModeTCG {
		t.Fatalf("KVMModeFromEnv(tcg) = %q, %v; want %q, nil", got, err, KVMModeTCG)
	}
	t.Setenv("KIDO_ACCEL", "hvf")
	if _, err := KVMModeFromEnv(); err == nil {
		t.Fatal("KVMModeFromEnv(hvf) succeeded, want error")
	}
}

func TestLinuxQEMUAccelConfig(t *testing.T) {
	kvmArgs, kvmTimeout := linuxQEMUAccelConfig(KVMModeKVM)
	wantKVM := []string{"-machine", "virt,gic-version=host", "-accel", "kvm", "-cpu", "host"}
	if !slices.Equal(kvmArgs, wantKVM) || kvmTimeout != readyTimeout {
		t.Fatalf("KVM config = %v, %v; want %v, %v", kvmArgs, kvmTimeout, wantKVM, readyTimeout)
	}
	for _, a := range kvmArgs {
		if strings.Contains(a, "virtualization=on") {
			t.Fatalf("KVM args unexpectedly enable nested EL2: %v", kvmArgs)
		}
	}

	tcgArgs, tcgTimeout := linuxQEMUAccelConfig(KVMModeTCG)
	wantTCG := []string{"-machine", "virt,gic-version=3", "-accel", "tcg,thread=multi", "-cpu", "max"}
	if !slices.Equal(tcgArgs, wantTCG) || tcgTimeout != 120*time.Second {
		t.Fatalf("TCG config = %v, %v; want %v, 120s", tcgArgs, tcgTimeout, wantTCG)
	}
}

func TestQEMUBootArgsIncludesEmptyRomfile(t *testing.T) {
	launch := &qemuLaunch{
		machineArgs: []string{"-machine", "virt,gic-version=host", "-accel", "kvm", "-cpu", "host"},
	}
	args := qemuBootArgs(launch, &Info{CPUs: 2, MemoryMB: 1024}, "/img", "user,id=net0", nil)
	if !slices.Contains(args, "virtio-net-pci,netdev=net0,romfile=") {
		t.Fatalf("QEMU args missing romfile= on virtio-net-pci: %v", args)
	}
}

func TestReadCPUInfosFromSysfsAndFallback(t *testing.T) {
	sysCPU := t.TempDir()
	for i := 0; i < 4; i++ {
		dir := filepath.Join(sysCPU, fmt.Sprintf("cpu%d", i))
		if err := os.MkdirAll(filepath.Join(dir, "regs", "identification"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "cpufreq"), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "regs", "identification", "midr_el1"), []byte("0x00000000412fd050\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "cpu_capacity"), []byte("424\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "cpufreq", "cpuinfo_max_freq"), []byte("1800000\n"), 0o644)
	}
	for i := 4; i < 8; i++ {
		dir := filepath.Join(sysCPU, fmt.Sprintf("cpu%d", i))
		if err := os.MkdirAll(filepath.Join(dir, "cpufreq"), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "cpu_capacity"), []byte("1024\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "cpufreq", "cpuinfo_max_freq"), []byte("2400000\n"), 0o644)
	}
	var cpuinfo strings.Builder
	for i := 4; i < 8; i++ {
		fmt.Fprintf(&cpuinfo, "processor\t: %d\nCPU implementer\t: 0x41\nCPU variant\t: 0x4\nCPU part\t: 0xd0b\nCPU revision\t: 0\n\n", i)
	}
	procFile := filepath.Join(t.TempDir(), "cpuinfo")
	if err := os.WriteFile(procFile, []byte(cpuinfo.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	infos := readCPUInfosFrom([]int{0, 1, 2, 3, 4, 5, 6, 7}, sysCPU, procFile)
	plan, err := SelectKVMAffinity(infos, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.PinCPUs, []int{4, 5, 6, 7}) || plan.Summary() != "4x Cortex-A76 + 4x Cortex-A55" {
		t.Fatalf("plan = %+v, summary = %q", plan, plan.Summary())
	}
}

func TestParseProcCPUInfoMIDR(t *testing.T) {
	sample := `processor	: 0
BogoMIPS	: 48.00
Features	: fp asimd evtstrm aes
CPU implementer	: 0x41
CPU architecture: 8
CPU variant	: 0x2
CPU part	: 0xd05
CPU revision	: 0

processor	: 4
BogoMIPS	: 48.00
CPU implementer	: 0x41
CPU architecture: 8
CPU variant	: 0x4
CPU part	: 0xd0b
CPU revision	: 0`
	got := parseProcCPUInfoMIDR(sample)
	if got[0] != midrCortexA55 {
		t.Fatalf("cpu 0 MIDR = %#08x, want %#08x", got[0], midrCortexA55)
	}
	if got[4] != midrCortexA76 {
		t.Fatalf("cpu 4 MIDR = %#08x, want %#08x", got[4], midrCortexA76)
	}
	if len(got) != 2 {
		t.Fatalf("got %d CPUs, want 2: %v", len(got), got)
	}
}

func TestParseAndFormatCPUList(t *testing.T) {
	cases := []struct {
		spec    string
		want    []int
		fmtWant string
		wantErr bool
	}{
		{"4-7", []int{4, 5, 6, 7}, "4-7", false},
		{"0,1,2,3", []int{0, 1, 2, 3}, "0-3", false},
		{"6,4-5,4,0", []int{0, 4, 5, 6}, "0,4-6", false},
		{"4", []int{4}, "4", false},
		{"65535", []int{65535}, "65535", false},
		{"65535-65535", []int{65535}, "65535", false},
		{"", nil, "", true},
		{"7-4", nil, "", true},
		{"1,,2", nil, "", true},
		{"-1", nil, "", true},
		{"abc", nil, "", true},
		{"0-999999", nil, "", true},
		{"65536", nil, "", true},
		{"65535-65536", nil, "", true},
		{"9223372036854775807-9223372036854775807", nil, "", true},
	}
	for _, tc := range cases {
		got, err := ParseCPUList(tc.spec)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseCPUList(%q) = %v, want error", tc.spec, got)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("ParseCPUList(%q) = %v, %v; want %v", tc.spec, got, err, tc.want)
			continue
		}
		if f := FormatCPUList(got); f != tc.fmtWant {
			t.Errorf("FormatCPUList(%v) = %q, want %q", got, f, tc.fmtWant)
		}
	}
}

func TestSelectKVMAffinityRK3588(t *testing.T) {
	plan, err := SelectKVMAffinity(rk3588CPUs(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Heterogeneous || plan.Manual {
		t.Fatalf("unexpected flags: heterogeneous=%v manual=%v", plan.Heterogeneous, plan.Manual)
	}
	if !slices.Equal(plan.PinCPUs, []int{4, 5, 6, 7}) {
		t.Fatalf("PinCPUs = %v, want [4 5 6 7]", plan.PinCPUs)
	}
	if plan.Selected.Name != "Cortex-A76" || plan.MaxCPUs != 4 {
		t.Fatalf("Selected = %+v, MaxCPUs = %d", plan.Selected, plan.MaxCPUs)
	}
	if got := FormatCPUClusters(plan.Clusters); got != "4x Cortex-A76 [4-7], 4x Cortex-A55 [0-3]" {
		t.Fatalf("FormatCPUClusters = %q", got)
	}
	if got := plan.Summary(); got != "4x Cortex-A76 + 4x Cortex-A55" {
		t.Fatalf("Summary = %q", got)
	}
}

func TestSelectKVMAffinityRankingFallbacks(t *testing.T) {
	t.Run("freq tiebreak when capacity missing", func(t *testing.T) {
		cpus := rk3588CPUs()
		for i := range cpus {
			cpus[i].Capacity = 0
		}
		plan, err := SelectKVMAffinity(cpus, "")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(plan.PinCPUs, []int{4, 5, 6, 7}) {
			t.Fatalf("PinCPUs = %v, want [4 5 6 7]", plan.PinCPUs)
		}
	})

	t.Run("part tier tiebreak when capacity and freq missing", func(t *testing.T) {
		rk3399 := []CPUInfo{
			{ID: 0, MIDR: midrCortexA53, HasMIDR: true},
			{ID: 1, MIDR: midrCortexA53, HasMIDR: true},
			{ID: 2, MIDR: midrCortexA53, HasMIDR: true},
			{ID: 3, MIDR: midrCortexA53, HasMIDR: true},
			{ID: 4, MIDR: midrCortexA72, HasMIDR: true},
			{ID: 5, MIDR: midrCortexA72, HasMIDR: true},
		}
		plan, err := SelectKVMAffinity(rk3399, "")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(plan.PinCPUs, []int{4, 5}) || plan.Selected.Name != "Cortex-A72" {
			t.Fatalf("PinCPUs = %v, Selected = %+v", plan.PinCPUs, plan.Selected)
		}
	})

	t.Run("prime X tier beats A7x when capacity and freq missing", func(t *testing.T) {
		cpus := []CPUInfo{
			{ID: 0, MIDR: midrCortexA76, HasMIDR: true},
			{ID: 1, MIDR: midrCortexA76, HasMIDR: true},
			{ID: 2, MIDR: midrCortexX3, HasMIDR: true},
		}
		plan, err := SelectKVMAffinity(cpus, "")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(plan.PinCPUs, []int{2}) || plan.Selected.Name != "Cortex-X3" {
			t.Fatalf("PinCPUs = %v, Selected = %+v", plan.PinCPUs, plan.Selected)
		}
	})

	t.Run("core count tiebreak for unknown MIDRs", func(t *testing.T) {
		cpus := []CPUInfo{
			{ID: 0, MIDR: 0x51000001, HasMIDR: true},
			{ID: 1, MIDR: 0x51000001, HasMIDR: true},
			{ID: 2, MIDR: 0x51000002, HasMIDR: true},
			{ID: 3, MIDR: 0x51000002, HasMIDR: true},
			{ID: 4, MIDR: 0x51000002, HasMIDR: true},
		}
		plan, err := SelectKVMAffinity(cpus, "")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(plan.PinCPUs, []int{2, 3, 4}) {
			t.Fatalf("PinCPUs = %v, want [2 3 4]", plan.PinCPUs)
		}
	})

	t.Run("homogeneous host skips pinning", func(t *testing.T) {
		cpus := []CPUInfo{
			{ID: 0, MIDR: midrNeoverseN1, HasMIDR: true},
			{ID: 1, MIDR: midrNeoverseN1, HasMIDR: true},
			{ID: 2, MIDR: midrNeoverseN1, HasMIDR: true},
			{ID: 3, MIDR: midrNeoverseN1, HasMIDR: true},
		}
		plan, err := SelectKVMAffinity(cpus, "")
		if err != nil {
			t.Fatal(err)
		}
		if plan.Heterogeneous || plan.PinCPUs != nil || plan.MaxCPUs != 4 || plan.Summary() != "4x Neoverse-N1" {
			t.Fatalf("homogeneous plan = %+v", plan)
		}
	})

	t.Run("caller taskset pre-pinned to little cluster stays unpinned within allowed", func(t *testing.T) {
		plan, err := SelectKVMAffinity(rk3588CPUs()[:4], "")
		if err != nil {
			t.Fatal(err)
		}
		if plan.Heterogeneous || plan.PinCPUs != nil || plan.MaxCPUs != 4 || plan.Selected.Name != "Cortex-A55" {
			t.Fatalf("pre-pinned plan = %+v", plan)
		}
	})

	t.Run("missing MIDR stays homogeneous", func(t *testing.T) {
		cpus := []CPUInfo{{ID: 0}, {ID: 1}}
		plan, err := SelectKVMAffinity(cpus, "")
		if err != nil {
			t.Fatal(err)
		}
		if plan.Heterogeneous || plan.PinCPUs != nil || plan.MaxCPUs != 2 || plan.Summary() != "2 CPUs" {
			t.Fatalf("unknown MIDR plan = %+v", plan)
		}
	})
}

func TestSelectKVMAffinityOverrides(t *testing.T) {
	t.Run("valid little cluster override with whitespace", func(t *testing.T) {
		plan, err := SelectKVMAffinity(rk3588CPUs(), " 0-3 ")
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Manual || !slices.Equal(plan.PinCPUs, []int{0, 1, 2, 3}) || plan.Selected.Name != "Cortex-A55" || plan.MaxCPUs != 4 {
			t.Fatalf("override plan = %+v", plan)
		}
	})

	t.Run("valid subset of big cluster", func(t *testing.T) {
		plan, err := SelectKVMAffinity(rk3588CPUs(), "4,5")
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Manual || !slices.Equal(plan.PinCPUs, []int{4, 5}) || plan.MaxCPUs != 2 {
			t.Fatalf("subset plan = %+v", plan)
		}
	})

	t.Run("mixed clusters rejected and preserves topology", func(t *testing.T) {
		plan, err := SelectKVMAffinity(rk3588CPUs(), "3-4")
		if err == nil || !strings.Contains(err.Error(), "multiple CPU types") {
			t.Fatalf("mixed override err = %v, want multiple CPU types error", err)
		}
		if len(plan.Clusters) != 2 || plan.Summary() != "4x Cortex-A76 + 4x Cortex-A55" {
			t.Fatalf("expected topology preserved on error, got %+v", plan)
		}
	})

	t.Run("outside allowed cpuset rejected", func(t *testing.T) {
		_, err := SelectKVMAffinity(rk3588CPUs()[:4], "4-7")
		if err == nil || !strings.Contains(err.Error(), "not in this process's allowed CPUs") {
			t.Fatalf("out-of-allowed override err = %v", err)
		}
	})

	t.Run("homogeneous host rejects invalid override", func(t *testing.T) {
		homogeneous := []CPUInfo{
			{ID: 0, MIDR: midrNeoverseN1, HasMIDR: true},
			{ID: 1, MIDR: midrNeoverseN1, HasMIDR: true},
			{ID: 2, MIDR: midrNeoverseN1, HasMIDR: true},
			{ID: 3, MIDR: midrNeoverseN1, HasMIDR: true},
		}
		plan, err := SelectKVMAffinity(homogeneous, "99")
		if err == nil || !strings.Contains(err.Error(), "not in this process's allowed CPUs") {
			t.Fatalf("homogeneous invalid override err = %v", err)
		}
		if plan.Summary() != "4x Neoverse-N1" {
			t.Fatalf("homogeneous summary on error = %q", plan.Summary())
		}
		if _, err := SelectKVMAffinity(homogeneous, "bad"); err == nil {
			t.Fatal("accepted malformed override on homogeneous host")
		}
		plan, err = SelectKVMAffinity(homogeneous, "   ")
		if err != nil || plan.Manual || plan.PinCPUs != nil {
			t.Fatalf("whitespace override = %+v, %v", plan, err)
		}
	})

	t.Run("malformed list rejected", func(t *testing.T) {
		if _, err := SelectKVMAffinity(rk3588CPUs(), "7-4"); err == nil {
			t.Fatal("accepted inverted range")
		}
	})
}

func TestResolvePlanCPUs(t *testing.T) {
	rkPlan, err := SelectKVMAffinity(rk3588CPUs(), "")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := resolvePlanCPUs(0, rkPlan); err != nil || n != 2 {
		t.Fatalf("default RK3588 cpus = %d, %v; want 2", n, err)
	}
	if n, err := resolvePlanCPUs(4, rkPlan); err != nil || n != 4 {
		t.Fatalf("max RK3588 cpus = %d, %v; want 4", n, err)
	}
	if _, err := resolvePlanCPUs(5, rkPlan); err == nil || !strings.Contains(err.Error(), "heterogeneous") || !strings.Contains(err.Error(), "Cortex-A76") {
		t.Fatalf("excess RK3588 cpus err = %v", err)
	}

	oneCorePlan, err := SelectKVMAffinity(rk3588CPUs(), "4")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := resolvePlanCPUs(0, oneCorePlan); err != nil || n != 1 {
		t.Fatalf("default 1-core plan cpus = %d, %v; want 1", n, err)
	}
	if _, err := resolvePlanCPUs(2, oneCorePlan); err == nil || !strings.Contains(err.Error(), "KIDO_KVM_CPUS") {
		t.Fatalf("excess manual cpus err = %v", err)
	}
	if _, err := resolvePlanCPUs(-1, rkPlan); err == nil {
		t.Fatal("accepted negative --cpus")
	}
}
