package mount

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kido-sh/kido/internal/engine"
)

func liveContainer(t *testing.T, live bool, ms ...engine.Mount) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home", "containers", testCID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range ms {
		m, err := Resolve(ms[i])
		if err != nil {
			t.Fatal(err)
		}
		ms[i] = m
	}
	if err := Save(dir, ms, live); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLiveSharesDirectoriesOnly(t *testing.T) {
	src := t.TempDir()
	file := filepath.Join(t.TempDir(), "f")
	write(t, file, "x")
	dir := liveContainer(t, true,
		engine.Mount{Source: src, Target: "/data"},
		engine.Mount{Source: file, Target: "/etc/f", ReadOnly: true},
		engine.Mount{Source: src, Target: "/ro", ReadOnly: true})
	args, err := VZArgs(dir)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(src)
	want := []string{"--extra-disk-ro", "mounts/in-1.img", "--share", "m0=" + real, "--share-ro", "m2=" + real}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("VZArgs = %q, want %q", args, want)
	}
	for _, i := range []string{"0", "2"} {
		if _, err := os.Stat(filepath.Join(dir, diskDir, "in-"+i+".img")); !os.IsNotExist(err) {
			t.Fatalf("live mount %s was packed: %v", i, err)
		}
	}
	if _, err := QEMUArgs(dir); err == nil || !strings.Contains(err.Error(), "needs the vz backend") {
		t.Fatalf("QEMUArgs with live mounts: %v", err)
	}
}

func TestNotLiveCopiesDirectories(t *testing.T) {
	dir := liveContainer(t, false, engine.Mount{Source: t.TempDir(), Target: "/data"})
	args, err := VZArgs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--extra-disk-ro", "mounts/in-0.img", "--extra-disk", "mounts/out-0.img"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("VZArgs = %q, want %q", args, want)
	}
}

func TestLiveSourceMustStayADirectory(t *testing.T) {
	src := t.TempDir()
	dir := liveContainer(t, true, engine.Mount{Source: src, Target: "/data"})
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	write(t, src, "now a file")
	if _, err := VZArgs(dir); err == nil || !strings.Contains(err.Error(), "no longer a directory") {
		t.Fatalf("VZArgs after the source became a file: %v", err)
	}
	os.Remove(src)
	if _, err := VZArgs(dir); err == nil {
		t.Fatal("VZArgs after the source went away: no error")
	}
}

type guestLog struct{ calls [][]string }

func (l *guestLog) run(_ context.Context, args []string) ([]byte, error) {
	l.calls = append(l.calls, args)
	return nil, nil
}

func TestPopulateMountsLiveShares(t *testing.T) {
	dir := liveContainer(t, true,
		engine.Mount{Source: t.TempDir(), Target: "/data"},
		engine.Mount{Source: t.TempDir(), Target: "/ro", ReadOnly: true})
	if _, err := VZArgs(dir); err != nil {
		t.Fatal(err)
	}
	var g guestLog
	if err := Populate(context.Background(), dir, g.run); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/bin/sh", "-c", liveScript, "sh", "m0", "/data", "rw"},
		{"/bin/sh", "-c", liveScript, "sh", "m1", "/ro", "ro"},
	}
	if !reflect.DeepEqual(g.calls, want) {
		t.Fatalf("guest calls = %q", g.calls)
	}
	st, _ := load(dir)
	for _, m := range st.Mounts {
		if m.Populated {
			t.Fatalf("live mount %s marked populated", m.Target)
		}
	}
}

func TestLiveScriptRemovesNothing(t *testing.T) {
	body := strings.TrimPrefix(liveScript, prelude)
	for _, w := range []string{"rm ", "rm\t", "rmdir", "unlink", "mv "} {
		if strings.Contains(body, w) {
			t.Fatalf("liveScript contains %q", w)
		}
	}
}

func TestCollectFlushesLiveWrites(t *testing.T) {
	rw := liveContainer(t, true, engine.Mount{Source: t.TempDir(), Target: "/data"})
	var g guestLog
	Collect(context.Background(), rw, g.run, io.Discard)
	if want := [][]string{{"/bin/sync"}}; !reflect.DeepEqual(g.calls, want) {
		t.Fatalf("Collect with a read-write live mount ran %q", g.calls)
	}
	ro := liveContainer(t, true, engine.Mount{Source: t.TempDir(), Target: "/data", ReadOnly: true})
	g = guestLog{}
	Collect(context.Background(), ro, g.run, io.Discard)
	if len(g.calls) != 0 {
		t.Fatalf("Collect with only a read-only live mount ran %q", g.calls)
	}
}

func TestSaveRefusesMissingSource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home", "containers", testCID)
	os.MkdirAll(dir, 0o700)
	src := filepath.Join(t.TempDir(), "gone")
	if err := Save(dir, []engine.Mount{{Source: src, Target: "/data"}}, true); err == nil {
		t.Fatal("Save with a missing source: no error")
	}
}

