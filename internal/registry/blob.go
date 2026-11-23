package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/term"
)

const (
	sparseBlock = 4096
	copyChunk   = 1 << 20
	// minFreeSpace is what a pull leaves free at least, whatever sizes a registry declares.
	minFreeSpace = 1 << 30
	freeCheckGap = 256 << 20
)

// maxDecoderWindow bounds decoder memory; kido's own encoder uses a 4 MiB window.
const maxDecoderWindow = 128 << 20

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// compressFile writes in zstd-compressed to a new temp file in dir, hashing the compressed bytes
// on the way, and returns the temp file's descriptor (digest, size) and the input size. The
// encoder compresses large sections on several goroutines and keeps memory bounded.
func compressFile(ctx context.Context, in io.Reader, dir string, p *progress) (path string, d descriptor, rawSize int64, err error) {
	out, err := os.CreateTemp(dir, "push-*.zst")
	if err != nil {
		return "", d, 0, err
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(out.Name())
		}
	}()
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(out, h)}
	// Four 16 MiB jobs at a time: on the 1.1 GB base disk 0.3 s and 265 MiB RSS, against 0.9 s
	// and 25 MiB single-threaded and 0.4 s and 676 MiB with eight jobs of 32 MiB.
	enc, err := zstd.NewWriter(cw,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithWindowSize(4<<20),
		zstd.WithEncoderConcurrency(min(runtime.GOMAXPROCS(0), 4)),
		zstd.WithConcurrentBlocks(true))
	if err != nil {
		return "", d, 0, err
	}
	rawSize, err = enc.ReadFrom(p.reader(ctxReader{ctx, in}))
	if cerr := enc.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", d, 0, err
	}
	if err = out.Close(); err != nil {
		return "", d, 0, err
	}
	d = descriptor{Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)), Size: cw.n}
	return out.Name(), d, rawSize, nil
}

func hashFile(ctx context.Context, f io.Reader) (descriptor, error) {
	h := sha256.New()
	n, err := io.Copy(h, ctxReader{ctx, f})
	if err != nil {
		return descriptor{}, err
	}
	return descriptor{Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// decompressFile expands a verified zstd blob into a new file at dst, which must come out at
// exactly size bytes. All-zero blocks become holes, so a sparse disk stays sparse.
func decompressFile(ctx context.Context, src, dst string, size int64, p *progress) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dec, err := zstd.NewReader(in, zstd.WithDecoderMaxWindow(maxDecoderWindow))
	if err != nil {
		return err
	}
	defer dec.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := writeSparse(out, p.reader(ctxReader{ctx, dec}), size)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s: decompressed to %d bytes, manifest says %d", dst, n, size)
	}
	return nil
}

var zeroBlock [sparseBlock]byte

// writeSparse copies r into the empty file f, seeking over all-zero blocks instead of writing
// them, and refuses to produce more than limit bytes. APFS fills holes shorter than about 16 MiB
// with zeros when it writes the data around them back, so every freeCheckGap bytes the zero runs
// so far are punched out again and the free space is checked against what is really allocated.
func writeSparse(f *os.File, r io.Reader, limit int64) (int64, error) {
	buf := make([]byte, copyChunk)
	var off, settled int64
	var zeros []extent
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if off+int64(n) > limit {
				return off, fmt.Errorf("data exceeds the declared %d bytes", limit)
			}
			if werr := writeNonZero(f, buf[:n], off, &zeros); werr != nil {
				return off, werr
			}
			off += int64(n)
			if off-settled >= freeCheckGap {
				if err := settle(f, &zeros, off); err != nil {
					return off, err
				}
				settled = off
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return off, err
		}
	}
	if err := f.Truncate(off); err != nil {
		return off, err
	}
	return off, settle(f, &zeros, off)
}

// settle writes the file back, punches the zero runs recorded so far below end and checks that
// the disk keeps minFreeSpace. A run reaching end stays recorded: the next data written after it
// can make APFS fill it again.
func settle(f *os.File, zeros *[]extent, end int64) error {
	if len(*zeros) > 0 {
		if err := syscall.Fsync(int(f.Fd())); err != nil {
			return err
		}
		for _, z := range *zeros {
			start, stop := z.off, min(z.off+z.n, end)
			stop -= stop % sparseBlock
			// Holes only save space: a file system without F_PUNCHHOLE keeps the zeros.
			if stop > start && punchHole(f, start, stop-start) != nil {
				break
			}
		}
		last := (*zeros)[len(*zeros)-1]
		*zeros = (*zeros)[:0]
		if last.off+last.n >= end {
			*zeros = append(*zeros, last)
		}
	}
	return checkFreeSpace(f, 0)
}

