package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/proto"
)

var (
	testKey   ed25519.PrivateKey
	agentAddr string
	remote    bool
)

// TestMain starts the agent on 127.0.0.1, or, with KIDO_AGENT_ADDR and KIDO_AGENT_KEY (a container's key
// file) set, runs
// the tests that do not need host files against that agent (a guest behind QEMU's hostfwd).
func TestMain(m *testing.M) {
	if os.Getenv("KIDO_AGENT_TEST_MAIN") == "1" {
		main()
		return
	}
	if addr := os.Getenv("KIDO_AGENT_ADDR"); addr != "" {
		seed, err := os.ReadFile(os.Getenv("KIDO_AGENT_KEY"))
		if err != nil || len(seed) != ed25519.SeedSize {
			panic("KIDO_AGENT_KEY must name a container's key file")
		}
		agentAddr, testKey, remote = addr, ed25519.NewKeyFromSeed(seed), true
		os.Exit(m.Run())
	}
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		panic(err)
	}
	testKey = key
	cmd := exec.Command(os.Args[0], "--listen", "127.0.0.1:0", "--key", proto.EncodeKey(pub), "--secret", proto.EncodeSecret(proto.BootSecret(key)))
	cmd.Env = append(os.Environ(), "KIDO_AGENT_TEST_MAIN=1")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		panic(err)
	}
	if err := cmd.Start(); err != nil {
		panic(err)
	}
	sc := bufio.NewScanner(stderr)
	re := regexp.MustCompile(`listening on (\S+)`)
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			agentAddr = m[1]
			break
		}
	}
	if agentAddr == "" {
		cmd.Process.Kill()
		panic("agent did not start")
	}
	go io.Copy(io.Discard, stderr)
	code := m.Run()
	cmd.Process.Kill()
	cmd.Wait()
	os.Exit(code)
}

func localOnly(t *testing.T) {
	if remote {
		t.Skip("needs the agent on this host")
	}
}

type client struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	ch   *proto.Channel
	resp proto.Response

	mu     sync.Mutex
	stdout bytes.Buffer
	stderr bytes.Buffer
	status int32
	done   chan struct{}
}

