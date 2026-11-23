// Package registry pushes kido images to OCI registries and pulls them back.
//
// An image travels as an OCI index (platform darwin/arm64) pointing at an OCI image manifest with
// artifactType ArtifactType: the config blob is image.json, and the layers are disk.img and
// initrd.cpio compressed with zstd and the kernel as it is.
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
)

const (
	ArtifactType    = "application/vnd.kido.image.v1"
	MediaTypeConfig = "application/vnd.kido.image.config.v1+json"
	MediaTypeDisk   = "application/vnd.kido.image.disk.v1.raw+zstd"
	MediaTypeKernel = "application/vnd.kido.image.kernel.v1"
	MediaTypeInitrd = "application/vnd.kido.image.initrd.v1+zstd"

	mediaTypeManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeIndex    = "application/vnd.oci.image.index.v1+json"

	annotationCreated          = "org.opencontainers.image.created"
	annotationTitle            = "org.opencontainers.image.title"
	annotationVersion          = "sh.kido.version"
	annotationUncompressedSize = "sh.kido.uncompressed.size"

	configFile      = "image.json"
	maxManifestSize = 4 << 20
	maxConfigSize   = 1 << 20
	maxUncompressed = 2 << 40
)

type part struct {
	file      string
	mediaType string
	zstd      bool
}

// parts are the image files that travel as layers, in manifest order. Pulled files are always
// written under these fixed names, never under names taken from the manifest.
var parts = []part{
	{"disk.img", MediaTypeDisk, true},
	{"kernel", MediaTypeKernel, false},
	{"initrd.cpio", MediaTypeInitrd, true},
}

type platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Platform     *platform         `json:"platform,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        descriptor        `json:"config"`
	Layers        []descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type index struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Manifests     []descriptor      `json:"manifests"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

var (
	digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	uuidRE   = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
)

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// imageConfig is an image's image.json.
type imageConfig struct {
	Name     string             `json:"name"`
	Tag      string             `json:"tag"`
	Created  time.Time          `json:"created"`
	RootUUID string             `json:"rootUUID"`
	Size     int64              `json:"size"`
	Parent   string             `json:"parent,omitempty"`
	Config   engine.ImageConfig `json:"config"`
}

// parseImageConfig type-checks image.json. The root UUID ends up on the guest kernel command line,
// so it must be a plain UUID.
func parseImageConfig(b []byte) (*imageConfig, error) {
	var c imageConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("image.json: %w", err)
	}
	if !uuidRE.MatchString(c.RootUUID) {
		return nil, fmt.Errorf("image.json: invalid rootUUID %q", printable(c.RootUUID))
	}
	if c.Size < 0 {
		return nil, fmt.Errorf("image.json: negative size")
	}
	if c.Parent != "" {
		if _, err := imageref.Parse(c.Parent); err != nil {
			return nil, fmt.Errorf("image.json: parent: %w", err)
		}
	}
	return &c, nil
}

// registryConfig is the config blob pushed for an image: the validated image.json without name,
// tag and size, which describe only the local copy, encoded the same way every time, so one image
// keeps one digest whatever it is called and however its image.json was written.
func registryConfig(c *imageConfig) ([]byte, error) {
	return json.Marshal(struct {
		Created  time.Time          `json:"created"`
		RootUUID string             `json:"rootUUID"`
		Parent   string             `json:"parent,omitempty"`
		Config   engine.ImageConfig `json:"config"`
	}{c.Created.UTC(), c.RootUUID, c.Parent, c.Config})
}

// localImageConfig is image.json for the local copy: the validated fields encoded again, with the
// copy's name, tag and size. Encoding the struct drops keys that differ from the known ones only in
// case, which the runtime's decoder could resolve differently from this validation.
func localImageConfig(c *imageConfig, ref imageref.Ref, size int64) ([]byte, error) {
	l := *c
	l.Name, l.Tag, l.Size = ref.Name, ref.Tag, size
	out, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func partByMediaType(mt string) (part, bool) {
	for _, p := range parts {
		if p.mediaType == mt {
			return p, true
		}
	}
	return part{}, false
}

// checkManifest accepts exactly one layer of each kido media type and nothing else.
func checkManifest(m *manifest) error {
	if m.SchemaVersion != 2 || m.MediaType != mediaTypeManifest {
		return fmt.Errorf("unsupported manifest (schemaVersion %d, mediaType %q)", m.SchemaVersion, printable(m.MediaType))
	}
	if m.ArtifactType != ArtifactType {
		return fmt.Errorf("not a kido image: artifactType %q", printable(m.ArtifactType))
	}
	if m.Config.MediaType != MediaTypeConfig {
		return fmt.Errorf("not a kido image: config media type %q", printable(m.Config.MediaType))
	}
	if !digestRE.MatchString(m.Config.Digest) || m.Config.Size <= 0 || m.Config.Size > maxConfigSize {
		return fmt.Errorf("invalid config descriptor")
	}
	if len(m.Layers) != len(parts) {
		return fmt.Errorf("kido image with %d layers, want %d", len(m.Layers), len(parts))
	}
	seen := map[string]bool{}
	for _, l := range m.Layers {
		p, ok := partByMediaType(l.MediaType)
		if !ok {
			return fmt.Errorf("unknown layer media type %q", printable(l.MediaType))
		}
		if seen[p.mediaType] {
			return fmt.Errorf("duplicate layer %s", p.mediaType)
		}
		seen[p.mediaType] = true
		if !digestRE.MatchString(l.Digest) || l.Size <= 0 {
			return fmt.Errorf("invalid descriptor for %s", p.file)
		}
		if p.zstd {
			if _, err := uncompressedSize(l); err != nil {
				return err
			}
		}
	}
	return nil
}

func uncompressedSize(d descriptor) (int64, error) {
	n, err := strconv.ParseInt(d.Annotations[annotationUncompressedSize], 10, 64)
	if err != nil || n < 0 || n > maxUncompressed {
		return 0, fmt.Errorf("layer %s: missing or invalid %s annotation", d.Digest, annotationUncompressedSize)
	}
	return n, nil
}

// Sanitize replaces control characters, so text that came from a registry cannot drive the
// terminal it is printed on.
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, s)
}

// printable is Sanitize for one registry-provided value, cut to a readable length.
func printable(s string) string {
	s = Sanitize(s)
	if r := []rune(s); len(r) > 300 {
		return string(r[:300]) + "..."
	}
	return s
}
