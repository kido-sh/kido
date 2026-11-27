//go:build linux

package runtime

import (
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
	args := kvmBootArgs(info, "/img", "02:dc:72:a1:b2:c3", 41234, []int{4, 5, 6, 7}, []string{"--extra-disk", "m0.img"}, false)
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
}
