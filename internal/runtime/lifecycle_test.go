package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
)

func lcRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func lcContainer(t *testing.T, r *Runtime, name string, fill func(*Info)) *Info {
	t.Helper()
	info, err := r.allocate(name, func(i *Info) error {
		i.Image = "darwin:latest"
		if fill != nil {
			fill(i)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestRename(t *testing.T) {
	r := lcRuntime(t)
	a := lcContainer(t, r, "a", nil)
	lcContainer(t, r, "b", nil)
	if err := r.Rename("a", "b"); err == nil {
		t.Error("renamed onto a name in use")
	}
	if err := r.Rename("a", "bad name"); err == nil {
		t.Error("accepted an invalid name")
	}
	if err := r.Rename(a.ID, "c"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Get("c"); err != nil || got.ID != a.ID {
		t.Fatalf("Get(c) = %v, %v", got, err)
	}
}

func TestTagImageKeepsCreatedAndConfig(t *testing.T) {
	r := lcRuntime(t)
	src := imageref.Ref{Name: "darwin", Tag: "latest"}
	cfg := engine.ImageConfig{Cmd: []string{"/bin/sh"}, Env: []string{"A=1"}}
	_, err := r.storeImage(src, &imageJSON{RootUUID: "c42a6242-de93-4b00-9b82-af06e5178cf5", Config: cfg}, func(tmp string) error {
		for _, f := range []string{"disk.img", "kernel", "initrd.cpio"} {
			if err := os.WriteFile(filepath.Join(tmp, f), []byte(f), 0o600); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := r.lcSetImageCreated(src, created); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TagImage(context.Background(), "darwin", "mirror/darwin:v1"); err != nil {
		t.Fatal(err)
	}
	img, err := r.Image(context.Background(), "mirror/darwin:v1")
	if err != nil {
		t.Fatal(err)
	}
	if !img.Created.Equal(created) || img.Config.Cmd[0] != "/bin/sh" || img.RootUUID == "" {
		t.Fatalf("tagged image %+v", img)
	}
	if b, err := os.ReadFile(filepath.Join(imageref.Ref{Name: "mirror/darwin", Tag: "v1"}.Dir(r.Home), "kernel")); err != nil || string(b) != "kernel" {
		t.Fatalf("kernel of the tag: %q, %v", b, err)
	}
}

func TestPruneTempKeepsRecentEntries(t *testing.T) {
	r := lcRuntime(t)
	old := time.Now().Add(-2 * time.Hour)
	mk := func(dir string, age time.Time) string {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(dir, "part")
		os.WriteFile(f, []byte("x"), 0o600)
		os.Chtimes(f, age, age)
		os.Chtimes(dir, age, age)
		return dir
	}
	oldTmp := mk(filepath.Join(r.Home, "tmp", "load-old"), old)
	busy := mk(filepath.Join(r.Home, "tmp", "load-busy"), old)
	os.WriteFile(filepath.Join(busy, "fresh"), []byte("y"), 0o600)
	oldImg := mk(filepath.Join(r.imagesDir(), ".tmp-old"), old)
	newImg := mk(filepath.Join(r.imagesDir(), ".tmp-new"), time.Now())
	p, err := r.PruneTemp()
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{oldTmp, oldImg} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s survived the prune", gone)
		}
	}
	for _, kept := range []string{busy, newImg} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was pruned: %v", kept, err)
		}
	}
	if len(p.Names) != 2 {
		t.Errorf("pruned %v", p.Names)
	}
}

func TestWaitAndRecordExit(t *testing.T) {
	r := lcRuntime(t)
	c := lcContainer(t, r, "w", func(i *Info) { i.StartedAt = time.Now(); i.PID = 999999 })
	if err := r.RecordExit(c.ID, 111, 3, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(c.ID); got.ExitCode != nil {
		t.Fatalf("RecordExit for another boot set the exit code to %d", *got.ExitCode)
	}
	if err := r.RecordExit(c.ID, 999999, 7, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if code, err := r.Wait(ctx, "w"); err != nil || code != 7 {
		t.Fatalf("Wait = %d, %v; want 7", code, err)
	}
}
