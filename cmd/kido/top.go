package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("top", "Display the running processes of a container", ctrTop)
}

// ctrCappedBuffer keeps the first max bytes written to it and drops the rest.
type ctrCappedBuffer struct {
	bytes.Buffer
	max int
}

func (b *ctrCappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		b.Buffer.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// ctrTop runs ps in the guest: every process in the VM belongs to the container.
func ctrTop(ctx context.Context, args []string) int {
	fs := ctrFlags("top", "CONTAINER [ps OPTIONS]")
	if err := fs.Parse(args); err != nil {
		return 125
	}
	if fs.NArg() < 1 {
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
	if info.State != engine.StateRunning {
		return ctrFail(fmt.Errorf("container %s is not running", info.Name))
	}
	psArgs := fs.Args()[1:]
	if len(psArgs) == 0 {
		psArgs = []string{"-ef"}
	}
	stdout := &ctrCappedBuffer{max: 8 << 20}
	stderr := &ctrCappedBuffer{max: 64 << 10}
	p := engine.ProcessSpec{Args: append([]string{"/bin/ps"}, psArgs...)}
	status, err := rt.Exec(ctx, info.ID, p, engine.Stdio{Stdout: stdout, Stderr: stderr})
	if err != nil {
		return ctrFail(err)
	}
	// ps exits 1 when no process matches, after printing the header.
	msg := strings.TrimSpace(stderr.String())
	if status != 0 && (msg != "" || stdout.Len() == 0) {
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", status)
		}
		return ctrFail(fmt.Errorf("ps failed: %s", runtime.Sanitize(msg)))
	}
	fmt.Fprint(os.Stdout, runtime.Sanitize(stdout.String()))
	return 0
}
