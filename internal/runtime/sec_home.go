package runtime

import (
	"fmt"
	"os"
	"syscall"
)

// checkPrivateDir refuses a state directory that another user owns or that group or others can
// write, since whoever can write it controls the images and VMs kido runs. follow allows the
// directory itself to be a symlink (KIDO_HOME may be one).
func checkPrivateDir(dir string, follow bool) error {
	stat := os.Lstat
	if follow {
		stat = os.Stat
	}
	fi, err := stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not by you (uid %d); refusing to use it", dir, st.Uid, os.Geteuid())
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (mode %v); fix it with: chmod go-w %s", dir, fi.Mode().Perm(), dir)
	}
	return nil
}
