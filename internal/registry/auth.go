package registry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// credential is what config.json or a credential helper holds for one registry.
type credential struct {
	Username      string
	Password      string
	IdentityToken string
}

type authEntry struct {
	Auth          string `json:"auth,omitempty"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
}

type credConfig struct {
	Auths       map[string]authEntry `json:"auths"`
	CredsStore  string               `json:"credsStore"`
	CredHelpers map[string]string    `json:"credHelpers"`
}

var helperNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func credConfigPath() (string, error) {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return filepath.Join(d, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}

func loadCredConfig() (*credConfig, error) {
	path, err := credConfigPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &credConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c credConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// authKeyHost reduces a Docker config key ("https://index.docker.io/v1/", "localhost:5001") to its host.
func authKeyHost(k string) string {
	k = strings.TrimPrefix(strings.TrimPrefix(k, "https://"), "http://")
	k, _, _ = strings.Cut(k, "/")
	return strings.ToLower(k)
}

// matchKey picks key itself, else the one other key naming the same host (sorted, so the pick
// does not depend on map order).
func matchKey[V any](m map[string]V, key string) (V, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if authKeyHost(k) == authKeyHost(key) {
			keys = append(keys, k)
		}
	}
	var zero V
	if len(keys) == 0 {
		return zero, false
	}
	slices.Sort(keys)
	return m[keys[0]], true
}

func (c *credConfig) helperFor(key string) string {
	if h, ok := matchKey(c.CredHelpers, key); ok {
		return h
	}
	return c.CredsStore
}

// lookupCredential finds the credential stored for key: a per-registry helper, then the default
// store, then the config file itself. No credential means anonymous access.
func lookupCredential(ctx context.Context, key string) (credential, error) {
	cfg, err := loadCredConfig()
	if err != nil {
		return credential{}, err
	}
	if helper := cfg.helperFor(key); helper != "" {
		c, found, err := helperGet(ctx, helper, key)
		if err != nil {
			return credential{}, err
		}
		if found {
			return c, nil
		}
	}
	a, ok := matchKey(cfg.Auths, key)
	if !ok {
		return credential{}, nil
	}
	c := credential{Username: a.Username, Password: a.Password, IdentityToken: a.IdentityToken}
	if a.Auth != "" {
		raw, err := base64.StdEncoding.DecodeString(a.Auth)
		if err != nil {
			return credential{}, fmt.Errorf("config.json: unreadable auth entry for %s", printable(key))
		}
		c.Username, c.Password, _ = strings.Cut(string(raw), ":")
	}
	return c, nil
}

func helperCommand(ctx context.Context, helper, verb string) (*exec.Cmd, error) {
	if !helperNameRE.MatchString(helper) {
		return nil, fmt.Errorf("config.json: invalid credential helper name %q", printable(helper))
	}
	bin, err := exec.LookPath("docker-credential-" + helper)
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, bin, verb), nil
}

// helperGet asks docker-credential-<helper> for key. A missing helper or an unknown key is not an
// error: the registry may allow anonymous access.
func helperGet(ctx context.Context, helper, key string) (credential, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd, err := helperCommand(ctx, helper, "get")
	if errors.Is(err, exec.ErrNotFound) {
		return credential{}, false, nil
	}
	if err != nil {
		return credential{}, false, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdin = strings.NewReader(key)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stdout.String(), "credentials not found") {
			return credential{}, false, nil
		}
		return credential{}, false, fmt.Errorf("credential helper %s: %v: %s", helper, err, printable(strings.TrimSpace(stderr.String())))
	}
	var out struct{ Username, Secret string }
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return credential{}, false, fmt.Errorf("credential helper %s: unreadable reply", helper)
	}
	if out.Username == "<token>" {
		return credential{IdentityToken: out.Secret}, true, nil
	}
	return credential{Username: out.Username, Password: out.Secret}, true, nil
}

// storeCredential saves a credential in the configured credential
// helper when there is one, otherwise base64-encoded in config.json (0600), keeping every other
// field of the file.
func storeCredential(ctx context.Context, key string, c credential) (string, error) {
	cfg, err := loadCredConfig()
	if err != nil {
		return "", err
	}
	if helper := cfg.helperFor(key); helper != "" {
		cmd, err := helperCommand(ctx, helper, "store")
		if err != nil {
			return "", fmt.Errorf("credential helper %s: %w", helper, err)
		}
		in, _ := json.Marshal(map[string]string{"ServerURL": key, "Username": c.Username, "Secret": c.Password})
		var stderr bytes.Buffer
		cmd.Stdin, cmd.Stderr = bytes.NewReader(in), &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("credential helper %s: %v: %s", helper, err, printable(strings.TrimSpace(stderr.String())))
		}
		return "credential helper " + helper, nil
	}

	path, err := credConfigPath()
	if err != nil {
		return "", err
	}
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &raw); err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	auths := map[string]json.RawMessage{}
	if a, ok := raw["auths"]; ok {
		if err := json.Unmarshal(a, &auths); err != nil {
			return "", fmt.Errorf("%s: auths: %w", path, err)
		}
	}
	for k := range auths {
		if authKeyHost(k) == authKeyHost(key) {
			delete(auths, k)
		}
	}
	entry, _ := json.Marshal(authEntry{Auth: base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))})
	auths[key] = entry
	raw["auths"], _ = json.Marshal(auths)
	out, err := json.MarshalIndent(raw, "", "\t")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.json-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(out, '\n')); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}
