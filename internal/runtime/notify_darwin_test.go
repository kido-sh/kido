package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/mount"
	"github.com/kido-sh/kido/internal/notify"

	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == NotifyCommand {
		os.Exit(relayProbe(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func realTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func openRoot(t *testing.T, dir string) int {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW_ANY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return fd
}

func TestStatBeneathStaysInsideTheRoot(t *testing.T) {
	base := realTempDir(t)
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	d, saved := filepath.Join(root, "d"), filepath.Join(root, "saved")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(outside, 0o700)
	os.WriteFile(filepath.Join(d, "x"), []byte("inside!"), 0o600)
	os.WriteFile(filepath.Join(outside, "x"), make([]byte, 4242), 0o600)
	fd := openRoot(t, root)

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
	for i := 0; i < 20000; i++ {
		r, _ := statBeneath(fd, "d/x")
		if r.State == notify.Present && r.Size != 7 {
			close(stop)
			t.Fatalf("read the file outside the root: %+v", r)
		}
		if r.State == notify.Present {
			inside++
		}
	}
	close(stop)
	<-done
	if inside == 0 {
		t.Fatal("never read the file inside the root")
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
		t.Fatalf("followed a final symlink to a directory: %+v", r)
	}
}

func TestStatBeneathStates(t *testing.T) {
	root := realTempDir(t)
	os.WriteFile(filepath.Join(root, "f"), []byte("12345"), 0o640)
	fd := openRoot(t, root)
	var st unix.Stat_t
	if err := unix.Lstat(filepath.Join(root, "f"), &st); err != nil {
		t.Fatal(err)
	}
	got, ino := statBeneath(fd, "f")
	if ino != st.Ino {
		t.Fatalf("inode %d, want %d", ino, st.Ino)
	}
	want := notify.Record{State: notify.Present, Mode: uint32(st.Mode), Nlink: uint32(st.Nlink), Size: 5,
		MtimeSec: st.Mtim.Sec, MtimeNsec: uint32(st.Mtim.Nsec), CtimeSec: st.Ctim.Sec, CtimeNsec: uint32(st.Ctim.Nsec), Path: "f"}
	if got != want {
		t.Fatalf("present: got %+v, want %+v", got, want)
	}
	if r, _ := statBeneath(fd, "missing"); r.State != notify.Absent {
		t.Fatalf("missing: %+v", r)
	}
	if r, _ := statBeneath(fd, "f/x"); r.State != notify.Unknown {
		t.Fatalf("below a file: %+v, want unknown", r)
	}
}

func TestStatBeneathRefusesEscapes(t *testing.T) {
	base := realTempDir(t)
	root := filepath.Join(base, "root")
	os.MkdirAll(root, 0o700)
	os.WriteFile(filepath.Join(base, "outside"), []byte("x"), 0o600)
	fd := openRoot(t, root)
	for _, rel := range []string{"../outside", "../missing", filepath.Join(base, "outside"), "/etc/hosts"} {
		if r, _ := statBeneath(fd, rel); r.State != notify.Unknown {
			t.Errorf("%s: %+v, want unknown", rel, r)
		}
	}
}

func TestRelayFramesBatch(t *testing.T) {
	root := realTempDir(t)
	r := testRelay(root)
	r.shares[0].FD = openRoot(t, root)
	want := map[string]notify.State{}
	for i := 0; i < 3000; i++ {
		name := fmt.Sprintf("file-with-a-longish-name-%04d", i)
		if i%3 == 0 {
			os.WriteFile(filepath.Join(root, name), nil, 0o600)
			want[name] = notify.Present
		} else {
			want[name] = notify.Absent
		}
		r.add(notify.Event{Path: root + "/" + name})
		r.add(notify.Event{Path: root + "/" + name})
	}
	frames, _ := r.frames()
	if len(frames) < 2 {
		t.Fatalf("%d frames for %d records", len(frames), len(want))
	}
	got := map[string]notify.State{}
	for _, f := range frames {
		share, batch, err := notify.DecodeFrame(f)
		if err != nil || share != "m0" || len(batch) > notify.MaxBatch {
			t.Fatalf("frame: share %q, %d bytes, %v", share, len(batch), err)
		}
		recs, err := notify.ParseBatch(batch)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range recs {
			if _, dup := got[rec.Path]; dup {
				t.Fatalf("%s sent twice", rec.Path)
			}
			got[rec.Path] = rec.State
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for p, s := range want {
		if got[p] != s {
			t.Fatalf("%s: state %d, want %d", p, got[p], s)
		}
	}
	if again, _ := r.frames(); len(again) != 0 {
		t.Fatalf("%d frames sent twice", len(again))
	}
}

func TestRelayFramesMarks(t *testing.T) {
	root := realTempDir(t)
	r := testRelay(root)
	r.shares[0].FD = openRoot(t, root)
	os.WriteFile(filepath.Join(root, "plain"), nil, 0o600)
	os.WriteFile(filepath.Join(root, "new"), nil, 0o600)
	os.MkdirAll(filepath.Join(root, "d"), 0o700)
	r.add(notify.Event{Path: root + "/plain", Flags: 0x1000})
	r.add(notify.Event{Path: root + "/new", Flags: notify.FlagItemCreated})
	r.add(notify.Event{Path: root + strings.Repeat("/d", notify.MaxDepth+1)})
	frames, _ := r.frames()
	if len(frames) != 1 {
		t.Fatalf("%d frames", len(frames))
	}
	_, recs := shareOf(frames[0])
	got := map[string]notify.Record{}
	for _, rec := range recs {
		got[rec.Path] = rec
	}
	cut := strings.TrimPrefix(strings.Repeat("/d", notify.MaxDepth), "/")
	if rec := got["plain"]; rec.State != notify.Present || rec.NameChanged {
		t.Errorf("a modified file: %+v", rec)
	}
	if rec := got["new"]; rec.State != notify.Present || !rec.NameChanged {
		t.Errorf("a created file: %+v", rec)
	}
	if rec := got[cut]; rec.State != notify.Unknown || !rec.NameChanged {
		t.Errorf("a path past the depth limit: %+v", rec)
	}
}

func TestRelayJudgesNames(t *testing.T) {
	root := realTempDir(t)
	r := testRelay(root)
	r.shares[0].FD = openRoot(t, root)
	ino := func(name string) uint64 {
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(root, name), &st); err != nil {
			t.Fatal(err)
		}
		return st.Ino
	}
	send := func(evs ...notify.Event) notify.Record {
		t.Helper()
		for _, ev := range evs {
			ev.Path = root + "/" + ev.Path
			r.add(ev)
		}
		frames, _ := r.frames()
		if len(frames) != 1 {
			t.Fatalf("%d frames", len(frames))
		}
		_, recs := shareOf(frames[0])
		if len(recs) != 1 {
			t.Fatalf("%d records", len(recs))
		}
		return recs[0]
	}
	created := notify.FlagItemCreated | 0x10000
	os.WriteFile(filepath.Join(root, "f"), []byte("1"), 0o600)
	if rec := send(notify.Event{Path: "f", Flags: created, FileID: ino("f")}); !rec.NameChanged || rec.Replaced {
		t.Fatalf("a new file: %+v", rec)
	}
	os.WriteFile(filepath.Join(root, "f"), []byte("12"), 0o600)
	if rec := send(notify.Event{Path: "f", Flags: created | 0x1000, FileID: ino("f")}); rec.NameChanged || rec.Replaced {
		t.Fatalf("a rewrite whose event kept ItemCreated: %+v", rec)
	}
	old := ino("f")
	os.WriteFile(filepath.Join(root, "f.tmp"), []byte("new"), 0o600)
	os.Rename(filepath.Join(root, "f.tmp"), filepath.Join(root, "f"))
	if rec := send(notify.Event{Path: "f", Flags: notify.FlagItemRenamed, FileID: ino("f")}); !rec.Replaced || !rec.NameChanged {
		t.Fatalf("a replace the relay saw before: %+v", rec)
	}
	os.Remove(filepath.Join(root, "f"))
	if rec := send(notify.Event{Path: "f", Flags: notify.FlagItemRemoved}); !rec.NameChanged || rec.State != notify.Absent {
		t.Fatalf("a removal: %+v", rec)
	}
	if rec := send(notify.Event{Path: "f", Flags: notify.FlagItemRemoved}); rec.NameChanged {
		t.Fatalf("still absent: %+v", rec)
	}

	os.WriteFile(filepath.Join(root, "g"), []byte("1"), 0o600)
	before := ino("g")
	os.WriteFile(filepath.Join(root, "g.tmp"), []byte("2"), 0o600)
	os.Rename(filepath.Join(root, "g.tmp"), filepath.Join(root, "g"))
	rec := send(notify.Event{Path: "g", Flags: notify.FlagItemRenamed, FileID: ino("g")},
		notify.Event{Path: "g", Flags: notify.FlagItemRemoved | created, FileID: before})
	if !rec.Replaced || !rec.NameChanged {
		t.Fatalf("a replace seen first through its events' file IDs: %+v", rec)
	}
	if old == before {
		t.Fatal("inodes were reused; the test proves nothing")
	}

	p := &r.pend[0]
	for i := 0; i < notifyMaxKnown+5; i++ {
		p.known[fmt.Sprint(i)] = pathState{}
		if len(p.known) > notifyMaxKnown {
			p.judge(&notify.Record{Path: "x", State: notify.Absent}, 0, false)
		}
	}
	if len(p.known) > notifyMaxKnown+1 {
		t.Fatalf("the relay remembers %d paths", len(p.known))
	}
}

func TestRelayRunReconnectsAndFlushes(t *testing.T) {
	root := realTempDir(t)
	r := testRelay(root, root+"/other")
	r.shares[0].FD, r.shares[1].FD = openRoot(t, root), openRoot(t, root)
	r.keepalive = 20 * time.Millisecond
	sessions := make(chan *fakeSession, 4)
	first, second := newFakeSession(), newFakeSession()
	sessions <- first
	sessions <- second
	var dials sync.WaitGroup
	dials.Add(2)
	r.dial = func(context.Context) (notifySession, error) {
		s := <-sessions
		dials.Done()
		return s, nil
	}
	events, readErr := make(chan notify.Event), make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- r.run(ctx, events, readErr) }()

	events <- notify.Event{Path: root + "/a"}
	waitFor(t, "the first batch", func() bool { return len(first.got()) > 0 })
	if share, recs := shareOf(first.got()[0]); share != "m0" || len(recs) != 1 || recs[0].Path != "a" {
		t.Fatalf("first frame: %s %+v", share, recs)
	}
	first.mu.Lock()
	first.fail = errors.New("broken pipe")
	first.mu.Unlock()
	events <- notify.Event{Path: root + "/b"}
	dials.Wait()
	waitFor(t, "flushes on the new session", func() bool {
		flushed := map[string]bool{}
		for _, f := range second.got() {
			if share, recs := shareOf(f); len(recs) == 1 && recs[0].Flush {
				flushed[share] = true
			}
		}
		return flushed["m0"] && flushed["m1"]
	})
	waitFor(t, "a keepalive", func() bool {
		for _, f := range second.got() {
			if len(f) == 0 {
				return true
			}
		}
		return false
	})
	close(events)
	readErr <- nil
	if err := <-result; err != nil {
		t.Fatalf("run after the events ended: %v", err)
	}
	if log := r.log.w.(*syncBuffer).String(); strings.Count(log, "notifications on") != 2 {
		t.Fatalf("log does not say notifications were on for each session:\n%s", log)
	}
}

func share(t *testing.T, name, src string) mount.Share {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(src, &st); err != nil {
		t.Fatal(err)
	}
	return mount.Share{Name: name, Source: src, Dev: uint64(st.Dev), Ino: st.Ino}
}

func TestOpenNotify(t *testing.T) {
	a, b := realTempDir(t), realTempDir(t)
	var warn bytes.Buffer
	if nb, err := openNotify(nil, &warn); nb != nil || err != nil {
		t.Fatalf("no mounts: %v, %v", nb, err)
	}
	shares := []mount.Share{share(t, "m0", a), share(t, "m1", b)}
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
	for i, f := range nb.roots {
		var st, want unix.Stat_t
		unix.Fstat(int(f.Fd()), &st)
		unix.Stat([]string{a, b}[i], &want)
		if st.Ino != want.Ino || st.Dev != want.Dev {
			t.Fatalf("root %d is not the share's directory", i)
		}
	}
	got := strings.Join(nb.vmmArgs(4), " ")
	if want := "--notify-fd 4 --watch " + a + " --watch " + b; got != want {
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
		t.Fatalf("a share root replaced by a symlink: %v", err)
	}
	os.MkdirAll(filepath.Join(moved, "sub"), 0o700)
	below := share(t, "m0", filepath.Join(moved, "sub"))
	below.Source = filepath.Join(a, "sub")
	if nb, err := openNotify([]mount.Share{below}, &warn); err == nil {
		nb.close()
		t.Fatal("a share root below a symlink was opened")
	}

	swapped := share(t, "m1", b)
	if err := os.Rename(b, b+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b, 0o700); err != nil {
		t.Fatal(err)
	}
	if nb, err := openNotify([]mount.Share{swapped}, &warn); err == nil || !strings.Contains(err.Error(), "another directory") {
		nb.close()
		t.Fatalf("a share root replaced by another directory: %v", err)
	}
}

func TestOpenNotifyRootSpelling(t *testing.T) {
	base := realTempDir(t)
	stored := filepath.Join(base, "Proj\u00e9t")
	if err := os.Mkdir(stored, 0o700); err != nil {
		t.Fatal(err)
	}
	typed := filepath.Join(base, "proje\u0301t")
	if _, err := os.Stat(typed); err != nil {
		t.Skipf("the volume is case or normalization sensitive: %v", err)
	}
	nb, err := openNotify([]mount.Share{share(t, "m0", typed)}, io.Discard)
	if err != nil || nb == nil {
		t.Fatalf("openNotify: %v", err)
	}
	defer nb.close()
	if nb.shares[0].Root != stored {
		t.Fatalf("root %q, want the stored spelling %q", nb.shares[0].Root, stored)
	}
}

type fdInfo struct {
	FD   int
	Dev  uint64
	Ino  uint64
	Mode uint32
}

type relayReport struct {
	Args []string
	Env  []string
	Pid  int
	Sid  int
	Dir  string
	FDs  []fdInfo
}

func relayProbe(args []string) int {
	rep := relayReport{Args: args, Env: os.Environ(), Pid: os.Getpid()}
	rep.Sid, _ = unix.Getsid(0)
	rep.Dir, _ = os.Getwd()
	for fd := 0; fd < 256; fd++ {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil {
			rep.FDs = append(rep.FDs, fdInfo{FD: fd, Dev: uint64(st.Dev), Ino: st.Ino, Mode: uint32(st.Mode)})
		}
	}
	if json.NewEncoder(os.Stdout).Encode(rep) != nil {
		return 1
	}
	return 0
}

func TestStartRelay(t *testing.T) {
	t.Setenv("KIDO_TEST_SECRET", "must not reach the relay")
	t.Run("environment", func(t *testing.T) {
		dir := realTempDir(t)
		t.Setenv("HOME", dir)
		rep := startProbe(t, dir)
		if !slices.Contains(rep.Env, "HOME="+dir) {
			t.Errorf("the relay did not get HOME: %q", rep.Env)
		}
	})
	t.Run("no environment", func(t *testing.T) {
		for _, k := range notifyEnv {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
		if rep := startProbe(t, realTempDir(t)); len(rep.Env) != 0 {
			t.Errorf("the relay got %q", rep.Env)
		}
	})
}

func startProbe(t *testing.T, dir string) relayReport {
	t.Helper()
	a, b := realTempDir(t), realTempDir(t)
	nb, err := openNotify([]mount.Share{share(t, "m0", a), share(t, "m1", b)}, io.Discard)
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
	in, out := stat(nb.r), stat(nb.w)
	roots := []unix.Stat_t{stat(nb.roots[0]), stat(nb.roots[1])}
	shares := nb.shares
	var warn bytes.Buffer
	(&Runtime{Home: dir}).startRelay("c1", dir, nb, &warn)
	if nb.r != nil || nb.w != nil || nb.roots != nil {
		t.Fatal("startRelay kept kido's copies of the descriptors")
	}
	var rep relayReport
	waitFor(t, "the relay's report", func() bool {
		data, _ := os.ReadFile(filepath.Join(dir, "notify.log"))
		return json.Unmarshal(data, &rep) == nil
	})
	if want := []string{"c1", "m0=" + shares[0].Root, "m1=" + shares[1].Root}; !slices.Equal(rep.Args, want) {
		t.Errorf("args %q, want %q", rep.Args, want)
	}
	for _, kv := range rep.Env {
		if k, _, _ := strings.Cut(kv, "="); !slices.Contains(notifyEnv, k) {
			t.Errorf("the relay got %s", k)
		}
	}
	if rep.Sid != rep.Pid {
		t.Errorf("the relay is not a session leader: sid %d pid %d", rep.Sid, rep.Pid)
	}
	if fi, err := os.Stat(rep.Dir); err != nil || !sameDir(t, fi, dir) {
		t.Errorf("the relay runs in %q, want %q", rep.Dir, dir)
	}
	logSt, err := os.Stat(filepath.Join(dir, "notify.log"))
	if err != nil {
		t.Fatal(err)
	}
	fds := map[int]fdInfo{}
	for _, f := range rep.FDs {
		fds[f.FD] = f
		if f.Dev == uint64(out.Dev) && f.Ino == out.Ino {
			t.Errorf("the relay got the pipe's write end at %d", f.FD)
		}
	}
	is := func(fd int, st unix.Stat_t) bool { return fds[fd].Dev == uint64(st.Dev) && fds[fd].Ino == st.Ino }
	if !is(0, in) {
		t.Error("stdin is not the pipe")
	}
	logIno := logSt.Sys().(*syscall.Stat_t).Ino
	if fds[1].Ino != logIno || fds[2].Ino != logIno {
		t.Error("stdout and stderr are not notify.log")
	}
	if !is(3, roots[0]) || !is(4, roots[1]) {
		t.Error("descriptors 3 and 4 are not the share roots")
	}
	if warn.Len() != 0 {
		t.Errorf("warnings: %s", warn.String())
	}
	waitFor(t, "kido to reap the relay", func() bool { return unix.Kill(rep.Pid, 0) == unix.ESRCH })
	return rep
}

func sameDir(t *testing.T, fi os.FileInfo, dir string) bool {
	t.Helper()
	want, err := os.Stat(dir)
	return err == nil && os.SameFile(fi, want)
}
