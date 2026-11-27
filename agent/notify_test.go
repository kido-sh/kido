package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"errors"
	"net"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/notify"
	"github.com/kido-sh/kido/internal/proto"

	"golang.org/x/sys/unix"
)

type fakeMounts struct {
	mu       sync.Mutex
	entries  []mountInfo
	seen     map[string]mountInfo // what fstatfs reports for each mount point once opened
	fds      map[int]string
	next     int
	probeErr error
	errs     []error        // returned by the next fsctl calls with a batch
	dead     map[int]bool   // descriptors of file systems unmounted since they were opened
	failOpen map[string]int // opens of a mount point to fail
	calls    []string
	batches  [][]byte
}

func (f *fakeMounts) list() ([]mountInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mountInfo(nil), f.entries...), nil
}

func (f *fakeMounts) open(path string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOpen[path] > 0 {
		f.failOpen[path]--
		return -1, unix.EIO
	}
	f.next++
	if f.fds == nil {
		f.fds = map[int]string{}
	}
	f.fds[f.next] = path
	return f.next, nil
}

func (f *fakeMounts) info(fd int) (mountInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, ok := f.fds[fd]
	if !ok || f.dead[fd] {
		return mountInfo{}, unix.EBADF
	}
	if m, ok := f.seen[path]; ok {
		return m, nil
	}
	for _, e := range f.entries {
		if e.on == path {
			return e, nil
		}
	}
	return mountInfo{}, unix.ENOENT
}

func (f *fakeMounts) fsctl(fd int, batch []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if batch == nil {
		if f.dead[fd] {
			return unix.ENOTTY
		}
		return f.probeErr
	}
	f.calls = append(f.calls, f.fds[fd])
	f.batches = append(f.batches, append([]byte(nil), batch...))
	if f.dead[fd] {
		return unix.ENOTTY
	}
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return err
	}
	return nil
}

func (f *fakeMounts) close(fd int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fds, fd)
}

func (f *fakeMounts) sent() ([]string, [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...), append([][]byte(nil), f.batches...)
}

func vfs(from, on string, id int32) mountInfo {
	return mountInfo{from: from, typ: "virtiofs", on: on, fsid: unix.Fsid{Val: [2]int32{id, id}}}
}

