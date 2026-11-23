package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/proto"
)

// startKeyAgent runs a second agent, in key mode, for the tests in this file.
func startKeyAgent(t *testing.T) (string, ed25519.PrivateKey) { return startAgent(t, true) }

// startAgent runs a second agent in key mode, given the boot secret of its host key or not.
func startAgent(t *testing.T, withSecret bool) (string, ed25519.PrivateKey) {
	t.Helper()
	localOnly(t)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--listen", "127.0.0.1:0", "--key", proto.EncodeKey(pub)}
	if withSecret {
		args = append(args, "--secret", proto.EncodeSecret(proto.BootSecret(priv)))
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "KIDO_AGENT_TEST_MAIN=1")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	sc := bufio.NewScanner(stderr)
	re := regexp.MustCompile(`listening on (\S+) .*key authentication`)
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			go io.Copy(io.Discard, stderr)
			return m[1], priv
		}
	}
	t.Fatal("key agent did not start")
	return "", nil
}

func keyRequest(t *testing.T, addr string, key ed25519.PrivateKey, agent ed25519.PublicKey, req proto.Request) (ed25519.PublicKey, proto.Response, error) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	got, ch, err := proto.ClientHandshake(conn, br, key, agent, req)
	if err != nil {
		return nil, proto.Response{}, err
	}
	var resp proto.Response
	err = ch.ReadMessage(proto.FrameResponse, &resp)
	return got, resp, err
}

func TestKeyAuth(t *testing.T) {
	addr, key := startKeyAgent(t)
	agent, resp, err := keyRequest(t, addr, key, nil, proto.Request{Op: proto.OpPing})
	if err != nil || !resp.OK {
		t.Fatalf("ping: %v %+v", err, resp)
	}
	if _, resp, err = keyRequest(t, addr, key, agent, proto.Request{Op: proto.OpPing}); err != nil || !resp.OK {
		t.Fatalf("ping with the recorded agent key: %v %+v", err, resp)
	}

	_, other, _ := ed25519.GenerateKey(nil)
	if _, resp, err = keyRequest(t, addr, other, agent, proto.Request{Op: proto.OpPing}); err != nil || resp.OK || resp.Error != "unauthorized" {
		t.Fatalf("wrong host key: %v %+v", err, resp)
	}
	impostor, _, _ := ed25519.GenerateKey(nil)
	if _, _, err = keyRequest(t, addr, key, impostor, proto.Request{Op: proto.OpPing}); !errors.Is(err, proto.ErrAgentIdentity) {
		t.Fatalf("unexpected agent key accepted: %v", err)
	}
}

// tamperWriter flips one bit of the write numbered n, at offset off, and records what it passes on.
type tamperWriter struct {
	w      io.Writer
	n, off int
	writes int
	sent   bytes.Buffer
}

func (t *tamperWriter) Write(p []byte) (int, error) {
	if t.writes == t.n {
		p = bytes.Clone(p)
		p[t.off] ^= 0x20
	}
	t.writes++
	t.sent.Write(p)
	return t.w.Write(p)
}

// handshakeThrough runs a ping handshake whose writes go through tw and returns the client's
// error reading the response.
func handshakeThrough(t *testing.T, addr string, key ed25519.PrivateKey, tw *tamperWriter) error {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	tw.w = conn
	_, ch, err := proto.ClientHandshake(tw, bufio.NewReader(conn), key, nil, proto.Request{Op: proto.OpPing})
	if err != nil {
		t.Fatal(err)
	}
	var resp proto.Response
	return ch.ReadMessage(proto.FrameResponse, &resp)
}

func TestKeyAuthRefusesReplayAndTampering(t *testing.T) {
	addr, key := startKeyAgent(t)
	clean := &tamperWriter{n: -1}
	if err := handshakeThrough(t, addr, key, clean); err != nil {
		t.Fatal(err)
	}
	for _, off := range []int{0, 1, 5, 40} {
		if err := handshakeThrough(t, addr, key, &tamperWriter{n: 1, off: off}); err == nil {
			t.Fatalf("a request with a bit flipped at %d was answered", off)
		}
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	conn.Write(clean.sent.Bytes())
	br := bufio.NewReader(conn)
	var c proto.Challenge
	if err := proto.ReadLine(br, &c); err != nil || len(c.Eph) == 0 {
		t.Fatalf("replayed hello: no challenge: %v", err)
	}
	if rest, err := io.ReadAll(br); len(rest) != 0 || isTimeout(err) {
		t.Fatalf("replayed request: got %d bytes (%v), want the connection closed without an answer", len(rest), err)
	}
}

func TestKeyAuthRefusesOldClientHello(t *testing.T) {
	addr, _ := startKeyAgent(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	proto.WriteLine(conn, proto.Hello{Op: proto.OpHello, Nonce: proto.NewNonce()})
	var c proto.Challenge
	if err := proto.ReadLine(bufio.NewReader(conn), &c); err != nil || len(c.Nonce) != 0 || c.Error != proto.NeedsNewerCLI {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestKeyAuthTellsOldClients(t *testing.T) {
	addr, _ := startKeyAgent(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	proto.WriteLine(conn, proto.Request{Op: proto.OpPing})
	var resp proto.Response
	if err := proto.ReadLine(bufio.NewReader(conn), &resp); err != nil || resp.OK || resp.Error != proto.NeedsNewerCLI {
		t.Fatalf("%v %+v", err, resp)
	}
}

func TestPreAuthLimits(t *testing.T) {
	addr, key := startKeyAgent(t)
	dial := func() net.Conn {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	closedSoon := func(c net.Conn, d time.Duration) bool {
		c.SetReadDeadline(time.Now().Add(d))
		_, err := c.Read(make([]byte, 1))
		return err != nil && !isTimeout(err)
	}

	var idle []net.Conn
	for range maxPreAuth {
		idle = append(idle, dial())
	}
	time.Sleep(100 * time.Millisecond)
	extra := dial()
	if !closedSoon(idle[0], time.Second) {
		t.Fatal("the oldest unauthenticated connection was not evicted for a new one")
	}
	if closedSoon(extra, 100*time.Millisecond) {
		t.Fatal("the new connection was refused")
	}
	var last net.Conn
	for range 3 * maxPreAuth {
		last = dial()
	}
	lastAt := time.Now()
	for i := range 5 {
		if _, resp, err := keyRequest(t, addr, key, nil, proto.Request{Op: proto.OpPing}); err != nil || !resp.OK {
			t.Fatalf("ping %d while %d idle connections are held: %v %+v", i, 4*maxPreAuth, err, resp)
		}
	}
	if !closedSoon(extra, time.Second) {
		t.Fatal("an old idle connection survived newer ones")
	}
	if !closedSoon(last, preAuthTimeout+2*time.Second) {
		t.Fatal("idle unauthenticated connection still open")
	}
	if d := time.Since(lastAt); d < preAuthTimeout-time.Second/2 || d > preAuthTimeout+time.Second {
		t.Fatalf("idle connection closed after %s, want about %s", d, preAuthTimeout)
	}

	long := dial()
	long.SetDeadline(time.Now().Add(preAuthTimeout - time.Second/2))
	go io.WriteString(long, strings.Repeat("x", proto.MaxLine+1024))
	if line, err := bufio.NewReader(long).ReadString('\n'); err == nil || isTimeout(err) {
		t.Fatalf("oversized hello: got %q, %v; want the connection closed before the deadline", line, err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
