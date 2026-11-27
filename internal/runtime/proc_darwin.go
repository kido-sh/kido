package runtime

import (
	"strings"

	"golang.org/x/sys/unix"
)

const procStateZombie = 5

func kinfo(pid int) (*unix.KinfoProc, bool) {
	if pid <= 0 {
		return nil, false
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return nil, false
	}
	return kp, true
}

func procStart(pid int) int64 {
	kp, ok := kinfo(pid)
	if !ok {
		return 0
	}
	return kp.Proc.P_starttime.Sec*1_000_000 + int64(kp.Proc.P_starttime.Usec)
}

func processAlive(pid int, start int64) bool {
	kp, ok := kinfo(pid)
	if !ok || kp.Proc.P_stat == procStateZombie {
		return false
	}
	comm, _, _ := strings.Cut(string(kp.Proc.P_comm[:]), "\x00")
	if comm == "" || !strings.HasPrefix("qemu-system-aarch64", comm) && comm != vmmComm {
		return false
	}
	if start != 0 && kp.Proc.P_starttime.Sec*1_000_000+int64(kp.Proc.P_starttime.Usec) != start {
		return false
	}
	return true
}

func hostProcStart(pid int) int64 {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return 0
	}
	return kp.Proc.P_starttime.Sec*1_000_000 + int64(kp.Proc.P_starttime.Usec)
}

func hostProcAlive(pid int, start int64) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid || kp.Proc.P_stat == procStateZombie {
		return false
	}
	return start == 0 || kp.Proc.P_starttime.Sec*1_000_000+int64(kp.Proc.P_starttime.Usec) == start
}
