package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kido-sh/kido/internal/imageref"
)

const (
	dockerHubHost    = "registry-1.docker.io"
	dockerHubAuthKey = "https://index.docker.io/v1/"
)

// remote is an image reference resolved to a registry endpoint.
type remote struct {
	ref      imageref.Ref
	host     string // registry API host[:port]
	repo     string // repository path on that registry
	authKey  string // key of the registry in config.json
	insecure bool   // plain HTTP: loopback registries only
}

func parseRemote(s string) (remote, error) {
	ref, err := imageref.Parse(s)
	if err != nil {
		return remote{}, err
	}
	r := remote{ref: ref, host: dockerHubHost, repo: ref.Name}
	if first, rest, ok := strings.Cut(ref.Name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.host, r.repo = first, rest
	}
	if r.host == "docker.io" || r.host == "index.docker.io" {
		r.host = dockerHubHost
	}
	r.authKey = r.host
	if r.host == dockerHubHost {
		r.authKey = dockerHubAuthKey
		if !strings.Contains(r.repo, "/") {
			r.repo = "library/" + r.repo
		}
	}
	r.insecure = isLoopback(r.host)
	return r, nil
}

func isLoopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type client struct {
	r     remote
	hc    *http.Client
	base  string
	canon string // canonicalHost of the registry
	cred  credential
	scope string
	authz string
}

func newClient(ctx context.Context, r remote, push bool) (*client, error) {
	cred, err := lookupCredential(ctx, r.authKey)
	if err != nil {
		return nil, err
	}
	scope := "repository:" + r.repo + ":pull"
	if push {
		scope += ",push"
	}
	c := newClientWith(r, cred, scope)
	return c, c.login(ctx)
}

func newClientWith(r remote, cred credential, scope string) *client {
	scheme := "https"
	if r.insecure {
		scheme = "http"
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 5 * time.Minute
	base := scheme + "://" + r.host
	u, _ := url.Parse(base)
	c := &client{
		r:     r,
		base:  base,
		canon: canonicalHost(u),
		cred:  cred,
		scope: scope,
	}
	c.hc = &http.Client{Transport: tr, CheckRedirect: c.checkRedirect}
	return c
}

// checkRedirect keeps the registry's credentials off every other host: net/http still copies
// Authorization to a subdomain or to another port of the same host before asking.
func (c *client) checkRedirect(req *http.Request, via []*http.Request) error {
	if !c.isRegistry(req.URL) {
		req.Header.Del("Authorization")
	}
	return refuseDowngrade(req, via)
}

// canonicalHost is host:port with the scheme's default port filled in.
func canonicalHost(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	return strings.ToLower(u.Hostname()) + ":" + port
}

// isRegistry tells requests to the registry itself, which alone get its credentials and may
// ask for them, from redirects to storage hosts and upload locations elsewhere.
func (c *client) isRegistry(u *url.URL) bool { return canonicalHost(u) == c.canon }

func refuseDowngrade(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect from HTTPS to %s://%s", req.URL.Scheme, printable(req.URL.Host))
	}
	return nil
}

// sameHostOnly keeps a token request, which carries the password or refresh token, on its realm.
func sameHostOnly(req *http.Request, via []*http.Request) error {
	if canonicalHost(req.URL) != canonicalHost(via[0].URL) {
		return fmt.Errorf("refusing token redirect to %s", printable(req.URL.Host))
	}
	return refuseDowngrade(req, via)
}

// login pings /v2/ and sets up whatever authorization the registry asks for.
func (c *client) login(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	switch {
	case !c.isRegistry(resp.Request.URL):
		return fmt.Errorf("%s: /v2/ redirected to %s", c.r.host, printable(resp.Request.URL.Host))
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return c.authorize(ctx, resp.Header.Values("WWW-Authenticate"))
	default:
		return responseError(resp, c.r.host)
	}
}

func (c *client) authorize(ctx context.Context, challenges []string) error {
	scheme, params := pickChallenge(challenges)
	switch scheme {
	case "basic":
		if c.cred.Username != "" {
			c.authz = "Basic " + basicAuth(c.cred)
		}
		return nil
	case "bearer":
		tok, err := c.fetchToken(ctx, params)
		if err != nil {
			return err
		}
		c.authz = "Bearer " + tok
		return nil
	default:
		return fmt.Errorf("%s: unsupported authentication challenge %q", c.r.host, printable(strings.Join(challenges, "; ")))
	}
}

