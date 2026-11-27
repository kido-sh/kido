package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/kido-sh/kido/internal/runtime"
	"github.com/kido-sh/kido/internal/setup"
)

const (
	minMacOSMajor = 15
	doctorSummary = "Check that this Mac can run kido containers"
)

func doctorUsage() string {
	return "\n\nChecks the Mac, the VM backend, KIDO_HOME, disk space, the local images and the registry\n" +
		"credentials, and prints a fix for each problem. Exit status 1 when a check fails."
}

func doctorPlatformChecks(ctx context.Context, home, backend string, berr error) []checkResult {
	results := []checkResult{checkAppleSilicon(), checkMacOS(ctx, backend), checkHypervisor(), checkBackend(backend, berr)}
	switch backend {
	case "qemu":
		results = append(results, checkPriority(ctx))
		results = append(results, checkQEMU(ctx, home)...)
	case "vz":
		results = append(results, checkVMM())
	}
	return results
}

func diskMountLabel(_ string, st *unix.Statfs_t) string {
	return unix.ByteSliceToString(st.Mntonname[:])
}

func checkPriority(ctx context.Context) checkResult {
	const name = "Priority"
	out, err := runFixed(ctx, nil, "/bin/ps", "-o", "pri=", "-p", strconv.Itoa(os.Getpid()))
	if err != nil {
		return warn(name, "could not read the scheduling priority: "+err.Error(), "")
	}
	pri, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return warn(name, fmt.Sprintf("could not parse the scheduling priority %q", strings.TrimSpace(out)), "")
	}
	if pri <= 20 {
		return warn(name, fmt.Sprintf("priority %d: this shell runs under a utility or background QoS clamp, so QEMU guests wake late and their timers fire 20+ ms late", pri),
			"start kido from a normal terminal; a launchd job that starts your shell needs ProcessType=Interactive")
	}
	return pass(name, fmt.Sprintf("priority %d", pri))
}

func checkAppleSilicon() checkResult {
	const name = "Apple Silicon"
	if v, err := unix.SysctlUint32("hw.optional.arm64"); err != nil || v != 1 {
		return fail(name, "this Mac is not Apple Silicon (hw.optional.arm64 is not 1)",
			"kido runs arm64 guests with Hypervisor.framework and needs an Apple Silicon Mac")
	}
	chip, err := unix.Sysctl("machdep.cpu.brand_string")
	if err != nil || chip == "" {
		chip = "arm64"
	}
	if v, err := unix.SysctlUint32("sysctl.proc_translated"); err == nil && v == 1 {
		return fail(name, chip+", but kido itself runs under Rosetta", "install or build the darwin/arm64 kido")
	}
	return pass(name, chip)
}

func checkMacOS(ctx context.Context, backend string) checkResult {
	const name = "macOS"
	out, err := runFixed(ctx, []string{}, "/usr/bin/sw_vers")
	if err != nil {
		return fail(name, "sw_vers failed: "+err.Error(), "kido only runs on macOS")
	}
	var product, build string
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(k) {
		case "ProductVersion":
			product = strings.TrimSpace(v)
		case "BuildVersion":
			build = strings.TrimSpace(v)
		}
	}
	detail := product + " (" + build + ")"
	major, err := strconv.Atoi(strings.SplitN(product, ".", 2)[0])
	if err != nil {
		return warn(name, "cannot parse the version: "+strings.TrimSpace(out), "")
	}
	if major < minMacOSMajor {
		fix := "update to macOS 15 or later"
		if backend == "qemu" {
			fix += "; QEMU's HVF backend uses the in-kernel GIC that macOS 15 added"
		}
		return warn(name, detail+": older than macOS 15, untested", fix)
	}
	return pass(name, detail)
}

func checkBackend(backend string, err error) checkResult {
	const name = "Backend"
	if err != nil {
		return fail(name, err.Error(), "unset KIDO_BACKEND, or set it to vz or qemu")
	}
	if backend == "qemu" {
		return pass(name, "qemu: QEMU with Hypervisor.framework")
	}
	return pass(name, "vz: Virtualization.framework (KIDO_BACKEND=qemu selects QEMU)")
}

