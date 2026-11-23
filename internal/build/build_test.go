package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/proto"
)

const testID = "c0ffee000001"

type copyCall struct {
	dir     string
	entries []tarEntry
}

type commitCall struct {
	ref, image string
	cfg        engine.ImageConfig
}

// fakeEngine records what a build asks of the engine and simulates a guest with a set of
// directories and per-command exit statuses.
type fakeEngine struct {
	t         *testing.T
	images    []*engine.Image
	calls     []string
	created   []engine.CreateOptions
	runs      []engine.ProcessSpec
	dirs      map[string]bool
	copies    []copyCall
	commits   []commitCall
	removed   []string
	exit      map[string]int // last argument of a RUN -> exit status
	execErr   map[string]error
	createErr error
	chownJSON []int // size of each chown request as the agent receives it
	onRun     func()
	removeOK  bool
}

func newFake(t *testing.T) *fakeEngine {
	return &fakeEngine{
		t:      t,
		images: []*engine.Image{{Name: "darwin", Tag: "latest", Config: engine.ImageConfig{Cmd: []string{"/bin/sh"}, Env: []string{"PATH=/usr/bin:/bin"}, Labels: map[string]string{"base": "yes"}}}},
		dirs:   map[string]bool{"/": true},
		exit:   map[string]int{}, execErr: map[string]error{},
	}
}

func (f *fakeEngine) Create(ctx context.Context, opts engine.CreateOptions) (*engine.Container, error) {
	f.calls = append(f.calls, "create "+opts.Image)
	f.created = append(f.created, opts)
	return &engine.Container{ID: testID, Image: opts.Image, State: engine.StateRunning}, f.createErr
}

func (f *fakeEngine) Exec(ctx context.Context, ref string, p engine.ProcessSpec, stdio engine.Stdio) (int, error) {
	if ref != testID {
		f.t.Errorf("exec in %q", ref)
	}
	a := p.Args
	switch {
	case a[0] == "/bin/test":
		f.calls = append(f.calls, "test -d "+a[2])
		if f.dirs[a[2]] {
			return 0, nil
		}
		return 1, nil
	case a[0] == "/bin/sh" && len(a) == 7 && a[2] == mkdirScript:
		c := fmt.Sprintf("mkdir %s owner=%s", a[4], a[5])
		if a[6] != "" {
			c += " mode=" + a[6]
		}
		f.calls = append(f.calls, c)
		f.dirs[a[4]] = true
		return 0, nil
	case a[0] == "/bin/chown":
		f.calls = append(f.calls, strings.Join(a, " "))
		req, _ := json.Marshal(proto.Request{Op: proto.OpExec, Args: a})
		f.chownJSON = append(f.chownJSON, len(req))
		return 0, nil
	}
	f.calls = append(f.calls, "run "+strings.Join(a, " "))
	f.runs = append(f.runs, p)
	if f.onRun != nil {
		f.onRun()
	}
	last := a[len(a)-1]
	if err := f.execErr[last]; err != nil {
		return f.exit[last], err
	}
	fmt.Fprintf(stdio.Stdout, "out: %s\n", last)
	return f.exit[last], nil
}

func (f *fakeEngine) CopyIn(ctx context.Context, ref string, r io.Reader, dstDir string) error {
	if !f.dirs[dstDir] {
		f.t.Errorf("CopyIn to %s before it was created", dstDir)
	}
	f.calls = append(f.calls, "copy "+dstDir)
	f.copies = append(f.copies, copyCall{dstDir, readTar(f.t, r)})
	return nil
}

func (f *fakeEngine) Commit(ctx context.Context, ref, image string, cfg engine.ImageConfig) (*engine.Image, error) {
	f.calls = append(f.calls, "commit "+image)
	f.commits = append(f.commits, commitCall{ref, image, cfg})
	return &engine.Image{Name: image, Config: cfg}, nil
}

func (f *fakeEngine) Remove(ctx context.Context, ref string, force bool) error {
	if ctx.Err() != nil {
		f.t.Errorf("Remove with a cancelled context")
	}
	if !force {
		f.t.Errorf("Remove without force")
	}
	f.calls = append(f.calls, "remove "+ref)
	f.removed = append(f.removed, ref)
	return nil
}

