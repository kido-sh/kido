package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type entry struct {
	name, link string
	typ        byte
	body       string
	size       int64 // header size when body is empty; 0 means len(body)
	mode       int64
}

func tarOf(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: e.mode}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractKeepsOnlyWhatKidoUses(t *testing.T) {
	dir := t.TempDir()
	stats, err := extractTar(bytes.NewReader(tarOf(t, []entry{
		{name: "./", typ: tar.TypeDir},
		{name: "./usr/local/bin/", typ: tar.TypeDir},
		{name: "./usr/local/bin/qemu-system-aarch64", typ: tar.TypeReg, body: "q", mode: 0o755},
		{name: "./usr/local/bin/qemu-img", typ: tar.TypeReg, body: "i", mode: 0o755},
		{name: "./usr/local/bin/qemu-system-x86_64", typ: tar.TypeReg, body: "x", mode: 0o755},
		{name: "./usr/local/lib/deps/libfoo.1.dylib", typ: tar.TypeReg, body: "f"},
		{name: "./usr/local/lib/deps/libfoo.dylib", typ: tar.TypeSymlink, link: "libfoo.1.dylib"},
		{name: "./usr/local/share/qemu/efi-virtio.rom", typ: tar.TypeReg, body: "rom"},
		{name: "./usr/local/share/qemu/edk2-aarch64-code.fd", typ: tar.TypeReg, body: "uefi"},
		{name: "./usr/local/include/qemu-plugin.h", typ: tar.TypeReg, body: "h"},
	})), dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 4 || stats.Skipped != 5 {
		t.Errorf("stats = %+v, want 4 files and 5 skipped", stats)
	}
	for name, mode := range map[string]os.FileMode{
		"bin/qemu-system-aarch64":   0o700,
		"bin/qemu-img":              0o700,
		"lib/deps/libfoo.1.dylib":   0o600,
		"share/qemu/efi-virtio.rom": 0o600,
	} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if fi.Mode().Perm() != mode {
			t.Errorf("%s: mode %v, want %v", name, fi.Mode().Perm(), mode)
		}
	}
	for _, name := range []string{"bin/qemu-system-x86_64", "share/qemu/edk2-aarch64-code.fd", "include"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was extracted", name)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "lib/deps/libfoo.dylib")); err != nil || string(b) != "f" {
		t.Errorf("symlink: %q, %v", b, err)
	}
}

func TestExtractRefusesHostileEntries(t *testing.T) {
	cases := map[string][]entry{
		"dotdot":           {{name: "usr/local/lib/../../../../etc/x", typ: tar.TypeReg, body: "x"}},
		"absolute":         {{name: "/usr/local/lib/x", typ: tar.TypeReg, body: "x"}},
		"absolute symlink": {{name: "lib/x", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"dotdot symlink":   {{name: "lib/x", typ: tar.TypeSymlink, link: "../../x"}},
		"symlink chain": {
			{name: "lib/a/s", typ: tar.TypeSymlink, link: "."},
			{name: "lib/a/t", typ: tar.TypeSymlink, link: "s/../../../x"},
		},
		"hard link":   {{name: "lib/x", typ: tar.TypeLink, link: "bin/qemu-img"}},
		"device":      {{name: "lib/x", typ: tar.TypeChar}},
		"fifo":        {{name: "lib/x", typ: tar.TypeFifo}},
		"duplicate":   {{name: "lib/x", typ: tar.TypeReg, body: "1"}, {name: "./usr/local/lib/x", typ: tar.TypeReg, body: "2"}},
		"over a link": {{name: "lib/x", typ: tar.TypeSymlink, link: "y"}, {name: "lib/x", typ: tar.TypeReg, body: "2"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "tree")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := extractTar(bytes.NewReader(tarOf(t, entries)), dir); err == nil {
				t.Fatal("extracted without an error")
			}
			if _, err := os.Lstat(filepath.Join(parent, "x")); err == nil {
				t.Fatal("wrote outside the tree")
			}
		})
	}
}

func TestExtractRefusesWritesThroughEscapingSymlink(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "tree")
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A link planted before extraction (not by the archive) must not let a file escape either.
	if err := os.Symlink(parent, filepath.Join(dir, "lib", "out")); err != nil {
		t.Fatal(err)
	}
	_, err := extractTar(bytes.NewReader(tarOf(t, []entry{{name: "lib/out/x", typ: tar.TypeReg, body: "x"}})), dir)
	if err == nil {
		t.Fatal("extracted through a symlink that leaves the tree")
	}
	if _, err := os.Lstat(filepath.Join(parent, "x")); err == nil {
		t.Fatal("wrote outside the tree")
	}
}

