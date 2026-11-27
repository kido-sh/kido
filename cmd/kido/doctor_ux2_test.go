package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckHomeLongPathWarns(t *testing.T) {
	home := filepath.Join(t.TempDir(), strings.Repeat("d", maxHomeLen))
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if r := checkHome(home, "qemu"); r.status != checkWarn || !strings.Contains(r.detail, "commit") {
		t.Fatalf("got %v %q", r.status, r.detail)
	}
	if r := checkHome(home, "vz"); r.status != checkPass {
		t.Fatalf("vz: got %v %q", r.status, r.detail)
	}
}
