package runtime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

func processListensLoopback(ctx context.Context, pid, port int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if pid <= 0 {
		return false, nil
	}
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return false, err
	}
	inodes := parseProcNetTCPInodes(string(data), port, false)
	if data6, err6 := os.ReadFile("/proc/net/tcp6"); err6 == nil {
		inodes = append(inodes, parseProcNetTCPInodes(string(data6), port, true)...)
	}
	if len(inodes) == 0 {
		return false, nil
	}
	want := make(map[string]bool, len(inodes))
	for _, ino := range inodes {
		want["socket:["+ino+"]"] = true
	}
	fdDir := "/proc/" + strconv.Itoa(pid) + "/fd"
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err == nil && want[target] {
			return true, nil
		}
	}
	return false, nil
}
