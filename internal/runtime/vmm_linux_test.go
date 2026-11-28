//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kido-sh/kido/internal/engine"
)

func TestKVMMAC(t *testing.T) {
	if got := kvmMAC("a1b2c3d4e5f6"); got != "02:dc:72:a1:b2:c3" {
		t.Fatalf("kvmMAC = %q", got)
	}
	if got := kvmMAC("bad"); got != "02:dc:72:00:00:01" {
		t.Fatalf("kvmMAC fallback = %q", got)
	}
}

func TestKVMBootArgs(t *testing.T) {
	info := &Info{
		ID:       "a1b2c3d4e5f6",
		CPUs:     4,
		MemoryMB: 1024,
		Network:  NetworkNone,
		Ports: []engine.PortMapping{
			{Protocol: "tcp", HostIP: "127.0.0.1", HostPort: 8080, GuestPort: 80},
			{Protocol: "udp", HostIP: "127.0.0.1", HostPort: 9999, GuestPort: 9999},
		},
	}
	args := kvmBootArgs(info, "/img", "02:dc:72:a1:b2:c3", 41234, []int{4, 5, 6, 7}, []string{"--extra-disk", "m0.img", "--share", "m1=/srv/rw", "--share-ro", "m2=/srv/ro"}, false)
	for _, want := range []string{
		"--kernel", "/img/kernel",
		"--initrd", "/img/initrd.cpio",
		"--disk", "disk.img",
		"--cmdline-file", "boot-args",
		"--pin-cpus", "4-7",
		"--restrict",
		"tcp:127.0.0.1:41234:10.0.2.15:1023",
		"tcp:127.0.0.1:8080:10.0.2.15:80",
		"udp:127.0.0.1:9999:10.0.2.15:9999",
		"--extra-disk", "m0.img",
		"--share", "m1=/srv/rw",
		"--share-ro", "m2=/srv/ro",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("kvmBootArgs missing %q in %v", want, args)
		}
	}
	cowArgs := kvmBootArgs(info, "/img", "02:dc:72:a1:b2:c3", 41234, nil, nil, true)
	for _, want := range []string{"--disk-base", "base.img", "--disk", "disk.cow"} {
		if !slices.Contains(cowArgs, want) {
			t.Errorf("kvmBootArgs(cow) missing %q in %v", want, cowArgs)
		}
	}
	infoGPU := *info
	infoGPU.GPU = true
	infoGPU.GPUMemoryMB = 512
	tmpWorker := filepath.Join(t.TempDir(), "kido-vk-worker")
	if err := os.WriteFile(tmpWorker, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIDO_VK_WORKER", tmpWorker)
	gpuArgs := kvmBootArgs(&infoGPU, "/img", "02:dc:72:a1:b2:c3", 41234, nil, nil, false)
	for _, want := range []string{"--gpu", "--gpu-memory", "512", "--vk-worker", tmpWorker} {
		if !slices.Contains(gpuArgs, want) {
			t.Errorf("kvmBootArgs(gpu) missing %q in %v", want, gpuArgs)
		}
	}
}

func TestVKWorkerPathAndGPUSupportedLinux(t *testing.T) {
	if err := gpuSupported(backendVZ); err == nil {
		t.Fatal("expected gpuSupported(backendVZ) to fail on Linux")
	}

	dir := t.TempDir()
	nonExec := filepath.Join(dir, "kido-vk-worker-noexec")
	if err := os.WriteFile(nonExec, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIDO_VK_WORKER", nonExec)
	if got := VKWorkerPath(); got != "" {
		t.Fatalf("expected non-executable KIDO_VK_WORKER to return empty, got %q", got)
	}

	execPath := filepath.Join(dir, "kido-vk-worker")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIDO_VK_WORKER", execPath)
	if got := VKWorkerPath(); got != execPath {
		t.Fatalf("VKWorkerPath = %q, want %q", got, execPath)
	}

	t.Setenv("KIDO_VK_WORKER", "")
	t.Setenv("KIDO_VMM", filepath.Join(dir, "kido-vmm"))
	if err := os.WriteFile(filepath.Join(dir, "kido-vmm"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := VKWorkerPath(); got != execPath {
		t.Fatalf("VKWorkerPath sibling = %q, want %q", got, execPath)
	}
}
