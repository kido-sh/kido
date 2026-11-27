package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kido-sh/kido/internal/mount"
	"github.com/kido-sh/kido/internal/notify"
	"github.com/kido-sh/kido/internal/proto"

	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == NotifyCommand {
		os.Exit(relayProbe(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// realTempDir is t.TempDir without symlinks: on macOS it lies under /var, a symlink to /private/var,
// and share roots are always real paths.
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

func testRelay(roots ...string) *relay {
	r := &relay{log: newBudgetLog(&syncBuffer{}, notifyLogBudget), now: time.Now, flushGap: notifyFlushGap,
		backoff: backoff{min: time.Millisecond, max: 5 * time.Millisecond, stable: time.Hour}, keepalive: time.Hour}
	for i, root := range roots {
		r.shares = append(r.shares, NotifyShare{Name: fmt.Sprintf("m%d", i), Root: root, FD: -1})
	}
	r.init()
	return r
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		root, path, rel string
		ok              bool
	}{
		{"/x/a", "/x/a/b", "b", true},
		{"/x/a", "/x/a", "", true},
		{"/x/a", "/x/ab", "", false},
		{"/x/a", "/x", "", false},
		{"/", "/y/z", "y/z", true},
	} {
		if rel, ok := within(c.root, c.path); rel != c.rel || ok != c.ok {
			t.Errorf("within(%q, %q) = %q %v", c.root, c.path, rel, ok)
		}
	}
}

func TestRelayAddReachesEveryShare(t *testing.T) {
	r := testRelay("/x/a", "/x/a/b", "/x/a", "/y")
	r.add(notify.Event{Path: "/x/a/b/c"})
	for i, want := range []string{"b/c", "c", "b/c", ""} {
		got := strings.Join(r.pend[i].paths, ",")
		if got != want || r.pend[i].flush {
			t.Errorf("share %d: paths %q flush %v, want %q", i, got, r.pend[i].flush, want)
		}
	}
	r.add(notify.Event{Path: "/elsewhere/f"})
	for i := range r.pend {
		if !r.pend[i].flush {
			t.Errorf("an event outside every share did not flush share %d", i)
		}
	}
}

func TestRelayAddFlushes(t *testing.T) {
	r := testRelay("/r", "/s")
	r.add(notify.Event{Path: "/r/a"})
	r.add(notify.Event{Path: "/r/a"})
	if p := r.pend[0]; p.flush || len(p.paths) != 1 {
		t.Fatalf("one path twice: %+v", p)
	}
	for name, ev := range map[string]notify.Event{
		"lost events":  {Path: "/r/x", Flags: notify.FlagKernelDropped},
		"root changed": {Path: "/r", Flags: notify.FlagRootChanged},
		"the root":     {Path: "/r"},
		"bad path":     {Path: "/r/a/../b"},
	} {
		r := testRelay("/r", "/s")
		r.add(notify.Event{Path: "/r/a"})
		r.add(ev)
		if p := r.pend[0]; !p.flush || len(p.paths) != 0 {
			t.Errorf("%s: share not flushed: %+v", name, p)
		}
		if r.pend[1].flush {
			t.Errorf("%s: flushed the other share too", name)
		}
	}
	r = testRelay("/r", "/s")
	r.add(notify.Event{Flush: true})
	if !r.pend[0].flush || !r.pend[1].flush {
		t.Fatal("a flush event did not flush every share")
	}
	r = testRelay("/r")
	r.add(notify.Event{Path: "/r/a"})
	r.add(notify.Event{Path: "/r/a", Flags: notify.FlagItemRenamed})
	r.add(notify.Event{Path: "/r/b", Flags: notify.FlagItemCreated | notify.FlagItemRemoved})
	r.add(notify.Event{Path: "/r/c"})
	deep := "/r" + strings.Repeat("/d", notify.MaxDepth+3)
	r.add(notify.Event{Path: deep})
	cut := strings.TrimPrefix("/r"+strings.Repeat("/d", notify.MaxDepth), "/r/")
	if p := r.pend[0]; p.flush || p.marks["a"] != markName || p.marks["b"] != markName || p.marks["c"] != 0 ||
		p.marks[cut] != markDeep || len(p.paths) != 4 {
		t.Fatalf("marks: %+v", p)
	}
	r = testRelay("/r")
	long := strings.Repeat("n", 1000)
	for i := 0; !r.pend[0].flush; i++ {
		if i > notifyMaxPending {
			t.Fatal("pending records never turned into a flush")
		}
		r.add(notify.Event{Path: fmt.Sprintf("/r/%d%s", i, long)})
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

// TestRelayJudgesNames: FSEvents keeps a path's flags, so only the relay's own memory of what it said,
// and the file IDs of the events, tell a new or replaced name from an old one changed again.
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

func TestRelayForgets(t *testing.T) {
	p := &relayPending{known: map[string]pathState{}}
	dir, file := uint32(unix.S_IFDIR|0o755), uint32(unix.S_IFREG|0o644)
	judge := func(rel string, st notify.State, mode uint32, ino uint64, ids ...uint64) notify.Record {
		p.ids = map[string][]uint64{rel: ids}
		rec := notify.Record{Path: rel, State: st, Mode: mode}
		p.judge(&rec, ino, false)
		return rec
	}
	judge("a", notify.Present, dir, 10)
	judge("a/f", notify.Present, file, 11)
	judge("b/f", notify.Present, file, 12)
	judge("a", notify.Absent, 0, 0)
	if _, ok := p.known["a/f"]; ok {
		t.Fatal("kept what it said below a directory that went")
	}
	if _, ok := p.known["b/f"]; !ok {
		t.Fatal("forgot a path outside that directory")
	}
	judge("b", notify.Present, dir, 20)
	judge("b", notify.Present, dir, 21)
	if _, ok := p.known["b/f"]; ok {
		t.Fatal("kept what it said below a replaced directory")
	}
	judge("pre/f", notify.Present, file, 25)
	judge("pre", notify.Absent, 0, 0)
	if _, ok := p.known["pre/f"]; ok {
		t.Fatal("kept what it said below a pre-existing directory that went")
	}
	judge("g", notify.Present, file, 30)
	if rec := judge("g", notify.Present, file, 31, 31); !rec.Replaced {
		t.Fatalf("a replace: %+v", rec)
	}
	if rec := judge("g", notify.Present, file, 31, 30); rec.Replaced {
		t.Fatalf("the old file ID of a replace already reported counted again: %+v", rec)
	}

	r := testRelay("/r")
	r.pend[0].known["x"] = pathState{present: true, ino: 1}
	r.flushShare(0)
	if len(r.pend[0].known) != 0 {
		t.Fatal("a flush left the relay's memory of what it said")
	}

	p = &relayPending{known: map[string]pathState{}}
	long := strings.Repeat("n", 4000)
	for i := 0; i < notifyKnownBytes/4000+10; i++ {
		judge(fmt.Sprintf("%s/%d", long, i), notify.Present, file, uint64(i+1))
		if p.knownSize > notifyKnownBytes {
			t.Fatalf("the relay remembers %d bytes of paths", p.knownSize)
		}
	}
	size := 0
	for k := range p.known {
		size += len(k)
	}
	if size != p.knownSize {
		t.Fatalf("counted %d bytes of paths, holds %d", p.knownSize, size)
	}
}

func TestBudgetLogRate(t *testing.T) {
	var b bytes.Buffer
	l := newBudgetLog(&b, notifyLogBudget)
	clock := time.Unix(1000, 0)
	l.now, l.last = func() time.Time { return clock }, clock
	for i := 0; i < notifyLogBurst+5; i++ {
		l.printf("line %d", i)
	}
	if n := strings.Count(b.String(), "\n"); n != notifyLogBurst {
		t.Fatalf("%d lines at once, want %d", n, notifyLogBurst)
	}
	clock = clock.Add(notifyLogEvery)
	l.printf("later")
	if !strings.Contains(b.String(), "later (5 lines dropped before this one)") {
		t.Fatalf("the next line does not say what was dropped:\n%s", b.String())
	}
	b.Reset()
	clock = clock.Add(time.Hour)
	l.printf("an event outside every share (%s)", "/x/a\n2026-11-27T00:00:00Z kido notify: notifications on\rz")
	if strings.Count(b.String(), "\n") != 1 || strings.Contains(b.String(), "\r") {
		t.Fatalf("a name with line breaks made more than one line: %q", b.String())
	}
	b.Reset()
	l.printf("%s", strings.Repeat("é", notifyLogLine))
	if line := strings.TrimSuffix(b.String(), "\n"); len(line) > notifyLogLine+64 || !utf8.ValidString(line) {
		t.Fatalf("a long line: %d bytes, valid UTF-8 %v", len(line), utf8.ValidString(line))
	}
}

func TestRelayFramesFlushAtOnceAfterABadRecord(t *testing.T) {
	r := testRelay("/r")
	p := &r.pend[0]
	p.paths, p.marks["a//b"] = []string{"a//b"}, markDeep
	frames, wait := r.frames()
	if len(frames) != 1 || wait != 0 {
		t.Fatalf("%d frames, wait %v", len(frames), wait)
	}
	if _, recs := shareOf(frames[0]); len(recs) != 1 || !recs[0].Flush {
		t.Fatalf("not a flush: %+v", recs)
	}
}

func TestRelayDrainIsBounded(t *testing.T) {
	r := testRelay("/r")
	events := make(chan notify.Event, notifyMaxDrain+10)
	for i := 0; i < notifyMaxDrain+10; i++ {
		events <- notify.Event{Path: fmt.Sprintf("/r/%d", i)}
	}
	r.drain(events)
	if len(events) != 10 || len(r.pend[0].paths) != notifyMaxDrain {
		t.Fatalf("drained %d, left %d", len(r.pend[0].paths), len(events))
	}
}

func TestBackoff(t *testing.T) {
	b := backoff{min: 100 * time.Millisecond, max: 800 * time.Millisecond, stable: 10 * time.Second}
	t0 := time.Unix(1000, 0)
	check := func(want time.Duration) {
		t.Helper()
		if d := b.failed(t0); d < want || d > want+want/2 || b.cur != want {
			t.Fatalf("wait %v (step %v), want %v plus up to half", d, b.cur, want)
		}
	}
	for _, want := range []time.Duration{100, 200, 400, 800, 800} {
		check(want * time.Millisecond)
	}
	b.cur = 200 * time.Millisecond
	b.connected(t0)
	t0 = t0.Add(time.Second)
	check(400 * time.Millisecond)
	b.connected(t0)
	t0 = t0.Add(10 * time.Second)
	check(100 * time.Millisecond)
}

func TestRelayFlushGap(t *testing.T) {
	r := testRelay("/r")
	clock := time.Unix(1000, 0)
	r.now = func() time.Time { return clock }
	isFlush := func(f []byte) bool {
		_, batch, err := notify.DecodeFrame(f)
		recs, perr := notify.ParseBatch(batch)
		return err == nil && perr == nil && len(recs) == 1 && recs[0].Flush
	}
	r.flushShare(0)
	if frames, _ := r.frames(); len(frames) != 1 || !isFlush(frames[0]) {
		t.Fatalf("first flush: %d frames", len(frames))
	}
	r.flushShare(0)
	frames, wait := r.frames()
	if len(frames) != 0 || wait <= 0 || wait > notifyFlushGap {
		t.Fatalf("a second flush at once: %d frames, wait %v", len(frames), wait)
	}
	clock = clock.Add(notifyFlushGap)
	if frames, _ := r.frames(); len(frames) != 1 || !isFlush(frames[0]) {
		t.Fatalf("after the gap: %d frames", len(frames))
	}
}

type fakeSession struct {
	mu     sync.Mutex
	frames [][]byte
	fail   error
	done   chan struct{}
	once   sync.Once
}

func newFakeSession() *fakeSession { return &fakeSession{done: make(chan struct{})} }

func (f *fakeSession) send(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.frames = append(f.frames, append([]byte(nil), b...))
	return nil
}

func (f *fakeSession) closed() <-chan struct{} { return f.done }
func (f *fakeSession) close()                  { f.once.Do(func() { close(f.done) }) }

func (f *fakeSession) got() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.frames...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func shareOf(f []byte) (string, []notify.Record) {
	share, batch, _ := notify.DecodeFrame(f)
	if batch == nil {
		return share, nil
	}
	recs, _ := notify.ParseBatch(batch)
	return share, recs
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

func TestRelayRunUnsupported(t *testing.T) {
	r := testRelay("/r")
	r.dial = func(context.Context) (notifySession, error) { return nil, errNotifyUnsupported }
	result := make(chan error, 1)
	go func() { result <- r.run(context.Background(), make(chan notify.Event), make(chan error)) }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run kept going without notifications")
	}
}

func TestRelayRunCancel(t *testing.T) {
	r := testRelay("/r")
	r.dial = func(context.Context) (notifySession, error) { return newFakeSession(), nil }
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- r.run(ctx, make(chan notify.Event), make(chan error)) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run ignored its context")
	}
}

func TestReadEventsEndsWithAFlush(t *testing.T) {
	good := notify.AppendEvent(nil, notify.Event{Path: "/r/a"})
	in := append(good, 0xff, 0xff, 0xff, 0xff)
	out := make(chan notify.Event, 4)
	err := readEvents(bytes.NewReader(in), out)
	var got []notify.Event
	for ev := range out {
		got = append(got, ev)
	}
	if err == nil || len(got) != 2 || got[0].Path != "/r/a" || !got[1].Flush {
		t.Fatalf("got %+v, %v; want the event, a flush and an error", got, err)
	}
	out = make(chan notify.Event, 4)
	if err := readEvents(bytes.NewReader(good), out); err != nil {
		t.Fatalf("a clean end: %v", err)
	}
}

func TestBudgetLog(t *testing.T) {
	var b bytes.Buffer
	l := newBudgetLog(&b, 200)
	l.printf("agent said %q", "\x1b]52;c;bad\x07")
	if strings.ContainsAny(b.String(), "\x1b\x07") {
		t.Fatalf("control characters reached the log: %q", b.String())
	}
	for i := 0; i < 10; i++ {
		l.printf("line %d with some padding to use up the budget", i)
	}
	if b.Len() > 200+len("kido notify: log limit reached\n") || !strings.HasSuffix(b.String(), "log limit reached\n") {
		t.Fatalf("budget not kept: %d bytes, ends %q", b.Len(), b.String()[max(0, b.Len()-40):])
	}
	n := b.Len()
	l.printf("after the limit")
	if b.Len() != n {
		t.Fatal("wrote past the limit")
	}
}

// share is what mount.Shares gives for src.
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

// TestOpenNotifyRootSpelling: FSEvents spells paths as the volume stores them, so the relay must
// match against that spelling, not the one the user typed.
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
	Dev  int32
	Ino  uint64
	Mode uint16
}

type relayReport struct {
	Args []string
	Env  []string
	Pid  int
	Sid  int
	Dir  string
	FDs  []fdInfo
}

// relayProbe stands in for the relay when startRelay starts the test binary: it reports what it was
// given on stdout, which is notify.log.
func relayProbe(args []string) int {
	rep := relayReport{Args: args, Env: os.Environ(), Pid: os.Getpid()}
	rep.Sid, _ = unix.Getsid(0)
	rep.Dir, _ = os.Getwd()
	for fd := 0; fd < 256; fd++ {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil {
			rep.FDs = append(rep.FDs, fdInfo{FD: fd, Dev: st.Dev, Ino: st.Ino, Mode: st.Mode})
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

// startProbe runs startRelay on two share roots with the test binary as the relay, and checks what
// the relay was given.
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
		if f.Dev == out.Dev && f.Ino == out.Ino {
			t.Errorf("the relay got the pipe's write end at %d", f.FD)
		}
	}
	is := func(fd int, st unix.Stat_t) bool { return fds[fd].Dev == st.Dev && fds[fd].Ino == st.Ino }
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

func TestNotifyDialUsesThePinnedAgent(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "c1"
	if err := os.MkdirAll(r.dir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	os.WriteFile(r.file(id, "key"), seed, 0o600)
	hostKey := ed25519.NewKeyFromSeed(seed)
	host := hostKey.Public().(ed25519.PublicKey)
	_, agentKey, _ := ed25519.GenerateKey(nil)
	_, otherKey, _ := ed25519.GenerateKey(nil)
	os.WriteFile(r.file(id, "agent.pub"), agentKey.Public().(ed25519.PublicKey), 0o600)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type served struct {
		req   *proto.Request
		frame []byte
		err   error
	}
	serve := func(identity ed25519.PrivateKey, boot []byte, resp proto.Response) <-chan served {
		out := make(chan served, 1)
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				out <- served{err: err}
				return
			}
			defer conn.Close()
			br := bufio.NewReader(conn)
			var h proto.Hello
			if err := proto.ReadLine(br, &h); err != nil {
				out <- served{err: err}
				return
			}
			c, ch, err := proto.AgentChallenge(identity, boot, h, conn, br)
			if err == nil {
				err = proto.WriteLine(conn, c)
			}
			var req *proto.Request
			if err == nil {
				req, err = ch.ReadRequest(host)
			}
			if err == nil {
				err = ch.WriteMessage(proto.FrameResponse, resp)
			}
			var frame []byte
			if err == nil && resp.OK {
				_, frame, err = ch.ReadFrame()
			}
			out <- served{req: req, frame: frame, err: err}
		}()
		return out
	}
	ctx := context.Background()
	dial := r.notifyDial(id, ln.Addr().(*net.TCPAddr).Port, []string{"m0", "m1"})

	got := serve(agentKey, nil, proto.Response{OK: true})
	s, err := dial(ctx)
	if err != nil {
		t.Fatalf("the pinned agent: %v", err)
	}
	if err := s.send([]byte("frame")); err != nil {
		t.Fatal(err)
	}
	g := <-got
	if g.err != nil || g.req.Op != proto.OpNotify || !slices.Equal(g.req.Args, []string{"m0", "m1"}) || string(g.frame) != "frame" {
		t.Fatalf("the agent saw %+v", g)
	}
	s.close()

	got = serve(otherKey, nil, proto.Response{OK: true})
	if _, err := dial(ctx); !errors.Is(err, proto.ErrAgentIdentity) {
		t.Fatalf("another identity: %v", err)
	}
	<-got
	got = serve(agentKey, nil, proto.Response{Error: `unknown op "notify"`})
	if _, err := dial(ctx); !errors.Is(err, errNotifyUnsupported) {
		t.Fatalf("an agent without notify: %v", err)
	}
	<-got
	os.Remove(r.file(id, "agent.pub"))
	got = serve(agentKey, proto.BootSecret(hostKey), proto.Response{OK: true})
	if s, err := dial(ctx); err == nil {
		s.close()
		t.Fatal("a first contact: dialed an agent with no pinned identity")
	}
	ln.Close()
	if g := <-got; g.req != nil {
		t.Fatalf("an agent with no pinned identity got a request: %+v", g.req)
	}
}
