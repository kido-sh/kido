package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCappedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	w, err := OpenCappedLog(path)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	total := 0
	for total < 5*LogMax+123 {
		n, err := w.Write(chunk)
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	w.Close()
	for _, p := range []string{path, path + ".1"} {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() > LogMax || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
	if fi, _ := os.Stat(path + ".1"); fi.Size() != LogMax {
		t.Fatalf("rotated file holds %d bytes, want %d", fi.Size(), LogMax)
	}
}
