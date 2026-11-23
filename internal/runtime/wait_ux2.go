package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// removedExitTTL is how long the exit status of an auto-removed container stays readable.
const removedExitTTL = 10 * time.Minute

// exitsDir keeps the exit status of --rm containers after their removal, so a kido wait that polls
// after the removal still reports it.
func (r *Runtime) exitsDir() string { return filepath.Join(r.Home, "exits") }

// recordRemovedExit stores the exit status of container id before it is removed, and drops
// statuses older than removedExitTTL.
func (r *Runtime) recordRemovedExit(id string, status int) error {
	dir := r.exitsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if fi, err := e.Info(); err == nil && idRE.MatchString(e.Name()) && time.Since(fi.ModTime()) > removedExitTTL {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return writeFileAtomic(filepath.Join(dir, id), []byte(strconv.Itoa(status)+"\n"))
}

// removedExit returns the exit status recorded for container id when it was auto-removed.
func (r *Runtime) removedExit(id string) (int, bool) {
	if !idRE.MatchString(id) {
		return 0, false
	}
	path := filepath.Join(r.exitsDir(), id)
	fi, err := os.Stat(path)
	if err != nil || time.Since(fi.ModTime()) > removedExitTTL {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	status, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return status, err == nil
}
