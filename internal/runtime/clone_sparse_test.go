package runtime

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestCopySparsePreservesHolesAndContent(t *testing.T) {
	dir := t.TempDir()
	const logicalSize = 72 << 20

	srcPath := filepath.Join(dir, "src.img")
	src, err := os.OpenFile(srcPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Truncate(logicalSize); err != nil {
		src.Close()
		t.Fatal(err)
	}
	extents := []struct {
		off int64
		n   int
	}{
		{0, 256 << 10},
		{24 << 20, 128 << 10},
		{48<<20 + 137, 4099},
	}
	for _, e := range extents {
		buf := make([]byte, e.n)
		if _, err := rand.Read(buf); err != nil {
			src.Close()
			t.Fatal(err)
		}
		if _, err := src.WriteAt(buf, e.off); err != nil {
			src.Close()
			t.Fatal(err)
		}
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	in, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	dstPath := filepath.Join(dir, "dst.img")
	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := copySparse(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	srcSt, err := os.Stat(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	dstSt, err := os.Stat(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if dstSt.Size() != logicalSize {
		t.Fatalf("size = %d, want %d", dstSt.Size(), logicalSize)
	}
	srcAlloc := srcSt.Sys().(*syscall.Stat_t).Blocks * 512
	dstAlloc := dstSt.Sys().(*syscall.Stat_t).Blocks * 512
	if dstAlloc > 4<<20 || dstAlloc > srcAlloc+64<<10 {
		t.Fatalf("allocated %d bytes (src %d, logical %d): sparse holes were inflated", dstAlloc, srcAlloc, logicalSize)
	}
	wantBytes, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	gotBytes, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatal("sparse copy content differs from source")
	}
}

func TestCopySparseEdgeCases(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name     string
		setup    func(*os.File) error
		wantSize int64
		maxAlloc int64
	}{
		{
			name:     "empty file",
			setup:    func(f *os.File) error { return nil },
			wantSize: 0,
			maxAlloc: 0,
		},
		{
			name:     "all hole file",
			setup:    func(f *os.File) error { return f.Truncate(24 << 20) },
			wantSize: 24 << 20,
			maxAlloc: 64 << 10,
		},
		{
			name: "leading hole to eof data",
			setup: func(f *os.File) error {
				if err := f.Truncate(24 << 20); err != nil {
					return err
				}
				_, err := f.WriteAt([]byte("tail-payload"), (24<<20)-12)
				return err
			},
			wantSize: 24 << 20,
			maxAlloc: 1 << 20,
		},
		{
			name: "small dense file",
			setup: func(f *os.File) error {
				_, err := f.Write([]byte("hello sparse world"))
				return err
			},
			wantSize: 18,
			maxAlloc: 1 << 20,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srcPath := filepath.Join(dir, tc.name+"-src")
			dstPath := filepath.Join(dir, tc.name+"-dst")
			sf, err := os.OpenFile(srcPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.setup(sf); err != nil {
				sf.Close()
				t.Fatal(err)
			}
			sf.Close()

			in, err := os.Open(srcPath)
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if err := copySparse(out, in); err != nil {
				out.Close()
				t.Fatal(err)
			}
			if err := out.Close(); err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(dstPath)
			if err != nil {
				t.Fatal(err)
			}
			if st.Size() != tc.wantSize {
				t.Fatalf("size = %d, want %d", st.Size(), tc.wantSize)
			}
			alloc := st.Sys().(*syscall.Stat_t).Blocks * 512
			if alloc > tc.maxAlloc {
				t.Fatalf("allocated %d bytes, want <= %d", alloc, tc.maxAlloc)
			}
			want, err := os.ReadFile(srcPath)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(dstPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("content mismatch")
			}
		})
	}
}

func TestPunchHoleDeallocatesBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hole.img")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := bytes.Repeat([]byte{0xab}, 1<<20)
	for range 8 {
		if _, err := f.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := ociPunchHole(f, 1<<20, 6<<20); err != nil {
		t.Skipf("filesystem does not support hole punching: %v", err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 8<<20 {
		t.Fatalf("size = %d, want %d", st.Size(), 8<<20)
	}
	alloc := st.Sys().(*syscall.Stat_t).Blocks * 512
	if alloc > 4<<20 {
		t.Fatalf("allocated %d bytes after punching 6 MiB hole", alloc)
	}
	hole := make([]byte, 4096)
	if _, err := f.ReadAt(hole, 2<<20); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hole, make([]byte, 4096)) {
		t.Fatal("punched region did not read back as zeros")
	}
}
