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
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kido-sh/kido/internal/imageref"
	"github.com/kido-sh/kido/internal/runtime"
)

const (
	minFreeBytes = 20e9
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
	register("doctor", doctorSummary, runDoctor)
}

func runDoctor(ctx context.Context, args []string) int {
	fl := ctrFlags("doctor", doctorUsage())
	if err := fl.Parse(args); err != nil {
		return 125
	}
	if fl.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "kido: \"doctor\" accepts no arguments.\nSee 'kido doctor --help'.")
		return 125
	}

	home := doctorHome()
	backend, berr := runtime.BackendFromEnv()
	results := doctorPlatformChecks(ctx, home, backend, berr)
	results = append(results, checkHome(home, backend), checkDiskSpace(home), checkImages(home), checkCredentials())

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

func checkHome(home, backend string) checkResult {
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
	if backend == "qemu" && len(home) > maxHomeLen {
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
	detail := fmt.Sprintf("%.1f GB free on %s", free/1e9, diskMountLabel(dir, &st))
	if free < minFreeBytes {
		return warn(name, detail+", under 20 GB",
			"free some space; an image takes about 1 GB and every container's disk grows with what it writes")
	}
	return pass(name, detail)
}

func checkImages(home string) checkResult {
	const name = "Images"
	dir := filepath.Join(home, "images")
	fix := "run `kido setup`, which pulls " + defaultBaseImage + ", or import one: kido image import DIR darwin"
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

// checkCredentials flags a credential helper that config.json names, which kido's registry commands
// would call but cannot find.
func checkCredentials() checkResult {
	const name = "Credentials"
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		uh, err := os.UserHomeDir()
		if err != nil {
			return pass(name, "no home directory, so no stored registry credentials")
		}
		dir = filepath.Join(uh, ".docker")
	}
	cfgPath := filepath.Join(dir, "config.json")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return pass(name, "none stored yet; kido login stores them in "+cfgPath)
	}
	var cfg struct {
		CredsStore  string            `json:"credsStore"`
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return warn(name, cfgPath+" is not valid JSON",
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
		return warn(name, cfgPath+" names "+strings.Join(missing, ", ")+", which is not on PATH",
			"registry pulls and pushes go anonymous: put the helper on PATH, "+
				"or drop credsStore/credHelpers from config.json and run `kido login SERVER`")
	}
	if len(found) > 0 {
		return pass(name, "from "+strings.Join(found, ", ")+", which "+cfgPath+" names")
	}
	return pass(name, "stored in "+cfgPath)
}
