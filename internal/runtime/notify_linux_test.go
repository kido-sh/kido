package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/kido-sh/kido/internal/mount"
	"github.com/kido-sh/kido/internal/notify"

	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == NotifyCommand {
		os.Exit(relayProbeLinux(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func realTempDirLinux(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func openRootLinux(t *testing.T, dir string) int {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return fd
}

func shareLinux(t *testing.T, name, src string) mount.Share {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(src, &st); err != nil {
		t.Fatal(err)
	}
	return mount.Share{Name: name, Source: src, Dev: uint64(st.Dev), Ino: st.Ino}
}

func TestStatBeneathLinuxStaysInsideRoot(t *testing.T) {
	base := realTempDirLinux(t)
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	d, saved := filepath.Join(root, "d"), filepath.Join(root, "saved")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(outside, 0o700)
	os.WriteFile(filepath.Join(d, "x"), []byte("inside!"), 0o600)
	os.WriteFile(filepath.Join(outside, "x"), make([]byte, 4242), 0o600)
	fd := openRootLinux(t, root)

	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			os.Rename(d, saved)
			os.Symlink(outside, d)
			os.Remove(d)
			os.Rename(saved, d)
		}
	}()
	inside := 0
	for i := 0; i < 10000; i++ {
		r, _ := statBeneath(fd, "d/x")
		if r.State == notify.Present && r.Size != 7 {
			close(stop)
			t.Fatalf("read file outside root: %+v", r)
		}
		if r.State == notify.Present {
			inside++
		}
	}
	close(stop)
	<-done
	if inside == 0 {
		t.Fatal("never read file inside root")
	}

	if err := os.Rename(d, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, d); err != nil {
		t.Fatal(err)
	}
	if r, _ := statBeneath(fd, "d/x"); r.State != notify.Unknown {
		t.Fatalf("through a symlink: %+v, want unknown", r)
	}
	if r, _ := statBeneath(fd, "d"); r.State == notify.Present && r.Mode&unix.S_IFMT == unix.S_IFDIR {
		t.Fatalf("followed final symlink to directory: %+v", r)
	}
}

func TestStatBeneathLinuxStatesAndEscapes(t *testing.T) {
	base := realTempDirLinux(t)
	root := filepath.Join(base, "root")
	os.MkdirAll(root, 0o700)
	os.WriteFile(filepath.Join(root, "f"), []byte("12345"), 0o640)
	os.WriteFile(filepath.Join(base, "outside"), []byte("x"), 0o600)
	fd := openRootLinux(t, root)

	var st unix.Stat_t
	if err := unix.Lstat(filepath.Join(root, "f"), &st); err != nil {
		t.Fatal(err)
	}
	got, ino := statBeneath(fd, "f")
	if ino != st.Ino {
		t.Fatalf("inode %d, want %d", ino, st.Ino)
	}
	want := notify.Record{
		State: notify.Present, Mode: uint32(st.Mode), Nlink: uint32(st.Nlink), Size: 5,
		MtimeSec: st.Mtim.Sec, MtimeNsec: uint32(st.Mtim.Nsec),
		CtimeSec: st.Ctim.Sec, CtimeNsec: uint32(st.Ctim.Nsec), Path: "f",
	}
	if got != want {
		t.Fatalf("present: got %+v, want %+v", got, want)
	}
	if r, _ := statBeneath(fd, "missing"); r.State != notify.Absent {
		t.Fatalf("missing: %+v", r)
	}
	if r, _ := statBeneath(fd, "f/x"); r.State != notify.Unknown {
		t.Fatalf("below a file: %+v, want unknown", r)
	}
	for _, rel := range []string{"../outside", "../missing", filepath.Join(base, "outside"), "/etc/hosts", ".", ".."} {
		if r, _ := statBeneath(fd, rel); r.State != notify.Unknown {
			t.Errorf("%s: %+v, want unknown", rel, r)
		}
	}
}

func TestOpenNotifyLinux(t *testing.T) {
	a, b := realTempDirLinux(t), realTempDirLinux(t)
	var warn bytes.Buffer
	if nb, err := openNotify(nil, &warn); nb != nil || err != nil {
		t.Fatalf("no mounts: %v, %v", nb, err)
	}
	shares := []mount.Share{shareLinux(t, "m0", a), shareLinux(t, "m1", b)}
	t.Setenv("KIDO_NO_NOTIFY", "1")
	if nb, err := openNotify(shares, &warn); nb != nil || err != nil {
		t.Fatalf("KIDO_NO_NOTIFY=1: %v, %v", nb, err)
	}
	t.Setenv("KIDO_NO_NOTIFY", "0")
	nb, err := openNotify(shares, &warn)
	if err != nil || nb == nil {
		t.Fatalf("openNotify: %v, %v (%s)", nb, err, warn.String())
	}
	defer nb.close()
	if len(nb.roots) != 2 || nb.r == nil || nb.w == nil {
		t.Fatalf("roots %d, pipe %v %v", len(nb.roots), nb.r, nb.w)
	}
	got := strings.Join(nb.vmmArgs(3), " ")
	if want := "--notify-fd 3 --watch " + a + " --watch " + b; got != want {
		t.Fatalf("vmmArgs = %q, want %q", got, want)
	}
	nb.close()
	nb.close()

	moved := a + "-moved"
	if err := os.Rename(a, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, a); err != nil {
		t.Fatal(err)
	}
	if nb, err := openNotify(shares, &warn); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		nb.close()
		t.Fatalf("share root replaced by symlink: %v", err)
	}

	swapped := shareLinux(t, "m1", b)
	if err := os.Rename(b, b+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b, 0o700); err != nil {
		t.Fatal(err)
	}
	if nb, err := openNotify([]mount.Share{swapped}, &warn); err == nil || !strings.Contains(err.Error(), "another directory") {
		nb.close()
		t.Fatalf("share root replaced by another directory: %v", err)
	}
}

type fdInfoLinux struct {
	FD  int
	Dev uint64
	Ino uint64
}

type relayReportLinux struct {
	Args []string
	Pid  int
	Sid  int
	FDs  []fdInfoLinux
}

func relayProbeLinux(args []string) int {
	rep := relayReportLinux{Args: args, Pid: os.Getpid()}
	rep.Sid, _ = unix.Getsid(0)
	for fd := 0; fd < 64; fd++ {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil {
			rep.FDs = append(rep.FDs, fdInfoLinux{FD: fd, Dev: uint64(st.Dev), Ino: st.Ino})
		}
	}
	if json.NewEncoder(os.Stdout).Encode(rep) != nil {
		return 1
	}
	return 0
}

func TestStartRelayLinux(t *testing.T) {
	dir := realTempDirLinux(t)
	a, b := realTempDirLinux(t), realTempDirLinux(t)
	nb, err := openNotify([]mount.Share{shareLinux(t, "m0", a), shareLinux(t, "m1", b)}, io.Discard)
	if err != nil || nb == nil {
		t.Fatalf("openNotify: %v", err)
	}
	stat := func(f *os.File) unix.Stat_t {
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	in := stat(nb.r)
	roots := []unix.Stat_t{stat(nb.roots[0]), stat(nb.roots[1])}
	shares := nb.shares
	var warn bytes.Buffer
	(&Runtime{Home: dir}).startRelay("c1", dir, nb, &warn)
	if nb.r != nil || nb.w != nil || nb.roots != nil {
		t.Fatal("startRelay kept kido's copies of descriptors")
	}
	var rep relayReportLinux
	waitFor(t, "relay report", func() bool {
		data, _ := os.ReadFile(filepath.Join(dir, "notify.log"))
		return json.Unmarshal(data, &rep) == nil
	})
	if want := []string{"c1", "m0=" + shares[0].Root, "m1=" + shares[1].Root}; !slices.Equal(rep.Args, want) {
		t.Errorf("args %q, want %q", rep.Args, want)
	}
	if rep.Sid != rep.Pid {
		t.Errorf("relay not session leader: sid %d pid %d", rep.Sid, rep.Pid)
	}
	logSt, err := os.Stat(filepath.Join(dir, "notify.log"))
	if err != nil {
		t.Fatal(err)
	}
	fds := map[int]fdInfoLinux{}
	for _, f := range rep.FDs {
		fds[f.FD] = f
	}
	is := func(fd int, st unix.Stat_t) bool { return fds[fd].Dev == uint64(st.Dev) && fds[fd].Ino == st.Ino }
	if !is(0, in) || !is(3, roots[0]) || !is(4, roots[1]) {
		t.Errorf("unexpected fds: %+v", fds)
	}
	logIno := logSt.Sys().(*syscall.Stat_t).Ino
	if fds[1].Ino != logIno || fds[2].Ino != logIno {
		t.Error("stdout/stderr not notify.log")
	}
	waitFor(t, "reap relay", func() bool { return unix.Kill(rep.Pid, 0) == unix.ESRCH })
}
