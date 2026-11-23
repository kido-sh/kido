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
	"strconv"
	"time"

	"github.com/kido-sh/kido/internal/imageref"
)

type PushOptions struct {
	Home     string    // KIDO_HOME
	Image    string    // local NAME[:TAG]
	Remote   string    // remote reference; empty: the same as Image
	Version  string    // kido version, recorded in the manifest
	Progress io.Writer // nil: no progress output
}

type layerFile struct {
	desc descriptor
	src  *os.File // the image's own file, sent as it is
	temp string   // or a compressed copy in KIDO_HOME/tmp
}

// Push uploads a local image and returns the remote reference and the digest of its index.
func Push(ctx context.Context, o PushOptions) (string, string, error) {
	local, err := imageref.Parse(o.Image)
	if err != nil {
		return "", "", err
	}
	if o.Remote == "" {
		o.Remote = local.String()
	}
	r, err := parseRemote(o.Remote)
	if err != nil {
		return "", "", err
	}
	cfg, files, err := openImage(o.Home, local)
	if err != nil {
		return "", "", err
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	ic, err := parseImageConfig(cfg)
	if err != nil {
		return "", "", fmt.Errorf("image %s: %w", local, err)
	}
	if cfg, err = registryConfig(ic); err != nil {
		return "", "", err
	}
	tmp, err := tempDir(o.Home)
	if err != nil {
		return "", "", err
	}

	var layers []layerFile
	defer func() {
		for _, l := range layers {
			if l.temp != "" {
				os.Remove(l.temp)
			}
		}
	}()
	for i, pt := range parts {
		src := files[i]
		st, err := src.Stat()
		if err != nil {
			return "", "", err
		}
		if !st.Mode().IsRegular() || st.Size() == 0 {
			return "", "", fmt.Errorf("image %s: %s is not a non-empty regular file", local, pt.file)
		}
		l := layerFile{src: src}
		ann := map[string]string{annotationTitle: pt.file}
		if pt.zstd {
			p := newProgress(o.Progress, pt.file, "compressing", st.Size())
			var raw int64
			l.temp, l.desc, raw, err = compressFile(ctx, src, tmp, p)
			if err != nil {
				p.finish("failed")
				return "", "", fmt.Errorf("compressing %s: %w", pt.file, err)
			}
			p.finish(fmt.Sprintf("compressed %s to %s", humanBytes(raw), humanBytes(l.desc.Size)))
			ann[annotationUncompressedSize] = strconv.FormatInt(raw, 10)
		} else if l.desc, err = hashFile(ctx, io.NewSectionReader(src, 0, st.Size())); err != nil {
			return "", "", err
		}
		l.desc.MediaType, l.desc.Annotations = pt.mediaType, ann
		layers = append(layers, l)
	}

	created := ic.Created
	if created.IsZero() {
		created = time.Now()
	}
	m := manifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeManifest,
		ArtifactType:  ArtifactType,
		Config:        descriptor{MediaType: MediaTypeConfig, Digest: digestOf(cfg), Size: int64(len(cfg))},
		Annotations:   map[string]string{annotationCreated: created.UTC().Format(time.RFC3339)},
	}
	if o.Version != "" {
		m.Annotations[annotationVersion] = o.Version
	}
	for _, l := range layers {
		m.Layers = append(m.Layers, l.desc)
	}
	mb, err := json.Marshal(m)
	if err != nil {
		return "", "", err
	}
	ib, err := json.Marshal(index{
		SchemaVersion: 2,
		MediaType:     mediaTypeIndex,
		Manifests: []descriptor{{
			MediaType:    mediaTypeManifest,
			ArtifactType: ArtifactType,
			Digest:       digestOf(mb),
			Size:         int64(len(mb)),
			Platform:     &platform{Architecture: "arm64", OS: "darwin"},
		}},
	})
	if err != nil {
		return "", "", err
	}

	c, err := newClient(ctx, r, true)
	if err != nil {
		return "", "", err
	}
	for _, l := range layers {
		if err := pushBlob(ctx, c, l, o.Progress); err != nil {
			return "", "", err
		}
	}
	if err := pushBlobBytes(ctx, c, m.Config, cfg); err != nil {
		return "", "", err
	}
	if err := c.putManifest(ctx, digestOf(mb), mediaTypeManifest, mb); err != nil {
		return "", "", err
	}
	if err := c.putManifest(ctx, r.ref.Tag, mediaTypeIndex, ib); err != nil {
		return "", "", err
	}
	return r.ref.String(), digestOf(ib), nil
}

func pushBlob(ctx context.Context, c *client, l layerFile, w io.Writer) error {
	name := l.desc.Annotations[annotationTitle]
	if ok, err := c.blobExists(ctx, l.desc); err != nil {
		return err
	} else if ok {
		if w != nil {
			fmt.Fprintf(w, "%s: already exists\n", name)
		}
		return nil
	}
	f := l.src
	if l.temp != "" {
		var err error
		if f, err = os.Open(l.temp); err != nil {
			return err
		}
		defer f.Close()
	}
	p := newProgress(w, name, "pushing", l.desc.Size)
	err := c.uploadBlob(ctx, l.desc, f, p)
	if err != nil {
		p.finish("failed")
		return fmt.Errorf("pushing %s: %w", name, err)
	}
	p.finish("pushed " + humanBytes(l.desc.Size))
	return nil
}

func pushBlobBytes(ctx context.Context, c *client, d descriptor, b []byte) error {
	if ok, err := c.blobExists(ctx, d); err != nil || ok {
		return err
	}
	if err := c.uploadBlob(ctx, d, bytes.NewReader(b), newProgress(nil, "", "", 0)); err != nil {
		return fmt.Errorf("pushing %s: %w", configFile, err)
	}
	return nil
}

// openImage reads image.json and opens the image's files under the image lock, so a concurrent
// pull --force or commit cannot swap the image halfway through a push. Only regular files are
// opened: a FIFO would block with the lock held.
func openImage(home string, ref imageref.Ref) ([]byte, []*os.File, error) {
	unlock, err := lockImages(home)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	dir := ref.Dir(home)
	if st, err := os.Lstat(filepath.Join(dir, configFile)); err == nil && !st.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("image %s: %s is not a regular file", ref, configFile)
	}
	cfg, err := readSmallFile(filepath.Join(dir, configFile), maxConfigSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("no such image: %s", ref)
	}
	if err != nil {
		return nil, nil, err
	}
	var files []*os.File
	for _, pt := range parts {
		path := filepath.Join(dir, pt.file)
		st, err := os.Lstat(path)
		if err == nil && !st.Mode().IsRegular() {
			err = fmt.Errorf("%s is not a regular file", pt.file)
		}
		var f *os.File
		if err == nil {
			f, err = os.Open(path)
		}
		if err != nil {
			for _, f := range files {
				f.Close()
			}
			return nil, nil, fmt.Errorf("image %s: %w", ref, err)
		}
		files = append(files, f)
	}
	return cfg, files, nil
}

func readSmallFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return b, nil
}
