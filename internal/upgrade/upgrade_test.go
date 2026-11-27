package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

type signer struct {
	priv ed25519.PrivateKey
	line string
}

func newSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return signer{priv, "ssh-ed25519 " + base64.StdEncoding.EncodeToString(keyBlob(pub)) + " test"}
}

func wireString(b []byte) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...)
}

func keyBlob(pub ed25519.PublicKey) []byte {
	return append(wireString([]byte("ssh-ed25519")), wireString(pub)...)
}

func (s signer) sign(msg []byte) []byte {
	digest := sha512.Sum512(msg)
	fields := [][]byte{[]byte(Namespace), nil, []byte("sha512")}
	signed := []byte("SSHSIG")
	for _, f := range append(fields, digest[:]) {
		signed = append(signed, wireString(f)...)
	}
	sig := append(wireString([]byte("ssh-ed25519")), wireString(ed25519.Sign(s.priv, signed))...)
	blob := binary.BigEndian.AppendUint32([]byte("SSHSIG"), 1)
	blob = append(blob, wireString(keyBlob(s.priv.Public().(ed25519.PublicKey)))...)
	for _, f := range fields {
		blob = append(blob, wireString(f)...)
	}
	blob = append(blob, wireString(sig)...)
	return []byte("-----BEGIN SSH SIGNATURE-----\n" + base64.StdEncoding.EncodeToString(blob) + "\n-----END SSH SIGNATURE-----\n")
}

func fakeKido(version string) string {
	return "#!/bin/sh\necho 'kido version " + version + "'\n"
}

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, d := range []string{"bin/", "libexec/", "libexec/kido/", "share/", "share/doc/", "share/doc/kido/"} {
		if err := tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	gz.Close()
	return buf.Bytes()
}

func releaseFiles(version string) map[string]string {
	m := map[string]string{
		"bin/kido":                 fakeKido(version),
		"libexec/kido/kido-agent":  "agent " + version,
		"share/doc/kido/README.md": "readme " + version,
		"share/doc/kido/LICENSE":   "license",
	}
	if goruntime.GOOS == "darwin" || (goruntime.GOOS == "linux" && goruntime.GOARCH == "arm64") {
		m["libexec/kido/kido-vmm"] = "vmm " + version
	}
	return m
}

func testTarballName(version string) string {
	return "kido-" + version + "-" + goruntime.GOOS + "-" + goruntime.GOARCH + ".tar.gz"
}

type release struct {
	version string
	files   map[string]string
	sums    []byte
	sig     []byte
}

