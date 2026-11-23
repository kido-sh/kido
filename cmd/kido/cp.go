package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("cp", "Copy files between a container and the host", ctrCp)
}

// ctrSplitRef splits CONTAINER:PATH; host paths that contain ':' need a ./ or / prefix.
func ctrSplitRef(s string) (ref, p string, ok bool) {
	i := strings.Index(s, ":")
	if i <= 0 || strings.HasPrefix(s, ".") || strings.Contains(s[:i], "/") {
		return "", s, false
	}
	return s[:i], s[i+1:], true
}

func ctrCp(ctx context.Context, args []string) int {
	fs := ctrFlags("cp", "CONTAINER:SRC_PATH DEST_PATH|-\n       kido cp SRC_PATH|- CONTAINER:DEST_PATH")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 125
	}
	sref, spath, sIn := ctrSplitRef(fs.Arg(0))
	dref, dpath, dIn := ctrSplitRef(fs.Arg(1))
	if sIn == dIn {
		return ctrFail(errors.New("cp: exactly one of the two paths must be CONTAINER:PATH"))
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	if sIn {
		err = ctrCopyOut(ctx, rt, sref, spath, dpath)
	} else {
		err = ctrCopyIn(ctx, rt, spath, dref, dpath)
	}
	if err != nil {
		return ctrFail(err)
	}
	return 0
}

func ctrCopyOut(ctx context.Context, rt *runtime.Runtime, ref, src, dst string) error {
	src, err := runtime.GuestPath(src)
	if err != nil {
		return err
	}
	if dst == "-" {
		return rt.CopyOut(ctx, ref, src, os.Stdout)
	}
	destDir, rename := dst, path.Base(src)
	if fi, err := os.Stat(dst); err != nil || !fi.IsDir() {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		destDir, rename = filepath.Dir(dst), filepath.Base(dst)
		if fi, err := os.Stat(destDir); err != nil || !fi.IsDir() {
			return fmt.Errorf("destination directory %s does not exist", destDir)
		}
	}
	if src == "/" {
		if destDir != dst {
			if err := os.Mkdir(dst, 0o755); err != nil {
				return err
			}
			destDir = dst
		}
		rename = ""
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(rt.CopyOut(ctx, ref, src, pw)) }()
	if err := runtime.ExtractToHost(pr, destDir, rename); err != nil {
		pr.CloseWithError(err)
		return err
	}
	_, err = io.Copy(io.Discard, pr)
	return err
}

func ctrCopyIn(ctx context.Context, rt *runtime.Runtime, src, ref, dst string) error {
	wantDir := strings.HasSuffix(dst, "/") || strings.HasSuffix(dst, "/.")
	dst, err := runtime.GuestPath(dst)
	if err != nil {
		return err
	}
	if src == "-" {
		return rt.CopyIn(ctx, ref, os.Stdin, dst)
	}
	if _, err := os.Lstat(src); err != nil {
		return err
	}
	name := filepath.Base(filepath.Clean(src))
	contentsOnly := name == "." || name == "/" || strings.HasSuffix(src, "/.")
	isDir, err := rt.GuestIsDir(ctx, ref, dst)
	if err != nil {
		return err
	}
	dstDir := dst
	switch {
	case contentsOnly:
		if !isDir {
			return fmt.Errorf("destination %s is not a directory in the container", dst)
		}
		name = "."
	case !isDir && wantDir && !ctrHostIsDir(src):
		return fmt.Errorf("destination directory %s does not exist in the container", dst)
	case !isDir:
		dstDir, name = path.Dir(dst), path.Base(dst)
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(runtime.TarHostPath(pw, src, name)) }()
	err = rt.CopyIn(ctx, ref, pr, dstDir)
	pr.CloseWithError(errors.New("copy finished"))
	return err
}

func ctrHostIsDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
