package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kido-sh/kido/internal/imageref"
	"github.com/kido-sh/kido/internal/setup"
)

const (
	minFreeBytes  = 20e9
	minMacOSMajor = 15
	// macOS sun_path is 104 bytes with the NUL; the longest socket is containers/<id>/monitor.sock.
	maxHomeLen = 103 - len("/containers/0123456789ab/monitor.sock")
)

type checkStatus int

const (
	checkPass checkStatus = iota
	checkWarn
	checkFail
)

func (s checkStatus) String() string {
	return [...]string{"PASS", "WARN", "FAIL"}[s]
}

type checkResult struct {
	name   string
	status checkStatus
	detail string
	fix    string
}

func pass(name, detail string) checkResult { return checkResult{name, checkPass, detail, ""} }

func warn(name, detail, fix string) checkResult { return checkResult{name, checkWarn, detail, fix} }

func fail(name, detail, fix string) checkResult { return checkResult{name, checkFail, detail, fix} }

func init() {
	register("doctor", "Check that this Mac can run kido containers", runDoctor)
}

func runDoctor(ctx context.Context, args []string) int {
	fl := ctrFlags("doctor", "\n\nChecks the Mac, QEMU, KIDO_HOME, disk space, the local images and that nothing needs\n"+
		"Docker, and prints a fix for each problem. Exit status 1 when a check fails.")
	if err := fl.Parse(args); err != nil {
		return 125
	}
	if fl.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "kido: \"doctor\" accepts no arguments.\nSee 'kido doctor --help'.")
		return 125
	}

	home := doctorHome()
	results := []checkResult{checkAppleSilicon(), checkMacOS(ctx), checkHypervisor(), checkPriority(ctx)}
	results = append(results, checkQEMU(ctx, home)...)
	results = append(results, checkHome(home), checkDiskSpace(home), checkImages(home), checkDocker())

	fmt.Println("kido", version)
	failed, warned := 0, 0
	for _, r := range results {
		fmt.Printf("%s  %-17s %s\n", r.status, r.name, r.detail)
		if r.fix != "" {
			fmt.Printf("      %-17s fix: %s\n", "", r.fix)
		}
		switch r.status {
		case checkFail:
			failed++
		case checkWarn:
			warned++
		}
	}
	fmt.Printf("\n%d passed, %d warned, %d failed\n", len(results)-failed-warned, warned, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// checkPriority warns when kido runs under a utility or background QoS clamp, which QEMU inherits:
// macOS then wakes idle vCPUs tens of milliseconds late, so every timer in the guest fires late.
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

// doctorHome mirrors runtime.DefaultHome without creating anything.
func doctorHome() string {
	home := os.Getenv("KIDO_HOME")
	if home == "" {
		if uh, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(uh, ".kido")
		} else {
			home = ".kido"
		}
	}
	if abs, err := filepath.Abs(home); err == nil {
		return abs
	}
	return home
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

func checkMacOS(ctx context.Context) checkResult {
	const name = "macOS"
	// An empty environment: SYSTEM_VERSION_COMPAT=1 would make sw_vers report a compatibility version.
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
		return warn(name, detail+": older than macOS 15, untested",
			"update to macOS 15 or later; QEMU's HVF backend uses the in-kernel GIC that macOS 15 added")
	}
	return pass(name, detail)
}

func checkHypervisor() checkResult {
	const name = "Hypervisor"
	if v, err := unix.SysctlUint32("kern.hv_support"); err != nil || v != 1 {
		return fail(name, "Hypervisor.framework is not available (kern.hv_support is not 1)",
			"run kido on the Mac itself; most virtual machines do not offer Hypervisor.framework")
	}
	return pass(name, "Hypervisor.framework is available")
}

