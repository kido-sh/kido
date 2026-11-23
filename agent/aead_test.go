package main

import (
	"bufio"
	"crypto/ed25519"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/proto"
)

// TestExecTamperedFrameHangsUp flips one bit of a sealed stdin frame on its way to the agent: the
// agent must pass nothing to the process, hang the session up and close the connection.
func TestExecTamperedFrameHangsUp(t *testing.T) {
	addr, key := startKeyAgent(t)
	dir := t.TempDir()
	marker, stdin := filepath.Join(dir, "hup"), filepath.Join(dir, "stdin")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	tw := &tamperWriter{w: conn, n: 3, off: 9}
	req := proto.Request{Op: proto.OpExec, Args: []string{"/bin/sh", "-c", "trap 'echo hup > " + marker + "; exit 1' HUP; echo ready; cat > " + stdin + "; echo eof"}}
	_, ch, err := proto.ClientHandshake(tw, bufio.NewReader(conn), key, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	var resp proto.Response
	if err := ch.ReadMessage(proto.FrameResponse, &resp); err != nil || !resp.OK {
		t.Fatalf("exec: %v %+v", err, resp)
	}
	if typ, p, err := ch.ReadFrame(); err != nil || typ != proto.FrameStdout || string(p) != "ready\n" {
		t.Fatalf("%d %q %v", typ, p, err)
	}
	if err := ch.WriteFrame(proto.FrameStdin, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { got, _ := os.ReadFile(stdin); return string(got) == "first\n" })
	if err := ch.WriteFrame(proto.FrameStdin, []byte("injected\n")); err != nil {
		t.Fatal(err)
	}
	for {
		typ, p, err := ch.ReadFrame()
		if err != nil {
			break
		}
		if typ == proto.FrameExit {
			t.Fatalf("the session ran to its end (%x) after a tampered frame", p)
		}
	}
	waitFor(t, func() bool { _, err := os.Stat(marker); return err == nil })
	if got, _ := os.ReadFile(stdin); string(got) != "first\n" {
		t.Fatalf("the process read %q, want only the frame before the tampered one", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}

// TestFirstContactNeedsBootSecret runs an agent that knows the public boot-args (the host key) but not
// the boot secret: kido must not accept it at the first contact, and must still talk to a pinned one.
func TestFirstContactNeedsBootSecret(t *testing.T) {
	addr, key := startAgent(t, false)
	if _, _, err := keyRequest(t, addr, key, nil, proto.Request{Op: proto.OpPing}); !errors.Is(err, proto.ErrBootProof) {
		t.Fatalf("first contact without a boot proof: %v, want ErrBootProof", err)
	}
	addr, key = startKeyAgent(t)
	agent, resp, err := keyRequest(t, addr, key, nil, proto.Request{Op: proto.OpPing})
	if err != nil || !resp.OK {
		t.Fatalf("first contact with the boot secret: %v %+v", err, resp)
	}
	_, other, _ := ed25519.GenerateKey(nil)
	if _, _, err := keyRequest(t, addr, other, nil, proto.Request{Op: proto.OpPing}); !errors.Is(err, proto.ErrBootProof) {
		t.Fatalf("first contact with another boot's key: %v, want ErrBootProof", err)
	}
	if _, resp, err := keyRequest(t, addr, key, agent, proto.Request{Op: proto.OpPing}); err != nil || !resp.OK {
		t.Fatalf("pinned agent: %v %+v", err, resp)
	}
}

// TestExecCutStdinHangsUp cuts the client-to-agent direction at a frame boundary, before
// FrameCloseStdin, as an on-path relay could: the process must never read that as a complete input.
func TestExecCutStdinHangsUp(t *testing.T) {
	localOnly(t)
	s := dialAgent(t, proto.Request{Op: proto.OpExec, Args: []string{"/bin/sh"}})
	if !s.resp.OK {
		t.Fatalf("exec: %+v", s.resp)
	}
	s.send(proto.FrameStdin, []byte("echo first\necho sec"))
	s.conn.(*net.TCPConn).CloseWrite()
	stdout, _, status := s.wait()
	if status != -1 || strings.Contains(stdout, "sec") {
		t.Fatalf("cut input ran to the end: stdout %q, status %d", stdout, status)
	}

	dir := t.TempDir()
	got, eof := filepath.Join(dir, "got"), filepath.Join(dir, "eof")
	s = dialAgent(t, proto.Request{Op: proto.OpExec, Args: []string{"/bin/sh", "-c", "trap '' HUP; cat > " + got + "; echo > " + eof}})
	if !s.resp.OK {
		t.Fatalf("exec: %+v", s.resp)
	}
	s.send(proto.FrameStdin, []byte("partial"))
	waitFor(t, func() bool { b, _ := os.ReadFile(got); return string(b) == "partial" })
	start := time.Now()
	s.conn.(*net.TCPConn).CloseWrite()
	if _, _, status := s.wait(); status != -1 {
		t.Fatalf("status %d after the cut, want no exit frame", status)
	}
	time.Sleep(hangupGrace + 500*time.Millisecond - time.Since(start))
	if _, err := os.Stat(eof); err == nil {
		t.Fatal("a process ignoring SIGHUP read the cut input as a clean end of input")
	}
}

func TestMergeEnvHidesValues(t *testing.T) {
	_, err := mergeEnv(nil, []string{"API_TOKEN=hunter2\x00"})
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "API_TOKEN") {
		t.Fatalf("%v", err)
	}
}

func TestListenHost(t *testing.T) {
	if got := listenHost("10.0.2.15"); got != "10.0.2.15" {
		t.Fatalf("kido_ip 10.0.2.15: %s", got)
	}
	if got := listenHost(""); got != "0.0.0.0" {
		t.Fatalf("no kido_ip: %s", got)
	}
}
