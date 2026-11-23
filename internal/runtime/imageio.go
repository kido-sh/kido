package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
	"github.com/kido-sh/kido/internal/registry"
)

// An image archive (kido save, kido load) is an OCI image layout in a tar stream. Each image is
// the artifact internal/registry pushes: an index.json entry named by its io.containerd.image.name
// annotation, pointing at a manifest whose config is image.json without name, tag and size, and
// whose layers are disk.img and initrd.cpio compressed with zstd and the kernel as it is.

const (
	ociLayoutFile = "oci-layout"
	ociIndexFile  = "index.json"
	ociBlobPrefix = "blobs/sha256/"
	ociLayout     = `{"imageLayoutVersion":"1.0.0"}`

	ociMediaTypeManifest = "application/vnd.oci.image.manifest.v1+json"
	ociMediaTypeIndex    = "application/vnd.oci.image.index.v1+json"

	ociAnnotationImageName    = "io.containerd.image.name"
	ociAnnotationRefName      = "org.opencontainers.image.ref.name"
	ociAnnotationCreated      = "org.opencontainers.image.created"
	ociAnnotationTitle        = "org.opencontainers.image.title"
	ociAnnotationUncompressed = "sh.kido.uncompressed.size"

	ociMaxLayout       = 64 << 10
	ociMaxIndex        = 4 << 20
	ociMaxManifest     = 4 << 20
	ociMaxConfig       = 1 << 20
	ociMaxUncompressed = 2 << 40
	// ociMaxWindow bounds decoder memory; kido's own encoder uses a 4 MiB window.
	ociMaxWindow = 128 << 20

	ociSparseBlock  = 4096
	ociCopyChunk    = 1 << 20
	ociMinFree      = 1 << 30
	ociFreeCheckGap = 256 << 20
)

type ociPart struct {
	file      string
	mediaType string
	zstd      bool
}

// ociParts are the image files that travel as layers, in manifest order. Loaded files are always
// written under these fixed names, never under names taken from the archive.
var ociParts = []ociPart{
	{"disk.img", registry.MediaTypeDisk, true},
	{"kernel", registry.MediaTypeKernel, false},
	{"initrd.cpio", registry.MediaTypeInitrd, true},
}

var (
	ociDigestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	ociBlobRE   = regexp.MustCompile(`^blobs/sha256/[a-f0-9]{64}$`)
)

type ociPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type ociDescriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Platform     *ociPlatform      `json:"platform,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

type ociManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        ociDescriptor     `json:"config"`
	Layers        []ociDescriptor   `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}

// ociBlob is a blob spooled to a file under KIDO_HOME/tmp.
type ociBlob struct {
	digest string
	size   int64
	path   string
}

// ociSaved is one image ready to be written to an archive.
type ociSaved struct {
	ref      imageref.Ref
	manifest []byte
	config   []byte
	layers   []ociBlob
}

