package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

const (
	cowMagic      = "KIDOCOW1"
	cowVersion    = 1
	cowBlockSize  = 4096
	cowSectorSize = 512
	cowHeaderSize = 32
	cowChunkBlks  = 64
)

func cowLayout(diskSize int64) (numBlocks, bitmapBytes, dataOffset int64, err error) {
	if diskSize < cowSectorSize || diskSize%cowSectorSize != 0 {
		return 0, 0, 0, fmt.Errorf("invalid CoW disk size %d", diskSize)
	}
	numBlocks = (diskSize-1)/cowBlockSize + 1
	bitmapBytes = (numBlocks-1)/8 + 1
	if bitmapBytes > math.MaxInt64-cowHeaderSize-(cowBlockSize-1) {
		return 0, 0, 0, errors.New("CoW header size overflow")
	}
	dataOffset = ((cowHeaderSize + bitmapBytes + cowBlockSize - 1) / cowBlockSize) * cowBlockSize
	if dataOffset > math.MaxInt64-diskSize {
		return 0, 0, 0, errors.New("CoW file size overflow")
	}
	return numBlocks, bitmapBytes, dataOffset, nil
}

func writeZeroRegion(f *os.File, off, length int64) error {
	var zeros [cowBlockSize]byte
	for done := int64(0); done < length; {
		n := min(int64(len(zeros)), length-done)
		if _, err := f.WriteAt(zeros[:n], off+done); err != nil {
			return err
		}
		done += n
	}
	return nil
}

