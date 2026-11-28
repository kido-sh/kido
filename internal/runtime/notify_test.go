package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kido-sh/kido/internal/notify"
	"github.com/kido-sh/kido/internal/proto"

	"golang.org/x/sys/unix"
)

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

func TestRelayPendingSyntheticReplaceSameInode(t *testing.T) {
	// Even when the filesystem immediately reuses the same inode number after
	// rm + recreate of a known file, SyntheticReplaceID must mark Replaced and
	// NameChanged so guest kqueue NOTE_DELETE watchers fire.
	p := &relayPending{known: map[string]pathState{}}
	file := uint32(unix.S_IFREG | 0o644)
	rec1 := notify.Record{Path: "f", State: notify.Present, Mode: file}
	p.judge(&rec1, 777, false)

	p.ids = map[string][]uint64{"f": {notify.SyntheticReplaceID}}
	rec2 := notify.Record{Path: "f", State: notify.Present, Mode: file}
	p.judge(&rec2, 777, true)
	if !rec2.Replaced || !rec2.NameChanged {
		t.Fatalf("same-inode unlink+recreate with SyntheticReplaceID: got %+v, want Replaced && NameChanged", rec2)
	}
}