// checkQEMU covers the QEMU that the runtime uses (setup.FindQEMU: $KIDO_QEMU, then the one kido setup
// installed, then the development default): the binary, its libraries, its entitlement and the
// qemu-img next to it.
func checkQEMU(ctx context.Context, home string) []checkResult {
	const name = "QEMU"
	setupFix := fmt.Sprintf("run `kido setup` (installs QEMU %s into %s), or set KIDO_QEMU to the absolute path of a qemu-system-aarch64 signed for HVF",
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
		signFix = "kido setup --force"
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
		imgFix = "kido setup --force"
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

// checkQEMULibraries reports dylibs that QEMU or qemu-img load but dyld would not find, and
// libraries they take from outside their own directory tree (e.g. Homebrew's).
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

func checkHome(home string) checkResult {
	const name = "KIDO_HOME"
	fi, err := os.Stat(home)
	if errors.Is(err, fs.ErrNotExist) {
		return warn(name, home+" does not exist yet", "kido creates it on first use; or: mkdir -m 700 "+home)
	}
	if err != nil {
		return fail(name, err.Error(), "")
	}
	if !fi.IsDir() {
		return fail(name, home+" is not a directory", "set KIDO_HOME to a directory")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fail(name, fmt.Sprintf("%s belongs to uid %d, not to you", home, st.Uid), "set KIDO_HOME to a directory you own")
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fail(name, fmt.Sprintf("%s has mode %04o; other users can read container state", home, perm), "chmod 700 "+home)
	}
	if len(home) > maxHomeLen {
		return warn(name, fmt.Sprintf("%s is %d characters: kido commit of a running QEMU container needs at most %d (the path of its monitor socket); everything else works",
			home, len(home), maxHomeLen), "stop a container before committing it, or set KIDO_HOME to a shorter path")
	}
	return pass(name, home)
}

func checkDiskSpace(home string) checkResult {
	const name = "Disk space"
	dir := home
	for {
		if _, err := os.Stat(dir); err == nil || filepath.Dir(dir) == dir {
			break
		}
		dir = filepath.Dir(dir)
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return warn(name, "statfs "+dir+": "+err.Error(), "")
	}
	free := float64(st.Bavail) * float64(st.Bsize)
	detail := fmt.Sprintf("%.1f GB free on %s", free/1e9, unix.ByteSliceToString(st.Mntonname[:]))
	if free < minFreeBytes {
		return warn(name, detail+", under 20 GB",
			"free some space; an image takes about 1 GB and every container's disk grows with what it writes")
	}
	return pass(name, detail)
}

func checkImages(home string) checkResult {
	const name = "Images"
	dir := filepath.Join(home, "images")
	fix := "pull one: kido setup --image REMOTE_REF; or import one: kido image import DIR darwin"
	manifests, _ := filepath.Glob(filepath.Join(dir, "*", "*", "image.json"))
	var refs []string
	for _, m := range manifests {
		tagDir := filepath.Dir(m)
		if ref, err := imageref.FromDir(filepath.Base(filepath.Dir(tagDir)), filepath.Base(tagDir)); err == nil {
			refs = append(refs, ref.String())
		}
	}
	if len(refs) == 0 {
		return fail(name, "no image under "+dir, fix)
	}
	detail := strings.Join(refs, ", ")
	if len(refs) > 5 {
		detail = strings.Join(refs[:5], ", ") + fmt.Sprintf(" and %d more", len(refs)-5)
	}
	return pass(name, detail)
}

func isExecutableFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errors.New("not found")
		}
		return err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return errors.New("not an executable file")
	}
	return nil
}

// runFixed runs a program with fixed arguments, never user input, and returns its output. A nil
// env inherits kido's environment.
func runFixed(ctx context.Context, env []string, program string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := firstLine(string(out)); msg != "" {
			return "", fmt.Errorf("%v: %s", err, msg)
		}
		return "", err
	}
	return string(out), nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

// checkDocker confirms that kido runs without Docker, and flags a Docker credential helper that kido's
// registry commands would call (they read Docker's config.json) but cannot find.
func checkDocker() checkResult {
	const name = "Docker-free"
	detail := "no Docker daemon or VM needed: containers run on QEMU, images live in KIDO_HOME"
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		uh, err := os.UserHomeDir()
		if err != nil {
			return pass(name, detail)
		}
		dir = filepath.Join(uh, ".docker")
	}
	cfgPath := filepath.Join(dir, "config.json")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return pass(name, detail)
	}
	var cfg struct {
		CredsStore  string            `json:"credsStore"`
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return warn(name, detail+"; "+cfgPath+" is not valid JSON",
			"kido pull, push and login read registry credentials from it: repair it or move it away")
	}
	helpers := []string{cfg.CredsStore}
	for _, h := range cfg.CredHelpers {
		helpers = append(helpers, h)
	}
	slices.Sort(helpers)
	var found, missing []string
	for _, h := range slices.Compact(helpers) {
		if h == "" || strings.ContainsRune(h, '/') {
			continue
		}
		if _, err := exec.LookPath("docker-credential-" + h); err != nil {
			missing = append(missing, "docker-credential-"+h)
		} else {
			found = append(found, "docker-credential-"+h)
		}
	}
	if len(missing) > 0 {
		return warn(name, detail+"; "+cfgPath+" names "+strings.Join(missing, ", ")+", which is not on PATH",
			"registry pulls and pushes go anonymous: put the helper on PATH (Docker Desktop ships it in ~/.docker/bin), "+
				"or drop credsStore/credHelpers from config.json and run `kido login SERVER`")
	}
	if len(found) > 0 {
		return pass(name, detail+"; registry credentials come from "+strings.Join(found, ", "))
	}
	return pass(name, detail)
}
