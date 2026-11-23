package registry

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"

	"github.com/kido-sh/kido/internal/imageref"
)

func allocated(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Sys().(*syscall.Stat_t).Blocks * 512
}

// dataExtents lists [start, end) of the allocated regions of f.
func dataExtents(t *testing.T, f *os.File) [][2]int64 {
	t.Helper()
	st, _ := f.Stat()
	var out [][2]int64
	for off := int64(0); off < st.Size(); {
		d, err := f.Seek(off, unix.SEEK_DATA)
		if err != nil {
			break
		}
		h, err := f.Seek(d, unix.SEEK_HOLE)
		if err != nil {
			break
		}
		out = append(out, [2]int64{d, h})
		off = h
	}
	return out
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// makeImage writes a fake image: a 64 MiB sparse disk with a few random extents (one unaligned,
// one at the very end), a random kernel and a half-zero initrd.
func makeImage(t *testing.T, home string, ref imageref.Ref) string {
	t.Helper()
	dir := ref.Dir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	disk, err := os.OpenFile(filepath.Join(dir, "disk.img"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	const size = 64 << 20
	if err := disk.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct{ off, n int64 }{{0, 1 << 20}, {10 << 20, 300 << 10}, {40<<20 + 123, 5000}, {size - 1000, 1000}} {
		if _, err := disk.WriteAt(randomBytes(int(e.n)), e.off); err != nil {
			t.Fatal(err)
		}
	}
	disk.Close()
	if err := os.WriteFile(filepath.Join(dir, "kernel"), randomBytes(2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	initrd := append(randomBytes(100<<10), make([]byte, 412<<10)...)
	if err := os.WriteFile(filepath.Join(dir, "initrd.cpio"), initrd, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"name":%q,"tag":%q,"created":"2026-11-22T01:00:00Z","rootUUID":"0b5c5a59-3d0e-4c8a-9a51-6a3e3f0f3d11","size":1,"config":{"cmd":["/bin/sh"],"env":["A=B"],"labels":{"x":"y"}}}`, ref.Name, ref.Tag)
	if err := os.WriteFile(filepath.Join(dir, "image.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWriteSparse(t *testing.T) {
	data := make([]byte, 5<<20+777)
	copy(data[1<<20+5:], "hello")
	copy(data[3<<20:], randomBytes(8192))
	copy(data[len(data)-3:], "end")
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n, err := writeSparse(f, bytes.NewReader(data), int64(len(data)))
	if err != nil || n != int64(len(data)) {
		t.Fatalf("writeSparse = %d, %v", n, err)
	}
	got, _ := os.ReadFile(f.Name())
	if !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	if a := allocated(t, f.Name()); a > 128<<10 {
		t.Fatalf("allocated %d bytes, want a sparse file", a)
	}

	g, _ := os.Create(filepath.Join(t.TempDir(), "over"))
	defer g.Close()
	if _, err := writeSparse(g, bytes.NewReader(data), int64(len(data))-1); err == nil {
		t.Fatal("writeSparse accepted more data than the limit")
	}
}

func TestWriteSparseShapes(t *testing.T) {
	big := func(n int, fill func([]byte)) []byte { b := make([]byte, n); fill(b); return b }
	for name, data := range map[string][]byte{
		"empty":               nil,
		"all zero":            make([]byte, 3<<20+5),
		"short tail":          big(4097, func(b []byte) { b[4096] = 1 }),
		"zeros across chunks": big(4<<20, func(b []byte) { b[0] = 1; b[len(b)-1] = 1 }),
		"ends in zeros":       big(2<<20+100, func(b []byte) { copy(b[1<<20-3:], "xyzzy") }),
		"dense":               randomBytes(1<<20 + 4095),
	} {
		f, err := os.Create(filepath.Join(t.TempDir(), "out"))
		if err != nil {
			t.Fatal(err)
		}
		n, err := writeSparse(f, bytes.NewReader(data), int64(len(data)))
		f.Close()
		got, _ := os.ReadFile(f.Name())
		if err != nil || n != int64(len(data)) || !bytes.Equal(got, data) {
			t.Errorf("%s: n=%d err=%v equal=%v", name, n, err, bytes.Equal(got, data))
		}
	}
}

// spacedReader yields size bytes of zeros with one 4 KiB block of 0xff every gap bytes.
type spacedReader struct{ off, size, gap int64 }

func (s *spacedReader) Read(p []byte) (int, error) {
	if s.off >= s.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), s.size-s.off))
	for i := range p[:n] {
		p[i] = 0
		if (s.off+int64(i))%s.gap < 4096 {
			p[i] = 0xff
		}
	}
	s.off += int64(n)
	return n, nil
}

// TestWriteSparseSpacedData: APFS would zero-fill every 15 MiB gap between these blocks, and
// fills the hole at a settle boundary again after the next write; only the data stays allocated.
func TestWriteSparseSpacedData(t *testing.T) {
	const size, gap = 1 << 30, 15 << 20
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if n, err := writeSparse(f, &spacedReader{size: size, gap: gap}, size); err != nil || n != size {
		t.Fatalf("writeSparse = %d, %v", n, err)
	}
	// st_blocks lags behind punched holes for a while; the extents do not.
	var data int64
	ext := dataExtents(t, f)
	for _, e := range ext {
		data += e[1] - e[0]
	}
	if want := int64(size/gap+1) * 4096; data > want {
		t.Fatalf("%d bytes in data extents, want %d; extents %v", data, want, ext)
	}
}

func TestParseRemote(t *testing.T) {
	for _, c := range []struct{ in, host, repo, key, tag string }{
		{"localhost:5001/darwin", "localhost:5001", "darwin", "localhost:5001", "latest"},
		{"127.0.0.1:5001/x/darwin:v1", "127.0.0.1:5001", "x/darwin", "127.0.0.1:5001", "v1"},
		{"ghcr.io/me/darwin:1.0", "ghcr.io", "me/darwin", "ghcr.io", "1.0"},
		{"darwin", dockerHubHost, "library/darwin", dockerHubAuthKey, "latest"},
		{"me/darwin:t", dockerHubHost, "me/darwin", dockerHubAuthKey, "t"},
		{"docker.io/darwin", dockerHubHost, "library/darwin", dockerHubAuthKey, "latest"},
	} {
		r, err := parseRemote(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if r.host != c.host || r.repo != c.repo || r.authKey != c.key || r.ref.Tag != c.tag {
			t.Errorf("%s: got %+v", c.in, r)
		}
		if want := strings.HasPrefix(c.in, "localhost") || strings.HasPrefix(c.in, "127."); r.insecure != want {
			t.Errorf("%s: insecure = %v", c.in, r.insecure)
		}
	}
	if r, _ := parseRemote("ghcr.io/me/darwin"); r.insecure {
		t.Error("ghcr.io must use HTTPS")
	}
}

func TestParseChallenge(t *testing.T) {
	s, p := parseChallenge(`Bearer realm="https://auth.example/token",service="reg",scope="repository:a/b:pull,push"`)
	if s != "bearer" || p["realm"] != "https://auth.example/token" || p["service"] != "reg" || p["scope"] != "repository:a/b:pull,push" {
		t.Fatalf("got %q %v", s, p)
	}
	s, p = pickChallenge([]string{`Basic realm="x"`, `Bearer realm="https://a/t"`})
	if s != "bearer" || p["realm"] != "https://a/t" {
		t.Fatalf("got %q %v", s, p)
	}
}

func TestCheckManifestRejects(t *testing.T) {
	good := func() *manifest {
		d := func(mt string, zstd bool) descriptor {
			x := descriptor{MediaType: mt, Digest: "sha256:" + strings.Repeat("a", 64), Size: 10}
			if zstd {
				x.Annotations = map[string]string{annotationUncompressedSize: "100"}
			}
			return x
		}
		return &manifest{SchemaVersion: 2, MediaType: mediaTypeManifest, ArtifactType: ArtifactType,
			Config: d(MediaTypeConfig, false),
			Layers: []descriptor{d(MediaTypeDisk, true), d(MediaTypeKernel, false), d(MediaTypeInitrd, true)}}
	}
	if err := checkManifest(good()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*manifest){
		"artifactType":   func(m *manifest) { m.ArtifactType = "application/vnd.oci.image.config.v1+json" },
		"config type":    func(m *manifest) { m.Config.MediaType = "application/json" },
		"unknown layer":  func(m *manifest) { m.Layers[1].MediaType = "application/x-evil" },
		"duplicate":      func(m *manifest) { m.Layers[2] = m.Layers[0] },
		"bad digest":     func(m *manifest) { m.Layers[0].Digest = "sha256:../../etc/passwd" },
		"no size":        func(m *manifest) { delete(m.Layers[0].Annotations, annotationUncompressedSize) },
		"missing layer":  func(m *manifest) { m.Layers = m.Layers[:2] },
		"huge config":    func(m *manifest) { m.Config.Size = maxConfigSize + 1 },
		"schema version": func(m *manifest) { m.SchemaVersion = 1 },
	} {
		m := good()
		mutate(m)
		if err := checkManifest(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestImageConfigRootUUID(t *testing.T) {
	if _, err := parseImageConfig([]byte(`{"rootUUID":"0b5c5a59-3d0e-4c8a-9a51-6a3e3f0f3d11 kido_ip=1.2.3.4"}`)); err == nil {
		t.Fatal("accepted a rootUUID with extra boot-args")
	}
	// The struct decoder takes the last of two keys that differ in case; the local copy must keep
	// only the value that was validated, whatever order a later decoder sees.
	c, err := parseImageConfig([]byte(`{"rootuuid":"x kido_ip=6.6.6.6","rootUUID":"0b5c5a59-3d0e-4c8a-9a51-6a3e3f0f3d11"}`))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := imageref.Parse("x:1")
	out, err := localImageConfig(c, ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "kido_ip") {
		t.Fatalf("local image.json keeps the unvalidated key: %s", out)
	}
}

// TestNoCredentialsToOtherHosts: a Basic-auth registry redirects a blob to a storage host on
// another port, which answers 401 with its own token realm. Neither the storage host nor that
// realm may see the registry's password (net/http copies Authorization to the same hostname).
func TestNoCredentialsToOtherHosts(t *testing.T) {
	var leaked atomic.Value
	realm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok {
			leaked.Store("token realm")
		}
		w.Write([]byte(`{"token":"t"}`))
	}))
	defer realm.Close()
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store("storage host")
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm.URL+`/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer storage.Close()
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "u" || p != "p" {
			w.Header().Set("WWW-Authenticate", `Basic realm="reg"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v2/" {
			http.Redirect(w, r, storage.URL+"/blob", http.StatusTemporaryRedirect)
		}
	}))
	defer reg.Close()
	host := strings.TrimPrefix(reg.URL, "http://")
	cfgDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", cfgDir)
	os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"auths":{"`+host+`":{"auth":"dTpw"}}}`), 0o600)

	r, _ := parseRemote(host + "/x")
	c, err := newClient(context.Background(), r, false)
	if err != nil {
		t.Fatal(err)
	}
	if c.authz == "" {
		t.Fatal("no Basic authorization set up")
	}
	d := descriptor{Digest: "sha256:" + strings.Repeat("a", 64), Size: 1}
	if err := c.fetchBlob(context.Background(), d, io.Discard, newProgress(nil, "", "", 0)); err == nil {
		t.Fatal("fetchBlob succeeded")
	}
	if who := leaked.Load(); who != nil {
		t.Fatalf("the registry's credentials reached the %s", who)
	}
}

// fakeRegistry serves one tag whose manifest and blobs the test controls.
func fakeRegistry(t *testing.T, manifests map[string][]byte, blobs map[string][]byte) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
		case strings.HasPrefix(r.URL.Path, "/v2/x/manifests/"):
			b, ok := manifests[strings.TrimPrefix(r.URL.Path, "/v2/x/manifests/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			var probe struct{ MediaType string }
			json.Unmarshal(b, &probe)
			w.Header().Set("Content-Type", probe.MediaType)
			w.Write(b)
		case strings.HasPrefix(r.URL.Path, "/v2/x/blobs/"):
			b, ok := blobs[strings.TrimPrefix(r.URL.Path, "/v2/x/blobs/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPullRejectsTamperedBlob(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	cfg := []byte(`{"name":"x","tag":"latest","rootUUID":"0b5c5a59-3d0e-4c8a-9a51-6a3e3f0f3d11","size":0,"config":{}}`)
	kernel := []byte("kernel")
	desc := func(mt string, b []byte) descriptor {
		return descriptor{MediaType: mt, Digest: digestOf(b), Size: int64(len(b)), Annotations: map[string]string{annotationUncompressedSize: "6"}}
	}
	m := manifest{SchemaVersion: 2, MediaType: mediaTypeManifest, ArtifactType: ArtifactType,
		Config: desc(MediaTypeConfig, cfg),
		Layers: []descriptor{desc(MediaTypeDisk, []byte("disk..")), desc(MediaTypeKernel, kernel), desc(MediaTypeInitrd, []byte("initrd"))}}
	mb, _ := json.Marshal(m)
	blobs := map[string][]byte{
		digestOf(cfg):              cfg,
		m.Layers[0].Digest:         []byte("DISK.."), // same size, wrong content
		digestOf(kernel):           kernel,
		digestOf([]byte("initrd")): []byte("initrd"),
	}
	srv := fakeRegistry(t, map[string][]byte{"latest": mb}, blobs)
	home := t.TempDir()
	ref := strings.TrimPrefix(srv.URL, "http://") + "/x:latest"
	_, _, err := Pull(context.Background(), PullOptions{Home: home, Remote: ref})
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("Pull = %v, want a digest mismatch", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "images")); len(entries) != 0 {
		t.Fatalf("a failed pull left %v in the store", entries)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "tmp")); len(entries) != 0 {
		t.Fatalf("a failed pull left %v in tmp", entries)
	}

	other := m
	other.ArtifactType = "application/vnd.example.other"
	ob, _ := json.Marshal(other)
	srv = fakeRegistry(t, map[string][]byte{"latest": ob}, blobs)
	ref = strings.TrimPrefix(srv.URL, "http://") + "/x:latest"
	if _, _, err := Pull(context.Background(), PullOptions{Home: home, Remote: ref}); err == nil || !strings.Contains(err.Error(), "not a kido image") {
		t.Fatalf("Pull = %v, want not a kido image", err)
	}
}

// TestPullWithBearerToken pulls through a registry that wants a token from a separate realm,
// with a credential stored in config.json.
func TestPullWithBearerToken(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", cfgDir)
	enc, _ := zstd.NewWriter(nil)
	disk := append(randomBytes(5000), make([]byte, 3<<20)...)
	initrd := randomBytes(777)
	kernel := randomBytes(1234)
	cfg := []byte(`{"name":"x","tag":"latest","rootUUID":"0b5c5a59-3d0e-4c8a-9a51-6a3e3f0f3d11","size":0,"parent":"base:latest","config":{}}`)
	blobs := map[string][]byte{}
	desc := func(mt string, stored []byte, raw int) descriptor {
		blobs[digestOf(stored)] = stored
		d := descriptor{MediaType: mt, Digest: digestOf(stored), Size: int64(len(stored))}
		if raw >= 0 {
			d.Annotations = map[string]string{annotationUncompressedSize: fmt.Sprint(raw)}
		}
		return d
	}
	m := manifest{SchemaVersion: 2, MediaType: mediaTypeManifest, ArtifactType: ArtifactType,
		Config: desc(MediaTypeConfig, cfg, -1),
		Layers: []descriptor{
			desc(MediaTypeDisk, enc.EncodeAll(disk, nil), len(disk)),
			desc(MediaTypeKernel, kernel, -1),
			desc(MediaTypeInitrd, enc.EncodeAll(initrd, nil), len(initrd)),
		}}
	mb, _ := json.Marshal(m)

	var srv *httptest.Server
	inner := fakeRegistry(t, map[string][]byte{"latest": mb}, blobs)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			u, p, ok := r.BasicAuth()
			if !ok || u != "u" || p != "p" || r.URL.Query().Get("scope") != "repository:x:pull" || r.URL.Query().Get("service") != "fake" {
				http.Error(w, "denied", http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"token":"tok"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake"`, srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(fmt.Sprintf(`{"auths":{"http://%s":{"auth":"dTpw"}}}`, host)), 0o600)

	home := t.TempDir()
	if _, _, err := Pull(context.Background(), PullOptions{Home: home, Remote: host + "/x", Image: "x:1"}); err != nil {
		t.Fatal(err)
	}
	ref, _ := imageref.Parse("x:1")
	got, _ := os.ReadFile(filepath.Join(ref.Dir(home), "disk.img"))
	if !bytes.Equal(got, disk) {
		t.Fatal("disk.img differs")
	}
}

// memRegistry is enough of the distribution API in memory for push and pull.
type memRegistry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte
	types     map[string]string
	uploads   map[string]*bytes.Buffer
}

func newMemRegistry() *memRegistry {
	return &memRegistry{blobs: map[string][]byte{}, manifests: map[string][]byte{}, types: map[string]string{}, uploads: map[string]*bytes.Buffer{}}
}

func (m *memRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := r.URL.Path
	last := p[strings.LastIndex(p, "/")+1:]
	switch {
	case p == "/v2/":
	case strings.Contains(p, "/blobs/uploads/") && r.Method == http.MethodPost:
		id := fmt.Sprint(len(m.uploads))
		m.uploads[id] = &bytes.Buffer{}
		w.Header().Set("Location", p+id)
		w.WriteHeader(http.StatusAccepted)
	case strings.Contains(p, "/blobs/uploads/") && r.Method == http.MethodPatch:
		io.Copy(m.uploads[last], r.Body)
		w.Header().Set("Location", p)
		w.WriteHeader(http.StatusAccepted)
	case strings.Contains(p, "/blobs/uploads/") && r.Method == http.MethodPut:
		b := m.uploads[last].Bytes()
		if d := r.URL.Query().Get("digest"); d != digestOf(b) {
			http.Error(w, `{"errors":[{"code":"DIGEST_INVALID"}]}`, http.StatusBadRequest)
			return
		}
		m.blobs[digestOf(b)] = b
		w.WriteHeader(http.StatusCreated)
	case strings.Contains(p, "/blobs/"):
		b, ok := m.blobs[last]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	case strings.Contains(p, "/manifests/") && r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		for _, k := range []string{last, digestOf(b)} {
			m.manifests[k], m.types[k] = b, r.Header.Get("Content-Type")
		}
		w.Header().Set("Docker-Content-Digest", digestOf(b))
		w.WriteHeader(http.StatusCreated)
	case strings.Contains(p, "/manifests/"):
		b, ok := m.manifests[last]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", m.types[last])
		w.Header().Set("Docker-Content-Digest", digestOf(b))
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

// TestRoundTrip pushes a fake image and pulls it back under another name: to the registry named
// by KIDO_TEST_REGISTRY (for example localhost:5001, a `registry:2` container), or to an in-memory
// one.
func TestRoundTrip(t *testing.T) {
	reg := os.Getenv("KIDO_TEST_REGISTRY")
	if reg == "" {
		srv := httptest.NewServer(newMemRegistry())
		defer srv.Close()
		reg = strings.TrimPrefix(srv.URL, "http://")
	}
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	ctx := context.Background()
	home := t.TempDir()
	src, _ := imageref.Parse("fake:v1")
	srcDir := makeImage(t, home, src)
	remote := reg + "/kido-test/fake:v1"

	gotRef, digest, err := Push(ctx, PushOptions{Home: home, Image: "fake:v1", Remote: remote, Version: "test", Progress: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pushed %s@%s", gotRef, digest)
	// A second push finds every blob in place.
	if _, d2, err := Push(ctx, PushOptions{Home: home, Image: "fake:v1", Remote: remote, Version: "test"}); err != nil || d2 != digest {
		t.Fatalf("second push = %s, %v; want the same digest %s", d2, err, digest)
	}

	local, pulled, err := Pull(ctx, PullOptions{Home: home, Remote: remote, Image: "copy:v2", Progress: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	if local != "copy:v2" || pulled != digest {
		t.Fatalf("Pull = %s %s, want copy:v2 %s", local, pulled, digest)
	}
	dstRef, _ := imageref.Parse("copy:v2")
	dst := dstRef.Dir(home)
	for _, f := range []string{"disk.img", "kernel", "initrd.cpio"} {
		if a, b := fileDigest(t, filepath.Join(srcDir, f)), fileDigest(t, filepath.Join(dst, f)); a != b {
			t.Errorf("%s differs after the round trip", f)
		}
	}
	srcAlloc, dstAlloc := allocated(t, filepath.Join(srcDir, "disk.img")), allocated(t, filepath.Join(dst, "disk.img"))
	t.Logf("disk.img allocated: source %d bytes, pulled %d bytes, logical %d", srcAlloc, dstAlloc, 64<<20)
	// The four random extents hold 1.3 MiB; APFS allocates in 16 KiB units.
	if dstAlloc > 2<<20 || dstAlloc > srcAlloc {
		t.Errorf("pulled disk.img allocates %d bytes, source %d: not sparse", dstAlloc, srcAlloc)
	}

	var ic imageConfig
	b, _ := os.ReadFile(filepath.Join(dst, "image.json"))
	if err := json.Unmarshal(b, &ic); err != nil {
		t.Fatal(err)
	}
	if ic.Name != "copy" || ic.Tag != "v2" || ic.Size != dstAlloc || ic.Config.Cmd[0] != "/bin/sh" || ic.Config.Labels["x"] != "y" {
		t.Errorf("image.json = %s", b)
	}
	for _, p := range []string{dst, filepath.Join(dst, "disk.img"), filepath.Join(dst, "image.json"), filepath.Join(dst, "kernel")} {
		st, _ := os.Stat(p)
		want := os.FileMode(0o600)
		if st.IsDir() {
			want = 0o700
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v", p, st.Mode().Perm(), want)
		}
	}

	// Name, tag and size stay out of the config blob, so the copy pushes to the same digest.
	if _, d3, err := Push(ctx, PushOptions{Home: home, Image: "copy:v2", Remote: remote, Version: "test"}); err != nil || d3 != digest {
		t.Fatalf("pushing the pulled copy = %s, %v; want %s", d3, err, digest)
	}

	if _, _, err := Pull(ctx, PullOptions{Home: home, Remote: remote, Image: "copy:v2"}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("pull over an existing image = %v, want a refusal", err)
	}
	refused := false
	_, _, err = Pull(ctx, PullOptions{Home: home, Remote: remote, Image: "copy:v2", Force: true, InUse: func(string) error {
		refused = true
		return fmt.Errorf("in use")
	}})
	if err == nil || !refused {
		t.Fatalf("pull --force over an image in use = %v", err)
	}
	if _, _, err := Pull(ctx, PullOptions{Home: home, Remote: remote, Image: "copy:v2", Force: true}); err != nil {
		t.Fatalf("pull --force: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "tmp")); len(entries) != 0 {
		t.Errorf("tmp not empty: %v", entries)
	}
}
