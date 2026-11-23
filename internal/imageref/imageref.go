// Package imageref parses image references and maps them to the local image store.
package imageref

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	nameRE = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	tagRE  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

type Ref struct {
	Name string // "darwin", "ghcr.io/x/darwin", "localhost:5001/darwin"
	Tag  string // "latest" when not given
}

func (r Ref) String() string { return r.Name + ":" + r.Tag }

// Parse accepts NAME or NAME:TAG; digests are not supported yet.
func Parse(s string) (Ref, error) {
	if strings.Contains(s, "@") {
		return Ref{}, fmt.Errorf("image reference %q: digests are not supported yet", s)
	}
	name, tag := s, "latest"
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		name, tag = s[:i], s[i+1:]
	}
	if !nameRE.MatchString(name) || len(name) > 255 {
		return Ref{}, fmt.Errorf("invalid image name %q", name)
	}
	if !tagRE.MatchString(tag) {
		return Ref{}, fmt.Errorf("invalid image tag %q", tag)
	}
	return Ref{Name: name, Tag: tag}, nil
}

// Dir is where the image lives under KIDO_HOME: images/<escaped name>/<tag>.
func (r Ref) Dir(home string) string {
	return filepath.Join(home, "images", url.PathEscape(r.Name), r.Tag)
}

// FromDir is the inverse of Dir for a directory inside images/.
func FromDir(nameDir, tag string) (Ref, error) {
	name, err := url.PathUnescape(nameDir)
	if err != nil {
		return Ref{}, err
	}
	return Parse(name + ":" + tag)
}
