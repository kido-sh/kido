package proto

import (
	"bufio"
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Key authentication and the sealed channel. Every boot the host makes an ed25519 key pair and gives
// the guest only the public key (boot-arg kido_key); the agent has its own identity key, which the
// host records at the first ping after boot. Per connection, each side with a fresh X25519 key:
//
//	client -> agent  Hello{op "hello", nonce C, eph Ec}
//	agent  -> client Challenge{nonce A, key, eph Ea, sig: agent key over AgentDomain+T,
//	                           proof: HMAC-SHA256(S, BootProofDomain+T)}
//	client -> agent  sealed FrameRequest {"sig": host key over RequestDomain+T+req, "req": req}
//	agent  -> client sealed FrameResponse, then sealed frames
//
// T is the transcript hash over C, Ec, A, Ea and the agent key; the host's signature over it proves
// the host. Each direction has its own AES-256-GCM key, HKDF-SHA256 of the X25519 secret with T as
// salt (see Channel). The client checks the agent before it sends the request, the request (with its
// environment) never travels in the clear, and someone who relays the connection has neither
// ephemeral private key.
//
// S is the boot secret: derived from the boot's host key, handed to the guest in the boot-args and
// kept out of every host argv. The host pins the agent identity at its first contact after boot only
// with a valid proof, so a process that answers on the agent's address first (another vz guest, a
// local user) cannot get its own key pinned. Guest users can read S from kern.bootargs, but none
// runs before the first contact, and afterwards the pinned identity is what counts.
const (
	OpHello       = "hello"
	NonceSize     = 32
	AgentDomain   = "kido-auth-v2 agent\x00"
	RequestDomain = "kido-auth-v2 request\x00"
	// NeedsNewerCLI is sent to a client that starts with a request, or with a hello that has no
	// ephemeral key.
	NeedsNewerCLI    = "this kido-agent needs a newer kido; upgrade the kido CLI"
	BootSecretDomain = "kido-boot-secret-v1\x00"
	BootProofDomain  = "kido-boot-proof-v1\x00"
)

// ErrNoChallenge wraps a failure to send the hello or read the challenge: the agent has seen at
// most the hello, so a new connection may retry.
var ErrNoChallenge = errors.New("no challenge from the agent")

var ErrAgentIdentity = errors.New("the guest agent did not prove the identity recorded at boot (another process may hold its port); restart the container")

// ErrOlderAgent is an agent that answers the hello without an ephemeral key.
var ErrOlderAgent = errors.New("the guest runs an older kido-agent that cannot encrypt its channel; rebake or re-import the image")

// ErrBootProof is a first contact whose agent does not prove the boot secret.
var ErrBootProof = errors.New("the guest agent did not prove this boot's secret: the image has an older kido-agent (rebake or re-import it) or another process answered on its port")

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrBadRequest   = errors.New("bad request")
)

type Hello struct {
	Op    string `json:"op"`
	Nonce []byte `json:"nonce"`
	Eph   []byte `json:"eph,omitempty"`
}

// Challenge is the agent's answer to a Hello. A refusal is a Challenge with only Error set.
type Challenge struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Nonce []byte `json:"nonce,omitempty"`
	Key   []byte `json:"key,omitempty"`
	Eph   []byte `json:"eph,omitempty"`
	Sig   []byte `json:"sig,omitempty"`
	Proof []byte `json:"proof,omitempty"`
}

// SignedRequest carries a Request as the exact bytes that were signed.
type SignedRequest struct {
	Sig []byte          `json:"sig"`
	Req json.RawMessage `json:"req"`
}

// EncodeKey is the boot-args form of a public key.
func EncodeKey(k ed25519.PublicKey) string { return base64.RawURLEncoding.EncodeToString(k) }

func ParseKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not a base64url ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}

// BootSecret is the boot secret S of the boot whose host key is key.
func BootSecret(key ed25519.PrivateKey) []byte {
	m := hmac.New(sha256.New, key.Seed())
	m.Write([]byte(BootSecretDomain))
	return m.Sum(nil)
}

// EncodeSecret is the boot-args form of a boot secret.
func EncodeSecret(s []byte) string { return base64.RawURLEncoding.EncodeToString(s) }

func ParseSecret(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != sha256.Size {
		return nil, errors.New("not a base64url boot secret")
	}
	return b, nil
}

func bootProof(secret []byte, t [sha256.Size]byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(BootProofDomain))
	m.Write(t[:])
	return m.Sum(nil)
}

func NewNonce() []byte {
	n := make([]byte, NonceSize)
	rand.Read(n)
	return n
}

func message(domain string, parts ...[]byte) []byte {
	m := []byte(domain)
	for _, p := range parts {
		m = append(m, p...)
	}
	return m
}

