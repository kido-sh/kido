package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/kido-sh/kido/internal/build"
	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("build", "Build an image from a Dockerfile", buildMain)
}

const buildUsage = `[OPTIONS] PATH

Runs in a build VM started from the FROM image.
Instructions: FROM (one local image; no scratch, no multi-stage), RUN, COPY, ADD, ENV, ARG,
WORKDIR, USER, CMD, ENTRYPOINT, LABEL, EXPOSE (kept in the kido.expose label).
COPY and ADD read from PATH only and honour .dockerignore. ADD takes local files only:
URLs are refused, and a local tar archive is copied as a file, not unpacked.
Exit status: that of the failing RUN step, or 125 for any other error.`

// buildOutput passes the builder's output through, holding back a write that is a whole
// "Successfully built" line until more output follows, so that the last one can be replaced.
type buildOutput struct {
	w    io.Writer
	held []byte
}

func (o *buildOutput) Write(p []byte) (int, error) {
	if err := o.flush(); err != nil {
		return 0, err
	}
	if bytes.HasPrefix(p, []byte("Successfully built ")) && bytes.IndexByte(p, '\n') == len(p)-1 {
		o.held = append([]byte(nil), p...)
		return len(p), nil
	}
	return o.w.Write(p)
}

func (o *buildOutput) flush() error {
	if o.held == nil {
		return nil
	}
	_, err := o.w.Write(o.held)
	o.held = nil
	return err
}

func buildMain(ctx context.Context, args []string) int {
	fs := ctrFlags("build", buildUsage)
	var tags, buildArgs ctrList
	fs.Var(&tags, "t", "name and optional tag of the image, NAME[:TAG]")
	fs.Var(&tags, "tag", "name and optional tag of the image, NAME[:TAG]")
	file := fs.String("f", "", "the Dockerfile (default PATH/Dockerfile; - reads standard input)")
	fs.StringVar(file, "file", "", "the Dockerfile (default PATH/Dockerfile; - reads standard input)")
	fs.Var(&buildArgs, "build-arg", "set a build-time variable KEY=VALUE, or KEY to take its value from the environment")
	noRm := fs.Bool("no-rm", false, "keep the build container")
	rm := fs.Bool("rm", true, "remove the build container when the build ends")
	quiet := fs.Bool("q", false, "suppress the build output and print the image ID on success")
	fs.BoolVar(quiet, "quiet", false, "suppress the build output and print the image ID on success")
	fs.Bool("no-cache", false, "accepted for Docker compatibility: kido has no build cache, every step runs")
	fs.Bool("pull", false, "accepted for Docker compatibility: FROM uses the local image (see kido pull)")
	cpus := fs.Int("cpus", 0, "virtual CPUs of the build VM (default 2)")
	memory := fs.String("memory", "", "memory of the build VM in MB, or with an m/g suffix (default 1024)")
	fs.StringVar(memory, "m", "", "memory of the build VM (same as --memory)")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 125
	}
	pos := fs.Args()
	mem, err := ctrMemory(*memory)
	if err != nil {
		return buildFail(err)
	}
	if len(tags) > 1 {
		return buildFail(errors.New("only one -t is supported"))
	}
	contextDir := pos[0]
	if contextDir == "-" || strings.Contains(contextDir, "://") {
		return buildFail(fmt.Errorf("context %s: only a local directory is supported", contextDir))
	}
	out := &buildOutput{w: os.Stdout}
	if *quiet {
		out.w = io.Discard
	}
	opts := build.Options{
		ContextDir: contextDir, NoRm: *noRm || !*rm, CPUs: *cpus, MemoryMB: mem,
		Stdout: out, Stderr: os.Stderr, BuildArgs: map[string]string{},
	}
	if len(tags) == 1 {
		opts.Tag = tags[0]
	}
	for _, a := range buildArgs {
		k, v, ok := strings.Cut(a, "=")
		if k == "" {
			return buildFail(fmt.Errorf("--build-arg %q: want KEY=VALUE", a))
		}
		if !ok {
			if v, ok = os.LookupEnv(k); !ok {
				continue
			}
		}
		opts.BuildArgs[k] = v
	}
	switch *file {
	case "":
	case "-":
		if opts.Dockerfile, err = io.ReadAll(io.LimitReader(os.Stdin, 4<<20+1)); err == nil && len(opts.Dockerfile) > 4<<20 {
			err = errors.New("standard input holds more than 4 MiB")
		}
	default:
		opts.Dockerfile, err = os.ReadFile(*file)
	}
	if err != nil {
		return buildFail(fmt.Errorf("Dockerfile: %w", err))
	}

	rt, err := runtime.New(runtime.DefaultHome())
	if err != nil {
		return buildFail(err)
	}
	// Ctrl-C or a closed terminal cancels the build, which still removes the build container
	// (main already does this for SIGTERM).
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGHUP)
	defer stop()
	img, err := build.Build(ctx, rt, opts)
	if err != nil {
		out.flush()
		return buildFail(err)
	}
	ref := img.Name + ":" + img.Tag
	if stored, err := rt.Image(ctx, ref); err == nil {
		img = stored
	}
	id := ctrImageID(img)
	if string(out.held) != "Successfully built "+ref+"\n" {
		out.flush()
	}
	if *quiet {
		fmt.Println("sha256:" + id)
		return 0
	}
	fmt.Printf("Successfully built %s\n", id[:12])
	if opts.Tag != "" {
		fmt.Printf("Successfully tagged %s\n", ref)
	}
	return 0
}

func buildFail(err error) int {
	fmt.Fprintln(os.Stderr, "kido build:", err)
	if se, ok := errors.AsType[*build.StepError](err); ok {
		return se.ExitStatus
	}
	return 125
}
