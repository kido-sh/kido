package runtime

import (
	"errors"
	"fmt"
	"os/exec"
	goruntime "runtime"
)

func resolveQEMUCPUs(requested int, backend string) (int, error) {
	mode, err := KVMModeFromEnv()
	if err != nil {
		return 0, err
	}
	if backend == backendKVM {
		if mode == KVMModeTCG {
			return 0, errors.New("KIDO_ACCEL=tcg is not supported by the kvm backend; unset KIDO_ACCEL or use KIDO_BACKEND=qemu")
		}
		plan, err := SelectLinuxKVMAffinity()
		if err != nil {
			return 0, err
		}
		if plan.MaxCPUs > kvmMaxCPUs {
			plan.MaxCPUs = kvmMaxCPUs
		}
		return resolvePlanCPUs(requested, plan)
	}
	if mode == KVMModeTCG {
		maxCPUs := goruntime.NumCPU()
		if allowed, err := linuxAllowedCPUs(); err == nil && len(allowed) > 0 {
			maxCPUs = len(allowed)
		}
		if requested == 0 {
			return min(defaultCPUs, maxCPUs), nil
		}
		if requested < 1 || requested > maxCPUs {
			return 0, fmt.Errorf("--cpus %d: must be between 1 and %d", requested, maxCPUs)
		}
		return requested, nil
	}
	plan, err := SelectLinuxKVMAffinity()
	if err != nil {
		return 0, err
	}
	return resolvePlanCPUs(requested, plan)
}

func prepareQEMUBoot(cpus int) (*qemuLaunch, error) {
	mode, err := KVMModeFromEnv()
	if err != nil {
		return nil, err
	}
	machineArgs, timeout := linuxQEMUAccelConfig(mode)
	if mode == KVMModeTCG {
		return &qemuLaunch{
			machineArgs:  machineArgs,
			readyTimeout: timeout,
			start:        func(cmd *exec.Cmd) error { return cmd.Start() },
		}, nil
	}
	if _, err := probeDevKVM(); err != nil {
		return nil, err
	}
	plan, err := SelectLinuxKVMAffinity()
	if err != nil {
		return nil, err
	}
	if _, err := resolvePlanCPUs(cpus, plan); err != nil {
		return nil, err
	}
	mask, err := cpuSetFromIDs(plan.PinCPUs)
	if err != nil {
		return nil, err
	}
	return &qemuLaunch{
		machineArgs:  machineArgs,
		readyTimeout: timeout,
		start:        func(cmd *exec.Cmd) error { return startPinned(cmd, mask) },
	}, nil
}
