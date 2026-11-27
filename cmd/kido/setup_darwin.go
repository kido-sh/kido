package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kido-sh/kido/internal/setup"
)

const setupSummary = "Pull the base image, and install QEMU for the qemu backend"

func setupUsage() string {
	return "[OPTIONS]\n\n" +
		"Pulls the base image " + defaultBaseImage + " as darwin:latest. With the qemu backend\n" +
		"(KIDO_BACKEND=qemu) or --qemu-url, it also downloads QEMU " + setup.PinnedQEMU.Version + " for darwin arm64 from\n" +
		"github.com/cocoa-xu/qemu-build, checks its pinned SHA-256, unpacks what kido needs into\n" +
		"KIDO_HOME/qemu, signs it for Hypervisor.framework when needed, checks that it starts and makes it\n" +
		"the QEMU kido uses (KIDO_QEMU still wins). Running it again downloads nothing."
}

func setupQEMU(ctx context.Context, home string, rel setup.Release, custom, force bool) int {
	env := os.Getenv("KIDO_QEMU")
	if env != "" && !custom && !force {
		if fi, err := os.Stat(env); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			return setupFail(fmt.Errorf("KIDO_QEMU=%s is not an executable file: fix or unset it, then run this again", env))
		}
		fmt.Fprintf(os.Stderr, "KIDO_QEMU is set, so kido uses %s and setup installs no QEMU.\n"+
			"Unset KIDO_QEMU to use the QEMU that kido setup installs (or pass --force to install it anyway).\n", env)
		return 0
	}
	res, err := setup.Install(ctx, setup.Options{Home: home, Release: rel, Force: force, Log: os.Stderr})
	if err != nil {
		return setupFail(err)
	}
	fmt.Printf("QEMU ready: %s\n  %s -> %s\n", res.Version, filepath.Join(setup.QEMUDir(home), "current"), rel.Version)
	if n := len(res.Libraries.External); n > 0 {
		fmt.Printf("  note: it loads %d libraries from outside %s (%s); it stops working if they go away\n",
			n, res.Dir, strings.Join(setup.HomebrewFormulas(res.Libraries.External), ", "))
	}
	if env != "" {
		fmt.Printf("  note: KIDO_QEMU=%s still takes precedence; unset it to use this QEMU\n", env)
	}
	return 0
}