// SaveImages writes the named images to w as one image archive. The layers are compressed or
// copied under KIDO_HOME/tmp first, so nothing reaches w unless every image could be read.
func (r *Runtime) SaveImages(ctx context.Context, w io.Writer, names []string) error {
	if len(names) == 0 {
		return errors.New("no image to save")
	}
	var refs []imageref.Ref
	seen := map[imageref.Ref]bool{}
	for _, n := range names {
		ref, err := imageref.Parse(n)
		if err != nil {
			return err
		}
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	work, err := r.ociWorkDir("save-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	var imgs []*ociSaved
	for _, ref := range refs {
		s, err := r.ociPrepare(ctx, ref, work)
		if err != nil {
			return err
		}
		imgs = append(imgs, s)
	}
	return ociWrite(ctx, w, imgs)
}

// ociPrepare turns one image into the blobs of an archive.
func (r *Runtime) ociPrepare(ctx context.Context, ref imageref.Ref, work string) (*ociSaved, error) {
	raw, files, err := r.ociOpen(ref)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	var j imageJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("image %s: image.json: %w", ref, err)
	}
	if !uuidRE.MatchString(j.RootUUID) {
		return nil, fmt.Errorf("image %s: invalid rootUUID %s", ref, ociQuote(j.RootUUID))
	}
	if j.Parent != "" {
		if _, err := imageref.Parse(j.Parent); err != nil {
			return nil, fmt.Errorf("image %s: parent: %w", ref, err)
		}
	}
	config, err := ociConfig(&j)
	if err != nil {
		return nil, err
	}
	created := j.Created
	if created.IsZero() {
		created = time.Now()
	}
	m := ociManifest{
		SchemaVersion: 2,
		MediaType:     ociMediaTypeManifest,
		ArtifactType:  registry.ArtifactType,
		Config:        ociDescriptor{MediaType: registry.MediaTypeConfig, Digest: ociDigest(config), Size: int64(len(config))},
		Annotations:   map[string]string{ociAnnotationCreated: created.UTC().Format(time.RFC3339)},
	}
	s := &ociSaved{ref: ref, config: config}
	for i, p := range ociParts {
		st, err := files[i].Stat()
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() || st.Size() == 0 {
			return nil, fmt.Errorf("image %s: %s is not a non-empty regular file", ref, p.file)
		}
		// A compressed copy takes at most about what the file has allocated; a plain copy, its size.
		need := st.Size()
		if p.zstd {
			need = st.Sys().(*syscall.Stat_t).Blocks * 512
		}
		b, n, err := ociSpool(ctx, files[i], work, p.zstd, need)
		if err != nil {
			return nil, fmt.Errorf("image %s: %s: %w", ref, p.file, err)
		}
		s.layers = append(s.layers, b)
		ann := map[string]string{ociAnnotationTitle: p.file}
		if p.zstd {
			ann[ociAnnotationUncompressed] = strconv.FormatInt(n, 10)
		}
		m.Layers = append(m.Layers, ociDescriptor{MediaType: p.mediaType, Digest: b.digest, Size: b.size, Annotations: ann})
	}
	if s.manifest, err = json.Marshal(m); err != nil {
		return nil, err
	}
	return s, nil
}

// ociConfig is the config blob registry.Push uploads for the image, byte for byte: image.json
// without name, tag and size, so one image keeps one digest whatever it is called.
func ociConfig(j *imageJSON) ([]byte, error) {
	return json.Marshal(struct {
		Created  time.Time          `json:"created"`
		RootUUID string             `json:"rootUUID"`
		Parent   string             `json:"parent,omitempty"`
		Config   engine.ImageConfig `json:"config"`
	}{j.Created.UTC(), j.RootUUID, j.Parent, j.Config})
}

// ociOpen reads image.json and opens the image's files under the image lock, so a concurrent
// load, pull or commit cannot swap the image halfway through a save.
func (r *Runtime) ociOpen(ref imageref.Ref) ([]byte, []*os.File, error) {
	unlock, err := lock(filepath.Join(r.imagesDir(), ".lock"))
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	dir := ref.Dir(r.Home)
	raw, err := ociReadFile(filepath.Join(dir, "image.json"), ociMaxConfig)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("no such image: %s", ref)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("image %s: %w", ref, err)
	}
	var files []*os.File
	for _, p := range ociParts {
		f, err := ociOpenRegular(filepath.Join(dir, p.file))
		if err != nil {
			for _, f := range files {
				f.Close()
			}
			return nil, nil, fmt.Errorf("image %s: %w", ref, err)
		}
		files = append(files, f)
	}
	return raw, files, nil
}

// ociOpenRegular opens path only when it is a regular file: a FIFO would block with the image
// lock held.
func ociOpenRegular(path string) (*os.File, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	return os.Open(path)
}

func ociReadFile(path string, limit int64) ([]byte, error) {
	f, err := ociOpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ociReadAll(f, limit, filepath.Base(path))
}

