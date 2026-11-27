package runtime

import (
	"os"
	"strconv"
)

func readProcStat(pid int) (procStat, bool) {
	if pid <= 0 {
		return procStat{}, false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStat{}, false
	}
	st, ok := parseProcStat(string(b))
	if !ok || st.pid != pid {
		return procStat{}, false
	}
	return st, true
}

func procStart(pid int) int64 {
	st, ok := readProcStat(pid)
	if !ok {
		return 0
	}
	return st.start
}

func processAlive(pid int, start int64) bool {
	st, ok := readProcStat(pid)
	if !ok {
		return false
	}
	return procStatAlive(st, start, true)
}

func hostProcStart(pid int) int64 {
	return procStart(pid)
}

func hostProcAlive(pid int, start int64) bool {
	st, ok := readProcStat(pid)
	if !ok {
		return false
	}
	return procStatAlive(st, start, false)
}
