package runtime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCowCreateAndFlatten(t *testing.T) {
	dir := t.TempDir()
	const diskSize = 10*cowBlockSize + 512

	baseData := make([]byte, diskSize)
	for i := range baseData {
		baseData[i] = byte((i*31 + 7) & 0xff)
	}
	clear(baseData[4*cowBlockSize : 6*cowBlockSize])

	imgDisk := filepath.Join(dir, "image-disk.img")
	if err := os.WriteFile(imgDisk, baseData, 0o600); err != nil {
		t.Fatal(err)
	}
	baseLink := filepath.Join(dir, "base.img")
	if err := os.Link(imgDisk, baseLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(imgDisk); err != nil {
		t.Fatal(err)
	}

	cowPath := filepath.Join(dir, "disk.cow")
	if err := createCowOverlay(cowPath, diskSize); err != nil {
		t.Fatalf("createCowOverlay: %v", err)
	}

	cowFile, err := os.OpenFile(cowPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var hdr [cowHeaderSize]byte
	if _, err := cowFile.ReadAt(hdr[:], 0); err != nil {
		t.Fatal(err)
	}
	if string(hdr[0:8]) != cowMagic {
		t.Fatalf("magic = %q, want %q", hdr[0:8], cowMagic)
	}
	dataOff := int64(binary.LittleEndian.Uint64(hdr[24:32]))
	if dataOff != cowBlockSize {
		t.Fatalf("dataOffset = %d, want %d", dataOff, cowBlockSize)
	}

	want := append([]byte(nil), baseData...)
	setDirty := func(blk int64, data []byte) {
		off := blk * cowBlockSize
		if _, err := cowFile.WriteAt(data, dataOff+off); err != nil {
			t.Fatal(err)
		}
		var b [1]byte
		if _, err := cowFile.ReadAt(b[:], cowHeaderSize+blk/8); err != nil {
			t.Fatal(err)
		}
		b[0] |= 1 << uint(blk%8)
		if _, err := cowFile.WriteAt(b[:], cowHeaderSize+blk/8); err != nil {
			t.Fatal(err)
		}
		copy(want[off:off+int64(len(data))], data)
	}

	blk1 := bytes.Repeat([]byte{0xA5}, cowBlockSize)
	setDirty(1, blk1)

	blk2 := make([]byte, cowBlockSize)
	setDirty(2, blk2)

	blk5 := bytes.Repeat([]byte{0x5A}, cowBlockSize)
	setDirty(5, blk5)

	blk10 := bytes.Repeat([]byte{0xCC}, 512)
	setDirty(10, blk10)

	if err := cowFile.Close(); err != nil {
		t.Fatal(err)
	}

	flatPath := filepath.Join(dir, "flat.img")
	if err := flattenCowOverlay(baseLink, cowPath, flatPath); err != nil {
		t.Fatalf("flattenCowOverlay: %v", err)
	}
	got, err := os.ReadFile(flatPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		for i := range want {
			if i >= len(got) || got[i] != want[i] {
				t.Fatalf("flattened mismatch at offset %d (len got=%d want=%d): got 0x%02x want 0x%02x", i, len(got), len(want), got[i], want[i])
			}
		}
		t.Fatalf("flattened length mismatch: got %d want %d", len(got), len(want))
	}
	fi, err := os.Stat(flatPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("flattened mode = %o, want 0600", fi.Mode().Perm())
	}
}

func TestCowRejectsSymlinkAndBadHeaders(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.img")
	if err := os.WriteFile(base, make([]byte, 2*cowBlockSize), 0o600); err != nil {
		t.Fatal(err)
	}
	cow := filepath.Join(dir, "disk.cow")
	if err := createCowOverlay(cow, 2*cowBlockSize); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "link.cow")
	if err := os.Symlink(cow, link); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.img")
	if err := flattenCowOverlay(base, link, dst); !errors.Is(err, unix.ELOOP) {
		t.Fatalf("symlink cow err = %v, want ELOOP", err)
	}

	bad := filepath.Join(dir, "bad.cow")
	if err := os.WriteFile(bad, []byte("NOTACOW1"+string(make([]byte, 64))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := flattenCowOverlay(base, bad, dst); err == nil {
		t.Fatal("flattenCowOverlay accepted bad magic")
	}
	if err := createCowOverlay(filepath.Join(dir, "unaligned.cow"), 513); err == nil {
		t.Fatal("createCowOverlay accepted unaligned disk size")
	}
}
