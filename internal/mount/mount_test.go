package mount

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/engine"
)

const testCID = "0123456789ab"

type ent struct {
	name, body, link string
	typ              byte
	mode             int64
}

// boot saves m for a fake container, packs it like a boot does, and returns the container dir.
func boot(t *testing.T, base string, m engine.Mount) string {
	t.Helper()
	dir := filepath.Join(base, "home", "containers", testCID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := Resolve(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, []engine.Mount{m}); err != nil {
		t.Fatal(err)
	}
	if _, err := QEMUArgs(dir); err != nil {
		t.Fatal(err)
	}
	st, _ := load(dir)
	st.Mounts[0].Populated = true
	save(dir, st)
	return dir
}

// guestSends writes entries as the guest's tar onto mount 0's out disk and marks it collected.
func guestSends(t *testing.T, dir string, entries []ent) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, diskDir, "out-0.img"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Seek(headerSize, io.SeekStart)
	tw := tar.NewWriter(f)
	for _, e := range entries {
		typ, mode := e.typ, e.mode
		if typ == 0 {
			typ = tar.TypeReg
		}
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Linkname: e.link, Size: int64(len(e.body)), ModTime: time.Now()}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, e.body)
	}
	tw.Close()
	st, _ := load(dir)
	st.Mounts[0].Collected = true
	save(dir, st)
}

func write(t *testing.T, p, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func TestApplyCopiesBackAndKeepsConflicts(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	for n, b := range map[string]string{"keep.txt": "k", "edit.txt": "e1", "both.txt": "b1", "del.txt": "d", "sub/old.txt": "o"} {
		write(t, filepath.Join(src, n), b)
	}
	dir := boot(t, base, engine.Mount{Source: src, Target: "/work"})
	write(t, filepath.Join(src, "both.txt"), "b-host")
	os.Chtimes(filepath.Join(src, "both.txt"), time.Now(), time.Now().Add(time.Hour))
	guestSends(t, dir, []ent{
		{name: "./", typ: tar.TypeDir, mode: 0o755},
		{name: "./keep.txt", body: "k"},
		{name: "./edit.txt", body: "e2"},
		{name: "./both.txt", body: "b-guest"},
		{name: "./new.txt", body: "n"},
		{name: "./sub/", typ: tar.TypeDir, mode: 0o755},
		{name: "./sub/old.txt", body: "o"},
		{name: "./newdir/n2.txt", body: "n2"},
	})
	var warn bytes.Buffer
	Apply(dir, &warn)
	for n, want := range map[string]string{
		"keep.txt": "k", "edit.txt": "e2", "both.txt": "b-host", "both.txt.kido-" + testCID: "b-guest",
		"new.txt": "n", "newdir/n2.txt": "n2", "del.txt": "d", "sub/old.txt": "o",
	} {
		if got := read(t, filepath.Join(src, n)); got != want {
			t.Errorf("%s = %q, want %q", n, got, want)
		}
	}
	w := warn.String()
	if !strings.Contains(w, "changed on the host and in the container") || !strings.Contains(w, "1 paths deleted") {
		t.Errorf("warnings:\n%s", w)
	}
	if _, err := os.Stat(filepath.Join(dir, diskDir, "out-0.img")); !os.IsNotExist(err) {
		t.Errorf("out disk left behind: %v", err)
	}
	ents, _ := os.ReadDir(src)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".kido-tmp-") {
			t.Errorf("temp file left: %s", e.Name())
		}
	}
}

