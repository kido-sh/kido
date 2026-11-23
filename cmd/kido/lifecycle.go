package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("kill", "Kill one or more running containers", ctrKill)
	register("restart", "Restart one or more containers", ctrRestart)
	register("wait", "Block until one or more containers stop, then print their exit codes", ctrWait)
	register("rename", "Rename a container", ctrRename)
	register("port", "List port mappings or a specific mapping for the container", ctrPortCmd)
	register("create", "Create a new container without starting it", ctrCreate)
}

// ctrGuard keeps SIGINT, SIGTERM and SIGHUP from killing kido until the returned func is called, so
// an interrupt during boot or cleanup cannot leave a VM behind. Handlers registered meanwhile (boot
// cancellation, forwarding to the process) still see the signals.
func ctrGuard() func() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	return func() { signal.Stop(c) }
}

// ctrForeground runs the container's main process attached to this terminal and winds the
// container down after it (see ctrFinish).
func ctrForeground(ctx context.Context, rt *runtime.Runtime, id string, m *runtime.Process, interactive, autoRemove bool) int {
	vmPID := rt.VMPID(id)
	p := engine.ProcessSpec{Args: m.Args, Env: m.Env, Dir: m.Dir, User: m.User, TTY: m.TTY}
	status, execErr := ctrInteract(ctx, rt, id, p, interactive, func(pid int) { rt.SetOwner(id, pid) })
	if execErr != nil && !rt.StoppedByOthers(id, vmPID, 2*time.Second) {
		status = ctrFail(execErr)
	}
	return ctrFinish(rt, id, vmPID, status, execErr, autoRemove)
}

// ctrFinish records how the main process of container id (VM vmPID) ended and winds the container
// down: removed with autoRemove, otherwise stopped, unless kido stop, kill or rm already did it, in
// which case a broken exec stream means the process was killed (137).
func ctrFinish(rt *runtime.Runtime, id string, vmPID, status int, execErr error, autoRemove bool) int {
	settle := time.Duration(0)
	if execErr != nil {
		settle = 2 * time.Second
	}
	others := rt.StoppedByOthers(id, vmPID, settle)
	if execErr != nil && others {
		status, execErr = 137, nil
	}
	rt.RecordExit(id, vmPID, status, execErr)
	if autoRemove {
		if err := rt.Remove(context.Background(), id, true); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			fmt.Fprintln(os.Stderr, "kido: remove:", err)
		}
	} else if !others {
		if err := rt.Stop(context.Background(), id, 10*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "kido: stop:", err)
		}
	}
	return status
}

// ctrStartContainer boots a container once the owner of its previous boot has let go, and starts
// its main process in the background when it has one.
func ctrStartContainer(ctx context.Context, rt *runtime.Runtime, id string, spawnMain bool) (int, error) {
	if err := rt.WaitOwner(ctx, id, 15*time.Second); err != nil {
		return 125, err
	}
	done := ctrGuard()
	defer done()
	bootCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGHUP)
	err := rt.Start(bootCtx, id)
	stop()
	if err != nil {
		return 125, err
	}
	info, err := rt.Get(id)
	if err != nil {
		return 125, err
	}
	if spawnMain && info.Main != nil && len(info.Main.Args) > 0 {
		return ctrSpawnMain(rt, id)
	}
	return 0, nil
}

func ctrSignal(s string) (syscall.Signal, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if n, err := strconv.Atoi(s); err == nil {
		if n < 1 || n > 31 {
			return 0, fmt.Errorf("invalid signal %s", s)
		}
		return syscall.Signal(n), nil
	}
	if !strings.HasPrefix(s, "SIG") {
		s = "SIG" + s
	}
	if sig := unix.SignalNum(s); sig != 0 {
		return sig, nil
	}
	return 0, fmt.Errorf("invalid signal %s", strings.TrimPrefix(s, "SIG"))
}

