package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/kido-sh/kido/internal/engine"
)

func seedTestImage(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, "images", "darwin", "latest")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "containers"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"disk.img", "kernel", "initrd.cpio"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	imgJSON := `{"name":"darwin","tag":"latest","rootUUID":"11111111-1111-1111-1111-111111111111","size":1}`
	if err := os.WriteFile(filepath.Join(dir, "image.json"), []byte(imgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareGPUValidation(t *testing.T) {
	home := t.TempDir()
	seedTestImage(t, home)
	ctx := context.Background()

	// --gpu-memory without --gpu must fail on any backend.
	rQEMU := &Runtime{Home: home, Backend: backendQEMU}
	if _, err := rQEMU.prepare(ctx, engine.CreateOptions{Image: "darwin", GPUMemoryMB: 512}); err == nil || !strings.Contains(err.Error(), "--gpu-memory requires --gpu") {
		t.Fatalf("expected --gpu-memory requires --gpu error, got %v", err)
	}

	// --gpu on QEMU must fail cleanly.
	if _, err := rQEMU.prepare(ctx, engine.CreateOptions{Image: "darwin", GPU: true}); err == nil || !strings.Contains(err.Error(), "vz backend") {
		t.Fatalf("expected backend error on qemu, got %v", err)
	}

	nativeBackend := backendVZ
	wrongBackend := backendKVM
	if goruntime.GOOS == "linux" {
		nativeBackend = backendKVM
		wrongBackend = backendVZ
	}
	rWrong := &Runtime{Home: home, Backend: wrongBackend}
	if _, err := rWrong.prepare(ctx, engine.CreateOptions{Image: "darwin", GPU: true}); err == nil {
		t.Fatalf("expected --gpu to fail on mismatched backend %s", wrongBackend)
	}

	// Range checks on host's native GPU backend (only run if host supports GPU).
	if gpuSupported(nativeBackend) == nil {
		rNative := &Runtime{Home: home, Backend: nativeBackend}
		for _, badMem := range []int{32, 131073} {
			if _, err := rNative.prepare(ctx, engine.CreateOptions{Image: "darwin", GPU: true, GPUMemoryMB: badMem}); err == nil || !strings.Contains(err.Error(), "between 64 and 131072") {
				t.Fatalf("expected range error for GPUMemoryMB=%d, got %v", badMem, err)
			}
		}
		info, err := rNative.prepare(ctx, engine.CreateOptions{Image: "darwin", GPU: true, GPUMemoryMB: 2048})
		if err != nil {
			t.Fatalf("prepare valid --gpu failed: %v", err)
		}
		if !info.GPU || info.GPUMemoryMB != 2048 {
			t.Fatalf("unexpected info GPU fields: %+v", info)
		}
		c := info.container()
		if !c.GPU || c.GPUMemoryMB != 2048 {
			t.Fatalf("container() omitted GPU fields: %+v", c)
		}
	}
}

func TestInfoGPUJSONRoundTrip(t *testing.T) {
	in := Info{ID: "abc", Image: "darwin:latest", GPU: true, GPUMemoryMB: 4096}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Info
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.GPU || out.GPUMemoryMB != 4096 {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}
