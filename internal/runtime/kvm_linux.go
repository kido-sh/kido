package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"

	"golang.org/x/sys/unix"
)

const (
	kvmGetAPIVersion      = 0xae00
	kvmExpectedAPIVersion = 12
)

func probeDevKVM() (int, error) {
	fd, err := unix.Open("/dev/kvm", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
			return 0, fmt.Errorf("cannot open /dev/kvm: %w (add your user to the kvm group with: sudo usermod -aG kvm $USER, then re-login, or set KIDO_ACCEL=tcg)", err)
		}
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENODEV) {
			return 0, fmt.Errorf("KVM is unavailable (/dev/kvm: %w), enable KVM kernel support or set KIDO_ACCEL=tcg", err)
		}
		return 0, fmt.Errorf("open /dev/kvm: %w", err)
	}
	defer unix.Close(fd)
	ver, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), kvmGetAPIVersion, 0)
	if errno != 0 {
		return 0, fmt.Errorf("KVM_GET_API_VERSION on /dev/kvm failed: %w", errno)
	}
	if int(ver) != kvmExpectedAPIVersion {
		return int(ver), fmt.Errorf("/dev/kvm API version %d, want %d", int(ver), kvmExpectedAPIVersion)
	}
	return int(ver), nil
}

func linuxAllowedCPUs() ([]int, error) {
	var allowed unix.CPUSet
	if err := unix.SchedGetaffinity(0, &allowed); err != nil {
		return nil, fmt.Errorf("sched_getaffinity: %w", err)
	}
	var cpus []int
	limit := len(allowed) * 64
	for id := 0; id < limit; id++ {
		if allowed.IsSet(id) {
			cpus = append(cpus, id)
		}
	}
	return cpus, nil
}

func readLinuxCPUInfos(allowed []int) []CPUInfo {
	return readCPUInfosFrom(allowed, "/sys/devices/system/cpu", "/proc/cpuinfo")
}

func SelectLinuxKVMAffinity() (KVMAffinityPlan, error) {
	allowed, err := linuxAllowedCPUs()
	if err != nil {
		return KVMAffinityPlan{}, err
	}
	return SelectKVMAffinity(readLinuxCPUInfos(allowed), os.Getenv("KIDO_KVM_CPUS"))
}

func ProbeLinuxKVM() KVMDiag {
	var d KVMDiag
	ver, err := probeDevKVM()
	d.APIVersion = ver
	d.Err = err
	d.Available = err == nil
	plan, perr := SelectLinuxKVMAffinity()
	d.Plan = plan
	d.PlanErr = perr
	return d
}

func cpuSetFromIDs(ids []int) (*unix.CPUSet, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var set unix.CPUSet
	limit := len(set) * 64
	for _, id := range ids {
		if id < 0 || id >= limit {
			return nil, fmt.Errorf("CPU %d out of CPUSet range [0, %d)", id, limit)
		}
		set.Set(id)
	}
	return &set, nil
}

func startPinned(cmd *exec.Cmd, mask *unix.CPUSet) error {
	if mask == nil {
		return cmd.Start()
	}
	errc := make(chan error, 1)
	go func() {
		goruntime.LockOSThread()
		if err := unix.SchedSetaffinity(0, mask); err != nil {
			errc <- fmt.Errorf("sched_setaffinity: %w", err)
			return
		}
		errc <- cmd.Start()
	}()
	return <-errc
}