func ctrKill(ctx context.Context, args []string) int {
	fs := ctrFlags("kill", "[OPTIONS] CONTAINER [CONTAINER...]")
	sigName := fs.String("s", "KILL", "signal to send to the main process; KILL stops the VM at once")
	fs.StringVar(sigName, "signal", "KILL", "signal to send to the main process; KILL stops the VM at once")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 125
	}
	sig, err := ctrSignal(*sigName)
	if err != nil {
		return ctrFail(err)
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	code := 0
	for _, ref := range fs.Args() {
		info, err := rt.Get(ref)
		if err == nil && info.State != engine.StateRunning {
			err = fmt.Errorf("container %s is not running", info.Name)
		}
		if err != nil {
			code = ctrFail(err)
			continue
		}
		if sig == syscall.SIGKILL {
			err = rt.Kill(ctx, info.ID)
			if err == nil && !rt.OwnerAlive(info.ID) {
				rt.RecordExit(info.ID, 0, 137, nil)
			}
		} else {
			err = rt.Signal(ctx, info.ID, sig)
		}
		if err != nil {
			code = ctrFail(err)
			continue
		}
		fmt.Println(ref)
	}
	return code
}

func ctrRestart(ctx context.Context, args []string) int {
	fs := ctrFlags("restart", "[OPTIONS] CONTAINER [CONTAINER...]")
	secs := fs.Int("t", 10, "seconds to wait for a clean shutdown before killing the VM")
	fs.IntVar(secs, "time", 10, "seconds to wait for a clean shutdown before killing the VM")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 125
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	code := 0
	for _, ref := range fs.Args() {
		info, err := rt.Get(ref)
		if err != nil {
			code = ctrFail(err)
			continue
		}
		if info.State == engine.StateRunning {
			if *secs <= 0 {
				err = rt.Kill(ctx, info.ID)
			} else {
				err = rt.Stop(ctx, info.ID, time.Duration(*secs)*time.Second)
			}
			if err != nil {
				code = ctrFail(err)
				continue
			}
		}
		if status, err := ctrStartContainer(ctx, rt, info.ID, true); err != nil {
			fmt.Fprintln(os.Stderr, "kido:", err)
			code = status
			continue
		}
		fmt.Println(ref)
	}
	return code
}

func ctrWait(ctx context.Context, args []string) int {
	fs := ctrFlags("wait", "CONTAINER [CONTAINER...]")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 125
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	code := 0
	for _, ref := range fs.Args() {
		status, err := rt.Wait(ctx, ref)
		if err != nil {
			code = ctrFail(err)
			continue
		}
		fmt.Println(status)
	}
	return code
}

func ctrRename(ctx context.Context, args []string) int {
	fs := ctrFlags("rename", "CONTAINER NEW_NAME")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 125
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	if err := rt.Rename(fs.Arg(0), fs.Arg(1)); err != nil {
		return ctrFail(err)
	}
	return 0
}

func ctrPortCmd(ctx context.Context, args []string) int {
	fs := ctrFlags("port", "CONTAINER [PRIVATE_PORT[/PROTO]]")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 125
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	info, err := rt.Get(fs.Arg(0))
	if err != nil {
		return ctrFail(err)
	}
	if fs.NArg() == 1 {
		for _, p := range info.Ports {
			fmt.Printf("%d/%s -> %s:%d\n", p.GuestPort, p.Protocol, p.HostIP, p.HostPort)
		}
		return 0
	}
	want, proto, _ := strings.Cut(fs.Arg(1), "/")
	if proto == "" {
		proto = "tcp"
	}
	port, err := strconv.Atoi(want)
	if err != nil {
		return ctrFail(fmt.Errorf("invalid port %q", fs.Arg(1)))
	}
	found := false
	for _, p := range info.Ports {
		if p.GuestPort == port && p.Protocol == proto {
			fmt.Printf("%s:%d\n", p.HostIP, p.HostPort)
			found = true
		}
	}
	if !found {
		return ctrFail(fmt.Errorf("no public port '%d/%s' published for %s", port, proto, info.Name))
	}
	return 0
}

