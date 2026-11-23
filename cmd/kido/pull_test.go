package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kido-sh/kido/internal/runtime"
)

func TestCheckPullPolicy(t *testing.T) {
	for _, p := range []string{"missing", "never"} {
		if err := checkPullPolicy(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"always", "", "Missing"} {
		if err := checkPullPolicy(p); err == nil {
			t.Errorf("%q: accepted", p)
		}
	}
}

func TestImageOrPull(t *testing.T) {
	rt, err := runtime.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := imageOrPull(ctx, rt, "nosuch", "never", nil); !errors.Is(err, runtime.ErrNoImage) {
		t.Errorf("never: %v, want ErrNoImage", err)
	}
	var out strings.Builder
	_, err = imageOrPull(ctx, rt, "127.0.0.1:1/nosuch", "missing", &out)
	if err == nil || !strings.HasPrefix(err.Error(), "pull 127.0.0.1:1/nosuch:latest: ") {
		t.Errorf("missing: %v, want a pull error", err)
	}
	if want := "Unable to find image '127.0.0.1:1/nosuch:latest' locally\n"; out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
}
