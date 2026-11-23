package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Options for Install.
type Options struct {
	Home    string // KIDO_HOME, absolute
	Release Release
	Force   bool      // download and unpack again even when this version is installed
	Log     io.Writer // progress lines; nil discards them
}

// Result describes the QEMU that Install put in place.
type Result struct {
	Dir        string // $KIDO_HOME/qemu/<version>
	QEMU       QEMU
	Version    string   // first line of qemu-system-aarch64 --version
	Downloaded bool     // false when the version was already unpacked or came from a local file
	Signed     []string // binaries that had to be signed
	Libraries  Libraries
}

// MissingLibrariesError means the archive unpacked but QEMU cannot start because dyld would not
// find some of its libraries.
type MissingLibrariesError struct {
	Dir     string
	Missing []string
}

func (e *MissingLibrariesError) Error() string {
	msg := fmt.Sprintf("QEMU is unpacked in %s but cannot start: it loads %d libraries that are not on this Mac:\n  %s",
		e.Dir, len(e.Missing), strings.Join(e.Missing, "\n  "))
	if f := HomebrewFormulas(e.Missing); len(f) > 0 {
		msg += "\nThis QEMU build takes them from Homebrew. Install them with\n  brew install " + strings.Join(f, " ") +
			"\nand run `kido setup` again (nothing is downloaded twice), or set KIDO_QEMU to a QEMU that runs here."
	}
	return msg
}

const markerName = ".kido-setup.json"

type marker struct {
	Version   string    `json:"version"`
	URL       string    `json:"url"`
	SHA256    string    `json:"sha256"`
	Installed time.Time `json:"installed"`
}

var (
	versionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)
	sha256RE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validVersion(v string) bool { return versionRE.MatchString(v) && v != "current" }

// Install downloads (or reads) a QEMU archive, checks its SHA-256, unpacks what kido needs into
// $KIDO_HOME/qemu/<version>, signs qemu-system-aarch64 for HVF when its signature lacks the
// entitlement, checks that both programs start, and points $KIDO_HOME/qemu/current at the version.
func Install(ctx context.Context, o Options) (*Result, error) {
	log := o.Log
	if log == nil {
		log = io.Discard
	}
	rel := o.Release
	rel.SHA256 = strings.ToLower(rel.SHA256)
	if !validVersion(rel.Version) {
		return nil, fmt.Errorf("invalid QEMU version name %q", rel.Version)
	}
	if !sha256RE.MatchString(rel.SHA256) {
		return nil, fmt.Errorf("invalid SHA-256 %q: want 64 hex digits", rel.SHA256)
	}
	local := !strings.HasPrefix(rel.URL, "https://")
	if local && (strings.Contains(rel.URL, "://") || !filepath.IsAbs(rel.URL)) {
		return nil, fmt.Errorf("%q is neither an https URL nor an absolute file path", rel.URL)
	}

	qdir := QEMUDir(o.Home)
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		return nil, err
	}
	unlock, err := lockDir(qdir)
	if err != nil {
		return nil, err
	}
	defer unlock()

	vdir := filepath.Join(qdir, rel.Version)
	res := &Result{Dir: vdir, QEMU: newQEMU(filepath.Join(vdir, "bin", qemuName), SourceSetup)}
	if !o.Force && installed(vdir, rel) {
		fmt.Fprintf(log, "QEMU %s is already unpacked in %s\n", rel.Version, vdir)
		if res.Signed, err = signTree(ctx, vdir); err != nil {
			return nil, err
		}
	} else if err := fetchAndUnpack(ctx, qdir, vdir, rel, local, log, res); err != nil {
		return nil, err
	}
	for _, s := range res.Signed {
		fmt.Fprintf(log, "Signed %s ad-hoc (with %s for qemu-system-aarch64)\n", s, HypervisorEntitlement)
	}

	var missing []string
	for _, exe := range []string{res.QEMU.Path, res.QEMU.Img} {
		libs, err := CheckLibraries(exe)
		if err != nil {
			return res, fmt.Errorf("%s: %w", exe, err)
		}
		missing = append(missing, libs.Missing...)
		res.Libraries.External = append(res.Libraries.External, libs.External...)
	}
	res.Libraries.Missing = sortedKeys(setOf(missing))
	res.Libraries.External = sortedKeys(setOf(res.Libraries.External))
	if len(res.Libraries.Missing) > 0 {
		return res, &MissingLibrariesError{Dir: vdir, Missing: res.Libraries.Missing}
	}
	if res.Version, err = runTool(ctx, res.QEMU.Path, "--version"); err != nil {
		return res, fmt.Errorf("%s does not start: %v: %s", res.QEMU.Path, err, res.Version)
	}
	if out, err := runTool(ctx, res.QEMU.Img, "--version"); err != nil {
		return res, fmt.Errorf("%s does not start: %v: %s", res.QEMU.Img, err, out)
	}
	if err := switchCurrent(qdir, rel.Version); err != nil {
		return res, err
	}
	return res, nil
}

