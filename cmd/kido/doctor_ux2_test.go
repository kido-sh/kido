package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kido-sh/kido/internal/setup"
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

func TestCheckUnpackedQEMU(t *testing.T) {
	home := t.TempDir()
	if r := checkUnpackedQEMU(home); r != nil {
		t.Fatalf("no qemu dir: got %+v", r)
	}
	bin := filepath.Join(setup.QEMUDir(home), setup.PinnedQEMU.Version, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "qemu-system-aarch64"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := checkUnpackedQEMU(home)
	if len(r) == 0 || r[0].status != checkFail || !strings.Contains(r[0].detail, "is unpacked in") ||
		strings.Contains(r[0].fix, "run `KIDO_BACKEND=qemu kido setup` (installs") {
		t.Fatalf("got %+v", r)
	}
}
