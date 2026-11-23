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
	"strings"

	"github.com/kido-sh/kido/internal/engine"
)

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
// resolved (Resolve) and checked (Check).
func Save(dir string, ms []engine.Mount) error {
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
		st.Mounts = append(st.Mounts, mountState{Mount: m})
	}
	if err := Check(ms); err != nil {
		return err
	}
	return save(dir, st)
}

// List returns the mounts recorded for the container in dir.
func List(dir string) ([]engine.Mount, error) {
	st, err := load(dir)
	if st == nil {
		return nil, err
	}
	var out []engine.Mount
	for _, m := range st.Mounts {
		out = append(out, m.Mount)
	}
	return out, nil
}

// QEMUArgs packs every source of the container in dir into a disk and returns the QEMU arguments
// that attach them, or nil when the container has no mounts. QEMU must run in dir.
func QEMUArgs(dir string) ([]string, error) {
	st, err := load(dir)
	if err != nil {
		return nil, err
	}
	if st == nil || len(st.Mounts) == 0 {
		os.Remove(filepath.Join(dir, cfgFile))
		return nil, nil
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
	var cfg strings.Builder
	for i := range st.Mounts {
		m := &st.Mounts[i]
		in := filepath.Join(ddir, fmt.Sprintf("in-%d.img", i))
		man, size, file, err := packDisk(in, header(st.Token, "in", i), m.Source, m.Target, home, m.ReadOnly)
		if err != nil {
			return nil, fmt.Errorf("mount %s: %w", m.Source, err)
		}
		m.File, m.Size, m.Collected = file, size, false
		writeDrive(&cfg, fmt.Sprintf("kidoin%d", i), diskDir+"/"+filepath.Base(in), true)
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
		writeDrive(&cfg, fmt.Sprintf("kidoout%d", i), diskDir+"/"+filepath.Base(out), false)
	}
	if err := writeFile(filepath.Join(dir, cfgFile), []byte(cfg.String())); err != nil {
		return nil, err
	}
	if err := save(dir, st); err != nil {
		return nil, err
	}
	return []string{"-readconfig", cfgFile}, nil
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