func (f *fakeEngine) Images(ctx context.Context) ([]*engine.Image, error) { return f.images, nil }

func (f *fakeEngine) CopyOut(context.Context, string, string, io.Writer) error { panic("unused") }
func (f *fakeEngine) Stop(context.Context, string, time.Duration) error        { panic("unused") }
func (f *fakeEngine) Start(context.Context, string) error                      { panic("unused") }
func (f *fakeEngine) List(context.Context, bool) ([]*engine.Container, error)  { panic("unused") }
func (f *fakeEngine) Inspect(context.Context, string) (*engine.Container, error) {
	panic("unused")
}
func (f *fakeEngine) ImportImage(context.Context, string, string, string, string, string) (*engine.Image, error) {
	panic("unused")
}
func (f *fakeEngine) RemoveImage(context.Context, string) error { panic("unused") }

var _ engine.Engine = (*fakeEngine)(nil)

func runBuild(t *testing.T, f *fakeEngine, files map[string]string, df string, mod func(*Options)) (*engine.Image, string, string, error) {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, files)
	var out, errw bytes.Buffer
	opts := Options{ContextDir: dir, Dockerfile: []byte(df), Tag: "myimg:1", Stdout: &out, Stderr: &errw}
	if mod != nil {
		mod(&opts)
	}
	img, err := Build(context.Background(), f, opts)
	return img, out.String(), errw.String(), err
}

func TestBuild(t *testing.T) {
	f := newFake(t)
	df := `ARG BASE=darwin
ARG VERSION
FROM ${BASE}:latest
ARG VERSION
ARG GREETING=hello
ENV APP=/app PATH=/app/bin:/usr/bin:/bin
ENV APP=/srv OLD=$APP
LABEL org.example.version="$VERSION" "quoted key"=v
WORKDIR $APP
WORKDIR sub
COPY hello.txt .
COPY dir /opt/dir
COPY --chown=1000 bin/tool bin/
ADD archive.tar /opt/
USER nobody:nogroup
RUN echo "$GREETING"
RUN ["/bin/tool", "--flag"]
EXPOSE 80 53/udp ${PORT:-8080} 80
ENTRYPOINT ["/bin/tool"]
CMD ["--help"]
`
	files := map[string]string{"hello.txt": "hi", "dir/a": "a", "dir/sub/b": "b", "bin/tool": "t", "archive.tar": "tar"}
	img, out, errw, err := runBuild(t, f, files, df, func(o *Options) {
		o.BuildArgs = map[string]string{"VERSION": "1.2", "UNUSED": "x"}
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, errw)
	}
	wantCalls := []string{
		"create darwin:latest",
		"mkdir /srv owner=",
		"mkdir /srv/sub owner=",
		"mkdir /srv/sub owner=",
		"copy /srv/sub",
		"mkdir /opt/dir owner= mode=755",
		"copy /opt/dir",
		"mkdir /srv/sub/bin owner=1000:1000",
		"copy /srv/sub/bin",
		"/bin/chown 1000:1000 /srv/sub/bin/tool",
		"mkdir /opt owner=",
		"copy /opt",
		`run /bin/sh -c echo "$GREETING"`,
		"run /bin/tool --flag",
		"commit myimg:1",
		"remove " + testID,
	}
	if !reflect.DeepEqual(f.calls, wantCalls) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(wantCalls, "\n"))
	}
	if got := f.created[0].Labels[BuildLabel]; got != "myimg:1" {
		t.Errorf("build label %q", got)
	}
	wantCopies := []copyCall{
		{"/srv/sub", []tarEntry{{Name: "hello.txt", Type: '0', Mode: 0o644, Body: "hi"}}},
		{"/opt/dir", []tarEntry{{Name: "a", Type: '0', Mode: 0o644, Body: "a"}, {Name: "sub/", Type: '5', Mode: 0o755}, {Name: "sub/b", Type: '0', Mode: 0o644, Body: "b"}}},
		{"/srv/sub/bin", []tarEntry{{Name: "tool", Type: '0', Mode: 0o644, Body: "t"}}},
		{"/opt", []tarEntry{{Name: "archive.tar", Type: '0', Mode: 0o644, Body: "tar"}}},
	}
	if !reflect.DeepEqual(f.copies, wantCopies) {
		t.Errorf("copies:\n%+v\nwant\n%+v", f.copies, wantCopies)
	}
	wantEnv := []string{"PATH=/app/bin:/usr/bin:/bin", "APP=/srv", "OLD=/app"}
	run := f.runs[0]
	if !reflect.DeepEqual(run.Env, append(wantEnv, "VERSION=1.2", "GREETING=hello")) || run.Dir != "/srv/sub" || run.User != "nobody:nogroup" {
		t.Errorf("RUN spec %+v", run)
	}
	wantCfg := engine.ImageConfig{
		Entrypoint: []string{"/bin/tool"},
		Cmd:        []string{"--help"},
		Env:        wantEnv,
		WorkingDir: "/srv/sub",
		User:       "nobody:nogroup",
		Labels:     map[string]string{"base": "yes", "org.example.version": "1.2", "quoted key": "v", ExposeLabel: "80/tcp 53/udp 8080/tcp"},
	}
	if c := f.commits[0]; c.ref != testID || !reflect.DeepEqual(c.cfg, wantCfg) {
		t.Errorf("commit %+v\nwant cfg %+v", c, wantCfg)
	}
	if img == nil || img.Name != "myimg:1" {
		t.Errorf("image %+v", img)
	}
	for _, s := range []string{"Step 1/20 : ARG BASE=darwin\n", "Step 3/20 : FROM ${BASE}:latest\n ---> Running in " + testID + "\n", `out: echo "$GREETING"`, "Step 20/20 : CMD [\"--help\"]\n", "Successfully built myimg:1\n"} {
		if !strings.Contains(out, s) {
			t.Errorf("stdout lacks %q:\n%s", s, out)
		}
	}
	for _, s := range []string{"build-args [UNUSED] were not consumed", "ADD copies archive.tar as a file"} {
		if !strings.Contains(errw, s) {
			t.Errorf("stderr lacks %q:\n%s", s, errw)
		}
	}
}

