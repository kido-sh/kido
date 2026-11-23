package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/runtime"
)

const ctrExecCmd = "__exec"

func init() {
	register("exec", "Execute a command in a running container", ctrExec)
	if len(os.Args) > 1 && os.Args[1] == ctrExecCmd {
		register(ctrExecCmd, "", ctrExecMain)
	}
}

func ctrOpen() (*runtime.Runtime, error) {
	return runtime.New(runtime.DefaultHome())
}

// ctrFail prints err and returns the Docker-style exit status for it.
func ctrFail(err error) int {
	fmt.Fprintln(os.Stderr, "kido:", err)
	var ee *runtime.ExecError
	if errors.As(err, &ee) {
		return ee.Status
	}
	return 125
}

type ctrList []string

func (l *ctrList) String() string     { return strings.Join(*l, ",") }
func (l *ctrList) Set(v string) error { *l = append(*l, v); return nil }

// ctrExpand splits combined one-letter boolean flags (-it, -dit) that come before the first
// positional argument; valued names the flags that take a separate value.
func ctrExpand(args []string, bools string, valued ...string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || a == "-" || !strings.HasPrefix(a, "-") {
			return append(out, args[i:]...)
		}
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			out = append(out, a)
			continue
		}
		if slices.Contains(valued, name) {
			out = append(out, a)
			if i+1 < len(args) {
				i++
				out = append(out, args[i])
			}
			continue
		}
		if !strings.HasPrefix(a, "--") && len(name) > 1 && strings.Trim(name, bools) == "" {
			for _, c := range name {
				out = append(out, "-"+string(c))
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

// ctrEnv resolves -e values: KEY=VALUE as given, a bare KEY takes the host's value when set.
func ctrEnv(in []string) []string {
	var out []string
	for _, e := range in {
		if strings.Contains(e, "=") {
			out = append(out, e)
		} else if v, ok := os.LookupEnv(e); ok {
			out = append(out, e+"="+v)
		}
	}
	return out
}

// ctrInteract runs p with this terminal's stdio: raw mode and resize frames for a TTY, and
// SIGINT/SIGTERM/SIGHUP forwarded to the process. started, when set, gets the guest pid.
func ctrInteract(ctx context.Context, rt *runtime.Runtime, ref string, p engine.ProcessSpec, interactive bool, started func(int)) (int, error) {
	stdio := engine.Stdio{Stdout: os.Stdout, Stderr: os.Stderr}
	if interactive {
		stdio.Stdin = os.Stdin
	}
	notify := []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}
	resize := make(chan engine.Size, 4)
	sendSize := func() {
		for _, fd := range []int{int(os.Stdout.Fd()), int(os.Stdin.Fd())} {
			if w, h, err := term.GetSize(fd); err == nil {
				select {
				case resize <- engine.Size{Rows: uint16(h), Cols: uint16(w)}:
				default:
				}
				return
			}
		}
	}
	if p.TTY {
		stdio.Resize = resize
		notify = append(notify, syscall.SIGWINCH)
		sendSize()
		if fd := int(os.Stdin.Fd()); interactive && term.IsTerminal(fd) {
			state, err := term.MakeRaw(fd)
			if err != nil {
				return 125, err
			}
			defer term.Restore(fd, state)
		}
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, notify...)
	defer signal.Stop(sigs)
	fwd := make(chan int, 8)
	stdio.Signal = fwd
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case s := <-sigs:
				if s == syscall.SIGWINCH {
					sendSize()
					continue
				}
				select {
				case fwd <- int(s.(syscall.Signal)):
				default:
				}
			}
		}
	}()
	return rt.ExecNotify(context.WithoutCancel(ctx), ref, p, stdio, started)
}

