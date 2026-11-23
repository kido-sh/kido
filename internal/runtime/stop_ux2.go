package runtime

import (
	"context"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/kido-sh/kido/internal/engine"
)

// stopMain ends the main process of a running container the way docker stop does: SIGTERM, up to
// timeout for it to exit, then SIGKILL. Stop calls it with the container locked and Stopping saved,
// so the process's owner records its exit status and leaves the VM to Stop. Nothing is sent when
// the caller is the owner itself: its main process has already exited.
func (r *Runtime) stopMain(ctx context.Context, info *Info, timeout time.Duration) {
	o := r.owner(info.ID)
	if o == nil || o.PID == os.Getpid() || o.GuestPID <= 1 || o.VMPID != info.PID || !hostProcAlive(o.PID, o.Start) {
		return
	}
	if r.Signal(ctx, info.ID, syscall.SIGTERM) != nil {
		return
	}
	if r.waitGuestExit(ctx, info.ID, o.GuestPID, timeout) {
		return
	}
	if r.Signal(ctx, info.ID, syscall.SIGKILL) == nil {
		r.waitGuestExit(ctx, info.ID, o.GuestPID, 2*time.Second)
	}
}

// waitGuestExit waits up to timeout for process pid in the guest to be gone and reports whether it is.
func (r *Runtime) waitGuestExit(ctx context.Context, id string, pid int, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p := engine.ProcessSpec{Args: []string{"/bin/sh", "-c", `while kill -0 "$1" 2>/dev/null; do sleep 0.1; done`, "sh", strconv.Itoa(pid)}}
	status, err := r.Exec(ctx, id, p, engine.Stdio{})
	return err == nil && status == 0
}
