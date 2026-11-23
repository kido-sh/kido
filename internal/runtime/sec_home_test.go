package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRefusesSharedHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if _, err := New(home); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(home); fi.Mode().Perm() != 0o700 {
		t.Fatalf("new home mode %v, want 0700", fi.Mode().Perm())
	}
	os.Chmod(home, 0o777)
	if _, err := New(home); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("group/other-writable home accepted: %v", err)
	}
	os.Chmod(home, 0o755)
	os.Chmod(filepath.Join(home, "containers"), 0o770)
	if _, err := New(home); err == nil {
		t.Fatal("group-writable containers/ accepted")
	}
	os.Chmod(filepath.Join(home, "containers"), 0o700)
	os.Rename(filepath.Join(home, "images"), filepath.Join(home, "real-images"))
	os.Symlink("real-images", filepath.Join(home, "images"))
	if _, err := New(home); err == nil {
		t.Fatal("symlinked images/ accepted")
	}
	if _, err := New("/"); err == nil || !strings.Contains(err.Error(), "owned by uid") && os.Geteuid() != 0 {
		t.Fatalf("home owned by root accepted: %v", err)
	}
}
