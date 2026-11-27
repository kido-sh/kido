//go:build linux

package runtime

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kido-sh/kido/internal/imageref"
	"github.com/kido-sh/kido/internal/mount"
	"github.com/kido-sh/kido/internal/proto"
	"golang.org/x/sys/unix"
)

const (
	kvmGuestIP   = "10.0.2.15"
	kvmGatewayIP = "10.0.2.2"
	kvmDNSIP     = "10.0.2.3"
	kvmMaxCPUs   = 32
)

func linuxKVMAvailable() bool {
	if VMMPath() == "" {
		return false
	}
	mode, err := KVMModeFromEnv()
	if err != nil || mode == KVMModeTCG {
		return false
	}
	_, err = probeDevKVM()
	return err == nil
}

func kvmMAC(id string) string {
	b, err := hex.DecodeString(id)
	if err != nil || len(b) < 3 {
		return "02:dc:72:00:00:01"
	}
	return fmt.Sprintf("02:dc:72:%02x:%02x:%02x", b[0], b[1], b[2])
}

func (r *Runtime) kvmPrepareDisk(info *Info, backing string) (*Info, error) {
	cleanup := func() {
		if idRE.MatchString(info.ID) {
			os.RemoveAll(r.dir(info.ID))
		}
	}
	ok, err := tryReflink(backing, r.file(info.ID, "disk.img"))
	if err != nil {
		cleanup()
		return nil, err
	}
	if ok {
		return info, nil
	}
	st, err := os.Lstat(backing)
	if err != nil {
		cleanup()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return r.vzPrepareDisk(info, backing)
	}
	basePath := r.file(info.ID, "base.img")
	err = os.Link(backing, basePath)
	if err == nil {
		bst, serr := os.Lstat(basePath)
		if serr != nil {
			cleanup()
			return nil, serr
		}
		if !bst.Mode().IsRegular() || bst.Size() < cowSectorSize {
			os.Remove(basePath)
			return r.vzPrepareDisk(info, backing)
		}
		diskSize := (bst.Size() / cowSectorSize) * cowSectorSize
		if err := createCowOverlay(r.file(info.ID, "disk.cow"), diskSize); err != nil {
			cleanup()
			return nil, err
		}
		return info, nil
	}
	if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EMLINK) {
		return r.vzPrepareDisk(info, backing)
	}
	cleanup()
	return nil, err
}

func kvmBootArgs(info *Info, imgDir, mac string, agentPort int, pinCPUs []int, mountArgs []string, cow bool) []string {
	args := []string{
		"--kernel", filepath.Join(imgDir, "kernel"),
		"--initrd", filepath.Join(imgDir, "initrd.cpio"),
	}
	if cow {
		args = append(args, "--disk-base", "base.img", "--disk", "disk.cow")
	} else {
		args = append(args, "--disk", "disk.img")
	}
	args = append(args,
		"--cmdline-file", "boot-args",
		"--cpus", strconv.Itoa(info.CPUs),
		"--memory", strconv.Itoa(info.MemoryMB),
		"--mac", mac,
		"--console-log", "console.log",
		"--state-file", "vmm.state",
	)
	if len(pinCPUs) > 0 {
		args = append(args, "--pin-cpus", FormatCPUList(pinCPUs))
	}
	if info.Network == NetworkNone {
		args = append(args, "--restrict")
	}
	args = append(args, "--forward", fmt.Sprintf("tcp:127.0.0.1:%d:%s:%d", agentPort, kvmGuestIP, proto.DefaultPort))
	for _, p := range info.Ports {
		args = append(args, "--forward", fmt.Sprintf("%s:%s:%d:%s:%d", p.Protocol, p.HostIP, p.HostPort, kvmGuestIP, p.GuestPort))
	}
	return append(args, mountArgs...)
}

