package build

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
)

// Context is a build context directory. Every read goes through an os.Root, so neither ".." nor a
// symlink, even one swapped in during the build, reaches a file outside it.
type Context struct {
	root   *os.Root
	ignore *ignoreMatcher
}

const maxContextFile = 4 << 20

func OpenContext(dir string) (*Context, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("build context: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("build context %s is not a directory", dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("build context: %w", err)
	}
	c := &Context{root: root}
	data, err := c.ReadFile(".dockerignore")
	switch {
	case err == nil:
		if c.ignore, err = parseIgnore(bytes.NewReader(data)); err != nil {
			root.Close()
			return nil, err
		}
	case !errors.Is(err, fs.ErrNotExist):
		root.Close()
		return nil, fmt.Errorf(".dockerignore: %w", err)
	}
	return c, nil
}

func (c *Context) Close() error { return c.root.Close() }

// ReadFile reads a regular file of the context, such as .dockerignore or the default Dockerfile.
// A FIFO or a device fails instead of blocking the build.
func (c *Context) ReadFile(name string) ([]byte, error) {
	f, err := c.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxContextFile {
		return nil, fmt.Errorf("%s: not a regular file of at most %d bytes", name, maxContextFile)
	}
	return io.ReadAll(io.LimitReader(f, maxContextFile))
}

// source is one COPY or ADD source, resolved in the context.
type source struct {
	rel      string      // slash-separated path in the context with every symlink resolved, "." for its root
	base     string      // the last element of the path as written, which names a copied file
	info     fs.FileInfo // what rel is
	excluded bool        // an ignored directory, which provides only the files "!" patterns re-include
}

var errEscape = errors.New("a symlink leads out of the build context")

// cleanSource refuses absolute paths and "..", and returns src relative to the context root.
func cleanSource(src string) (string, error) {
	if src == "" || strings.ContainsRune(src, 0) {
		return "", fmt.Errorf("invalid source path %q", src)
	}
	if path.IsAbs(src) {
		return "", fmt.Errorf("%s: absolute source paths are not allowed, sources are relative to the build context", src)
	}
	for _, el := range strings.Split(src, "/") {
		if el == ".." {
			return "", fmt.Errorf("%s: \"..\" is not allowed in a source path, it would leave the build context", src)
		}
	}
	return path.Clean(src), nil
}

func hasMeta(p string) bool { return strings.ContainsAny(p, "*?[") }

// resolve expands one COPY or ADD source, with Go's path.Match wildcards, into the files and
// directories it names. A wildcard may match nothing, as in COPY go.mod go.sum* ./.
func (c *Context) resolve(src string) ([]source, error) {
	rel, err := cleanSource(src)
	if err != nil {
		return nil, err
	}
	if !hasMeta(rel) {
		s, err := c.stat(rel)
		if err != nil {
			return nil, err
		}
		return []source{s}, nil
	}
	if _, err := path.Match(rel, ""); err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	depth := strings.Count(rel, "/")
	var out []source
	err = fs.WalkDir(c.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if c.ignore.excluded(p) {
			if d.IsDir() && !c.ignore.hasExclusions {
				return fs.SkipDir
			}
		} else if ok, _ := path.Match(rel, p); ok {
			s, err := c.stat(p)
			if err != nil {
				return err
			}
			out = append(out, s)
		}
		if d.IsDir() && strings.Count(p, "/") >= depth {
			return fs.SkipDir
		}
		return nil
	})
	return out, err
}

// realpath resolves every symlink in rel inside the context, so that .dockerignore applies to the
// file a path really names, and an ignored file is never sent.
func (c *Context) realpath(rel string) (string, error) {
	var done []string
	todo := strings.Split(rel, "/")
	for hops := 0; len(todo) > 0; {
		el := todo[0]
		todo = todo[1:]
		switch el {
		case "", ".":
			continue
		case "..":
			if len(done) == 0 {
				return "", errEscape
			}
			done = done[:len(done)-1]
			continue
		}
		p := path.Join(append(done, el)...)
		fi, err := c.root.Lstat(p)
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			done = append(done, el)
			continue
		}
		if hops++; hops > 40 {
			return "", errors.New("too many levels of symbolic links")
		}
		target, err := c.root.Readlink(p)
		if err != nil {
			return "", err
		}
		if path.IsAbs(target) {
			return "", errEscape
		}
		todo = append(strings.Split(target, "/"), todo...)
	}
	if len(done) == 0 {
		return ".", nil
	}
	return path.Join(done...), nil
}

