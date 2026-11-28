package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kido-sh/kido/internal/mount"
	"github.com/kido-sh/kido/internal/notify"

	"golang.org/x/sys/unix"
)

func openat2Confined(dirfd int, rel string, flags, resolve uint64) (int, error) {
	how := &unix.OpenHow{
		Flags:   flags | unix.O_CLOEXEC,
		Resolve: resolve,
	}
	for {
		fd, err := unix.Openat2(dirfd, rel, how)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return fd, err
	}
}

func statBeneath(dirfd int, rel string) (notify.Record, uint64) {
	r := notify.Record{Path: rel}
	if rel == "" || strings.HasPrefix(rel, "/") || strings.IndexByte(rel, 0) >= 0 {
		return r, 0
	}
	parentFD := dirfd
	base := rel
	if idx := strings.LastIndexByte(rel, '/'); idx >= 0 {
		parent := rel[:idx]
		base = rel[idx+1:]
		if parent == "" {
			return r, 0
		}
		fd, err := openat2Confined(
			dirfd,
			parent,
			unix.O_PATH|unix.O_DIRECTORY,
			unix.RESOLVE_BENEATH|unix.RESOLVE_NO_SYMLINKS|unix.RESOLVE_NO_MAGICLINKS,
		)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				r.State = notify.Absent
			}
			return r, 0
		}
		defer unix.Close(fd)
		parentFD = fd
	}
	if base == "" || base == "." || base == ".." {
		return r, 0
	}
	var st unix.Stat_t
	switch err := unix.Fstatat(parentFD, base, &st, unix.AT_SYMLINK_NOFOLLOW); {
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
	return os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
}

func openShareRootNoSymlinks(slashFD int, src string) (int, error) {
	if !filepath.IsAbs(src) {
		return -1, unix.EINVAL
	}
	rel := strings.TrimLeft(src, "/")
	if rel == "" {
		return unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	}
	return openat2Confined(
		slashFD,
		rel,
		unix.O_RDONLY|unix.O_DIRECTORY,
		unix.RESOLVE_NO_SYMLINKS|unix.RESOLVE_NO_MAGICLINKS,
	)
}

func openNotify(shares []mount.Share, warn io.Writer) (*notifyBoot, error) {
	if len(shares) == 0 || os.Getenv("KIDO_NO_NOTIFY") == "1" {
		return nil, nil
	}
	slashFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		fmt.Fprintf(warn, "kido: notifications off: open /: %v\n", err)
		return nil, nil
	}
	defer unix.Close(slashFD)

	nb := &notifyBoot{}
	for _, s := range shares {
		fd, err := openShareRootNoSymlinks(slashFD, s.Source)
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
	if nb.r, nb.w, err = os.Pipe(); err != nil {
		nb.close()
		fmt.Fprintf(warn, "kido: notifications off: %v\n", err)
		return nil, nil
	}
	return nb, nil
}
