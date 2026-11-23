package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/proto"
)

// ExecError is a failure to run the process at all; Status is the Docker-style exit status
// (125 kido error, 126 cannot run, 127 not found).
type ExecError struct {
	Status int
	Msg    string
}

func (e *ExecError) Error() string { return e.Msg }

func agentAddr(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

const connectAttempts = 5

// request retries a connection reset before the challenge: QEMU's user-net port forward resets
// connections that arrive at the same moment, and the agent has then seen only the hello.
func request(ctx context.Context, port int, cred *agentCred, req proto.Request, timeout time.Duration) (net.Conn, *proto.Channel, *proto.Response, error) {
	for attempt := 1; ; attempt++ {
		conn, ch, resp, err := requestOnce(ctx, port, cred, req, timeout)
		if err == nil || attempt == connectAttempts || !errors.Is(err, proto.ErrNoChallenge) ||
			!(errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF)) {
			return conn, ch, resp, err
		}
		select {
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		case <-time.After(time.Duration(attempt*(1+rand.IntN(4))) * time.Millisecond):
		}
	}
}

func requestOnce(ctx context.Context, port int, cred *agentCred, req proto.Request, timeout time.Duration) (net.Conn, *proto.Channel, *proto.Response, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", agentAddr(port))
	if err != nil {
		return nil, nil, nil, err
	}
	conn.SetDeadline(time.Now().Add(timeout))
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	fail := func(err error) (net.Conn, *proto.Channel, *proto.Response, error) {
		conn.Close()
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, err
	}
	ch, err := cred.handshake(conn, bufio.NewReader(conn), req)
	if err != nil {
		return fail(err)
	}
	var resp proto.Response
	if err := ch.ReadMessage(proto.FrameResponse, &resp); err != nil {
		return fail(err)
	}
	if !stop() {
		return fail(ctx.Err())
	}
	conn.SetDeadline(time.Time{})
	return conn, ch, &resp, nil
}

func ping(ctx context.Context, port int, cred *agentCred, timeout time.Duration) error {
	conn, _, resp, err := request(ctx, port, cred, proto.Request{Op: proto.OpPing}, timeout)
	if err != nil {
		return err
	}
	conn.Close()
	if !resp.OK {
		return fmt.Errorf("agent refused ping: %s", resp.Error)
	}
	return nil
}

func shutdown(port int, cred *agentCred) error {
	conn, _, resp, err := request(context.Background(), port, cred, proto.Request{Op: proto.OpShutdown}, 2*time.Second)
	if err != nil {
		return err
	}
	conn.Close()
	if !resp.OK {
		return fmt.Errorf("agent refused shutdown: %s", resp.Error)
	}
	return nil
}

func (r *Runtime) Exec(ctx context.Context, ref string, p engine.ProcessSpec, stdio engine.Stdio) (int, error) {
	return r.ExecNotify(ctx, ref, p, stdio, nil)
}

// ExecNotify is Exec that calls started once the agent has started the process.
func (r *Runtime) ExecNotify(ctx context.Context, ref string, p engine.ProcessSpec, stdio engine.Stdio, started func(pid int)) (int, error) {
	info, err := r.Resolve(ref)
	if err != nil {
		return 125, err
	}
	if info.State != engine.StateRunning {
		return 125, fmt.Errorf("container %s is not running", info.Name)
	}
	cred, err := r.agentCred(info.ID)
	if err != nil {
		return 125, err
	}
	return execAt(ctx, info.AgentPort, cred, p, stdio, started)
}

func execAt(ctx context.Context, port int, cred *agentCred, p engine.ProcessSpec, stdio engine.Stdio, started func(pid int)) (int, error) {
	if len(p.Args) == 0 {
		return 125, errors.New("no command specified")
	}
	req := proto.Request{Op: proto.OpExec, Args: p.Args, Env: p.Env, Dir: p.Dir, User: p.User, TTY: p.TTY}
	if p.TTY && stdio.Resize != nil {
		select {
		case s := <-stdio.Resize:
			req.Rows, req.Cols = s.Rows, s.Cols
		default:
		}
	}
	conn, ch, resp, err := request(ctx, port, cred, req, 10*time.Second)
	if err != nil {
		return 125, fmt.Errorf("guest agent: %w", err)
	}
	defer conn.Close()
	if !resp.OK {
		return execFailure(resp.Error)
	}
	if started != nil {
		started(resp.PID)
	}

	write := ch.WriteFrame
	done := make(chan struct{})
	defer close(done)

	go func() {
		if stdio.Stdin == nil {
			write(proto.FrameCloseStdin, nil)
			return
		}
		buf := make([]byte, 32<<10)
		for {
			n, err := stdio.Stdin.Read(buf)
			if n > 0 {
				if write(proto.FrameStdin, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				write(proto.FrameCloseStdin, nil)
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				conn.Close()
				return
			case s, ok := <-stdio.Resize:
				if !ok {
					stdio.Resize = nil
					continue
				}
				write(proto.FrameResize, proto.ResizePayload(s.Rows, s.Cols))
			case sig, ok := <-stdio.Signal:
				if !ok {
					stdio.Signal = nil
					continue
				}
				if sig > 0 && sig < 256 {
					write(proto.FrameSignal, []byte{byte(sig)})
				}
			}
		}
	}()

	stdout, stderr := stdio.Stdout, stdio.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	for {
		typ, payload, err := ch.ReadFrame()
		if err != nil {
			if ctx.Err() != nil {
				return 125, ctx.Err()
			}
			return 125, fmt.Errorf("connection to the guest agent lost: %w", err)
		}
		switch typ {
		case proto.FrameStdout:
			stdout.Write(payload)
		case proto.FrameStderr:
			stderr.Write(payload)
		case proto.FrameExit:
			status, err := proto.ParseExit(payload)
			if err != nil {
				return 125, err
			}
			return int(status), nil
		}
	}
}

func execFailure(msg string) (int, error) {
	status := 125
	switch {
	case strings.Contains(msg, "executable file not found"):
		status = 127
	case strings.Contains(msg, "permission denied"), strings.Contains(msg, "exec format error"),
		strings.HasPrefix(msg, "working directory"):
		status = 126
	}
	return status, &ExecError{Status: status, Msg: Sanitize(msg)}
}
