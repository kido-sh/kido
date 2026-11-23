package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("logs", "Fetch the output of a container's main process (or its serial console)", ctrLogs)
}

// ctrTail positions f at the start of its last n lines.
func ctrTail(f *os.File, n int) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	const chunk = 64 << 10
	end := st.Size()
	pos := end
	lines := 0
	buf := make([]byte, chunk)
	for pos > 0 {
		size := int64(chunk)
		if pos < size {
			size = pos
		}
		pos -= size
		if _, err := f.ReadAt(buf[:size], pos); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		b := buf[:size]
		for i := len(b) - 1; i >= 0; i-- {
			if b[i] != '\n' || pos+int64(i) == end-1 {
				continue
			}
			lines++
			if lines == n {
				_, err := f.Seek(pos+int64(i)+1, io.SeekStart)
				return err
			}
		}
	}
	_, err = f.Seek(0, io.SeekStart)
	return err
}

func ctrLogs(ctx context.Context, args []string) int {
	fl := ctrFlags("logs", "[OPTIONS] CONTAINER")
	follow := fl.Bool("f", false, "follow log output while the container runs")
	fl.BoolVar(follow, "follow", false, "follow log output while the container runs")
	tail := fl.String("n", "all", "number of lines to show from the end of the logs")
	fl.StringVar(tail, "tail", "all", "number of lines to show from the end of the logs")
	console := fl.Bool("console", false, "show the VM's serial console instead of the main process output")
	if err := fl.Parse(ctrExpand(ctrInterspersed(fl, args), "f", "n", "tail")); err != nil {
		return 125
	}
	if fl.NArg() != 1 {
		fl.Usage()
		return 125
	}
	lines := -1
	if *tail != "all" {
		n, err := strconv.Atoi(*tail)
		if err != nil || n < 0 {
			return ctrFail(errors.New("--tail: want a number of lines or all"))
		}
		lines = n
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	info, err := rt.Get(fl.Arg(0))
	if err != nil {
		return ctrFail(err)
	}
	name := "main.log"
	if *console {
		name = "console.log"
	}
	f, err := os.Open(filepath.Join(rt.ContainerDir(info.ID), name))
	if errors.Is(err, fs.ErrNotExist) {
		if !*follow || info.State != engine.StateRunning {
			return 0
		}
		f, err = ctrWaitLog(ctx, filepath.Join(rt.ContainerDir(info.ID), name), func() bool {
			now, err := rt.Get(info.ID)
			return err == nil && now.State == engine.StateRunning
		})
		if f == nil {
			return 0
		}
	}
	if err != nil {
		return ctrFail(err)
	}
	defer f.Close()
	switch {
	case lines == 0:
		_, err = f.Seek(0, io.SeekEnd)
	case lines > 0:
		err = ctrTail(f, lines)
	}
	if err != nil {
		return ctrFail(err)
	}
	// The main process's output is shown raw, as by docker logs; the serial console is guest text.
	var out io.Writer = os.Stdout
	if *console {
		out = ctrSanitizer{os.Stdout}
	}
	if _, err := io.Copy(out, f); err != nil {
		return ctrFail(err)
	}
	for *follow {
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(200 * time.Millisecond):
		}
		n, _ := io.Copy(out, f)
		if n > 0 {
			continue
		}
		if now, err := rt.Get(info.ID); err != nil || now.State != engine.StateRunning {
			io.Copy(out, f)
			return 0
		}
	}
	return 0
}

// ctrWaitLog waits for a log file to appear while running reports true.
func ctrWaitLog(ctx context.Context, path string, running func() bool) (*os.File, error) {
	for running() {
		if f, err := os.Open(path); err == nil {
			return f, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, nil
}

// ctrSanitizer escapes control characters on their way to w (see runtime.Sanitize).
type ctrSanitizer struct{ w io.Writer }

func (s ctrSanitizer) Write(p []byte) (int, error) {
	if _, err := io.WriteString(s.w, runtime.Sanitize(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
