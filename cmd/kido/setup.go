package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/kido-sh/kido/internal/imageref"
	"github.com/kido-sh/kido/internal/registry"
	"github.com/kido-sh/kido/internal/runtime"
	"github.com/kido-sh/kido/internal/setup"
)

func init() {
	register("setup", "Install QEMU into KIDO_HOME (and pull a base image with --image)", runSetup)
}

func runSetup(ctx context.Context, args []string) int {
	fl := ctrFlags("setup", "[OPTIONS]\n\n"+
		"Downloads QEMU "+setup.PinnedQEMU.Version+" for darwin arm64 from github.com/cocoa-xu/qemu-build, checks its\n"+
		"pinned SHA-256, unpacks what kido needs into KIDO_HOME/qemu, signs it for Hypervisor.framework\n"+
		"when needed, checks that it starts and makes it the QEMU kido uses (KIDO_QEMU still wins).\n"+
		"Running it again downloads nothing.")
	image := fl.String("image", "", "Also pull this base image (REMOTE_REF) from an OCI registry")
	imageName := fl.String("image-name", "darwin:latest", "Local NAME[:TAG] for the --image pull")
	force := fl.Bool("force", false, "Unpack QEMU again even when KIDO_QEMU is set or the version is installed, and replace an existing --image-name")
	qemuURL := fl.String("qemu-url", "", "Install QEMU from this https URL or local .tar.xz instead of the pinned release")
	qemuSHA := fl.String("qemu-sha256", "", "SHA-256 of the --qemu-url archive (required with it)")
	qemuVersion := fl.String("qemu-version", "", "Directory name for --qemu-url under KIDO_HOME/qemu (default: custom-<sha prefix>)")
	if err := fl.Parse(args); err != nil {
		return 125
	}
	if fl.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "kido: \"setup\" accepts no arguments.\nSee 'kido setup --help'.")
		return 125
	}
	home, err := filepath.Abs(runtime.DefaultHome())
	if err != nil {
		return setupFail(err)
	}
	rel, err := setupRelease(*qemuURL, *qemuSHA, *qemuVersion)
	if err != nil {
		return setupFail(err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	status := 0
	env := os.Getenv("KIDO_QEMU")
	if env != "" && *qemuURL == "" && !*force {
		if fi, err := os.Stat(env); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			status = setupFail(fmt.Errorf("KIDO_QEMU=%s is not an executable file: fix or unset it (then kido setup installs QEMU)", env))
		} else {
			fmt.Fprintf(os.Stderr, "KIDO_QEMU is set, so kido uses %s and setup installs no QEMU.\n"+
				"Unset KIDO_QEMU to use the QEMU that kido setup installs (or pass --force to install it anyway).\n", env)
		}
	} else {
		res, err := setup.Install(ctx, setup.Options{Home: home, Release: rel, Force: *force, Log: os.Stderr})
		if err != nil {
			status = setupFail(err)
		} else {
			fmt.Printf("QEMU ready: %s\n  %s -> %s\n", res.Version, filepath.Join(setup.QEMUDir(home), "current"), rel.Version)
			if n := len(res.Libraries.External); n > 0 {
				fmt.Printf("  note: it loads %d libraries from outside %s (%s); it stops working if they go away\n",
					n, res.Dir, strings.Join(setup.HomebrewFormulas(res.Libraries.External), ", "))
			}
			if env != "" {
				fmt.Printf("  note: KIDO_QEMU=%s still takes precedence; unset it to use this QEMU\n", env)
			}
		}
	}

	if *image != "" {
		if st := setupPull(ctx, home, *image, *imageName, *force); st != 0 {
			status = st
		}
	} else if !setupHaveImages(home) {
		fmt.Println("No image yet: pull one with `kido setup --image REMOTE_REF`, or import one with `kido image import DIR darwin`.")
	}
	if status == 0 {
		fmt.Println("Run `kido doctor` to check the whole setup.")
	}
	return status
}

func setupRelease(url, sha, version string) (setup.Release, error) {
	if url == "" {
		if sha != "" || version != "" {
			return setup.Release{}, errors.New("--qemu-sha256 and --qemu-version only go with --qemu-url")
		}
		return setup.PinnedQEMU, nil
	}
	if len(sha) != 64 {
		return setup.Release{}, errors.New("--qemu-url needs --qemu-sha256 with the archive's 64-digit SHA-256")
	}
	if !strings.HasPrefix(url, "https://") {
		if strings.Contains(url, "://") {
			return setup.Release{}, fmt.Errorf("--qemu-url %q: only https URLs and local files", url)
		}
		abs, err := filepath.Abs(url)
		if err != nil {
			return setup.Release{}, err
		}
		url = abs
	}
	if version == "" {
		version = "custom-" + strings.ToLower(sha[:12])
	}
	return setup.Release{Version: version, URL: url, SHA256: sha}, nil
}

func setupPull(ctx context.Context, home, remote, local string, force bool) int {
	ref, err := imageref.Parse(local)
	if err != nil {
		return setupFail(fmt.Errorf("--image-name: %w", err))
	}
	if _, err := os.Stat(filepath.Join(ref.Dir(home), "image.json")); err == nil && !force {
		fmt.Printf("Image %s is already here; pass --force to pull %s over it.\n", ref, remote)
		return 0
	}
	fmt.Fprintf(os.Stderr, "Pulling %s as %s\n", remote, ref)
	got, digest, err := registry.Pull(ctx, registry.PullOptions{
		Home: home, Remote: remote, Image: ref.String(), Force: force, Progress: os.Stderr,
		InUse: func(r string) error { return regImageInUse(ctx, home, r) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "kido setup: pull:", registry.Sanitize(err.Error()))
		return 125
	}
	fmt.Printf("Image ready: %s (%s from %s)\n", got, digest, remote)
	return 0
}

func setupHaveImages(home string) bool {
	m, _ := filepath.Glob(filepath.Join(home, "images", "*", "*", "image.json"))
	return len(m) > 0
}

func setupFail(err error) int {
	fmt.Fprintln(os.Stderr, "kido setup:", err)
	return 125
}