func TestBuildFailingStep(t *testing.T) {
	for _, noRm := range []bool{false, true} {
		f := newFake(t)
		f.exit["false"] = 3
		_, out, _, err := runBuild(t, f, nil, "FROM darwin\nRUN false\nRUN never\n", func(o *Options) { o.NoRm = noRm })
		se, ok := errors.AsType[*StepError](err)
		if !ok || se.ExitStatus != 3 || se.Step != 2 || se.Steps != 3 || se.Line != 2 || se.Instruction != "RUN false" {
			t.Fatalf("got %#v", err)
		}
		if len(f.commits) != 0 || strings.Contains(out, "never") {
			t.Errorf("the build went on after a failed step: %v", f.calls)
		}
		if removed := len(f.removed) == 1; removed == noRm {
			t.Errorf("noRm=%v: removed %v", noRm, f.removed)
		}
	}
}

func TestBuildProgramNotFound(t *testing.T) {
	f := newFake(t)
	f.exit["nope"], f.execErr["nope"] = 127, errors.New("executable file not found")
	_, _, _, err := runBuild(t, f, nil, "FROM darwin\nRUN [\"nope\"]\n", nil)
	if se, ok := errors.AsType[*StepError](err); !ok || se.ExitStatus != 127 || !strings.Contains(se.Error(), "not found") {
		t.Fatalf("got %v", err)
	}
	if len(f.removed) != 1 {
		t.Error("the build container was not removed")
	}
}

func TestBuildCancelledStillRemoves(t *testing.T) {
	f := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.onRun = cancel
	f.execErr["sleep 100"] = context.Canceled
	dir := t.TempDir()
	_, err := Build(ctx, f, Options{ContextDir: dir, Dockerfile: []byte("FROM darwin\nRUN sleep 100\n"), Tag: "x"})
	if !errors.Is(err, context.Canceled) || len(f.removed) != 1 {
		t.Fatalf("err %v, removed %v", err, f.removed)
	}
}