type extent struct{ off, n int64 }

// writeNonZero writes b at off, one write per run of blocks that are not all zero, and appends
// the zero runs to zeros. off is a multiple of copyChunk, so the blocks line up with the file
// system's.
func writeNonZero(f *os.File, b []byte, off int64, zeros *[]extent) error {
	block := func(i int) []byte { return b[i:min(i+sparseBlock, len(b))] }
	isZero := func(i int) bool { blk := block(i); return bytes.Equal(blk, zeroBlock[:len(blk)]) }
	for i := 0; i < len(b); {
		zero := isZero(i)
		j := i
		for j < len(b) && isZero(j) == zero {
			j += len(block(j))
		}
		if !zero {
			if _, err := f.WriteAt(b[i:j], off+int64(i)); err != nil {
				return err
			}
		} else if z := *zeros; len(z) > 0 && z[len(z)-1].off+z[len(z)-1].n == off+int64(i) {
			z[len(z)-1].n += int64(j - i)
		} else {
			*zeros = append(*zeros, extent{off + int64(i), int64(j - i)})
		}
		i = j
	}
	return nil
}

// checkFreeSpace fails when need more bytes on f's file system would leave less than
// minFreeSpace, so sizes a registry declares cannot fill the disk.
func checkFreeSpace(f *os.File, need int64) error {
	var st syscall.Statfs_t
	if err := syscall.Fstatfs(int(f.Fd()), &st); err != nil {
		return err
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	if free-need < minFreeSpace {
		return fmt.Errorf("not enough free space: %s free, %s more needed, %s must stay free", humanBytes(free), humanBytes(need), humanBytes(minFreeSpace))
	}
	return nil
}

// progress prints "label: bytes [/ total] (rate)" lines while a transfer runs: refreshed in
// place on a terminal, one line every few seconds otherwise.
type progress struct {
	w     io.Writer
	label string
	verb  string
	total int64
	n     atomic.Int64
	start atomic.Int64
	tty   bool
	stop  chan struct{}
	done  chan struct{}
}

func newProgress(w io.Writer, label, verb string, total int64) *progress {
	p := &progress{w: w, label: label, verb: verb, total: total}
	p.start.Store(time.Now().UnixNano())
	if w == nil {
		return p
	}
	if f, ok := w.(*os.File); ok {
		p.tty = term.IsTerminal(int(f.Fd()))
	}
	p.stop, p.done = make(chan struct{}), make(chan struct{})
	go p.loop()
	return p
}

func (p *progress) loop() {
	defer close(p.done)
	every := 5 * time.Second
	if p.tty {
		every = 500 * time.Millisecond
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			s := fmt.Sprintf("%s: %s %s", p.label, p.verb, humanBytes(p.n.Load()))
			if p.total > 0 {
				s += " / " + humanBytes(p.total)
			}
			s += " (" + p.rate() + ")"
			if p.tty {
				fmt.Fprintf(p.w, "\r\x1b[K%s", s)
			} else {
				fmt.Fprintln(p.w, s)
			}
		}
	}
}

func (p *progress) elapsed() time.Duration {
	return time.Duration(time.Now().UnixNano() - p.start.Load())
}

func (p *progress) rate() string {
	el := max(p.elapsed().Seconds(), 1e-3)
	return humanBytes(int64(float64(p.n.Load())/el)) + "/s"
}

func (p *progress) reset() {
	p.n.Store(0)
	p.start.Store(time.Now().UnixNano())
}

// finish stops the ticker and prints "label: <what> in <time> (<rate>)".
func (p *progress) finish(what string) {
	if p.w == nil {
		return
	}
	close(p.stop)
	<-p.done
	if p.tty {
		fmt.Fprint(p.w, "\r\x1b[K")
	}
	fmt.Fprintf(p.w, "%s: %s in %.1fs (%s)\n", p.label, what, p.elapsed().Seconds(), p.rate())
}

func (p *progress) reader(r io.Reader) io.Reader { return &progressReader{r: r, p: p} }

type progressReader struct {
	r io.Reader
	p *progress
}

func (pr *progressReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	pr.p.n.Add(int64(n))
	return n, err
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
