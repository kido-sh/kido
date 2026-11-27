package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/kido-sh/kido/internal/proto"
)

const (
	preAuthTimeout = 2 * time.Second
	maxPreAuth     = 256
)

// pending holds the connections that have not authenticated yet, oldest first. Past maxPreAuth one
// is closed to make room instead of refusing the new one: the oldest that has not sent its hello,
// else the oldest. kido sends its hello with the connection and finishes within milliseconds, so
// connections that sit idle cannot lock it out.
type pending struct {
	mu    sync.Mutex
	conns []pendingConn
}

type pendingConn struct {
	c     net.Conn
	hello bool
}

// add records c and returns the connection it evicts, if any.
func (p *pending) add(c net.Conn) net.Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	var old net.Conn
	if len(p.conns) >= maxPreAuth {
		i := 0
		for j, x := range p.conns {
			if !x.hello {
				i = j
				break
			}
		}
		old = p.conns[i].c
		p.conns = append(p.conns[:i], p.conns[i+1:]...)
	}
	p.conns = append(p.conns, pendingConn{c: c})
	return old
}

// helloSeen marks c as having sent a well-formed hello.
func (p *pending) helloSeen(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.conns {
		if p.conns[i].c == c {
			p.conns[i].hello = true
			return
		}
	}
}

func (p *pending) remove(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, x := range p.conns {
		if x.c == c {
			p.conns = append(p.conns[:i], p.conns[i+1:]...)
			return
		}
	}
}

// authenticate runs the handshake of proto/auth.go on a new connection and returns its request and
// the sealed channel for the rest of the connection. A request that does not open gets no answer.
func (s *server) authenticate(conn net.Conn, br *bufio.Reader) (*proto.Request, *proto.Channel, error) {
	var h proto.Hello
	if err := proto.ReadLine(br, &h); err != nil {
		return nil, nil, fmt.Errorf("bad hello: %w", err)
	}
	if h.Op != proto.OpHello || len(h.Nonce) != proto.NonceSize || len(h.Eph) == 0 {
		proto.WriteLine(conn, proto.Response{Error: proto.NeedsNewerCLI})
		return nil, nil, errors.New("client did not start with a hello carrying an ephemeral key")
	}
	s.pending.helloSeen(conn)
	c, ch, err := proto.AgentChallenge(s.identity, s.boot, h, conn, br)
	if err != nil {
		return nil, nil, err
	}
	if err := proto.WriteLine(conn, c); err != nil {
		return nil, nil, err
	}
	req, err := ch.ReadRequest(s.hostKey)
	switch {
	case errors.Is(err, proto.ErrUnauthorized):
		ch.WriteMessage(proto.FrameResponse, proto.Response{Error: proto.ErrUnauthorized.Error()})
		return nil, nil, err
	case errors.Is(err, proto.ErrBadRequest):
		ch.WriteMessage(proto.FrameResponse, proto.Response{Error: proto.ErrBadRequest.Error()})
		return nil, nil, err
	case err != nil:
		return nil, nil, fmt.Errorf("bad request: %w", err)
	}
	return req, ch, nil
}

// rateLog drops messages that come faster than one a second, so that a flood of bad connections
// cannot fill the guest's log.
type rateLog struct {
	mu      sync.Mutex
	next    time.Time
	dropped int
}

func (l *rateLog) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Before(l.next) {
		l.dropped++
		return
	}
	l.next = now.Add(time.Second)
	msg := fmt.Sprintf(format, args...)
	if l.dropped > 0 {
		msg += fmt.Sprintf(" (and %d more like it)", l.dropped)
		l.dropped = 0
	}
	log.Print(msg)
}
