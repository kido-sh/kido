package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
)

// imageJSON is images/<name>/<tag>/image.json.
type imageJSON struct {
	Name     string             `json:"name"`
	Tag      string             `json:"tag"`
	Created  time.Time          `json:"created"`
	RootUUID string             `json:"rootUUID"`
	Size     int64              `json:"size"`
	Parent   string             `json:"parent,omitempty"`
	Config   engine.ImageConfig `json:"config"`
}

func (j *imageJSON) image() *engine.Image {
	return &engine.Image{Name: j.Name, Tag: j.Tag, Created: j.Created, Size: j.Size, RootUUID: j.RootUUID, Parent: j.Parent, Config: j.Config}
}

func (r *Runtime) imagesDir() string { return filepath.Join(r.Home, "images") }

func (r *Runtime) loadImage(ref imageref.Ref) (*imageJSON, error) {
	b, err := os.ReadFile(filepath.Join(ref.Dir(r.Home), "image.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no such image: %s", ref)
		}
		return nil, err
	}
	var j imageJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	j.Name, j.Tag = ref.Name, ref.Tag
	return &j, nil
}

// Image returns one local image.
func (r *Runtime) Image(ctx context.Context, name string) (*engine.Image, error) {
	ref, err := imageref.Parse(name)
	if err != nil {
		return nil, err
	}
	j, err := r.loadImage(ref)
	if err != nil {
		return nil, err
	}
	return j.image(), nil
}

// ImageDir is the store directory of an existing image.
func (r *Runtime) ImageDir(name string) (string, error) {
	ref, err := imageref.Parse(name)
	if err != nil {
		return "", err
	}
	if _, err := r.loadImage(ref); err != nil {
		return "", err
	}
	return ref.Dir(r.Home), nil
}