func TestBuildFailsBeforeCreate(t *testing.T) {
	for _, tc := range []struct{ df, want string }{
		{"FROM scratch\n", "FROM scratch is not supported"},
		{"FROM missing\n", "image missing:latest not found"},
		{"FROM myimg:1\n", "also its base image"},
		{"FROM darwin\nFROBNICATE\n", "line 2: FROBNICATE: unknown instruction"},
		{"FROM --platform=linux/amd64 darwin\n", "darwin/arm64 images only"},
		{"FROM darwin AS a\nCOPY --from=a x y\n", "line 2: COPY: --from is not supported"},
		{"FROM darwin\nCOPY ../x /x\n", `line 2: COPY: ../x: ".." is not allowed`},
		{"FROM darwin\nCOPY a \"/etc/passwd\" /x/\n", "absolute source paths are not allowed"},
		{"FROM darwin\nADD https://example.com/x /x\n", "ADD from a URL is not supported"},
	} {
		f := newFake(t)
		_, _, _, err := runBuild(t, f, nil, tc.df, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.df, err, tc.want)
		}
		if len(f.created) != 0 {
			t.Errorf("%q: created a container", tc.df)
		}
	}
}

func TestBuildStepErrors(t *testing.T) {
	for _, tc := range []struct{ df, want string }{
		{"FROM darwin\nCOPY a b /x\n", "must be a directory and end with a /"},
		{"FROM darwin\nCOPY --chown=-r a /x\n", "--chown=-r"},
		{"FROM darwin\nCOPY --chmod=999 a /x\n", "--chmod=999"},
		{"FROM darwin\nUSER \"a b\"\n", "invalid user"},
		{"FROM darwin\nEXPOSE 70000\n", "invalid port"},
		{"FROM darwin\nEXPOSE 80/sctp\n", "tcp or udp"},
		{"FROM darwin\nCOPY missing /x\n", "not found in the build context"},
		{"FROM darwin\nENV P=..\nCOPY $P/a /x\n", `(line 3) COPY: ../a: ".." is not allowed`},
		{"FROM darwin\nCOPY out /x\n", "out: a symlink leads out of the build context"},
	} {
		f := newFake(t)
		_, _, _, err := runBuild(t, f, map[string]string{"a": "a", "b": "b", "out": "->/etc/hosts"}, tc.df, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "(line ") {
			t.Errorf("%q: got %v, want %q", tc.df, err, tc.want)
		}
		if len(f.commits) != 0 || len(f.removed) != 1 {
			t.Errorf("%q: commits %v removed %v", tc.df, f.commits, f.removed)
		}
	}
}

func TestBuildConfigInheritance(t *testing.T) {
	f := newFake(t)
	if _, _, _, err := runBuild(t, f, nil, "FROM darwin\nENTRYPOINT /bin/tool -x\n", nil); err != nil {
		t.Fatal(err)
	}
	cfg := f.commits[0].cfg
	if cfg.Cmd != nil || !reflect.DeepEqual(cfg.Entrypoint, []string{"/bin/sh", "-c", "/bin/tool -x"}) || cfg.Labels["base"] != "yes" {
		t.Errorf("ENTRYPOINT must reset an inherited CMD: %+v", cfg)
	}
	f = newFake(t)
	if _, _, _, err := runBuild(t, f, nil, "FROM darwin\nCMD [\"a\"]\nENTRYPOINT [\"/x\"]\n", nil); err != nil {
		t.Fatal(err)
	}
	if cfg := f.commits[0].cfg; !reflect.DeepEqual(cfg.Cmd, []string{"a"}) {
		t.Errorf("a CMD of this Dockerfile survives ENTRYPOINT: %+v", cfg)
	}
}

