package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestArgsMessage(t *testing.T) {
	for _, c := range []struct {
		usage string
		n     int
		want  string
	}{
		{"[OPTIONS] IMAGE [COMMAND] [ARG...]", 0, "requires at least 1 argument"},
		{"[OPTIONS] IMAGE [COMMAND] [ARG...]", 5, ""},
		{"[OPTIONS] CONTAINER COMMAND [ARG...]", 1, "requires at least 2 arguments"},
		{"CONTAINER [CONTAINER...]", 0, "requires at least 1 argument"},
		{"CONTAINER NEW_NAME", 3, "requires exactly 2 arguments"},
		{"SOURCE_IMAGE[:TAG] TARGET_IMAGE[:TAG]", 1, "requires exactly 2 arguments"},
		{"[OPTIONS] PATH", 0, "requires exactly 1 argument"},
		{"[OPTIONS] [REPOSITORY[:TAG]]", 2, "requires at most 1 argument"},
		{"[OPTIONS]", 1, "accepts no arguments"},
		{"[OPTIONS]", 0, ""},
		{"CONTAINER [PRIVATE_PORT[/PROTO]]", 0, "requires at least 1 and at most 2 arguments"},
		{"CONTAINER [ps OPTIONS]", 0, "requires at least 1 argument"},
		{"CONTAINER:SRC_PATH DEST_PATH|-\n       kido cp SRC_PATH|- CONTAINER:DEST_PATH", 1, "requires exactly 2 arguments"},
		{"bash|zsh|fish\n\n  bash: ...", 0, "requires exactly 1 argument"},
	} {
		if got := ctrArgsMessage(c.usage, c.n); got != c.want {
			t.Errorf("%q with %d: %q, want %q", c.usage, c.n, got, c.want)
		}
	}
}

func TestArgsError(t *testing.T) {
	var b bytes.Buffer
	if !ctrArgsError(&b, "run", "[OPTIONS] IMAGE [COMMAND] [ARG...]", 0) {
		t.Fatal("no error for a missing IMAGE")
	}
	want := "\"kido run\" requires at least 1 argument.\nSee 'kido run --help'.\n\nUsage:  kido run [OPTIONS] IMAGE [COMMAND] [ARG...]\n"
	if !strings.HasPrefix(b.String(), want) || strings.Contains(b.String(), "Options:") {
		t.Fatalf("got %q", b.String())
	}
}