func ctrExec(ctx context.Context, args []string) int {
	fs := ctrFlags("exec", "[OPTIONS] CONTAINER COMMAND [ARG...]")
	interactive := fs.Bool("i", false, "keep stdin open")
	fs.BoolVar(interactive, "interactive", false, "keep stdin open")
	tty := fs.Bool("t", false, "allocate a pseudo-terminal")
	fs.BoolVar(tty, "tty", false, "allocate a pseudo-terminal")
	detach := fs.Bool("d", false, "run the command in the background (its output is discarded)")
	fs.BoolVar(detach, "detach", false, "run the command in the background (its output is discarded)")
	var env, envFiles ctrList
	fs.Var(&env, "e", "set an environment variable (KEY=VALUE, or KEY to pass the host's value)")
	fs.Var(&env, "env", "set an environment variable (KEY=VALUE, or KEY to pass the host's value)")
	fs.Var(&envFiles, "env-file", "read in a file of environment variables")
	dir := fs.String("w", "", "working directory inside the container")
	fs.StringVar(dir, "workdir", "", "working directory inside the container")
	user := fs.String("u", "", "user name or uid[:gid]")
	fs.StringVar(user, "user", "", "user name or uid[:gid]")
	if err := fs.Parse(ctrExpand(args, "itd", "e", "env", "env-file", "w", "workdir", "u", "user")); err != nil {
		return 125
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return 125
	}
	fileEnv, err := ctrReadEnvFiles(envFiles)
	if err != nil {
		return ctrFail(err)
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	info, err := rt.Get(fs.Arg(0))
	if err != nil {
		return ctrFail(err)
	}
	p := engine.ProcessSpec{Args: fs.Args()[1:], TTY: *tty}
	if info.Main != nil {
		p.Env, p.Dir, p.User = slices.Clone(info.Main.Env), info.Main.Dir, info.Main.User
	}
	p.Env = slices.Concat(p.Env, fileEnv, ctrEnv(env))
	if *dir != "" {
		p.Dir = *dir
	}
	if *user != "" {
		p.User = *user
	}
	if *detach {
		if info.State != engine.StateRunning {
			return ctrFail(fmt.Errorf("container %s is not running", info.Name))
		}
		status, err := ctrExecDetached(rt, info.ID, p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "kido:", err)
			return status
		}
		return 0
	}
	status, err := ctrInteract(ctx, rt, info.ID, p, *interactive, nil)
	if err != nil {
		return ctrFail(err)
	}
	return status
}

// ctrExecDetached runs p from a `kido __exec ID` helper in its own session, which holds the exec
// stream until the process exits and discards its output. It returns once the process has started
// (or failed to), so a missing executable is still reported here.
func ctrExecDetached(rt *runtime.Runtime, id string, p engine.ProcessSpec) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 125, err
	}
	spec, err := json.Marshal(p)
	if err != nil {
		return 125, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return 125, err
	}
	defer pr.Close()
	sr, sw, err := os.Pipe()
	if err != nil {
		pw.Close()
		return 125, err
	}
	cmd := exec.Command(self, ctrExecCmd, id)
	cmd.Env = append(os.Environ(), "KIDO_HOME="+rt.Home)
	cmd.Stdin = sr
	cmd.ExtraFiles = []*os.File{pw}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	sr.Close()
	pw.Close()
	if err != nil {
		sw.Close()
		return 125, err
	}
	sw.Write(spec)
	sw.Close()
	cmd.Process.Release()
	return ctrReadReport(pr, 30*time.Second)
}

// ctrReadReport reads a helper's start report: "ok", or "err STATUS MESSAGE".
func ctrReadReport(r *os.File, timeout time.Duration) (int, error) {
	r.SetReadDeadline(time.Now().Add(timeout))
	line, _ := bufio.NewReader(r).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "ok" {
		return 0, nil
	}
	if rest, ok := strings.CutPrefix(line, "err "); ok {
		code, msg, _ := strings.Cut(rest, " ")
		status, _ := strconv.Atoi(code)
		if status == 0 {
			status = 125
		}
		if msg == "" {
			msg = "the process failed to start"
		}
		return status, errors.New(msg)
	}
	return 125, errors.New("the process did not report back")
}

func ctrExecMain(ctx context.Context, args []string) int {
	report := os.NewFile(3, "report")
	reported := false
	say := func(format string, a ...any) {
		if report != nil && !reported {
			fmt.Fprintf(report, format+"\n", a...)
			report.Close()
			reported = true
		}
	}
	if len(args) != 1 {
		say("err 125 usage: kido %s CONTAINER", ctrExecCmd)
		return 125
	}
	var p engine.ProcessSpec
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&p); err != nil {
		say("err 125 bad process spec: %v", err)
		return 125
	}
	rt, err := ctrOpen()
	if err != nil {
		say("err 125 %v", err)
		return 125
	}
	stdio := engine.Stdio{Stdout: io.Discard, Stderr: io.Discard}
	status, err := rt.ExecNotify(context.Background(), args[0], p, stdio, func(int) { say("ok") })
	if err != nil {
		say("err %d %s", status, strings.ReplaceAll(err.Error(), "\n", " "))
	}
	return 0
}