func TestBuildCopyFileDestinations(t *testing.T) {
	f := newFake(t)
	f.dirs["/etc"] = true
	df := "FROM darwin\nCOPY f /etc\nCOPY f /etc/renamed\nCOPY f* /multi/\nWORKDIR /w\nCOPY f rel\n"
	if _, _, errw, err := runBuild(t, f, map[string]string{"f": "1", "f2": "2"}, df, nil); err != nil {
		t.Fatal(err, errw)
	}
	var got []string
	for _, c := range f.copies {
		for _, e := range c.entries {
			got = append(got, filepath.Join(c.dir, e.Name))
		}
	}
	want := []string{"/etc/f", "/etc/renamed", "/multi/f", "/multi/f2", "/w/rel"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestBuildArgScopes(t *testing.T) {
	f := newFake(t)
	df := `ARG TAG=latest
ARG ONLY_GLOBAL=g
FROM darwin:$TAG
ARG TAG
ARG TARGETARCH
ARG LATE=${ONLY_GLOBAL:-unset}
ENV TAG=fromenv
RUN x
`
	if _, _, errw, err := runBuild(t, f, nil, df, nil); err != nil {
		t.Fatal(err, errw)
	}
	want := []string{"PATH=/usr/bin:/bin", "TAG=fromenv", "TARGETARCH=arm64", "LATE=unset"}
	if got := f.runs[0].Env; !reflect.DeepEqual(got, want) {
		t.Fatalf("env %v want %v", got, want)
	}
}

func TestBuildChownBatches(t *testing.T) {
	f := newFake(t)
	files := map[string]string{}
	for i := range 1500 {
		files[fmt.Sprintf("tree/file-with-a-rather-long-name-%04d", i)] = ""
	}
	files["tree/zz-link"] = "->/etc/master.passwd"
	if _, _, errw, err := runBuild(t, f, files, "FROM darwin\nCOPY --chown=app:staff tree /data\n", nil); err != nil {
		t.Fatal(err, errw)
	}
	if n := len(f.copies[0].entries); n != 1501 {
		t.Fatalf("copied %d entries", n)
	}
	seen := map[string]int{}
	batches := 0
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "/bin/chown app:staff ") {
			continue
		}
		batches++
		for _, p := range strings.Fields(c)[2:] {
			seen[p]++
		}
	}
	if batches < 2 || len(seen) != 1500 {
		t.Fatalf("%d batches, %d paths", batches, len(seen))
	}
	for _, n := range f.chownJSON {
		if n >= proto.MaxLine-4<<10 {
			t.Errorf("chown request of %d bytes against a %d byte limit", n, proto.MaxLine)
		}
	}
	for p, n := range seen {
		if n != 1 || !strings.HasPrefix(p, "/data/file-with") {
			t.Fatalf("%s chowned %d times", p, n)
		}
	}
}

