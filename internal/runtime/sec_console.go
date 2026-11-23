package runtime

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// kido is daemonless, so the guest console cannot go straight to a file: a guest that writes to it
// forever would fill the host disk. QEMU writes the console to its stdout, a pipe read by a small
// process of our own (this binary, re-run with consolePumpEnv set), which keeps console.log and
// console.log.1 at most LogMax bytes each and exits when QEMU does.
const (
	consolePumpEnv = "KIDO_INTERNAL_CONSOLE_PUMP"
	LogMax         = 2 << 20
)

func init() {
	if dir := os.Getenv(consolePumpEnv); dir != "" {
		w, err := OpenCappedLog(filepath.Join(dir, "console.log"))
		if err != nil {
			os.Exit(1)
		}
		io.Copy(w, os.Stdin)
		w.Close()
		os.Exit(0)
	}
}

// startConsolePump starts the console writer for the container directory dir and returns the
// pipe that QEMU must use as its stdout.
func startConsolePump(dir string) (*os.File, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	cmd := exec.Command(exe)
	cmd.Env = []string{consolePumpEnv + "=" + dir}
	cmd.Stdin = pr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		pw.Close()
		return nil, err
	}
	cmd.Process.Release()
	return pw, nil
}

// cappedLog appends to path; when the file reaches LogMax it becomes path.1 (replacing the older
// one) and a new path is started.
type cappedLog struct {
	path string
	f    *os.File
	size int64
}

// OpenCappedLog opens path (0600) for appending, bounded as described at cappedLog.
func OpenCappedLog(path string) (io.WriteCloser, error) {
	l := &cappedLog{path: path}
	return l, l.open()
}

func (l *cappedLog) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *cappedLog) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		if l.size >= LogMax {
			l.f.Close()
			if err := os.Rename(l.path, l.path+".1"); err != nil {
				return n, err
			}
			if err := l.open(); err != nil {
				return n, err
			}
		}
		chunk := p
		if room := LogMax - l.size; int64(len(chunk)) > room {
			chunk = chunk[:room]
		}
		w, err := l.f.Write(chunk)
		n += w
		l.size += int64(w)
		p = p[w:]
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (l *cappedLog) Close() error { return l.f.Close() }