func serve(t *testing.T, s signer, latest string, rels ...release) *httptest.Server {
	t.Helper()
	paths := map[string][]byte{}
	for _, r := range rels {
		name := testTarballName(r.version)
		tgz := tarball(t, r.files)
		sum := sha256.Sum256(tgz)
		sums := r.sums
		if sums == nil {
			sums = fmt.Appendf(nil, "%s  %s\n%s  install.sh\n", hex.EncodeToString(sum[:]), name, strings.Repeat("0", 64))
		}
		sig := r.sig
		if sig == nil {
			sig = s.sign(sums)
		}
		dirs := []string{"/releases/download/v" + r.version}
		if r.version == latest {
			dirs = append(dirs, "/releases/latest/download")
		}
		for _, d := range dirs {
			paths[d+"/SHA256SUMS"], paths[d+"/SHA256SUMS.sig"] = sums, sig
		}
		paths["/releases/download/v"+r.version+"/"+name] = tgz
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := paths[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func install(t *testing.T, version string) (string, map[string]string) {
	t.Helper()
	prefix := t.TempDir()
	files := releaseFiles(version)
	for name, body := range files {
		p := filepath.Join(prefix, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return prefix, files
}

func options(srv *httptest.Server, s signer, prefix, current string) Options {
	return Options{Current: current, Exe: filepath.Join(prefix, "bin", "kido"), BaseURL: srv.URL + "/releases", Key: s.line, Client: srv.Client()}
}

func installed(t *testing.T, prefix string, want map[string]string) {
	t.Helper()
	for name, body := range want {
		b, err := os.ReadFile(filepath.Join(prefix, name))
		if err != nil || string(b) != body {
			t.Errorf("%s = %q, %v; want %q", name, b, err, body)
		}
	}
	for _, dir := range []string{"bin", "libexec/kido", "share/doc/kido"} {
		entries, _ := os.ReadDir(filepath.Join(prefix, dir))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".kido-upgrade-") {
				t.Errorf("leftover %s/%s", dir, e.Name())
			}
		}
	}
}

func TestUpgrade(t *testing.T) {
	s := newSigner(t)
	srv := serve(t, s, "0.2.0", release{version: "0.2.0", files: releaseFiles("0.2.0")})
	prefix, _ := install(t, "0.1.0")
	res, err := Run(context.Background(), options(srv, s, prefix, "0.1.0"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Upgraded || !res.Newer || res.Release != "0.2.0" {
		t.Errorf("result %+v", res)
	}
	installed(t, prefix, releaseFiles("0.2.0"))
	wantModes := map[string]os.FileMode{
		"bin/kido":                0o755,
		"libexec/kido/kido-agent": 0o755,
		"share/doc/kido/LICENSE":  0o644,
	}
	if goruntime.GOOS == "darwin" || (goruntime.GOOS == "linux" && goruntime.GOARCH == "arm64") {
		wantModes["libexec/kido/kido-vmm"] = 0o755
	}
	for name, mode := range wantModes {
		if fi, err := os.Stat(filepath.Join(prefix, name)); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s mode %v, %v; want %v", name, fi.Mode().Perm(), err, mode)
		}
	}
}

func TestNoUpgrade(t *testing.T) {
	s := newSigner(t)
	srv := serve(t, s, "0.2.0", release{version: "0.2.0", files: releaseFiles("0.2.0")})
	for _, c := range []struct {
		name, current string
		check, newer  bool
	}{
		{"up to date", "0.2.0", false, false},
		{"newer than the latest", "0.3.0", false, false},
		{"check only", "0.1.0", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			prefix, old := install(t, c.current)
			o := options(srv, s, prefix, c.current)
			o.Check = c.check
			res, err := Run(context.Background(), o)
			if err != nil || res.Upgraded || res.Newer != c.newer || res.Release != "0.2.0" {
				t.Errorf("result %+v, %v", res, err)
			}
			installed(t, prefix, old)
		})
	}
}

func TestExplicitVersion(t *testing.T) {
	s := newSigner(t)
	srv := serve(t, s, "0.3.0", release{version: "0.3.0", files: releaseFiles("0.3.0")}, release{version: "0.2.0", files: releaseFiles("0.2.0")})
	prefix, _ := install(t, "0.3.0")
	o := options(srv, s, prefix, "0.3.0")
	o.Want = "0.2.0"
	if res, err := Run(context.Background(), o); err != nil || !res.Upgraded {
		t.Fatalf("result %+v, %v", res, err)
	}
	installed(t, prefix, releaseFiles("0.2.0"))
}

func TestRefused(t *testing.T) {
	s, other := newSigner(t), newSigner(t)
	good := releaseFiles("0.2.0")
	sums := []byte(strings.Repeat("0", 64) + "  " + testTarballName("0.2.0") + "\n")
	withEntry := func(name, body string) map[string]string {
		m := releaseFiles("0.2.0")
		m[name] = body
		return m
	}
	missingHelper := "libexec/kido/kido-agent"
	if goruntime.GOOS == "darwin" || (goruntime.GOOS == "linux" && goruntime.GOARCH == "arm64") {
		missingHelper = "libexec/kido/kido-vmm"
	}
	without := releaseFiles("0.2.0")
	delete(without, missingHelper)
	broken := releaseFiles("0.2.0")
	broken["bin/kido"] = fakeKido("0.1.9")
	for _, c := range []struct {
		name string
		rel  release
		want string
	}{
		{"another key", release{version: "0.2.0", files: good, sig: other.sign([]byte("x"))}, "signed by another key"},
		{"changed after signing", release{version: "0.2.0", files: good, sig: s.sign([]byte("x"))}, "bad signature"},
		{"checksum", release{version: "0.2.0", files: good, sums: sums}, "does not match its SHA-256"},
		{"unexpected file", release{version: "0.2.0", files: withEntry("bin/other", "x")}, "unexpected entry"},
		{"path escape", release{version: "0.2.0", files: withEntry("../evil", "x")}, "unexpected entry"},
		{"missing helper", release{version: "0.2.0", files: without}, "no " + missingHelper},
		{"new kido reports another version", release{version: "0.2.0", files: broken}, "not version 0.2.0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.rel.sums != nil && c.rel.sig == nil {
				c.rel.sig = s.sign(c.rel.sums)
			}
			srv := serve(t, s, "0.2.0", c.rel)
			prefix, old := install(t, "0.1.0")
			res, err := Run(context.Background(), options(srv, s, prefix, "0.1.0"))
			if err == nil || !strings.Contains(err.Error(), c.want) || res.Upgraded {
				t.Errorf("result %+v, %v; want an error with %q", res, err, c.want)
			}
			installed(t, prefix, old)
		})
	}
}

func TestNoRelease(t *testing.T) {
	s := newSigner(t)
	srv := serve(t, s, "", release{version: "0.2.0", files: releaseFiles("0.2.0")})
	prefix, old := install(t, "0.1.0")
	if _, err := Run(context.Background(), options(srv, s, prefix, "0.1.0")); err == nil || !strings.Contains(err.Error(), "no kido release") {
		t.Errorf("latest: %v", err)
	}
	o := options(srv, s, prefix, "0.1.0")
	o.Want = "0.1.5"
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "no kido 0.1.5 release") {
		t.Errorf("--version 0.1.5: %v", err)
	}
	o.Want = "0.1.0"
	if res, err := Run(context.Background(), o); err != nil || res.Upgraded || res.Release != "0.1.0" {
		t.Errorf("--version of this kido: %+v, %v", res, err)
	}
	installed(t, prefix, old)
}

