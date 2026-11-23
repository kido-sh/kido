package registry

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Login checks a username and password against a registry and stores them. It returns where
// the credential was stored.
func Login(ctx context.Context, server, username, password string) (string, error) {
	r, err := parseServer(server)
	if err != nil {
		return "", err
	}
	if username == "" || password == "" {
		return "", fmt.Errorf("username and password are required")
	}
	cred := credential{Username: username, Password: password}
	c := newClientWith(r, cred, "")
	if err := c.login(ctx); err != nil {
		return "", err
	}
	if c.authz == "" {
		c.authz = "Basic " + basicAuth(cred)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v2/", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", c.authz)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", responseError(resp, "login to "+r.host)
	}
	return storeCredential(ctx, r.authKey, cred)
}

// parseServer accepts a host[:port], optionally with a scheme and a path. Empty means docker.io.
func parseServer(s string) (remote, error) {
	host := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(s), "https://"), "http://")
	host, _, _ = strings.Cut(host, "/")
	if host == "" {
		host = "docker.io"
	}
	r, err := parseRemote(host + "/login")
	if err != nil || (r.host == dockerHubHost && host != "docker.io" && host != "index.docker.io" && host != dockerHubHost) {
		return remote{}, fmt.Errorf("invalid registry address %q", printable(s))
	}
	return r, nil
}
