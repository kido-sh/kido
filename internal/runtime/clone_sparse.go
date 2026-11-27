package runtime

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func copySparse(dst, src *os.File) error {
	st, err := src.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size == 0 {
		return nil
	}
	for off := int64(0); off < size; {
		dataOff, err := src.Seek(off, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break
			}
			if off == 0 {
				if _, serr := src.Seek(0, io.SeekStart); serr != nil {
					return serr
				}
				if _, serr := dst.Seek(0, io.SeekStart); serr != nil {
					return serr
				}
				_, cerr := io.Copy(dst, src)
				return cerr
			}
			return err
		}
		if dataOff >= size {
			break
		}
		holeOff, err := src.Seek(dataOff, unix.SEEK_HOLE)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				holeOff = size
			} else {
				return err
			}
		}
		if holeOff > size {
			holeOff = size
		}
		if holeOff <= dataOff {
			return fmt.Errorf("sparse copy: invalid extent [%d, %d)", dataOff, holeOff)
		}
		if _, err := src.Seek(dataOff, io.SeekStart); err != nil {
			return err
		}
		if _, err := dst.Seek(dataOff, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(dst, src, holeOff-dataOff); err != nil {
			return err
		}
		off = holeOff
	}
	return dst.Truncate(size)
}
