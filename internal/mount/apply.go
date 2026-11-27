package mount

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const maxEntries = 1 << 20

// copyBackLimit caps the bytes one Apply writes to the host.
var copyBackLimit int64 = outSize

var errBudget = errors.New("the container sent back more than the copy-back limit")

// Apply copies the changes Collect packed into the sources; it runs after QEMU has exited. The tar
// comes from the guest and is untrusted: it is applied through os.Root (nothing outside the source
// is written), only directories, regular files and symlinks that stay inside the mount are taken,
// permissions are limited to 0777, and the total size and entry count are capped. A file that
// changed both on the host and in the container is not overwritten; the container's version is
// saved beside it as NAME.kido-<container id>. Files deleted in the container are kept. Everything
// worth knowing is reported to warn.
func Apply(dir string, warn io.Writer) {
	st, err := load(dir)
	if err != nil {
		fmt.Fprintf(warn, "kido: mounts: %v\n", err)
		return
	}
	if st == nil {
		return
	}
	ddir := filepath.Join(dir, diskDir)
	home := filepath.Dir(filepath.Dir(dir))
	for i := range st.Mounts {
		m := &st.Mounts[i]
		out := filepath.Join(ddir, fmt.Sprintf("out-%d.img", i))
		man := filepath.Join(ddir, fmt.Sprintf("manifest-%d.json", i))
		if m.Live || m.ReadOnly || !m.Collected {
			continue
		}
		m.Collected = false
		err := sameSource(m.Source)
		if _, lerr := os.Lstat(m.Source); m.File && errors.Is(lerr, fs.ErrNotExist) {
			err = sameSource(filepath.Dir(m.Source))
		}
		if err == nil {
			err = applyMount(m, out, man, filepath.Base(dir), home, warn)
		}
		if err != nil {
			kept := filepath.Join(ddir, fmt.Sprintf("unapplied-%d.img", i))
			if os.Rename(out, kept) == nil {
				fmt.Fprintf(warn, "kido: mounts: changes in %s were not all copied back: %s; the container's tar is in %s (after the first 64 KiB)\n", m.Target, sanitize(err.Error()), kept)
			} else {
				fmt.Fprintf(warn, "kido: mounts: changes in %s were not all copied back: %s\n", m.Target, sanitize(err.Error()))
			}
		}
		os.Remove(out)
		os.Remove(man)
	}
	if err := save(dir, st); err != nil {
		fmt.Fprintf(warn, "kido: mounts: %v\n", err)
	}
}

type applier struct {
	root      *os.Root
	rootDir   string
	file      bool   // a file mount: the only entry taken is guestName, written as hostName
	guestName string // base name of the target
	hostName  string // base name of the source
	man       manifest
	seen      map[string]bool
	cid       string
	skip      string // the kido state directory relative to the source, never written
	warn      io.Writer
	budget    int64
	odd       int      // entries of a type that is never copied back
	links     []string // symlinks this apply created
}

