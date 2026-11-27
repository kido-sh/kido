package build

import (
	"strings"
	"testing"
)

func TestIgnore(t *testing.T) {
	m, err := parseIgnore(strings.NewReader(`# comment
*.md
!README.md
  /secret
**/*.log
docs
!docs/keep.txt
build/**
node_modules/
a?c
[xy].bin
`))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"x.md":            true,
		"README.md":       false,
		"sub/x.md":        false,
		"secret":          true,
		"secret/key":      true,
		"sub/secret":      false,
		"a.log":           true,
		"deep/er/b.log":   true,
		"docs":            true,
		"docs/a.txt":      true,
		"docs/keep.txt":   false,
		"build":           false,
		"build/out":       true,
		"build/x/y":       true,
		"node_modules":    true,
		"node_modules/pk": true,
		"abc":             true,
		"abbc":            false,
		"x.bin":           true,
		"z.bin":           false,
		"main.go":         false,
		".":               false,
	} {
		if got := m.excluded(path); got != want {
			t.Errorf("excluded(%q) = %v, want %v", path, got, want)
		}
	}
	if !m.hasExclusions {
		t.Error("hasExclusions")
	}
}

func TestIgnoreDoubleStar(t *testing.T) {
	m, err := parseIgnore(strings.NewReader("**\n!keep/**\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !m.excluded("a/b") || m.excluded("keep/x") || m.excluded("keep/x/y") {
		t.Fatal("** then !keep/**")
	}
	m, _ = parseIgnore(strings.NewReader("**/vendor\n"))
	if !m.excluded("vendor") || !m.excluded("a/b/vendor/x") || m.excluded("vendorx") {
		t.Fatal("**/vendor")
	}
	m, _ = parseIgnore(strings.NewReader("**foo\n[^a]x\n[a-c]y\n"))
	for p, want := range map[string]bool{"afoo": true, "d/afoo": true, "foo": true, "foox": false, "bx": true, "ax": false, "by": true, "dy": false} {
		if m.excluded(p) != want {
			t.Errorf("excluded(%q) != %v", p, want)
		}
	}
	if _, err := parseIgnore(strings.NewReader("[ab\n")); err == nil || !strings.Contains(err.Error(), ".kidoignore") {
		t.Errorf("an unterminated class is an error: %v", err)
	}
	if _, err := parseIgnore(strings.NewReader("!\n")); err == nil || !strings.Contains(err.Error(), ".kidoignore") {
		t.Fatalf("a lone ! is an error: %v", err)
	}
}
