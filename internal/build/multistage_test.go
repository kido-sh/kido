package build

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
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
)

type fakeFSNode struct {
	typ  byte // tar.TypeReg, tar.TypeDir, tar.TypeSymlink
	mode int64
	body string
	link string
}

type fakeContainer struct {
	id    string
	image string
	fs    map[string]fakeFSNode
}

type multiFakeEngine struct {
	t             *testing.T
	images        map[string]*engine.Image
	imageFS       map[string]map[string]fakeFSNode
	containers    map[string]*fakeContainer
	active        map[string]bool
	nextID        int
	maxConcurrent int

	calls         []string
	created       []engine.CreateOptions
	runs          []engine.ProcessSpec
	copies        []copyCall
	commits       []commitCall
	removed       []string
	removedImages []string

	onRun       func(cid string, p engine.ProcessSpec, c *fakeContainer) (int, error)
	onCreate    func(opts engine.CreateOptions, cid string) error
	execHook    func(ctx context.Context, cid string, p engine.ProcessSpec, stdio engine.Stdio) (handled bool, status int, err error)
	copyOutHook func(ctx context.Context, cid, srcPath string, w io.Writer) (handled bool, err error)
}

func newMultiFake(t *testing.T) *multiFakeEngine {
	t.Helper()
	m := &multiFakeEngine{
		t:          t,
		images:     map[string]*engine.Image{},
		imageFS:    map[string]map[string]fakeFSNode{},
		containers: map[string]*fakeContainer{},
		active:     map[string]bool{},
	}
	m.addImage("darwin:latest", engine.ImageConfig{
		Cmd:    []string{"/bin/sh"},
		Env:    []string{"PATH=/usr/bin:/bin"},
		Labels: map[string]string{"base": "yes"},
	}, map[string]fakeFSNode{
		"/":            {typ: tar.TypeDir, mode: 0o755},
		"/bin":         {typ: tar.TypeDir, mode: 0o755},
		"/bin/echo":    {typ: tar.TypeReg, mode: 0o755, body: "echo-bin"},
		"/private":     {typ: tar.TypeDir, mode: 0o755},
		"/private/tmp": {typ: tar.TypeDir, mode: 0o777},
		"/tmp":         {typ: tar.TypeSymlink, mode: 0o777, link: "private/tmp"},
	})
	return m
}

func (m *multiFakeEngine) addImage(ref string, cfg engine.ImageConfig, files map[string]fakeFSNode) {
	parsed, err := imageref.Parse(ref)
	if err != nil {
		m.t.Fatal(err)
	}
	m.images[parsed.String()] = &engine.Image{Name: parsed.Name, Tag: parsed.Tag, Config: cloneImageConfig(cfg)}
	cp := map[string]fakeFSNode{"/": {typ: tar.TypeDir, mode: 0o755}}
	for k, v := range files {
		cp[path.Clean(k)] = v
	}
	m.imageFS[parsed.String()] = cp
}

func (m *multiFakeEngine) Create(ctx context.Context, opts engine.CreateOptions) (*engine.Container, error) {
	m.nextID++
	cid := fmt.Sprintf("c0ffee%06d", m.nextID)
	m.calls = append(m.calls, "create "+opts.Image)
	m.created = append(m.created, opts)

	fsCopy := map[string]fakeFSNode{"/": {typ: tar.TypeDir, mode: 0o755}}
	for k, v := range m.imageFS[opts.Image] {
		fsCopy[k] = v
	}
	m.containers[cid] = &fakeContainer{id: cid, image: opts.Image, fs: fsCopy}
	m.active[cid] = true
	if len(m.active) > m.maxConcurrent {
		m.maxConcurrent = len(m.active)
	}

	var err error
	if m.onCreate != nil {
		err = m.onCreate(opts, cid)
	}
	return &engine.Container{ID: cid, Image: opts.Image, State: engine.StateRunning}, err
}

