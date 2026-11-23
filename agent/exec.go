package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kido-sh/kido/internal/proto"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const (
	defaultPath = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	rootHome    = "/var/root"
	// drainTimeout bounds how long output is still forwarded after the process exits, for
	// descendants that keep stdout or the terminal open.
	drainTimeout = 500 * time.Millisecond
	// hangupGrace is how long a hung-up process without a terminal has to exit on SIGHUP before its
	// process group is killed.
	hangupGrace = time.Second
	maxSignal   = 31
)

type execSpec struct {
	path string
	env  []string
	dir  string
	cred *syscall.Credential
}

func prepare(req *proto.Request) (*execSpec, error) {
	if len(req.Args) == 0 || req.Args[0] == "" {
		return nil, errors.New("no command given")
	}
	for _, a := range req.Args {
		if strings.IndexByte(a, 0) >= 0 {
			return nil, errors.New("argument contains a NUL byte")
		}
	}

	spec := &execSpec{dir: "/"}
	home := rootHome
	if req.User != "" {
		a, err := lookupUser(req.User)
		if err != nil {
			return nil, err
		}
		home = a.home
		if a.uid != uint32(os.Geteuid()) || a.gid != uint32(os.Getegid()) || os.Geteuid() == 0 {
			spec.cred = &syscall.Credential{Uid: a.uid, Gid: a.gid, Groups: a.groups}
		}
	}

	env := []string{"PATH=" + defaultPath, "HOME=" + home}
	if req.TTY {
		env = append(env, "TERM=xterm")
	}
	env, err := mergeEnv(env, req.Env)
	if err != nil {
		return nil, err
	}
	spec.env = env

	if req.Dir != "" {
		if !filepath.IsAbs(req.Dir) || strings.IndexByte(req.Dir, 0) >= 0 {
			return nil, fmt.Errorf("working directory %q is not an absolute path", req.Dir)
		}
		spec.dir = filepath.Clean(req.Dir)
	}
	if st, err := os.Stat(spec.dir); err != nil {
		return nil, fmt.Errorf("working directory: %w", err)
	} else if !st.IsDir() {
		return nil, fmt.Errorf("working directory %s: not a directory", spec.dir)
	}

	spec.path, err = lookPath(req.Args[0], envValue(env, "PATH"), spec.dir)
	if err != nil {
		return nil, err
	}
	return spec, nil
}

