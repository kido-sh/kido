// Package mount implements kido's copy mounts (-v and --mount type=bind).
//
// The guest kernel has no shared-folder filesystem (no NFS client, 9p or virtio-fs), so a mount is a
// copy: at every boot the host packs the source into a raw tar disk and the guest unpacks it at the
// target. For a read-write mount, the guest packs the target onto a second disk when the container
// stops, and the host copies new and changed files back into the source. It never overwrites a
// file that also changed on the host, and it never deletes host files.
package mount

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kido-sh/kido/internal/engine"
)

// systemDirs are guest directories a mount may not replace: the container could not boot or exec
// without them.
var systemDirs = []string{
	"/bin", "/sbin", "/usr", "/usr/bin", "/usr/sbin", "/usr/lib", "/usr/libexec", "/usr/local",
	"/etc", "/var", "/tmp", "/private", "/private/etc", "/private/var", "/private/tmp",
	"/Library", "/Users", "/Volumes", "/var/root", "/private/var/root",
}

// ParseVolume parses Docker's -v HOST:GUEST[:OPTIONS] with OPTIONS ro or rw. HOST is an absolute
// path or a relative one starting with "." or "/"; named volumes are not supported.
func ParseVolume(s string) (engine.Mount, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return engine.Mount{}, fmt.Errorf("-v %s: want HOST:CONTAINER[:ro]", s)
	}
	host, guest := parts[0], parts[1]
	if !strings.HasPrefix(host, "/") && !strings.HasPrefix(host, ".") {
		return engine.Mount{}, fmt.Errorf("-v %s: named volumes are not supported; give a host path (absolute, or starting with ./)", s)
	}
	m := engine.Mount{Source: host, Target: guest}
	if len(parts) == 3 {
		for _, o := range strings.Split(parts[2], ",") {
			switch o {
			case "ro", "readonly":
				m.ReadOnly = true
			case "rw":
				m.ReadOnly = false
			case "cached", "delegated", "consistent":
			default:
				return engine.Mount{}, fmt.Errorf("-v %s: unsupported option %q (kido supports ro and rw)", s, o)
			}
		}
	}
	return m, nil
}

// ParseMount parses Docker's --mount type=bind,source=HOST,target=GUEST[,readonly].
func ParseMount(s string) (engine.Mount, error) {
	r := csv.NewReader(strings.NewReader(s))
	fields, err := r.Read()
	if err != nil {
		return engine.Mount{}, fmt.Errorf("--mount %s: %v", s, err)
	}
	var m engine.Mount
	typ := ""
	for _, f := range fields {
		key, val, _ := strings.Cut(f, "=")
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "type":
			typ = val
		case "source", "src":
			m.Source = val
		case "target", "destination", "dst":
			m.Target = val
		case "readonly", "ro":
			switch strings.ToLower(val) {
			case "", "true", "1":
				m.ReadOnly = true
			case "false", "0":
				m.ReadOnly = false
			default:
				return engine.Mount{}, fmt.Errorf("--mount %s: invalid readonly value %q", s, val)
			}
		case "consistency":
		default:
			return engine.Mount{}, fmt.Errorf("--mount %s: unsupported option %q", s, key)
		}
	}
	if typ != "bind" {
		return engine.Mount{}, fmt.Errorf("--mount %s: only type=bind is supported", s)
	}
	if m.Source == "" || m.Target == "" {
		return engine.Mount{}, fmt.Errorf("--mount %s: source and target are required", s)
	}
	return m, nil
}

// Resolve makes m's source an absolute, symlink-free path that exists, and checks its target.
func Resolve(m engine.Mount) (engine.Mount, error) {
	src, err := filepath.Abs(m.Source)
	if err != nil {
		return m, err
	}
	if src, err = filepath.EvalSymlinks(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, fmt.Errorf("mount source %s does not exist", m.Source)
		}
		return m, fmt.Errorf("mount source %s: %w", m.Source, err)
	}
	fi, err := os.Stat(src)
	if err != nil {
		return m, err
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return m, fmt.Errorf("mount source %s is neither a directory nor a regular file", m.Source)
	}
	m.Source = src
	if m.Target, err = checkTarget(m.Target); err != nil {
		return m, err
	}
	return m, nil
}

func checkTarget(t string) (string, error) {
	if !strings.HasPrefix(t, "/") {
		return "", fmt.Errorf("mount target %q must be an absolute path", t)
	}
	if len(t) > 1024 {
		return "", fmt.Errorf("mount target %.40q... is too long", t)
	}
	for _, c := range t {
		if c < 0x20 || c == 0x7f {
			return "", fmt.Errorf("mount target %q contains a control character", t)
		}
	}
	if slices.Contains(strings.Split(t, "/"), "..") {
		return "", fmt.Errorf("mount target %q must not contain ..", t)
	}
	clean := path.Clean(t)
	switch {
	case clean == "/":
		return "", errors.New("mount target / is not allowed")
	case clean == "/dev" || strings.HasPrefix(clean, "/dev/"),
		clean == "/System" || strings.HasPrefix(clean, "/System/"),
		clean == "/usr/libexec/kido-agent",
		slices.Contains(systemDirs, clean):
		return "", fmt.Errorf("mount target %s would replace a system path of the container", clean)
	}
	return clean, nil
}

// Check validates a set of resolved mounts: unique targets, none inside another.
func Check(ms []engine.Mount) error {
	for i, a := range ms {
		for _, b := range ms[i+1:] {
			if a.Target == b.Target {
				return fmt.Errorf("duplicate mount target %s", a.Target)
			}
			if strings.HasPrefix(b.Target, a.Target+"/") || strings.HasPrefix(a.Target, b.Target+"/") {
				return fmt.Errorf("mount targets %s and %s must not be nested", a.Target, b.Target)
			}
		}
	}
	return nil
}