func TestApplyHostileTar(t *testing.T) {
	base := t.TempDir()
	src, outside := filepath.Join(base, "src"), filepath.Join(base, "outside")
	os.MkdirAll(outside, 0o755)
	write(t, filepath.Join(src, "a.txt"), "a")
	os.Symlink(outside, filepath.Join(src, "evil"))
	dir := boot(t, base, engine.Mount{Source: src, Target: "/work"})
	guestSends(t, dir, []ent{
		{name: "/etc/abs", body: "x"},
		{name: "../escape", body: "x"},
		{name: "a/../../escape2", body: "x"},
		{name: "evil/through", body: "x"},
		{name: "abs-link", typ: tar.TypeSymlink, link: "/etc/passwd"},
		{name: "up-link", typ: tar.TypeSymlink, link: "../../outside"},
		{name: "in-link", typ: tar.TypeSymlink, link: "a.txt"},
		{name: "chr", typ: tar.TypeChar},
		{name: "fifo", typ: tar.TypeFifo},
		{name: "hard", typ: tar.TypeLink, link: "a.txt"},
		{name: "suid", body: "s", mode: 0o4755},
	})
	var warn bytes.Buffer
	Apply(dir, &warn)
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("wrote outside the source: %v", ents)
	}
	for _, p := range []string{filepath.Join(base, "escape"), filepath.Join(base, "escape2"), filepath.Join(src, "chr"),
		filepath.Join(src, "fifo"), filepath.Join(src, "hard"), filepath.Join(src, "abs-link"), filepath.Join(src, "up-link")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was created", p)
		}
	}
	if l, err := os.Readlink(filepath.Join(src, "in-link")); err != nil || l != "a.txt" {
		t.Errorf("in-link = %q, %v", l, err)
	}
	if fi, err := os.Stat(filepath.Join(src, "suid")); err != nil || fi.Mode() != 0o755 {
		t.Errorf("suid mode = %v, %v", fi, err)
	}
	if w := warn.String(); !strings.Contains(w, "skipped 3 entries") || !strings.Contains(w, "points outside the mount") {
		t.Errorf("warnings:\n%s", w)
	}
}

func TestApplyBudget(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	os.MkdirAll(src, 0o755)
	dir := boot(t, base, engine.Mount{Source: src, Target: "/work"})
	guestSends(t, dir, []ent{{name: "big", body: strings.Repeat("x", 2000)}})
	defer func(n int64) { copyBackLimit = n }(copyBackLimit)
	copyBackLimit = 1000
	var warn bytes.Buffer
	Apply(dir, &warn)
	if _, err := os.Stat(filepath.Join(src, "big")); err == nil || !strings.Contains(warn.String(), "copy-back limit") {
		t.Errorf("budget not enforced: %v\n%s", err, warn.String())
	}
}

func TestFileMountAppliesOnlyTheFile(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "cfg", "app.conf")
	write(t, src, "v1")
	dir := boot(t, base, engine.Mount{Source: src, Target: "/etc/app/settings.conf"})
	guestSends(t, dir, []ent{{name: "./settings.conf", body: "v2"}, {name: "./other", body: "x"}})
	var warn bytes.Buffer
	Apply(dir, &warn)
	if got := read(t, src); got != "v2" {
		t.Errorf("file = %q", got)
	}
	if _, err := os.Stat(filepath.Join(base, "cfg", "other")); err == nil {
		t.Error("other was created")
	}
}

func TestPackKeepsSymlinksAndSkipsHome(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "home", "containers", testCID, "token"), "secret")
	os.Symlink("/etc/passwd", filepath.Join(base, "link"))
	dir := filepath.Join(base, "home", "containers", testCID)
	if _, _, _, err := packDisk(filepath.Join(base, "rw.img"), header("t", "in", 0), base, "/w", filepath.Join(base, "home"), false); err == nil {
		t.Error("rw mount of a source holding KIDO_HOME was packed")
	}
	img := filepath.Join(dir, "in.img")
	man, _, _, err := packDisk(img, header("t", "in", 0), base, "/w", filepath.Join(base, "home"), true)
	if err != nil {
		t.Fatal(err)
	}
	if e := man["link"]; e.Kind != 'l' || e.Link != "/etc/passwd" {
		t.Errorf("link entry = %+v", e)
	}
	f, _ := os.Open(img)
	defer f.Close()
	f.Seek(headerSize, io.SeekStart)
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if strings.Contains(h.Name, "home") {
			t.Errorf("packed %s", h.Name)
		}
		if h.Name == "./link" && (h.Typeflag != tar.TypeSymlink || h.Size != 0) {
			t.Errorf("link packed as %c size %d", h.Typeflag, h.Size)
		}
	}
}

