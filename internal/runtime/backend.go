package runtime

import (
	"fmt"
	"os"
	goruntime "runtime"
)

const (
	backendQEMU = "qemu"
	backendVZ   = "vz"
	backendKVM  = "kvm"
)

func BackendFromEnv() (string, error) {
	return backendFromEnv(goruntime.GOOS, os.Getenv("KIDO_BACKEND"), linuxKVMAvailable())
}

func backendFromEnv(goos, b string, linuxKVM bool) (string, error) {
	switch b {
	case "":
		if goos == "darwin" {
			return backendVZ, nil
		}
		if goos == "linux" && linuxKVM {
			return backendKVM, nil
		}
		return backendQEMU, nil
	case backendVZ:
		if goos != "darwin" {
			return "", fmt.Errorf("KIDO_BACKEND=%s requires macOS (Virtualization.framework is not available on %s); use %s", backendVZ, goos, backendQEMU)
		}
		return backendVZ, nil
	case backendKVM:
		if goos != "linux" {
			return "", fmt.Errorf("KIDO_BACKEND=%s requires Linux (/dev/kvm is not available on %s); use %s or %s", backendKVM, goos, backendVZ, backendQEMU)
		}
		return backendKVM, nil
	case backendQEMU:
		return backendQEMU, nil
	default:
		if goos == "linux" {
			return "", fmt.Errorf("KIDO_BACKEND=%q: must be %s or %s on %s", b, backendKVM, backendQEMU, goos)
		}
		if goos != "darwin" {
			return "", fmt.Errorf("KIDO_BACKEND=%q: must be %s on %s", b, backendQEMU, goos)
		}
		return "", fmt.Errorf("KIDO_BACKEND=%q: must be %s or %s", b, backendVZ, backendQEMU)
	}
}