func applyMount(m *mountState, out, manPath, cid, home string, warn io.Writer) error {
	mb, err := os.ReadFile(manPath)
	if err != nil {
		return err
	}
	a := &applier{man: manifest{}, seen: map[string]bool{}, cid: cid, warn: warn, budget: copyBackLimit}
	if err := json.Unmarshal(mb, &a.man); err != nil {
		return err
	}
	a.rootDir = m.Source
	if m.File {
		a.file, a.rootDir = true, filepath.Dir(m.Source)
		a.guestName, a.hostName = path.Base(m.Target), filepath.Base(m.Source)
	} else if rel, err := filepath.Rel(m.Source, home); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		a.skip = filepath.ToSlash(rel)
	}
	if a.root, err = os.OpenRoot(a.rootDir); err != nil {
		return err
	}
	defer a.root.Close()
	f, err := os.Open(out)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(headerSize, io.SeekStart); err != nil {
		return err
	}
	tr := tar.NewReader(io.LimitReader(f, outSize))
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read the container's tar: %w", err)
		}
		if n >= maxEntries {
			return fmt.Errorf("the container sent back more than %d entries", maxEntries)
		}
		if err := a.entry(h, tr); err != nil {
			return err
		}
	}
	// The lexical check above cannot see through symlinks (s -> . then L -> s/..): resolve every new
	// link through the root once all are in place, and remove the ones that leave it.
	for _, l := range a.links {
		if _, err := a.root.Stat(l); err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.root.Remove(l)
			fmt.Fprintf(warn, "kido: mounts: removed symlink %s from the container: it resolves outside the mount\n", a.show(l))
		}
	}
	if a.odd > 0 {
		fmt.Fprintf(warn, "kido: mounts: skipped %d entries in %s that are not files, directories or symlinks\n", a.odd, m.Target)
	}
	if !a.file {
		deleted := 0
		for k := range a.man {
			if !a.seen[k] {
				deleted++
			}
		}
		if deleted > 0 {
			fmt.Fprintf(warn, "kido: mounts: %d paths deleted in the container's %s were kept in %s (copy mounts never delete host files)\n", deleted, m.Target, m.Source)
		}
	}
	return nil
}

// entry applies one tar entry; only a budget overrun stops the whole apply.
func (a *applier) entry(h *tar.Header, r io.Reader) error {
	name, ok := cleanName(h.Name)
	if !ok {
		fmt.Fprintf(a.warn, "kido: mounts: skipped %q from the container: not a path inside the mount\n", h.Name)
		return nil
	}
	key, host := name, name
	if a.file {
		if name != a.guestName {
			fmt.Fprintf(a.warn, "kido: mounts: skipped %q from the container: not the mounted file\n", h.Name)
			return nil
		}
		key, host = ".", a.hostName
	} else if a.skip != "" && (name == a.skip || strings.HasPrefix(name, a.skip+"/")) {
		return nil
	}
	switch h.Typeflag {
	case tar.TypeDir:
		if a.file {
			a.odd++
			return nil
		}
		a.seen[key] = true
		if key != "." {
			a.dir(host)
		}
	case tar.TypeReg:
		a.seen[key] = true
		return a.regular(key, host, h, r)
	case tar.TypeXGlobalHeader:
	case tar.TypeSymlink:
		if a.file {
			a.odd++
			return nil
		}
		a.seen[key] = true
		a.symlink(key, host, h.Linkname)
	default:
		a.odd++
	}
	return nil
}