// ctrConfirm asks before a destructive prune, like Docker; anything but y or yes declines.
func ctrConfirm(warning string) bool {
	fmt.Printf("%s\nAre you sure you want to continue? [y/N] ", warning)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	if line == "" {
		fmt.Println()
	}
	return false
}

func ctrPruneFlags(name string, args []string, withAll bool) (all, force bool, ok bool) {
	fs := ctrFlags(name, "[OPTIONS]")
	fs.BoolVar(&force, "f", false, "do not prompt for confirmation")
	fs.BoolVar(&force, "force", false, "do not prompt for confirmation")
	if withAll {
		fs.BoolVar(&all, "a", false, "also remove every image no container uses")
		fs.BoolVar(&all, "all", false, "also remove every image no container uses")
	}
	if err := fs.Parse(ctrExpand(ctrInterspersed(fs, args), "af")); err != nil {
		return false, false, false
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return false, false, false
	}
	return all, force, true
}

func ctrContainerPrune(ctx context.Context, args []string) int {
	_, force, ok := ctrPruneFlags("container prune", args, false)
	if !ok {
		return 125
	}
	if !force && !ctrConfirm("WARNING! This will remove all stopped containers.") {
		return 0
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	p, err := rt.PruneContainers(ctx)
	ctrPrintPruned("Deleted Containers:", p)
	fmt.Println("Total reclaimed space:", ctrSize(p.Bytes))
	if err != nil {
		return ctrFail(err)
	}
	return 0
}

func ctrPrintPruned(title string, p runtime.Pruned) {
	if len(p.Names) == 0 {
		return
	}
	fmt.Println(title)
	for _, n := range p.Names {
		fmt.Println(n)
	}
	fmt.Println()
}

func ctrSystemPrune(ctx context.Context, args []string) int {
	all, force, ok := ctrPruneFlags("system prune", args, true)
	if !ok {
		return 125
	}
	warning := "WARNING! This will remove:\n  - all stopped containers\n"
	if all {
		warning += "  - all images without at least one container associated to them\n"
	}
	warning += "  - leftovers of interrupted image operations\n"
	if !force && !ctrConfirm(warning) {
		return 0
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	var errs []error
	pc, err := rt.PruneContainers(ctx)
	errs = append(errs, err)
	ctrPrintPruned("Deleted Containers:", pc)
	total := pc.Bytes
	if all {
		pi, err := rt.PruneImages(ctx)
		errs = append(errs, err)
		ctrPrintPruned("Deleted Images:", pi)
		total += pi.Bytes
	}
	pt, err := rt.PruneTemp()
	errs = append(errs, err)
	total += pt.Bytes
	fmt.Println("Total reclaimed space:", ctrSize(total))
	if err := errors.Join(errs...); err != nil {
		return ctrFail(err)
	}
	return 0
}

func ctrCreate(ctx context.Context, args []string) int {
	return ctrRunOrCreate(ctx, "create", args)
}

// ctrOptString is a string flag that knows whether it was given, so --entrypoint "" can clear.
type ctrOptString struct {
	value string
	set   bool
}

func (o *ctrOptString) String() string     { return o.value }
func (o *ctrOptString) Set(v string) error { o.value, o.set = v, true; return nil }

// ctrMainArgs is the main process of a new container, as Docker computes it: the image's
// ENTRYPOINT and CMD, the command line replacing CMD, and --entrypoint replacing the ENTRYPOINT
// and dropping the image's CMD.
func ctrMainArgs(cfg engine.ImageConfig, ep *ctrOptString, args []string) []string {
	entry, cmd := cfg.Entrypoint, cfg.Cmd
	if ep.set {
		entry, cmd = nil, nil
		if ep.value != "" {
			entry = []string{ep.value}
		}
	}
	if len(args) > 0 {
		cmd = args
	}
	return slices.Concat(entry, cmd)
}
