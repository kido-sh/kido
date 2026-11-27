package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCtrNotifyRefusesBadShares runs the relay command in a child with a directory at descriptor 3,
// so that only the share name can refuse it.
func TestCtrNotifyRefusesBadShares(t *testing.T) {
	if arg, ok := os.LookupEnv("KIDO_TEST_NOTIFY_SHARE"); ok {
		os.Exit(ctrNotify(context.Background(), []string{"c1", arg}))
	}
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	for _, arg := range []string{"m/0=/x", "=/x", "m0", strings.Repeat("m", 65) + "=/x", "m\x1b0=/x"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCtrNotifyRefusesBadShares$")
		cmd.Env = append(os.Environ(), "KIDO_TEST_NOTIFY_SHARE="+arg, "KIDO_HOME="+t.TempDir())
		cmd.ExtraFiles = []*os.File{dir}
		var ee *exec.ExitError
		if err := cmd.Run(); !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Errorf("share %q: %v, want exit status 2", arg, err)
		}
	}
}