func dialAgent(t *testing.T, req proto.Request) *client {
	t.Helper()
	conn, err := net.Dial("tcp", agentAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	s := &client{t: t, conn: conn, br: bufio.NewReader(conn), status: -1, done: make(chan struct{})}
	_, ch, err := proto.ClientHandshake(conn, s.br, testKey, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	s.ch = ch
	if err := ch.ReadMessage(proto.FrameResponse, &s.resp); err != nil {
		t.Fatal(err)
	}
	if s.resp.OK && req.Op == proto.OpExec {
		go s.readFrames()
	} else {
		close(s.done)
	}
	return s
}

func (s *client) readFrames() {
	defer close(s.done)
	for {
		typ, p, err := s.ch.ReadFrame()
		if err != nil {
			return
		}
		s.mu.Lock()
		switch typ {
		case proto.FrameStdout:
			s.stdout.Write(p)
		case proto.FrameStderr:
			s.stderr.Write(p)
		case proto.FrameExit:
			s.status, _ = proto.ParseExit(p)
		}
		s.mu.Unlock()
	}
}

func (s *client) send(typ byte, p []byte) {
	s.t.Helper()
	if err := s.ch.WriteFrame(typ, p); err != nil {
		s.t.Fatal(err)
	}
}

func (s *client) wait() (stdout, stderr string, status int32) {
	s.t.Helper()
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
		s.t.Fatal("no exit frame")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stdout.String(), s.stderr.String(), s.status
}

func (s *client) waitOutput(pattern string) {
	s.t.Helper()
	re := regexp.MustCompile(pattern)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		out := s.stdout.String()
		s.mu.Unlock()
		if re.MatchString(out) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.t.Fatalf("no %q in output %q", pattern, s.stdout.String())
}

func run(t *testing.T, req proto.Request, stdin string) (string, string, int32) {
	t.Helper()
	req.Op = proto.OpExec
	s := dialAgent(t, req)
	if !s.resp.OK {
		t.Fatalf("exec %q: %s", req.Args, s.resp.Error)
	}
	if stdin != "" {
		s.send(proto.FrameStdin, []byte(stdin))
	}
	s.send(proto.FrameCloseStdin, nil)
	return s.wait()
}

func TestPing(t *testing.T) {
	s := dialAgent(t, proto.Request{Op: proto.OpPing})
	if !s.resp.OK || s.resp.Version != proto.Version {
		t.Fatalf("ping: %+v", s.resp)
	}
}

func TestUnknownOp(t *testing.T) {
	s := dialAgent(t, proto.Request{Op: "nope"})
	if s.resp.OK || !strings.Contains(s.resp.Error, "unknown op") {
		t.Fatalf("unknown op: %+v", s.resp)
	}
}

func TestExecOutputAndStatus(t *testing.T) {
	stdout, stderr, status := run(t, proto.Request{Args: []string{"/bin/sh", "-c", "echo out; echo err >&2; exit 3"}}, "")
	if stdout != "out\n" || stderr != "err\n" || status != 3 {
		t.Fatalf("got stdout %q stderr %q status %d", stdout, stderr, status)
	}
	if _, _, status := run(t, proto.Request{Args: []string{"true"}}, ""); status != 0 {
		t.Fatalf("true: status %d", status)
	}
}

func TestExecStdin(t *testing.T) {
	stdout, _, status := run(t, proto.Request{Args: []string{"cat"}}, "hello\nworld\n")
	if stdout != "hello\nworld\n" || status != 0 {
		t.Fatalf("cat: %q, status %d", stdout, status)
	}
	big := strings.Repeat("0123456789abcdef", 1<<16)
	stdout, _, status = run(t, proto.Request{Args: []string{"wc", "-c"}}, big)
	if strings.TrimSpace(stdout) != fmt.Sprint(len(big)) || status != 0 {
		t.Fatalf("wc -c: %q, status %d", stdout, status)
	}
}

func TestExecEnvDirUser(t *testing.T) {
	localOnly(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	stdout, _, status := run(t, proto.Request{
		Args: []string{"/bin/sh", "-c", `echo "$FOO|$HOME|$PATH|$(printenv TERM)"; pwd -P; id -u; id -g`},
		Env:  []string{"FOO=bar baz"},
		Dir:  dir,
		User: user,
	}, "")
	want := fmt.Sprintf("bar baz|/|%s|\n%s\n%d\n%d\n", defaultPath, dir, os.Getuid(), os.Getgid())
	if stdout != want || status != 0 {
		t.Fatalf("got %q status %d, want %q", stdout, status, want)
	}
	stdout, _, _ = run(t, proto.Request{Args: []string{"/bin/sh", "-c", `echo "$HOME|$PATH"`}, Env: []string{"PATH=/bin:/usr/bin"}}, "")
	if stdout != rootHome+"|/bin:/usr/bin\n" {
		t.Fatalf("env override: %q", stdout)
	}
}

func TestExecErrors(t *testing.T) {
	localOnly(t)
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	os.WriteFile(plain, []byte("#!/bin/sh\n"), 0o644)
	cases := []struct {
		req    proto.Request
		prefix string
	}{
		{proto.Request{Args: []string{"no-such-program-kido"}}, "executable file not found"},
		{proto.Request{Args: []string{"/no/such/program"}}, "executable file not found"},
		{proto.Request{Args: []string{plain}}, "permission denied"},
		{proto.Request{Args: []string{"plain"}, Env: []string{"PATH=" + dir}}, "permission denied"},
		{proto.Request{Args: []string{"/tmp"}}, "permission denied"},
		{proto.Request{Args: []string{"true"}, Dir: "relative"}, "working directory"},
		{proto.Request{Args: []string{"true"}, Dir: "/no/such/dir"}, "working directory"},
		{proto.Request{Args: []string{"true"}, Env: []string{"NOEQUALS"}}, "invalid environment"},
		{proto.Request{Args: []string{"true"}, User: "no-such-user-kido"}, "unable to find user"},
		{proto.Request{}, "no command"},
	}
	for _, c := range cases {
		c.req.Op = proto.OpExec
		s := dialAgent(t, c.req)
		if s.resp.OK || !strings.HasPrefix(s.resp.Error, c.prefix) {
			t.Errorf("%q: got %+v, want error %q", c.req.Args, s.resp, c.prefix)
		}
	}
}

func TestExecSignals(t *testing.T) {
	s := dialAgent(t, proto.Request{Op: proto.OpExec, Args: []string{"/bin/sleep", "30"}})
	if !s.resp.OK || s.resp.PID <= 0 {
		t.Fatalf("exec: %+v", s.resp)
	}
	time.Sleep(100 * time.Millisecond)
	s.send(proto.FrameSignal, []byte{byte(syscall.SIGTERM)})
	if _, _, status := s.wait(); status != 128+int32(syscall.SIGTERM) {
		t.Fatalf("SIGTERM: status %d", status)
	}

	s = dialAgent(t, proto.Request{Op: proto.OpExec, Args: []string{"/bin/sh", "-c", "/bin/sleep 30; echo survived"}})
	time.Sleep(100 * time.Millisecond)
	s.send(proto.FrameSignal, []byte{byte(syscall.SIGINT)})
	if stdout, _, status := s.wait(); status != 128+int32(syscall.SIGINT) || stdout != "" {
		t.Fatalf("SIGINT to the process group: stdout %q status %d", stdout, status)
	}

	if _, _, status := run(t, proto.Request{Args: []string{"/bin/sh", "-c", "kill -KILL $$"}}, ""); status != 137 {
		t.Fatalf("killed by SIGKILL: status %d", status)
	}
}

func TestExecTTY(t *testing.T) {
	stdout, stderr, status := run(t, proto.Request{Args: []string{"/bin/sh", "-c", "tty; stty size; echo $TERM; echo err >&2"}, TTY: true, Rows: 24, Cols: 80}, "")
	if !regexp.MustCompile(`^/dev/ttys\d+\r\n24 80\r\nxterm\r\nerr\r\n$`).MatchString(stdout) || stderr != "" || status != 0 {
		t.Fatalf("tty: stdout %q stderr %q status %d", stdout, stderr, status)
	}

	s := dialAgent(t, proto.Request{Op: proto.OpExec, Args: []string{"/bin/sh"}, TTY: true, Rows: 24, Cols: 80, Env: []string{"PS1=$ "}})
	if !s.resp.OK {
		t.Fatalf("exec: %+v", s.resp)
	}
	s.send(proto.FrameStdin, []byte("stty size\n"))
	s.waitOutput(`24 80`)
	s.send(proto.FrameResize, proto.ResizePayload(40, 132))
	s.send(proto.FrameStdin, []byte("stty size\n"))
	s.waitOutput(`40 132`)
	s.send(proto.FrameStdin, []byte("/bin/sleep 30\n"))
	time.Sleep(200 * time.Millisecond)
	s.send(proto.FrameSignal, []byte{byte(syscall.SIGINT)})
	s.send(proto.FrameStdin, []byte("echo after-$?\n"))
	s.waitOutput(`after-130`)
	s.send(proto.FrameStdin, []byte("exit 5\n"))
	if _, _, status := s.wait(); status != 5 {
		t.Fatalf("interactive shell: status %d", status)
	}

	s = dialAgent(t, proto.Request{Op: proto.OpExec, Args: []string{"cat"}, TTY: true})
	s.send(proto.FrameStdin, []byte("typed\n"))
	s.waitOutput(`typed\r\ntyped\r\n`)
	s.send(proto.FrameStdin, []byte{0x04})
	if _, _, status := s.wait(); status != 0 {
		t.Fatalf("cat on a TTY after ^D: status %d", status)
	}
}

func TestExecBackgroundHolder(t *testing.T) {
	start := time.Now()
	stdout, _, status := run(t, proto.Request{Args: []string{"/bin/sh", "-c", "echo fg; /bin/sleep 5 & exit 2"}}, "")
	if stdout != "fg\n" || status != 2 {
		t.Fatalf("got %q status %d", stdout, status)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("exit waited %v for a background process", d)
	}
}

func TestConcurrentExecs(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stdout, _, status := run(t, proto.Request{Args: []string{"/bin/sh", "-c", fmt.Sprintf("sleep 0.2; echo %d; exit %d", i, i%7)}}, "")
			if stdout != fmt.Sprintf("%d\n", i) || status != int32(i%7) {
				t.Errorf("exec %d: %q status %d", i, stdout, status)
			}
		}()
	}
	wg.Wait()
}

func TestClientGoneHangsUp(t *testing.T) {
	localOnly(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "hup")
	s := dialAgent(t, proto.Request{Op: proto.OpExec, Args: []string{"/bin/sh", "-c", "trap 'echo hup > " + marker + "; exit 1' HUP; while :; do sleep 0.05; done"}, TTY: true})
	if !s.resp.OK {
		t.Fatalf("exec: %+v", s.resp)
	}
	time.Sleep(200 * time.Millisecond)
	s.conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the session was not hung up after the client went away")
}

func TestLookupUser(t *testing.T) {
	localOnly(t)
	dir := t.TempDir()
	passwdFile, groupFile = filepath.Join(dir, "passwd"), filepath.Join(dir, "group")
	t.Cleanup(func() { passwdFile, groupFile = "/etc/passwd", "/etc/group" })
	os.WriteFile(passwdFile, []byte("# comment\nroot:*:0:0:System Administrator:/var/root:/bin/sh\nnobody:*:-2:-2:Unprivileged User:/var/empty:/usr/bin/false\nalice:*:501:20::/Users/alice:/bin/zsh\n"), 0o644)
	os.WriteFile(groupFile, []byte("wheel:*:0:root\nstaff:*:20:root\nadmin:*:80:alice,root\nnobody:*:-2:\n"), 0o644)
	cases := []struct {
		spec   string
		uid    uint32
		gid    uint32
		groups []uint32
		home   string
	}{
		{"root", 0, 0, []uint32{0, 20, 80}, "/var/root"},
		{"alice", 501, 20, []uint32{20, 80}, "/Users/alice"},
		{"nobody", 4294967294, 4294967294, []uint32{4294967294}, "/var/empty"},
		{"501", 501, 20, []uint32{20, 80}, "/Users/alice"},
		{"1234", 1234, 0, []uint32{0}, "/"},
		{"1234:56", 1234, 56, []uint32{56}, "/"},
		{"alice:wheel", 501, 0, []uint32{0, 80}, "/Users/alice"},
	}
	for _, c := range cases {
		a, err := lookupUser(c.spec)
		if err != nil {
			t.Errorf("%s: %v", c.spec, err)
			continue
		}
		if a.uid != c.uid || a.gid != c.gid || a.home != c.home || fmt.Sprint(a.groups) != fmt.Sprint(c.groups) {
			t.Errorf("%s: got %+v", c.spec, *a)
		}
	}
	for _, bad := range []string{"", ":", "bob", "alice:", "alice:nogroup", "../x", "-1"} {
		if _, err := lookupUser(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