func fetchAndUnpack(ctx context.Context, qdir, vdir string, rel Release, local bool, log io.Writer, res *Result) error {
	staging, err := os.MkdirTemp(qdir, ".setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	archive := rel.URL
	if local {
		fmt.Fprintf(log, "Checking %s\n", archive)
		if err := verifyFile(archive, rel.SHA256, rel.Size); err != nil {
			return err
		}
	} else {
		archive = filepath.Join(staging, "qemu.archive")
		size := ""
		if rel.Size > 0 {
			size = fmt.Sprintf(" (%.1f MB)", float64(rel.Size)/1e6)
		}
		fmt.Fprintf(log, "Downloading QEMU %s%s\n  from %s\n", rel.Version, size, rel.URL)
		if err := download(ctx, rel.URL, archive, rel.SHA256, rel.Size, log); err != nil {
			return err
		}
		res.Downloaded = true
	}
	fmt.Fprintf(log, "SHA-256 %s verified\n", rel.SHA256)

	tree := filepath.Join(staging, "tree")
	if err := os.Mkdir(tree, 0o700); err != nil {
		return err
	}
	stats, err := unpack(ctx, archive, tree)
	if err != nil {
		return fmt.Errorf("unpacking the QEMU archive: %w", err)
	}
	for _, name := range []string{qemuName, qemuImgName} {
		if !isRegular(filepath.Join(tree, "bin", name)) {
			return fmt.Errorf("the QEMU archive has no bin/%s (or usr/local/bin/%s)", name, name)
		}
	}
	fmt.Fprintf(log, "Unpacked %d files (%.1f MB) into %s; skipped %d entries kido does not use\n",
		stats.Files, float64(stats.Bytes)/1e6, vdir, stats.Skipped)
	if res.Signed, err = signTree(ctx, tree); err != nil {
		return err
	}
	m, err := json.Marshal(marker{Version: rel.Version, URL: rel.URL, SHA256: rel.SHA256, Installed: time.Now().UTC()})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tree, markerName), append(m, '\n'), 0o600); err != nil {
		return err
	}
	if _, err := os.Lstat(vdir); err == nil {
		if err := os.Rename(vdir, filepath.Join(staging, "old")); err != nil {
			return err
		}
	}
	return os.Rename(tree, vdir)
}

// installed reports whether vdir holds rel, unpacked by an earlier Install.
func installed(vdir string, rel Release) bool {
	b, err := os.ReadFile(filepath.Join(vdir, markerName))
	if err != nil {
		return false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil || m.SHA256 != rel.SHA256 || m.Version != rel.Version {
		return false
	}
	return isRegular(filepath.Join(vdir, "bin", qemuName)) && isRegular(filepath.Join(vdir, "bin", qemuImgName))
}

func signTree(ctx context.Context, dir string) ([]string, error) {
	var signed []string
	for _, b := range []struct {
		name string
		hvf  bool
	}{{qemuName, true}, {qemuImgName, false}} {
		did, err := ensureSigned(ctx, filepath.Join(dir, "bin", b.name), b.hvf)
		if err != nil {
			return nil, err
		}
		if did {
			signed = append(signed, b.name)
		}
	}
	return signed, nil
}

// switchCurrent atomically points qdir/current at version.
func switchCurrent(qdir, version string) error {
	tmp := filepath.Join(qdir, fmt.Sprintf(".current-%d", os.Getpid()))
	os.Remove(tmp)
	if err := os.Symlink(version, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(qdir, "current")); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// lockDir takes an exclusive flock on dir/.lock so two kido setup runs do not interleave.
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another `kido setup` is installing QEMU into " + dir)
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

func setOf(items []string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}
