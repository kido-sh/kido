package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/runtime"
)

const ctrAttachCmd = "__attach"

func init() {
	register("run", "Create a container from an image and run a command in it", ctrRun)
	if len(os.Args) > 1 && os.Args[1] == ctrAttachCmd {
		register(ctrAttachCmd, "", ctrAttachMain)
	}
}

// ctrPort parses -p [IP:]HOST:GUEST[/tcp|udp] or GUEST[/proto] (random host port).
func ctrPort(s string) (engine.PortMapping, error) {
	p := engine.PortMapping{Protocol: "tcp"}
	spec := s
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		p.Protocol = strings.ToLower(spec[i+1:])
		spec = spec[:i]
	}
	parts := strings.Split(spec, ":")
	var host, guest string
	switch len(parts) {
	case 1:
		guest = parts[0]
	case 2:
		host, guest = parts[0], parts[1]
	case 3:
		p.HostIP, host, guest = parts[0], parts[1], parts[2]
		if net.ParseIP(p.HostIP).To4() == nil {
			return p, fmt.Errorf("-p %s: %q is not an IPv4 address", s, p.HostIP)
		}
	default:
		return p, fmt.Errorf("-p %s: want [IP:]HOST:GUEST[/tcp|udp]", s)
	}
	var err error
	if host != "" {
		if p.HostPort, err = strconv.Atoi(host); err != nil || p.HostPort < 1 || p.HostPort > 65535 {
			return p, fmt.Errorf("-p %s: invalid host port %q", s, host)
		}
	}
	if p.GuestPort, err = strconv.Atoi(guest); err != nil || p.GuestPort < 1 || p.GuestPort > 65535 {
		return p, fmt.Errorf("-p %s: invalid container port %q", s, guest)
	}
	if p.Protocol != "tcp" && p.Protocol != "udp" {
		return p, fmt.Errorf("-p %s: protocol must be tcp or udp", s)
	}
	return p, nil
}

// ctrMemory parses --memory as MB, with an optional m or g suffix.
func ctrMemory(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	mult := 1
	switch strings.ToLower(s[len(s)-1:]) {
	case "g":
		mult, s = 1024, s[:len(s)-1]
	case "m":
		s = s[:len(s)-1]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid --memory %q (MB, or a number with an m or g suffix)", s)
	}
	return n * mult, nil
}

func ctrRun(ctx context.Context, args []string) int { return ctrRunOrCreate(ctx, "run", args) }

// ctrRunOrCreate is kido run, or with cmdName "create" kido create, which makes the container without
// booting it.
func ctrRunOrCreate(ctx context.Context, cmdName string, args []string) int {
	create := cmdName == "create"
	fs := ctrFlags(cmdName, "[OPTIONS] IMAGE [COMMAND] [ARG...]")
	interactive := fs.Bool("i", false, "keep stdin open")
	fs.BoolVar(interactive, "interactive", false, "keep stdin open")
	tty := fs.Bool("t", false, "allocate a pseudo-terminal")
	fs.BoolVar(tty, "tty", false, "allocate a pseudo-terminal")
	detach := fs.Bool("d", false, "run in the background and print the container ID")
	fs.BoolVar(detach, "detach", false, "run in the background and print the container ID")
	var entrypoint ctrOptString
	fs.Var(&entrypoint, "entrypoint", "overwrite the image's ENTRYPOINT (and drop its CMD); \"\" clears it")
	autoRemove := fs.Bool("rm", false, "remove the container when it exits")
	name := fs.String("name", "", "container name")
	var env, envFiles, ports ctrList
	fs.Var(&env, "e", "set an environment variable (KEY=VALUE)")
	fs.Var(&env, "env", "set an environment variable (KEY=VALUE)")
	fs.Var(&envFiles, "env-file", "read in a file of environment variables")
	fs.Var(&ports, "p", "publish a port: [IP:]HOST:GUEST[/tcp|udp] (host IP defaults to 127.0.0.1)")
	fs.Var(&ports, "publish", "publish a port: [IP:]HOST:GUEST[/tcp|udp]")
	dir := fs.String("w", "", "working directory inside the container")
	fs.StringVar(dir, "workdir", "", "working directory inside the container")
	user := fs.String("u", "", "user name or uid[:gid]")
	fs.StringVar(user, "user", "", "user name or uid[:gid]")
	cpus := fs.Int("cpus", 0, "virtual CPUs (default 2)")
	network := fs.String("network", "", "a network from 'kido network create' (vz backend), or none: no outside access (published ports still work)")
	memory := fs.String("memory", "", "memory in MB, or with an m/g suffix (default 1024)")
	fs.StringVar(memory, "m", "", "memory (same as --memory)")
	pull := fs.String("pull", "missing", `pull the image before running ("missing" or "never")`)
	mounts := ctrMountFlags(fs)
	valued := []string{"name", "e", "env", "p", "publish", "w", "workdir", "u", "user", "cpus", "memory", "m", "entrypoint", "v", "volume", "mount", "env-file", "pull"}
	if err := fs.Parse(ctrExpand(args, "itd", valued...)); err != nil {
		return 125
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return 125
	}
	if err := checkPullPolicy(*pull); err != nil {
		return ctrFail(err)
	}
	fileEnv, err := ctrReadEnvFiles(envFiles)
	if err != nil {
		return ctrFail(err)
	}
	mem, err := ctrMemory(*memory)
	if err != nil {
		return ctrFail(err)
	}
	opts := engine.CreateOptions{Name: *name, Image: fs.Arg(0), CPUs: *cpus, MemoryMB: mem, Network: *network}
	for _, s := range ports {
		p, err := ctrPort(s)
		if err != nil {
			return ctrFail(err)
		}
		opts.Ports = append(opts.Ports, p)
	}
	if opts.Mounts, err = mounts.parse(); err != nil {
		return ctrFail(err)
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	img, err := imageOrPull(ctx, rt, opts.Image, *pull, os.Stderr)
	if err != nil {
		return ctrFail(err)
	}
	mainProc := &runtime.Process{
		Args: ctrMainArgs(img.Config, &entrypoint, fs.Args()[1:]),
		Env:  slices.Concat(img.Config.Env, fileEnv, ctrEnv(env)),
		Dir:  img.Config.WorkingDir,
		User: img.Config.User,
		TTY:  *tty,
	}
	if *dir != "" {
		mainProc.Dir = *dir
	}
	if *user != "" {
		mainProc.User = *user
	}
	if len(mainProc.Args) == 0 && !*detach && !create {
		return ctrFail(errors.New("no command specified (the image has no default command)"))
	}

	defer ctrGuard()()
	bootCtx, stopBoot := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGHUP)
	var c *engine.Container
	if create {
		c, err = rt.CreateStopped(bootCtx, opts)
	} else {
		c, err = rt.Create(bootCtx, opts)
	}
	stopBoot()
	if c != nil {
		rt.Update(c.ID, func(i *runtime.Info) {
			i.Main, i.Detached, i.AutoRemove = mainProc, *detach, *autoRemove
		})
	}
	if err != nil {
		if c != nil && *autoRemove {
			rt.Remove(context.Background(), c.ID, true)
		}
		return ctrFail(err)
	}
	if create {
		fmt.Println(c.ID)
		return 0
	}

	if *detach {
		if len(mainProc.Args) > 0 {
			if status, err := ctrSpawnMain(rt, c.ID); err != nil {
				fmt.Fprintln(os.Stderr, "kido:", err)
				return status
			}
		}
		fmt.Println(c.ID)
		return 0
	}

	// --rm discards the disk, so the VM is killed instead of shut down cleanly.
	return ctrForeground(ctx, rt, c.ID, mainProc, *interactive, *autoRemove)
}

