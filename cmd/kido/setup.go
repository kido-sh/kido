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

const defaultBaseImage = "ghcr.io/kido-sh/darwin"

func init() {
	register("setup", "Pull the base image, and install QEMU for the qemu backend", runSetup)
}

func runSetup(ctx context.Context, args []string) int {
	fl := ctrFlags("setup", "[OPTIONS]\n\n"+
		"Pulls the base image "+defaultBaseImage+" as darwin:latest. With the qemu backend\n"+
		"(KIDO_BACKEND=qemu) or --qemu-url, it also downloads QEMU "+setup.PinnedQEMU.Version+" for darwin arm64 from\n"+
		"github.com/cocoa-xu/qemu-build, checks its pinned SHA-256, unpacks what kido needs into\n"+
		"KIDO_HOME/qemu, signs it for Hypervisor.framework when needed, checks that it starts and makes it\n"+
		"the QEMU kido uses (KIDO_QEMU still wins). Running it again downloads nothing.")
	image := fl.String("image", defaultBaseImage, "The base image (REMOTE_REF) to pull; an empty one pulls nothing")
	imageName := fl.String("image-name", "darwin:latest", "Local NAME[:TAG] for the base image")
	force := fl.Bool("force", false, "Pull the base image again over --image-name, and unpack QEMU again even when KIDO_QEMU is set or the version is installed")
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
	backend, err := runtime.BackendFromEnv()
	if err != nil {
		return setupFail(err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	status := 0
	if backend == "qemu" || *qemuURL != "" {
		status = setupQEMU(ctx, home, rel, *qemuURL != "", *force)
	}
	if *image != "" {
		if st := setupPull(ctx, home, *image, *imageName, *force); st != 0 {
			status = st
		}
	} else if !setupHaveImages(home) {
		fmt.Println("No image yet: `kido setup` pulls " + defaultBaseImage + ", and `kido image import DIR darwin` imports one.")
	}
	if status == 0 {
		fmt.Println("Run `kido doctor` to check the whole setup.")
	}
	return status
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
