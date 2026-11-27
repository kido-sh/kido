package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kido-sh/kido/internal/notify"
	"github.com/kido-sh/kido/internal/runtime"

	"golang.org/x/sys/unix"
)

func init() {
	register(runtime.NotifyCommand, "", ctrNotify)
}

// ctrNotify is the relay vzBoot starts once the agent is pinned and the mounts are up: stdin is
// kido-vmm's record pipe, the arguments are the container and its live shares as NAME=ROOT, and
// descriptors 3 onward are those roots, in order.
func ctrNotify(ctx context.Context, args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: kido %s ID NAME=ROOT...\n", runtime.NotifyCommand)
		return 2
	}
	var shares []runtime.NotifyShare
	for i, a := range args[1:] {
		name, root, ok := strings.Cut(a, "=")
		if !ok || !notify.ValidShare(name) {
			fmt.Fprintf(os.Stderr, "kido notify: bad share %q\n", a)
			return 2
		}
		var st unix.Stat_t
		if unix.Fstat(3+i, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			fmt.Fprintf(os.Stderr, "kido notify: share %q has no directory descriptor\n", a)
			return 2
		}
		shares = append(shares, runtime.NotifyShare{Name: name, Root: root, FD: 3 + i})
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	if err := rt.RunNotifyRelay(ctx, args[0], os.Stdin, shares, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "kido notify: %s\n", runtime.Sanitize(err.Error()))
		return 1
	}
	return 0
}