func testBatch(t *testing.T, path string) []byte {
	t.Helper()
	b, err := notify.AppendRecord(nil, notify.Record{State: notify.Absent, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNotifyMountsChecksTheFileSystem(t *testing.T) {
	f := &fakeMounts{
		entries: []mountInfo{
			{from: "kido:m0", typ: "apfs", on: "/wrong-type"},
			vfs("kido:m0", "/swapped", 1),
			vfs("kido:m1", "/other", 2),
			vfs("kido:m0", "/data", 3),
			vfs("kido:m0", "/again", 4),
		},
		seen: map[string]mountInfo{"/swapped": vfs("kido:m0", "/swapped", 9)},
	}
	m := newNotifyMounts(f, []string{"m0"})
	if err := m.send("m0", testBatch(t, "a")); err != nil {
		t.Fatal(err)
	}
	if calls, _ := f.sent(); strings.Join(calls, " ") != "/data /again" {
		t.Fatalf("batch went to %q, want every virtio-fs mount of kido:m0", calls)
	}
	if err := m.send("m1", testBatch(t, "a")); err == nil {
		t.Fatal("a share the session did not name was accepted")
	}
	m.closeAll()
	if len(f.fds) != 0 {
		t.Fatalf("closeAll left %d descriptors open", len(f.fds))
	}
}

func TestNotifyMountsFindsANewMount(t *testing.T) {
	f := &fakeMounts{entries: []mountInfo{vfs("kido:m0", "/data", 1)}}
	m := newNotifyMounts(f, []string{"m0"})
	clock := time.Unix(1000, 0)
	m.now = func() time.Time { return clock }
	send := func(want string) {
		t.Helper()
		before, _ := f.sent()
		if err := m.send("m0", testBatch(t, "a")); err != nil {
			t.Fatal(err)
		}
		calls, _ := f.sent()
		if got := strings.Join(calls[len(before):], " "); got != want {
			t.Fatalf("batch went to %q, want %q", got, want)
		}
	}
	send("/data")
	f.mu.Lock()
	f.entries = append(f.entries, vfs("kido:m0", "/again", 2))
	f.mu.Unlock()
	send("/data")
	clock = clock.Add(notifyRecheck)
	send("/data /again")
	if len(f.fds) != 2 {
		t.Fatalf("%d descriptors open, want one per mount", len(f.fds))
	}
}

func TestNotifyMountsRetriesAFailedOpen(t *testing.T) {
	f := &fakeMounts{entries: []mountInfo{vfs("kido:m0", "/data", 1), vfs("kido:m0", "/again", 2)},
		failOpen: map[string]int{"/again": 1}}
	m := newNotifyMounts(f, []string{"m0"})
	clock := time.Unix(1000, 0)
	m.now = func() time.Time { return clock }
	if err := m.send("m0", testBatch(t, "a")); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(notifyRecheck)
	if err := m.send("m0", testBatch(t, "b")); err != nil {
		t.Fatal(err)
	}
	if calls, _ := f.sent(); strings.Join(calls, " ") != "/data /data /again" {
		t.Fatalf("batches went to %q: a mount that failed to open once was not tried again", calls)
	}
}

func TestNotifyMountsResendsOnlyWhereNeeded(t *testing.T) {
	f := &fakeMounts{entries: []mountInfo{vfs("kido:m0", "/data", 1), vfs("kido:m0", "/again", 2)}, dead: map[int]bool{}}
	m := newNotifyMounts(f, []string{"m0"})
	if _, err := m.find("m0"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.dead[m.shares["m0"].open[1].fd] = true
	f.entries[1] = vfs("kido:m0", "/again", 3)
	f.mu.Unlock()
	if err := m.send("m0", testBatch(t, "a")); err != nil {
		t.Fatal(err)
	}
	if calls, _ := f.sent(); strings.Join(calls, " ") != "/data /again /again" {
		t.Fatalf("batches went to %q: want the mount that took the batch left out of the retry", calls)
	}
}

func TestNotifyProbeUnmountedMeanwhile(t *testing.T) {
	f := &fakeMounts{entries: []mountInfo{vfs("kido:m0", "/data", 1)}, dead: map[int]bool{}}
	m := newNotifyMounts(f, []string{"m0"})
	if _, err := m.find("m0"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.dead[m.shares["m0"].open[0].fd] = true
	f.mu.Unlock()
	if err := m.probe(); !errors.Is(err, errNoMounts) {
		t.Fatalf("a mount unmounted between finding and asking it: %v, want an error the relay retries after", err)
	}
}

// TestNotifyMountsFindsTheMountAgain unmounts and mounts a share again between batches: XNU answers
// ffsctl on a descriptor of an unmounted file system with ENOTTY.
func TestNotifyMountsFindsTheMountAgain(t *testing.T) {
	f := &fakeMounts{entries: []mountInfo{vfs("kido:m0", "/data", 1)}, dead: map[int]bool{}}
	m := newNotifyMounts(f, []string{"m0"})
	if err := m.send("m0", testBatch(t, "a")); err != nil {
		t.Fatal(err)
	}
	old := m.shares["m0"].open[0].fd
	f.mu.Lock()
	f.dead[old] = true
	f.entries = []mountInfo{vfs("kido:m0", "/data", 2)}
	f.mu.Unlock()
	if err := m.send("m0", testBatch(t, "b")); err != nil {
		t.Fatalf("after the share was mounted again: %v", err)
	}
	if calls, _ := f.sent(); len(calls) != 3 || len(m.shares["m0"].open) != 1 || m.shares["m0"].open[0].fd == old {
		t.Fatalf("%d fsctl calls, mounts %v: want the new mount found after ENOTTY", len(calls), m.shares["m0"].open)
	}
	if _, open := f.fds[old]; open {
		t.Fatal("the old mount's descriptor was not closed")
	}
	f.errs = []error{unix.EIO, unix.EIO}
	if err := m.send("m0", testBatch(t, "c")); !errors.Is(err, unix.EIO) {
		t.Fatalf("EIO twice: got %v", err)
	}
	if calls, _ := f.sent(); len(calls) != 5 {
		t.Fatalf("%d fsctl calls, want one retry after an error", len(calls))
	}
}

func TestNotifyProbe(t *testing.T) {
	for _, c := range []struct {
		err  error
		want error
	}{{nil, nil}, {unix.ENOTTY, errUnsupported}, {unix.EINVAL, errUnsupported}, {unix.ENOTSUP, errUnsupported},
		{unix.EPERM, unix.EPERM}} {
		f := &fakeMounts{entries: []mountInfo{vfs("kido:m0", "/data", 1)}, probeErr: c.err}
		m := newNotifyMounts(f, []string{"m0"})
		if err := m.probe(); !errors.Is(err, c.want) && err != c.want {
			t.Errorf("probe with fsctl error %v: got %v, want %v", c.err, err, c.want)
		}
	}
	m := newNotifyMounts(&fakeMounts{}, []string{"m0"})
	if err := m.probe(); !errors.Is(err, errNoMounts) {
		t.Errorf("with nothing mounted: %v, want an error the relay retries after", err)
	}
}

// startNotifyAgent runs an agent in this process, with f for its file systems.
func startNotifyAgent(t *testing.T, f *fakeMounts) string {
	t.Helper()
	localOnly(t)
	_, identity, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{hostKey: testKey.Public().(ed25519.PublicKey), identity: identity, boot: proto.BootSecret(testKey),
		conns: make(chan struct{}, 8), poweroff: func() {}, mounts: f}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.pending.add(conn)
			go s.serve(conn)
		}
	}()
	return ln.Addr().String()
}

type notifyClient struct {
	conn net.Conn
	ch   *proto.Channel
	resp proto.Response
}

func dialNotify(t *testing.T, addr string, shares ...string) *notifyClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	_, ch, err := proto.ClientHandshake(conn, br, testKey, nil, proto.Request{Op: proto.OpNotify, Args: shares})
	if err != nil {
		t.Fatal(err)
	}
	c := &notifyClient{conn: conn, ch: ch}
	if err := ch.ReadMessage(proto.FrameResponse, &c.resp); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *notifyClient) frame(t *testing.T, typ byte, share string, batch []byte) {
	t.Helper()
	var p []byte
	if share != "" {
		var err error
		if p, err = notify.EncodeFrame(share, batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.ch.WriteFrame(typ, p); err != nil {
		t.Fatal(err)
	}
}

// closedByAgent reports whether the agent closed the connection within a few seconds.
func (c *notifyClient) closedByAgent() bool {
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := c.ch.ReadFrame()
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

func waitCalls(t *testing.T, f *fakeMounts, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, batches := f.sent()
		if len(batches) >= n || time.Now().After(deadline) {
			return batches
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNotifySession(t *testing.T) {
	f := &fakeMounts{entries: []mountInfo{vfs("kido:m0", "/data", 1)}}
	addr := startNotifyAgent(t, f)
	c := dialNotify(t, addr, "m0")
	if !c.resp.OK {
		t.Fatalf("notify refused: %q", c.resp.Error)
	}
	good := testBatch(t, "dir/file")
	c.frame(t, proto.FrameNotify, "m0", good)
	c.frame(t, proto.FrameNotify, "", nil)
	bad := append([]byte(nil), good...)
	bad[2] = 9
	c.frame(t, proto.FrameNotify, "m0", bad)
	c.frame(t, proto.FrameNotify, "m0", testBatch(t, "second"))
	flush, _ := notify.AppendRecord(nil, notify.Record{Flush: true})
	batches := waitCalls(t, f, 3)
	if len(batches) != 3 || !bytes.Equal(batches[0], good) || !bytes.Equal(batches[1], flush) ||
		!bytes.Equal(batches[2], testBatch(t, "second")) {
		t.Fatalf("the driver got %d batches, want the valid one, a flush for the malformed one, then the next", len(batches))
	}

	second := dialNotify(t, addr, "m0")
	if !second.resp.OK {
		t.Fatalf("second session refused: %q", second.resp.Error)
	}
	if !c.closedByAgent() {
		t.Fatal("a new notify session did not end the old one")
	}
	second.frame(t, proto.FrameStdin, "m0", good)
	if !second.closedByAgent() {
		t.Fatal("a frame of another type did not end the session")
	}
}

func TestNotifySessionRefusals(t *testing.T) {
	f := &fakeMounts{entries: []mountInfo{vfs("kido:m0", "/data", 1)}, probeErr: unix.ENOTTY}
	addr := startNotifyAgent(t, f)
	if c := dialNotify(t, addr, "m0"); c.resp.OK || !strings.Contains(c.resp.Error, "no notifications") {
		t.Fatalf("an old driver: got %+v", c.resp)
	}
	f.mu.Lock()
	f.probeErr = nil
	f.mu.Unlock()
	if c := dialNotify(t, addr, "../m0"); c.resp.OK {
		t.Fatal("a bad share name was accepted")
	}
	if c := dialNotify(t, addr, "m7"); c.resp.OK || c.resp.Error != errNoMounts.Error() {
		t.Fatalf("a session whose shares are not mounted: %+v", c.resp)
	}
}

// TestSysMountsOpenLetsTheVolumeUnmount mounts a disk image, opens its root as notify does, and
// unmounts it: the agent's descriptor must not make the file system busy.
func TestSysMountsOpenLetsTheVolumeUnmount(t *testing.T) {
	dmg := filepath.Join(t.TempDir(), "v.dmg")
	if out, err := exec.Command("hdiutil", "create", "-quiet", "-size", "4m", "-fs", "HFS+", "-volname", "kidonotify", dmg).CombinedOutput(); err != nil {
		t.Skipf("hdiutil create: %v %s", err, out)
	}
	mnt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("hdiutil", "attach", "-nobrowse", "-noverify", "-mountpoint", mnt, dmg).Output()
	disk := regexp.MustCompile(`/dev/disk[0-9]+`).FindString(string(out))
	if disk != "" {
		t.Cleanup(func() {
			if out, err := exec.Command("hdiutil", "detach", "-force", disk).CombinedOutput(); err != nil {
				t.Errorf("hdiutil detach %s: %v %s", disk, err, out)
			}
		})
	}
	if err != nil || disk == "" {
		t.Skipf("hdiutil attach: %v %s", err, out)
	}
	fd, err := sysMounts{}.open(mnt)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, err := (sysMounts{}).info(fd); err != nil {
		t.Fatalf("fstatfs on the descriptor: %v", err)
	}
	// Spotlight and others may hold a fresh volume for a moment; the descriptor would hold it for good.
	for range 10 {
		if out, err = exec.Command("/sbin/umount", mnt).CombinedOutput(); err == nil || !bytes.Contains(out, []byte("busy")) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("umount with the descriptor open: %v %s", err, out)
	}
}