func ociReadAll(r io.Reader, limit int64, name string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return b, nil
}

// ociWrite writes oci-layout, index.json, the manifests and configs, then the layers; a blob
// that several images share is written once.
func ociWrite(ctx context.Context, w io.Writer, imgs []*ociSaved) error {
	idx := ociIndex{SchemaVersion: 2, MediaType: ociMediaTypeIndex}
	for _, s := range imgs {
		idx.Manifests = append(idx.Manifests, ociDescriptor{
			MediaType:    ociMediaTypeManifest,
			ArtifactType: registry.ArtifactType,
			Digest:       ociDigest(s.manifest),
			Size:         int64(len(s.manifest)),
			Platform:     &ociPlatform{Architecture: "arm64", OS: "darwin"},
			Annotations:  map[string]string{ociAnnotationImageName: s.ref.String(), ociAnnotationRefName: s.ref.Tag},
		})
	}
	ib, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	epoch := time.Unix(0, 0)
	header := func(name string, size int64) error {
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: 0o644, ModTime: epoch})
	}
	small := func(name string, b []byte) error {
		if err := header(name, int64(len(b))); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	if err := small(ociLayoutFile, []byte(ociLayout)); err != nil {
		return err
	}
	if err := small(ociIndexFile, ib); err != nil {
		return err
	}
	for _, d := range []string{"blobs/", ociBlobPrefix} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d, Mode: 0o755, ModTime: epoch}); err != nil {
			return err
		}
	}
	written := map[string]bool{}
	blobName := func(digest string) string { return ociBlobPrefix + strings.TrimPrefix(digest, "sha256:") }
	for _, s := range imgs {
		for _, b := range [][]byte{s.manifest, s.config} {
			if d := ociDigest(b); !written[d] {
				written[d] = true
				if err := small(blobName(d), b); err != nil {
					return err
				}
			}
		}
	}
	buf := make([]byte, ociCopyChunk)
	for _, s := range imgs {
		for _, b := range s.layers {
			if written[b.digest] {
				continue
			}
			written[b.digest] = true
			f, err := os.Open(b.path)
			if err != nil {
				return err
			}
			if err = header(blobName(b.digest), b.size); err == nil {
				_, err = io.CopyBuffer(tw, ociCtxReader{ctx, f}, buf)
			}
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

// ociArchive is what an archive holds that LoadImages uses, its blobs spooled under work.
type ociArchive struct {
	layout []byte
	index  []byte
	blobs  map[string]ociBlob
}

// ociLoad is one verified image of an archive, its layers in ociParts order.
type ociLoad struct {
	ref    imageref.Ref
	cfg    *imageJSON
	layers []ociDescriptor
}

// LoadImages adds every image of an image archive to the store and returns their references.
// The archive is untrusted: only the entries of an OCI layout are read, every blob is checked
// against its digest, manifests and configs are checked like a pull checks them, and no image
// is stored before all of them are verified and extracted.
func (r *Runtime) LoadImages(ctx context.Context, rd io.Reader) ([]string, error) {
	work, err := r.ociWorkDir("load-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	a, err := ociRead(ctx, rd, work)
	if err != nil {
		return nil, err
	}
	loads, err := a.images()
	if err != nil {
		return nil, err
	}
	for _, l := range loads {
		if users := r.imageUsers(l.ref); len(users) > 0 {
			return nil, fmt.Errorf("image %s is in use by container %s", l.ref, users[0])
		}
	}
	if err := a.extract(ctx, loads, work); err != nil {
		return nil, err
	}
	var refs []string
	for i, l := range loads {
		dir := ociImageDir(work, i)
		img, err := r.storeImage(l.ref, &imageJSON{RootUUID: l.cfg.RootUUID, Parent: l.cfg.Parent, Config: l.cfg.Config}, func(tmp string) error {
			for _, p := range ociParts {
				if err := os.Rename(filepath.Join(dir, p.file), filepath.Join(tmp, p.file)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return refs, err
		}
		refs = append(refs, l.ref.String())
		if !l.cfg.Created.IsZero() {
			if err := r.ociSetCreated(l.ref, img.Created, l.cfg.Created); err != nil {
				return refs, err
			}
		}
	}
	return refs, nil
}

// ociRead reads an archive, keeping oci-layout and index.json in memory and spooling each blob
// under work while it verifies its digest. Other regular files, such as the manifest.json and
// repositories of a legacy image archive, are skipped; links, devices and names that escape are refused.
func ociRead(ctx context.Context, rd io.Reader, work string) (*ociArchive, error) {
	dir := filepath.Join(work, "blobs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	a := &ociArchive{blobs: map[string]ociBlob{}}
	seen := map[string]bool{}
	tr := tar.NewReader(ociCtxReader{ctx, rd})
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if ociEscapes(hdr.Name) {
			return nil, fmt.Errorf("archive entry %s escapes the archive", ociQuote(hdr.Name))
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("archive entry %s is %s: only regular files and directories are allowed", ociQuote(hdr.Name), ociTypeName(hdr.Typeflag))
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name != ociLayoutFile && name != ociIndexFile && !ociBlobRE.MatchString(name) {
			continue
		}
		if seen[name] {
			return nil, fmt.Errorf("the archive holds %s twice", name)
		}
		seen[name] = true
		switch name {
		case ociLayoutFile:
			a.layout, err = ociReadEntry(tr, hdr, ociMaxLayout, name)
		case ociIndexFile:
			a.index, err = ociReadEntry(tr, hdr, ociMaxIndex, name)
		default:
			digest := "sha256:" + strings.TrimPrefix(name, ociBlobPrefix)
			var b ociBlob
			if b, _, err = ociSpool(ctx, tr, dir, false, hdr.Size); err != nil {
				err = fmt.Errorf("reading %s: %w", name, err)
				break
			}
			if b.digest != digest {
				os.Remove(b.path)
				err = fmt.Errorf("blob %s does not match its digest", digest)
				break
			}
			a.blobs[digest] = b
		}
		if err != nil {
			return nil, err
		}
	}
	if a.layout == nil || a.index == nil {
		return nil, errors.New("not an image archive: oci-layout or index.json is missing")
	}
	return a, nil
}

func ociTypeName(t byte) string {
	switch t {
	case tar.TypeSymlink:
		return "a symbolic link"
	case tar.TypeLink:
		return "a hard link"
	case tar.TypeChar, tar.TypeBlock:
		return "a device"
	case tar.TypeFifo:
		return "a FIFO"
	}
	return "of type " + strconv.Quote(string(t))
}

// ociEscapes reports a name that is empty, absolute or has a ".." element.
func ociEscapes(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") {
		return true
	}
	for _, e := range strings.Split(name, "/") {
		if e == ".." {
			return true
		}
	}
	return false
}

func ociReadEntry(tr *tar.Reader, hdr *tar.Header, limit int64, name string) ([]byte, error) {
	if hdr.Size > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	b, err := io.ReadAll(tr)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return b, nil
}

// images checks oci-layout, index.json and, for each image, its manifest, config and blobs.
func (a *ociArchive) images() ([]ociLoad, error) {
	var layout struct {
		Version string `json:"imageLayoutVersion"`
	}
	if err := json.Unmarshal(a.layout, &layout); err != nil || layout.Version != "1.0.0" {
		return nil, errors.New("not an image archive: unsupported oci-layout")
	}
	var idx ociIndex
	if err := json.Unmarshal(a.index, &idx); err != nil {
		return nil, fmt.Errorf("index.json: %w", err)
	}
	if idx.SchemaVersion != 2 || (idx.MediaType != "" && idx.MediaType != ociMediaTypeIndex) {
		return nil, fmt.Errorf("index.json: unsupported index (schemaVersion %d, mediaType %s)", idx.SchemaVersion, ociQuote(idx.MediaType))
	}
	if len(idx.Manifests) == 0 {
		return nil, errors.New("the archive holds no image")
	}
	var loads []ociLoad
	seen := map[imageref.Ref]bool{}
	for i, d := range idx.Manifests {
		l, err := a.image(d)
		if err != nil {
			return nil, fmt.Errorf("index.json entry %d: %w", i+1, err)
		}
		if seen[l.ref] {
			return nil, fmt.Errorf("the archive holds image %s twice", l.ref)
		}
		seen[l.ref] = true
		loads = append(loads, l)
	}
	return loads, nil
}

func (a *ociArchive) image(d ociDescriptor) (ociLoad, error) {
	var l ociLoad
	if d.MediaType != ociMediaTypeManifest || (d.ArtifactType != "" && d.ArtifactType != registry.ArtifactType) {
		return l, fmt.Errorf("not a kido image (media type %s, artifact type %s)", ociQuote(d.MediaType), ociQuote(d.ArtifactType))
	}
	if p := d.Platform; p != nil && (p.OS != "darwin" || p.Architecture != "arm64") {
		return l, fmt.Errorf("platform %s/%s, want darwin/arm64", ociQuote(p.OS), ociQuote(p.Architecture))
	}
	name := d.Annotations[ociAnnotationImageName]
	if name == "" || len(name) > 400 {
		return l, fmt.Errorf("missing or invalid %s annotation", ociAnnotationImageName)
	}
	ref, err := imageref.Parse(name)
	if err != nil {
		return l, err
	}
	l.ref = ref
	mb, err := a.read(d, ociMaxManifest)
	if err != nil {
		return l, fmt.Errorf("image %s: manifest: %w", ref, err)
	}
	var m ociManifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return l, fmt.Errorf("image %s: manifest: %w", ref, err)
	}
	if err := ociCheckManifest(&m); err != nil {
		return l, fmt.Errorf("image %s: %w", ref, err)
	}
	cb, err := a.read(m.Config, ociMaxConfig)
	if err != nil {
		return l, fmt.Errorf("image %s: config: %w", ref, err)
	}
	if l.cfg, err = ociParseConfig(cb); err != nil {
		return l, fmt.Errorf("image %s: %w", ref, err)
	}
	l.layers = make([]ociDescriptor, len(ociParts))
	for _, ld := range m.Layers {
		k := ociPartIndex(ld.MediaType)
		if _, err := a.blob(ld); err != nil {
			return l, fmt.Errorf("image %s: %s: %w", ref, ociParts[k].file, err)
		}
		l.layers[k] = ld
	}
	return l, nil
}

// blob is the spooled blob for d, which must have d's size.
func (a *ociArchive) blob(d ociDescriptor) (ociBlob, error) {
	b, ok := a.blobs[d.Digest]
	if !ok {
		return b, fmt.Errorf("blob %s is missing from the archive", ociQuote(d.Digest))
	}
	if b.size != d.Size {
		return b, fmt.Errorf("blob %s has %d bytes, its descriptor says %d", d.Digest, b.size, d.Size)
	}
	return b, nil
}

func (a *ociArchive) read(d ociDescriptor, limit int64) ([]byte, error) {
	if !ociDigestRE.MatchString(d.Digest) || d.Size <= 0 || d.Size > limit {
		return nil, errors.New("invalid descriptor")
	}
	b, err := a.blob(d)
	if err != nil {
		return nil, err
	}
	return ociReadFile(b.path, limit)
}

func ociPartIndex(mediaType string) int {
	for i, p := range ociParts {
		if p.mediaType == mediaType {
			return i
		}
	}
	return -1
}

// ociCheckManifest accepts exactly one layer of each kido media type and nothing else, like
// internal/registry's checkManifest.
func ociCheckManifest(m *ociManifest) error {
	if m.SchemaVersion != 2 || m.MediaType != ociMediaTypeManifest {
		return fmt.Errorf("unsupported manifest (schemaVersion %d, mediaType %s)", m.SchemaVersion, ociQuote(m.MediaType))
	}
	if m.ArtifactType != registry.ArtifactType {
		return fmt.Errorf("not a kido image: artifactType %s", ociQuote(m.ArtifactType))
	}
	if m.Config.MediaType != registry.MediaTypeConfig {
		return fmt.Errorf("not a kido image: config media type %s", ociQuote(m.Config.MediaType))
	}
	if !ociDigestRE.MatchString(m.Config.Digest) || m.Config.Size <= 0 || m.Config.Size > ociMaxConfig {
		return errors.New("invalid config descriptor")
	}
	if len(m.Layers) != len(ociParts) {
		return fmt.Errorf("kido image with %d layers, want %d", len(m.Layers), len(ociParts))
	}
	seen := map[int]bool{}
	for _, l := range m.Layers {
		k := ociPartIndex(l.MediaType)
		if k < 0 {
			return fmt.Errorf("unknown layer media type %s", ociQuote(l.MediaType))
		}
		if seen[k] {
			return fmt.Errorf("duplicate layer %s", l.MediaType)
		}
		seen[k] = true
		if !ociDigestRE.MatchString(l.Digest) || l.Size <= 0 {
			return fmt.Errorf("invalid descriptor for %s", ociParts[k].file)
		}
		if ociParts[k].zstd {
			if _, err := ociUncompressedSize(l); err != nil {
				return err
			}
		}
	}
	return nil
}

func ociUncompressedSize(d ociDescriptor) (int64, error) {
	n, err := strconv.ParseInt(d.Annotations[ociAnnotationUncompressed], 10, 64)
	if err != nil || n < 0 || n > ociMaxUncompressed {
		return 0, fmt.Errorf("layer %s: missing or invalid %s annotation", ociQuote(d.Digest), ociAnnotationUncompressed)
	}
	return n, nil
}

// ociParseConfig type-checks a config blob like internal/registry's parseImageConfig. The root
// UUID ends up on the guest kernel command line, so it must be a plain UUID.
func ociParseConfig(b []byte) (*imageJSON, error) {
	var c imageJSON
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("image config: %w", err)
	}
	if !uuidRE.MatchString(c.RootUUID) {
		return nil, fmt.Errorf("image config: invalid rootUUID %s", ociQuote(c.RootUUID))
	}
	if c.Size < 0 {
		return nil, errors.New("image config: negative size")
	}
	if c.Parent != "" {
		if _, err := imageref.Parse(c.Parent); err != nil {
			return nil, fmt.Errorf("image config: parent: %w", err)
		}
	}
	return &c, nil
}

func ociImageDir(work string, i int) string {
	return filepath.Join(work, "image-"+strconv.Itoa(i))
}

// extract expands each image's layers into its directory under work; a layer an earlier image
// already expanded is cloned from it. Blobs no image uses are removed first, and each layer blob
// after its last use, so the archive's copy does not stay next to the extracted files longer than
// needed.
func (a *ociArchive) extract(ctx context.Context, loads []ociLoad, work string) error {
	last := map[string]int{}
	for i, l := range loads {
		for _, d := range l.layers {
			last[d.Digest] = i
		}
	}
	for digest, b := range a.blobs {
		if _, ok := last[digest]; !ok {
			os.Remove(b.path)
		}
	}
	done := map[string]string{}
	for i, l := range loads {
		dir := ociImageDir(work, i)
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		for k, p := range ociParts {
			d := l.layers[k]
			src, dst := a.blobs[d.Digest].path, filepath.Join(dir, p.file)
			size, _ := ociUncompressedSize(d)
			key := fmt.Sprintf("%s %t %d", d.Digest, p.zstd, size)
			var err error
			switch prev, ok := done[key]; {
			case ok:
				err = cloneFile(prev, dst)
			case p.zstd:
				err = ociDecompress(ctx, src, dst, size)
			default:
				err = cloneFile(src, dst)
			}
			if err != nil {
				return fmt.Errorf("image %s: extracting %s: %w", l.ref, p.file, err)
			}
			done[key] = dst
		}
		for _, d := range l.layers {
			if last[d.Digest] == i {
				os.Remove(a.blobs[d.Digest].path)
			}
		}
	}
	return nil
}

// ociSetCreated puts the archive's creation time back into image.json, unless the image was
// replaced after storeImage stamped it.
func (r *Runtime) ociSetCreated(ref imageref.Ref, stamped, created time.Time) error {
	unlock, err := lock(filepath.Join(r.imagesDir(), ".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	j, err := r.loadImage(ref)
	if err != nil {
		return err
	}
	if !j.Created.Equal(stamped) {
		return nil
	}
	j.Created = created.UTC()
	return writeJSON(filepath.Join(ref.Dir(r.Home), "image.json"), j)
}

// ociWorkDir is a new directory under KIDO_HOME/tmp, on the store's volume.
func (r *Runtime) ociWorkDir(prefix string) (string, error) {
	tmp := filepath.Join(r.Home, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(tmp, prefix)
}

func ociDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ociQuote quotes a value taken from an archive for a message: control characters escaped, cut
// to a readable length.
func ociQuote(s string) string {
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return strconv.Quote(s)
}

type ociCtxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ociCtxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type ociCounter struct {
	w io.Writer
	n int64
}

func (c *ociCounter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ociSpool copies src into a new file in dir, zstd-compressed when compress is set, hashing what
// it writes, and returns the file with the number of bytes read from src. need is what the copy
// may take at most; it fails up front when that would leave less than ociMinFree free.
func ociSpool(ctx context.Context, src io.Reader, dir string, compress bool, need int64) (b ociBlob, n int64, err error) {
	out, err := os.CreateTemp(dir, "blob-")
	if err != nil {
		return b, 0, err
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(out.Name())
		}
	}()
	if err = ociCheckFree(out, need); err != nil {
		return b, 0, err
	}
	h := sha256.New()
	cw := &ociCounter{w: io.MultiWriter(out, h)}
	in := ociCtxReader{ctx, src}
	if compress {
		var enc *zstd.Encoder
		enc, err = zstd.NewWriter(cw,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithWindowSize(4<<20),
			zstd.WithEncoderConcurrency(min(goruntime.GOMAXPROCS(0), 4)),
			zstd.WithConcurrentBlocks(true))
		if err != nil {
			return b, 0, err
		}
		n, err = enc.ReadFrom(in)
		if cerr := enc.Close(); err == nil {
			err = cerr
		}
	} else {
		n, err = io.CopyBuffer(cw, in, make([]byte, ociCopyChunk))
	}
	if err != nil {
		return b, 0, err
	}
	if err = out.Close(); err != nil {
		return b, 0, err
	}
	return ociBlob{digest: "sha256:" + hex.EncodeToString(h.Sum(nil)), size: cw.n, path: out.Name()}, n, nil
}

// ociDecompress expands a verified zstd blob into a new file at dst, which must come out at
// exactly size bytes. All-zero blocks become holes, so a sparse disk stays sparse.
func ociDecompress(ctx context.Context, src, dst string, size int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dec, err := zstd.NewReader(in, zstd.WithDecoderMaxWindow(ociMaxWindow))
	if err != nil {
		return err
	}
	defer dec.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := ociWriteSparse(out, ociCtxReader{ctx, dec}, size)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("decompressed to %d bytes, the manifest says %d", n, size)
	}
	return nil
}

var ociZeroBlock [ociSparseBlock]byte

type ociExtent struct{ off, n int64 }

// ociWriteSparse copies r into the empty file f, skipping all-zero blocks instead of writing
// them, and refuses to produce more than limit bytes. APFS fills holes shorter than about 16 MiB
// with zeros when it writes the data around them back, so every ociFreeCheckGap bytes the zero
// runs so far are punched out again and the free space is checked against what is really
// allocated.
func ociWriteSparse(f *os.File, r io.Reader, limit int64) (int64, error) {
	if err := ociCheckFree(f, 0); err != nil {
		return 0, err
	}
	buf := make([]byte, ociCopyChunk)
	var off, settled int64
	var zeros []ociExtent
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if off+int64(n) > limit {
				return off, fmt.Errorf("data exceeds the declared %d bytes", limit)
			}
			if werr := ociWriteNonZero(f, buf[:n], off, &zeros); werr != nil {
				return off, werr
			}
			off += int64(n)
			if off-settled >= ociFreeCheckGap {
				if err := ociSettle(f, &zeros, off); err != nil {
					return off, err
				}
				settled = off
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return off, err
		}
	}
	if err := f.Truncate(off); err != nil {
		return off, err
	}
	return off, ociSettle(f, &zeros, off)
}

// ociSettle writes the file back, punches the zero runs recorded so far below end and checks
// that the disk keeps ociMinFree. A run reaching end stays recorded: the next data written after
// it can make APFS fill it again.
func ociSettle(f *os.File, zeros *[]ociExtent, end int64) error {
	if len(*zeros) > 0 {
		if err := syscall.Fsync(int(f.Fd())); err != nil {
			return err
		}
		for _, z := range *zeros {
			start, stop := z.off, min(z.off+z.n, end)
			stop -= stop % ociSparseBlock
			// Holes only save space: a file system without F_PUNCHHOLE keeps the zeros.
			if stop > start && ociPunchHole(f, start, stop-start) != nil {
				break
			}
		}
		last := (*zeros)[len(*zeros)-1]
		*zeros = (*zeros)[:0]
		if last.off+last.n >= end {
			*zeros = append(*zeros, last)
		}
	}
	return ociCheckFree(f, 0)
}

// ociWriteNonZero writes b at off, one write per run of blocks that are not all zero, and
// appends the zero runs to zeros. off is a multiple of ociCopyChunk, so the blocks line up with
// the file system's.
func ociWriteNonZero(f *os.File, b []byte, off int64, zeros *[]ociExtent) error {
	block := func(i int) []byte { return b[i:min(i+ociSparseBlock, len(b))] }
	isZero := func(i int) bool { blk := block(i); return bytes.Equal(blk, ociZeroBlock[:len(blk)]) }
	for i := 0; i < len(b); {
		zero := isZero(i)
		j := i
		for j < len(b) && isZero(j) == zero {
			j += len(block(j))
		}
		if !zero {
			if _, err := f.WriteAt(b[i:j], off+int64(i)); err != nil {
				return err
			}
		} else if z := *zeros; len(z) > 0 && z[len(z)-1].off+z[len(z)-1].n == off+int64(i) {
			z[len(z)-1].n += int64(j - i)
		} else {
			*zeros = append(*zeros, ociExtent{off + int64(i), int64(j - i)})
		}
		i = j
	}
	return nil
}

// ociPunchHole deallocates [off, off+n) of f, which must be block-aligned. F_PUNCHHOLE takes a
// struct fpunchhole {u32 flags, reserved; off_t offset, length}: the leading fields of Fstore_t.
func ociPunchHole(f *os.File, off, n int64) error {
	return unix.FcntlFstore(f.Fd(), unix.F_PUNCHHOLE, &unix.Fstore_t{Offset: off, Length: n})
}

// ociCheckFree fails when need more bytes on f's file system would leave less than ociMinFree,
// so sizes an archive declares cannot fill the disk.
func ociCheckFree(f *os.File, need int64) error {
	var st syscall.Statfs_t
	if err := syscall.Fstatfs(int(f.Fd()), &st); err != nil {
		return err
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	if free-need < ociMinFree {
		return fmt.Errorf("not enough free space: %d MiB free, %d MiB more needed, %d MiB must stay free", free>>20, need>>20, ociMinFree>>20)
	}
	return nil
}
