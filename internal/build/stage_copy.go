package build

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
	"github.com/kido-sh/kido/internal/runtime"
)

const (
	findStageScript     = `cd -- "$1" 2>/dev/null || exit 0; find . -maxdepth "$2" -print0`
	realpathStageScript = `cd -- "$1" && /bin/pwd -P`
	maxFindOutput       = 16 << 20
	maxRealpathOutput   = 64 << 10
	maxStageTarEntries  = 250_000
	maxStageTarBytes    = 16 << 30
)

var errTarRejected = errors.New("unsafe tar entry")

func tarReject(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errTarRejected}, args...)...)
}

type stagedSource struct {
	base      string
	isDir     bool
	isSymlink bool
	mode      int64
	count     int
	tarPath   string
}

type stagedCopy struct {
	sources []stagedSource
	err     error
}

type tarMeta struct {
	isDir     bool
	isSymlink bool
	mode      int64
	count     int
}

type replayItem struct {
	src    stagedSource
	rename string
}

type cappedBuffer struct {
	max    int
	buf    bytes.Buffer
	over   bool
	cancel context.CancelFunc
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.over || c.buf.Len()+len(p) > c.max {
		c.over = true
		if c.cancel != nil {
			c.cancel()
		}
		return 0, fmt.Errorf("guest output exceeded %d bytes", c.max)
	}
	return c.buf.Write(p)
}

func (b *builder) ensureStageDir() (string, error) {
	if b.stageDir != "" {
		return b.stageDir, nil
	}
	dir, err := os.MkdirTemp("", "kido-build-*")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	b.stageDir = dir
	return dir, nil
}

func (b *builder) extractTasksFromContainer(ctx context.Context, containerID string, tasks []copyTask) error {
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if task.prepErr != nil {
			b.stagedCopies[task.key] = stagedCopy{err: task.prepErr}
			continue
		}
		sc := b.extractCopyTask(ctx, containerID, task)
		if err := ctx.Err(); err != nil {
			return err
		}
		b.stagedCopies[task.key] = sc
	}
	return nil
}

func (b *builder) extractExternalImagesForStage(ctx context.Context, st *stage) error {
	var needed []imageref.Ref
	seen := map[imageref.Ref]bool{}
	for j := range st.steps {
		cref, ok := b.copyFromRefs[copyKey{st.index, j}]
		if !ok || cref.kind != refImage || b.extractedImages[cref.image] || seen[cref.image] {
			continue
		}
		seen[cref.image] = true
		needed = append(needed, cref.image)
	}
	for _, ref := range needed {
		if err := b.extractFromExternalImage(ctx, ref, b.imageCopies[ref]); err != nil {
			return err
		}
		b.extractedImages[ref] = true
	}
	return nil
}

func (b *builder) extractFromExternalImage(ctx context.Context, ref imageref.Ref, tasks []copyTask) error {
	c, err := b.eng.Create(ctx, engine.CreateOptions{
		Image: ref.String(), CPUs: b.opts.CPUs, MemoryMB: b.opts.MemoryMB,
		Labels: map[string]string{BuildLabel: b.target.String()},
	})
	var tmpID string
	if c != nil {
		tmpID = c.ID
		defer func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if rerr := b.eng.Remove(cctx, tmpID, true); rerr != nil {
				fmt.Fprintf(b.errw, "kido build: removing stage copy container %s: %v\n", tmpID, rerr)
			}
		}()
	}
	if err != nil {
		return fmt.Errorf("copy from %s: %w", ref, err)
	}
	return b.extractTasksFromContainer(ctx, tmpID, tasks)
}