// cleanName turns a tar entry name into a slash path relative to the mount root ("." for the
// root), refusing absolute names and names that leave the root.
func cleanName(n string) (string, bool) {
	if n == "" || strings.ContainsRune(n, 0) || path.IsAbs(n) {
		return "", false
	}
	c := path.Clean(n)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

// show quotes a host path for a warning: its name comes from the guest.
func (a *applier) show(host string) string {
	return strconv.Quote(filepath.Join(a.rootDir, filepath.FromSlash(host)))
}

func (a *applier) dir(host string) {
	fi, err := a.root.Lstat(host)
	switch {
	case err == nil && fi.IsDir():
	case err == nil:
		fmt.Fprintf(a.warn, "kido: mounts: kept %s; the container has a directory there\n", a.show(host))
	case errors.Is(err, fs.ErrNotExist):
		if err := a.root.MkdirAll(host, 0o755); err != nil {
			fmt.Fprintf(a.warn, "kido: mounts: %q\n", err.Error())
		}
	default:
		fmt.Fprintf(a.warn, "kido: mounts: %q\n", err.Error())
	}
}

func (a *applier) parent(host string) bool {
	p := path.Dir(host)
	if p == "." {
		return true
	}
	if err := a.root.MkdirAll(p, 0o755); err != nil {
		fmt.Fprintf(a.warn, "kido: mounts: skipped %s: %q\n", a.show(host), err.Error())
		return false
	}
	return true
}

func (a *applier) regular(key, host string, h *tar.Header, r io.Reader) error {
	if !a.parent(host) {
		return nil
	}
	suffix, err := randomHex(6)
	if err != nil {
		return err
	}
	tmp := path.Join(path.Dir(host), ".kido-tmp-"+suffix)
	f, err := a.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fmt.Fprintf(a.warn, "kido: mounts: skipped %s: %q\n", a.show(host), err.Error())
		return nil
	}
	sum := sha256.New()
	n, err := io.Copy(f, io.TeeReader(io.LimitReader(r, a.budget+1), sum))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > a.budget {
		err = errBudget
	}
	if err != nil {
		a.root.Remove(tmp)
		return err
	}
	a.budget -= n
	mode := fs.FileMode(h.Mode) & fs.ModePerm
	hash := hex.EncodeToString(sum.Sum(nil))
	me, known := a.man[key]
	if known && me.Kind == 'f' && me.Hash == hash && fs.FileMode(me.Mode) == mode {
		a.root.Remove(tmp)
		return nil
	}
	a.root.Chmod(tmp, mode)
	a.root.Chtimes(tmp, h.ModTime, h.ModTime)
	fi, err := a.root.Lstat(host)
	hostSame := err == nil && known && me.Kind == 'f' && fi.Mode().IsRegular() &&
		fi.Size() == me.Size && fi.ModTime().UnixNano() == me.MTime
	if (errors.Is(err, fs.ErrNotExist) && !known) || hostSame {
		if err := a.root.Rename(tmp, host); err != nil {
			a.root.Remove(tmp)
			fmt.Fprintf(a.warn, "kido: mounts: skipped %s: %q\n", a.show(host), err.Error())
		}
		return nil
	}
	a.conflict(tmp, host)
	return nil
}

// conflict keeps the host's file and saves the container's version (tmp) beside it.
func (a *applier) conflict(tmp, host string) {
	defer a.root.Remove(tmp)
	for i := 0; i < 100; i++ {
		name := host + ".kido-" + a.cid
		if i > 0 {
			name += fmt.Sprintf("-%d", i)
		}
		err := a.root.Link(tmp, name)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			break
		}
		fmt.Fprintf(a.warn, "kido: mounts: %s changed on the host and in the container; kept the host's version, the container's is %s\n", a.show(host), a.show(name))
		return
	}
	fmt.Fprintf(a.warn, "kido: mounts: %s changed on the host and in the container; kept the host's version and dropped the container's\n", a.show(host))
}

func (a *applier) symlink(key, host, target string) {
	if !inside(path.Dir(key), target) {
		fmt.Fprintf(a.warn, "kido: mounts: skipped symlink %s -> %q: it points outside the mount\n", a.show(host), target)
		return
	}
	me, known := a.man[key]
	if known && me.Kind == 'l' && me.Link == target {
		return
	}
	if !a.parent(host) {
		return
	}
	fi, err := a.root.Lstat(host)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !known:
	case err == nil && known && me.Kind == 'l' && fi.Mode()&fs.ModeSymlink != 0:
		if cur, err := a.root.Readlink(host); err != nil || cur != me.Link {
			fmt.Fprintf(a.warn, "kido: mounts: kept symlink %s; it changed on the host and in the container\n", a.show(host))
			return
		}
		if err := a.root.Remove(host); err != nil {
			fmt.Fprintf(a.warn, "kido: mounts: %q\n", err.Error())
			return
		}
	default:
		fmt.Fprintf(a.warn, "kido: mounts: kept %s; the container made it a symlink to %q\n", a.show(host), target)
		return
	}
	if err := a.root.Symlink(target, host); err != nil {
		fmt.Fprintf(a.warn, "kido: mounts: %q\n", err.Error())
		return
	}
	a.links = append(a.links, host)
}

// inside reports whether a relative symlink target, read from directory dir of the mount, stays
// in the mount.
func inside(dir, target string) bool {
	if target == "" || path.IsAbs(target) || strings.ContainsRune(target, 0) {
		return false
	}
	j := path.Join(dir, target)
	return j != ".." && !strings.HasPrefix(j, "../")
}
