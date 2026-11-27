package main

import (
	"strings"

	"golang.org/x/sys/unix"
)

func ctrSysctl(name string) string {
	s, err := unix.Sysctl(name)
	if err != nil {
		return ""
	}
	return s
}

func ctrReadHostInfo() ctrHostInfo {
	info := ctrHostInfo{
		OperatingSystem: strings.TrimSpace("macOS " + ctrSysctl("kern.osproductversion")),
		KernelVersion:   ctrSysctl("kern.osrelease"),
	}
	if mem, err := unix.SysctlUint64("hw.memsize"); err == nil {
		info.MemTotal = int64(mem)
	}
	return info
}