func (b *builder) extractCopyTask(ctx context.Context, containerID string, task copyTask) stagedCopy {
	var sources []stagedSource
	cleanupSources := func() {
		for _, s := range sources {
			os.Remove(s.tarPath)
		}
	}
	for _, rawSrc := range task.srcs {
		rel, trailingSlash, err := cleanStageSource(rawSrc)
		if err != nil {
			cleanupSources()
			return stagedCopy{err: err}
		}
		if hasMeta(rel) {
			matches, err := b.listStageMatches(ctx, containerID, rel)
			if err != nil {
				cleanupSources()
				return stagedCopy{err: fmt.Errorf("%s: %w", rawSrc, err)}
			}
			for _, m := range matches {
				if trailingSlash {
					ok, err := b.isDirIn(ctx, containerID, "/"+m)
					if err != nil {
						cleanupSources()
						return stagedCopy{err: fmt.Errorf("%s: %w", m, err)}
					}
					if !ok {
						continue
					}
				}
				src, err := b.extractStageSource(ctx, containerID, m, trailingSlash)
				if err != nil {
					cleanupSources()
					return stagedCopy{err: fmt.Errorf("%s: %w", m, err)}
				}
				sources = append(sources, src)
			}
			continue
		}
		src, err := b.extractStageSource(ctx, containerID, rel, trailingSlash)
		if err != nil {
			cleanupSources()
			return stagedCopy{err: fmt.Errorf("%s: %w", rawSrc, err)}
		}
		sources = append(sources, src)
	}
	if len(sources) == 0 {
		return stagedCopy{err: fmt.Errorf("no source files were specified: nothing in %s matches %s", task.from.raw, strings.Join(task.srcs, " "))}
	}
	return stagedCopy{sources: sources}
}

func (b *builder) listStageMatches(ctx context.Context, containerID, pattern string) ([]string, error) {
	if _, err := path.Match(pattern, ""); err != nil {
		return nil, err
	}
	segs := strings.Split(pattern, "/")
	prefixEnd := 0
	for prefixEnd < len(segs) && !hasMeta(segs[prefixEnd]) {
		prefixEnd++
	}
	prefix := ""
	searchDir := "/"
	if prefixEnd > 0 {
		prefix = strings.Join(segs[:prefixEnd], "/")
		searchDir = "/" + prefix
	}
	depth := len(segs) - prefixEnd
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &cappedBuffer{max: maxFindOutput, cancel: cancel}
	stderr := &tailBuffer{max: 64 << 10}
	status, err := b.eng.Exec(childCtx, containerID, engine.ProcessSpec{
		Args: []string{"/bin/sh", "-c", findStageScript, "sh", searchDir, strconv.Itoa(depth)},
		Env:  []string{"PATH=/bin:/usr/bin:/sbin:/usr/sbin"},
	}, engine.Stdio{Stdout: out, Stderr: stderr})
	if out.over && ctx.Err() == nil {
		return nil, errors.New("too many matching entries in source stage")
	}
	if err != nil {
		return nil, err
	}
	if status != 0 && out.buf.Len() == 0 {
		msg := runtime.Sanitize(strings.TrimSpace(stderr.String()))
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", status)
		}
		return nil, fmt.Errorf("find: %s", msg)
	}

	var matches []string
	seen := map[string]bool{}
	for _, raw := range bytes.Split(out.buf.Bytes(), []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		s := string(raw)
		if strings.ContainsRune(s, 0) || path.IsAbs(s) {
			return nil, fmt.Errorf("invalid path %q from source stage", s)
		}
		hasDotDot := false
		for _, el := range strings.Split(s, "/") {
			if el == ".." {
				hasDotDot = true
				break
			}
		}
		if hasDotDot {
			return nil, fmt.Errorf("invalid path %q from source stage", s)
		}
		c := path.Clean(s)
		if c == "." || c == "" || c == ".." || strings.HasPrefix(c, "../") {
			continue
		}
		cand := c
		if prefix != "" {
			cand = prefix + "/" + c
		}
		if ok, _ := path.Match(pattern, cand); ok && !seen[cand] {
			seen[cand] = true
			matches = append(matches, cand)
		}
	}
	slices.Sort(matches)
	return matches, nil
}

