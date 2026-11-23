package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/imageref"
)

func histImage(t *testing.T, r *Runtime, name string, created time.Time, parent string, extra map[string]string) {
	t.Helper()
	ref, err := imageref.Parse(name)
	if err != nil {
		t.Fatal(err)
	}
	j := map[string]any{"created": created, "size": 1000, "parent": parent}
	for k, v := range extra {
		j[k] = v
	}
	b, _ := json.Marshal(j)
	dir := ref.Dir(r.Home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "image.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestImageHistory(t *testing.T) {
	r := lcRuntime(t)
	t0 := time.Now().Add(-time.Hour)
	histImage(t, r, "base:1", t0, "", nil)
	histImage(t, r, "mid:1", t0.Add(time.Minute), "base:1", map[string]string{"comment": "c", "createdBy": "kido commit"})
	histImage(t, r, "top:1", t0.Add(2*time.Minute), "mid:1", nil)
	h, err := r.ImageHistory(context.Background(), "top:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 3 || h[0].Ref != "top:1" || h[1].Ref != "mid:1" || h[2].Ref != "base:1" || h[2].Image == nil {
		t.Fatalf("history = %+v", h)
	}
	if h[1].Comment != "c" || h[1].CreatedBy != "kido commit" {
		t.Errorf("mid:1 created by %q, comment %q", h[1].CreatedBy, h[1].Comment)
	}

	histImage(t, r, "orphan:1", t0, "removed:1", nil)
	histImage(t, r, "renewed:1", t0.Add(time.Hour), "", nil)
	histImage(t, r, "child:1", t0, "renewed:1", nil)
	histImage(t, r, "loop:1", t0, "loop:2", nil)
	histImage(t, r, "loop:2", t0, "loop:1", nil)
	for name, want := range map[string]int{"orphan:1": 2, "child:1": 2, "loop:1": 3} {
		h, err := r.ImageHistory(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if len(h) != want || h[len(h)-1].Image != nil {
			t.Errorf("%s: history %+v, want %d entries ending in a missing one", name, h, want)
		}
	}
	if _, err := r.ImageHistory(context.Background(), "nope:1"); err == nil {
		t.Error("want an error for a missing image")
	}
}
