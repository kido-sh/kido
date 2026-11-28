package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProbeHelpAndBadArgs(t *testing.T) {
	if code := ctrProbe(context.Background(), []string{"--unknown-flag"}); code != 125 {
		t.Fatalf("unknown flag exit = %d, want 125", code)
	}
	if code := ctrProbe(context.Background(), []string{"extra-arg"}); code != 125 {
		t.Fatalf("positional arg exit = %d, want 125", code)
	}
	if code := ctrProbe(context.Background(), []string{"--no-coherency-test", "--force-coherency-test"}); code != 125 {
		t.Fatalf("mutually exclusive flags exit = %d, want 125", code)
	}
	opts := ctrCommandFlags(ctrProbe, nil)
	want := map[string]bool{
		"json":                 false,
		"no-coherency-test":    false,
		"force-coherency-test": false,
	}
	for _, o := range opts {
		for _, l := range o.Long {
			if _, ok := want[l]; ok {
				want[l] = true
			}
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("ctrCommandFlags(ctrProbe) missing --%s", k)
		}
	}
	if !strings.Contains(commands["probe"].summary, "Probe") {
		t.Fatalf("unexpected probe summary: %q", commands["probe"].summary)
	}
}

func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := fn()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	r.Close()
	return code, string(b)
}

func TestProbePlatformBehavior(t *testing.T) {
	if runtime.GOOS != "linux" {
		code, out := captureStdout(t, func() int {
			return ctrProbe(context.Background(), []string{"--json"})
		})
		if code == 0 {
			t.Fatalf("non-linux probe exit = 0, want non-zero")
		}
		if out != "" {
			t.Fatalf("non-linux probe wrote unexpected stdout %q", out)
		}
		return
	}

	dir := t.TempDir()
	fake := filepath.Join(dir, "kido-probe")
	script := "#!/bin/sh\necho \"args:$*\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIDO_PROBE", fake)
	code, out := captureStdout(t, func() int {
		return ctrProbe(context.Background(), []string{"--json", "--force-coherency-test"})
	})
	if code != 0 {
		t.Fatalf("linux probe exit = %d, want 0", code)
	}
	if got := strings.TrimSpace(out); got != "args:--json --force-coherency-test" {
		t.Fatalf("forwarded args = %q, want %q", got, "args:--json --force-coherency-test")
	}

	failBin := filepath.Join(dir, "kido-probe-fail")
	if err := os.WriteFile(failBin, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIDO_PROBE", failBin)
	if code := ctrProbe(context.Background(), nil); code != 7 {
		t.Fatalf("non-zero exit propagation = %d, want 7", code)
	}

	sigBin := filepath.Join(dir, "kido-probe-sig")
	if err := os.WriteFile(sigBin, []byte("#!/bin/sh\nkill -9 $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIDO_PROBE", sigBin)
	if code := ctrProbe(context.Background(), nil); code != 137 {
		t.Fatalf("signal exit propagation = %d, want 137", code)
	}

	t.Setenv("KIDO_PROBE", "relative/kido-probe")
	if code := ctrProbe(context.Background(), nil); code == 0 {
		t.Fatalf("relative KIDO_PROBE unexpectedly succeeded")
	}

	t.Setenv("KIDO_PROBE", filepath.Join(dir, "does-not-exist"))
	if code := ctrProbe(context.Background(), nil); code == 0 {
		t.Fatalf("missing KIDO_PROBE unexpectedly succeeded")
	}
}
