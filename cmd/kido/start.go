package main

import (
	"context"
	"fmt"
	"os"

	"github.com/kido-sh/kido/internal/engine"
)

func init() {
	register("start", "Start one or more stopped containers", ctrStart)
}

func ctrStart(ctx context.Context, args []string) int {
	fs := ctrFlags("start", "[OPTIONS] CONTAINER [CONTAINER...]")
	attach := fs.Bool("a", false, "attach to the main process's output and return its exit status")
	fs.BoolVar(attach, "attach", false, "attach to the main process's output and return its exit status")
	interactive := fs.Bool("i", false, "attach the main process's stdin")
	fs.BoolVar(interactive, "interactive", false, "attach the main process's stdin")
	if err := fs.Parse(ctrExpand(ctrInterspersed(fs, args), "ai")); err != nil {
		return 125
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 125
	}
	if (*attach || *interactive) && fs.NArg() > 1 {
		return ctrFail(fmt.Errorf("you cannot start and attach multiple containers at once"))
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	if *attach || *interactive {
		info, err := rt.Get(fs.Arg(0))
		if err != nil {
			return ctrFail(err)
		}
		if info.State == engine.StateRunning {
			return ctrFail(fmt.Errorf("container %s is already running; use kido logs -f or kido exec", info.Name))
		}
		if info.Main == nil || len(info.Main.Args) == 0 {
			return ctrFail(fmt.Errorf("container %s has no command to attach to", info.Name))
		}
		defer ctrGuard()()
		if status, err := ctrStartContainer(ctx, rt, info.ID, false); err != nil {
			fmt.Fprintln(os.Stderr, "kido:", err)
			return status
		}
		return ctrForeground(ctx, rt, info.ID, info.Main, *interactive, info.AutoRemove)
	}
	code := 0
	for _, ref := range fs.Args() {
		info, err := rt.Get(ref)
		if err != nil {
			code = ctrFail(err)
			continue
		}
		if info.State != engine.StateRunning {
			if status, err := ctrStartContainer(ctx, rt, info.ID, true); err != nil {
				fmt.Fprintln(os.Stderr, "kido:", err)
				code = status
				continue
			}
		}
		fmt.Println(ref)
	}
	return code
}
