package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
	"github.com/kido-sh/kido/internal/registry"
	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("pull", "Pull an image from an OCI registry", regPull)
}

func regPull(ctx context.Context, args []string) int {
	fs := ctrFlags("pull", "[OPTIONS] REMOTE_REF [LOCAL_NAME[:TAG]]\n\n"+
		"Pulls into the local store; LOCAL_NAME defaults to REMOTE_REF.")
	force := fs.Bool("force", false, "replace an existing local image")
	quiet := fs.Bool("q", false, "suppress progress output")
	fs.BoolVar(quiet, "quiet", false, "suppress progress output")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	pos := fs.Args()
	if len(pos) < 1 || len(pos) > 2 {
		fs.Usage()
		return 125
	}
	home := runtime.DefaultHome()
	opts := registry.PullOptions{Home: home, Remote: pos[0], Force: *force, Progress: regProgress(*quiet)}
	if len(pos) == 2 {
		opts.Image = pos[1]
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	opts.InUse = func(ref string) error { return regImageInUse(ctx, home, ref) }
	ref, digest, err := registry.Pull(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kido pull:", registry.Sanitize(err.Error()))
		return 125
	}
	fmt.Fprintln(os.Stderr, "Digest:", digest)
	fmt.Println(ref)
	return 0
}

func checkPullPolicy(policy string) error {
	if policy != "missing" && policy != "never" {
		return fmt.Errorf("invalid --pull value %q: kido supports missing and never", policy)
	}
	return nil
}

func imageOrPull(ctx context.Context, rt *runtime.Runtime, name, policy string, progress io.Writer) (*engine.Image, error) {
	img, err := rt.Image(ctx, name)
	if policy == "never" || !errors.Is(err, runtime.ErrNoImage) {
		return img, err
	}
	ref, err := imageref.Parse(name)
	if err != nil {
		return nil, err
	}
	if progress != nil {
		fmt.Fprintf(progress, "Unable to find image '%s' locally\n", ref)
	}
	pullCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	_, digest, err := registry.Pull(pullCtx, registry.PullOptions{Home: rt.Home, Remote: ref.String(), Progress: progress})
	if err != nil {
		if img, ierr := rt.Image(ctx, name); ierr == nil {
			return img, nil
		}
		return nil, fmt.Errorf("pull %s: %s", ref, registry.Sanitize(err.Error()))
	}
	if progress != nil {
		fmt.Fprintln(progress, "Digest:", digest)
		fmt.Fprintf(progress, "Status: Downloaded newer image for %s\n", ref)
	}
	return rt.Image(ctx, name)
}

// regImageInUse refuses to replace an image that backs a container's disk.
func regImageInUse(ctx context.Context, home, ref string) error {
	rt, err := runtime.New(home)
	if err != nil {
		return err
	}
	cs, err := rt.List(ctx, true)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.Image == ref {
			return fmt.Errorf("image %s is in use by container %s: remove the container first", ref, c.Name)
		}
	}
	return nil
}
