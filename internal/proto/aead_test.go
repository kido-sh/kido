package proto

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"testing"
	"time"
)

func channelPair(t *testing.T, secret []byte, transcript [32]byte) (cli, agt *Channel, c2a, a2c *bytes.Buffer) {
	t.Helper()
	c2a, a2c = new(bytes.Buffer), new(bytes.Buffer)
	cli, err := newChannel(secret, transcript, true, c2a, a2c)
	if err != nil {
		t.Fatal(err)
	}
	agt, err = newChannel(secret, transcript, false, a2c, c2a)
	if err != nil {
		t.Fatal(err)
	}
	return cli, agt, c2a, a2c
}

func randomPair(t *testing.T) (cli, agt *Channel, c2a, a2c *bytes.Buffer) {
	secret := make([]byte, 32)
	rand.Read(secret)
	var tr [32]byte
	rand.Read(tr[:])
	return channelPair(t, secret, tr)
}

func mustWrite(t *testing.T, c *Channel, typ byte, p string) []byte {
	t.Helper()
	w := c.w.(*bytes.Buffer)
	before := w.Len()
	if err := c.WriteFrame(typ, []byte(p)); err != nil {
		t.Fatal(err)
	}
	return bytes.Clone(w.Bytes()[before:])
}

func TestChannelFrames(t *testing.T) {
	cli, agt, _, _ := randomPair(t)
	big := bytes.Repeat([]byte{0xa5}, MaxFrame)
	for _, p := range [][]byte{nil, []byte("ls\n"), big} {
		if err := cli.WriteFrame(FrameStdin, p); err != nil {
			t.Fatal(err)
		}
		typ, got, err := agt.ReadFrame()
		if err != nil || typ != FrameStdin || !bytes.Equal(got, p) {
			t.Fatalf("frame of %d bytes: %v type %d, %d bytes", len(p), err, typ, len(got))
		}
	}
	mustWrite(t, agt, FrameExit, string(ExitPayload(3)))
	if typ, got, err := cli.ReadFrame(); err != nil || typ != FrameExit || !bytes.Equal(got, ExitPayload(3)) {
		t.Fatalf("exit: %v %d %x", err, typ, got)
	}
	if err := cli.WriteFrame(FrameStdin, make([]byte, MaxFrame+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversized frame written: %v", err)
	}
}

func TestChannelHidesPayload(t *testing.T) {
	cli, _, c2a, _ := randomPair(t)
	mustWrite(t, cli, FrameStdin, "PASSWORD=hunter2")
	if bytes.Contains(c2a.Bytes(), []byte("hunter2")) {
		t.Fatal("payload in the clear")
	}
}

func TestChannelRejectsTampering(t *testing.T) {
	const payload = "rm -rf /tmp/x\n"
	for _, off := range []int{0, 4, 5, 9, 5 + len(payload), 5 + len(payload) + 15} {
		cli, agt, c2a, _ := randomPair(t)
		mustWrite(t, cli, FrameStdin, payload)
		c2a.Bytes()[off] ^= 0x01
		if _, p, err := agt.ReadFrame(); err == nil {
			t.Fatalf("bit flipped at %d: frame accepted: %q", off, p)
		} else if off != 4 && !errors.Is(err, ErrFrameAuth) {
			t.Fatalf("bit flipped at %d: %v, want ErrFrameAuth", off, err)
		}
		mustWrite(t, cli, FrameStdin, "next")
		if _, _, err := agt.ReadFrame(); err == nil {
			t.Fatalf("bit flipped at %d: the channel kept reading after a bad frame", off)
		}
	}
}

func TestChannelRejectsReplayReorderDropAndReflection(t *testing.T) {
	cases := map[string]func(t *testing.T, cli, agt *Channel, c2a, a2c *bytes.Buffer){
		"replay": func(t *testing.T, cli, agt *Channel, c2a, _ *bytes.Buffer) {
			one := mustWrite(t, cli, FrameStdin, "one")
			if _, _, err := agt.ReadFrame(); err != nil {
				t.Fatal(err)
			}
			c2a.Write(one)
		},
		"reorder": func(t *testing.T, cli, _ *Channel, c2a, _ *bytes.Buffer) {
			one := mustWrite(t, cli, FrameStdin, "one")
			two := mustWrite(t, cli, FrameStdin, "two")
			c2a.Reset()
			c2a.Write(two)
			c2a.Write(one)
		},
		"drop": func(t *testing.T, cli, _ *Channel, c2a, _ *bytes.Buffer) {
			mustWrite(t, cli, FrameStdin, "one")
			two := mustWrite(t, cli, FrameStdin, "two")
			c2a.Reset()
			c2a.Write(two)
		},
		"reflection": func(t *testing.T, _, agt *Channel, c2a, a2c *bytes.Buffer) {
			out := mustWrite(t, agt, FrameStdout, "one")
			a2c.Reset()
			c2a.Write(out)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			cli, agt, c2a, a2c := randomPair(t)
			setup(t, cli, agt, c2a, a2c)
			if _, p, err := agt.ReadFrame(); !errors.Is(err, ErrFrameAuth) {
				t.Fatalf("got %q, %v; want ErrFrameAuth", p, err)
			}
		})
	}
}

