//go:build !linux

package main

import (
	"context"
	"fmt"
	"os"
	goruntime "runtime"
)

func runPlatformProbe(_ context.Context, _ []string) int {
	fmt.Fprintf(os.Stderr, "kido probe is only supported on ARM64 Linux hosts (detected %s/%s).\n", goruntime.GOOS, goruntime.GOARCH)
	if goruntime.GOOS == "darwin" {
		fmt.Fprintln(os.Stderr, "On macOS, run `kido doctor` to inspect Virtualization.framework and host Metal support.")
	}
	return 1
}