func basicAuth(c credential) string {
	return base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))
}

func pickChallenge(challenges []string) (string, map[string]string) {
	var scheme string
	var params map[string]string
	for _, h := range challenges {
		s, p := parseChallenge(h)
		if s == "bearer" || scheme == "" {
			scheme, params = s, p
		}
	}
	return scheme, params
}

// parseChallenge reads `Bearer realm="...",service="...",scope="a:b:pull,push"`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for {
		rest = strings.TrimLeft(rest, " ,")
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var val strings.Builder
		if strings.HasPrefix(after, `"`) {
			i := 1
			for ; i < len(after) && after[i] != '"'; i++ {
				if after[i] == '\\' && i+1 < len(after) {
					i++
				}
				val.WriteByte(after[i])
			}
			rest = after[min(i+1, len(after)):]
		} else {
			v, r, _ := strings.Cut(after, ",")
			val.WriteString(strings.TrimSpace(v))
			rest = r
		}
		params[key] = val.String()
	}
	return strings.ToLower(scheme), params
}

func (c *client) fetchToken(ctx context.Context, p map[string]string) (string, error) {
	realm, err := url.Parse(p["realm"])
	if err != nil || realm.Host == "" {
		return "", fmt.Errorf("%s: invalid token realm %q", c.r.host, printable(p["realm"]))
	}
	if realm.Scheme != "https" && !(realm.Scheme == "http" && isLoopback(realm.Host)) {
		return "", fmt.Errorf("%s: refusing token realm %s://%s: not HTTPS", c.r.host, printable(realm.Scheme), printable(realm.Host))
	}
	realmHost := printable(realm.Host)
	var req *http.Request
	if c.cred.IdentityToken != "" {
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {c.cred.IdentityToken},
			"service":       {p["service"]},
			"scope":         {c.scope},
			"client_id":     {"kido"},
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, realm.String(), strings.NewReader(form.Encode()))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		q := realm.Query()
		if s := p["service"]; s != "" {
			q.Set("service", s)
		}
		if c.scope != "" {
			q.Set("scope", c.scope)
		}
		realm.RawQuery = q.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
		if err != nil {
			return "", err
		}
		if c.cred.Username != "" {
			req.SetBasicAuth(c.cred.Username, c.cred.Password)
		}
	}
	tc := &http.Client{Transport: c.hc.Transport, CheckRedirect: sameHostOnly}
	resp, err := tc.Do(req)
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", responseError(resp, "token from "+realmHost)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", fmt.Errorf("token from %s: %w", realmHost, err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	if t.Token == "" {
		return "", fmt.Errorf("token from %s: empty token", realmHost)
	}
	return t.Token, nil
}

// do sends req with the registry's authorization (only to the registry host itself) and
// re-authorizes once when the registry itself answers 401, e.g. after a token expired.
func (c *client) do(req *http.Request) (*http.Response, error) {
	for retried := false; ; retried = true {
		if c.authz != "" && c.isRegistry(req.URL) {
			req.Header.Set("Authorization", c.authz)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || retried || !c.isRegistry(resp.Request.URL) ||
			(req.Body != nil && req.GetBody == nil) {
			return resp, nil
		}
		challenges := resp.Header.Values("WWW-Authenticate")
		if len(challenges) == 0 {
			return resp, nil
		}
		drain(resp)
		if err := c.authorize(req.Context(), challenges); err != nil {
			return nil, err
		}
		if req.GetBody != nil {
			if req.Body, err = req.GetBody(); err != nil {
				return nil, err
			}
		}
	}
}

func (c *client) url(path string) string { return c.base + "/v2/" + c.r.repo + path }

func (c *client) blobExists(ctx context.Context, d descriptor) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.url("/blobs/"+d.Digest), nil)
	if err != nil {
		return false, err
	}
	resp, err := c.do(req)
	if err != nil {
		return false, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, responseError(resp, "checking blob "+d.Digest)
}

