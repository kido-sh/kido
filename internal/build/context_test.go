package build

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeTree creates files under dir: a name ending in "/" is a directory, "->" makes a symlink.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.HasSuffix(name, "/"):
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		case strings.HasPrefix(content, "->"):
			if err := os.Symlink(strings.TrimPrefix(content, "->"), p); err != nil {
				t.Fatal(err)
			}
		default:
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func openTestContext(t *testing.T, files map[string]string) (*Context, string) {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, files)
	c, err := OpenContext(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, dir
}

func TestResolveRefusesEscapes(t *testing.T) {
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"secret": "s", "d/secret": "s"})
	c, _ := openTestContext(t, map[string]string{
		"ok":          "x",
		"dir/f":       "y",
		"out-file":    "->" + filepath.Join(outside, "secret"),
		"out-dir":     "->" + filepath.Join(outside, "d"),
		"out-rel":     "->../" + filepath.Base(outside) + "/secret",
		"in-link":     "->dir/f",
		"in-dir-link": "->dir",
	})
	for _, src := range []string{"../x", "dir/../ok", "/etc/passwd", "/ok", "out-file", "out-dir", "out-dir/secret", "out-rel", "missing", ""} {
		if got, err := c.resolve(src); err == nil {
			t.Errorf("resolve(%q) = %v, want an error", src, got)
		}
	}
	for src, isDir := range map[string]bool{"ok": false, ".": true, "./dir/": true, "in-link": false, "in-dir-link": true, "in-dir-link/f": false} {
		got, err := c.resolve(src)
		if err != nil || len(got) != 1 || got[0].info.IsDir() != isDir {
			t.Errorf("resolve(%q) = %v, %v", src, got, err)
		}
	}
}

func TestResolveWildcard(t *testing.T) {
	c, _ := openTestContext(t, map[string]string{
		"a.go": "", "b.go": "", "c.txt": "", "sub/d.go": "", "sub/e.go": "", "ignored.go": "",
		".kidoignore": "ignored.go\n",
	})
	got, err := c.resolve("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.rel)
	}
	if want := []string{"a.go", "b.go"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("*.go: got %v want %v", names, want)
	}
	if got, err = c.resolve("sub/*.go"); err != nil || len(got) != 2 {
		t.Fatalf("sub/*.go: %v %v", got, err)
	}
	if got, err := c.resolve("*.rs"); err != nil || len(got) != 0 {
		t.Fatalf("a wildcard may match nothing: %v %v", got, err)
	}
	if _, err := c.resolve("ignored.go"); err == nil || !strings.Contains(err.Error(), ".kidoignore") {
		t.Fatalf("an ignored source: %v", err)
	}
}

type tarEntry struct {
	Name     string
	Type     byte
	Mode     int64
	Uid, Gid int
	Link     string
	Body     string
}

func readTar(t *testing.T, r io.Reader) []tarEntry {
	t.Helper()
	var out []tarEntry
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tarEntry{hdr.Name, hdr.Typeflag, hdr.Mode, hdr.Uid, hdr.Gid, hdr.Linkname, string(body)})
	}
}

func TestEntriesAndTar(t *testing.T) {
	c, dir := openTestContext(t, map[string]string{
		"src/bin/tool":       "#!/bin/sh\n",
		"src/lib/a.txt":      "a",
		"src/link":           "->lib/a.txt",
		"src/abs-link":       "->/etc/hosts",
		"src/cache/x":        "x",
		"src/cache/keep.txt": "k",
		"src/notes.md":       "n",
		".kidoignore":        "src/cache\n!src/cache/keep.txt\n*/*.md\n",
	})
	if err := os.Chmod(filepath.Join(dir, "src/bin/tool"), 0o755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "src/cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	srcs, err := c.resolve("src")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := c.entries(srcs[0], "src")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := c.writeTar(&buf, entries, -1); err != nil {
		t.Fatal(err)
	}
	want := []tarEntry{
		{Name: "abs-link", Type: tar.TypeSymlink, Mode: 0o777, Link: "/etc/hosts"},
		{Name: "bin/", Type: tar.TypeDir, Mode: 0o755},
		{Name: "bin/tool", Type: tar.TypeReg, Mode: 0o4755, Body: "#!/bin/sh\n"},
		{Name: "cache/", Type: tar.TypeDir, Mode: 0o700},
		{Name: "cache/keep.txt", Type: tar.TypeReg, Mode: 0o644, Body: "k"},
		{Name: "lib/", Type: tar.TypeDir, Mode: 0o755},
		{Name: "lib/a.txt", Type: tar.TypeReg, Mode: 0o644, Body: "a"},
		{Name: "link", Type: tar.TypeSymlink, Mode: 0o777, Link: "lib/a.txt"},
	}
	if got := readTar(t, &buf); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}

	file, err := c.resolve("src/lib/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	entries, err = c.entries(file[0], "renamed")
	if err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := c.writeTar(&buf, entries, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readTar(t, &buf); !reflect.DeepEqual(got, []tarEntry{{Name: "renamed", Type: tar.TypeReg, Mode: 0o600, Body: "a"}}) {
		t.Fatalf("single file with --chmod: %+v", got)
	}
}

func TestTopLevelSymlinkIsFollowed(t *testing.T) {
	c, _ := openTestContext(t, map[string]string{"real/f": "data", "alias": "->real"})
	srcs, err := c.resolve("alias")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := c.entries(srcs[0], "alias")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := c.writeTar(&buf, entries, -1); err != nil {
		t.Fatal(err)
	}
	if got := readTar(t, &buf); len(got) != 1 || got[0].Name != "f" || got[0].Body != "data" {
		t.Fatalf("got %+v", got)
	}
}

func TestSwappedSymlinkCannotEscape(t *testing.T) {
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"secret": "s", "d/f": "s"})
	c, dir := openTestContext(t, map[string]string{"f": "ok", "d/f": "ok"})
	var entries []entry
	for _, src := range []string{"f", "d"} {
		srcs, err := c.resolve(src)
		if err != nil {
			t.Fatal(err)
		}
		es, err := c.entries(srcs[0], src)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, es...)
	}
	rel, err := filepath.Rel(dir, outside)
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"f": filepath.Join(rel, "secret"), "d": filepath.Join(rel, "d")} {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range entries {
		if err := c.writeTar(io.Discard, []entry{e}, -1); err == nil {
			t.Errorf("%s swapped for a relative symlink out of the context was read", e.rel)
		}
	}
}

