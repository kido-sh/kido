package runtime

import (
	"strconv"
	"strings"
)

const qemuComm = "qemu-system-aarch64"

type procStat struct {
	pid   int
	comm  string
	state byte
	start int64
}

func parseProcStat(line string) (procStat, bool) {
	open := strings.IndexByte(line, '(')
	close := strings.LastIndexByte(line, ')')
	if open <= 0 || close <= open {
		return procStat{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line[:open]))
	if err != nil || pid <= 0 {
		return procStat{}, false
	}
	fields := strings.Fields(line[close+1:])
	if len(fields) < 20 || len(fields[0]) != 1 {
		return procStat{}, false
	}
	start, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil || start < 0 {
		return procStat{}, false
	}
	return procStat{
		pid:   pid,
		comm:  line[open+1 : close],
		state: fields[0][0],
		start: start,
	}, true
}

func procStatDead(state byte) bool {
	return state == 'Z' || state == 'X' || state == 'x'
}

func isVMComm(comm string) bool {
	if comm == vmmComm {
		return true
	}
	return len(comm) >= 15 && strings.HasPrefix(qemuComm, comm)
}

func procStatAlive(st procStat, start int64, vmOnly bool) bool {
	if procStatDead(st.state) {
		return false
	}
	if vmOnly && !isVMComm(st.comm) {
		return false
	}
	if start != 0 && st.start != start {
		return false
	}
	return true
}
