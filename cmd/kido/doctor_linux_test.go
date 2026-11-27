package main

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/kido-sh/kido/internal/runtime"
)

func testRK3588CPUs() []runtime.CPUInfo {
	return []runtime.CPUInfo{
		{ID: 0, MIDR: 0x412fd050, HasMIDR: true, Capacity: 424, MaxFreqKHz: 1800000},
		{ID: 1, MIDR: 0x412fd050, HasMIDR: true, Capacity: 424, MaxFreqKHz: 1800000},
		{ID: 2, MIDR: 0x412fd050, HasMIDR: true, Capacity: 424, MaxFreqKHz: 1800000},
		{ID: 3, MIDR: 0x412fd050, HasMIDR: true, Capacity: 424, MaxFreqKHz: 1800000},
		{ID: 4, MIDR: 0x414fd0b0, HasMIDR: true, Capacity: 1024, MaxFreqKHz: 2400000},
		{ID: 5, MIDR: 0x414fd0b0, HasMIDR: true, Capacity: 1024, MaxFreqKHz: 2400000},
		{ID: 6, MIDR: 0x414fd0b0, HasMIDR: true, Capacity: 1024, MaxFreqKHz: 2400000},
		{ID: 7, MIDR: 0x414fd0b0, HasMIDR: true, Capacity: 1024, MaxFreqKHz: 2400000},
	}
}

func testHomogeneousCPUs() []runtime.CPUInfo {
	return []runtime.CPUInfo{
		{ID: 0, MIDR: 0x413fd0c1, HasMIDR: true},
		{ID: 1, MIDR: 0x413fd0c1, HasMIDR: true},
		{ID: 2, MIDR: 0x413fd0c1, HasMIDR: true},
		{ID: 3, MIDR: 0x413fd0c1, HasMIDR: true},
	}
}

func diagFor(cpus []runtime.CPUInfo, override string) runtime.KVMDiag {
	plan, err := runtime.SelectKVMAffinity(cpus, override)
	return runtime.KVMDiag{Available: true, APIVersion: 12, Plan: plan, PlanErr: err}
}

func TestEvaluateArchitectureRK3588(t *testing.T) {
	r := evaluateArchitecture("arm64", diagFor(testRK3588CPUs(), ""), runtime.KVMModeKVM, nil)
	want := "arm64 (4x Cortex-A76 + 4x Cortex-A55, KVM pins to CPUs 4-7)"
	if r.status != checkPass || r.detail != want {
		t.Fatalf("got %v %q, want PASS %q", r.status, r.detail, want)
	}
	if strings.Contains(r.detail, ";") {
		t.Errorf("detail contains semicolon: %q", r.detail)
	}

	rOverride := evaluateArchitecture("arm64", diagFor(testRK3588CPUs(), " 0-3 "), runtime.KVMModeKVM, nil)
	if rOverride.status != checkPass || !strings.Contains(rOverride.detail, "CPUs 0-3 via KIDO_KVM_CPUS") {
		t.Fatalf("override 0-3: got %+v", rOverride)
	}

	rMixed := evaluateArchitecture("arm64", diagFor(testRK3588CPUs(), "3-4"), runtime.KVMModeKVM, nil)
	if rMixed.status != checkFail || !strings.Contains(rMixed.detail, "KIDO_KVM_CPUS") || !strings.Contains(rMixed.fix, "4-7") {
		t.Fatalf("mixed override under KVM: got %+v", rMixed)
	}
	if strings.Contains(rMixed.detail, ";") {
		t.Errorf("mixed detail contains semicolon: %q", rMixed.detail)
	}

	rMixedTCG := evaluateArchitecture("arm64", diagFor(testRK3588CPUs(), "3-4"), runtime.KVMModeTCG, nil)
	if rMixedTCG.status != checkWarn {
		t.Fatalf("mixed override under TCG: got %+v, want WARN", rMixedTCG)
	}

	rNonARM := evaluateArchitecture("amd64", diagFor(testRK3588CPUs(), ""), runtime.KVMModeKVM, nil)
	if rNonARM.status != checkFail {
		t.Fatalf("non-arm64: got %+v, want FAIL", rNonARM)
	}
}

func TestEvaluateArchitectureHomogeneous(t *testing.T) {
	r := evaluateArchitecture("arm64", diagFor(testHomogeneousCPUs(), ""), runtime.KVMModeKVM, nil)
	if r.status != checkPass || r.detail != "arm64 (4x Neoverse-N1)" {
		t.Fatalf("homogeneous default: got %+v", r)
	}

	rValid := evaluateArchitecture("arm64", diagFor(testHomogeneousCPUs(), " 0-1 "), runtime.KVMModeKVM, nil)
	if rValid.status != checkPass || !strings.Contains(rValid.detail, "CPUs 0-1 via KIDO_KVM_CPUS") {
		t.Fatalf("homogeneous valid override: got %+v", rValid)
	}

	rBad := evaluateArchitecture("arm64", diagFor(testHomogeneousCPUs(), "99"), runtime.KVMModeKVM, nil)
	if rBad.status != checkFail || !strings.Contains(rBad.detail, "KIDO_KVM_CPUS") {
		t.Fatalf("homogeneous invalid override: got %+v, want FAIL", rBad)
	}

	rBadTCG := evaluateArchitecture("arm64", diagFor(testHomogeneousCPUs(), "99"), runtime.KVMModeTCG, nil)
	if rBadTCG.status != checkWarn {
		t.Fatalf("homogeneous invalid override under TCG: got %+v, want WARN", rBadTCG)
	}
}

