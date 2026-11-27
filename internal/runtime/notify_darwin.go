package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"unsafe"

	"github.com/kido-sh/kido/internal/mount"
	"github.com/kido-sh/kido/internal/notify"

	"golang.org/x/sys/unix"
)

const (
	atSymlinkNofollowAny = 0x0800
	atResolveBeneath     = 0x2000
)

func statBeneath(dirfd int, rel string) (notify.Record, uint64) {
	r := notify.Record{Path: rel}
	var st unix.Stat_t
	switch err := unix.Fstatat(dirfd, rel, &st, atSymlinkNofollowAny|atResolveBeneath); {
	case err == nil:
		r.State = notify.Present
		r.Mode, r.Nlink, r.Size = uint32(st.Mode), uint32(st.Nlink), uint64(st.Size)
		r.MtimeSec, r.MtimeNsec = st.Mtim.Sec, uint32(st.Mtim.Nsec)
		r.CtimeSec, r.CtimeNsec = st.Ctim.Sec, uint32(st.Ctim.Nsec)
		return r, st.Ino
	case errors.Is(err, unix.ENOENT):
		r.State = notify.Absent
	}
	return r, 0
}

func fdPath(fd int) (string, error) {
	buf := make([]byte, unix.PathMax)
	if _, _, e := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		return "", e
	}
	return unix.ByteSliceToString(buf), nil
}

func openNotify(shares []mount.Share, warn io.Writer) (*notifyBoot, error) {
	if len(shares) == 0 || os.Getenv("KIDO_NO_NOTIFY") == "1" {
		return nil, nil
	}
	nb := &notifyBoot{}
	for _, s := range shares {
		fd, err := unix.Open(s.Source, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW_ANY|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ELOOP) {
			nb.close()
			return nil, fmt.Errorf("mount %s: the path now leads through a symbolic link; refusing to follow it", s.Source)
		}
		var root string
		var st unix.Stat_t
		if err == nil {
			nb.roots = append(nb.roots, os.NewFile(uintptr(fd), s.Source))
			err = unix.Fstat(fd, &st)
		}
		if err == nil && (uint64(st.Dev) != s.Dev || st.Ino != s.Ino) {
			nb.close()
			return nil, fmt.Errorf("mount %s: the path now leads to another directory; refusing to follow it", s.Source)
		}
		if err == nil {
			root, err = fdPath(fd)
		}
		if err != nil {
			nb.close()
			fmt.Fprintf(warn, "kido: notifications off: %s: %v\n", s.Source, err)
			return nil, nil
		}
		nb.shares = append(nb.shares, NotifyShare{Name: s.Name, Root: root})
	}
	var err error
	if nb.r, nb.w, err = os.Pipe(); err != nil {
		nb.close()
		fmt.Fprintf(warn, "kido: notifications off: %v\n", err)
		return nil, nil
	}
	return nb, nil
}