func (r *Runtime) kvmBoot(ctx context.Context, info *Info) error {
	ref, err := imageref.Parse(info.Image)
	if err != nil {
		return err
	}
	if !uuidRE.MatchString(info.RootUUID) {
		return fmt.Errorf("image %s: invalid root UUID %q", info.Image, info.RootUUID)
	}
	mode, err := KVMModeFromEnv()
	if err != nil {
		return err
	}
	if mode == KVMModeTCG {
		return errors.New("KIDO_ACCEL=tcg is not supported by the kvm backend; unset KIDO_ACCEL or use KIDO_BACKEND=qemu")
	}
	vmm := VMMPath()
	if vmm == "" {
		return errors.New("the kvm backend needs kido-vmm: install it next to kido or in ../libexec/kido, set KIDO_VMM, or use KIDO_BACKEND=qemu")
	}
	if _, err := probeDevKVM(); err != nil {
		return err
	}
	plan, err := SelectLinuxKVMAffinity()
	if err != nil {
		return err
	}
	if plan.MaxCPUs > kvmMaxCPUs {
		plan.MaxCPUs = kvmMaxCPUs
	}
	if _, err := resolvePlanCPUs(info.CPUs, plan); err != nil {
		return err
	}
	authArgs, err := r.authBootArgs(info.ID)
	if err != nil {
		return err
	}
	agentPort, err := freePort("127.0.0.1", "tcp")
	if err != nil {
		return err
	}
	dir := r.dir(info.ID)
	for _, f := range []string{"agent.port", "vmm.state", "boot-args", "console.log.1"} {
		os.Remove(filepath.Join(dir, f))
	}
	margs, shares, err := mount.VZArgs(dir)
	if err != nil {
		return err
	}
	if len(shares) > 0 {
		return fmt.Errorf("live virtio-fs shares are not yet supported by the Linux kvm backend; recreate with copy mounts")
	}
	for _, f := range []string{"console.log", "vmm.log"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			return err
		}
	}
	vlog, err := os.OpenFile(filepath.Join(dir, "vmm.log"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer vlog.Close()

	bootArgs := strings.Join(slices.Concat([]string{
		"serial=3", "kido_rtc_wait=5", "kido_rtc_delay=0", "kido_quiet_wait=0",
		"ext4-uuid=" + info.RootUUID,
		"kido_ip=" + kvmGuestIP, "kido_gw=" + kvmGatewayIP, "kido_dns=" + kvmDNSIP,
		"kido_time=" + strconv.FormatInt(time.Now().Unix(), 10),
	}, authArgs), " ")
	if err := os.WriteFile(filepath.Join(dir, "boot-args"), []byte(bootArgs), 0o600); err != nil {
		return err
	}

	cow := false
	if _, err := os.Lstat(filepath.Join(dir, "disk.cow")); err == nil {
		cow = true
	} else if !errors.Is(err, os.ErrNotExist) {
		os.Remove(filepath.Join(dir, "boot-args"))
		return err
	}
	args := kvmBootArgs(info, ref.Dir(r.Home), kvmMAC(info.ID), agentPort, plan.PinCPUs, margs, cow)
	cmd := exec.Command(vmm, args...)
	cmd.Dir = dir
	cmd.Stdout = vlog
	cmd.Stderr = vlog
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		os.Remove(filepath.Join(dir, "boot-args"))
		return fmt.Errorf("start %s: %w", vmm, err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	info.PID = cmd.Process.Pid
	info.PIDStart = procStart(info.PID)
	info.AgentPort = agentPort
	info.StartedAt = time.Now().UTC()
	info.FinishedAt = time.Time{}
	info.ExitCode = nil
	info.Error = ""
	info.Stopping = false
	if err := os.WriteFile(filepath.Join(dir, "agent.port"), []byte(strconv.Itoa(agentPort)), 0o600); err != nil {
		os.Remove(filepath.Join(dir, "boot-args"))
		cmd.Process.Kill()
		return err
	}
	if err := r.save(info); err != nil {
		os.Remove(filepath.Join(dir, "boot-args"))
		cmd.Process.Kill()
		return err
	}

	err = r.waitAgent(ctx, info.ID, agentPort, exited)
	os.Remove(filepath.Join(dir, "boot-args"))
	if err == nil {
		if err = mount.Populate(ctx, dir, r.mountGuest(info.ID)); err == nil {
			return nil
		}
		cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
		info.PID, info.FinishedAt, info.Error = 0, time.Now().UTC(), Sanitize(err.Error())
		r.save(info)
		return err
	}

	cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
	}
	info.PID = 0
	info.FinishedAt = time.Now().UTC()
	info.Error = Sanitize(err.Error())
	r.save(info)
	if ctx.Err() != nil {
		return fmt.Errorf("boot interrupted: %w", ctx.Err())
	}
	msg := err.Error()
	if t := tail(filepath.Join(dir, "console.log"), 20); t != "" {
		msg += "\nconsole.log (last lines):\n" + t
	}
	return vzFailure(vmm, dir, msg)
}
