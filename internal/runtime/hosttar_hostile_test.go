package runtime

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractToHostPinsHostileEntriesUnderRename(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add(&tar.Header{Name: "./.zshrc", Typeflag: tar.TypeReg, Mode: 0o755}, "evil")
	add(&tar.Header{Name: "./.git/hooks/pre-commit", Typeflag: tar.TypeReg, Mode: 0o755}, "evil")
	add(&tar.Header{Name: "x/..", Typeflag: tar.TypeDir, Mode: 0o777}, "")
	tw.Close()

	dir := t.TempDir()
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = ExtractToHost(&buf, dir, "x")

	for _, p := range []string{".zshrc", ".git"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Fatalf("%s was created outside the renamed entry", p)
		}
	}
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode() != before.Mode() {
		t.Fatalf("destination mode changed from %v to %v", before.Mode(), after.Mode())
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "x" {
			t.Fatalf("unexpected entry %q in the destination", e.Name())
		}
	}
}

func TestExtractToHostKeepsDestinationModeForRootCopy(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "x/..", Typeflag: tar.TypeDir, Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	dir := t.TempDir()
	before, _ := os.Stat(dir)
	if err := ExtractToHost(&buf, dir, "."); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(dir)
	if after.Mode() != before.Mode() {
		t.Fatalf("destination mode changed from %v to %v", before.Mode(), after.Mode())
	}
}