func (m *multiFakeEngine) Exec(ctx context.Context, ref string, p engine.ProcessSpec, stdio engine.Stdio) (int, error) {
	c, ok := m.containers[ref]
	if !ok || !m.active[ref] {
		return 125, fmt.Errorf("container %s is not running", ref)
	}
	if m.execHook != nil {
		if handled, status, err := m.execHook(ctx, ref, p, stdio); handled {
			return status, err
		}
	}
	a := p.Args
	switch {
	case a[0] == "/bin/test" && len(a) == 3 && a[1] == "-d":
		m.calls = append(m.calls, "test -d "+a[2])
		resolved := m.resolveSymlinkPath(c, a[2])
		if n, ok := c.fs[resolved]; ok && n.typ == tar.TypeDir {
			return 0, nil
		}
		return 1, nil
	case a[0] == "/bin/sh" && len(a) == 7 && a[2] == mkdirScript:
		dir, owner, mode := a[4], a[5], a[6]
		s := fmt.Sprintf("mkdir %s owner=%s", dir, owner)
		if mode != "" {
			s += " mode=" + mode
		}
		m.calls = append(m.calls, s)
		cur := ""
		for _, part := range strings.Split(strings.TrimPrefix(path.Clean(dir), "/"), "/") {
			if part == "" {
				continue
			}
			cur += "/" + part
			if _, exists := c.fs[cur]; !exists {
				c.fs[cur] = fakeFSNode{typ: tar.TypeDir, mode: 0o755}
			}
		}
		return 0, nil
	case a[0] == "/bin/chown":
		m.calls = append(m.calls, strings.Join(a, " "))
		return 0, nil
	case a[0] == "/bin/sh" && len(a) == 6 && a[2] == findStageScript:
		searchDir := m.resolveSymlinkPath(c, a[4])
		if n, ok := c.fs[searchDir]; !ok || n.typ != tar.TypeDir {
			return 0, nil
		}
		var maxDepth int
		fmt.Sscanf(a[5], "%d", &maxDepth)
		var buf bytes.Buffer
		buf.WriteString(".\x00")
		var keys []string
		for k := range c.fs {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		prefix := searchDir
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		for _, k := range keys {
			if k == searchDir || !strings.HasPrefix(k, prefix) {
				continue
			}
			rel := strings.TrimPrefix(k, prefix)
			depth := strings.Count(rel, "/") + 1
			if depth <= maxDepth {
				buf.WriteString("./" + rel + "\x00")
			}
		}
		if stdio.Stdout != nil {
			stdio.Stdout.Write(buf.Bytes())
		}
		return 0, nil
	case a[0] == "/bin/sh" && len(a) == 5 && a[2] == realpathStageScript:
		resolved := m.resolveSymlinkPath(c, a[4])
		if n, ok := c.fs[resolved]; !ok || n.typ != tar.TypeDir {
			if stdio.Stderr != nil {
				fmt.Fprintf(stdio.Stderr, "cd: %s: not a directory\n", a[4])
			}
			return 1, nil
		}
		if stdio.Stdout != nil {
			fmt.Fprintln(stdio.Stdout, resolved)
		}
		return 0, nil
	}

	m.calls = append(m.calls, "run "+strings.Join(a, " "))
	m.runs = append(m.runs, p)
	if m.onRun != nil {
		return m.onRun(ref, p, c)
	}
	return 0, nil
}

func (m *multiFakeEngine) resolveSymlinkPath(c *fakeContainer, p string) string {
	clean := path.Clean(p)
	for range 20 {
		parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
		cur := ""
		replaced := false
		for i, seg := range parts {
			if seg == "" {
				continue
			}
			cur += "/" + seg
			if n, ok := c.fs[cur]; ok && n.typ == tar.TypeSymlink {
				target := n.link
				if !path.IsAbs(target) {
					target = path.Join(path.Dir(cur), target)
				}
				rest := strings.Join(parts[i+1:], "/")
				if rest != "" {
					clean = path.Clean(target + "/" + rest)
				} else {
					clean = path.Clean(target)
				}
				replaced = true
				break
			}
		}
		if !replaced {
			return clean
		}
	}
	return clean
}

func (m *multiFakeEngine) CopyIn(ctx context.Context, ref string, r io.Reader, dstDir string) error {
	c, ok := m.containers[ref]
	if !ok || !m.active[ref] {
		return fmt.Errorf("container %s is not running", ref)
	}
	entries := readTar(m.t, r)
	m.calls = append(m.calls, "copyin "+dstDir)
	m.copies = append(m.copies, copyCall{dir: dstDir, entries: entries})
	for _, e := range entries {
		full := path.Join(dstDir, strings.TrimSuffix(e.Name, "/"))
		c.fs[full] = fakeFSNode{typ: e.Type, mode: e.Mode, body: e.Body, link: e.Link}
	}
	return nil
}

func (m *multiFakeEngine) CopyOut(ctx context.Context, ref string, srcPath string, w io.Writer) error {
	c, ok := m.containers[ref]
	if !ok || !m.active[ref] {
		return fmt.Errorf("container %s is not running", ref)
	}
	m.calls = append(m.calls, "copyout "+srcPath)
	if m.copyOutHook != nil {
		handled, err := m.copyOutHook(ctx, ref, srcPath, w)
		if handled {
			return err
		}
	}
	src := path.Clean(srcPath)
	parent := path.Dir(src)
	base := path.Base(src)
	resolvedParent := m.resolveSymlinkPath(c, parent)
	target := "/"
	if src != "/" {
		target = path.Join(resolvedParent, base)
	}
	node, exists := c.fs[target]
	if !exists {
		return fmt.Errorf("/bin/tar: tar: ./%s: Cannot stat: No such file or directory", base)
	}
	tw := tar.NewWriter(w)
	writeNode := func(tarName string, n fakeFSNode) error {
		hdr := &tar.Header{Typeflag: n.typ, Name: tarName, Mode: n.mode, Linkname: n.link}
		if n.typ == tar.TypeDir && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		if n.typ == tar.TypeReg {
			hdr.Size = int64(len(n.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if n.typ == tar.TypeReg {
			_, err := tw.Write([]byte(n.body))
			return err
		}
		return nil
	}

	topName := "."
	if src != "/" {
		topName = "./" + base
	}
	if err := writeNode(topName, node); err != nil {
		return err
	}
	if node.typ == tar.TypeDir {
		prefix := target
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		var keys []string
		for k := range c.fs {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			rel := strings.TrimPrefix(k, prefix)
			name := "./" + rel
			if src != "/" {
				name = "./" + base + "/" + rel
			}
			if err := writeNode(name, c.fs[k]); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

func (m *multiFakeEngine) Commit(ctx context.Context, ref, image string, cfg engine.ImageConfig) (*engine.Image, error) {
	c, ok := m.containers[ref]
	if !ok {
		return nil, fmt.Errorf("no such container %s", ref)
	}
	m.calls = append(m.calls, "commit "+image)
	m.commits = append(m.commits, commitCall{ref: ref, image: image, cfg: cloneImageConfig(cfg)})
	parsed, err := imageref.Parse(image)
	if err != nil {
		return nil, err
	}
	img := &engine.Image{Name: parsed.Name, Tag: parsed.Tag, Config: cloneImageConfig(cfg)}
	m.images[parsed.String()] = img
	fsCopy := map[string]fakeFSNode{}
	for k, v := range c.fs {
		fsCopy[k] = v
	}
	m.imageFS[parsed.String()] = fsCopy
	return img, nil
}

func (m *multiFakeEngine) Remove(ctx context.Context, ref string, force bool) error {
	if ctx.Err() != nil {
		m.t.Errorf("Remove(%s) called with cancelled context", ref)
	}
	if !force {
		m.t.Errorf("Remove(%s) without force", ref)
	}
	m.calls = append(m.calls, "remove "+ref)
	m.removed = append(m.removed, ref)
	delete(m.active, ref)
	delete(m.containers, ref)
	return nil
}

func (m *multiFakeEngine) RemoveImage(ctx context.Context, name string) error {
	if ctx.Err() != nil {
		m.t.Errorf("RemoveImage(%s) called with cancelled context", name)
	}
	parsed, err := imageref.Parse(name)
	if err != nil {
		return err
	}
	for _, c := range m.containers {
		if c.image == parsed.String() {
			return fmt.Errorf("image %s is in use by container %s", parsed, c.id)
		}
	}
	m.calls = append(m.calls, "rmi "+parsed.String())
	m.removedImages = append(m.removedImages, parsed.String())
	delete(m.images, parsed.String())
	delete(m.imageFS, parsed.String())
	return nil
}

func (m *multiFakeEngine) Images(ctx context.Context) ([]*engine.Image, error) {
	var out []*engine.Image
	for _, img := range m.images {
		out = append(out, img)
	}
	return out, nil
}

func (m *multiFakeEngine) Stop(context.Context, string, time.Duration) error       { panic("unused") }
func (m *multiFakeEngine) Start(context.Context, string) error                     { panic("unused") }
func (m *multiFakeEngine) List(context.Context, bool) ([]*engine.Container, error) { panic("unused") }
func (m *multiFakeEngine) Inspect(context.Context, string) (*engine.Container, error) {
	panic("unused")
}
func (m *multiFakeEngine) ImportImage(context.Context, string, string, string, string, string) (*engine.Image, error) {
	panic("unused")
}

var _ engine.Engine = (*multiFakeEngine)(nil)

func (m *multiFakeEngine) assertNoLeaks(t *testing.T) {
	t.Helper()
	if m.maxConcurrent > 1 {
		t.Errorf("maxConcurrent VMs = %d, want <= 1", m.maxConcurrent)
	}
	if len(m.active) != 0 || len(m.containers) != 0 {
		t.Errorf("leaked containers: active=%v all=%v", m.active, m.containers)
	}
	for name := range m.images {
		if strings.HasPrefix(name, "kido-build-stage-") {
			t.Errorf("leaked temporary stage image %s", name)
		}
	}
}

func runMultiBuild(t *testing.T, m *multiFakeEngine, files map[string]string, df string, mod func(*Options)) (*engine.Image, string, string, error) {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, files)
	var out, errw bytes.Buffer
	opts := Options{ContextDir: dir, Kidofile: []byte(df), Tag: "app:1", Stdout: &out, Stderr: &errw}
	if mod != nil {
		mod(&opts)
	}
	img, err := Build(context.Background(), m, opts)
	return img, out.String(), errw.String(), err
}

func TestMultiStageCopyFromNameAndIndex(t *testing.T) {
	m := newMultiFake(t)
	m.onRun = func(cid string, p engine.ProcessSpec, c *fakeContainer) (int, error) {
		if strings.Contains(strings.Join(p.Args, " "), "make-artifacts") {
			c.fs["/out"] = fakeFSNode{typ: tar.TypeDir, mode: 0o750}
			c.fs["/out/app"] = fakeFSNode{typ: tar.TypeReg, mode: 0o755, body: "elf-or-macho"}
			c.fs["/out/ignored.txt"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "stage-not-ignored"}
			c.fs["/out/sub"] = fakeFSNode{typ: tar.TypeDir, mode: 0o755}
			c.fs["/out/sub/data.txt"] = fakeFSNode{typ: tar.TypeReg, mode: 0o640, body: "payload"}
			c.fs["/out/link"] = fakeFSNode{typ: tar.TypeSymlink, mode: 0o777, link: "sub/data.txt"}
			c.fs["/builder-only.txt"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "secret"}
		}
		return 0, nil
	}

	df := `FROM darwin AS builder
RUN make-artifacts

FROM darwin
COPY host.txt /host.txt
COPY --from=builder --chown=1000 /out/app /usr/local/bin/app
COPY --from=builder /out/ignored.txt /from-stage-ignored.txt
COPY --from=0 --chmod=600 /out/sub /srv/sub
COPY --from=builder /out/link /srv/link
`
	files := map[string]string{
		"host.txt":    "from-host",
		"ignored.txt": "must-stay-ignored-on-host",
		".kidoignore": "ignored.txt\n",
	}
	img, _, errw, err := runMultiBuild(t, m, files, df, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, errw)
	}
	m.assertNoLeaks(t)
	if img == nil || img.Name != "app" || img.Tag != "1" {
		t.Fatalf("unexpected image: %+v", img)
	}

	finalFS := m.imageFS["app:1"]
	if got := finalFS["/usr/local/bin/app"]; got.body != "elf-or-macho" || got.mode != 0o755 {
		t.Errorf("/usr/local/bin/app = %+v", got)
	}
	if got := finalFS["/from-stage-ignored.txt"]; got.body != "stage-not-ignored" {
		t.Errorf(".kidoignore must not affect COPY --from: got %+v", got)
	}
	if got := finalFS["/srv/sub/data.txt"]; got.body != "payload" || got.mode != 0o600 {
		t.Errorf("/srv/sub/data.txt = %+v, want mode 0600", got)
	}
	if got := finalFS["/srv/link"]; got.typ != tar.TypeSymlink || got.link != "sub/data.txt" {
		t.Errorf("/srv/link = %+v", got)
	}
	if _, leaked := finalFS["/builder-only.txt"]; leaked {
		t.Error("builder-only.txt leaked into final image")
	}
	if !slices.Contains(m.calls, "/bin/chown 1000:1000 /usr/local/bin/app") {
		t.Errorf("expected chown on /usr/local/bin/app, calls: %v", m.calls)
	}
}

func TestMultiStageFromPriorStageInheritance(t *testing.T) {
	m := newMultiFake(t)
	m.onRun = func(cid string, p engine.ProcessSpec, c *fakeContainer) (int, error) {
		if strings.Contains(strings.Join(p.Args, " "), "write-base") {
			c.fs["/base-file"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "base"}
		}
		return 0, nil
	}

	df := `FROM darwin AS base
ARG BASE_ARG=secret-arg
ENV KEEP=from-base OVER=from-base
WORKDIR /work
USER 1000
LABEL l0=v0
EXPOSE 8080
ENTRYPOINT ["/bin/base"]
CMD ["arg0"]
RUN write-base

FROM base as mid
ENV OVER=from-mid
LABEL l1=v1
RUN echo mid-check

FROM mid
ENTRYPOINT ["/bin/final"]
`
	_, _, errw, err := runMultiBuild(t, m, nil, df, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, errw)
	}
	m.assertNoLeaks(t)
	if len(m.removedImages) != 2 {
		t.Fatalf("expected 2 intermediate stage images removed, got %v", m.removedImages)
	}
	if got := m.imageFS["app:1"]["/base-file"].body; got != "base" {
		t.Errorf("inherited file missing from final image: %q", got)
	}
	if slices.Contains(m.runs[1].Env, "BASE_ARG=secret-arg") {
		t.Errorf("stage 0 ARG leaked into stage 1 RUN env: %v", m.runs[1].Env)
	}
	finalCfg := m.images["app:1"].Config
	if finalCfg.WorkingDir != "/work" || finalCfg.User != "1000" {
		t.Errorf("inherited workdir/user lost: %+v", finalCfg)
	}
	if finalCfg.Labels["l0"] != "v0" || finalCfg.Labels["l1"] != "v1" || finalCfg.Labels[ExposeLabel] != "8080/tcp" {
		t.Errorf("inherited labels lost: %+v", finalCfg.Labels)
	}
	if !reflect.DeepEqual(finalCfg.Entrypoint, []string{"/bin/final"}) || finalCfg.Cmd != nil {
		t.Errorf("ENTRYPOINT in child stage must reset inherited CMD: %+v", finalCfg)
	}
	if v, ok := envLookup(finalCfg.Env, "KEEP"); !ok || v != "from-base" {
		t.Errorf("KEEP env lost: %v", finalCfg.Env)
	}
	if v, ok := envLookup(finalCfg.Env, "OVER"); !ok || v != "from-mid" {
		t.Errorf("OVER env not overridden: %v", finalCfg.Env)
	}
}

func TestMultiStageMetadataIsolation(t *testing.T) {
	m := newMultiFake(t)
	df := `FROM darwin AS builder
ARG BUILD_ONLY=1
ENV BUILD_ENV=yes
WORKDIR /build
USER 1000
LABEL build=true
EXPOSE 9090
ENTRYPOINT ["/build-entry"]
CMD ["build-cmd"]

FROM darwin
RUN check-clean
`
	_, _, errw, err := runMultiBuild(t, m, nil, df, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, errw)
	}
	m.assertNoLeaks(t)
	if len(m.created) != 1 {
		t.Fatalf("unreachable builder stage should have been skipped, created=%v", m.created)
	}

	m2 := newMultiFake(t)
	m2.onRun = func(cid string, p engine.ProcessSpec, c *fakeContainer) (int, error) {
		if strings.Contains(strings.Join(p.Args, " "), "make") {
			c.fs["/f"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "ok"}
		}
		return 0, nil
	}
	df2 := `FROM darwin AS builder
ARG BUILD_ONLY=1
ENV BUILD_ENV=yes
WORKDIR /build
USER 1000
LABEL build=true
EXPOSE 9090
ENTRYPOINT ["/build-entry"]
CMD ["build-cmd"]
RUN make

FROM darwin
COPY --from=builder /f /f
RUN check-clean
`
	if _, _, errw, err := runMultiBuild(t, m2, nil, df2, nil); err != nil {
		t.Fatalf("%v\n%s", err, errw)
	}
	m2.assertNoLeaks(t)
	run := m2.runs[1]
	if run.Dir != "" || run.User != "" {
		t.Errorf("stage 0 workdir/user leaked into stage 1: %+v", run)
	}
	if _, ok := envLookup(run.Env, "BUILD_ENV"); ok {
		t.Errorf("BUILD_ENV leaked: %v", run.Env)
	}
	if _, ok := envLookup(run.Env, "BUILD_ONLY"); ok {
		t.Errorf("BUILD_ONLY ARG leaked: %v", run.Env)
	}
	cfg := m2.images["app:1"].Config
	if cfg.Labels["build"] != "" || cfg.Labels[ExposeLabel] != "" {
		t.Errorf("labels leaked from stage 0: %+v", cfg.Labels)
	}
	if cfg.Entrypoint != nil || !reflect.DeepEqual(cfg.Cmd, []string{"/bin/sh"}) {
		t.Errorf("entrypoint/cmd leaked from stage 0: %+v", cfg)
	}
}

func TestMultiStageTargetAndReachability(t *testing.T) {
	df := `FROM darwin AS builder
LABEL stage=builder
RUN make

FROM missing-never-used AS unused
RUN fail-if-run

FROM darwin AS final
LABEL stage=final
COPY --from=builder /art /art
`
	m := newMultiFake(t)
	m.onRun = func(cid string, p engine.ProcessSpec, c *fakeContainer) (int, error) {
		if strings.Contains(strings.Join(p.Args, " "), "make") {
			c.fs["/art"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "ok"}
		}
		return 0, nil
	}
	if _, _, errw, err := runMultiBuild(t, m, nil, df, nil); err != nil {
		t.Fatalf("default target: %v\n%s", err, errw)
	}
	m.assertNoLeaks(t)
	if len(m.created) != 2 {
		t.Fatalf("expected 2 stages built (skipping unused), created=%d", len(m.created))
	}
	if m.images["app:1"].Config.Labels["stage"] != "final" {
		t.Errorf("expected final stage committed, got %+v", m.images["app:1"].Config.Labels)
	}

	m2 := newMultiFake(t)
	m2.onRun = m.onRun
	if _, _, errw, err := runMultiBuild(t, m2, nil, df, func(o *Options) { o.Target = "builder" }); err != nil {
		t.Fatalf("--target builder: %v\n%s", err, errw)
	}
	m2.assertNoLeaks(t)
	if len(m2.created) != 1 {
		t.Fatalf("expected 1 stage built for --target builder, created=%d", len(m2.created))
	}
	if m2.images["app:1"].Config.Labels["stage"] != "builder" {
		t.Errorf("expected builder stage committed, got %+v", m2.images["app:1"].Config.Labels)
	}

	m3 := newMultiFake(t)
	_, _, _, err := runMultiBuild(t, m3, nil, df, func(o *Options) { o.Target = "nope" })
	if err == nil || !strings.Contains(err.Error(), `target stage "nope" not found`) {
		t.Fatalf("want unknown target error, got %v", err)
	}
	if len(m3.created) != 0 {
		t.Fatalf("created container for unknown target: %v", m3.created)
	}
}

func TestMultiStageCopyFromExternalImage(t *testing.T) {
	m := newMultiFake(t)
	pulled := []string{}
	pullFn := func(ctx context.Context, name string) error {
		pulled = append(pulled, name)
		if name == "tools:1" {
			m.addImage("tools:1", engine.ImageConfig{}, map[string]fakeFSNode{
				"/usr":           {typ: tar.TypeDir, mode: 0o755},
				"/usr/bin":       {typ: tar.TypeDir, mode: 0o755},
				"/usr/bin/jq":    {typ: tar.TypeReg, mode: 0o755, body: "jq-bin"},
				"/usr/bin/other": {typ: tar.TypeReg, mode: 0o755, body: "other-bin"},
			})
			return nil
		}
		return fmt.Errorf("cannot pull %s", name)
	}

	df := `FROM darwin
COPY --from=tools:1 /usr/bin/jq /usr/local/bin/jq
COPY --from=tools:1 /usr/bin/other /usr/local/bin/other
COPY --from=darwin /bin/echo /out/echo
`
	_, _, errw, err := runMultiBuild(t, m, nil, df, func(o *Options) { o.Pull = pullFn })
	if err != nil {
		t.Fatalf("%v\n%s", err, errw)
	}
	m.assertNoLeaks(t)
	if !reflect.DeepEqual(pulled, []string{"tools:1"}) {
		t.Errorf("pulled = %v, want [tools:1]", pulled)
	}
	toolsBoots := 0
	for _, c := range m.created {
		if c.Image == "tools:1" {
			toolsBoots++
		}
	}
	if toolsBoots != 1 {
		t.Errorf("tools:1 booted %d times, want 1 (deduplicated)", toolsBoots)
	}
	finalFS := m.imageFS["app:1"]
	if finalFS["/usr/local/bin/jq"].body != "jq-bin" || finalFS["/usr/local/bin/other"].body != "other-bin" || finalFS["/out/echo"].body != "echo-bin" {
		t.Errorf("copied external image files missing: %+v", finalFS)
	}
}

func TestMultiStageGlobalArgsAndDynamicCopyFrom(t *testing.T) {
	m := newMultiFake(t)
	m.onRun = func(cid string, p engine.ProcessSpec, c *fakeContainer) (int, error) {
		if strings.Contains(strings.Join(p.Args, " "), "build-b") {
			c.fs["/out"] = fakeFSNode{typ: tar.TypeDir, mode: 0o755}
			c.fs["/out/tool"] = fakeFSNode{typ: tar.TypeReg, mode: 0o755, body: "from-b"}
		}
		return 0, nil
	}

	df := `ARG BASE=darwin
ARG PICK=builder_a

FROM $BASE AS builder_a
RUN build-a

FROM $BASE AS builder_b
RUN build-b

FROM $BASE
ENV DIR=/out
COPY --from=$PICK $DIR/tool /usr/local/bin/tool
`
	_, _, errw, err := runMultiBuild(t, m, nil, df, func(o *Options) {
		o.BuildArgs = map[string]string{"PICK": "builder_b"}
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, errw)
	}
	m.assertNoLeaks(t)
	if len(m.created) != 2 {
		t.Fatalf("expected builder_a skipped and 2 stages built, created=%d", len(m.created))
	}
	if got := m.imageFS["app:1"]["/usr/local/bin/tool"].body; got != "from-b" {
		t.Errorf("tool body = %q, want from-b", got)
	}
}

func TestMultiStageWildcardsAndTrailingSlashSymlink(t *testing.T) {
	m := newMultiFake(t)
	var realpathEnvs [][]string
	m.execHook = func(ctx context.Context, cid string, p engine.ProcessSpec, stdio engine.Stdio) (bool, int, error) {
		if len(p.Args) == 5 && p.Args[0] == "/bin/sh" && p.Args[2] == realpathStageScript {
			realpathEnvs = append(realpathEnvs, slices.Clone(p.Env))
		}
		return false, 0, nil
	}
	m.onRun = func(cid string, p engine.ProcessSpec, c *fakeContainer) (int, error) {
		if strings.Contains(strings.Join(p.Args, " "), "populate") {
			c.fs["/out"] = fakeFSNode{typ: tar.TypeDir, mode: 0o755}
			c.fs["/out/a.txt"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "aaa"}
			c.fs["/out/b.txt"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "bbb"}
			c.fs["/out/skip.log"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "log"}
			c.fs["/out/dir1"] = fakeFSNode{typ: tar.TypeDir, mode: 0o755}
			c.fs["/out/dir1/one.txt"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "one"}
			c.fs["/out/dirfile"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "not-a-dir"}
			c.fs["/realdir"] = fakeFSNode{typ: tar.TypeDir, mode: 0o755}
			c.fs["/realdir/two.txt"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "two"}
			c.fs["/out/dirlink"] = fakeFSNode{typ: tar.TypeSymlink, mode: 0o777, link: "/realdir"}
			c.fs["/private/tmp/t.txt"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "from-tmp"}
			c.fs["/private/dir with space "] = fakeFSNode{typ: tar.TypeDir, mode: 0o755}
			c.fs["/private/dir with space /sp.txt"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "spaced"}
			c.fs["/spacedir"] = fakeFSNode{typ: tar.TypeSymlink, mode: 0o777, link: "/private/dir with space "}
		}
		return 0, nil
	}

	df := `FROM darwin AS builder
RUN populate

FROM darwin
COPY --from=builder /out/*.txt /out/missing*.md /dst/
COPY --from=builder /tmp/ /copied-tmp/
COPY --from=builder /tmp/. /copied-tmp-dot/
COPY --from=builder /spacedir/ /copied-space/
COPY --from=builder /out/dir*/ /copied-dirs/
`
	_, _, errw, err := runMultiBuild(t, m, nil, df, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, errw)
	}
	m.assertNoLeaks(t)
	finalFS := m.imageFS["app:1"]
	if finalFS["/dst/a.txt"].body != "aaa" || finalFS["/dst/b.txt"].body != "bbb" {
		t.Errorf("wildcard copies missing: %+v", finalFS)
	}
	if _, exists := finalFS["/dst/skip.log"]; exists {
		t.Error("non-matching file should not be copied")
	}
	if finalFS["/copied-tmp/t.txt"].body != "from-tmp" {
		t.Errorf("trailing-slash symlinked dir /tmp/ did not resolve: %+v", finalFS)
	}
	if finalFS["/copied-tmp-dot/t.txt"].body != "from-tmp" {
		t.Errorf("trailing /. symlinked dir /tmp/. did not resolve: %+v", finalFS)
	}
	if finalFS["/copied-space/sp.txt"].body != "spaced" {
		t.Errorf("symlinked dir ending with space did not resolve: %+v", finalFS)
	}
	if finalFS["/copied-dirs/one.txt"].body != "one" || finalFS["/copied-dirs/two.txt"].body != "two" {
		t.Errorf("wildcard dir*/ copies missing: %+v", finalFS)
	}
	if _, exists := finalFS["/copied-dirs/dirfile"]; exists {
		t.Error("regular file matching dir*/ should have been skipped")
	}
	if len(realpathEnvs) == 0 {
		t.Error("expected resolveGuestDir to run")
	}
	for _, env := range realpathEnvs {
		if !reflect.DeepEqual(env, []string{"PATH=/bin:/usr/bin:/sbin:/usr/sbin"}) {
			t.Errorf("resolveGuestDir env = %v, want pinned PATH", env)
		}
	}

	for _, src := range []string{"/out/a.txt/", "/out/a.txt/."} {
		m2 := newMultiFake(t)
		m2.onRun = m.onRun
		_, _, _, err = runMultiBuild(t, m2, nil, "FROM darwin AS builder\nRUN populate\nFROM darwin\nCOPY --from=builder "+src+" /dst/\n", nil)
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("%s on regular file: got %v, want not a directory", src, err)
		}
		m2.assertNoLeaks(t)
	}
}

func TestMultiStageCleanupOnErrorAndCancel(t *testing.T) {
	t.Run("stage1 RUN failure cleans up stage0 temp image and containers", func(t *testing.T) {
		m := newMultiFake(t)
		m.onRun = func(cid string, p engine.ProcessSpec, c *fakeContainer) (int, error) {
			cmd := strings.Join(p.Args, " ")
			if strings.Contains(cmd, "make") {
				c.fs["/art"] = fakeFSNode{typ: tar.TypeReg, mode: 0o644, body: "x"}
				return 0, nil
			}
			if strings.Contains(cmd, "boom") {
				return 7, nil
			}
			return 0, nil
		}
		df := `FROM darwin AS base
RUN make

FROM base
COPY --from=base /art /art2
RUN boom
`
		_, _, _, err := runMultiBuild(t, m, nil, df, nil)
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.ExitStatus != 7 {
			t.Fatalf("got %v, want StepError exit 7", err)
		}
		m.assertNoLeaks(t)
		if len(m.removedImages) != 1 {
			t.Errorf("expected temp stage image removed, removedImages=%v", m.removedImages)
		}
	})

	t.Run("cancelled context cleans up containers and temp images", func(t *testing.T) {
		m := newMultiFake(t)
		ctx, cancel := context.WithCancel(context.Background())
		m.onRun = func(cid string, p engine.ProcessSpec, c *fakeContainer) (int, error) {
			if strings.Contains(strings.Join(p.Args, " "), "cancel-here") {
				cancel()
				return 1, context.Canceled
			}
			return 0, nil
		}
		df := "FROM darwin AS base\nRUN ok\nFROM base\nRUN cancel-here\n"
		dir := t.TempDir()
		_, err := Build(ctx, m, Options{ContextDir: dir, Kidofile: []byte(df), Tag: "app:1"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
		m.assertNoLeaks(t)
		if len(m.removedImages) != 1 {
			t.Errorf("expected temp stage image removed on cancel, removedImages=%v", m.removedImages)
		}
	})

	t.Run("external image create failure still removes allocated container", func(t *testing.T) {
		m := newMultiFake(t)
		m.addImage("ext:1", engine.ImageConfig{}, nil)
		m.onCreate = func(opts engine.CreateOptions, cid string) error {
			if opts.Image == "ext:1" {
				return errors.New("boot failed")
			}
			return nil
		}
		_, _, _, err := runMultiBuild(t, m, nil, "FROM darwin\nCOPY --from=ext:1 /a /b\n", nil)
		if err == nil || !strings.Contains(err.Error(), "boot failed") {
			t.Fatalf("got %v, want boot failed", err)
		}
		m.assertNoLeaks(t)
	})

	t.Run("dynamic .. source error fails at exact COPY step", func(t *testing.T) {
		m := newMultiFake(t)
		df := "FROM darwin AS builder\nRUN ok\nFROM darwin\nENV P=..\nCOPY --from=builder $P/etc/passwd /x\n"
		_, _, _, err := runMultiBuild(t, m, nil, df, nil)
		if err == nil || !strings.Contains(err.Error(), `(line 5) COPY:`) || !strings.Contains(err.Error(), `".." is not allowed`) {
			t.Fatalf("got %v", err)
		}
		m.assertNoLeaks(t)
	})
}

func makeRawTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.Name,
			Typeflag: e.Type,
			Mode:     e.Mode,
			Uid:      e.Uid,
			Gid:      e.Gid,
			Linkname: e.Link,
		}
		if e.Type == tar.TypeReg || e.Type == 0 {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSanitizeExtractedTarSecurity(t *testing.T) {
	badCases := []struct {
		name        string
		expectedTop string
		entries     []tarEntry
		wantErr     string
	}{
		{
			name:        "absolute path",
			expectedTop: "out",
			entries:     []tarEntry{{Name: "/etc/passwd", Type: tar.TypeReg, Mode: 0o644, Body: "x"}},
			wantErr:     "refusing tar entry",
		},
		{
			name:        "dot-dot traversal",
			expectedTop: "out",
			entries:     []tarEntry{{Name: "./out/../../etc/passwd", Type: tar.TypeReg, Mode: 0o644, Body: "x"}},
			wantErr:     "refusing tar entry",
		},
		{
			name:        "entry outside expectedTop",
			expectedTop: "out",
			entries: []tarEntry{
				{Name: "./out/", Type: tar.TypeDir, Mode: 0o755},
				{Name: "./other/evil", Type: tar.TypeReg, Mode: 0o644, Body: "x"},
			},
			wantErr: "outside",
		},
		{
			name:        "symlink write-through",
			expectedTop: "out",
			entries: []tarEntry{
				{Name: "./out/", Type: tar.TypeDir, Mode: 0o755},
				{Name: "./out/sub", Type: tar.TypeSymlink, Mode: 0o777, Link: "/etc"},
				{Name: "./out/sub/passwd", Type: tar.TypeReg, Mode: 0o644, Body: "pwn"},
			},
			wantErr: "is a symlink",
		},
		{
			name:        "reverse symlink over existing dir",
			expectedTop: "out",
			entries: []tarEntry{
				{Name: "./out/", Type: tar.TypeDir, Mode: 0o755},
				{Name: "./out/sub/file", Type: tar.TypeReg, Mode: 0o644, Body: "ok"},
				{Name: "./out/sub", Type: tar.TypeSymlink, Mode: 0o777, Link: "/etc"},
			},
			wantErr: "already exists in archive",
		},
		{
			name:        "hard link outside expectedTop",
			expectedTop: "out",
			entries: []tarEntry{
				{Name: "./out/", Type: tar.TypeDir, Mode: 0o755},
				{Name: "./out/hl", Type: tar.TypeLink, Mode: 0o644, Link: "./other/secret"},
			},
			wantErr: "refusing hard link",
		},
		{
			name:        "hard link to missing target",
			expectedTop: "out",
			entries: []tarEntry{
				{Name: "./out/", Type: tar.TypeDir, Mode: 0o755},
				{Name: "./out/hl", Type: tar.TypeLink, Mode: 0o644, Link: "./out/nope"},
			},
			wantErr: "refusing hard link",
		},
		{
			name:        "hard link to symlink",
			expectedTop: "out",
			entries: []tarEntry{
				{Name: "./out/", Type: tar.TypeDir, Mode: 0o755},
				{Name: "./out/s", Type: tar.TypeSymlink, Mode: 0o777, Link: "/etc/passwd"},
				{Name: "./out/hl", Type: tar.TypeLink, Mode: 0o644, Link: "./out/s"},
			},
			wantErr: "refusing hard link",
		},
		{
			name:        "device node rejected",
			expectedTop: "out",
			entries: []tarEntry{
				{Name: "./out/", Type: tar.TypeDir, Mode: 0o755},
				{Name: "./out/null", Type: tar.TypeChar, Mode: 0o666},
			},
			wantErr: "unsupported file type",
		},
		{
			name:        "fifo rejected",
			expectedTop: "out",
			entries: []tarEntry{
				{Name: "./out/", Type: tar.TypeDir, Mode: 0o755},
				{Name: "./out/pipe", Type: tar.TypeFifo, Mode: 0o644},
			},
			wantErr: "unsupported file type",
		},
		{
			name:        "extra entry after single file",
			expectedTop: "app",
			entries: []tarEntry{
				{Name: "./app", Type: tar.TypeReg, Mode: 0o755, Body: "bin"},
				{Name: "./app/sneaky", Type: tar.TypeReg, Mode: 0o644, Body: "bad"},
			},
			wantErr: "unexpected tar entry",
		},
		{
			name:        "empty symlink linkname",
			expectedTop: "out",
			entries: []tarEntry{
				{Name: "./out/", Type: tar.TypeDir, Mode: 0o755},
				{Name: "./out/badlink", Type: tar.TypeSymlink, Mode: 0o777, Link: ""},
			},
			wantErr: "empty symlink target",
		},
	}

	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			raw := makeRawTar(t, tc.entries)
			var out bytes.Buffer
			_, err := sanitizeExtractedTar(bytes.NewReader(raw), &out, tc.expectedTop, false)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got err=%v, want substring %q", err, tc.wantErr)
			}
			if !errors.Is(err, errTarRejected) {
				t.Errorf("expected errTarRejected, got %v", err)
			}
		})
	}

	t.Run("valid directory with hard link and owner reset strips non-root suid/sgid", func(t *testing.T) {
		raw := makeRawTar(t, []tarEntry{
			{Name: "./out/", Type: tar.TypeDir, Mode: 0o6750, Uid: 501, Gid: 20},
			{Name: "./out/nonroot.bin", Type: tar.TypeReg, Mode: 0o7755, Uid: 501, Gid: 20, Body: "hello"},
			{Name: "./out/nonroot-uid.bin", Type: tar.TypeReg, Mode: 0o6755, Uid: 501, Gid: 0, Body: "u"},
			{Name: "./out/nonroot-gid.bin", Type: tar.TypeReg, Mode: 0o6755, Uid: 0, Gid: 20, Body: "g"},
			{Name: "./out/root.bin", Type: tar.TypeReg, Mode: 0o6755, Uid: 0, Gid: 0, Body: "r"},
			{Name: "./out/link.bin", Type: tar.TypeLink, Mode: 0o644, Link: "./out/nonroot.bin"},
			{Name: "./out/s", Type: tar.TypeSymlink, Mode: 0o777, Link: "nonroot.bin"},
		})
		var out bytes.Buffer
		meta, err := sanitizeExtractedTar(bytes.NewReader(raw), &out, "out", false)
		if err != nil {
			t.Fatal(err)
		}
		if !meta.isDir || meta.mode != 0o750 || meta.count != 6 {
			t.Fatalf("meta = %+v", meta)
		}
		got := readTar(t, &out)
		want := []tarEntry{
			{Name: "nonroot.bin", Type: tar.TypeReg, Mode: 0o1755, Uid: 0, Gid: 0, Body: "hello"},
			{Name: "nonroot-uid.bin", Type: tar.TypeReg, Mode: 0o2755, Uid: 0, Gid: 0, Body: "u"},
			{Name: "nonroot-gid.bin", Type: tar.TypeReg, Mode: 0o4755, Uid: 0, Gid: 0, Body: "g"},
			{Name: "root.bin", Type: tar.TypeReg, Mode: 0o6755, Uid: 0, Gid: 0, Body: "r"},
			{Name: "link.bin", Type: tar.TypeLink, Mode: 0o644, Uid: 0, Gid: 0, Link: "nonroot.bin"},
			{Name: "s", Type: tar.TypeSymlink, Mode: 0o777, Uid: 0, Gid: 0, Link: "nonroot.bin"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got  %+v\nwant %+v", got, want)
		}
	})

	t.Run("single top-level non-root file strips suid/sgid", func(t *testing.T) {
		raw := makeRawTar(t, []tarEntry{
			{Name: "./tool", Type: tar.TypeReg, Mode: 0o6755, Uid: 1000, Gid: 1000, Body: "bin"},
		})
		var out bytes.Buffer
		meta, err := sanitizeExtractedTar(bytes.NewReader(raw), &out, "tool", false)
		if err != nil {
			t.Fatal(err)
		}
		if meta.isDir || meta.mode != 0o0755 {
			t.Fatalf("meta = %+v, want mode 0755", meta)
		}
		got := readTar(t, &out)
		if len(got) != 1 || got[0].Mode != 0o0755 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("root dot entry with non-zero size rejected", func(t *testing.T) {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(&tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 0o755, Size: 5}); err != nil {
			t.Fatal(err)
		}
		buf.Write([]byte("12345"))
		var out bytes.Buffer
		_, err := sanitizeExtractedTar(bytes.NewReader(buf.Bytes()), &out, "", true)
		if !errors.Is(err, errTarRejected) || !strings.Contains(err.Error(), "invalid size") {
			t.Fatalf("got err=%v, want errTarRejected invalid size", err)
		}
	})

	t.Run("entry count and byte limits enforced", func(t *testing.T) {
		raw := makeRawTar(t, []tarEntry{
			{Name: "./out/", Type: tar.TypeDir, Mode: 0o755},
			{Name: "./out/a", Type: tar.TypeReg, Mode: 0o644, Body: "1234"},
			{Name: "./out/b", Type: tar.TypeReg, Mode: 0o644, Body: "5678"},
		})
		var out bytes.Buffer
		_, err := sanitizeExtractedTarLimits(bytes.NewReader(raw), &out, "out", false, 2, 1024)
		if !errors.Is(err, errTarRejected) || !strings.Contains(err.Error(), "exceeds limit of 2 entries") {
			t.Fatalf("entry limit: got %v", err)
		}
		out.Reset()
		_, err = sanitizeExtractedTarLimits(bytes.NewReader(raw), &out, "out", false, 100, 6)
		if !errors.Is(err, errTarRejected) || !strings.Contains(err.Error(), "exceeds limit of 6 bytes") {
			t.Fatalf("byte limit: got %v", err)
		}
	})

	t.Run("cross-source symlink write-through rejected in replayStagedTar", func(t *testing.T) {
		dir := t.TempDir()
		f1 := filepath.Join(dir, "d1.tar")
		f2 := filepath.Join(dir, "d2.tar")
		if err := os.WriteFile(f1, makeRawTar(t, []tarEntry{{Name: "sub", Type: tar.TypeSymlink, Mode: 0o777, Link: "/etc"}}), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f2, makeRawTar(t, []tarEntry{{Name: "sub/passwd", Type: tar.TypeReg, Mode: 0o644, Body: "pwn"}}), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		_, err := replayStagedTar(&out, []replayItem{
			{src: stagedSource{isDir: true, count: 1, tarPath: f1}},
			{src: stagedSource{isDir: true, count: 1, tarPath: f2}},
		}, -1, false)
		if err == nil || !strings.Contains(err.Error(), "is a symlink") {
			t.Fatalf("expected cross-source symlink write-through rejection, got %v", err)
		}
	})

	t.Run("replayStagedTar skips chownPaths when collectChown is false", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "f.tar")
		if err := os.WriteFile(f, makeRawTar(t, []tarEntry{{Name: "a", Type: tar.TypeReg, Mode: 0o644, Body: "x"}}), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		paths, err := replayStagedTar(&out, []replayItem{{src: stagedSource{isDir: true, count: 1, tarPath: f}}}, -1, false)
		if err != nil {
			t.Fatal(err)
		}
		if paths != nil {
			t.Fatalf("expected nil chownPaths when collectChown=false, got %v", paths)
		}
		out.Reset()
		paths, err = replayStagedTar(&out, []replayItem{{src: stagedSource{isDir: true, count: 1, tarPath: f}}}, -1, true)
		if err != nil || !reflect.DeepEqual(paths, []string{"a"}) {
			t.Fatalf("collectChown=true: paths=%v err=%v", paths, err)
		}
	})

	t.Run("hostile tar from guest CopyOut cancels child context immediately", func(t *testing.T) {
		m := newMultiFake(t)
		cancelled := false
		m.copyOutHook = func(ctx context.Context, cid, srcPath string, w io.Writer) (bool, error) {
			raw := makeRawTar(t, []tarEntry{
				{Name: "./out/", Type: tar.TypeDir, Mode: 0o755},
				{Name: "./out/../../etc/pwn", Type: tar.TypeReg, Mode: 0o644, Body: "bad"},
			})
			_, _ = w.Write(raw)
			select {
			case <-ctx.Done():
				cancelled = true
				return true, ctx.Err()
			case <-time.After(3 * time.Second):
				return true, errors.New("timed out waiting for cancellation")
			}
		}
		df := "FROM darwin AS builder\nRUN ok\nFROM darwin\nCOPY --from=builder /out /dst\n"
		_, _, _, err := runMultiBuild(t, m, nil, df, nil)
		if err == nil || !strings.Contains(err.Error(), "refusing tar entry") {
			t.Fatalf("expected hostile tar rejection, got %v", err)
		}
		if !cancelled {
			t.Error("expected CopyOut child context to be cancelled on early tar rejection")
		}
		m.assertNoLeaks(t)
	})

	t.Run("overflowing resolveGuestDir and findStageScript cancel child context", func(t *testing.T) {
		m := newMultiFake(t)
		resolveCancelled := false
		m.execHook = func(ctx context.Context, cid string, p engine.ProcessSpec, stdio engine.Stdio) (bool, int, error) {
			if len(p.Args) == 5 && p.Args[0] == "/bin/sh" && p.Args[2] == realpathStageScript {
				_, _ = stdio.Stdout.Write(bytes.Repeat([]byte("a"), maxRealpathOutput+1024))
				select {
				case <-ctx.Done():
					resolveCancelled = true
					return true, 1, ctx.Err()
				case <-time.After(3 * time.Second):
					return true, 1, errors.New("timed out waiting for resolveGuestDir cancel")
				}
			}
			return false, 0, nil
		}
		_, _, _, err := runMultiBuild(t, m, nil, "FROM darwin AS builder\nRUN ok\nFROM darwin\nCOPY --from=builder /tmp/ /dst/\n", nil)
		if err == nil || !strings.Contains(err.Error(), "too long") || !resolveCancelled {
			t.Fatalf("resolveGuestDir overflow: err=%v cancelled=%v", err, resolveCancelled)
		}
		m.assertNoLeaks(t)

		m2 := newMultiFake(t)
		findCancelled := false
		m2.execHook = func(ctx context.Context, cid string, p engine.ProcessSpec, stdio engine.Stdio) (bool, int, error) {
			if len(p.Args) == 6 && p.Args[0] == "/bin/sh" && p.Args[2] == findStageScript {
				chunk := bytes.Repeat([]byte("a"), 1<<20)
				for range 20 {
					_, _ = stdio.Stdout.Write(chunk)
				}
				select {
				case <-ctx.Done():
					findCancelled = true
					return true, 1, ctx.Err()
				case <-time.After(3 * time.Second):
					return true, 1, errors.New("timed out waiting for findStageScript cancel")
				}
			}
			return false, 0, nil
		}
		_, _, _, err = runMultiBuild(t, m2, nil, "FROM darwin AS builder\nRUN ok\nFROM darwin\nCOPY --from=builder /out/*.txt /dst/\n", nil)
		if err == nil || !strings.Contains(err.Error(), "too many matching entries") || !findCancelled {
			t.Fatalf("findStageScript overflow: err=%v cancelled=%v", err, findCancelled)
		}
		m2.assertNoLeaks(t)
	})

	t.Run("staged tar is unlinked immediately after copyFrom", func(t *testing.T) {
		m := newMultiFake(t)
		stageDir := t.TempDir()
		tarFile := filepath.Join(stageDir, "copy-test.tar")
		if err := os.WriteFile(tarFile, makeRawTar(t, []tarEntry{{Name: "file", Type: tar.TypeReg, Mode: 0o644, Body: "hi"}}), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := m.Create(context.Background(), engine.CreateOptions{Image: "darwin:latest"})
		if err != nil {
			t.Fatal(err)
		}
		defer m.Remove(context.Background(), c.ID, true)
		b := &builder{
			eng:  m,
			id:   c.ID,
			args: map[string]*string{},
			stagedCopies: map[copyKey]stagedCopy{
				{1, 0}: {sources: []stagedSource{{base: "f", mode: 0o644, count: 1, tarPath: tarFile}}},
			},
		}
		s := &step{words: []string{"/f", "/dst/f"}, flags: map[string]string{"from": "builder"}}
		if err := b.copyFrom(context.Background(), s, copyKey{1, 0}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(tarFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staged tar %s was not unlinked after copyFrom: %v", tarFile, err)
		}
	})
}

func TestCleanStageSource(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantRel  string
		wantDir  bool
		wantFail bool
	}{
		{"/out/app", "out/app", false, false},
		{"/out/dir/", "out/dir", true, false},
		{"/tmp/.", "tmp", true, false},
		{"/out/a.txt/.", "out/a.txt", true, false},
		{".", ".", true, false},
		{"/.", ".", true, false},
		{"/", ".", true, false},
		{"/out/.hidden", "out/.hidden", false, false},
		{"/out/foo.", "out/foo.", false, false},
		{"/out/../etc/passwd", "", false, true},
		{"../secret", "", false, true},
		{"", "", false, true},
	} {
		rel, dir, err := cleanStageSource(tc.in)
		if tc.wantFail {
			if err == nil {
				t.Errorf("cleanStageSource(%q) = (%q, %v), want error", tc.in, rel, dir)
			}
			continue
		}
		if err != nil || rel != tc.wantRel || dir != tc.wantDir {
			t.Errorf("cleanStageSource(%q) = (%q, %v, %v), want (%q, %v, nil)", tc.in, rel, dir, err, tc.wantRel, tc.wantDir)
		}
	}
}

func TestFindAndRealpathScripts(t *testing.T) {
	shells := [][]string{{"/bin/sh", "sh"}}
	if _, err := os.Stat("/bin/zsh"); err == nil {
		shells = append(shells, []string{"/bin/zsh", "sh"})
	}
	for _, sh := range shells {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{
			"a.txt":        "a",
			"b file.txt":   "b",
			"sub/c.txt":    "c",
			"sub/deep/d.t": "d",
			"linkdir":      "->sub",
		})
		runScript := func(script string, args ...string) ([]byte, error) {
			cmd := exec.Command(sh[0], append([]string{"-c", script, "sh"}, args...)...)
			cmd.Args[0] = sh[1]
			return cmd.CombinedOutput()
		}
		out, err := runScript(findStageScript, dir, "1")
		if err != nil {
			t.Fatalf("%s find depth 1: %v %s", sh[0], err, out)
		}
		var names []string
		for _, raw := range bytes.Split(out, []byte{0}) {
			if len(raw) > 0 {
				names = append(names, string(raw))
			}
		}
		if !slices.Contains(names, "./a.txt") || !slices.Contains(names, "./b file.txt") || slices.Contains(names, "./sub/c.txt") {
			t.Errorf("%s find depth 1 unexpected: %v", sh[0], names)
		}

		missingOut, err := runScript(findStageScript, filepath.Join(dir, "nope"), "1")
		if err != nil || len(missingOut) != 0 {
			t.Errorf("%s missing dir should exit 0 with empty output: err=%v out=%q", sh[0], err, missingOut)
		}

		realOut, err := runScript(realpathStageScript, filepath.Join(dir, "linkdir"))
		if err != nil {
			t.Fatalf("%s realpath: %v %s", sh[0], err, realOut)
		}
		wantReal, _ := filepath.EvalSymlinks(filepath.Join(dir, "sub"))
		if got := strings.TrimSpace(string(realOut)); got != wantReal {
			t.Errorf("%s realpath = %q, want %q", sh[0], got, wantReal)
		}
	}
}