func (r *Runtime) Images(ctx context.Context) ([]*engine.Image, error) {
	names, err := os.ReadDir(r.imagesDir())
	if err != nil {
		return nil, err
	}
	var out []*engine.Image
	for _, n := range names {
		if !n.IsDir() {
			continue
		}
		tags, err := os.ReadDir(filepath.Join(r.imagesDir(), n.Name()))
		if err != nil {
			continue
		}
		for _, t := range tags {
			if !t.IsDir() {
				continue
			}
			ref, err := imageref.FromDir(n.Name(), t.Name())
			if err != nil || ref.Dir(r.Home) != filepath.Join(r.imagesDir(), n.Name(), t.Name()) {
				continue
			}
			if j, err := r.loadImage(ref); err == nil {
				out = append(out, j.image())
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// SetImageConfig replaces an image's default configuration.
func (r *Runtime) SetImageConfig(ctx context.Context, name string, cfg engine.ImageConfig) error {
	ref, err := imageref.Parse(name)
	if err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(r.imagesDir(), ".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	j, err := r.loadImage(ref)
	if err != nil {
		return err
	}
	j.Config = cfg
	return writeJSON(filepath.Join(ref.Dir(r.Home), "image.json"), j)
}

func (r *Runtime) ImportImage(ctx context.Context, name string, disk, kernel, initrd string, rootUUID string) (*engine.Image, error) {
	ref, err := imageref.Parse(name)
	if err != nil {
		return nil, err
	}
	if !uuidRE.MatchString(rootUUID) {
		return nil, fmt.Errorf("invalid root UUID %q", rootUUID)
	}
	files := []*string{&disk, &kernel, &initrd}
	for _, f := range files {
		resolved, err := filepath.EvalSymlinks(*f)
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(resolved)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", *f)
		}
		*f = resolved
	}
	return r.storeImage(ref, &imageJSON{RootUUID: rootUUID}, func(tmp string) error {
		for _, c := range [][2]string{{disk, "disk.img"}, {kernel, "kernel"}, {initrd, "initrd.cpio"}} {
			if err := cloneFile(c[0], filepath.Join(tmp, c[1])); err != nil {
				return err
			}
		}
		return nil
	})
}

// storeImage fills a temporary directory with fill, writes image.json and renames it into place,
// replacing an existing image of the same name only when no container uses it.
func (r *Runtime) storeImage(ref imageref.Ref, j *imageJSON, fill func(tmp string) error) (*engine.Image, error) {
	if err := os.MkdirAll(filepath.Dir(ref.Dir(r.Home)), 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(r.imagesDir(), ".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o700); err != nil {
		return nil, err
	}
	if err := fill(tmp); err != nil {
		return nil, err
	}
	st, err := os.Stat(filepath.Join(tmp, "disk.img"))
	if err != nil {
		return nil, err
	}
	j.Name, j.Tag = ref.Name, ref.Tag
	j.Created = time.Now().UTC()
	j.Size = st.Sys().(*syscall.Stat_t).Blocks * 512
	if err := writeJSON(filepath.Join(tmp, "image.json"), j); err != nil {
		return nil, err
	}

	unlock, err := lock(filepath.Join(r.imagesDir(), ".lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	dst := ref.Dir(r.Home)
	if _, err := os.Stat(dst); err == nil {
		if users := r.imageUsers(ref); len(users) > 0 {
			return nil, fmt.Errorf("image %s is in use by container %s", ref, users[0])
		}
		old := tmp + "-old"
		if err := os.Rename(dst, old); err != nil {
			return nil, err
		}
		defer os.RemoveAll(old)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return nil, err
	}
	return j.image(), nil
}

func (r *Runtime) imageUsers(ref imageref.Ref) []string {
	infos, _ := r.all()
	var users []string
	for _, info := range infos {
		if info.Image == ref.String() {
			users = append(users, info.Name)
		}
	}
	return users
}

func (r *Runtime) RemoveImage(ctx context.Context, name string) error {
	ref, err := imageref.Parse(name)
	if err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(r.imagesDir(), ".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := r.loadImage(ref); err != nil {
		return err
	}
	if users := r.imageUsers(ref); len(users) > 0 {
		return fmt.Errorf("image %s is in use by container %s: remove the container first", ref, users[0])
	}
	dir := ref.Dir(r.Home)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	os.Remove(filepath.Dir(dir))
	return nil
}

// cloneFile copies src to dst as an APFS clone when it can (cp -c), else byte by byte.
func cloneFile(src, dst string) error {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err == nil {
		return os.Chmod(dst, 0o600)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (r *Runtime) Commit(ctx context.Context, ref string, image string, cfg engine.ImageConfig) (*engine.Image, error) {
	target, err := imageref.Parse(image)
	if err != nil {
		return nil, err
	}
	info, err := r.Resolve(ref)
	if err != nil {
		return nil, err
	}
	unlock, err := r.lockContainer(info.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if info, err = r.load(info.ID); err != nil {
		return nil, err
	}
	if target.String() == info.Image {
		return nil, fmt.Errorf("cannot commit container %s onto its own image %s", info.Name, info.Image)
	}
	parent, err := imageref.Parse(info.Image)
	if err != nil {
		return nil, err
	}
	if info.Backend == backendVZ {
		return r.vzCommit(ctx, info, target, parent, cfg)
	}
	parentDir := parent.Dir(r.Home)

	running := info.State == engine.StateRunning
	if running {
		syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		r.ExecNotify(syncCtx, info.ID, engine.ProcessSpec{Args: []string{"/bin/sync"}}, engine.Stdio{}, nil)
		cancel()
		if err := r.monitor(info.ID, "stop"); err != nil {
			return nil, fmt.Errorf("pause: %w", err)
		}
		defer r.monitor(info.ID, "cont")
	}
	return r.storeImage(target, &imageJSON{RootUUID: info.RootUUID, Parent: parent.String(), Config: cfg}, func(tmp string) error {
		if r.QEMUErr != nil {
			return r.QEMUErr
		}
		out, err := exec.CommandContext(ctx, r.QEMUImg, "convert", "-U", "-O", "raw", "-S", "4k",
			r.file(info.ID, "disk.qcow2"), filepath.Join(tmp, "disk.img")).CombinedOutput()
		if err != nil {
			return fmt.Errorf("qemu-img convert: %v: %s", err, bytes.TrimSpace(out))
		}
		if err := os.Chmod(filepath.Join(tmp, "disk.img"), 0o600); err != nil {
			return err
		}
		for _, f := range []string{"kernel", "initrd.cpio"} {
			if err := cloneFile(filepath.Join(parentDir, f), filepath.Join(tmp, f)); err != nil {
				return err
			}
		}
		return nil
	})
}
