package mount

import (
	"archive/tar"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// manifestEntry is what a path looked like when it was packed. Apply compares the container's copy
// with Hash to find what the container changed, and the host file with Size and MTime to find what
// changed on the host meanwhile.
type manifestEntry struct {
	Kind  byte   `json:"k"` // 'd', 'f' or 'l'
	Mode  uint32 `json:"m"` // permission bits
	Size  int64  `json:"s,omitempty"`
	MTime int64  `json:"t"` // unix nanoseconds
	Hash  string `json:"h,omitempty"`
	Link  string `json:"l,omitempty"`
}

// manifest maps a slash-separated path relative to the mount root ("." for the root itself, which
// is the file of a file mount) to its entry.
type manifest map[string]manifestEntry

// packDisk writes hdr and then a tar of src to a new disk image at dst. A directory is packed as
// "./" and its contents, a file under the base name of target. Symlinks are packed as symlinks and
// never followed, and other special files are left out. A read-only mount skips the kido state
// directory home when src contains it; a read-write mount of such a source is refused.
func packDisk(dst string, hdr []byte, src, target, home string, readOnly bool) (manifest, int64, bool, error) {
	fi, err := os.Lstat(src)
	if err != nil {
		return nil, 0, false, err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()
	bw := bufio.NewWriterSize(f, 1<<20)
	if _, err := bw.Write(hdr); err != nil {
		return nil, 0, false, err
	}
	p := &packer{tw: tar.NewWriter(bw), man: manifest{}}
	file := fi.Mode().IsRegular()
	if file {
		root, err := os.OpenRoot(filepath.Dir(src))
		if err != nil {
			return nil, 0, false, err
		}
		defer root.Close()
		p.root = root
		err = p.file(filepath.Base(src), ".", path.Base(target))
		if err != nil {
			return nil, 0, false, err
		}
	} else {
		skip := ""
		if rel, err := filepath.Rel(src, home); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			if !readOnly {
				return nil, 0, false, fmt.Errorf("the source contains kido's state directory %s; mount a subdirectory, or add :ro", home)
			}
			skip = filepath.ToSlash(rel)
		}
		root, err := os.OpenRoot(src)
		if err != nil {
			return nil, 0, false, err
		}
		defer root.Close()
		p.root = root
		err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if skip != "" && (name == skip || strings.HasPrefix(name, skip+"/")) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			return p.entry(name)
		})
		if err != nil {
			return nil, 0, false, err
		}
	}
	if err := p.tw.Close(); err != nil {
		return nil, 0, false, err
	}
	if err := bw.Flush(); err != nil {
		return nil, 0, false, err
	}
	return p.man, p.size, file, f.Close()
}

type packer struct {
	root *os.Root
	tw   *tar.Writer
	man  manifest
	size int64 // an upper bound of what the unpacked copy takes in the guest
}

func (p *packer) entry(name string) error {
	fi, err := p.root.Lstat(name)
	if err != nil {
		return err
	}
	tarName := "./"
	if name != "." {
		tarName += name
	}
	switch {
	case fi.IsDir():
		if name != "." {
			tarName += "/"
		}
		p.man[name] = manifestEntry{Kind: 'd', Mode: uint32(fi.Mode().Perm()), MTime: fi.ModTime().UnixNano()}
		return p.write(fi, &tar.Header{Typeflag: tar.TypeDir, Name: tarName}, nil)
	case fi.Mode()&fs.ModeSymlink != 0:
		link, err := p.root.Readlink(name)
		if err != nil {
			return err
		}
		p.man[name] = manifestEntry{Kind: 'l', Mode: uint32(fi.Mode().Perm()), MTime: fi.ModTime().UnixNano(), Link: link}
		return p.write(fi, &tar.Header{Typeflag: tar.TypeSymlink, Name: tarName, Linkname: link}, nil)
	case fi.Mode().IsRegular():
		return p.file(name, name, tarName)
	}
	return nil
}

// file packs the regular file name (in p.root) as tarName and records it under key.
func (p *packer) file(name, key, tarName string) error {
	f, err := p.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", name)
	}
	h := sha256.New()
	err = p.write(fi, &tar.Header{Typeflag: tar.TypeReg, Name: tarName, Size: fi.Size()}, func(w io.Writer) error {
		n, err := io.CopyN(w, io.TeeReader(f, h), fi.Size())
		if err == io.EOF {
			return fmt.Errorf("%s shrank while it was copied (%d of %d bytes); try again", name, n, fi.Size())
		}
		return err
	})
	if err != nil {
		return err
	}
	p.man[key] = manifestEntry{Kind: 'f', Mode: uint32(fi.Mode().Perm()), Size: fi.Size(),
		MTime: fi.ModTime().UnixNano(), Hash: hex.EncodeToString(h.Sum(nil))}
	return nil
}

func (p *packer) write(fi fs.FileInfo, h *tar.Header, body func(io.Writer) error) error {
	h.Mode = int64(fi.Mode().Perm())
	h.ModTime = fi.ModTime().Truncate(time.Second)
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		h.Uid, h.Gid = int(st.Uid), int(st.Gid)
	}
	if err := p.tw.WriteHeader(h); err != nil {
		return err
	}
	p.size += 4096 + (h.Size+4095)/4096*4096
	if body == nil {
		return nil
	}
	return body(p.tw)
}
