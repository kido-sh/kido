package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/kido-sh/kido/internal/engine"
)

// exportScript tars the root filesystem without descending into other mounts (devfs). Sockets and
// device nodes are reported on stderr instead: tar cannot archive a socket, and device nodes
// belong to the VM, not to the container's files.
const exportScript = `cd / || exit 125
find . -xdev -type s -print | while IFS= read -r f; do echo "kido-skip socket $f"; done >&2
find . -xdev \( -type c -o -type b \) -print | while IFS= read -r f; do echo "kido-skip device $f"; done >&2
find . -xdev ! -type s ! -type c ! -type b -print0 | tar -c -f - --no-recursion --null -T -`

// Export writes the container's root filesystem to w as a tar stream whose entry names are relative
// to /, the way docker export writes them. Each socket or device node left out is passed to skipped
// as "socket PATH" or "device PATH", unsanitized; calls never overlap.
func (r *Runtime) Export(ctx context.Context, ref string, w io.Writer, skipped func(string)) error {
	var mu sync.Mutex
	report := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		skipped(s)
	}
	stderr := &exportStderr{skipped: report, rest: &tailBuffer{max: 64 << 10}}
	pr, pw := io.Pipe()
	filtered := make(chan error, 1)
	go func() {
		err := exportFilter(pr, w, report)
		if err == nil {
			_, err = io.Copy(io.Discard, pr)
		}
		filtered <- err
		pr.CloseWithError(errors.New("export stopped"))
	}()
	p := engine.ProcessSpec{Args: []string{"/bin/sh", "-c", exportScript}, Env: []string{"PATH=/bin:/usr/bin:/sbin:/usr/sbin"}}
	status, err := r.Exec(ctx, ref, p, engine.Stdio{Stdout: pw, Stderr: stderr})
	stderr.flush()
	if err == nil && status != 0 {
		msg := Sanitize(strings.TrimSpace(stderr.rest.String()))
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", status)
		}
		err = fmt.Errorf("export: %s", msg)
	}
	select {
	case ferr := <-filtered:
		// The filter gave up while the guest was still writing (w failed): that is the cause.
		pw.Close()
		return ferr
	default:
	}
	pw.CloseWithError(err)
	ferr := <-filtered
	if err != nil {
		return err
	}
	return ferr
}

// exportFilter re-encodes the guest's tar stream: "./" goes from the names, and device nodes and
// names that would leave the archive's root are dropped.
func exportFilter(in io.Reader, out io.Writer, skipped func(string)) error {
	tr := tar.NewReader(in)
	tw := tar.NewWriter(out)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return tw.Close()
		}
		if err != nil {
			return err
		}
		hdr.Name = strings.TrimPrefix(hdr.Name, "./")
		if hdr.Name == "" || hdr.Name == "." {
			continue
		}
		if hdr.Typeflag == tar.TypeChar || hdr.Typeflag == tar.TypeBlock {
			skipped("device ./" + hdr.Name)
			continue
		}
		if hdr.Typeflag == tar.TypeLink {
			hdr.Linkname = strings.TrimPrefix(hdr.Linkname, "./")
		}
		if !SafeEntryName(hdr.Name) || hdr.Typeflag == tar.TypeLink && !SafeEntryName(hdr.Linkname) {
			skipped("unsafe entry " + hdr.Name)
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

// exportStderr passes the script's "kido-skip" lines to skipped and keeps the tail of the rest.
type exportStderr struct {
	skipped func(string)
	rest    *tailBuffer
	line    []byte
}

func (e *exportStderr) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if room := 4096 - len(e.line); room > 0 {
			e.line = append(e.line, chunk[:min(len(chunk), room)]...)
		}
		if i < 0 {
			break
		}
		e.emit()
		p = p[i+1:]
	}
	return n, nil
}

func (e *exportStderr) emit() {
	if s, ok := strings.CutPrefix(string(e.line), "kido-skip "); ok {
		e.skipped(s)
	} else {
		e.rest.Write(append(e.line, '\n'))
	}
	e.line = e.line[:0]
}

func (e *exportStderr) flush() {
	if len(e.line) > 0 {
		e.emit()
	}
}