func checkVMM() checkResult {
	const name = "kido-vmm"
	p := runtime.VMMPath()
	if p == "" {
		return fail(name, "not found next to kido or in ../libexec/kido", "reinstall kido, or set KIDO_VMM to a kido-vmm")
	}
	switch ok, err := setup.HasEntitlement(p, setup.VirtualizationEntitlement); {
	case err != nil:
		return fail(name, p+": "+err.Error(), "reinstall kido, or set KIDO_VMM to a kido-vmm")
	case !ok:
		return fail(name, p+" lacks "+setup.VirtualizationEntitlement+", so Virtualization.framework refuses to start VMs",
			"reinstall kido, or sign it with `make sign`")
	}
	return pass(name, p)
}

func checkHypervisor() checkResult {
	const name = "Hypervisor"
	if v, err := unix.SysctlUint32("kern.hv_support"); err != nil || v != 1 {
		return fail(name, "Hypervisor.framework is not available (kern.hv_support is not 1)",
			"run kido on the Mac itself; most virtual machines do not offer Hypervisor.framework")
	}
	return pass(name, "Hypervisor.framework is available")
}

func checkQEMU(ctx context.Context, home string) []checkResult {
	const name = "QEMU"
	setupFix := fmt.Sprintf("run `KIDO_BACKEND=qemu kido setup` (installs QEMU %s into %s), or set KIDO_QEMU to the absolute path of a qemu-system-aarch64 signed for HVF",
		setup.PinnedQEMU.Version, setup.QEMUDir(home))
	q, err := setup.FindQEMU(home)
	if errors.Is(err, setup.ErrNoQEMU) {
		if r := checkUnpackedQEMU(home); r != nil {
			return r
		}
		return []checkResult{fail(name, "not found: KIDO_QEMU is unset and "+q.Path+" does not exist", setupFix)}
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
		results = append(results, fail(name, fmt.Sprintf("%s (%s) does not run: %v", q.Path, source, err),
			"see QEMU libraries below; a missing dylib is the usual cause"))
	} else {
		results = append(results, pass(name, firstLine(out)+": "+q.Path+" ("+source+")"))
	}
	results = append(results, checkQEMULibraries(q))

	const entName = "QEMU entitlement"
	signFix := "sign " + q.Path + " with the " + setup.HypervisorEntitlement + " entitlement (README: Troubleshooting)"
	if q.Source == setup.SourceSetup {
		signFix = "KIDO_BACKEND=qemu kido setup --force"
	}
	switch ok, err := setup.HasEntitlement(q.Path, setup.HypervisorEntitlement); {
	case err != nil:
		results = append(results, fail(entName, err.Error(), signFix))
	case !ok:
		results = append(results, fail(entName, "the signature lacks "+setup.HypervisorEntitlement+"; HVF refuses to start", signFix))
	default:
		results = append(results, pass(entName, setup.HypervisorEntitlement))
	}

	const imgName = "qemu-img"
	imgFix := "put qemu-img from the same QEMU build next to " + filepath.Base(q.Path)
	if q.Source == setup.SourceSetup {
		imgFix = "KIDO_BACKEND=qemu kido setup --force"
	}
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

func checkQEMULibraries(q setup.QEMU) checkResult {
	const name = "QEMU libraries"
	var missing, external []string
	for _, exe := range []string{q.Path, q.Img} {
		libs, err := setup.CheckLibraries(exe)
		if err != nil {
			if exe == q.Path {
				return warn(name, err.Error(), "")
			}
			continue
		}
		missing = append(missing, libs.Missing...)
		external = append(external, libs.External...)
	}
	slices.Sort(missing)
	slices.Sort(external)
	missing, external = slices.Compact(missing), slices.Compact(external)
	if len(missing) > 0 {
		fix := "point KIDO_QEMU at a QEMU that runs here, or wait for a self-contained QEMU release"
		if f := setup.HomebrewFormulas(missing); len(f) > 0 {
			fix = "brew install " + strings.Join(f, " ") + " (this QEMU build takes them from Homebrew), or point KIDO_QEMU at a self-contained QEMU"
		}
		return fail(name, fmt.Sprintf("%d missing: %s", len(missing), strings.Join(missing, ", ")), fix)
	}
	if len(external) > 0 {
		from := strings.Join(setup.HomebrewFormulas(external), ", ")
		if from == "" {
			from = strings.Join(external, ", ")
		} else {
			from = "Homebrew: " + from
		}
		return warn(name, fmt.Sprintf("not self-contained: %d libraries come from outside its directory (%s)", len(external), from),
			"nothing to do while they stay installed; QEMU stops starting if they are removed")
	}
	return pass(name, "self-contained: nothing outside its directory but system libraries")
}