func TestParse(t *testing.T) {
	good := map[string]engine.Mount{
		"./a:/work":       {Source: "./a", Target: "/work"},
		"/a:/w:ro":        {Source: "/a", Target: "/w", ReadOnly: true},
		"/a:/w:rw,cached": {Source: "/a", Target: "/w"},
	}
	for s, want := range good {
		if got, err := ParseVolume(s); err != nil || got != want {
			t.Errorf("ParseVolume(%q) = %+v, %v", s, got, err)
		}
	}
	for _, s := range []string{"name:/w", "/a", "/a:/w:z", "/a:/w:ro:x"} {
		if _, err := ParseVolume(s); err == nil {
			t.Errorf("ParseVolume(%q) succeeded", s)
		}
	}
	if m, err := ParseMount("type=bind,src=/a,dst=/w,readonly"); err != nil || !m.ReadOnly || m.Target != "/w" {
		t.Errorf("ParseMount = %+v, %v", m, err)
	}
	if _, err := ParseMount("type=volume,src=x,dst=/w"); err == nil {
		t.Error("type=volume accepted")
	}
	for _, tg := range []string{"w", "/", "/usr", "/a/../b", "/dev/x", "/System/x", "/tmp", "/a\nb"} {
		if _, err := checkTarget(tg); err == nil {
			t.Errorf("target %q accepted", tg)
		}
	}
	if err := Check([]engine.Mount{{Target: "/a"}, {Target: "/a/b"}}); err == nil {
		t.Error("nested targets accepted")
	}
}

func TestApplyRemovesLinksThatResolveOutside(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	write(t, filepath.Join(src, "a.txt"), "a")
	dir := boot(t, base, engine.Mount{Source: src, Target: "/work"})
	guestSends(t, dir, []ent{
		{name: "L2", typ: tar.TypeSymlink, link: "t/.."},
		{name: "s", typ: tar.TypeSymlink, link: "."},
		{name: "L", typ: tar.TypeSymlink, link: "s/.."},
		{name: "M", typ: tar.TypeSymlink, link: "s/../x"},
		{name: "t", typ: tar.TypeSymlink, link: "."},
		{name: "ok", typ: tar.TypeSymlink, link: "s/a.txt"},
	})
	var warn bytes.Buffer
	Apply(dir, &warn)
	for _, n := range []string{"L", "L2", "M"} {
		if l, err := os.Readlink(filepath.Join(src, n)); err == nil {
			t.Errorf("%s -> %s was kept", n, l)
		}
	}
	for _, n := range []string{"s", "t", "ok"} {
		if _, err := os.Readlink(filepath.Join(src, n)); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if !strings.Contains(warn.String(), "resolves outside the mount") {
		t.Errorf("warnings:\n%s", warn.String())
	}
}

func TestWarningsEscapeGuestNames(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	os.MkdirAll(src, 0o755)
	dir := boot(t, base, engine.Mount{Source: src, Target: "/work"})
	guestSends(t, dir, []ent{
		{name: "x\x1b]0;pwned\x07", typ: tar.TypeSymlink, link: "/\x1b[2J"},
		{name: "/\x1b[31mabs", body: "x"},
	})
	var warn bytes.Buffer
	Apply(dir, &warn)
	if w := warn.String(); strings.ContainsAny(w, "\x1b\x07") || !strings.Contains(w, `\x1b`) {
		t.Errorf("unescaped or missing warning: %q", w)
	}
}

func TestFailedApplyKeepsOneTarPerMount(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	os.MkdirAll(src, 0o755)
	dir := boot(t, base, engine.Mount{Source: src, Target: "/work"})
	for range 3 {
		f, _ := os.OpenFile(filepath.Join(dir, diskDir, "out-0.img"), os.O_WRONLY|os.O_CREATE, 0o600)
		f.WriteAt(bytes.Repeat([]byte("garbage!"), 200), headerSize)
		f.Close()
		st, _ := load(dir)
		st.Mounts[0].Collected = true
		save(dir, st)
		var warn bytes.Buffer
		Apply(dir, &warn)
		if !strings.Contains(warn.String(), "not all copied back") {
			t.Fatalf("warnings: %s", warn.String())
		}
	}
	ents, _ := os.ReadDir(filepath.Join(dir, diskDir))
	kept := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "unapplied-") {
			kept++
		}
	}
	if kept != 1 {
		t.Errorf("%d unapplied tars kept, want 1", kept)
	}
}
