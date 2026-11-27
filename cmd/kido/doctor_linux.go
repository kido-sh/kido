package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/kido-sh/kido/internal/runtime"
	"github.com/kido-sh/kido/internal/setup"
)

const doctorSummary = "Check that this Linux host can run kido containers"

func doctorUsage() string {
	return "\n\nChecks the Linux arm64 host, KVM, QEMU, KIDO_HOME, disk space, the local images and the\n" +
		"registry credentials, and prints a fix for each problem. Exit status 1 when a check fails."
}

func doctorPlatformChecks(ctx context.Context, home, backend string, berr error) []checkResult {
	mode, merr := runtime.KVMModeFromEnv()
	kvm := runtime.ProbeLinuxKVM()
	results := []checkResult{
		evaluateArchitecture(goruntime.GOARCH, kvm, mode, merr),
		checkLinux(),
		checkKVM(kvm, mode, merr),
		checkBackend(backend, berr, mode, merr),
	}
	if berr == nil && backend == "qemu" {
		results = append(results, checkQEMU(ctx, home, mode, merr)...)
	}
	return results
}

func diskMountLabel(dir string, _ *unix.Statfs_t) string {
	return dir
}

func evaluateArchitecture(goarch string, kvm runtime.KVMDiag, mode string, modeErr error) checkResult {
	const name = "Architecture"
	if goarch != "arm64" {
		return fail(name, goarch+": kido runs arm64 guests with KVM and needs an arm64 Linux host",
			"run kido on an arm64 (aarch64) Linux host")
	}
	tcg := modeErr == nil && mode == runtime.KVMModeTCG
	summary := kvm.Plan.Summary()
	if kvm.PlanErr != nil {
		detail := fmt.Sprintf("arm64 (%v)", kvm.PlanErr)
		if summary != "" {
			detail = fmt.Sprintf("arm64 (%s, %v)", summary, kvm.PlanErr)
		}
		fix := "check process CPU affinity and KIDO_KVM_CPUS"
		if len(kvm.Plan.Clusters) > 0 {
			fix = "unset KIDO_KVM_CPUS or set it to CPUs from one cluster, such as " + runtime.FormatCPUList(kvm.Plan.Clusters[0].CPUs)
		}
		if tcg {
			return warn(name, detail+", ignored with KIDO_ACCEL=tcg", fix)
		}
		return fail(name, detail, fix)
	}
	if kvm.Plan.Manual {
		if tcg {
			return pass(name, fmt.Sprintf("arm64 (%s, KIDO_KVM_CPUS=%s ignored under TCG)", summary, runtime.FormatCPUList(kvm.Plan.PinCPUs)))
		}
		return pass(name, fmt.Sprintf("arm64 (%s, KVM pins to CPUs %s via KIDO_KVM_CPUS)", summary, runtime.FormatCPUList(kvm.Plan.PinCPUs)))
	}
	if kvm.Plan.Heterogeneous {
		if tcg {
			return pass(name, fmt.Sprintf("arm64 (%s, no KVM pinning under TCG)", summary))
		}
		return pass(name, fmt.Sprintf("arm64 (%s, KVM pins to CPUs %s)", summary, runtime.FormatCPUList(kvm.Plan.PinCPUs)))
	}
	return pass(name, fmt.Sprintf("arm64 (%s)", summary))
}

func checkLinux() checkResult {
	const name = "Linux"
	osName := linuxOSName()
	if rel := linuxKernelRelease(); rel != "" {
		return pass(name, fmt.Sprintf("%s (kernel %s)", osName, rel))
	}
	return pass(name, osName)
}

func checkKVM(kvm runtime.KVMDiag, mode string, modeErr error) checkResult {
	const name = "KVM"
	tcg := modeErr == nil && mode == runtime.KVMModeTCG
	if kvm.Available {
		return pass(name, fmt.Sprintf("/dev/kvm is accessible (API v%d)", kvm.APIVersion))
	}
	switch {
	case errors.Is(kvm.Err, unix.EACCES), errors.Is(kvm.Err, unix.EPERM):
		fix := "run `sudo usermod -aG kvm $USER` and log in again, or set KIDO_ACCEL=tcg for software emulation"
		if tcg {
			return warn(name, "/dev/kvm: permission denied (KIDO_ACCEL=tcg selected)", fix)
		}
		return fail(name, "/dev/kvm: permission denied", fix)
	case errors.Is(kvm.Err, unix.ENOENT), errors.Is(kvm.Err, unix.ENODEV):
		fix := "enable KVM in the kernel or firmware and load the kvm module, or set KIDO_ACCEL=tcg for software emulation"
		if tcg {
			return warn(name, "/dev/kvm is not available (KIDO_ACCEL=tcg selected)", fix)
		}
		return fail(name, "/dev/kvm is not available", fix)
	case kvm.APIVersion != 0:
		fix := "kido requires Linux KVM API version 12, or set KIDO_ACCEL=tcg for software emulation"
		if tcg {
			return warn(name, fmt.Sprintf("/dev/kvm reports API version %d, want 12 (KIDO_ACCEL=tcg selected)", kvm.APIVersion), fix)
		}
		return fail(name, fmt.Sprintf("/dev/kvm reports API version %d, want 12", kvm.APIVersion), "kido requires Linux KVM API version 12")
	default:
		msg := "/dev/kvm is not available"
		if kvm.Err != nil {
			msg = kvm.Err.Error()
		}
		if tcg {
			return warn(name, msg+" (KIDO_ACCEL=tcg selected)", "check /dev/kvm permissions and kernel KVM support")
		}
		return fail(name, msg, "check /dev/kvm permissions and kernel KVM support")
	}
}

