//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/kido-sh/kido/internal/runtime"
)

func isExecutableRegular(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

func findProbeBinary() (string, error) {
	if p := os.Getenv("KIDO_PROBE"); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("KIDO_PROBE must be an absolute path, got %q", p)
		}
		if !isExecutableRegular(p) {
			return "", fmt.Errorf("KIDO_PROBE=%q is not an executable file", p)
		}
		return p, nil
	}
	var dirs []string
	if vmm := runtime.VMMPath(); vmm != "" && filepath.IsAbs(vmm) {
		if real, err := filepath.EvalSymlinks(vmm); err == nil && filepath.IsAbs(real) {
			vmm = real
		}
		dirs = append(dirs, filepath.Dir(vmm))
	}
	if exe, err := os.Executable(); err == nil && exe != "" && filepath.IsAbs(exe) {
		if real, err := filepath.EvalSymlinks(exe); err == nil && filepath.IsAbs(real) {
			exe = real
		}
		d := filepath.Dir(exe)
		dirs = append(dirs, d, filepath.Join(d, "..", "libexec", "kido"))
	}
	for _, d := range dirs {
		p := filepath.Join(d, "kido-probe")
		if isExecutableRegular(p) {
			return p, nil
		}
	}
	if p, err := exec.LookPath("kido-probe"); err == nil && filepath.IsAbs(p) {
		return p, nil
	}
	return "", errors.New("kido-probe helper binary not found (install in libexec/kido or set KIDO_PROBE)")
}

func runPlatformProbe(ctx context.Context, args []string) int {
	bin, err := findProbeBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kido probe: %v\n", err)
		return 1
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				sig := ws.Signal()
				fmt.Fprintf(os.Stderr, "kido probe: terminated by signal %s\n", sig)
				return 128 + int(sig)
			}
			if code := ee.ExitCode(); code >= 0 {
				return code
			}
		}
		fmt.Fprintf(os.Stderr, "kido probe: %v\n", err)
		return 1
	}
	return 0
}