func TestRollback(t *testing.T) {
	s := newSigner(t)
	srv := serve(t, s, "0.2.0", release{version: "0.2.0", files: releaseFiles("0.2.0")})
	prefix, old := install(t, "0.1.0")
	rename = func(from, to string) error {
		if filepath.Base(to) == "kido" {
			return errors.New("injected")
		}
		return os.Rename(from, to)
	}
	defer func() { rename = os.Rename }()
	if _, err := Run(context.Background(), options(srv, s, prefix, "0.1.0")); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("err %v, want the injected one", err)
	}
	installed(t, prefix, old)
}

func TestInstallKind(t *testing.T) {
	s := newSigner(t)
	srv := serve(t, s, "0.2.0", release{version: "0.2.0", files: releaseFiles("0.2.0")})
	prefix, _ := install(t, "0.1.0")
	cellar := filepath.Join(t.TempDir(), "Cellar", "kido", "0.1.0", "bin")
	if err := os.MkdirAll(cellar, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cellar, "kido"), nil, 0o755)
	o := options(srv, s, prefix, "0.1.0")
	o.Exe = filepath.Join(cellar, "kido")
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrHomebrew) {
		t.Errorf("Homebrew: %v", err)
	}
	if _, err := Run(context.Background(), options(srv, s, prefix, "1d9e608-dirty")); err == nil || !strings.Contains(err.Error(), "not installed from a release") {
		t.Errorf("source build: %v", err)
	}
}

func TestSSHKeygenSignature(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	msg := filepath.Join(dir, "SHA256SUMS")
	os.WriteFile(msg, []byte("abc  kido-0.2.0-darwin-arm64.tar.gz\n"), 0o644)
	if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, ns := range []string{Namespace, "file"} {
		os.Remove(msg + ".sig")
		if out, err := exec.Command(keygen, "-Y", "sign", "-q", "-f", key, "-n", ns, msg).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		pubLine, _ := os.ReadFile(key + ".pub")
		pub, err := parsePublicKey(string(pubLine))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := os.ReadFile(msg)
		sig, _ := os.ReadFile(msg + ".sig")
		err = verifySSHSig(pub, body, sig)
		if ns == Namespace && err != nil {
			t.Errorf("ssh-keygen's signature: %v", err)
		}
		if ns != Namespace && err == nil {
			t.Errorf("a signature in namespace %q was accepted", ns)
		}
		if ns == Namespace && verifySSHSig(pub, append(body, '\n'), sig) == nil {
			t.Error("a changed file was accepted")
		}
	}
}

