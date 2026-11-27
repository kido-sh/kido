package runtime

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func vmnetSupported() error {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return err
	}
	major, _, _ := strings.Cut(v, ".")
	if n, err := strconv.Atoi(major); err != nil || n < 26 {
		return fmt.Errorf("networks need macOS 26 or later (this is %s)", v)
	}
	return nil
}

func processRunning(comm string) bool {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return true
	}
	for _, kp := range procs {
		if name, _, _ := strings.Cut(string(kp.Proc.P_comm[:]), "\x00"); name == comm {
			return true
		}
	}
	return false
}
