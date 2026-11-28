// Package upgrade replaces an installed kido with a signed release from GitHub.
package upgrade

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const DefaultBaseURL = "https://github.com/kido-sh/kido/releases"

const (
	sumsName   = "SHA256SUMS"
	sigName    = "SHA256SUMS.sig"
	maxSums    = 64 << 10
	maxSig     = 16 << 10
	maxTarball = 512 << 20
)

//go:embed release.pub
var releaseKey string

var ErrHomebrew = errors.New("this kido was installed with Homebrew: run `brew upgrade kido`")

var errNotFound = errors.New("not found")

var (
	versionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	tarballRE = tarballPattern(goruntime.GOOS, goruntime.GOARCH)
)

func tarballPattern(goos, goarch string) *regexp.Regexp {
	return regexp.MustCompile(`^kido-([0-9.]+)-` + regexp.QuoteMeta(goos) + `-` + regexp.QuoteMeta(goarch) + `\.tar\.gz$`)
}

// bin/kido comes last, so the helpers it starts are in place before it is.
var payload = []struct {
	name string
	mode os.FileMode
}{
	{"libexec/kido/kido-agent", 0o755},
	{"libexec/kido/kido-vmm", 0o755},
	{"libexec/kido/kido-vk-worker", 0o755},
	{"libexec/kido/kido-probe", 0o755},
	{"share/doc/kido/README.md", 0o644},
	{"share/doc/kido/LICENSE", 0o644},
	{"bin/kido", 0o755},
}

var required = requiredFiles(goruntime.GOOS, goruntime.GOARCH)

func requiredFiles(goos, goarch string) []string {
	if goos == "darwin" || (goos == "linux" && goarch == "arm64") {
		return []string{"bin/kido", "libexec/kido/kido-agent", "libexec/kido/kido-vmm"}
	}
	return []string{"bin/kido", "libexec/kido/kido-agent"}
}

var rename = os.Rename

type Options struct {
	Current string
	Want    string
	Check   bool
	Exe     string
	BaseURL string
	Key     string
	Client  *http.Client
	Log     io.Writer
}

type Result struct {
	Current, Release string
	Newer            bool
	Upgraded         bool
}

func Run(ctx context.Context, o Options) (Result, error) {
	res := Result{Current: o.Current}
	cur, ok := parseVersion(o.Current)
	if !ok {
		return res, fmt.Errorf("this kido (%s) was not installed from a release; reinstall it with https://kido.sh/install.sh", o.Current)
	}
	keyLine := o.Key
	if keyLine == "" {
		keyLine = releaseKey
	}
	key, err := parsePublicKey(keyLine)
	if err != nil {
		return res, err
	}
	prefix, err := installPrefix(o.Exe)
	if err != nil {
		return res, err
	}
	base := o.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	if !strings.HasPrefix(base, "https://") {
		return res, fmt.Errorf("%s: only https is allowed", base)
	}
	dir := base + "/latest/download"
	if o.Want != "" {
		if _, ok := parseVersion(o.Want); !ok {
			return res, fmt.Errorf("version %q: want X.Y.Z", o.Want)
		}
		if o.Want == o.Current {
			res.Release = o.Current
			return res, nil
		}
		dir = base + "/download/v" + o.Want
	}

	sums, err := o.fetch(ctx, dir+"/"+sumsName, maxSums)
	if errors.Is(err, errNotFound) {
		if o.Want != "" {
			return res, fmt.Errorf("there is no kido %s release at %s", o.Want, base)
		}
		return res, fmt.Errorf("there is no kido release at %s yet", base)
	}
	if err != nil {
		return res, err
	}
	sig, err := o.fetch(ctx, dir+"/"+sigName, maxSig)
	if err != nil {
		return res, err
	}
	if err := verifySSHSig(key, sums, sig); err != nil {
		return res, fmt.Errorf("%s: %w", sumsName, err)
	}
	name, version, want, err := pickTarball(sums)
	if err != nil {
		return res, err
	}
	if o.Want != "" && version != o.Want {
		return res, fmt.Errorf("release v%s lists kido %s", o.Want, version)
	}
	res.Release = version
	v, _ := parseVersion(version)
	res.Newer = compare(v, cur) > 0
	if version == o.Current || (o.Want == "" && !res.Newer) || o.Check {
		return res, nil
	}

	unlock, err := lock(filepath.Join(prefix, "libexec", "kido"))
	if err != nil {
		return res, err
	}
	defer unlock()
	removeLeftovers(prefix)
	o.logf("Downloading kido %s\n", version)
	tarball, err := o.download(ctx, base+"/download/v"+version+"/"+name, want)
	if err != nil {
		return res, err
	}
	defer os.Remove(tarball)
	o.logf("Verified the signature and the SHA-256\n")
	files, err := stage(tarball, prefix)
	defer func() {
		for _, f := range files {
			os.Remove(f.tmp)
		}
	}()
	if err != nil {
		return res, err
	}
	if err := check(ctx, files, version); err != nil {
		return res, err
	}
	if err := swap(files); err != nil {
		return res, err
	}
	res.Upgraded = true
	return res, nil
}

func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return v, false
	}
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func compare(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func installPrefix(exe string) (string, error) {
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	if strings.Contains(real, "/Cellar/") {
		return "", ErrHomebrew
	}
	bin := filepath.Dir(real)
	prefix := filepath.Dir(bin)
	if filepath.Base(real) != "kido" || filepath.Base(bin) != "bin" {
		return "", fmt.Errorf("%s is not PREFIX/bin/kido; reinstall it with https://kido.sh/install.sh", real)
	}
	if fi, err := os.Stat(filepath.Join(prefix, "libexec", "kido")); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s has no libexec/kido; reinstall kido with https://kido.sh/install.sh", prefix)
	}
	return prefix, nil
}

