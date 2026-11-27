package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func processListensLoopback(ctx context.Context, pid, port int) (bool, error) {
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-a", "-p", strconv.Itoa(pid),
		"-iTCP@127.0.0.1:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return false, fmt.Errorf("lsof: %w", err)
	}
	return strings.TrimSpace(string(out)) == strconv.Itoa(pid), nil
}