// AgentChallenge answers hello h for the agent with identity key identity and boot secret boot (nil:
// no proof). It returns the challenge to send and the channel for everything after it, writing to w
// and reading from r.
func AgentChallenge(identity ed25519.PrivateKey, boot []byte, h Hello, w io.Writer, r io.Reader) (Challenge, *Channel, error) {
	if len(h.Nonce) != NonceSize {
		return Challenge{}, nil, errors.New("hello without a nonce")
	}
	peer, err := ecdh.X25519().NewPublicKey(h.Eph)
	if err != nil {
		return Challenge{}, nil, fmt.Errorf("hello: %w", err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Challenge{}, nil, err
	}
	secret, err := eph.ECDH(peer)
	if err != nil {
		return Challenge{}, nil, fmt.Errorf("hello: %w", err)
	}
	an, ea := NewNonce(), eph.PublicKey().Bytes()
	key := identity.Public().(ed25519.PublicKey)
	t := transcriptHash(h.Nonce, h.Eph, an, ea, key)
	ch, err := newChannel(secret, t, false, w, r)
	if err != nil {
		return Challenge{}, nil, err
	}
	c := Challenge{Nonce: an, Key: key, Eph: ea, Sig: ed25519.Sign(identity, message(AgentDomain, t[:]))}
	if boot != nil {
		c.Proof = bootProof(boot, t)
	}
	return c, ch, nil
}

// ReadRequest opens the sealed request that follows the challenge and checks the host's signature on
// it. A request that does not open returns the channel's error and deserves no answer.
func (c *Channel) ReadRequest(host ed25519.PublicKey) (*Request, error) {
	typ, p, err := c.readFrame(MaxLine - 1)
	if err != nil {
		return nil, err
	}
	if typ != FrameRequest {
		c.rerr = fmt.Errorf("proto: got frame type %d, want a request", typ)
		return nil, c.rerr
	}
	var sr SignedRequest
	if err := json.Unmarshal(p, &sr); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	if len(sr.Sig) != ed25519.SignatureSize || len(sr.Req) == 0 ||
		!ed25519.Verify(host, message(RequestDomain, c.transcript[:], sr.Req), sr.Sig) {
		return nil, ErrUnauthorized
	}
	var req Request
	if err := json.Unmarshal(sr.Req, &req); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	return &req, nil
}

// ClientHandshake authenticates a new connection, agrees on its keys and sends req sealed. agent is
// the agent identity recorded at boot; nil (the first contact) accepts an agent that proves its key
// and the boot secret of key. It returns the agent's key and the channel for the response and the
// frames.
func ClientHandshake(w io.Writer, br *bufio.Reader, key ed25519.PrivateKey, agent ed25519.PublicKey, req Request) (ed25519.PublicKey, *Channel, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	cn, ec := NewNonce(), eph.PublicKey().Bytes()
	if err := WriteLine(w, Hello{Op: OpHello, Nonce: cn, Eph: ec}); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrNoChallenge, err)
	}
	var c Challenge
	if err := ReadLine(br, &c); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrNoChallenge, err)
	}
	if len(c.Nonce) != NonceSize || len(c.Key) != ed25519.PublicKeySize {
		if c.Error != "" {
			return nil, nil, fmt.Errorf("agent refused the connection: %q", c.Error)
		}
		return nil, nil, errors.New("agent sent no challenge")
	}
	if len(c.Eph) == 0 {
		return nil, nil, ErrOlderAgent
	}
	peer, err := ecdh.X25519().NewPublicKey(c.Eph)
	if err != nil {
		return nil, nil, ErrAgentIdentity
	}
	t := transcriptHash(cn, ec, c.Nonce, c.Eph, c.Key)
	if agent != nil && !bytes.Equal(c.Key, agent) || !ed25519.Verify(c.Key, message(AgentDomain, t[:]), c.Sig) {
		return nil, nil, ErrAgentIdentity
	}
	if agent == nil && !hmac.Equal(c.Proof, bootProof(BootSecret(key), t)) {
		return nil, nil, ErrBootProof
	}
	secret, err := eph.ECDH(peer)
	if err != nil {
		return nil, nil, ErrAgentIdentity
	}
	ch, err := newChannel(secret, t, true, w, br)
	if err != nil {
		return nil, nil, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	sig := ed25519.Sign(key, message(RequestDomain, t[:], body))
	line := fmt.Appendf(nil, `{"sig":"%s","req":%s}`, base64.StdEncoding.EncodeToString(sig), body)
	if len(line) >= MaxLine {
		return nil, nil, fmt.Errorf("proto: request of %d bytes exceeds %d", len(line), MaxLine)
	}
	if err := ch.WriteFrame(FrameRequest, line); err != nil {
		return nil, nil, err
	}
	return ed25519.PublicKey(c.Key), ch, nil
}
