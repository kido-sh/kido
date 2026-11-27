package runtime

import (
	"os"

	"golang.org/x/sys/unix"
)

func cloneFile(src, dst string) error {
	fd, err := unix.Open(src, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: src, Err: err}
	}
	in := os.NewFile(uintptr(fd), src)
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		if err := copySparse(out, in); err != nil {
			out.Close()
			os.Remove(dst)
			return err
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}
