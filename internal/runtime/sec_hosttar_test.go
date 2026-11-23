package runtime

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestExtractToHostSymlinkEscape writes a symlink entry, then a file through it, for link targets
// outside the destination, inside it but outside the copied entry, and absolute.
func TestExtractToHostSymlinkEscape(t *testing.T) {
	for name, target := range map[string]string{
		"outside":  "../../victim",
		"absolute": "",
		"sibling":  "../home",
		"self":     "..",
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			dest := filepath.Join(base, "dest")
			victim := filepath.Join(base, "victim")
			for _, d := range []string{dest, victim, filepath.Join(dest, "home")} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if target == "" {
				target = victim
			}
			data := tarOf(t,
				&tar.Header{Name: "x/", Typeflag: tar.TypeDir, Mode: 0o755},
				&tar.Header{Name: "x/l", Typeflag: tar.TypeSymlink, Linkname: target},
				&tar.Header{Name: "x/l/.zshrc", Typeflag: tar.TypeReg, Mode: 0o644})
			if err := ExtractToHost(bytes.NewReader(data), dest, "x"); err == nil {
				t.Fatal("extraction through a symlink succeeded")
			}
			for _, p := range []string{
				filepath.Join(victim, ".zshrc"),
				filepath.Join(dest, ".zshrc"),
				filepath.Join(dest, "home", ".zshrc"),
				filepath.Join(base, ".zshrc"),
			} {
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Fatalf("%s was written through the symlink", p)
				}
			}
		})
	}
}

// TestExtractToHostExistingSymlink covers a destination entry that is already a symlink, as a
// concurrent writer of a shared destination could plant it.
func TestExtractToHostExistingSymlink(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	victim := filepath.Join(base, "victim")
	os.MkdirAll(dest, 0o700)
	os.MkdirAll(victim, 0o700)
	os.WriteFile(filepath.Join(victim, "f"), []byte("keep"), 0o600)
	if err := os.Symlink(victim, filepath.Join(dest, "x")); err != nil {
		t.Fatal(err)
	}
	data := tarOf(t, &tar.Header{Name: "x/f", Typeflag: tar.TypeReg, Mode: 0o644})
	if err := ExtractToHost(bytes.NewReader(data), dest, "x"); err == nil {
		t.Fatal("extraction into a symlinked directory succeeded")
	}
	if b, _ := os.ReadFile(filepath.Join(victim, "f")); string(b) != "keep" {
		t.Fatalf("victim overwritten: %q", b)
	}

	if err := os.Symlink(filepath.Join(victim, "f"), filepath.Join(dest, "y")); err != nil {
		t.Fatal(err)
	}
	data = tarOf(t, &tar.Header{Name: "y", Typeflag: tar.TypeReg, Mode: 0o777})
	if err := ExtractToHost(bytes.NewReader(data), dest, "y"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(victim, "f")); string(b) != "keep" {
		t.Fatalf("victim overwritten through a final symlink: %q", b)
	}
	fi, err := os.Lstat(filepath.Join(dest, "y"))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o755 {
		t.Fatalf("y: %v %v, want a regular file with mode 0755", fi, err)
	}
}

// TestExtractToHostSwapAfterCheck swaps a checked directory for a symlink out of the destination,
// as a racing local user could, and expects os.Root to stop the write.
func TestExtractToHostSwapAfterCheck(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	victim := filepath.Join(base, "victim")
	os.MkdirAll(dest, 0o700)
	os.MkdirAll(victim, 0o700)
	if err := os.Symlink(victim, filepath.Join(dest, "x")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	x := &extractor{root: root, dirs: map[string]bool{".": true, "x": true}}
	err = x.extract(&tar.Header{Name: "x/f", Typeflag: tar.TypeReg, Mode: 0o644}, "x/f", bytes.NewReader(nil), "x")
	if err == nil {
		t.Fatal("write through a swapped-in symlink succeeded")
	}
	if _, err := os.Lstat(filepath.Join(victim, "f")); !os.IsNotExist(err) {
		t.Fatal("victim/f was created")
	}
}
