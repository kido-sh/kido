package mount

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/kido-sh/kido/internal/engine"
)

// LiveLabel is the image label that says the guest mounts kido's virtio-fs shares; its value is "1".
const LiveLabel = "sh.kido.virtiofs"

const (
	magic      = "KIDOMOUNT1"
	headerSize = 64 << 10 // the disk header the guest identifies a disk by; the tar follows it
	outSize    = 4 << 30  // sparse; also bounds what the guest can send back
	stateFile  = "mounts.json"
	cfgFile    = "mounts.cfg"
	diskDir    = "mounts"
)

type mountState struct {
	engine.Mount
	Live      bool  `json:"live,omitempty"`      // the source directory is shared over virtio-fs, not copied
	File      bool  `json:"file,omitempty"`      // the source is a regular file
	Size      int64 `json:"size,omitempty"`      // bytes packed at the last boot
	Populated bool  `json:"populated,omitempty"` // the target holds a copy kido made
	Collected bool  `json:"collected,omitempty"` // the out disk holds the guest's tar, not applied yet
}

type state struct {
	Token  string       `json:"token,omitempty"`
	Mounts []mountState `json:"mounts"`
}

// Save records the mounts of a new container in its directory dir. The sources must have been
// resolved (Resolve) and checked (Check). With live, which needs the vz backend and an image with
// LiveLabel, each source that is a directory is shared live; the others are copied.
func Save(dir string, ms []engine.Mount, live bool) error {
	if len(ms) == 0 {
		return nil
	}
	st := &state{}
	for _, m := range ms {
		if !filepath.IsAbs(m.Source) {
			return fmt.Errorf("mount source %s is not absolute", m.Source)
		}
		t, err := checkTarget(m.Target)
		if err != nil {
			return err
		}
		m.Target = t
		fi, err := os.Stat(m.Source)
		if err != nil {
			return fmt.Errorf("mount %s: %w", m.Source, err)
		}
		st.Mounts = append(st.Mounts, mountState{Mount: m, Live: live && fi.IsDir()})
	}
	if err := Check(ms); err != nil {
		return err
	}
	return save(dir, st)
}

// Mounted is a recorded mount and how it reaches the container: Mode is "live" or "copy".
type Mounted struct {
	engine.Mount
	Mode string `json:"mode"`
}

// List returns the mounts recorded for the container in dir.
func List(dir string) ([]Mounted, error) {
	st, err := load(dir)
	if st == nil {
		return nil, err
	}
	var out []Mounted
	for _, m := range st.Mounts {
		mode := "copy"
		if m.Live {
			mode = "live"
		}
		out = append(out, Mounted{m.Mount, mode})
	}
	return out, nil
}

type Disk struct {
	ID       string
	File     string
	ReadOnly bool
}

// Disks packs every copied source of the container in dir into a disk and returns the disks to
// attach, or nil when it has none. Each File is relative to dir.
func Disks(dir string) ([]Disk, error) {
	st, err := load(dir)
	if err != nil || st == nil || !hasCopies(st) {
		return nil, err
	}
	ddir := filepath.Join(dir, diskDir)
	if err := os.MkdirAll(ddir, 0o700); err != nil {
		return nil, err
	}
	keepUnapplied(dir, st)
	if st.Token, err = randomHex(8); err != nil {
		return nil, err
	}
	home := filepath.Dir(filepath.Dir(dir))
	var disks []Disk
	for i := range st.Mounts {
		m := &st.Mounts[i]
		if m.Live {
			continue
		}
		if err := sameSource(m.Source); err != nil {
			return nil, fmt.Errorf("mount %s: %w", m.Source, err)
		}
		in := filepath.Join(ddir, fmt.Sprintf("in-%d.img", i))
		man, size, file, err := packDisk(in, header(st.Token, "in", i), m.Source, m.Target, home, m.ReadOnly)
		if err != nil {
			return nil, fmt.Errorf("mount %s: %w", m.Source, err)
		}
		m.File, m.Size, m.Collected = file, size, false
		disks = append(disks, Disk{ID: fmt.Sprintf("kidoin%d", i), File: diskDir + "/" + filepath.Base(in), ReadOnly: true})
		if m.ReadOnly {
			continue
		}
		mb, err := json.Marshal(man)
		if err != nil {
			return nil, err
		}
		if err := writeFile(filepath.Join(ddir, fmt.Sprintf("manifest-%d.json", i)), mb); err != nil {
			return nil, err
		}
		out := filepath.Join(ddir, fmt.Sprintf("out-%d.img", i))
		if err := makeOutDisk(out, header(st.Token, "out", i)); err != nil {
			return nil, err
		}
		disks = append(disks, Disk{ID: fmt.Sprintf("kidoout%d", i), File: diskDir + "/" + filepath.Base(out)})
	}
	if err := save(dir, st); err != nil {
		return nil, err
	}
	return disks, nil
}

type Share struct {
	Name     string
	Source   string
	ReadOnly bool
	Dev, Ino uint64 // of Source when Shares checked it
}

