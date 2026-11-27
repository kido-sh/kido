//go:build darwin

// Command kido-agent is the guest daemon of kido: it runs commands in the VM on behalf of the CLI.
package main

import (
	"bufio"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kido-sh/kido/internal/proto"

	"golang.org/x/sys/unix"
)

const (
	maxConns = 256
	rbHalt   = 0x08 // RB_HALT from <sys/reboot.h>
)

type server struct {
	hostKey  ed25519.PublicKey  // verifies requests
	identity ed25519.PrivateKey // proves this agent to the host; memory only
	boot     []byte             // this boot's secret, proven at the host's first contact
	pending  pending
	conns    chan struct{}
	refusals rateLog
	halt     sync.Once
	poweroff func()

	mounts     mountTable
	notifyMu   sync.Mutex
	notifyConn net.Conn   // the one notify session
	notifySend sync.Mutex // one batch at a time, also while a new session replaces the old
	notifyLog  rateLog
}

func main() {
	listen := flag.String("listen", "", "listen address (default 0.0.0.0:<kido_port> from kern.bootargs)")
	key := flag.String("key", "", "host public key, base64url (default kido_key from kern.bootargs)")
	secret := flag.String("secret", "", "boot secret, base64url (default kido_secret from kern.bootargs)")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("kido-agent: ")

	ba := bootArgs()
	if *key == "" {
		*key = ba.key
	}
	if *secret == "" {
		*secret = ba.secret
	}
	s := &server{
		conns:    make(chan struct{}, maxConns),
		poweroff: poweroff,
		mounts:   sysMounts{},
	}
	pub, err := proto.ParseKey(*key)
	if err != nil {
		log.Fatalf("kido_key: %v (the host passes it in the boot-args; --key for tests)", err)
	}
	s.hostKey = pub
	if *secret == "" {
		log.Printf("no kido_secret in the boot-args: kido will not accept this agent at its first contact")
	} else if s.boot, err = proto.ParseSecret(*secret); err != nil {
		log.Fatalf("kido_secret: %v", err)
	}
	if _, s.identity, err = ed25519.GenerateKey(nil); err != nil {
		log.Fatal(err)
	}
	if *listen == "" {
		if ba.port >= 1024 {
			log.Fatalf("kido_port=%d: the agent port must be below 1024, so that guest users cannot bind it next to the agent", ba.port)
		}
		*listen = net.JoinHostPort(listenHost(ba.ip), strconv.Itoa(ba.port))
	}

	ln, err := listenWhenUp(*listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s (protocol %s, key authentication, sealed channel)", ln.Addr(), proto.Version)
	for {
		conn, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			log.Printf("accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if old := s.pending.add(conn); old != nil {
			s.refusals.printf("closing %s: %d newer connections are authenticating", old.RemoteAddr(), maxPreAuth)
			old.Close()
		}
		go s.serve(conn)
	}
}

type bootConfig struct {
	key    string
	secret string
	ip     string
	port   int
}

// listenHost is the guest's own address from kido_ip. Bound there, the agent gets the host's
// connections even if a guest user holds the wildcard address on the agent port: only root may bind
// a specific address below 1024, and the specific socket wins over the wildcard one.
func listenHost(ip string) string {
	if ip == "" {
		return "0.0.0.0"
	}
	if a := net.ParseIP(ip); a == nil || a.To4() == nil {
		log.Fatalf("kido_ip=%q is not an IPv4 address", ip)
	}
	return ip
}

// listenWhenUp listens on addr, waiting for the boot to configure the address if it has not yet.
func listenWhenUp(addr string) (net.Listener, error) {
	deadline := time.Now().Add(time.Minute)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil || !errors.Is(err, syscall.EADDRNOTAVAIL) || time.Now().After(deadline) {
			return ln, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// bootArgs reads kido_key, kido_secret, kido_ip and kido_port from kern.bootargs; the port
// defaults to proto.DefaultPort.
func bootArgs() bootConfig {
	c := bootConfig{port: proto.DefaultPort}
	args, err := unix.Sysctl("kern.bootargs")
	if err != nil {
		return c
	}
	for _, f := range strings.Fields(args) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "kido_key":
			c.key = v
		case "kido_secret":
			c.secret = v
		case "kido_ip":
			c.ip = v
		case "kido_port":
			if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 {
				c.port = p
			}
		}
	}
	return c
}

// serve authenticates conn within preAuthTimeout (unless newer connections evict it first), then
// runs its request as one of at most maxConns sessions.
func (s *server) serve(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(preAuthTimeout))
	br := bufio.NewReaderSize(conn, 4<<10)
	req, ch, err := s.authenticate(conn, br)
	s.pending.remove(conn)
	if err != nil {
		s.refusals.printf("%s: %v", conn.RemoteAddr(), err)
		return
	}
	if br.Buffered() == 0 {
		ch.SetReader(bufio.NewReaderSize(conn, 64<<10))
	}
	respond := func(r proto.Response) { ch.WriteMessage(proto.FrameResponse, r) }
	select {
	case s.conns <- struct{}{}:
		defer func() { <-s.conns }()
	default:
		s.refusals.printf("refusing %s: %d sessions open", conn.RemoteAddr(), maxConns)
		respond(proto.Response{Error: "too many sessions"})
		return
	}
	conn.SetDeadline(time.Time{})

	switch req.Op {
	case proto.OpPing:
		respond(proto.Response{OK: true, Version: proto.Version})
	case proto.OpExec:
		s.exec(conn, ch, req)
	case proto.OpNotify:
		s.notify(conn, ch, req, respond)
	case proto.OpShutdown:
		log.Printf("shutdown requested")
		respond(proto.Response{OK: true, Version: proto.Version})
		conn.Close()
		s.halt.Do(func() { go s.poweroff() })
	default:
		respond(proto.Response{Error: fmt.Sprintf("unknown op %q", req.Op)})
	}
}

func poweroff() {
	run := func(argv ...string) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			log.Printf("%s: %v", argv[0], err)
			return
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				log.Printf("%s: %v", argv[0], err)
			}
		case <-time.After(10 * time.Second):
			log.Printf("%s: still running after 10 s", argv[0])
		}
	}
	time.Sleep(100 * time.Millisecond)
	run("/bin/sync")
	run("/bin/poweroff")
	time.Sleep(5 * time.Second)
	log.Printf("still running 5 s after poweroff; halting")
	unix.Sync()
	syscall.Syscall(syscall.SYS_REBOOT, rbHalt, 0, 0)
}
