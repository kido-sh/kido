package runtime

import (
	"os"

	"golang.org/x/sys/unix"
)

func ociPunchHole(f *os.File, off, n int64) error {
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, n)
}
