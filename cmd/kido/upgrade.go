package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/kido-sh/kido/internal/upgrade"
)

func init() {
	register("upgrade", "Upgrade kido to the latest release", runUpgrade)
}

func runUpgrade(ctx context.Context, args []string) int {
	fs := ctrFlags("upgrade", "[OPTIONS]\n\n"+
		"Downloads the latest kido release from GitHub, checks its signature and SHA-256, and replaces\n"+
		"this kido and its helpers with it. A kido from Homebrew is upgraded with `brew upgrade kido`.")
	check := fs.Bool("check", false, "only show whether a newer release exists")
	want := fs.String("version", "", "install this release instead of the latest one, even an older one")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 125
	}
	exe, err := os.Executable()
	if err != nil {
		return upgradeFail(err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	res, err := upgrade.Run(ctx, upgrade.Options{
		Current: version, Want: strings.TrimPrefix(*want, "v"), Check: *check, Exe: exe, Log: os.Stderr,
	})
	if err != nil {
		return upgradeFail(err)
	}
	switch {
	case res.Upgraded:
		fmt.Printf("Upgraded kido %s to %s.\n", res.Current, res.Release)
	case res.Release == res.Current && *want != "":
		fmt.Printf("kido %s is already installed.\n", res.Current)
	case res.Release == res.Current:
		fmt.Printf("kido %s is the latest release.\n", res.Current)
	case res.Newer:
		fmt.Printf("kido %s is available. This kido is %s. Run `kido upgrade` to install it.\n", res.Release, res.Current)
	default:
		fmt.Printf("The latest release is kido %s. This kido is %s.\n", res.Release, res.Current)
	}
	return 0
}

func upgradeFail(err error) int {
	fmt.Fprintln(os.Stderr, "kido upgrade:", err)
	return 125
}