func TestChannelRejectsWrongKey(t *testing.T) {
	secret := make([]byte, 32)
	rand.Read(secret)
	var tr [32]byte
	rand.Read(tr[:])
	cli, _, _, _ := channelPair(t, secret, tr)
	other := bytes.Clone(secret)
	other[0] ^= 1
	otherTr := tr
	otherTr[31] ^= 1
	for name, k := range map[string]struct {
		secret []byte
		tr     [32]byte
	}{"secret": {other, tr}, "transcript": {secret, otherTr}} {
		frame := mustWrite(t, cli, FrameStdin, "data")
		_, agt, in, _ := channelPair(t, k.secret, k.tr)
		in.Write(frame)
		agt.rseq = cli.wseq - 1
		if _, _, err := agt.ReadFrame(); !errors.Is(err, ErrFrameAuth) {
			t.Fatalf("other %s: %v, want ErrFrameAuth", name, err)
		}
	}
}

func TestChannelSequenceLimits(t *testing.T) {
	cli, agt, _, _ := randomPair(t)
	cli.wseq, agt.rseq = math.MaxUint64-1, math.MaxUint64-1
	mustWrite(t, cli, FrameStdin, "last")
	if _, p, err := agt.ReadFrame(); err != nil || string(p) != "last" {
		t.Fatalf("last frame: %q %v", p, err)
	}
	if err := cli.WriteFrame(FrameStdin, []byte("wrapped")); !errors.Is(err, ErrFrameSequence) {
		t.Fatalf("write past the last nonce: %v", err)
	}
	if err := cli.WriteFrame(FrameStdin, nil); !errors.Is(err, ErrFrameSequence) {
		t.Fatalf("channel kept writing after exhaustion: %v", err)
	}

	cli, agt, _, _ = randomPair(t)
	mustWrite(t, cli, FrameStdin, "x")
	agt.rseq = math.MaxUint64
	if _, _, err := agt.ReadFrame(); !errors.Is(err, ErrFrameSequence) {
		t.Fatalf("read past the last nonce: %v", err)
	}
}

func TestChannelRefusesOversizedHeader(t *testing.T) {
	_, agt, c2a, _ := randomPair(t)
	c2a.Write([]byte{FrameStdin, 0xff, 0xff, 0xff, 0xff})
	if _, _, err := agt.ReadFrame(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("%v", err)
	}
}

// Handshake tests over in-memory connections.

type peers struct {
	host     ed25519.PrivateKey
	identity ed25519.PrivateKey
	boot     []byte
}

