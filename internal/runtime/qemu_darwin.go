package runtime

import (
	"fmt"
	"os/exec"
	goruntime "runtime"
)

func resolveQEMUCPUs(requested int, backend string) (int, error) {
	maxCPUs := goruntime.NumCPU()
	if requested == 0 {
		requested = min(defaultCPUs, maxCPUs)
	}
	if requested < 1 || requested > maxCPUs {
		return 0, fmt.Errorf("--cpus %d: must be between 1 and %d", requested, maxCPUs)
	}
	return requested, nil
}

func prepareQEMUBoot(cpus int) (*qemuLaunch, error) {
	return &qemuLaunch{
		machineArgs:  []string{"-machine", "virt,gic-version=3", "-accel", "hvf", "-cpu", "host"},
		readyTimeout: readyTimeout,
		start:        func(cmd *exec.Cmd) error { return cmd.Start() },
	}, nil
}
