package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"golang.org/x/term"

	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("save", "Save one or more images to a tar archive (stdout by default)", ctrSave)
	register("load", "Load images from a tar archive or stdin", ctrLoad)
}

func ctrSave(ctx context.Context, args []string) int {
	fs := ctrFlags("save", "[OPTIONS] IMAGE [IMAGE...]")
	var output string
	fs.StringVar(&output, "o", "", "write to a file, instead of STDOUT")
	fs.StringVar(&output, "output", "", "write to a file, instead of STDOUT")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 125
	}
	if output == "" && term.IsTerminal(int(os.Stdout.Fd())) {
		return ctrFail(errors.New("refusing to write an image archive to a terminal; use -o or redirect"))
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	if output == "" {
		// A reader that goes away then fails the write with EPIPE instead of killing kido before
		// it removes its temporary files.
		signal.Ignore(syscall.SIGPIPE)
		err = rt.SaveImages(ctx, os.Stdout, fs.Args())
	} else {
		err = ctrSaveFile(ctx, rt, output, fs.Args())
	}
	if err != nil {
		return ctrFail(err)
	}
	return 0
}

// ctrSaveFile writes the archive to a temporary file next to path and renames it into place, so
// path never holds half an archive.
func ctrSaveFile(ctx context.Context, rt *runtime.Runtime, path string, names []string) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	defer os.Remove(f.Name())
	if err := rt.SaveImages(ctx, f, names); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func ctrLoad(ctx context.Context, args []string) int {
	fs := ctrFlags("load", "[OPTIONS]")
	var input string
	var quiet bool
	fs.StringVar(&input, "i", "", "read from tar archive file, instead of STDIN")
	fs.StringVar(&input, "input", "", "read from tar archive file, instead of STDIN")
	fs.BoolVar(&quiet, "q", false, "suppress the load output")
	fs.BoolVar(&quiet, "quiet", false, "suppress the load output")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 125
	}
	in := os.Stdin
	if input != "" {
		f, err := os.Open(input)
		if err != nil {
			return ctrFail(err)
		}
		defer f.Close()
		in = f
	} else if term.IsTerminal(int(os.Stdin.Fd())) {
		return ctrFail(errors.New("refusing to read an image archive from a terminal; use -i or redirect"))
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	refs, err := rt.LoadImages(ctx, in)
	if !quiet {
		for _, ref := range refs {
			fmt.Println("Loaded image:", ref)
		}
	}
	if err != nil {
		return ctrFail(err)
	}
	return 0
}