func (b *builder) resolveGuestDir(ctx context.Context, containerID, guestPath string) (string, error) {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &cappedBuffer{max: maxRealpathOutput, cancel: cancel}
	stderr := &tailBuffer{max: 64 << 10}
	status, err := b.eng.Exec(childCtx, containerID, engine.ProcessSpec{
		Args: []string{"/bin/sh", "-c", realpathStageScript, "sh", guestPath},
		Env:  []string{"PATH=/bin:/usr/bin:/sbin:/usr/sbin"},
	}, engine.Stdio{Stdout: out, Stderr: stderr})
	if out.over && ctx.Err() == nil {
		return "", errors.New("resolved path from source stage is too long")
	}
	if err != nil {
		return "", err
	}
	if status != 0 {
		msg := runtime.Sanitize(strings.TrimSpace(stderr.String()))
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", status)
		}
		return "", fmt.Errorf("not a directory: %s", msg)
	}
	real := strings.TrimSuffix(out.buf.String(), "\n")
	return runtime.GuestPath(real)
}

func (b *builder) extractStageSource(ctx context.Context, containerID, rel string, trailingSlash bool) (stagedSource, error) {
	guestPath := "/"
	expectedTop := ""
	base := "."
	if rel != "." {
		guestPath = "/" + rel
		base = path.Base(rel)
		expectedTop = base
	}
	src, err := b.copyOutToTempTar(ctx, containerID, guestPath, expectedTop, base, rel == ".")
	if err != nil {
		return stagedSource{}, err
	}
	if trailingSlash && src.isSymlink {
		os.Remove(src.tarPath)
		realPath, err := b.resolveGuestDir(ctx, containerID, guestPath)
		if err != nil {
			return stagedSource{}, err
		}
		realTop := ""
		if realPath != "/" {
			realTop = path.Base(realPath)
		}
		src, err = b.copyOutToTempTar(ctx, containerID, realPath, realTop, base, realPath == "/")
		if err != nil {
			return stagedSource{}, err
		}
	}
	if trailingSlash && !src.isDir {
		os.Remove(src.tarPath)
		return stagedSource{}, errors.New("not a directory")
	}
	return src, nil
}

func (b *builder) copyOutToTempTar(ctx context.Context, containerID, guestPath, expectedTop, base string, skipSpecial bool) (stagedSource, error) {
	dir, err := b.ensureStageDir()
	if err != nil {
		return stagedSource{}, err
	}
	f, err := os.CreateTemp(dir, "copy-*.tar")
	if err != nil {
		return stagedSource{}, err
	}
	tmpPath := f.Name()
	keep := false
	defer func() {
		f.Close()
		if !keep {
			os.Remove(tmpPath)
		}
	}()

	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	copyDone := make(chan error, 1)
	go func() {
		err := b.eng.CopyOut(childCtx, containerID, guestPath, pw)
		pw.CloseWithError(err)
		copyDone <- err
	}()
	meta, serr := sanitizeExtractedTar(pr, f, expectedTop, skipSpecial)
	if serr != nil {
		cancel()
	}
	pr.CloseWithError(errCopyEnd)
	cerr := <-copyDone

	if errors.Is(serr, errTarRejected) {
		return stagedSource{}, serr
	}
	if ctx.Err() != nil {
		return stagedSource{}, ctx.Err()
	}
	if cerr != nil && !errors.Is(cerr, errCopyEnd) && !errors.Is(cerr, context.Canceled) {
		return stagedSource{}, cerr
	}
	if serr != nil {
		return stagedSource{}, serr
	}
	if cerr != nil && !errors.Is(cerr, errCopyEnd) {
		return stagedSource{}, cerr
	}
	if err := f.Close(); err != nil {
		return stagedSource{}, err
	}
	keep = true
	return stagedSource{
		base:      base,
		isDir:     meta.isDir,
		isSymlink: meta.isSymlink,
		mode:      meta.mode,
		count:     meta.count,
		tarPath:   tmpPath,
	}, nil
}