func TestExtractLimits(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "lib/big", Typeflag: tar.TypeReg, Size: maxFileBytes + 1, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	// The header alone must be refused: nothing is read past it.
	_, err := extractTar(bytes.NewReader(buf.Bytes()), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v, want the per-file limit", err)
	}
	c := &capReader{r: bytes.NewReader(make([]byte, 10)), left: 4}
	if _, err := c.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(make([]byte, 10)); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("capReader past the cap: %v", err)
	}
}

func TestUnpackThroughSystemDecompressor(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(tarOf(t, []entry{
		{name: "bin/qemu-system-aarch64", typ: tar.TypeReg, body: "q", mode: 0o755},
		{name: "bin/qemu-img", typ: tar.TypeReg, body: "i", mode: 0o755},
	}))
	zw.Close()
	archive := filepath.Join(t.TempDir(), "q.tar.gz")
	if err := os.WriteFile(archive, gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stats, err := unpack(context.Background(), archive, dir)
	if err != nil || stats.Files != 2 {
		t.Fatalf("unpack: %+v, %v", stats, err)
	}
	if err := os.WriteFile(archive, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := unpack(context.Background(), archive, t.TempDir()); err == nil {
		t.Fatal("unpacked garbage")
	}
}

func TestFindQEMUOrder(t *testing.T) {
	home := t.TempDir()
	saved := DevQEMU
	t.Cleanup(func() { DevQEMU = saved })
	DevQEMU = filepath.Join(t.TempDir(), "absent", qemuName)

	t.Setenv("KIDO_QEMU", "")
	t.Setenv("PATH", t.TempDir())
	q, err := FindQEMU(home)
	if err != ErrNoQEMU || q.Path != filepath.Join(home, "qemu", "current", "bin", qemuName) {
		t.Fatalf("empty home: %+v, %v", q, err)
	}

	sysDir := t.TempDir()
	sysQEMU := filepath.Join(sysDir, qemuName)
	sysImg := filepath.Join(sysDir, qemuImgName)
	writeExe(t, sysQEMU)
	writeExe(t, sysImg)
	t.Setenv("PATH", sysDir)
	if q, err = findQEMUForOS("darwin", home); err != ErrNoQEMU || q.Path != filepath.Join(home, "qemu", "current", "bin", qemuName) {
		t.Fatalf("darwin PATH should be ignored: %+v, %v", q, err)
	}
	if q, err = findQEMUForOS("linux", home); err != nil || q.Source != SourceSystem || q.Path != sysQEMU || q.Img != sysImg {
		t.Fatalf("linux PATH fallback: %+v, %v", q, err)
	}
	if runtime.GOOS == "darwin" {
		if q, err = FindQEMU(home); err != ErrNoQEMU {
			t.Fatalf("FindQEMU on darwin with PATH QEMU: %+v, %v", q, err)
		}
	} else {
		if q, err = FindQEMU(home); err != nil || q.Source != SourceSystem || q.Path != sysQEMU || q.Img != sysImg {
			t.Fatalf("FindQEMU on %s with PATH QEMU: %+v, %v", runtime.GOOS, q, err)
		}
	}

	dev := filepath.Join(t.TempDir(), "bin", qemuName)
	writeExe(t, dev)
	DevQEMU = dev
	if q, err = findQEMUForOS("darwin", home); err != nil || q.Source != SourceDev || q.Path != dev {
		t.Fatalf("darwin dev fallback: %+v, %v", q, err)
	}
	if q, err = findQEMUForOS("linux", home); err != nil || q.Source != SourceDev || q.Path != dev {
		t.Fatalf("linux dev fallback: %+v, %v", q, err)
	}

	writeExe(t, filepath.Join(home, "qemu", "11.1.3", "bin", qemuName))
	if err := switchCurrent(filepath.Join(home, "qemu"), "11.1.3"); err != nil {
		t.Fatal(err)
	}
	if q, err = FindQEMU(home); err != nil || q.Source != SourceSetup || CurrentVersion(home) != "11.1.3" {
		t.Fatalf("installed: %+v, %v", q, err)
	}
	if q.Img != filepath.Join(home, "qemu", "current", "bin", qemuImgName) {
		t.Errorf("qemu-img = %s", q.Img)
	}

	t.Setenv("KIDO_QEMU", "/opt/q/bin/qemu-system-aarch64")
	if q, err = FindQEMU(home); err != nil || q.Source != SourceEnv || q.Img != "/opt/q/bin/qemu-img" {
		t.Fatalf("KIDO_QEMU: %+v, %v", q, err)
	}
	t.Setenv("KIDO_QEMU", "qemu-system-aarch64")
	if _, err = FindQEMU(home); err == nil {
		t.Fatal("a relative KIDO_QEMU was accepted")
	}
}

func TestFindQEMUPathSeparateImg(t *testing.T) {
	home := t.TempDir()
	saved := DevQEMU
	t.Cleanup(func() { DevQEMU = saved })
	DevQEMU = ""
	t.Setenv("KIDO_QEMU", "")

	qemuDir := t.TempDir()
	imgDir := t.TempDir()
	sysQEMU := filepath.Join(qemuDir, qemuName)
	sysImg := filepath.Join(imgDir, qemuImgName)
	writeExe(t, sysQEMU)
	writeExe(t, sysImg)
	t.Setenv("PATH", qemuDir+string(os.PathListSeparator)+imgDir)

	if q, err := findQEMUForOS("darwin", home); err != ErrNoQEMU {
		t.Fatalf("darwin findQEMUForOS = %+v, %v; want ErrNoQEMU", q, err)
	}
	q, err := findQEMUForOS("linux", home)
	if err != nil {
		t.Fatalf("findQEMUForOS(linux): %v", err)
	}
	if q.Source != SourceSystem || q.Path != sysQEMU || q.Img != sysImg {
		t.Fatalf("got %+v, want Path=%s Img=%s Source=%s", q, sysQEMU, sysImg, SourceSystem)
	}
}

func TestDecompressorFor(t *testing.T) {
	cases := []struct {
		goos    string
		archive string
		header  []byte
		want    string
	}{
		{"darwin", "q.tar.xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}, darwinDecompressor},
		{"linux", "q.archive", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}, "xz"},
		{"linux", "q.archive", []byte{0x1f, 0x8b, 0x08}, "gzip"},
		{"linux", "q.archive", []byte{'B', 'Z', 'h', '9'}, "bzip2"},
		{"linux", "q.tar.xz", []byte("not magic"), "xz"},
		{"linux", "q.tar.bz2", []byte("not magic"), "bzip2"},
		{"linux", "q.tar.gz", []byte("not magic"), "gzip"},
	}
	for _, tc := range cases {
		if got := decompressorForOS(tc.goos, tc.archive, tc.header); got != tc.want {
			t.Errorf("decompressorForOS(%q, %q, %x) = %q, want %q", tc.goos, tc.archive, tc.header, got, tc.want)
		}
	}
}

func writeExe(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestEntitlementsMatchPackaging(t *testing.T) {
	b, err := os.ReadFile("../../packaging/entitlements/qemu-hvf.entitlements")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != hvfEntitlements {
		t.Fatal("hvfEntitlements differs from packaging/entitlements/qemu-hvf.entitlements")
	}
	ok, err := plistKeyIsTrue([]byte(hvfEntitlements), HypervisorEntitlement)
	if err != nil || !ok {
		t.Fatalf("plistKeyIsTrue = %v, %v", ok, err)
	}
}

func TestCheckLibrariesSystemBinary(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Mach-O library check is macOS-only")
	}
	libs, err := CheckLibraries("/bin/ls")
	if err != nil {
		t.Fatal(err)
	}
	if len(libs.Missing) != 0 || len(libs.External) != 0 {
		t.Fatalf("/bin/ls: %+v", libs)
	}
}

func TestHomebrewFormulas(t *testing.T) {
	got := HomebrewFormulas([]string{
		"/opt/homebrew/opt/glib/lib/libglib-2.0.0.dylib",
		"/opt/homebrew/opt/glib/lib/libgio-2.0.0.dylib",
		"/usr/local/opt/zstd/lib/libzstd.1.dylib",
		"/opt/homebrew/Cellar/gnutls/3.8/lib/libgnutls.30.dylib",
		"@rpath/libfoo.dylib",
	})
	if strings.Join(got, " ") != "glib gnutls zstd" {
		t.Fatalf("got %v", got)
	}
}

func TestInstallValidatesInput(t *testing.T) {
	home := t.TempDir()
	good := strings.Repeat("a", 64)
	for _, rel := range []Release{
		{Version: "../x", URL: "https://example.com/q.tar.xz", SHA256: good},
		{Version: "current", URL: "https://example.com/q.tar.xz", SHA256: good},
		{Version: "1", URL: "http://example.com/q.tar.xz", SHA256: good},
		{Version: "1", URL: "relative/q.tar.xz", SHA256: good},
		{Version: "1", URL: "https://example.com/q.tar.xz", SHA256: "abc"},
	} {
		if _, err := Install(context.Background(), Options{Home: home, Release: rel}); err == nil {
			t.Errorf("%+v was accepted", rel)
		}
	}
}
