package build

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

type ignorePattern struct {
	re        *regexp.Regexp
	exclusion bool
}

// ignoreMatcher implements .dockerignore: patterns relative to the context root, "*" and "?" stay
// within one path element, "**" spans any number of them, "!" re-includes, the last match wins, and a
// path is excluded when it or one of its parent directories matches.
type ignoreMatcher struct {
	patterns      []ignorePattern
	hasExclusions bool
}

func parseIgnore(r io.Reader) (*ignoreMatcher, error) {
	m := &ignoreMatcher{}
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			line = strings.TrimPrefix(line, "\uFEFF")
			first = false
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		exclusion := line[0] == '!'
		if exclusion {
			line = strings.TrimSpace(line[1:])
			if line == "" {
				return nil, fmt.Errorf(".dockerignore: illegal exclusion pattern %q", "!")
			}
		}
		line = path.Clean(line)
		if len(line) > 1 && line[0] == '/' {
			line = line[1:]
		}
		re, err := compileIgnore(line)
		if err != nil {
			return nil, fmt.Errorf(".dockerignore: pattern %q: %w", line, err)
		}
		m.patterns = append(m.patterns, ignorePattern{re: re, exclusion: exclusion})
		m.hasExclusions = m.hasExclusions || exclusion
	}
	return m, sc.Err()
}

func compileIgnore(p string) (*regexp.Regexp, error) {
	// A leading "**" followed by plain text matches as a suffix: "**foo" matches "afoo".
	if len(p) > 2 && strings.HasPrefix(p, "**") && p[2] != '/' && !strings.ContainsAny(p[2:], `*?[\`) {
		return regexp.Compile("^.*" + regexp.QuoteMeta(p[2:]) + "$")
	}
	var b strings.Builder
	b.WriteString("^")
	rs := []rune(p)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; r {
		case '*':
			if i+1 < len(rs) && rs[i+1] == '*' {
				i++
				if i+1 < len(rs) && rs[i+1] == '/' {
					i++
				}
				if i+1 == len(rs) {
					b.WriteString(".*")
				} else {
					b.WriteString("(.*/)?")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := i + 1
			if j < len(rs) && rs[j] == '^' {
				j++
			}
			if j < len(rs) && rs[j] == ']' {
				j++
			}
			for j < len(rs) && rs[j] != ']' {
				if rs[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(rs) {
				return nil, errors.New("unterminated character class")
			}
			b.WriteByte('[')
			k := i + 1
			if rs[k] == '^' {
				b.WriteByte('^')
				k++
			}
			for ; k < j; k++ {
				switch c := rs[k]; {
				case c == '\\' && k+1 < j:
					k++
					b.WriteString(regexp.QuoteMeta(string(rs[k])))
				case c == '-':
					b.WriteByte('-')
				default:
					b.WriteString(regexp.QuoteMeta(string(c)))
				}
			}
			b.WriteByte(']')
			i = j
		case '\\':
			if i+1 < len(rs) {
				i++
				b.WriteString(regexp.QuoteMeta(string(rs[i])))
			} else {
				b.WriteString(`\\`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// excluded reports whether rel (slash-separated, relative to the context root) is ignored.
func (m *ignoreMatcher) excluded(rel string) bool {
	if m == nil || len(m.patterns) == 0 || rel == "." || rel == "" {
		return false
	}
	var parents []string
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' {
			parents = append(parents, rel[:i])
		}
	}
	matched := false
	for _, p := range m.patterns {
		if p.exclusion != matched {
			continue
		}
		hit := p.re.MatchString(rel)
		for _, parent := range parents {
			if hit {
				break
			}
			hit = p.re.MatchString(parent)
		}
		if hit {
			matched = !p.exclusion
		}
	}
	return matched
}