func cleanTarPath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) || path.IsAbs(name) {
		return "", tarReject("refusing tar entry %q", name)
	}
	for _, el := range strings.Split(name, "/") {
		if el == ".." {
			return "", tarReject("refusing tar entry %q", name)
		}
	}
	c := path.Clean(name)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", tarReject("refusing tar entry %q", name)
	}
	return c, nil
}

func sanitizedMode(hdr *tar.Header) int64 {
	mode := hdr.Mode & 0o7777
	if hdr.Uid != 0 {
		mode &^= 0o4000
	}
	if hdr.Gid != 0 {
		mode &^= 0o2000
	}
	return mode
}

func sanitizeExtractedTar(r io.Reader, w io.Writer, expectedTop string, skipSpecial bool) (tarMeta, error) {
	return sanitizeExtractedTarLimits(r, w, expectedTop, skipSpecial, maxStageTarEntries, maxStageTarBytes)
}

func sanitizeExtractedTarLimits(r io.Reader, w io.Writer, expectedTop string, skipSpecial bool, maxEntries int, maxBytes int64) (tarMeta, error) {
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	var meta tarMeta
	seenRoot := false
	singleFinished := false
	totalEntries := 0
	var totalBytes int64
	symlinks := map[string]bool{}
	dirs := map[string]bool{}
	regulars := map[string]bool{}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return tarMeta{}, err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		totalEntries++
		if totalEntries > maxEntries {
			return tarMeta{}, tarReject("archive exceeds limit of %d entries", maxEntries)
		}
		if strings.ContainsRune(hdr.Linkname, 0) {
			return tarMeta{}, tarReject("refusing tar entry %q with NUL link target", hdr.Name)
		}
		clean, err := cleanTarPath(hdr.Name)
		if err != nil {
			return tarMeta{}, err
		}

		var child string
		if expectedTop == "" {
			if clean == "." {
				if hdr.Typeflag != tar.TypeDir {
					return tarMeta{}, tarReject("refusing non-directory root entry %q", hdr.Name)
				}
				if hdr.Size != 0 {
					return tarMeta{}, tarReject("invalid size %d for directory %q", hdr.Size, hdr.Name)
				}
				seenRoot = true
				meta.isDir = true
				meta.mode = sanitizedMode(hdr)
				continue
			}
			meta.isDir = true
			child = clean
		} else {
			if clean == expectedTop {
				if seenRoot || meta.count > 0 {
					return tarMeta{}, tarReject("duplicate root tar entry %q", hdr.Name)
				}
				seenRoot = true
				switch hdr.Typeflag {
				case tar.TypeDir:
					if hdr.Size != 0 {
						return tarMeta{}, tarReject("invalid size %d for directory %q", hdr.Size, hdr.Name)
					}
					meta.isDir = true
					meta.mode = sanitizedMode(hdr)
				case tar.TypeReg, tar.TypeRegA:
					if hdr.Size < 0 {
						return tarMeta{}, tarReject("invalid size %d for file %q", hdr.Size, hdr.Name)
					}
					if hdr.Size > maxBytes-totalBytes {
						return tarMeta{}, tarReject("archive exceeds limit of %d bytes", maxBytes)
					}
					totalBytes += hdr.Size
					meta.mode = sanitizedMode(hdr)
					outHdr := &tar.Header{
						Typeflag: tar.TypeReg,
						Name:     "file",
						Size:     hdr.Size,
						Mode:     meta.mode,
						ModTime:  hdr.ModTime,
					}
					if err := tw.WriteHeader(outHdr); err != nil {
						return tarMeta{}, err
					}
					if _, err := io.CopyN(tw, tr, hdr.Size); err != nil {
						return tarMeta{}, err
					}
					meta.count = 1
					singleFinished = true
				case tar.TypeSymlink:
					if hdr.Size != 0 {
						return tarMeta{}, tarReject("invalid size %d for symlink %q", hdr.Size, hdr.Name)
					}
					if hdr.Linkname == "" {
						return tarMeta{}, tarReject("empty symlink target for %q", hdr.Name)
					}
					meta.isSymlink = true
					meta.mode = 0o777
					outHdr := &tar.Header{
						Typeflag: tar.TypeSymlink,
						Name:     "file",
						Linkname: hdr.Linkname,
						Mode:     0o777,
						ModTime:  hdr.ModTime,
					}
					if err := tw.WriteHeader(outHdr); err != nil {
						return tarMeta{}, err
					}
					meta.count = 1
					singleFinished = true
				default:
					return tarMeta{}, tarReject("unsupported file type %q for %q", hdr.Typeflag, hdr.Name)
				}
				continue
			}
			prefix := expectedTop + "/"
			if !strings.HasPrefix(clean, prefix) {
				return tarMeta{}, tarReject("refusing tar entry %q outside %q", hdr.Name, expectedTop)
			}
			if singleFinished || (seenRoot && !meta.isDir) {
				return tarMeta{}, tarReject("unexpected tar entry %q after single file", hdr.Name)
			}
			meta.isDir = true
			child = strings.TrimPrefix(clean, prefix)
		}

		if !runtime.SafeEntryName(child) {
			return tarMeta{}, tarReject("refusing tar entry %q", hdr.Name)
		}
		for i := 0; i < len(child); i++ {
			if child[i] == '/' {
				parent := child[:i]
				if symlinks[parent] {
					return tarMeta{}, tarReject("refusing tar entry %q: parent %q is a symlink", hdr.Name, parent)
				}
				dirs[parent] = true
			}
		}
		if symlinks[child] {
			return tarMeta{}, tarReject("refusing tar entry %q: path is already a symlink", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if hdr.Size != 0 {
				return tarMeta{}, tarReject("invalid size %d for directory %q", hdr.Size, hdr.Name)
			}
			dirs[child] = true
			outHdr := &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     child + "/",
				Mode:     sanitizedMode(hdr),
				ModTime:  hdr.ModTime,
			}
			if err := tw.WriteHeader(outHdr); err != nil {
				return tarMeta{}, err
			}
			meta.count++
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 {
				return tarMeta{}, tarReject("invalid size %d for file %q", hdr.Size, hdr.Name)
			}
			if hdr.Size > maxBytes-totalBytes {
				return tarMeta{}, tarReject("archive exceeds limit of %d bytes", maxBytes)
			}
			totalBytes += hdr.Size
			regulars[child] = true
			outHdr := &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     child,
				Size:     hdr.Size,
				Mode:     sanitizedMode(hdr),
				ModTime:  hdr.ModTime,
			}
			if err := tw.WriteHeader(outHdr); err != nil {
				return tarMeta{}, err
			}
			if _, err := io.CopyN(tw, tr, hdr.Size); err != nil {
				return tarMeta{}, err
			}
			meta.count++
		case tar.TypeSymlink:
			if hdr.Size != 0 {
				return tarMeta{}, tarReject("invalid size %d for symlink %q", hdr.Size, hdr.Name)
			}
			if hdr.Linkname == "" {
				return tarMeta{}, tarReject("empty symlink target for %q", hdr.Name)
			}
			if dirs[child] || regulars[child] {
				return tarMeta{}, tarReject("refusing symlink %q: path already exists in archive", hdr.Name)
			}
			symlinks[child] = true
			outHdr := &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     child,
				Linkname: hdr.Linkname,
				Mode:     0o777,
				ModTime:  hdr.ModTime,
			}
			if err := tw.WriteHeader(outHdr); err != nil {
				return tarMeta{}, err
			}
			meta.count++
		case tar.TypeLink:
			if hdr.Size != 0 {
				return tarMeta{}, tarReject("invalid size %d for hard link %q", hdr.Size, hdr.Name)
			}
			linkClean, err := cleanTarPath(hdr.Linkname)
			if err != nil {
				return tarMeta{}, tarReject("refusing hard link %q -> %q", hdr.Name, hdr.Linkname)
			}
			linkTarget := linkClean
			if expectedTop != "" {
				prefix := expectedTop + "/"
				if !strings.HasPrefix(linkClean, prefix) {
					return tarMeta{}, tarReject("refusing hard link %q -> %q", hdr.Name, hdr.Linkname)
				}
				linkTarget = strings.TrimPrefix(linkClean, prefix)
			}
			if !runtime.SafeEntryName(linkTarget) || !regulars[linkTarget] {
				return tarMeta{}, tarReject("refusing hard link %q -> %q", hdr.Name, hdr.Linkname)
			}
			regulars[child] = true
			outHdr := &tar.Header{
				Typeflag: tar.TypeLink,
				Name:     child,
				Linkname: linkTarget,
				Mode:     sanitizedMode(hdr),
				ModTime:  hdr.ModTime,
			}
			if err := tw.WriteHeader(outHdr); err != nil {
				return tarMeta{}, err
			}
			meta.count++
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			if skipSpecial {
				continue
			}
			return tarMeta{}, tarReject("unsupported file type %q for %q", hdr.Typeflag, hdr.Name)
		default:
			return tarMeta{}, tarReject("unsupported tar entry type %q for %q", hdr.Typeflag, hdr.Name)
		}
	}

	if !seenRoot && meta.count == 0 {
		return tarMeta{}, errors.New("empty archive from source stage")
	}
	if meta.isDir && meta.mode == 0 {
		meta.mode = 0o755
	}
	if err := tw.Close(); err != nil {
		return tarMeta{}, err
	}
	return meta, nil
}

