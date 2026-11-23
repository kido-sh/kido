package proto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
)

const (
	TranscriptDomain = "kido-aead-v1 transcript\x00"
	keyToAgent       = "kido-aead-v1 client to agent"
	keyToClient      = "kido-aead-v1 agent to client"
	frameHeaderSize  = 5
)

var (
	// ErrFrameAuth is a frame that does not open under the expected key and sequence number: it was
	// altered, dropped, replayed, reordered or reflected on the way.
	ErrFrameAuth     = errors.New("proto: a frame failed authentication; the connection was tampered with")
	ErrFrameSequence = errors.New("proto: frame sequence numbers exhausted")
)

// Channel carries the frames of one authenticated connection, each sealed with AES-256-GCM: one key
// per direction, the frame header (type and length) as additional data, and the frame's sequence
// number in its direction as the nonce. A frame that does not open ends the channel: every later
// read returns the same error. Writes may come from several goroutines, reads from one.
type Channel struct {
	wmu  sync.Mutex
	w    io.Writer
	seal cipher.AEAD
	wseq uint64
	wbuf []byte
	werr error

	r    io.Reader
	open cipher.AEAD
	rseq uint64
	rerr error

	transcript [sha256.Size]byte
}

// transcriptHash binds the session keys and both signatures to everything the handshake exchanged.
// Every part has a fixed length, so the concatenation is unambiguous.
func transcriptHash(clientNonce, clientEph, agentNonce, agentEph, agentKey []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(TranscriptDomain))
	for _, p := range [][]byte{clientNonce, clientEph, agentNonce, agentEph, agentKey} {
		h.Write(p)
	}
	var t [sha256.Size]byte
	h.Sum(t[:0])
	return t
}

func newChannel(secret []byte, transcript [sha256.Size]byte, client bool, w io.Writer, r io.Reader) (*Channel, error) {
	toAgent, err := directionAEAD(secret, transcript, keyToAgent)
	if err != nil {
		return nil, err
	}
	toClient, err := directionAEAD(secret, transcript, keyToClient)
	if err != nil {
		return nil, err
	}
	c := &Channel{w: w, r: r, transcript: transcript, seal: toClient, open: toAgent}
	if client {
		c.seal, c.open = toAgent, toClient
	}
	return c, nil
}

func directionAEAD(secret []byte, transcript [sha256.Size]byte, info string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, secret, transcript[:], info, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func seqNonce(seq uint64) []byte {
	var n [12]byte
	binary.BigEndian.PutUint64(n[4:], seq)
	return n[:]
}

// SetReader continues reading the frames from r, which must carry on the same byte stream.
func (c *Channel) SetReader(r io.Reader) { c.r = r }

// WriteFrame seals one frame and writes it with a single Write.
func (c *Channel) WriteFrame(typ byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return ErrFrameTooLarge
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.werr != nil {
		return c.werr
	}
	if c.wseq == math.MaxUint64 {
		c.werr = ErrFrameSequence
		return c.werr
	}
	var hdr [frameHeaderSize]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if n := frameHeaderSize + len(payload) + c.seal.Overhead(); cap(c.wbuf) < n {
		c.wbuf = make([]byte, 0, n)
	}
	b := c.seal.Seal(append(c.wbuf[:0], hdr[:]...), seqNonce(c.wseq), payload, hdr[:])
	c.wseq++
	if _, err := c.w.Write(b); err != nil {
		c.werr = err
		return err
	}
	return nil
}

// ReadFrame reads and opens the next frame. The payload is a new slice the caller may keep.
func (c *Channel) ReadFrame() (byte, []byte, error) { return c.readFrame(MaxFrame) }

func (c *Channel) readFrame(max uint32) (byte, []byte, error) {
	if c.rerr != nil {
		return 0, nil, c.rerr
	}
	typ, p, err := c.openNext(max)
	if err != nil {
		c.rerr = err
	}
	return typ, p, err
}

func (c *Channel) openNext(max uint32) (byte, []byte, error) {
	var hdr [frameHeaderSize]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > max {
		return 0, nil, ErrFrameTooLarge
	}
	if c.rseq == math.MaxUint64 {
		return 0, nil, ErrFrameSequence
	}
	buf := make([]byte, int(n)+c.open.Overhead())
	if _, err := io.ReadFull(c.r, buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	p, err := c.open.Open(buf[:0], seqNonce(c.rseq), buf, hdr[:])
	if err != nil {
		return 0, nil, ErrFrameAuth
	}
	c.rseq++
	return hdr[0], p, nil
}

// WriteMessage seals v as one JSON frame of type typ.
func (c *Channel) WriteMessage(typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) >= MaxLine {
		return fmt.Errorf("proto: message of %d bytes exceeds %d", len(b), MaxLine)
	}
	return c.WriteFrame(typ, b)
}

// ReadMessage reads the next frame, which must be of type typ and below MaxLine bytes, as JSON into v.
func (c *Channel) ReadMessage(typ byte, v any) error {
	t, p, err := c.readFrame(MaxLine - 1)
	if err != nil {
		return err
	}
	if t != typ {
		c.rerr = fmt.Errorf("proto: got frame type %d, want %d", t, typ)
		return c.rerr
	}
	return json.Unmarshal(p, v)
}