func newPeers(t *testing.T) peers {
	t.Helper()
	_, host, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return peers{host: host, identity: identity, boot: BootSecret(host)}
}

func (p peers) hostPub() ed25519.PublicKey  { return p.host.Public().(ed25519.PublicKey) }
func (p peers) agentPub() ed25519.PublicKey { return p.identity.Public().(ed25519.PublicKey) }

type agentResult struct {
	req *Request
	ch  *Channel
	err error
}

// serveAgent answers one connection like kido-agent, writing through w, and sends the result.
func (p peers) serveAgent(conn net.Conn, w io.Writer) chan agentResult {
	res := make(chan agentResult, 1)
	go func() {
		br := bufio.NewReader(conn)
		var h Hello
		if err := ReadLine(br, &h); err != nil {
			res <- agentResult{err: err}
			return
		}
		c, ch, err := AgentChallenge(p.identity, p.boot, h, w, br)
		if err == nil {
			err = WriteLine(w, c)
		}
		if err != nil {
			conn.Close()
			res <- agentResult{err: err}
			return
		}
		req, err := ch.ReadRequest(p.hostPub())
		if errors.Is(err, ErrUnauthorized) {
			ch.WriteMessage(FrameResponse, Response{Error: err.Error()})
		} else if err == nil {
			err = ch.WriteMessage(FrameResponse, Response{OK: true, PID: 7})
		}
		if err != nil {
			conn.Close()
		}
		res <- agentResult{req, ch, err}
	}()
	return res
}

func pipe(t *testing.T) (net.Conn, net.Conn) {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	a.SetDeadline(time.Now().Add(5 * time.Second))
	b.SetDeadline(time.Now().Add(5 * time.Second))
	return a, b
}

// tap records what passes through it and flips bit 0x20 of the byte at offset flip of write
// number n (no flip when n < 0).
type tap struct {
	w        io.Writer
	n, flip  int
	writes   int
	recorded bytes.Buffer
}

func (t *tap) Write(p []byte) (int, error) {
	if t.writes == t.n {
		p = bytes.Clone(p)
		p[t.flip] ^= 0x20
	}
	t.writes++
	t.recorded.Write(p)
	return t.w.Write(p)
}

func TestHandshakeSealsRequestAndFrames(t *testing.T) {
	p := newPeers(t)
	cc, ac := pipe(t)
	agentTap := &tap{w: ac, n: -1}
	res := p.serveAgent(ac, agentTap)
	clientTap := &tap{w: cc, n: -1}
	req := Request{Op: OpExec, Args: []string{"/bin/sh"}, Env: []string{"API_TOKEN=s3cr3t-env-value"}}
	key, ch, err := ClientHandshake(clientTap, bufio.NewReader(cc), p.host, p.agentPub(), req)
	if err != nil || !bytes.Equal(key, p.agentPub()) {
		t.Fatalf("%v", err)
	}
	var resp Response
	if err := ch.ReadMessage(FrameResponse, &resp); err != nil || !resp.OK || resp.PID != 7 {
		t.Fatalf("response: %v %+v", err, resp)
	}
	r := <-res
	if r.err != nil || r.req.Env[0] != req.Env[0] {
		t.Fatalf("agent: %v %+v", r.err, r.req)
	}
	go r.ch.WriteFrame(FrameStdout, []byte("stdout-marker"))
	if _, p, err := ch.ReadFrame(); err != nil || string(p) != "stdout-marker" {
		t.Fatalf("%q %v", p, err)
	}
	go ch.WriteFrame(FrameStdin, []byte("stdin-marker"))
	if _, p, err := r.ch.ReadFrame(); err != nil || string(p) != "stdin-marker" {
		t.Fatalf("%q %v", p, err)
	}
	for _, s := range []string{"s3cr3t-env-value", "API_TOKEN", "/bin/sh", "stdin-marker"} {
		if bytes.Contains(clientTap.recorded.Bytes(), []byte(s)) {
			t.Errorf("client sent %q in the clear", s)
		}
	}
	if bytes.Contains(agentTap.recorded.Bytes(), []byte("stdout-marker")) {
		t.Error("agent sent its output in the clear")
	}
}

