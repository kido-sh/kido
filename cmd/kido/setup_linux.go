package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/kido-sh/kido/internal/runtime"
	"github.com/kido-sh/kido/internal/setup"
)

const setupSummary = "Pull the base image and check Linux QEMU and KVM prerequisites"

func setupUsage() string {
	return "[OPTIONS]\n\n" +
		"Pulls the base image " + defaultBaseImage + " as darwin:latest and verifies that system\n" +
		"qemu-system-aarch64, qemu-img and /dev/kvm are ready. Running it again downloads nothing."
}

func setupQEMU(ctx context.Context, home string, _ setup.Release, custom, _ bool) int {
	if custom {
		return setupFail(errors.New("--qemu-url installs macOS QEMU archives, on Linux install system QEMU " +
			"(such as `sudo apt install qemu-system-arm qemu-utils`) or set KIDO_QEMU"))
	}
	status := 0
	q, err := setup.FindQEMU(home)
	if err != nil {
		setupFail(err)
		status = 125
	} else {
		if err := isExecutableFile(q.Path); err != nil {
			setupFail(fmt.Errorf("%s (%s): %v", q.Path, q.Source, err))
			status = 125
		} else if out, err := runFixed(ctx, nil, q.Path, "--version"); err != nil {
			setupFail(fmt.Errorf("%s does not start: %v", q.Path, err))
			status = 125
		} else {
			fmt.Printf("QEMU ready: %s (%s)\n", firstLine(out), q.Path)
		}
		if err := isExecutableFile(q.Img); err != nil {
			setupFail(fmt.Errorf("qemu-img (%s): %v, install it with `sudo apt install qemu-utils`", q.Img, err))
			status = 125
		} else if _, err := runFixed(ctx, nil, q.Img, "--version"); err != nil {
			setupFail(fmt.Errorf("%s does not start: %v", q.Img, err))
			status = 125
		}
	}

	mode, merr := runtime.KVMModeFromEnv()
	if merr != nil {
		setupFail(merr)
		return 125
	}
	kvm := checkKVM(runtime.ProbeLinuxKVM(), mode, nil)
	switch kvm.status {
	case checkPass:
		fmt.Printf("KVM ready: %s\n", kvm.detail)
	case checkWarn:
		fmt.Fprintf(os.Stderr, "kido setup: note: %s (%s)\n", kvm.detail, kvm.fix)
	case checkFail:
		fmt.Fprintf(os.Stderr, "kido setup: %s\n  fix: %s\n", kvm.detail, kvm.fix)
		status = 125
	}
	return status
}