// ctrSpawnMain starts `kido __attach ID` in its own session: it runs the container's main process,
// writes its output to main.log, and stops the VM when the process exits. It reports on fd 3 once the
// process has started, so a missing executable fails here, even with -d.
func ctrSpawnMain(rt *runtime.Runtime, id string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 125, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return 125, err
	}
	defer pr.Close()
	cmd := exec.Command(self, ctrAttachCmd, id)
	cmd.Env = append(os.Environ(), "KIDO_HOME="+rt.Home)
	cmd.ExtraFiles = []*os.File{pw}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	pw.Close()
	if err != nil {
		return 125, err
	}
	cmd.Process.Release()
	pr.SetReadDeadline(time.Now().Add(60 * time.Second))
	line, _ := bufio.NewReader(pr).ReadString('\n')
	line = strings.TrimSpace(line)
	switch {
	case line == "ok":
		return 0, nil
	case strings.HasPrefix(line, "err "):
		f := strings.SplitN(line, " ", 3)
		status, _ := strconv.Atoi(f[1])
		if status == 0 {
			status = 125
		}
		msg := "main process failed to start"
		if len(f) == 3 {
			msg = f[2]
		}
		return status, errors.New(msg)
	}
	return 125, fmt.Errorf("main process did not report back; see kido logs %s", id)
}

func ctrAttachMain(ctx context.Context, args []string) int {
	if len(args) != 1 {
		return 2
	}
	report := os.NewFile(3, "report")
	reported := false
	say := func(format string, a ...any) {
		if report != nil && !reported {
			fmt.Fprintf(report, format+"\n", a...)
			report.Close()
			reported = true
		}
	}
	rt, err := ctrOpen()
	if err != nil {
		say("err 125 %s", err)
		return 125
	}
	info, err := rt.Get(args[0])
	if err != nil || info.Main == nil || len(info.Main.Args) == 0 {
		say("err 125 container %s has no main process", args[0])
		return 125
	}
	logw, err := runtime.OpenCappedLog(filepath.Join(rt.ContainerDir(info.ID), "main.log"))
	if err != nil {
		say("err 125 %s", err)
		return 125
	}
	defer logw.Close()
	m := info.Main
	p := engine.ProcessSpec{Args: m.Args, Env: m.Env, Dir: m.Dir, User: m.User, TTY: m.TTY}
	stdio := engine.Stdio{Stdout: logw, Stderr: logw}
	vmPID := rt.VMPID(info.ID)
	status, execErr := rt.ExecNotify(context.Background(), info.ID, p, stdio, func(pid int) {
		rt.SetOwner(info.ID, pid)
		say("ok")
	})
	if execErr != nil && !(reported && rt.StoppedByOthers(info.ID, vmPID, 2*time.Second)) {
		fmt.Fprintln(logw, "kido:", execErr)
		say("err %d %s", status, strings.ReplaceAll(execErr.Error(), "\n", " "))
	}
	ctrFinish(rt, info.ID, vmPID, status, execErr, info.AutoRemove)
	return 0
}
