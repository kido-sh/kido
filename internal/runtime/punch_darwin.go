package runtime

import (
	"os"

	"golang.org/x/sys/unix"
)

func ociPunchHole(f *os.File, off, n int64) error {
	return unix.FcntlFstore(f.Fd(), unix.F_PUNCHHOLE, &unix.Fstore_t{Offset: off, Length: n})
}
