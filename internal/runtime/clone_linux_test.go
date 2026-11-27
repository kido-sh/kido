package runtime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCloneFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.img")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.img")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.img")
	err := cloneFile(link, dst)
	if err == nil {
		t.Fatal("cloneFile followed symlink src")
	}
	if !errors.Is(err, unix.ELOOP) {
		t.Fatalf("cloneFile symlink err = %v, want ELOOP", err)
	}
	if _, statErr := os.Lstat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("dst exists after refused symlink clone: %v", statErr)
	}
}

func TestCloneFileRegular(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img")
	want := []byte("clone payload")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.img")
	if err := cloneFile(src, dst); err != nil {
		t.Fatalf("cloneFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("cloned content = %q, want %q", got, want)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("cloned mode = %o, want 0600", fi.Mode().Perm())
	}
}