func checkBackend(backend string, err error, mode string, modeErr error) checkResult {
	const name = "Backend"
	if err != nil {
		return fail(name, err.Error(), "unset KIDO_BACKEND or set KIDO_BACKEND=qemu (vz is macOS-only)")
	}
	if backend == "vz" {
		return fail(name, "vz (Virtualization.framework) is macOS-only", "unset KIDO_BACKEND or set KIDO_BACKEND=qemu")
	}
	if modeErr != nil {
		return fail(name, modeErr.Error(), "unset KIDO_ACCEL or set it to auto, kvm, or tcg")
	}
	if mode == runtime.KVMModeTCG {
		return pass(name, "qemu: QEMU with TCG (KIDO_ACCEL=tcg)")
	}
	return pass(name, "qemu: QEMU with KVM")
}

func checkQEMU(ctx context.Context, home, mode string, modeErr error) []checkResult {
	const name = "QEMU"
	setupFix := "install qemu-system-aarch64 and qemu-img, such as `sudo apt install qemu-system-arm qemu-utils`, or set KIDO_QEMU to its absolute path"
	q, err := setup.FindQEMU(home)
	if errors.Is(err, setup.ErrNoQEMU) {
		return []checkResult{fail(name, "not found: KIDO_QEMU is unset and qemu-system-aarch64 is not on PATH", setupFix)}
	}
	if err != nil {
		return []checkResult{fail(name, err.Error(), setupFix)}
	}
	source := q.Source
	if v := setup.CurrentVersion(home); q.Source == setup.SourceSetup && v != "" {
		source += " " + v
	}
	if err := isExecutableFile(q.Path); err != nil {
		return []checkResult{fail(name, fmt.Sprintf("%s (%s): %v", q.Path, source, err), setupFix)}
	}
	var results []checkResult
	if out, err := runFixed(ctx, nil, q.Path, "--version"); err != nil {
		results = append(results, fail(name, fmt.Sprintf("%s (%s) does not run: %v", q.Path, source, err), setupFix))
	} else {
		results = append(results, pass(name, firstLine(out)+": "+q.Path+" ("+source+")"))
	}

	const accelName = "QEMU accel"
	tcg := modeErr == nil && mode == runtime.KVMModeTCG
	if out, err := runFixed(ctx, nil, q.Path, "-accel", "help"); err != nil {
		results = append(results, warn(accelName, fmt.Sprintf("%s -accel help failed: %v", filepath.Base(q.Path), err), ""))
	} else if accelHelpHasKVM(out) {
		results = append(results, pass(accelName, "kvm accelerator supported"))
	} else if tcg {
		results = append(results, warn(accelName, "QEMU does not list kvm in `-accel help` (KIDO_ACCEL=tcg selected)",
			"install a QEMU build with KVM support"))
	} else {
		results = append(results, fail(accelName, "QEMU does not list kvm in `-accel help`",
			"install a QEMU build with KVM support, such as `sudo apt install qemu-system-arm`"))
	}

	const imgName = "qemu-img"
	imgFix := "install qemu-img, such as `sudo apt install qemu-utils`"
	if err := isExecutableFile(q.Img); err != nil {
		return append(results, fail(imgName, fmt.Sprintf("%s: %v", q.Img, err), imgFix))
	}
	if out, err := runFixed(ctx, nil, q.Img, "--version"); err != nil {
		results = append(results, fail(imgName, fmt.Sprintf("%s does not run: %v", q.Img, err), imgFix))
	} else {
		results = append(results, pass(imgName, firstLine(out)))
	}
	return results
}

func accelHelpHasKVM(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "kvm" {
			return true
		}
	}
	return false
}