func TestHandshakeTamperedRequest(t *testing.T) {
	for _, off := range []int{0, 5, 40} {
		p := newPeers(t)
		cc, ac := pipe(t)
		res := p.serveAgent(ac, ac)
		_, ch, err := ClientHandshake(&tap{w: cc, n: 1, flip: off}, bufio.NewReader(cc), p.host, nil, Request{Op: OpPing})
		if err != nil {
			t.Fatal(err)
		}
		var resp Response
		if err := ch.ReadMessage(FrameResponse, &resp); err == nil {
			t.Fatalf("flip at %d: got a response %+v", off, resp)
		}
		if r := <-res; !errors.Is(r.err, ErrFrameAuth) {
			t.Fatalf("flip at %d: agent: %v, want ErrFrameAuth", off, r.err)
		}
	}
}

func TestHandshakeWrongHostKey(t *testing.T) {
	p := newPeers(t)
	_, other, _ := ed25519.GenerateKey(nil)
	cc, ac := pipe(t)
	res := p.serveAgent(ac, ac)
	_, ch, err := ClientHandshake(cc, bufio.NewReader(cc), other, p.agentPub(), Request{Op: OpPing})
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := ch.ReadMessage(FrameResponse, &resp); err != nil || resp.OK || resp.Error != ErrUnauthorized.Error() {
		t.Fatalf("%v %+v", err, resp)
	}
	if r := <-res; !errors.Is(r.err, ErrUnauthorized) {
		t.Fatalf("agent: %v", r.err)
	}
}

func TestHandshakePinnedIdentity(t *testing.T) {
	p := newPeers(t)
	impostor := newPeers(t)
	cc, ac := pipe(t)
	impostor.serveAgent(ac, ac)
	if _, _, err := ClientHandshake(cc, bufio.NewReader(cc), p.host, p.agentPub(), Request{Op: OpPing}); !errors.Is(err, ErrAgentIdentity) {
		t.Fatalf("%v", err)
	}
}

// TestHandshakeKeySubstitution relays a real agent and swaps one side's ephemeral key for the
// relay's own, which would let it read and inject frames.
func TestHandshakeKeySubstitution(t *testing.T) {
	for _, side := range []string{"client", "agent"} {
		p := newPeers(t)
		cc, mc := pipe(t)
		ma, ac := pipe(t)
		p.serveAgent(ac, ac)
		go func() {
			mine := make([]byte, 32)
			rand.Read(mine)
			fromClient, fromAgent := bufio.NewReader(mc), bufio.NewReader(ma)
			var h Hello
			if ReadLine(fromClient, &h) != nil {
				return
			}
			if side == "client" {
				h.Eph = mine
			}
			WriteLine(ma, h)
			var c Challenge
			if ReadLine(fromAgent, &c) != nil {
				return
			}
			if side == "agent" {
				c.Eph = mine
			}
			WriteLine(mc, c)
			go io.Copy(ma, fromClient)
			io.Copy(mc, fromAgent)
		}()
		if _, _, err := ClientHandshake(cc, bufio.NewReader(cc), p.host, p.agentPub(), Request{Op: OpPing}); !errors.Is(err, ErrAgentIdentity) {
			t.Fatalf("%s key swapped: %v, want ErrAgentIdentity", side, err)
		}
	}
}

