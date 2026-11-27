//go:build !linux

package runtime

import "os"

func preallocateCowBitmap(f *os.File, off, length int64) error {
	return writeZeroRegion(f, off, length)
}
