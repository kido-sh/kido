package runtime

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// TarHostPath writes src (file, directory or symlink, never followed) to w as a tar stream whose
// top-level entry is called name. Owners are reset to root.
func TarHostPath(w io.Writer, src, name string) error {
	if !SafeEntryName(name) || strings.Contains(name, "/") {
		return fmt.Errorf("invalid entry name %q", name)
	}
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !fi.Mode().IsRegular() && !fi.IsDir() {
			fmt.Fprintf(os.Stderr, "kido: skipping %s: not a regular file, directory or symlink\n", p)
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = path.Join(name, filepath.ToSlash(rel))
		if fi.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "root", "wheel"
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// ExtractToHost unpacks a tar stream into destDir. When renameTop is set, the first path component of
// every entry is replaced by it. Every operation goes through an os.Root opened on destDir, so
// neither a symlink in the stream nor a concurrent change on the host can lead a write out of
// destDir, and no write goes through a symlink. Group and other write bits are dropped.
func ExtractToHost(r io.Reader, destDir, renameTop string) error {
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return err
	}
	defer root.Close()
	x := &extractor{root: root, dirs: map[string]bool{".": true}}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := hostEntryPath(hdr.Name, renameTop)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		if err := x.extract(hdr, rel, tr, renameTop); err != nil {
			return err
		}
	}
}

type extractor struct {
	root *os.Root
	dirs map[string]bool // checked to be directories, not symlinks
}

func (x *extractor) extract(hdr *tar.Header, rel string, body io.Reader, renameTop string) error {
	if err := x.mkdirs(path.Dir(rel)); err != nil {
		return err
	}
	mode := fs.FileMode(hdr.Mode) & 0o755
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := x.mkdirs(rel); err != nil {
			return err
		}
		if rel != "." {
			return x.root.Chmod(rel, mode|0o700)
		}
	case tar.TypeReg:
		if err := x.replaceable(rel); err != nil {
			return err
		}
		f, err := x.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, body)
		if err == nil {
			err = f.Chmod(mode)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		x.root.Chtimes(rel, hdr.ModTime, hdr.ModTime)
	case tar.TypeSymlink:
		if err := x.replaceable(rel); err != nil {
			return err
		}
		return x.root.Symlink(hdr.Linkname, rel)
	case tar.TypeLink:
		lrel, err := hostEntryPath(hdr.Linkname, renameTop)
		if err != nil || lrel == "" {
			return fmt.Errorf("refusing hard link %q -> %q", hdr.Name, hdr.Linkname)
		}
		if err := x.mkdirs(path.Dir(lrel)); err != nil {
			return err
		}
		if fi, err := x.root.Lstat(lrel); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("refusing hard link %q -> %q", hdr.Name, hdr.Linkname)
		}
		if err := x.replaceable(rel); err != nil {
			return err
		}
		return x.root.Link(lrel, rel)
	default:
		fmt.Fprintf(os.Stderr, "kido: skipping %q: unsupported entry type %q\n", hdr.Name, hdr.Typeflag)
	}
	return nil
}

// mkdirs makes rel and its parents, refusing to pass through a symlink.
func (x *extractor) mkdirs(rel string) error {
	if x.dirs[rel] {
		return nil
	}
	if err := x.mkdirs(path.Dir(rel)); err != nil {
		return err
	}
	fi, err := x.root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := x.root.Mkdir(rel, 0o755); err != nil {
			return err
		}
	case err != nil:
		return err
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("refusing to write through symlink %q", rel)
	case !fi.IsDir():
		return fmt.Errorf("%q exists and is not a directory", rel)
	}
	x.dirs[rel] = true
	return nil
}

// replaceable removes a file or symlink at rel so that it can be created afresh; directories are
// refused.
func (x *extractor) replaceable(rel string) error {
	fi, err := x.root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("cannot overwrite directory %q with a non-directory", rel)
	}
	return x.root.Remove(rel)
}

func hostEntryPath(name, renameTop string) (string, error) {
	name = strings.TrimPrefix(name, "./")
	if name == "" || name == "." || name == "./" {
		if renameTop != "" {
			return renameTop, nil
		}
		return "", nil
	}
	if !SafeEntryName(name) {
		return "", fmt.Errorf("refusing tar entry %q", name)
	}
	name = path.Clean(name)
	if renameTop != "" {
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = renameTop + name[i:]
		} else {
			name = renameTop
		}
	}
	return name, nil
}