func (c *Context) stat(rel string) (source, error) {
	real, err := c.realpath(rel)
	var fi fs.FileInfo
	if err == nil {
		fi, err = c.root.Stat(real)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return source{}, fmt.Errorf("%s: not found in the build context", rel)
	case errors.Is(err, errEscape), err != nil && strings.Contains(err.Error(), "path escapes from parent"):
		return source{}, fmt.Errorf("%s: %w", rel, errEscape)
	case err != nil:
		return source{}, fmt.Errorf("%s: %w", rel, err)
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return source{}, fmt.Errorf("%s: not a regular file or directory", rel)
	}
	s := source{rel: real, base: path.Base(rel), info: fi}
	if c.ignore.excluded(rel) || c.ignore.excluded(real) {
		if !fi.IsDir() || !c.ignore.hasExclusions {
			return source{}, fmt.Errorf("%s: excluded by .dockerignore", rel)
		}
		s.excluded = true
	}
	return s, nil
}

// entry is one member of a COPY archive.
type entry struct {
	rel  string // path in the context
	name string // path in the archive, relative to the destination directory
	info fs.FileInfo
	link string // symlink target, copied as is and never followed
}

// entries lists what copying s puts in the archive: a file under name, or the contents of a
// directory (not the directory itself) under names relative to it. Symlinks inside a directory
// are copied as symlinks.
func (c *Context) entries(s source, name string) ([]entry, error) {
	if !s.info.IsDir() {
		return []entry{{rel: s.rel, name: name, info: s.info}}, nil
	}
	var out []entry
	dirs := map[string]bool{}
	err := fs.WalkDir(c.root.FS(), s.rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == s.rel {
			return nil
		}
		if c.ignore.excluded(p) {
			if d.IsDir() && !c.ignore.hasExclusions {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		name := p
		if s.rel != "." {
			name = p[len(s.rel)+1:]
		}
		// A file re-included by "!" under an excluded directory still needs its parents.
		for i := 0; i < len(name); i++ {
			if name[i] != '/' || dirs[name[:i]] {
				continue
			}
			pi, err := c.root.Lstat(path.Join(s.rel, name[:i]))
			if err != nil {
				return err
			}
			out = append(out, entry{rel: path.Join(s.rel, name[:i]), name: name[:i], info: pi})
			dirs[name[:i]] = true
		}
		e := entry{rel: p, name: name, info: info}
		m := info.Mode()
		switch {
		case m.IsDir():
			dirs[name] = true
		case m&fs.ModeSymlink != 0:
			if e.link, err = c.root.Readlink(p); err != nil {
				return err
			}
		case m&fs.ModeSocket != 0:
			return nil
		case !m.IsRegular():
			return fmt.Errorf("%s: unsupported file type %s", p, m.Type())
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.rel, err)
	}
	if s.excluded && len(out) == 0 {
		return nil, fmt.Errorf("%s: excluded by .dockerignore", s.rel)
	}
	return out, nil
}

func tarMode(m fs.FileMode) int64 {
	mode := int64(m.Perm())
	if m&fs.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		mode |= 0o1000
	}
	return mode
}

// writeTar writes entries as a tar stream owned by 0:0. chmod, when not negative, replaces the
// mode of every file and directory.
func (c *Context) writeTar(w io.Writer, entries []entry, chmod int64) error {
	tw := tar.NewWriter(w)
	for _, e := range entries {
		if err := c.writeEntry(tw, e, chmod); err != nil {
			return err
		}
	}
	return tw.Close()
}

func (c *Context) writeEntry(tw *tar.Writer, e entry, chmod int64) error {
	m := e.info.Mode()
	hdr := &tar.Header{Name: e.name, ModTime: e.info.ModTime(), Mode: tarMode(m)}
	if chmod >= 0 {
		hdr.Mode = chmod
	}
	switch {
	case m.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name += "/"
		return tw.WriteHeader(hdr)
	case m&fs.ModeSymlink != 0:
		hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, e.link, 0o777
		return tw.WriteHeader(hdr)
	}
	// O_NONBLOCK: a file swapped for a FIFO must not hang the build; it fails the check below.
	f, err := c.root.OpenFile(e.rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: no longer a regular file", e.rel)
	}
	hdr.Typeflag, hdr.Size = tar.TypeReg, fi.Size()
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
		return fmt.Errorf("%s: %w", e.rel, err)
	}
	return nil
}
