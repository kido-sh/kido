package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVZFailureIsSanitized(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "vmm.log"), []byte("kido-vmm   1.000: guest said \x1b]52;c;aGk=\x07 and \x1b[2J\n"), 0o600)
	err := vzFailure("/bin/kido-vmm", dir, "boot failed \x1b[31m")
	if strings.ContainsAny(err.Error(), "\x1b\x07") || !strings.Contains(err.Error(), "boot failed") {
		t.Fatalf("unsafe or missing text: %q", err.Error())
	}
}
