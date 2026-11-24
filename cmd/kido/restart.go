package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/runtime"
)

// ctrRestartPolicy checks --restart and returns it in the form kept in the container, "" for no.
func ctrRestartPolicy(s string) (string, error) {
	name, n, hasN := strings.Cut(s, ":")
	switch {
	case !hasN && (name == "no" || name == ""):
		return "", nil
	case !hasN && (name == "always" || name == "unless-stopped" || name == "on-failure"):
		return name, nil
	case hasN && name == "on-failure":
		if v, err := strconv.Atoi(n); err == nil && v >= 0 {
			return name + ":" + strconv.Itoa(v), nil
		}
	}
	return "", fmt.Errorf("invalid --restart %q: use no, always, unless-stopped or on-failure[:max-retries]", s)
}

// ctrShouldRestart reports whether policy restarts a main process that ended with status, after
// restarts earlier restarts. on-failure without a maximum retries for ever.
func ctrShouldRestart(policy string, status, restarts int) bool {
	name, n, _ := strings.Cut(policy, ":")
	switch name {
	case "always", "unless-stopped":
		return true
	case "on-failure":
		limit, _ := strconv.Atoi(n)
		return status != 0 && (limit == 0 || restarts < limit)
	}
	return false
}

// ctrRestartDelay doubles the wait between restarts up to a minute, and starts over at a second
// after a run of ten seconds or more.
func ctrRestartDelay(prev, ran time.Duration) time.Duration {
	if prev == 0 || ran >= 10*time.Second {
		return time.Second
	}
	return min(2*prev, time.Minute)
}

// ctrAwaitRestart waits delay before a restart that the policy of container id asks for. It
// reports false when there is to be none: the policy says no, or meanwhile someone stopped,
// removed or started the container. A kido start marks the container stopped first, so this
// gives up within 200 ms and the start does not wait long for the owner to exit.
func ctrAwaitRestart(rt *runtime.Runtime, id string, status int, delay time.Duration, logw io.Writer) bool {
	wanted := func() bool {
		info, err := rt.Get(id)
		return err == nil && !info.UserStop && info.State != engine.StateRunning &&
			ctrShouldRestart(info.Restart, status, info.Restarts)
	}
	if !wanted() {
		return false
	}
	fmt.Fprintf(logw, "kido: the main process exited with status %d; restarting in %s\n", status, delay)
	for end := time.Now().Add(delay); time.Now().Before(end); {
		time.Sleep(200 * time.Millisecond)
		if !wanted() {
			return false
		}
	}
	return true
}

// ctrMarkStopped records that the user stopped container ref, so its restart policy rests until
// the next kido start.
func ctrMarkStopped(rt *runtime.Runtime, ref string) {
	if info, err := rt.Get(ref); err == nil {
		rt.Update(info.ID, func(i *runtime.Info) { i.UserStop = true })
	}
}
