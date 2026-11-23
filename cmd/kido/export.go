package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("export", "Export a container's filesystem as a tar archive", ctrExport)
}

// ctrExportWarnings prints what export left out: the first few paths, then a count.
type ctrExportWarnings struct{ n int }

func (w *ctrExportWarnings) skipped(s string) {
	w.n++
	if w.n <= 20 {
		kind, p, _ := strings.Cut(s, " ")
		fmt.Fprintf(os.Stderr, "kido: export: skipped %s %s\n", kind, runtime.Sanitize(strings.TrimPrefix(p, ".")))
	}
}

func (w *ctrExportWarnings) done() {
	if w.n > 20 {
		fmt.Fprintf(os.Stderr, "kido: export: skipped %d more sockets or device nodes\n", w.n-20)
	}
}

func ctrExport(ctx context.Context, args []string) int {
	fset := ctrFlags("export", "[OPTIONS] CONTAINER")
	var output string
	fset.StringVar(&output, "o", "", "write to a file, instead of STDOUT")
	fset.StringVar(&output, "output", "", "write to a file, instead of STDOUT")
	if err := fset.Parse(ctrInterspersed(fset, args)); err != nil {
		return 125
	}
	if fset.NArg() != 1 {
		fset.Usage()
		return 125
	}
	if output == "" && term.IsTerminal(int(os.Stdout.Fd())) {
		return ctrFail(errors.New("refusing to write a tar archive to a terminal; use -o or redirect"))
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	info, err := rt.Get(fset.Arg(0))
	if err != nil {
		return ctrFail(err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	if info.State != engine.StateRunning {
		restore, err := ctrBootForExport(ctx, rt, info)
		if err != nil {
			return ctrFail(err)
		}
		defer restore()
	}
	warn := &ctrExportWarnings{}
	defer warn.done()
	if output == "" {
		signal.Ignore(syscall.SIGPIPE)
		err = rt.Export(ctx, info.ID, os.Stdout, warn.skipped)
	} else {
		err = ctrWriteFileAtomic(output, func(w io.Writer) error { return rt.Export(ctx, info.ID, w, warn.skipped) })
	}
	if err != nil {
		return ctrFail(err)
	}
	return 0
}

// ctrBootForExport boots a container that is not running, without its command, so its files can
// be read; the returned func stops it again and puts back the state it had (Created or Exited).
func ctrBootForExport(ctx context.Context, rt *runtime.Runtime, info *runtime.Info) (func(), error) {
	startedAt, finishedAt, exitCode, errMsg := info.StartedAt, info.FinishedAt, info.ExitCode, info.Error
	restore := func() {
		if err := rt.Stop(context.Background(), info.ID, 10*time.Second); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			fmt.Fprintln(os.Stderr, "kido: stop:", err)
		}
		rt.Update(info.ID, func(i *runtime.Info) {
			i.StartedAt, i.FinishedAt, i.ExitCode, i.Error = startedAt, finishedAt, exitCode, errMsg
		})
	}
	if _, err := ctrStartContainer(ctx, rt, info.ID, false); err != nil {
		restore()
		return nil, err
	}
	return restore, nil
}

// ctrWriteFileAtomic writes path through a temporary file next to it, so path never holds a
// partial result.
func ctrWriteFileAtomic(path string, write func(io.Writer) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	defer os.Remove(f.Name())
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