func mergeEnv(base, extra []string) ([]string, error) {
	out := append([]string(nil), base...)
	for _, kv := range extra {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.IndexByte(kv, 0) >= 0 {
			return nil, fmt.Errorf("invalid environment entry for %q", k)
		}
		replaced := false
		for i, old := range out {
			if strings.HasPrefix(old, k+"=") {
				out[i], replaced = kv, true
				break
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out, nil
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// lookPath resolves the program like execvp: a name with a slash is taken as is (relative to the
// working directory), a bare name is searched in the absolute entries of PATH.
func lookPath(file, pathEnv, dir string) (string, error) {
	if strings.Contains(file, "/") {
		p := file
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		switch err := checkExecutable(p); {
		case err == nil:
			return p, nil
		case errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("executable file not found: %q", file)
		case errors.Is(err, fs.ErrPermission):
			return "", fmt.Errorf("permission denied: %q", file)
		default:
			return "", err
		}
	}
	denied := false
	for _, d := range filepath.SplitList(pathEnv) {
		if !filepath.IsAbs(d) {
			continue
		}
		p := filepath.Join(d, file)
		switch err := checkExecutable(p); {
		case err == nil:
			return p, nil
		case errors.Is(err, fs.ErrPermission):
			denied = true
		}
	}
	if denied {
		return "", fmt.Errorf("permission denied: %q", file)
	}
	return "", fmt.Errorf("executable file not found in $PATH: %q", file)
}

func checkExecutable(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if st.IsDir() || st.Mode()&0o111 == 0 {
		return fs.ErrPermission
	}
	return nil
}

func startError(err error, file string) string {
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Op == "chdir" {
		return "working directory: " + err.Error()
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("executable file not found: %q: %v", file, err)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Sprintf("permission denied: %q: %v", file, err)
	}
	return err.Error()
}

type frameWriter struct {
	mu     sync.Mutex
	ch     *proto.Channel
	closed bool
}

func (f *frameWriter) frame(typ byte, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return net.ErrClosed
	}
	err := f.ch.WriteFrame(typ, p)
	if err != nil {
		f.closed = true
	}
	return err
}

// finish sends the exit status; no frame follows it.
func (f *frameWriter) finish(status int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return net.ErrClosed
	}
	f.closed = true
	return f.ch.WriteFrame(proto.FrameExit, proto.ExitPayload(status))
}

type session struct {
	conn net.Conn
	cmd  *exec.Cmd
	out  *frameWriter

	mu      sync.Mutex
	ptmx    *os.File // nil without a TTY
	ptmxFd  int
	exited  bool
	hungUp  bool
	stdinCh chan []byte
}

func (s *server) exec(conn net.Conn, ch *proto.Channel, req *proto.Request) {
	respond := func(r proto.Response) error { return ch.WriteMessage(proto.FrameResponse, r) }
	spec, err := prepare(req)
	if err != nil {
		log.Printf("exec %q: %v", firstArg(req), err)
		respond(proto.Response{Error: err.Error()})
		return
	}
	cmd := &exec.Cmd{
		Path:        spec.path,
		Args:        req.Args,
		Env:         spec.env,
		Dir:         spec.dir,
		SysProcAttr: &syscall.SysProcAttr{Credential: spec.cred},
	}
	sess := &session{conn: conn, cmd: cmd, out: &frameWriter{ch: ch}}

	var outputs []*os.File
	var stdin io.WriteCloser
	if req.TTY {
		ptmx, tty, err := pty.Open()
		if err != nil {
			respond(proto.Response{Error: "pty: " + err.Error()})
			return
		}
		if req.Rows > 0 && req.Cols > 0 {
			pty.Setsize(ptmx, &pty.Winsize{Rows: req.Rows, Cols: req.Cols})
		}
		if spec.cred != nil {
			tty.Chown(int(spec.cred.Uid), -1)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
		cmd.SysProcAttr.Setsid = true
		cmd.SysProcAttr.Setctty = true
		err = cmd.Start()
		tty.Close()
		if err != nil {
			ptmx.Close()
			respond(proto.Response{Error: startError(err, req.Args[0])})
			return
		}
		sess.ptmx, sess.ptmxFd = ptmx, int(ptmx.Fd())
		outputs = []*os.File{ptmx}
	} else {
		var files [6]*os.File
		for i := 0; i < 6; i += 2 {
			if files[i], files[i+1], err = os.Pipe(); err != nil {
				for _, f := range files {
					if f != nil {
						f.Close()
					}
				}
				respond(proto.Response{Error: "pipe: " + err.Error()})
				return
			}
		}
		inR, inW, outR, outW, errR, errW := files[0], files[1], files[2], files[3], files[4], files[5]
		cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
		cmd.SysProcAttr.Setpgid = true
		err = cmd.Start()
		inR.Close()
		outW.Close()
		errW.Close()
		if err != nil {
			inW.Close()
			outR.Close()
			errR.Close()
			respond(proto.Response{Error: startError(err, req.Args[0])})
			return
		}
		outputs = []*os.File{outR, errR}
		stdin = inW
	}

	pid := cmd.Process.Pid
	log.Printf("exec pid %d: %s (tty %v, user %q)", pid, req.Args[0], req.TTY, req.User)
	if err := respond(proto.Response{OK: true, PID: pid}); err != nil {
		sess.signal(syscall.SIGKILL)
	}

	var pumps sync.WaitGroup
	for i, f := range outputs {
		typ := proto.FrameStdout
		if i == 1 {
			typ = proto.FrameStderr
		}
		pumps.Add(1)
		go sess.pump(f, typ, &pumps)
	}
	sess.stdinCh = make(chan []byte, 16)
	if req.TTY {
		go stdinWriter(sess.stdinCh, sess.ptmx, nil)
	} else {
		go stdinWriter(sess.stdinCh, stdin, stdin)
	}
	go sess.readFrames(ch, req.TTY)

	cmd.Wait()
	sess.mu.Lock()
	sess.exited = true
	sess.mu.Unlock()
	status := exitStatus(cmd.ProcessState)

	drained := make(chan struct{})
	go func() {
		pumps.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(drainTimeout):
		if !req.TTY {
			for _, f := range outputs {
				f.Close()
			}
			<-drained
		}
	}
	sess.out.finish(status)
	conn.Close()
	if req.TTY {
		sess.closeOutput(outputs[0])
	}
	log.Printf("exit pid %d: status %d", pid, status)
}

func firstArg(req *proto.Request) string {
	if len(req.Args) == 0 {
		return ""
	}
	return req.Args[0]
}

func exitStatus(ps *os.ProcessState) int32 {
	if ps == nil {
		return 255
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int32(ws.Signal())
	}
	return int32(ps.ExitCode())
}

// pump forwards one output stream. When the client stops taking output the session is hung up
// and the stream closed, so that the writers get EPIPE or EIO instead of blocking forever.
func (s *session) pump(f *os.File, typ byte, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if werr := s.out.frame(typ, buf[:n]); werr != nil {
				s.hangup()
				s.closeOutput(f)
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *session) closeOutput(f *os.File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f == s.ptmx {
		s.ptmx = nil
	}
	f.Close()
}

func stdinWriter(ch <-chan []byte, w io.Writer, c io.Closer) {
	ok := true
	for b := range ch {
		if ok {
			if _, err := w.Write(b); err != nil {
				ok = false
			}
		}
	}
	if c != nil {
		c.Close()
	}
}

// readFrames handles the client's frames until the connection ends. Closing stdin on a TTY only
// stops forwarding, as with docker run -t without -i. A connection that ends while stdin is still
// open (or on a TTY) hangs the session up like a closed terminal: the input may have been cut short
// on the way, so a process without a terminal is killed rather than ever reading it as complete.
func (s *session) readFrames(ch *proto.Channel, tty bool) {
	stdinOpen := true
	closeStdin := func() {
		if stdinOpen {
			stdinOpen = false
			close(s.stdinCh)
		}
	}
	defer closeStdin()
	for {
		typ, p, err := ch.ReadFrame()
		if err != nil {
			if stdinOpen || tty || !errors.Is(err, io.EOF) {
				s.hangup()
			}
			if stdinOpen && !tty {
				s.killAfter(hangupGrace)
			}
			return
		}
		switch typ {
		case proto.FrameStdin:
			if stdinOpen && len(p) > 0 {
				s.stdinCh <- p
			}
		case proto.FrameCloseStdin:
			closeStdin()
		case proto.FrameSignal:
			if len(p) == 1 && p[0] >= 1 && p[0] <= maxSignal {
				s.signal(syscall.Signal(p[0]))
			}
		case proto.FrameResize:
			if rows, cols, err := proto.ParseResize(p); err == nil {
				s.resize(rows, cols)
			}
		default:
			log.Printf("pid %d: ignoring frame type %d", s.cmd.Process.Pid, typ)
		}
	}
}

// signal delivers sig to the terminal's foreground process group, or to the process group the
// command leads.
func (s *session) signal(sig syscall.Signal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return
	}
	pid := s.cmd.Process.Pid
	if s.ptmx != nil {
		if pgrp, err := unix.IoctlGetInt(s.ptmxFd, unix.TIOCGPGRP); err == nil && pgrp > 0 {
			if unix.Kill(-pgrp, sig) == nil {
				return
			}
		}
	}
	if unix.Kill(-pid, sig) != nil {
		unix.Kill(pid, sig)
	}
}

// killAfter gives the process d to exit after a hangup, then kills its process group.
func (s *session) killAfter(d time.Duration) {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		s.mu.Lock()
		exited := s.exited
		s.mu.Unlock()
		if exited {
			return
		}
	}
	s.signal(syscall.SIGKILL)
}

func (s *session) resize(rows, cols uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptmx == nil || rows == 0 || cols == 0 {
		return
	}
	unix.IoctlSetWinsize(s.ptmxFd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
}

func (s *session) hangup() {
	s.mu.Lock()
	first := !s.hungUp
	s.hungUp = true
	s.mu.Unlock()
	if first {
		s.signal(syscall.SIGHUP)
		s.conn.Close()
	}
}