type replayGuard struct {
	symlinks map[string]bool
	dirs     map[string]bool
	regulars map[string]bool
}

func newReplayGuard() *replayGuard {
	return &replayGuard{
		symlinks: map[string]bool{},
		dirs:     map[string]bool{},
		regulars: map[string]bool{},
	}
}

func (g *replayGuard) check(cleanName string, typeflag byte) error {
	for i := 0; i < len(cleanName); i++ {
		if cleanName[i] == '/' {
			parent := cleanName[:i]
			if g.symlinks[parent] {
				return tarReject("refusing tar entry %q: parent %q is a symlink", cleanName, parent)
			}
			g.dirs[parent] = true
		}
	}
	if g.symlinks[cleanName] {
		return tarReject("refusing tar entry %q: path is already a symlink", cleanName)
	}
	switch typeflag {
	case tar.TypeDir:
		g.dirs[cleanName] = true
	case tar.TypeReg, tar.TypeLink:
		g.regulars[cleanName] = true
	case tar.TypeSymlink:
		if g.dirs[cleanName] || g.regulars[cleanName] {
			return tarReject("refusing symlink %q: path already exists in archive", cleanName)
		}
		g.symlinks[cleanName] = true
	}
	return nil
}

func replayStagedTar(w io.Writer, items []replayItem, chmod int64, collectChown bool) ([]string, error) {
	tw := tar.NewWriter(w)
	guard := newReplayGuard()
	var chownPaths []string
	for _, item := range items {
		if item.src.count == 0 {
			continue
		}
		paths, err := replayOneTar(tw, guard, item, chmod, collectChown)
		if err != nil {
			return nil, err
		}
		chownPaths = append(chownPaths, paths...)
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return chownPaths, nil
}

func replayOneTar(tw *tar.Writer, guard *replayGuard, item replayItem, chmod int64, collectChown bool) ([]string, error) {
	f, err := os.Open(item.src.tarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var chownPaths []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return chownPaths, nil
		}
		if err != nil {
			return nil, err
		}
		name := hdr.Name
		if !item.src.isDir {
			name = item.rename
		}
		cleanName := strings.TrimSuffix(name, "/")
		if !runtime.SafeEntryName(cleanName) {
			return nil, tarReject("refusing tar entry %q", name)
		}
		if err := guard.check(cleanName, hdr.Typeflag); err != nil {
			return nil, err
		}
		outHdr := &tar.Header{
			Typeflag: hdr.Typeflag,
			Name:     name,
			Linkname: hdr.Linkname,
			Size:     hdr.Size,
			Mode:     hdr.Mode & 0o7777,
			ModTime:  hdr.ModTime,
		}
		if chmod >= 0 && (hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeDir) {
			outHdr.Mode = chmod
		}
		if err := tw.WriteHeader(outHdr); err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, tr, hdr.Size); err != nil {
				return nil, err
			}
		}
		if collectChown && hdr.Typeflag != tar.TypeSymlink {
			chownPaths = append(chownPaths, cleanName)
		}
	}
}

