package runtime

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/kido-sh/kido/internal/engine"
)

// GuestPath validates an absolute path inside the container and cleans it.
func GuestPath(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("invalid container path %q", p)
	}
	if !path.IsAbs(p) {
		return "", fmt.Errorf("container path %q must be absolute", p)
	}
	return path.Clean(p), nil
}

// SafeEntryName reports whether a tar entry name stays inside the directory it is unpacked in.
func SafeEntryName(name string) bool {
	if name == "" || strings.ContainsRune(name, 0) || path.IsAbs(name) {
		return false
	}
	c := path.Clean(name)
	return c != ".." && !strings.HasPrefix(c, "../")
}

func (r *Runtime) guestRun(ctx context.Context, ref string, args []string, stdin io.Reader, stdout io.Writer) error {
	stderr := &tailBuffer{max: 64 << 10}
	status, err := r.Exec(ctx, ref, engine.ProcessSpec{Args: args}, engine.Stdio{Stdin: stdin, Stdout: stdout, Stderr: stderr})
	if err != nil {
		return err
	}
	if status != 0 {
		msg := Sanitize(strings.TrimSpace(stderr.String()))
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", status)
		}
		return fmt.Errorf("%s: %s", args[0], msg)
	}
	return nil
}

// GuestIsDir reports whether p is a directory in the container.
func (r *Runtime) GuestIsDir(ctx context.Context, ref, p string) (bool, error) {
	p, err := GuestPath(p)
	if err != nil {
		return false, err
	}
	status, err := r.Exec(ctx, ref, engine.ProcessSpec{Args: []string{"/bin/test", "-d", p}}, engine.Stdio{})
	if err != nil {
		return false, err
	}
	return status == 0, nil
}

func (r *Runtime) CopyIn(ctx context.Context, ref string, tarStream io.Reader, dstDir string) error {
	dst, err := GuestPath(dstDir)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	sent := make(chan error, 1)
	go func() {
		err := filterTar(tarStream, pw)
		pw.CloseWithError(err)
		sent <- err
	}()
	err = r.guestRun(ctx, ref, []string{"/bin/tar", "-x", "-f", "-", "-C", dst}, pr, io.Discard)
	pr.CloseWithError(errors.New("copy finished"))
	if serr := <-sent; err == nil && serr != nil {
		return fmt.Errorf("the guest tar finished before the whole archive was sent: %w", serr)
	}
	return err
}

// filterTar re-encodes a tar stream, refusing entries that would land outside the target directory.
func filterTar(in io.Reader, out io.Writer) error {
	tr := tar.NewReader(in)
	tw := tar.NewWriter(out)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return tw.Close()
		}
		if err != nil {
			return err
		}
		if !SafeEntryName(hdr.Name) {
			return fmt.Errorf("refusing tar entry %q", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeLink && !SafeEntryName(hdr.Linkname) {
			return fmt.Errorf("refusing hard link %q -> %q", hdr.Name, hdr.Linkname)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

func (r *Runtime) CopyOut(ctx context.Context, ref string, srcPath string, w io.Writer) error {
	src, err := GuestPath(srcPath)
	if err != nil {
		return err
	}
	dir, base := path.Split(src)
	if src == "/" {
		dir, base = "/", "."
	} else {
		base = "./" + base
	}
	return r.guestRun(ctx, ref, []string{"/bin/tar", "-c", "-f", "-", "-C", dir, base}, nil, w)
}
