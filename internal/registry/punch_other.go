//go:build !darwin

package registry

import "os"

func punchHole(*os.File, int64, int64) error { return nil }
