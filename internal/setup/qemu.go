// Package setup installs and locates what kido needs on the host: a qemu-system-aarch64 that may
// use Hypervisor.framework, with qemu-img next to it.
package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Release is a QEMU archive that kido setup can install.
type Release struct {
	Version string // directory name under $KIDO_HOME/qemu
	URL     string // https URL, or the absolute path of a local archive
	SHA256  string // hex SHA-256 of the archive
	Size    int64  // archive size in bytes; 0 when unknown
}

// PinnedQEMU is the release kido is tested with: the darwin arm64 build of QEMU 11.1.3 from
// github.com/cocoa-xu/qemu-build, a .tar.xz with usr/local/{bin,lib,share}.
var PinnedQEMU = Release{
	Version: "11.1.3",
	URL:     "https://github.com/cocoa-xu/qemu-build/releases/download/v11.1.3/qemu-aarch64-apple-darwin.tar.xz",
	SHA256:  "3bd76b0f582b94764c9e87a06726b33bf3bbc28c65dd0b15a6b4cc0c66e6da4c",
	Size:    85522180,
}

// DevQEMU is a development QEMU, the last fallback of FindQEMU. It is empty unless the build sets
// it with -X, as `make DEV_QEMU=PATH` does.
var DevQEMU string

const (
	qemuName    = "qemu-system-aarch64"
	qemuImgName = "qemu-img"
)

// Where a QEMU was found.
const (
	SourceEnv   = "KIDO_QEMU"
	SourceSetup = "kido setup"
	SourceDev   = "development default"
)

// QEMU is a located qemu-system-aarch64 and the qemu-img in the same directory.
type QEMU struct {
	Path   string
	Img    string
	Source string
}

// ErrNoQEMU means that neither KIDO_QEMU nor an installed QEMU exists.
var ErrNoQEMU = errors.New("QEMU not found: run `KIDO_BACKEND=qemu kido setup` to install it, " +
	"or set KIDO_QEMU to the absolute path of a qemu-system-aarch64 signed for Hypervisor.framework")

// QEMUDir is where kido setup installs QEMU: one directory per version and a "current" symlink.
func QEMUDir(home string) string { return filepath.Join(home, "qemu") }

// FindQEMU looks for QEMU in this order: $KIDO_QEMU, $KIDO_HOME/qemu/current (from kido setup), then
// DevQEMU when it exists. When none exists it returns ErrNoQEMU with the path kido setup installs
// to, so a caller that only reports the error later still holds an absolute path.
func FindQEMU(home string) (QEMU, error) {
	if p := os.Getenv("KIDO_QEMU"); p != "" {
		q := newQEMU(p, SourceEnv)
		if !filepath.IsAbs(p) {
			return q, fmt.Errorf("KIDO_QEMU=%q is not an absolute path; qemu-img is looked up next to it", p)
		}
		return q, nil
	}
	installed := newQEMU(filepath.Join(QEMUDir(home), "current", "bin", qemuName), SourceSetup)
	if isRegular(installed.Path) {
		return installed, nil
	}
	if DevQEMU != "" && isRegular(DevQEMU) {
		return newQEMU(DevQEMU, SourceDev), nil
	}
	return installed, ErrNoQEMU
}

// CurrentVersion is the version that $KIDO_HOME/qemu/current points to, or "".
func CurrentVersion(home string) string {
	target, err := os.Readlink(filepath.Join(QEMUDir(home), "current"))
	if err != nil || !validVersion(target) {
		return ""
	}
	return target
}

func newQEMU(path, source string) QEMU {
	path = filepath.Clean(path)
	return QEMU{Path: path, Img: filepath.Join(filepath.Dir(path), qemuImgName), Source: source}
}

func isRegular(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