func pickTarball(sums []byte) (name, version, sum string, err error) {
	return pickTarballFor(sums, tarballRE)
}

func pickTarballFor(sums []byte, re *regexp.Regexp) (name, version, sum string, err error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		file := strings.TrimPrefix(f[1], "*")
		m := re.FindStringSubmatch(file)
		if m == nil {
			continue
		}
		if name != "" {
			return "", "", "", fmt.Errorf("%s lists more than one kido tarball", sumsName)
		}
		if _, ok := parseVersion(m[1]); !ok || len(f[0]) != 64 {
			return "", "", "", fmt.Errorf("%s: bad line for %s", sumsName, file)
		}
		name, version, sum = file, m[1], strings.ToLower(f[0])
	}
	if name == "" {
		return "", "", "", fmt.Errorf("%s lists no kido tarball", sumsName)
	}
	return name, version, sum, nil
}

func lock(dir string) (func(), error) {
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(d.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		d.Close()
		return nil, errors.New("another kido upgrade is running")
	}
	return func() { d.Close() }, nil
}

func (o *Options) logf(format string, args ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format, args...)
	}
}

func (o *Options) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return &http.Client{
		Timeout: 15 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("refusing a redirect to a non-https URL")
			}
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

func (o *Options) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kido/"+o.Current)
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%s: %w", url, errNotFound)
		}
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

func (o *Options) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	body, err := o.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, limit)
	}
	return b, nil
}

func (o *Options) download(ctx context.Context, url, want string) (string, error) {
	body, err := o.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	f, err := os.CreateTemp("", "kido-upgrade-*.tar.gz")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, maxTarball+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxTarball {
		err = fmt.Errorf("%s: larger than %d bytes", url, maxTarball)
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		err = fmt.Errorf("%s does not match its SHA-256 in %s", path.Base(url), sumsName)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// It runs under the lock, so no other upgrade owns the files it removes.
func removeLeftovers(prefix string) {
	for _, dir := range []string{"bin", "libexec/kido", "share/doc/kido"} {
		dir = filepath.Join(prefix, filepath.FromSlash(dir))
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".kido-upgrade-") {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}

type staged struct {
	name, tmp, dst, backup string
}

func stage(tarball, prefix string) ([]staged, error) {
	return stageFor(tarball, prefix, required)
}

func stageFor(tarball, prefix string, req []string) ([]staged, error) {
	f, err := os.Open(tarball)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	modes := map[string]os.FileMode{}
	dirs := map[string]bool{}
	for _, p := range payload {
		modes[p.name] = p.mode
		for d := path.Dir(p.name); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	got := map[string]staged{}
	var files []staged
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return files, err
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		switch {
		case hdr.Typeflag == tar.TypeDir && dirs[name]:
			continue
		case hdr.Typeflag != tar.TypeReg || modes[name] == 0:
			return files, fmt.Errorf("the release has an unexpected entry %q", hdr.Name)
		case got[name].tmp != "":
			return files, fmt.Errorf("the release has %s twice", name)
		}
		dst := filepath.Join(prefix, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return files, err
		}
		out, err := os.CreateTemp(filepath.Dir(dst), ".kido-upgrade-*")
		if err != nil {
			return files, err
		}
		s := staged{name: name, tmp: out.Name(), dst: dst}
		files = append(files, s)
		got[name] = s
		_, err = io.Copy(out, io.LimitReader(tr, maxTarball))
		if err == nil {
			err = out.Chmod(modes[name])
		}
		if err == nil {
			err = out.Sync()
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return files, err
		}
	}
	for _, r := range req {
		if got[r].tmp == "" {
			return files, fmt.Errorf("the release has no %s", r)
		}
	}
	ordered := files[:0:0]
	for _, p := range payload {
		if s, ok := got[p.name]; ok {
			ordered = append(ordered, s)
		}
	}
	return ordered, nil
}

func check(ctx context.Context, files []staged, version string) error {
	for _, f := range files {
		if f.name != "bin/kido" {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, f.tmp, "--version").Output()
		if err != nil {
			return fmt.Errorf("the new kido does not run: %w", err)
		}
		if got := strings.TrimSpace(string(out)); got != "kido version "+version {
			return fmt.Errorf("the new kido reports %q, not version %s", got, version)
		}
		return nil
	}
	return errors.New("the release has no bin/kido")
}

func swap(files []staged) error {
	var done []staged
	for _, f := range files {
		if _, err := os.Lstat(f.dst); err == nil {
			f.backup = f.tmp + ".old"
			if err := os.Link(f.dst, f.backup); err != nil {
				undo(done)
				return err
			}
		}
		if err := rename(f.tmp, f.dst); err != nil {
			if f.backup != "" {
				os.Remove(f.backup)
			}
			undo(done)
			return err
		}
		done = append(done, f)
	}
	for _, f := range done {
		if f.backup != "" {
			os.Remove(f.backup)
		}
	}
	return nil
}

func undo(done []staged) {
	for i := len(done) - 1; i >= 0; i-- {
		if f := done[i]; f.backup != "" {
			os.Rename(f.backup, f.dst)
		} else {
			os.Remove(f.dst)
		}
	}
}
