package setup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
)

// Limits for unpacking a QEMU archive. The pinned release unpacks to 722 MB, of which kido keeps
// about 50 MB.
const (
	maxStreamBytes = 4 << 30   // the decompressed tar stream, skipped entries included
	maxFileBytes   = 512 << 20 // one kept file
	maxKeptBytes   = 1 << 30   // all kept files
	maxEntries     = 50000
)

const darwinDecompressor = "/usr/bin/gzip"

func decompressorFor(archive string, header []byte) string {
	return decompressorForOS(runtime.GOOS, archive, header)
}

func decompressorForOS(goos, archive string, header []byte) string {
	if goos == "darwin" {
		return darwinDecompressor
	}
	switch {
	case bytes.HasPrefix(header, []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}):
		return "xz"
	case bytes.HasPrefix(header, []byte{0x1f, 0x8b}):
		return "gzip"
	case bytes.HasPrefix(header, []byte{'B', 'Z', 'h'}):
		return "bzip2"
	}
	lower := strings.ToLower(archive)
	switch {
	case strings.HasSuffix(lower, ".xz"):
		return "xz"
	case strings.HasSuffix(lower, ".bz2"):
		return "bzip2"
	default:
		return "gzip"
	}
}

type unpackStats struct {
	Files   int
	Bytes   int64
	Skipped int
}

// unpack decompresses a .tar.xz (or .tar.gz, .tar.bz2) archive and extracts the parts of a QEMU
// install that kido uses into the empty directory dir. A leading usr/local/ is stripped.
func unpack(ctx context.Context, archive, dir string) (unpackStats, error) {
	f, err := os.Open(archive)
	if err != nil {
		return unpackStats{}, err
	}
	defer f.Close()
	var hdr [6]byte
	n, _ := io.ReadFull(f, hdr[:])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return unpackStats{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, decompressorFor(archive, hdr[:n]), "-dc")
	cmd.Stdin = f
	stderr := &tailBuffer{max: 4096}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return unpackStats{}, err
	}
	if err := cmd.Start(); err != nil {
		return unpackStats{}, err
	}
	stream := &capReader{r: out, left: maxStreamBytes}
	stats, err := extractTar(stream, dir)
	if err == nil {
		_, err = io.Copy(io.Discard, stream)
	}
	if err != nil {
		cancel()
		cmd.Wait()
		return stats, err
	}
	if err := cmd.Wait(); err != nil {
		return stats, fmt.Errorf("decompressing %s: %v: %s", archive, err, strings.TrimSpace(stderr.String()))
	}
	return stats, nil
}

// extractTar writes the kept entries of a tar stream under dir through an os.Root, so no entry
// lands outside dir. Files are 0600 (0700 when executable) and never replace an existing entry.
func extractTar(r io.Reader, dir string) (unpackStats, error) {
	var stats unpackStats
	root, err := os.OpenRoot(dir)
	if err != nil {
		return stats, err
	}
	defer root.Close()
	tr := tar.NewReader(r)
	for entries := 0; ; entries++ {
		if entries >= maxEntries {
			return stats, fmt.Errorf("archive has more than %d entries", maxEntries)
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return stats, nil
		}
		if err != nil {
			return stats, fmt.Errorf("reading the archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name, err := archiveName(hdr.Name)
		if err != nil {
			return stats, err
		}
		if name == "" || !keep(name) {
			stats.Skipped++
			continue
		}
		if parent := path.Dir(name); parent != "." {
			if err := root.MkdirAll(parent, 0o700); err != nil {
				return stats, err
			}
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return stats, err
			}
		case tar.TypeReg, '\x00':
			if hdr.Size > maxFileBytes {
				return stats, fmt.Errorf("%s: %d bytes is over the %d MiB limit for one file", name, hdr.Size, maxFileBytes>>20)
			}
			if stats.Bytes+hdr.Size > maxKeptBytes {
				return stats, fmt.Errorf("archive unpacks to more than %d MiB", maxKeptBytes>>20)
			}
			n, err := writeFile(root, name, tr, hdr.Mode&0o111 != 0)
			if err != nil {
				return stats, err
			}
			stats.Files++
			stats.Bytes += n
		case tar.TypeSymlink:
			if err := checkLinkTarget(name, hdr.Linkname); err != nil {
				return stats, err
			}
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return stats, err
			}
		default:
			return stats, fmt.Errorf("%s: unsupported archive entry type %q", name, hdr.Typeflag)
		}
	}
}

func writeFile(root *os.Root, name string, r io.Reader, executable bool) (int64, error) {
	mode := os.FileMode(0o600)
	if executable {
		mode = 0o700
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

// archiveName turns a tar entry name into a clean relative path without the usr/local/ prefix,
// "" for the prefix directories themselves, or an error for absolute paths and ".." elements.
func archiveName(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive entry %q is not a relative path", name)
	}
	for _, elem := range strings.Split(name, "/") {
		if elem == ".." {
			return "", fmt.Errorf("archive entry %q leaves the archive", name)
		}
	}
	clean := path.Clean(name)
	switch clean {
	case ".", "usr", "usr/local":
		return "", nil
	}
	return strings.TrimPrefix(clean, "usr/local/"), nil
}

// keep reports whether kido needs an archive path: the two programs it runs, the libraries next to
// them, and QEMU's data files without the UEFI firmware (kido boots the kernel directly).
func keep(name string) bool {
	switch {
	case name == "bin/"+qemuName, name == "bin/"+qemuImgName:
		return true
	case strings.HasPrefix(name, "lib/"):
		return true
	case strings.HasPrefix(name, "share/qemu/"):
		return !strings.HasPrefix(path.Base(name), "edk2-")
	}
	return false
}

// checkLinkTarget allows only relative symlink targets without "..": each link then resolves
// inside its own directory, however links chain.
func checkLinkTarget(name, target string) error {
	if target == "" || strings.HasPrefix(target, "/") {
		return fmt.Errorf("symlink %s -> %q: only relative targets are allowed", name, target)
	}
	for _, elem := range strings.Split(target, "/") {
		if elem == ".." {
			return fmt.Errorf("symlink %s -> %q: targets may not contain ..", name, target)
		}
	}
	return nil
}

// capReader fails once more than left bytes were read, instead of silently truncating.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, fmt.Errorf("archive is larger than %d GiB uncompressed", maxStreamBytes>>30)
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