// Shares returns the container's live mounts, checking that each source is still a directory.
func Shares(dir string) ([]Share, error) {
	st, err := load(dir)
	if st == nil {
		return nil, err
	}
	var shares []Share
	for i, m := range st.Mounts {
		if !m.Live {
			continue
		}
		if err := sameSource(m.Source); err != nil {
			return nil, fmt.Errorf("mount %s: %w", m.Source, err)
		}
		fi, err := os.Stat(m.Source)
		if err == nil && !fi.IsDir() {
			err = errors.New("it is no longer a directory")
		}
		if err != nil {
			return nil, fmt.Errorf("mount %s: %w", m.Source, err)
		}
		sh := Share{Name: shareName(i), Source: m.Source, ReadOnly: m.ReadOnly}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			sh.Dev, sh.Ino = uint64(st.Dev), st.Ino
		}
		shares = append(shares, sh)
	}
	return shares, nil
}

func shareName(i int) string { return "m" + strconv.Itoa(i) }

func hasCopies(st *state) bool {
	for _, m := range st.Mounts {
		if !m.Live {
			return true
		}
	}
	return false
}

// sameSource refuses a source that no longer resolves to itself: a symlink placed in its path
// since the container was created, for instance through a live mount of a parent directory, must
// not send the mount somewhere else. A change between this check and its use is not caught.
func sameSource(src string) error {
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	if real != src {
		return fmt.Errorf("the path now leads to %s; refusing to follow it", real)
	}
	return nil
}

// QEMUArgs returns the QEMU arguments that attach the container's Disks. QEMU must run in dir.
func QEMUArgs(dir string) ([]string, error) {
	st, err := load(dir)
	if err != nil {
		return nil, err
	}
	if st != nil {
		for _, m := range st.Mounts {
			if m.Live {
				return nil, fmt.Errorf("mount %s is shared live, which needs the vz backend", m.Source)
			}
		}
	}
	disks, err := Disks(dir)
	if err != nil {
		return nil, err
	}
	if len(disks) == 0 {
		os.Remove(filepath.Join(dir, cfgFile))
		return nil, nil
	}
	var cfg strings.Builder
	for _, d := range disks {
		writeDrive(&cfg, d.ID, d.File, d.ReadOnly)
	}
	if err := writeFile(filepath.Join(dir, cfgFile), []byte(cfg.String())); err != nil {
		return nil, err
	}
	return []string{"-readconfig", cfgFile}, nil
}

// VZArgs returns the kido-vmm arguments that attach the container's Disks and Shares, and the
// Shares. kido-vmm must run in dir.
func VZArgs(dir string) ([]string, []Share, error) {
	shares, err := Shares(dir)
	if err != nil {
		return nil, nil, err
	}
	disks, err := Disks(dir)
	if err != nil {
		return nil, nil, err
	}
	var args []string
	for _, d := range disks {
		flag := "--extra-disk"
		if d.ReadOnly {
			flag = "--extra-disk-ro"
		}
		args = append(args, flag, d.File)
	}
	for _, sh := range shares {
		flag := "--share"
		if sh.ReadOnly {
			flag = "--share-ro"
		}
		args = append(args, flag, sh.Name+"="+sh.Source)
	}
	return args, shares, nil
}

// keepUnapplied moves aside the changes of an earlier boot that were collected but never applied,
// so packing this boot's disks cannot overwrite them.
func keepUnapplied(dir string, st *state) {
	for i := range st.Mounts {
		m := &st.Mounts[i]
		if !m.Collected {
			continue
		}
		m.Collected = false
		out := filepath.Join(dir, diskDir, fmt.Sprintf("out-%d.img", i))
		kept := filepath.Join(dir, diskDir, fmt.Sprintf("unapplied-%d.img", i))
		if os.Rename(out, kept) == nil {
			fmt.Fprintf(os.Stderr, "kido: mounts: changes in %s from an earlier run were not copied back; the container's tar is in %s (after the first 64 KiB)\n", m.Target, kept)
		}
	}
}

func writeDrive(b *strings.Builder, id, file string, readOnly bool) {
	fmt.Fprintf(b, "[drive %q]\n  if = \"none\"\n  file = %q\n  format = \"raw\"\n", id, file)
	if readOnly {
		b.WriteString("  readonly = \"on\"\n")
	}
	fmt.Fprintf(b, "\n[device %q]\n  driver = \"virtio-blk-pci\"\n  drive = %q\n\n", id+"dev", id)
}

func header(token, kind string, i int) []byte {
	h := make([]byte, headerSize)
	copy(h, fmt.Sprintf("%s %s %s %d\n", magic, token, kind, i))
	return h
}

func makeOutDisk(path string, hdr []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(hdr)
	if err == nil {
		err = f.Truncate(outSize)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func load(dir string) (*state, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st := &state{}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("%s: %w", stateFile, err)
	}
	return st, nil
}

func save(dir string, st *state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, stateFile), append(b, '\n'))
}

// writeFile replaces path atomically with a 0600 file.
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o600)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
