package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
	"github.com/kido-sh/kido/internal/registry"
)

const ioTestDiskSize = 64 << 20

func ioRandom(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// ioTestImage writes a fake image: a 64 MiB sparse disk with a few random extents (one
// unaligned, one at the very end), a random kernel and a half-zero initrd.
func ioTestImage(t *testing.T, home string, ref imageref.Ref) *imageJSON {
	t.Helper()
	dir := ref.Dir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	disk, err := os.OpenFile(filepath.Join(dir, "disk.img"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.Truncate(ioTestDiskSize); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct{ off, n int64 }{{0, 1 << 20}, {10 << 20, 300 << 10}, {40<<20 + 123, 5000}, {ioTestDiskSize - 1000, 1000}} {
		if _, err := disk.WriteAt(ioRandom(int(e.n)), e.off); err != nil {
			t.Fatal(err)
		}
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"kernel": ioRandom(2 << 20), "initrd.cpio": append(ioRandom(100<<10), make([]byte, 412<<10)...)}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	j := &imageJSON{
		Name:     ref.Name,
		Tag:      ref.Tag,
		Created:  time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC),
		RootUUID: "0b5c5a59-3d0e-4c8a-9a51-6a3e3f0f3d11",
		Size:     1,
		Parent:   "base:v1",
		Config: engine.ImageConfig{Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{"echo hi"}, Env: []string{"A=B"},
			WorkingDir: "/tmp", User: "root", Labels: map[string]string{"x": "y"}},
	}
	if err := writeJSON(filepath.Join(dir, "image.json"), j); err != nil {
		t.Fatal(err)
	}
	return j
}

type ioEntry struct {
	hdr  *tar.Header
	data []byte
}

func ioEntries(t *testing.T, archive []byte) []ioEntry {
	t.Helper()
	var out []ioEntry
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ioEntry{hdr, data})
	}
}

