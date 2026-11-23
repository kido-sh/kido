package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/kido-sh/kido/internal/registry"
	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("push", "Push an image to an OCI registry", regPush)
}

func regPush(ctx context.Context, args []string) int {
	fs := ctrFlags("push", "[OPTIONS] NAME[:TAG] [REMOTE_REF]\n\n"+
		"REMOTE_REF defaults to NAME[:TAG].\n"+
		"Credentials come from Docker's config (`docker login` or `kido login`); registries on\n"+
		"localhost or 127.0.0.1 are reached over plain HTTP, all others over HTTPS.")
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
	opts := registry.PushOptions{Home: runtime.DefaultHome(), Image: pos[0], Version: version, Progress: regProgress(*quiet)}
	if len(pos) == 2 {
		opts.Remote = pos[1]
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	ref, digest, err := registry.Push(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kido push:", registry.Sanitize(err.Error()))
		return 125
	}
	fmt.Printf("%s@%s\n", ref, digest)
	return 0
}

func regProgress(quiet bool) io.Writer {
	if quiet {
		return nil
	}
	return os.Stderr
}