func TestReleaseKey(t *testing.T) {
	if _, err := parsePublicKey(releaseKey); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	want := "release_key='" + strings.Join(strings.Fields(releaseKey)[:2], " ") + "'\n"
	if !strings.Contains(string(script), "\n"+want) {
		t.Errorf("install.sh does not set %s", strings.TrimSpace(want))
	}
}

func TestLinuxTarballAndRequired(t *testing.T) {
	darwinSum := strings.Repeat("a", 64)
	linuxSum := strings.Repeat("b", 64)
	sums := []byte(fmt.Sprintf("%s  kido-0.2.0-darwin-arm64.tar.gz\n%s  kido-0.2.0-linux-arm64.tar.gz\n%s  install.sh\n",
		darwinSum, linuxSum, strings.Repeat("0", 64)))

	name, ver, sum, err := pickTarballFor(sums, tarballPattern("linux", "arm64"))
	if err != nil {
		t.Fatalf("pickTarballFor linux/arm64: %v", err)
	}
	if name != "kido-0.2.0-linux-arm64.tar.gz" || ver != "0.2.0" || sum != linuxSum {
		t.Fatalf("got (%q, %q, %q)", name, ver, sum)
	}

	name, ver, sum, err = pickTarballFor(sums, tarballPattern("darwin", "arm64"))
	if err != nil || name != "kido-0.2.0-darwin-arm64.tar.gz" || ver != "0.2.0" || sum != darwinSum {
		t.Fatalf("darwin/arm64: got (%q, %q, %q), %v", name, ver, sum, err)
	}

	linuxFiles := map[string]string{
		"bin/kido":                 fakeKido("0.2.0"),
		"libexec/kido/kido-agent":  "agent 0.2.0",
		"libexec/kido/kido-vmm":    "vmm 0.2.0",
		"share/doc/kido/README.md": "readme",
		"share/doc/kido/LICENSE":   "license",
	}
	tgzPath := filepath.Join(t.TempDir(), "linux.tar.gz")
	if err := os.WriteFile(tgzPath, tarball(t, linuxFiles), 0o600); err != nil {
		t.Fatal(err)
	}

	stagedFiles, err := stageFor(tgzPath, t.TempDir(), requiredFiles("linux", "arm64"))
	if err != nil {
		t.Fatalf("stageFor linux/arm64: %v", err)
	}
	if len(stagedFiles) != len(linuxFiles) {
		t.Fatalf("staged %d files, want %d", len(stagedFiles), len(linuxFiles))
	}

	delete(linuxFiles, "libexec/kido/kido-vmm")
	noVMMPath := filepath.Join(t.TempDir(), "no-vmm.tar.gz")
	if err := os.WriteFile(noVMMPath, tarball(t, linuxFiles), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stageFor(noVMMPath, t.TempDir(), requiredFiles("linux", "arm64")); err == nil || !strings.Contains(err.Error(), "no libexec/kido/kido-vmm") {
		t.Fatalf("linux/arm64 stage without kido-vmm: %v", err)
	}
	if _, err := stageFor(noVMMPath, t.TempDir(), requiredFiles("linux", "amd64")); err != nil {
		t.Fatalf("linux/amd64 stage without kido-vmm: %v", err)
	}
	if _, err := stageFor(noVMMPath, t.TempDir(), requiredFiles("darwin", "arm64")); err == nil || !strings.Contains(err.Error(), "no libexec/kido/kido-vmm") {
		t.Fatalf("darwin/arm64 stage without kido-vmm: %v", err)
	}

	linuxFiles["libexec/kido/kido-vmm"] = "vmm 0.2.0"
	delete(linuxFiles, "libexec/kido/kido-agent")
	noAgentPath := filepath.Join(t.TempDir(), "no-agent.tar.gz")
	if err := os.WriteFile(noAgentPath, tarball(t, linuxFiles), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stageFor(noAgentPath, t.TempDir(), requiredFiles("linux", "arm64")); err == nil || !strings.Contains(err.Error(), "no libexec/kido/kido-agent") {
		t.Fatalf("linux/arm64 stage without kido-agent: %v", err)
	}
}