func TestCheckBackendAndKVMModes(t *testing.T) {
	mode, err := runtime.ParseKVMMode("hvf")
	if err == nil {
		t.Fatal("expected error for hvf")
	}
	if r := checkBackend("qemu", nil, mode, err); r.status != checkFail || !strings.Contains(r.detail, "KIDO_ACCEL") {
		t.Fatalf("invalid KIDO_ACCEL backend: got %+v, want FAIL", r)
	}

	tcgMode, tcgErr := runtime.ParseKVMMode(" tcg ")
	if tcgErr != nil || tcgMode != runtime.KVMModeTCG {
		t.Fatalf("ParseKVMMode( tcg ) = %q, %v", tcgMode, tcgErr)
	}
	if r := checkBackend("qemu", nil, tcgMode, tcgErr); r.status != checkPass || !strings.Contains(r.detail, "TCG") {
		t.Fatalf("whitespace tcg backend: got %+v", r)
	}

	denied := runtime.KVMDiag{Err: fmt.Errorf("cannot open /dev/kvm: %w", unix.EACCES)}
	if r := checkKVM(denied, runtime.KVMModeKVM, nil); r.status != checkFail {
		t.Fatalf("EACCES under KVM: got %+v, want FAIL", r)
	}
	if r := checkKVM(denied, tcgMode, tcgErr); r.status != checkWarn {
		t.Fatalf("EACCES under TCG: got %+v, want WARN", r)
	}
	if r := checkKVM(denied, mode, err); r.status != checkFail {
		t.Fatalf("EACCES under invalid KIDO_ACCEL: got %+v, want FAIL", r)
	}

	missing := runtime.KVMDiag{Err: fmt.Errorf("KVM is unavailable (/dev/kvm: %w)", unix.ENOENT)}
	if r := checkKVM(missing, runtime.KVMModeKVM, nil); r.status != checkFail {
		t.Fatalf("ENOENT under KVM: got %+v, want FAIL", r)
	}
	if r := checkKVM(missing, tcgMode, tcgErr); r.status != checkWarn {
		t.Fatalf("ENOENT under TCG: got %+v, want WARN", r)
	}

	badAPI := runtime.KVMDiag{APIVersion: 11, Err: fmt.Errorf("/dev/kvm API version 11, want 12")}
	if r := checkKVM(badAPI, runtime.KVMModeKVM, nil); r.status != checkFail {
		t.Fatalf("bad API under KVM: got %+v, want FAIL", r)
	}

	okDiag := runtime.KVMDiag{Available: true, APIVersion: 12}
	if r := checkKVM(okDiag, runtime.KVMModeKVM, nil); r.status != checkPass {
		t.Fatalf("available KVM: got %+v, want PASS", r)
	}
}

func TestProbeLinuxKVMParity(t *testing.T) {
	t.Setenv("KIDO_KVM_CPUS", "")
	t.Setenv("KIDO_ACCEL", "")
	diag := runtime.ProbeLinuxKVM()
	mode, merr := runtime.KVMModeFromEnv()
	if merr != nil || mode != runtime.KVMModeKVM {
		t.Fatalf("KVMModeFromEnv = %q, %v", mode, merr)
	}

	kvmRes := checkKVM(diag, mode, merr)
	if diag.Available && kvmRes.status != checkPass {
		t.Fatalf("ProbeLinuxKVM available but checkKVM = %+v", kvmRes)
	}
	if !diag.Available && kvmRes.status != checkFail {
		t.Fatalf("ProbeLinuxKVM unavailable but checkKVM = %+v", kvmRes)
	}

	archRes := evaluateArchitecture("arm64", diag, mode, merr)
	if diag.PlanErr == nil && archRes.status != checkPass {
		t.Fatalf("ProbeLinuxKVM plan ok but evaluateArchitecture = %+v", archRes)
	}
	if diag.PlanErr == nil && len(diag.Plan.PinCPUs) > 0 {
		wantPin := runtime.FormatCPUList(diag.Plan.PinCPUs)
		if !strings.Contains(archRes.detail, wantPin) {
			t.Fatalf("evaluateArchitecture detail %q missing pinned CPUs %q", archRes.detail, wantPin)
		}
	}

	t.Setenv("KIDO_KVM_CPUS", "invalid-spec")
	badDiag := runtime.ProbeLinuxKVM()
	if badDiag.PlanErr == nil {
		t.Fatal("ProbeLinuxKVM accepted invalid KIDO_KVM_CPUS")
	}
	if r := evaluateArchitecture("arm64", badDiag, mode, merr); r.status != checkFail {
		t.Fatalf("invalid KIDO_KVM_CPUS via ProbeLinuxKVM: got %+v, want FAIL", r)
	}
}

func TestLinuxSystemParsers(t *testing.T) {
	osr := "NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nVERSION_ID=\"24.04\"\n"
	if got := parseOSRelease(osr); got != "Ubuntu 24.04.1 LTS" {
		t.Errorf("parseOSRelease = %q", got)
	}
	mem := "MemTotal:       16384000 kB\nMemFree:         1234567 kB\n"
	if got := parseMemInfoTotal(mem); got != 16384000*1024 {
		t.Errorf("parseMemInfoTotal = %d", got)
	}
	if !accelHelpHasKVM("Accelerators supported in QEMU binary:\ntcg\nkvm\n") {
		t.Error("expected kvm in accel help")
	}
	if accelHelpHasKVM("Accelerators supported in QEMU binary:\ntcg\n") {
		t.Error("unexpected kvm in accel help")
	}
}
