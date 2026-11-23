package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	host := map[string]string{"HOSTVAR": "from host", "EMPTY": ""}
	lookup := func(k string) (string, bool) { v, ok := host[k]; return v, ok }
	in := "\xef\xbb\xbfFIRST=1\n" +
		"# a comment\n" +
		"   # an indented comment\n" +
		"\n" +
		"  \t\n" +
		"QUOTED=\"kept\" 'as is'\n" +
		"EQ=a=b=c\n" +
		"SPACES=  around  \n" +
		"BLANK=\n" +
		"  INDENTED=yes\n" +
		"HOSTVAR\n" +
		"EMPTY\n" +
		"UNSET_ON_HOST\n" +
		"CRLF=x\r\n" +
		"HASH=a#b\n"
	got, err := ctrParseEnvFile(strings.NewReader(in), lookup)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"FIRST=1", `QUOTED="kept" 'as is'`, "EQ=a=b=c", "SPACES=  around  ", "BLANK=",
		"INDENTED=yes", "HOSTVAR=from host", "EMPTY=", "CRLF=x", "HASH=a#b"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestParseEnvFileErrors(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	for _, in := range []string{"BAD KEY=1\n", "=value\n", "TRAILING \n", "OK=1\nBAD\xff=1\n", "NUL=a\x00b\n"} {
		if got, err := ctrParseEnvFile(strings.NewReader(in), none); err == nil {
			t.Errorf("%q: want an error, got %q", in, got)
		}
	}
}

func TestReadEnvFilesOrder(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.env"), filepath.Join(dir, "b.env")
	os.WriteFile(a, []byte("X=1\nY=a\n"), 0o600)
	os.WriteFile(b, []byte("X=2\n"), 0o600)
	got, err := ctrReadEnvFiles([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"X=1", "Y=a", "X=2"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := ctrReadEnvFiles([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("want an error for a missing file")
	}
}
