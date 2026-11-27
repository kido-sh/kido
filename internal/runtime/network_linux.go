package runtime

import (
	"errors"
	"os"
	"strconv"
)

func vmnetSupported() error {
	return errors.New("user networks require macOS 26 or later (vmnet is not available on Linux)")
}

func gpuSupported() error {
	return errors.New("--gpu requires the vz backend on macOS 27 or later")
}

func processRunning(comm string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		if st, ok := parseProcStat(string(b)); ok && !procStatDead(st.state) && st.comm == comm {
			return true
		}
	}
	return false
}
