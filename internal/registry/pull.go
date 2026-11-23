package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/kido-sh/kido/internal/imageref"
)

type PullOptions struct {
	Home     string    // KIDO_HOME
	Remote   string    // remote reference
	Image    string    // local NAME[:TAG]; empty: the same as Remote
	Force    bool      // replace an existing local image
	Progress io.Writer // nil: no progress output
	// InUse, when set, is asked before an existing image is replaced and refuses with an error,
	// e.g. because a container's disk is an overlay on it.
	InUse func(ref string) error
}

// Pull downloads an image into the local store and returns the local reference and the digest
// the remote reference resolved to. Nothing appears in the store until every blob has been
// verified; the image directory is then renamed into place.
func Pull(ctx context.Context, o PullOptions) (string, string, error) {
	r, err := parseRemote(o.Remote)
	if err != nil {
		return "", "", err
	}
	if o.Image == "" {
		o.Image = r.ref.String()
	}
	local, err := imageref.Parse(o.Image)
	if err != nil {
		return "", "", err
	}
	dst := local.Dir(o.Home)
	if _, err := os.Lstat(dst); err == nil {
		if !o.Force {
			return "", "", fmt.Errorf("image %s already exists; pass --force to replace it", local)
		}
		if o.InUse != nil {
			if err := o.InUse(local.String()); err != nil {
				return "", "", err
			}
		}
	}

	c, err := newClient(ctx, r, false)
	if err != nil {
		return "", "", err
	}
	m, digest, err := c.resolve(ctx, r.ref.Tag)
	if err != nil {
		return "", "", err
	}
	if err := checkManifest(m); err != nil {
		return "", "", fmt.Errorf("%s: %w", r.ref, err)
	}

	tmp, err := tempDir(o.Home)
	if err != nil {
		return "", "", err
	}
	work, err := os.MkdirTemp(tmp, "pull-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(work)
	img := filepath.Join(work, "image")
	if err := os.Mkdir(img, 0o700); err != nil {
		return "", "", err
	}

	var cfg bytes.Buffer
	if err := c.fetchBlob(ctx, m.Config, &cfg, newProgress(nil, "", "", 0)); err != nil {
		return "", "", err
	}
	ic, err := parseImageConfig(cfg.Bytes())
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", r.ref, err)
	}
	for i, l := range m.Layers {
		pt, _ := partByMediaType(l.MediaType)
		blob := filepath.Join(work, fmt.Sprintf("blob-%d", i))
		if err := downloadBlob(ctx, c, l, blob, pt.file, o.Progress); err != nil {
			return "", "", err
		}
		dstFile := filepath.Join(img, pt.file)
		if !pt.zstd {
			if err := os.Rename(blob, dstFile); err != nil {
				return "", "", err
			}
			continue
		}
		size, _ := uncompressedSize(l)
		p := newProgress(o.Progress, pt.file, "extracting", size)
		if err := decompressFile(ctx, blob, dstFile, size, p); err != nil {
			p.finish("failed")
			return "", "", fmt.Errorf("extracting %s: %w", pt.file, err)
		}
		p.finish("extracted " + humanBytes(size))
		if err := os.Remove(blob); err != nil {
			return "", "", err
		}
	}

	st, err := os.Stat(filepath.Join(img, "disk.img"))
	if err != nil {
		return "", "", err
	}
	// Like the runtime, size is what disk.img takes on disk.
	localCfg, err := localImageConfig(ic, local, st.Sys().(*syscall.Stat_t).Blocks*512)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(img, configFile), localCfg, 0o600); err != nil {
		return "", "", err
	}
	if err := install(o, local, img, work); err != nil {
		return "", "", err
	}
	return local.String(), digest, nil
}

// resolve fetches the manifest for ref, following an index to its darwin/arm64 entry, and
// returns it with the digest ref resolved to.
func (c *client) resolve(ctx context.Context, ref string) (*manifest, string, error) {
	body, mt, err := c.getManifest(ctx, ref)
	if err != nil {
		return nil, "", err
	}
	top := digestOf(body)
	switch mt {
	case mediaTypeManifest:
	case mediaTypeIndex:
		var idx index
		if err := json.Unmarshal(body, &idx); err != nil {
			return nil, "", fmt.Errorf("index %s: %w", ref, err)
		}
		var d *descriptor
		for i := range idx.Manifests {
			e := &idx.Manifests[i]
			if e.MediaType == mediaTypeManifest && e.Platform != nil && e.Platform.OS == "darwin" && e.Platform.Architecture == "arm64" &&
				(e.ArtifactType == "" || e.ArtifactType == ArtifactType) {
				d = e
				break
			}
		}
		if d == nil {
			return nil, "", fmt.Errorf("%s: no darwin/arm64 kido image in the index", c.r.ref)
		}
		if !digestRE.MatchString(d.Digest) || d.Size <= 0 || d.Size > maxManifestSize {
			return nil, "", fmt.Errorf("%s: invalid manifest descriptor in the index", c.r.ref)
		}
		if body, mt, err = c.getManifest(ctx, d.Digest); err != nil {
			return nil, "", err
		}
		if digestOf(body) != d.Digest || int64(len(body)) != d.Size {
			return nil, "", fmt.Errorf("%s: manifest does not match its digest %s", c.r.ref, d.Digest)
		}
		if mt != mediaTypeManifest {
			return nil, "", fmt.Errorf("%s: index entry has media type %q", c.r.ref, printable(mt))
		}
	default:
		return nil, "", fmt.Errorf("%s is not a kido image (media type %q)", c.r.ref, printable(mt))
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", fmt.Errorf("manifest %s: %w", ref, err)
	}
	return &m, top, nil
}

func downloadBlob(ctx context.Context, c *client, d descriptor, path, name string, w io.Writer) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := checkFreeSpace(f, d.Size); err != nil {
		f.Close()
		return fmt.Errorf("downloading %s: %w", name, err)
	}
	p := newProgress(w, name, "downloading", d.Size)
	err = c.fetchBlob(ctx, d, f, p)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		p.finish("failed")
		return fmt.Errorf("downloading %s: %w", name, err)
	}
	p.finish("downloaded " + humanBytes(d.Size))
	return nil
}

// install renames the verified image directory into the store under the runtime's image lock.
func install(o PullOptions, local imageref.Ref, img, work string) error {
	dst := local.Dir(o.Home)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	unlock, err := lockImages(o.Home)
	if err != nil {
		return err
	}
	defer unlock()
	if o.Force {
		if _, err := os.Lstat(dst); err == nil && o.InUse != nil {
			if err := o.InUse(local.String()); err != nil {
				return err
			}
		}
		old := filepath.Join(work, "old")
		switch err := os.Rename(dst, old); {
		case err == nil:
			if err := os.Rename(img, dst); err != nil {
				os.Rename(old, dst)
				return err
			}
			return nil
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
	}
	if err := os.Rename(img, dst); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("image %s already exists; pass --force to replace it", local)
		}
		return err
	}
	return nil
}

// lockImages takes the same lock as internal/runtime's image store (images/.lock).
func lockImages(home string) (func(), error) {
	dir := filepath.Join(home, "images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func tempDir(home string) (string, error) {
	d := filepath.Join(home, "tmp")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}
