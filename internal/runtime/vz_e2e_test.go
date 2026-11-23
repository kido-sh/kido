package runtime

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/engine"
)

// TestVZEndToEnd boots a container under Virtualization.framework and runs commands in it. It needs
// KIDO_VZ_E2E=1 and a KIDO_HOME holding the image $KIDO_VZ_E2E_IMAGE (default darwin); images built
// before the VZ fixes also need KIDO_VZ_KERNEL and KIDO_VZ_INITRD.
func TestVZEndToEnd(t *testing.T) {
	if os.Getenv("KIDO_VZ_E2E") == "" {
		t.Skip("set KIDO_VZ_E2E=1 to boot a VM")
	}
	t.Setenv("KIDO_BACKEND", backendVZ)
	r, err := New(DefaultHome())
	if err != nil {
		t.Fatal(err)
	}
	image := os.Getenv("KIDO_VZ_E2E_IMAGE")
	if image == "" {
		image = "darwin"
	}
	ctx := context.Background()
	start := time.Now()
	c, err := r.Create(ctx, engine.CreateOptions{Image: image})
	if c != nil {
		defer r.Remove(ctx, c.ID, true)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("create and boot: %v", time.Since(start))

	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		start := time.Now()
		status, err := r.Exec(ctx, c.ID, engine.ProcessSpec{Args: args}, engine.Stdio{Stdout: &out, Stderr: &out})
		if err != nil || status != 0 {
			t.Logf("vmm.log:\n%s", tail(r.file(c.ID, "vmm.log"), 20))
			t.Fatalf("%v: status %d, err %v, output %q", args, status, err, out.String())
		}
		t.Logf("%v: %v", args, time.Since(start))
		return out.String()
	}
	run("/bin/true")
	if got := run("/bin/sh", "-c", "echo hello"); strings.TrimSpace(got) != "hello" {
		t.Fatalf("echo: %q", got)
	}
	t.Log(run("/bin/sh", "-c", "ifconfig en0 | grep inet; tail -3 /var/log/kido-agent.log"))

	start = time.Now()
	if err := r.Stop(ctx, c.ID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Logf("stop: %v", time.Since(start))
}

// TestVZShell boots a vz container of $KIDO_VZ_E2E_IMAGE and runs each line of $KIDO_VZ_SHELL with
// /bin/sh -c; "sleep DURATION" waits and "commit NAME" commits the container.
func TestVZShell(t *testing.T) {
	script := os.Getenv("KIDO_VZ_SHELL")
	if os.Getenv("KIDO_VZ_E2E") == "" || script == "" {
		t.Skip("set KIDO_VZ_E2E=1 and KIDO_VZ_SHELL")
	}
	t.Setenv("KIDO_BACKEND", backendVZ)
	r, err := New(DefaultHome())
	if err != nil {
		t.Fatal(err)
	}
	image := os.Getenv("KIDO_VZ_E2E_IMAGE")
	if image == "" {
		image = "darwin"
	}
	ctx := context.Background()
	c, err := r.Create(ctx, engine.CreateOptions{Image: image})
	if c != nil {
		defer r.Remove(ctx, c.ID, true)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(script, "\n") {
		if line == "" {
			continue
		}
		if name, ok := strings.CutPrefix(line, "commit "); ok {
			start := time.Now()
			_, err := r.Commit(ctx, c.ID, name, engine.ImageConfig{})
			t.Logf("commit %s: %v, err %v", name, time.Since(start), err)
			continue
		}
		if d, ok := strings.CutPrefix(line, "sleep "); ok {
			s, _ := time.ParseDuration(d)
			time.Sleep(s)
			continue
		}
		var out bytes.Buffer
		start := time.Now()
		status, err := r.Exec(ctx, c.ID, engine.ProcessSpec{Args: []string{"/bin/sh", "-c", line}}, engine.Stdio{Stdout: &out, Stderr: &out})
		t.Logf("$ %s (%v, status %d, err %v)\n%s", line, time.Since(start), status, err, out.String())
	}
}