func TestIgnoreAppliesThroughSymlinks(t *testing.T) {
	c, _ := openTestContext(t, map[string]string{
		".env": "SECRET", "link": "->.env", "secrets/key": "k", "alias": "->secrets", "ok": "x", "ok-link": "->ok",
		".kidoignore": ".env\nsecrets\n",
	})
	for _, src := range []string{"link", "alias/key", "alias"} {
		if got, err := c.resolve(src); err == nil || !strings.Contains(err.Error(), ".kidoignore") {
			t.Errorf("resolve(%q) = %v, %v; want it excluded", src, got, err)
		}
	}
	got, err := c.resolve("ok-link")
	if err != nil || got[0].rel != "ok" || got[0].base != "ok-link" {
		t.Fatalf("resolve(ok-link) = %+v, %v", got, err)
	}
}

func TestExcludedDirWithUnrelatedExclusion(t *testing.T) {
	c, _ := openTestContext(t, map[string]string{"cache/x": "x", "README.md": "r", ".kidoignore": "cache\n!README.md\n"})
	srcs, err := c.resolve("cache")
	if err == nil {
		_, err = c.entries(srcs[0], "cache")
	}
	if err == nil || !strings.Contains(err.Error(), ".kidoignore") {
		t.Fatalf("an ignored directory with nothing re-included: %v", err)
	}
}

func TestFIFOsDoNotBlock(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, ".kidoignore"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		c, err := OpenContext(dir)
		if err == nil {
			c.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO .kidoignore must be refused")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenContext blocked on a FIFO")
	}
}

func TestKidoignoreSymlinkOutside(t *testing.T) {
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"ignore": "*\n"})
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".kidoignore": "->" + filepath.Join(outside, "ignore")})
	if c, err := OpenContext(dir); err == nil {
		c.Close()
		t.Fatal("a .kidoignore that points out of the context must not be read")
	}
}

func TestIgnoresStrayDockerignore(t *testing.T) {
	c, _ := openTestContext(t, map[string]string{
		"keep.txt":      "k",
		".dockerignore": "keep.txt\n",
	})
	if got, err := c.resolve("keep.txt"); err != nil || len(got) != 1 || got[0].rel != "keep.txt" {
		t.Fatalf("stray .dockerignore must not be read: %v, %v", got, err)
	}

	c2, _ := openTestContext(t, map[string]string{
		"keep.txt":      "k",
		"drop.txt":      "d",
		".dockerignore": "keep.txt\n",
		".kidoignore":   "drop.txt\n",
	})
	if got, err := c2.resolve("keep.txt"); err != nil || len(got) != 1 || got[0].rel != "keep.txt" {
		t.Fatalf("keep.txt must not be excluded by .dockerignore: %v, %v", got, err)
	}
	if _, err := c2.resolve("drop.txt"); err == nil || !strings.Contains(err.Error(), ".kidoignore") {
		t.Fatalf("drop.txt must be excluded by .kidoignore: %v", err)
	}
}

func TestExcludedDirWithReincludedFile(t *testing.T) {
	c, _ := openTestContext(t, map[string]string{
		"cache/x": "x", "cache/keep.txt": "k", "secret": "s",
		".kidoignore": "cache\n!cache/keep.txt\nsecret\n",
	})
	srcs, err := c.resolve("cache")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := c.entries(srcs[0], "cache")
	if err != nil || len(entries) != 1 || entries[0].name != "keep.txt" {
		t.Fatalf("entries %+v, %v", entries, err)
	}
	if _, err := c.resolve("secret"); err == nil {
		t.Fatal("an excluded file must stay excluded")
	}
}

func TestNormalizePort(t *testing.T) {
	for in, want := range map[string]string{"080": "80/tcp", "53/UDP": "53/udp", "8000-8010/tcp": "8000-8010/tcp"} {
		if got, err := normalizePort(in); err != nil || got != want {
			t.Errorf("normalizePort(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"0", "65536", "10-5", "x", "80/sctp", "-1", "80-"} {
		if got, err := normalizePort(bad); err == nil {
			t.Errorf("normalizePort(%q) = %q, want an error", bad, got)
		}
	}
}
