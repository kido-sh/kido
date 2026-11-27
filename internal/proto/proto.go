// Package proto is the wire protocol between the kido CLI on the host and kido-agent in the guest.
//
// A connection carries one request. After the handshake of auth.go every byte travels in sealed
// frames (aead.go): the client sends the Request as JSON in a FrameRequest, the agent answers with
// the Response as JSON in a FrameResponse. For "exec", frames follow in both directions until the
// agent sends FrameExit and closes the connection. For "notify", whose Args name the live shares,
// the client then sends FrameNotify frames until it closes the connection.
package proto

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	Version     = "2"
	DefaultPort = 1023
	MaxLine     = 1 << 16
	MaxFrame    = 1 << 20
)

const (
	OpPing     = "ping"
	OpExec     = "exec"
	OpShutdown = "shutdown"
	OpNotify   = "notify"
)

type Request struct {
	Op   string   `json:"op"`
	Args []string `json:"args,omitempty"`
	Env  []string `json:"env,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	User string   `json:"user,omitempty"`
	TTY  bool     `json:"tty,omitempty"`
	Rows uint16   `json:"rows,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
}

type Response struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

const (
	FrameStdin      byte = 0 // client -> agent: bytes for the process's stdin (or the PTY)
	FrameStdout     byte = 1 // agent -> client
	FrameStderr     byte = 2 // agent -> client (never sent for TTY sessions)
	FrameExit       byte = 3 // agent -> client: int32 big-endian status, 128+N when killed by signal N
	FrameCloseStdin byte = 4 // client -> agent: stdin reached EOF
	FrameSignal     byte = 5 // client -> agent: one byte, the signal number
	FrameResize     byte = 6 // client -> agent: uint16 rows, uint16 cols, big-endian
	FrameRequest    byte = 7 // client -> agent, first sealed frame: SignedRequest JSON
	FrameResponse   byte = 8 // agent -> client, first sealed frame: Response JSON
	FrameNotify     byte = 9 // client -> agent: one share's batch (internal/notify), or empty to keep the session
)

var ErrFrameTooLarge = errors.New("proto: frame too large")

func WriteLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) >= MaxLine {
		return fmt.Errorf("proto: line of %d bytes exceeds %d", len(b), MaxLine)
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadLine reads one JSON line into v. Use the same bufio.Reader for the frames that follow.
func ReadLine(r *bufio.Reader, v any) error {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return err
		}
		line = append(line, chunk...)
		if len(line) >= MaxLine {
			return fmt.Errorf("proto: line exceeds %d bytes", MaxLine)
		}
		if !isPrefix {
			break
		}
	}
	return json.Unmarshal(line, v)
}

func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return ErrFrameTooLarge
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, nil, ErrFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

func ExitPayload(status int32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(status))
	return b[:]
}

func ParseExit(p []byte) (int32, error) {
	if len(p) != 4 {
		return 0, fmt.Errorf("proto: exit frame of %d bytes", len(p))
	}
	return int32(binary.BigEndian.Uint32(p)), nil
}

func ResizePayload(rows, cols uint16) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[:2], rows)
	binary.BigEndian.PutUint16(b[2:], cols)
	return b[:]
}

func ParseResize(p []byte) (rows, cols uint16, err error) {
	if len(p) != 4 {
		return 0, 0, fmt.Errorf("proto: resize frame of %d bytes", len(p))
	}
	return binary.BigEndian.Uint16(p[:2]), binary.BigEndian.Uint16(p[2:]), nil
}
