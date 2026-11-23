package setup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// hvfEntitlements is packaging/entitlements/qemu-hvf.entitlements (a test keeps them equal).
const hvfEntitlements = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>com.apple.security.hypervisor</key>
	<true/>
</dict>
</plist>
`

const codesign = "/usr/bin/codesign"

// ensureSigned ad-hoc signs path unless its signature verifies (and, when hvf is set, grants the
// hypervisor entitlement). It reports whether it signed.
func ensureSigned(ctx context.Context, path string, hvf bool) (bool, error) {
	if signatureOK(ctx, path, hvf) {
		return false, nil
	}
	args := []string{"--force", "--sign", "-"}
	if hvf {
		ent, err := os.CreateTemp(filepath.Dir(path), ".entitlements-*.plist")
		if err != nil {
			return false, err
		}
		defer os.Remove(ent.Name())
		_, err = ent.WriteString(hvfEntitlements)
		if cerr := ent.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return false, err
		}
		args = append(args, "--entitlements", ent.Name())
	}
	if out, err := runTool(ctx, codesign, append(args, path)...); err != nil {
		return false, fmt.Errorf("codesign %s: %v: %s", filepath.Base(path), err, out)
	}
	if !signatureOK(ctx, path, hvf) {
		return true, fmt.Errorf("%s: the signature does not verify after signing", path)
	}
	return true, nil
}

func signatureOK(ctx context.Context, path string, hvf bool) bool {
	if hvf {
		if ok, err := HasEntitlement(path, HypervisorEntitlement); err != nil || !ok {
			return false
		}
	}
	_, err := runTool(ctx, codesign, "--verify", "--strict", path)
	return err == nil
}

// runTool runs a program with an empty environment and returns the first line of its output.
func runTool(ctx context.Context, program string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = []string{}
	out, err := cmd.CombinedOutput()
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line), err
}
