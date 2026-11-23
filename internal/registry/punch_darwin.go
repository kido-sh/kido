package registry

import (
	"os"

	"golang.org/x/sys/unix"
)

// punchHole deallocates [off, off+n) of f, which must be block-aligned. F_PUNCHHOLE takes a
// struct fpunchhole {u32 flags, reserved; off_t offset, length}: the leading fields of Fstore_t,
// so FcntlFstore reaches it through libSystem's fcntl.
func punchHole(f *os.File, off, n int64) error {
	return unix.FcntlFstore(f.Fd(), unix.F_PUNCHHOLE, &unix.Fstore_t{Offset: off, Length: n})
}
