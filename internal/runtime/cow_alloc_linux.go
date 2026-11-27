//go:build linux

package runtime

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func preallocateCowBitmap(f *os.File, off, length int64) error {
	for {
		err := unix.Fallocate(int(f.Fd()), 0, off, length)
		if err == nil {
			return nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) {
			return writeZeroRegion(f, off, length)
		}
		return err
	}
}