func createCowOverlay(path string, diskSize int64) error {
	_, bitmapBytes, dataOffset, err := cowLayout(diskSize)
	if err != nil {
		return err
	}
	var hdr [cowHeaderSize]byte
	copy(hdr[0:8], cowMagic)
	binary.LittleEndian.PutUint32(hdr[8:12], cowVersion)
	binary.LittleEndian.PutUint32(hdr[12:16], cowBlockSize)
	binary.LittleEndian.PutUint64(hdr[16:24], uint64(diskSize))
	binary.LittleEndian.PutUint64(hdr[24:32], uint64(dataOffset))

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(hdr[:]); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := preallocateCowBitmap(f, cowHeaderSize, bitmapBytes); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Truncate(dataOffset + diskSize); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func openNoFollowReadOnly(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func readCowMeta(f *os.File, baseSize int64) (diskSize, numBlocks, dataOffset int64, bitmap []byte, err error) {
	st, err := f.Stat()
	if err != nil {
		return 0, 0, 0, nil, err
	}
	if !st.Mode().IsRegular() {
		return 0, 0, 0, nil, errors.New("CoW overlay is not a regular file")
	}
	if st.Size() < cowHeaderSize {
		return 0, 0, 0, nil, fmt.Errorf("CoW overlay too small (%d bytes)", st.Size())
	}
	var hdr [cowHeaderSize]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return 0, 0, 0, nil, err
	}
	if string(hdr[0:8]) != cowMagic {
		return 0, 0, 0, nil, errors.New("invalid CoW magic")
	}
	if v := binary.LittleEndian.Uint32(hdr[8:12]); v != cowVersion {
		return 0, 0, 0, nil, fmt.Errorf("unsupported CoW version %d", v)
	}
	if bs := binary.LittleEndian.Uint32(hdr[12:16]); bs != cowBlockSize {
		return 0, 0, 0, nil, fmt.Errorf("unsupported CoW block size %d", bs)
	}
	rawSize := binary.LittleEndian.Uint64(hdr[16:24])
	rawOff := binary.LittleEndian.Uint64(hdr[24:32])
	if rawSize > math.MaxInt64 || rawOff > math.MaxInt64 {
		return 0, 0, 0, nil, errors.New("CoW header size out of range")
	}
	diskSize = int64(rawSize)
	dataOffset = int64(rawOff)
	if diskSize > baseSize {
		return 0, 0, 0, nil, fmt.Errorf("base disk (%d B) smaller than CoW disk size (%d B)", baseSize, diskSize)
	}
	numBlocks, bitmapBytes, minDataOff, err := cowLayout(diskSize)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	if dataOffset < minDataOff || dataOffset%cowBlockSize != 0 {
		return 0, 0, 0, nil, fmt.Errorf("invalid CoW data offset %d", dataOffset)
	}
	if dataOffset > math.MaxInt64-diskSize || st.Size() < dataOffset+diskSize {
		return 0, 0, 0, nil, fmt.Errorf("CoW overlay truncated (%d < %d)", st.Size(), dataOffset+diskSize)
	}
	bitmap = make([]byte, bitmapBytes)
	if _, err := io.ReadFull(io.NewSectionReader(f, cowHeaderSize, bitmapBytes), bitmap); err != nil {
		return 0, 0, 0, nil, err
	}
	return diskSize, numBlocks, dataOffset, bitmap, nil
}

func isAllZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func cowBlockDirty(bitmap []byte, block int64) bool {
	return (bitmap[block/8] & (1 << uint(block%8))) != 0
}

func flattenCowOverlay(basePath, cowPath, dstPath string) error {
	base, err := openNoFollowReadOnly(basePath)
	if err != nil {
		return err
	}
	defer base.Close()
	baseSt, err := base.Stat()
	if err != nil {
		return err
	}
	if !baseSt.Mode().IsRegular() {
		return errors.New("CoW base disk is not a regular file")
	}

	cow, err := openNoFollowReadOnly(cowPath)
	if err != nil {
		return err
	}
	defer cow.Close()

	diskSize, numBlocks, dataOffset, bitmap, err := readCowMeta(cow, baseSt.Size())
	if err != nil {
		return err
	}

	dst, err := os.OpenFile(dstPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		dst.Close()
		os.Remove(dstPath)
		return err
	}

	if err := copySparse(dst, base); err != nil {
		return fail(err)
	}
	if baseSt.Size() != diskSize {
		if err := dst.Truncate(diskSize); err != nil {
			return fail(err)
		}
	}

	buf := make([]byte, cowChunkBlks*cowBlockSize)
	for blk := int64(0); blk < numBlocks; {
		if !cowBlockDirty(bitmap, blk) {
			blk++
			continue
		}
		runStart := blk
		for blk < numBlocks && blk-runStart < cowChunkBlks && cowBlockDirty(bitmap, blk) {
			blk++
		}
		runOff := runStart * cowBlockSize
		runLen := (blk - runStart) * cowBlockSize
		if runOff+runLen > diskSize {
			runLen = diskSize - runOff
		}
		chunk := buf[:runLen]
		if _, err := io.ReadFull(io.NewSectionReader(cow, dataOffset+runOff, runLen), chunk); err != nil {
			return fail(err)
		}
		if err := applyCowChunk(dst, runOff, chunk); err != nil {
			return fail(err)
		}
	}

	if err := dst.Truncate(diskSize); err != nil {
		return fail(err)
	}
	if err := dst.Close(); err != nil {
		os.Remove(dstPath)
		return err
	}
	return nil
}

func applyCowChunk(dst *os.File, baseOff int64, chunk []byte) error {
	for pos := 0; pos < len(chunk); {
		end := min(pos+cowBlockSize, len(chunk))
		if isAllZero(chunk[pos:end]) {
			zeroStart := pos
			for pos < len(chunk) {
				bEnd := min(pos+cowBlockSize, len(chunk))
				if !isAllZero(chunk[pos:bEnd]) {
					break
				}
				pos = bEnd
			}
			spanOff := baseOff + int64(zeroStart)
			spanLen := int64(pos - zeroStart)
			if spanLen%cowBlockSize == 0 && ociPunchHole(dst, spanOff, spanLen) == nil {
				continue
			}
			if _, err := dst.WriteAt(chunk[zeroStart:pos], spanOff); err != nil {
				return err
			}
			continue
		}
		dataStart := pos
		for pos < len(chunk) {
			bEnd := min(pos+cowBlockSize, len(chunk))
			if isAllZero(chunk[pos:bEnd]) {
				break
			}
			pos = bEnd
		}
		if _, err := dst.WriteAt(chunk[dataStart:pos], baseOff+int64(dataStart)); err != nil {
			return err
		}
	}
	return nil
}