// TestFirstContactNeedsBootProof plays agents that know everything public about the boot (the host
// key, the port) but not the boot secret.
func TestFirstContactNeedsBootProof(t *testing.T) {
	p := newPeers(t)
	guessed := make([]byte, 32)
	rand.Read(guessed)
	for name, boot := range map[string][]byte{"no secret": nil, "guessed secret": guessed, "real secret": p.boot} {
		impostor := p
		impostor.boot = boot
		cc, ac := pipe(t)
		impostor.serveAgent(ac, ac)
		_, _, err := ClientHandshake(cc, bufio.NewReader(cc), p.host, nil, Request{Op: OpPing})
		if name == "real secret" {
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		} else if !errors.Is(err, ErrBootProof) {
			t.Fatalf("%s: %v, want ErrBootProof", name, err)
		}
	}
	cc, ac := pipe(t)
	impostor := p
	impostor.boot = nil
	impostor.serveAgent(ac, ac)
	if _, _, err := ClientHandshake(cc, bufio.NewReader(cc), p.host, p.agentPub(), Request{Op: OpPing}); err != nil {
		t.Fatalf("a pinned agent needs no boot proof: %v", err)
	}
}

// TestBootProofBoundToIdentity relays the first contact to the real agent and answers with its own
// identity and ephemeral key next to the real agent's proof.
func TestBootProofBoundToIdentity(t *testing.T) {
	p := newPeers(t)
	other := newPeers(t)
	cc, mc := pipe(t)
	ma, ac := pipe(t)
	p.serveAgent(ac, ac)
	go func() {
		fromClient, fromAgent := bufio.NewReader(mc), bufio.NewReader(ma)
		var h Hello
		if ReadLine(fromClient, &h) != nil {
			return
		}
		WriteLine(ma, h)
		var real Challenge
		if ReadLine(fromAgent, &real) != nil {
			return
		}
		mine, _, err := AgentChallenge(other.identity, nil, h, io.Discard, nil)
		if err != nil {
			return
		}
		mine.Proof = real.Proof
		WriteLine(mc, mine)
	}()
	if _, _, err := ClientHandshake(cc, bufio.NewReader(cc), p.host, nil, Request{Op: OpPing}); !errors.Is(err, ErrBootProof) {
		t.Fatalf("%v, want ErrBootProof", err)
	}
}

func TestHandshakeOlderAgent(t *testing.T) {
	p := newPeers(t)
	cc, ac := pipe(t)
	go func() {
		br := bufio.NewReader(ac)
		var h Hello
		if ReadLine(br, &h) != nil {
			return
		}
		an := NewNonce()
		WriteLine(ac, Challenge{Nonce: an, Key: p.agentPub(), Sig: ed25519.Sign(p.identity, message("kido-auth-v1 agent\x00", h.Nonce, an))})
	}()
	_, _, err := ClientHandshake(cc, bufio.NewReader(cc), p.host, nil, Request{Op: OpPing})
	if !errors.Is(err, ErrOlderAgent) {
		t.Fatalf("%v", err)
	}
}

func TestHelloCarriesEphemeralKey(t *testing.T) {
	cc, ac := pipe(t)
	got := make(chan Hello, 1)
	go func() {
		var h Hello
		ReadLine(bufio.NewReader(ac), &h)
		got <- h
		ac.Close()
	}()
	_, key, _ := ed25519.GenerateKey(nil)
	ClientHandshake(cc, bufio.NewReader(cc), key, nil, Request{Op: OpPing})
	h := <-got
	b, _ := json.Marshal(h)
	if h.Op != OpHello || len(h.Nonce) != NonceSize || len(h.Eph) != 32 {
		t.Fatalf("hello %s", b)
	}
}

func BenchmarkChannel32K(b *testing.B) {
	secret := make([]byte, 32)
	var tr [32]byte
	c2a := new(bytes.Buffer)
	cli, _ := newChannel(secret, tr, true, c2a, nil)
	agt, _ := newChannel(secret, tr, false, nil, c2a)
	p := make([]byte, 32<<10)
	b.SetBytes(int64(len(p)))
	for b.Loop() {
		cli.WriteFrame(FrameStdin, p)
		if _, _, err := agt.ReadFrame(); err != nil {
			b.Fatal(err)
		}
	}
}
