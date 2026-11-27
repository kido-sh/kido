package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

func vmnetSupported() error {
	return errors.New("user networks require macOS 26 or later (vmnet is not available on Linux)")
}

func gpuSupported(backend string) error {
	if backend != backendKVM {
		return errors.New("--gpu on Linux requires the kvm backend")
	}
	if VKWorkerPath() == "" {
		return errors.New("--gpu on Linux requires kido-vk-worker (install it next to kido-vmm, in PATH, or set KIDO_VK_WORKER)")
	}
	matches, _ := filepath.Glob("/dev/dri/renderD*")
	if len(matches) == 0 {
		return errors.New("--gpu on Linux requires a Vulkan render node under /dev/dri/renderD*")
	}
	for _, p := range matches {
		if unix.Access(p, unix.R_OK|unix.W_OK) == nil {
			return nil
		}
	}
	return errors.New("--gpu on Linux requires read/write access to /dev/dri/renderD* (add your user to the render group)")
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