func TestRedirectedSourceIsRefused(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	secret := filepath.Join(base, "secret")
	os.MkdirAll(filepath.Join(shared, "conf"), 0o700)
	os.MkdirAll(secret, 0o700)
	write(t, filepath.Join(shared, "app.conf"), "conf")
	write(t, filepath.Join(secret, "key"), "key")
	dir := liveContainer(t, true,
		engine.Mount{Source: shared, Target: "/app"},
		engine.Mount{Source: filepath.Join(shared, "conf"), Target: "/conf"},
		engine.Mount{Source: filepath.Join(shared, "app.conf"), Target: "/etc/app.conf", ReadOnly: true})
	if _, err := VZArgs(dir); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(shared, "conf"))
	os.Symlink(secret, filepath.Join(shared, "conf"))
	if _, err := VZArgs(dir); err == nil || !strings.Contains(err.Error(), "now leads to") {
		t.Fatalf("live source redirected: %v", err)
	}
	os.Remove(filepath.Join(shared, "conf"))
	os.MkdirAll(filepath.Join(shared, "conf"), 0o700)
	os.Remove(filepath.Join(shared, "app.conf"))
	os.Symlink(filepath.Join(secret, "key"), filepath.Join(shared, "app.conf"))
	if _, err := VZArgs(dir); err == nil || !strings.Contains(err.Error(), "now leads to") {
		t.Fatalf("copied source redirected: %v", err)
	}
}

func TestApplyRefusesRedirectedSource(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	elsewhere := filepath.Join(base, "elsewhere")
	os.MkdirAll(src, 0o700)
	os.MkdirAll(elsewhere, 0o700)
	dir := boot(t, base, engine.Mount{Source: src, Target: "/data"})
	guestSends(t, dir, []ent{{name: "./planted", body: "x"}})
	os.Remove(src)
	os.Symlink(elsewhere, src)
	var warn strings.Builder
	Apply(dir, &warn)
	if _, err := os.Stat(filepath.Join(elsewhere, "planted")); !os.IsNotExist(err) {
		t.Fatalf("Apply wrote through a redirected source: %v", err)
	}
	if !strings.Contains(warn.String(), "now leads to") {
		t.Fatalf("Apply warned %q", warn.String())
	}
}

type scanGuest struct {
	guestLog
	token string
}

func (g *scanGuest) run(ctx context.Context, args []string) ([]byte, error) {
	g.guestLog.run(ctx, args)
	if len(args) == 3 && args[2] == scanScript {
		return []byte("/dev/disk3 " + magic + " " + g.token + " in 1\n"), nil
	}
	return nil, nil
}

func TestPopulateCopiesBeforeLive(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	write(t, file, "x")
	dir := liveContainer(t, true,
		engine.Mount{Source: t.TempDir(), Target: "/data"},
		engine.Mount{Source: file, Target: "/etc/f", ReadOnly: true})
	if _, err := VZArgs(dir); err != nil {
		t.Fatal(err)
	}
	st, _ := load(dir)
	g := &scanGuest{token: st.Token}
	if err := Populate(context.Background(), dir, g.run); err != nil {
		t.Fatal(err)
	}
	var scripts []string
	for _, c := range g.calls {
		switch c[2] {
		case scanScript:
			scripts = append(scripts, "scan")
		case populateScript:
			scripts = append(scripts, "copy "+c[5])
		case liveScript:
			scripts = append(scripts, "live "+c[5])
		}
	}
	if want := []string{"scan", "copy /etc/f", "live /data"}; !reflect.DeepEqual(scripts, want) {
		t.Fatalf("Populate ran %q, want %q", scripts, want)
	}
}

func TestScriptsStayOnTheRootFileSystem(t *testing.T) {
	for name, s := range map[string]string{"populate": populateScript, "collect": collectScript, "live": liveScript} {
		if !strings.Contains(strings.TrimPrefix(s, prelude), "onroot ") {
			t.Fatalf("%sScript does not check that its target is on the root file system", name)
		}
	}
}

func TestQEMURefusesLiveEvenWithoutItsSource(t *testing.T) {
	src := t.TempDir()
	dir := liveContainer(t, true, engine.Mount{Source: src, Target: "/data"})
	os.Remove(src)
	if _, err := QEMUArgs(dir); err == nil || !strings.Contains(err.Error(), "needs the vz backend") {
		t.Fatalf("QEMUArgs: %v", err)
	}
}

func TestLiveOnlyMakesNoDisks(t *testing.T) {
	dir := liveContainer(t, true, engine.Mount{Source: t.TempDir(), Target: "/data"})
	if _, err := VZArgs(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, diskDir)); !os.IsNotExist(err) {
		t.Fatalf("a live-only container got a disk directory: %v", err)
	}
}

func TestCheckComparesThroughPrivate(t *testing.T) {
	for _, pair := range [][2]string{{"/tmp/w", "/private/tmp/w/x"}, {"/var/a", "/private/var/a"}, {"/private/etc/x/y", "/etc/x"}} {
		if err := Check([]engine.Mount{{Target: pair[0]}, {Target: pair[1]}}); err == nil {
			t.Fatalf("Check(%q, %q): no error", pair[0], pair[1])
		}
	}
	if err := Check([]engine.Mount{{Target: "/tmp/a"}, {Target: "/private/tmp/b"}, {Target: "/variable"}}); err != nil {
		t.Fatalf("Check of unrelated targets: %v", err)
	}
}

func TestApplyAfterTheSourceFileWasRemoved(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "dir")
	os.MkdirAll(src, 0o700)
	file := filepath.Join(src, "f")
	write(t, file, "host")
	dir := boot(t, base, engine.Mount{Source: file, Target: "/etc/f"})
	guestSends(t, dir, []ent{{name: "./f", body: "container"}})
	os.Remove(file)
	var warn strings.Builder
	Apply(dir, &warn)
	if strings.Contains(warn.String(), "now leads to") {
		t.Fatalf("Apply refused a removed file source: %q", warn.String())
	}
	names, _ := os.ReadDir(src)
	if len(names) == 0 {
		t.Fatalf("Apply kept nothing of the container's file; warned %q", warn.String())
	}
}