// uploadBlob streams a blob in one PATCH and commits it with its digest.
func (c *client) uploadBlob(ctx context.Context, d descriptor, src io.ReaderAt, p *progress) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/blobs/uploads/"), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted {
		defer drain(resp)
		return responseError(resp, "starting upload")
	}
	drain(resp)
	loc, err := c.uploadLocation(resp)
	if err != nil {
		return err
	}

	body := func() io.ReadCloser {
		p.reset()
		return io.NopCloser(p.reader(io.NewSectionReader(src, 0, d.Size)))
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPatch, loc.String(), body())
	if err != nil {
		return err
	}
	req.ContentLength = d.Size
	req.GetBody = func() (io.ReadCloser, error) { return body(), nil }
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err = c.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		defer drain(resp)
		return responseError(resp, "uploading "+d.Digest)
	}
	drain(resp)
	if loc, err = c.uploadLocation(resp); err != nil {
		return err
	}

	q := loc.Query()
	q.Set("digest", d.Digest)
	loc.RawQuery = q.Encode()
	req, err = http.NewRequestWithContext(ctx, http.MethodPut, loc.String(), nil)
	if err != nil {
		return err
	}
	resp, err = c.do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated {
		return responseError(resp, "committing "+d.Digest)
	}
	return nil
}

func (c *client) uploadLocation(resp *http.Response) (*url.URL, error) {
	loc, err := resp.Location()
	if err != nil {
		return nil, fmt.Errorf("%s: upload without a usable Location: %w", c.r.host, err)
	}
	if strings.HasPrefix(c.base, "https:") && loc.Scheme != "https" {
		return nil, fmt.Errorf("%s: refusing upload location %s://%s", c.r.host, printable(loc.Scheme), printable(loc.Host))
	}
	return loc, nil
}

func (c *client) putManifest(ctx context.Context, ref, mediaType string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url("/manifests/"+ref), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mediaType)
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated {
		return responseError(resp, "putting manifest "+ref)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != "" && got != digestOf(body) {
		return fmt.Errorf("registry stored manifest %s as %s", digestOf(body), printable(got))
	}
	return nil
}

// getManifest returns the manifest bytes and their media type, refusing a body whose own
// mediaType disagrees with the Content-Type header or whose digest disagrees with
// Docker-Content-Digest.
func (c *client) getManifest(ctx context.Context, ref string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/manifests/"+ref), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", mediaTypeIndex+", "+mediaTypeManifest)
	resp, err := c.do(req)
	if err != nil {
		return nil, "", err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, "", responseError(resp, "manifest "+c.r.repo+":"+ref)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxManifestSize {
		return nil, "", fmt.Errorf("manifest %s is larger than %d bytes", ref, maxManifestSize)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != "" && got != digestOf(body) {
		return nil, "", fmt.Errorf("manifest %s: digest %s, registry says %s", ref, digestOf(body), printable(got))
	}
	var probe struct {
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, "", fmt.Errorf("manifest %s: %w", ref, err)
	}
	header, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	mt := probe.MediaType
	if mt == "" {
		mt = header
	}
	if (header == mediaTypeIndex || header == mediaTypeManifest) && header != mt {
		return nil, "", fmt.Errorf("manifest %s: Content-Type %q but mediaType %q", ref, printable(header), printable(mt))
	}
	return body, mt, nil
}

// fetchBlob copies a blob to w, failing unless it has exactly the size and digest of d.
func (c *client) fetchBlob(ctx context.Context, d descriptor, w io.Writer, p *progress) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/blobs/"+d.Digest), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return responseError(resp, "blob "+d.Digest)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), p.reader(io.LimitReader(resp.Body, d.Size+1)))
	if err != nil {
		return err
	}
	if n != d.Size {
		return fmt.Errorf("blob %s: got %d bytes, want %d", d.Digest, n, d.Size)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != d.Digest {
		return fmt.Errorf("blob %s: digest mismatch (got %s)", d.Digest, got)
	}
	return nil
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

func responseError(resp *http.Response, what string) error {
	msg := printable(resp.Status)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var e struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &e) == nil {
		for _, x := range e.Errors {
			msg += ": " + printable(strings.TrimSpace(x.Code+" "+x.Message))
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		msg += " (log in with `kido login`)"
	}
	return fmt.Errorf("%s: %s", what, msg)
}