func ioArchive(t *testing.T, entries []ioEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := *e.hdr
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func ioClone(entries []ioEntry) []ioEntry {
	out := make([]ioEntry, len(entries))
	for i, e := range entries {
		h := *e.hdr
		out[i] = ioEntry{&h, bytes.Clone(e.data)}
	}
	return out
}

func ioFind(t *testing.T, entries []ioEntry, name string) int {
	t.Helper()
	for i, e := range entries {
		if e.hdr.Name == name {
			return i
		}
	}
	t.Fatalf("no archive entry %s", name)
	return -1
}

func ioBlobName(digest string) string { return ociBlobPrefix + strings.TrimPrefix(digest, "sha256:") }

// ioRewriteManifest changes the first image's manifest with fn and stores it under its new
// digest, with index.json pointing at it.
func ioRewriteManifest(t *testing.T, entries []ioEntry, fn func(*ociManifest)) []ioEntry {
	t.Helper()
	entries = ioClone(entries)
	ii := ioFind(t, entries, ociIndexFile)
	var idx ociIndex
	if err := json.Unmarshal(entries[ii].data, &idx); err != nil {
		t.Fatal(err)
	}
	mi := ioFind(t, entries, ioBlobName(idx.Manifests[0].Digest))
	var m ociManifest
	if err := json.Unmarshal(entries[mi].data, &m); err != nil {
		t.Fatal(err)
	}
	fn(&m)
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	entries[mi].hdr.Name, entries[mi].data = ioBlobName(ociDigest(mb)), mb
	idx.Manifests[0].Digest, idx.Manifests[0].Size = ociDigest(mb), int64(len(mb))
	if entries[ii].data, err = json.Marshal(idx); err != nil {
		t.Fatal(err)
	}
	return entries
}

func ioAllocated(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Sys().(*syscall.Stat_t).Blocks * 512
}

// ioAssertStore fails unless images/ holds exactly the image directories want, besides the
// lock, and nothing is left under tmp/.
func ioAssertStore(t *testing.T, home string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "images"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		if e.Name() != ".lock" {
			got = append(got, e.Name())
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("images/ holds %v, want %v", got, want)
	}
	tmp, err := os.ReadDir(filepath.Join(home, "tmp"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range tmp {
		t.Errorf("tmp/%s left behind", e.Name())
	}
}

func TestImageIORoundTrip(t *testing.T) {
	ctx := context.Background()
	src, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref := imageref.Ref{Name: "darwin", Tag: "latest"}
	orig := ioTestImage(t, src.Home, ref)
	// A second image with the same files: the archive holds each blob once.
	mirror := imageref.Ref{Name: "mirror/darwin", Tag: "v2"}
	if err := os.MkdirAll(mirror.Dir(src.Home), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"disk.img", "kernel", "initrd.cpio", "image.json"} {
		if err := cloneFile(filepath.Join(ref.Dir(src.Home), f), filepath.Join(mirror.Dir(src.Home), f)); err != nil {
			t.Fatal(err)
		}
	}

	var archive bytes.Buffer
	if err := src.SaveImages(ctx, &archive, []string{"darwin", "darwin:latest", "mirror/darwin:v2"}); err != nil {
		t.Fatal(err)
	}
	ioAssertStore(t, src.Home, "darwin", "mirror%2Fdarwin")
	entries := ioEntries(t, archive.Bytes())
	var names []string
	for _, e := range entries {
		names = append(names, e.hdr.Name)
	}
	if len(names) != 9 || names[0] != ociLayoutFile || names[1] != ociIndexFile {
		t.Fatalf("archive entries %v", names)
	}
	var idx ociIndex
	if err := json.Unmarshal(entries[1].data, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Manifests) != 2 {
		t.Fatalf("index.json: %s", entries[1].data)
	}
	for i, want := range []imageref.Ref{ref, mirror} {
		d := idx.Manifests[i]
		if d.Annotations[ociAnnotationImageName] != want.String() || d.Annotations[ociAnnotationRefName] != want.Tag ||
			d.ArtifactType != registry.ArtifactType || d.Platform == nil || d.Platform.OS != "darwin" || d.Digest != idx.Manifests[0].Digest {
			t.Errorf("index.json entry %d: %+v", i, d)
		}
	}
	if names[4] != ioBlobName(idx.Manifests[0].Digest) || entries[5].hdr.Size > ociMaxConfig {
		t.Errorf("the manifest and config do not come before the layers: %v", names)
	}

	dst, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	refs, err := dst.LoadImages(ctx, bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(refs, []string{"darwin:latest", "mirror/darwin:v2"}) {
		t.Fatalf("loaded %v", refs)
	}
	ioAssertStore(t, dst.Home, "darwin", "mirror%2Fdarwin")
	for _, r := range []imageref.Ref{ref, mirror} {
		for _, f := range []string{"disk.img", "kernel", "initrd.cpio"} {
			want, err := os.ReadFile(filepath.Join(ref.Dir(src.Home), f))
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(r.Dir(dst.Home), f))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s %s differs after the round trip", r, f)
			}
		}
		j, err := dst.loadImage(r)
		if err != nil {
			t.Fatal(err)
		}
		if !j.Created.Equal(orig.Created) || j.RootUUID != orig.RootUUID || j.Parent != orig.Parent || !reflect.DeepEqual(j.Config, orig.Config) {
			t.Errorf("%s image.json %+v, want %+v", r, j, orig)
		}
		if n := ioAllocated(t, filepath.Join(r.Dir(dst.Home), "disk.img")); n > 4<<20 {
			t.Errorf("%s disk.img allocates %d of %d bytes: holes lost", r, n, ioTestDiskSize)
		} else {
			t.Logf("%s disk.img allocates %d of %d bytes", r, n, ioTestDiskSize)
		}
	}
}

func TestImageIORefusesHostileArchives(t *testing.T) {
	ctx := context.Background()
	src, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ioTestImage(t, src.Home, imageref.Ref{Name: "darwin", Tag: "latest"})
	var good bytes.Buffer
	if err := src.SaveImages(ctx, &good, []string{"darwin"}); err != nil {
		t.Fatal(err)
	}
	entries := ioEntries(t, good.Bytes())
	var idx ociIndex
	if err := json.Unmarshal(entries[ioFind(t, entries, ociIndexFile)].data, &idx); err != nil {
		t.Fatal(err)
	}
	var m ociManifest
	if err := json.Unmarshal(entries[ioFind(t, entries, ioBlobName(idx.Manifests[0].Digest))].data, &m); err != nil {
		t.Fatal(err)
	}
	kernel := ioFind(t, entries, ioBlobName(m.Layers[1].Digest))

	with := func(extra ...ioEntry) []byte { return ioArchive(t, append(ioClone(entries), extra...)) }
	file := func(name string) ioEntry {
		return ioEntry{&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644}, []byte("x")}
	}
	rewrite := func(fn func(*ociManifest)) []byte { return ioArchive(t, ioRewriteManifest(t, entries, fn)) }

	tampered := ioClone(entries)
	tampered[kernel].data[100] ^= 1
	missing := ioClone(entries)
	missing = append(missing[:kernel], missing[kernel+1:]...)
	bigIndex := ioClone(entries)
	ii := ioFind(t, bigIndex, ociIndexFile)
	bigIndex[ii].data = append(bigIndex[ii].data, bytes.Repeat([]byte(" "), ociMaxIndex)...)

	cases := []struct {
		name    string
		archive []byte
	}{
		{"parent traversal", with(file("../evil"))},
		{"traversal inside blobs", with(file("blobs/sha256/../../../evil"))},
		{"absolute name", with(file("/tmp/evil"))},
		{"symlink", with(ioEntry{&tar.Header{Typeflag: tar.TypeSymlink, Name: "evil", Linkname: "/etc/passwd"}, nil})},
		{"hard link", with(ioEntry{&tar.Header{Typeflag: tar.TypeLink, Name: "evil", Linkname: ociIndexFile}, nil})},
		{"device", with(ioEntry{&tar.Header{Typeflag: tar.TypeChar, Name: "evil", Devmajor: 1, Devminor: 3}, nil})},
		{"duplicate blob", with(entries[kernel])},
		{"digest mismatch", ioArchive(t, tampered)},
		{"missing blob", ioArchive(t, missing)},
		{"oversize index.json", ioArchive(t, bigIndex)},
		{"wrong media type", rewrite(func(m *ociManifest) { m.Layers[1].MediaType = "application/octet-stream" })},
		{"extra layer", rewrite(func(m *ociManifest) { m.Layers = append(m.Layers, m.Layers[1]) })},
		{"not a kido artifact", rewrite(func(m *ociManifest) { m.ArtifactType = "application/vnd.oci.image.config.v1+json" })},
		{"uncompressed size smaller than the data", rewrite(func(m *ociManifest) {
			m.Layers[0].Annotations[ociAnnotationUncompressed] = strconv.Itoa(ioTestDiskSize / 2)
		})},
		{"uncompressed size larger than the data", rewrite(func(m *ociManifest) {
			m.Layers[0].Annotations[ociAnnotationUncompressed] = strconv.Itoa(ioTestDiskSize + 4096)
		})},
	}
	dst, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refs, err := dst.LoadImages(ctx, bytes.NewReader(c.archive))
			if err == nil {
				t.Fatalf("loaded %v", refs)
			}
			t.Logf("refused: %v", err)
			ioAssertStore(t, dst.Home)
		})
	}

	// The same archive with "./" names and a legacy archive's extra files still loads.
	dotted := ioClone(entries)
	for _, e := range dotted {
		e.hdr.Name = "./" + e.hdr.Name
	}
	dotted = append(dotted, file("manifest.json"), file("repositories"))
	refs, err := dst.LoadImages(ctx, bytes.NewReader(ioArchive(t, dotted)))
	if err != nil || !slices.Equal(refs, []string{"darwin:latest"}) {
		t.Fatalf("the untouched archive: %v, %v", refs, err)
	}
	ioAssertStore(t, dst.Home, "darwin")

	// An image a container uses is not replaced.
	if err := os.MkdirAll(filepath.Join(dst.containersDir(), "0123456789ab"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(dst.file("0123456789ab", "container.json"), &Info{ID: "0123456789ab", Name: "c1", Image: "darwin:latest"}); err != nil {
		t.Fatal(err)
	}
	if refs, err := dst.LoadImages(ctx, bytes.NewReader(good.Bytes())); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("replaced an image in use: %v, %v", refs, err)
	}
	ioAssertStore(t, dst.Home, "darwin")
}