func TestBuildDefaultDockerfileAndTag(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM darwin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	img, err := Build(context.Background(), f, Options{ContextDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(img.Name, "build-") || !strings.HasSuffix(img.Name, ":latest") {
		t.Fatalf("generated tag %q", img.Name)
	}
}

func TestBuildDefaultPath(t *testing.T) {
	f := newFake(t)
	f.images = append(f.images, &engine.Image{Name: "bare", Tag: "latest"})
	if _, _, errw, err := runBuild(t, f, nil, "FROM bare\nENV PATH=/opt/bin:$PATH\nRUN x\n", nil); err != nil {
		t.Fatal(err, errw)
	}
	want := []string{"PATH=/opt/bin:" + defaultPath}
	if got := f.commits[0].cfg.Env; !reflect.DeepEqual(got, want) {
		t.Fatalf("env %v want %v", got, want)
	}
	if got := f.runs[0].Env; !reflect.DeepEqual(got, want) {
		t.Fatalf("RUN env %v want %v", got, want)
	}
}

func TestBuildCreateFailsAfterAllocating(t *testing.T) {
	f := newFake(t)
	f.createErr = errors.New("boot interrupted")
	if _, _, _, err := runBuild(t, f, nil, "FROM darwin\nRUN x\n", nil); err == nil || !strings.Contains(err.Error(), "boot interrupted") {
		t.Fatalf("got %v", err)
	}
	if !reflect.DeepEqual(f.removed, []string{testID}) {
		t.Fatalf("a container that failed to boot was not removed: %v", f.removed)
	}
}

func TestBuildBaseConfigChecks(t *testing.T) {
	f := newFake(t)
	f.images = append(f.images, &engine.Image{Name: "rel", Tag: "latest", Config: engine.ImageConfig{WorkingDir: "app"}})
	if _, _, errw, err := runBuild(t, f, nil, "FROM rel\nWORKDIR sub\n", nil); err != nil {
		t.Fatal(err, errw)
	}
	if !slices.Contains(f.calls, "mkdir /app/sub owner=") || f.commits[0].cfg.WorkingDir != "/app/sub" {
		t.Fatalf("calls %v", f.calls)
	}
	f = newFake(t)
	f.images = append(f.images, &engine.Image{Name: "baduser", Tag: "latest", Config: engine.ImageConfig{User: "-R"}})
	if _, _, _, err := runBuild(t, f, nil, "FROM baduser\nWORKDIR /x\n", nil); err == nil || len(f.created) != 0 {
		t.Fatalf("an invalid image user must stop the build before a VM starts: %v", err)
	}
}

func TestBuildOptionalWildcard(t *testing.T) {
	f := newFake(t)
	if _, _, errw, err := runBuild(t, f, map[string]string{"go.mod": "m"}, "FROM darwin\nWORKDIR /src\nCOPY go.mod go.sum* ./\n", nil); err != nil {
		t.Fatal(err, errw)
	}
	if len(f.copies) != 1 || len(f.copies[0].entries) != 1 || f.copies[0].entries[0].Name != "go.mod" {
		t.Fatalf("copies %+v", f.copies)
	}
	f = newFake(t)
	if _, _, _, err := runBuild(t, f, nil, "FROM darwin\nCOPY nothing* /x/\n", nil); err == nil || !strings.Contains(err.Error(), "no source files were specified") {
		t.Fatalf("got %v", err)
	}
}

func TestBuildArgOverridesDefault(t *testing.T) {
	f := newFake(t)
	_, _, errw, err := runBuild(t, f, nil, "FROM darwin\nARG X=1\nRUN x\n", func(o *Options) { o.BuildArgs = map[string]string{"X": "2"} })
	if err != nil {
		t.Fatal(err, errw)
	}
	if !slices.Contains(f.runs[0].Env, "X=2") || strings.Contains(errw, "not consumed") {
		t.Fatalf("env %v, stderr %q", f.runs[0].Env, errw)
	}
}

func TestBuildWorkdirAndCopyModes(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"secrets/k": "k", "f": "f", "Dockerfile": "FROM darwin\nUSER app\nWORKDIR /w\nCOPY secrets /secrets\nCOPY --chmod=750 secrets /s2\nCOPY f /files/\nUSER 1000\nWORKDIR /n\n"})
	if err := os.Chmod(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), f, Options{ContextDir: dir, Tag: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mkdir /w owner=app", "mkdir /n owner=1000:1000", "mkdir /secrets owner= mode=700", "mkdir /s2 owner= mode=750", "mkdir /files owner="} {
		if !slices.Contains(f.calls, want) {
			t.Errorf("no %q in %v", want, f.calls)
		}
	}
}

// TestMkdirScript runs the guest helper with the host's /bin/sh and with zsh started as sh, which
// is what the guest's /bin/sh is.
func TestMkdirScript(t *testing.T) {
	script := strings.Replace(mkdirScript, "/bin/chown", "echo", 1)
	shells := [][]string{{"/bin/sh", "sh"}}
	if _, err := os.Stat("/bin/zsh"); err == nil {
		shells = append(shells, []string{"/bin/zsh", "sh"})
	}
	for _, sh := range shells {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "a"), 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, "a", "b", "c")
		run := func(args ...string) (string, error) {
			cmd := exec.Command(sh[0], append([]string{"-c", script, "sh"}, args...)...)
			cmd.Args[0] = sh[1]
			out, err := cmd.CombinedOutput()
			return string(out), err
		}
		out, err := run(target, "app:staff", "700")
		if err != nil {
			t.Fatalf("%s: %v %s", sh[0], err, out)
		}
		want := fmt.Sprintf("app:staff %s %s\n", target, filepath.Dir(target))
		if out != want {
			t.Errorf("%s: chown %q, want %q", sh[0], out, want)
		}
		if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", sh[0], fi, err)
		}
		if out, err := run(target, "app", "755"); err != nil || out != "" {
			t.Errorf("%s: an existing directory is left alone: %q %v", sh[0], out, err)
		}
		if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: the mode of an existing directory changed", sh[0])
		}
		if _, err := run("rel/dir", "", ""); err == nil {
			t.Errorf("%s: a relative path must fail", sh[0])
		}
	}
}

func TestTailBufferKeepsLastBytes(t *testing.T) {
	tb := &tailBuffer{max: 4}
	for _, s := range []string{"ab", "cdef", "", "g"} {
		if n, err := tb.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got := tb.String(); got != "defg" {
		t.Fatalf("got %q, want %q", got, "defg")
	}
}
