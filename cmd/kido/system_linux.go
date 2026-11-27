package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func ctrReadHostInfo() ctrHostInfo {
	return ctrHostInfo{
		OperatingSystem: linuxOSName(),
		KernelVersion:   linuxKernelRelease(),
		MemTotal:        linuxMemTotal(),
	}
}

func linuxOSName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	if name := parseOSRelease(string(b)); name != "" {
		return name
	}
	return "Linux"
}

func parseOSRelease(data string) string {
	var pretty, name, versionID string
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = unquoteOSRelease(strings.TrimSpace(v))
		switch strings.TrimSpace(k) {
		case "PRETTY_NAME":
			pretty = v
		case "NAME":
			name = v
		case "VERSION_ID":
			versionID = v
		}
	}
	if pretty != "" {
		return pretty
	}
	return strings.TrimSpace(name + " " + versionID)
}

func unquoteOSRelease(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		s = s[1 : len(s)-1]
	}
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`).Replace(s)
}

func linuxKernelRelease() string {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return ""
	}
	return unix.ByteSliceToString(uts.Release[:])
}

func linuxMemTotal() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMemInfoTotal(string(b))
}

func parseMemInfoTotal(data string) int64 {
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb < 0 {
			return 0
		}
		return kb * 1024
	}
	return 0
}