func (b *builder) copyFrom(ctx context.Context, s *step, key copyKey) error {
	sc, ok := b.stagedCopies[key]
	if !ok {
		return errors.New("internal error: staged copy not found")
	}
	defer func() {
		for _, src := range sc.sources {
			if src.tarPath != "" {
				os.Remove(src.tarPath)
			}
		}
		delete(b.stagedCopies, key)
	}()
	if sc.err != nil {
		return sc.err
	}
	dst, err := expandWord(s.words[len(s.words)-1], b.lookup)
	if err != nil {
		return err
	}
	owner, err := b.owner(s)
	if err != nil {
		return err
	}
	chmod, err := b.chmod(s)
	if err != nil {
		return err
	}
	dirForm := dst == "" || dst == "." || strings.HasSuffix(dst, "/")
	if strings.ContainsRune(dst, 0) {
		return fmt.Errorf("invalid destination %q", dst)
	}
	if !path.IsAbs(dst) {
		dst = path.Join(b.workdirOrRoot(), dst)
	}
	dst = path.Clean(dst)
	if len(sc.sources) > 1 && !dirForm {
		return errors.New("with more than one source the destination must be a directory and end with a /")
	}
	targetDir := dst
	items := make([]replayItem, 0, len(sc.sources))
	totalEntries := 0
	for _, src := range sc.sources {
		rename := src.base
		if !src.isDir && !dirForm {
			isDir, err := b.isDir(ctx, dst)
			if err != nil {
				return err
			}
			if !isDir {
				targetDir, rename = path.Dir(dst), path.Base(dst)
			}
		}
		if src.isDir {
			rename = ""
		}
		items = append(items, replayItem{src: src, rename: rename})
		totalEntries += src.count
	}
	mode := ""
	if len(sc.sources) == 1 && sc.sources[0].isDir {
		m := sc.sources[0].mode & 0o7777
		if chmod >= 0 {
			m = chmod
		}
		mode = strconv.FormatInt(m, 8)
	}
	if err := b.mkdir(ctx, targetDir, owner, mode); err != nil {
		return err
	}
	if totalEntries == 0 {
		return nil
	}
	chownPaths, err := b.copyInStaged(ctx, items, targetDir, chmod, owner != "")
	if err != nil {
		return err
	}
	if owner == "" || len(chownPaths) == 0 {
		return nil
	}
	return b.chownPaths(ctx, owner, targetDir, chownPaths)
}

func (b *builder) copyInStaged(ctx context.Context, items []replayItem, dir string, chmod int64, collectChown bool) ([]string, error) {
	pr, pw := io.Pipe()
	type result struct {
		paths []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		paths, err := replayStagedTar(pw, items, chmod, collectChown)
		pw.CloseWithError(err)
		done <- result{paths, err}
	}()
	err := b.eng.CopyIn(ctx, b.id, pr, dir)
	pr.CloseWithError(errCopyEnd)
	res := <-done
	if res.err != nil && !errors.Is(res.err, errCopyEnd) {
		return nil, res.err
	}
	return res.paths, err
}
