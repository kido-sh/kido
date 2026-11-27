package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
)

// CreateStopped makes a container, its disk and state, without booting it.
func (r *Runtime) CreateStopped(ctx context.Context, opts engine.CreateOptions) (*engine.Container, error) {
	info, err := r.prepare(ctx, opts)
	if err != nil {
		return nil, err
	}
	return info.container(), nil
}

// Rename gives a container a new unique name.
func (r *Runtime) Rename(ref, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid container name %q: must match %s", name, nameRE)
	}
	unlock, err := lock(filepath.Join(r.containersDir(), ".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	info, err := r.Resolve(ref)
	if err != nil {
		return err
	}
	if info.Name == name {
		return fmt.Errorf("container %s already has the name %s", info.ID, name)
	}
	all, err := r.all()
	if err != nil {
		return err
	}
	for _, c := range all {
		if c.ID != info.ID && (c.Name == name || c.ID == name) {
			return fmt.Errorf("container name %q is already in use by container %s", name, c.ID)
		}
	}
	return r.Update(info.ID, func(i *Info) { i.Name = name })
}

// Owner is the host process that runs a container's main process (kido run in the foreground, or
// the kido __attach helper of a detached container) and the guest pid of that process.
type Owner struct {
	PID      int   `json:"pid"`
	Start    int64 `json:"start"`
	GuestPID int   `json:"guestPid,omitempty"`
	VMPID    int   `json:"vmPid,omitempty"`
}

// SetOwner records this process as the owner of container id's main process, guestPID inside the
// VM that runs now.
func (r *Runtime) SetOwner(id string, guestPID int) error {
	info, err := r.load(id)
	if err != nil {
		return err
	}
	o := Owner{PID: os.Getpid(), Start: hostProcStart(os.Getpid()), GuestPID: guestPID, VMPID: info.PID}
	return writeJSON(r.file(id, "owner.json"), o)
}

func (r *Runtime) owner(id string) *Owner {
	b, err := os.ReadFile(r.file(id, "owner.json"))
	if err != nil {
		return nil
	}
	var o Owner
	if json.Unmarshal(b, &o) != nil || o.PID <= 0 {
		return nil
	}
	return &o
}

// OwnerAlive reports whether the recorded owner of container id still runs.
func (r *Runtime) OwnerAlive(id string) bool {
	o := r.owner(id)
	return o != nil && o.PID != os.Getpid() && hostProcAlive(o.PID, o.Start)
}

// WaitOwner waits up to timeout for the owner of container id to exit, so that a new boot is not
// stopped by the owner of the previous one as it winds down.
func (r *Runtime) WaitOwner(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for r.OwnerAlive(id) {
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s: the process that ran its previous main process is still exiting", id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}

// Signal sends sig to the main process of a running container, inside the guest.
func (r *Runtime) Signal(ctx context.Context, ref string, sig syscall.Signal) error {
	info, err := r.Resolve(ref)
	if err != nil {
		return err
	}
	if info.State != engine.StateRunning {
		return fmt.Errorf("container %s is not running", info.Name)
	}
	o := r.owner(info.ID)
	if o == nil || o.GuestPID <= 1 || o.VMPID != info.PID || !hostProcAlive(o.PID, o.Start) {
		return fmt.Errorf("container %s has no main process to signal (use kido stop, or kido kill without -s)", info.Name)
	}
	var out bytes.Buffer
	p := engine.ProcessSpec{Args: []string{"/bin/kill", "-" + strconv.Itoa(int(sig)), strconv.Itoa(o.GuestPID)}}
	status, err := r.Exec(ctx, info.ID, p, engine.Stdio{Stdout: &out, Stderr: &out})
	if err != nil {
		// The signal can end the main process, and with it the VM, before kill reports back.
		if now, lerr := r.load(info.ID); lerr != nil || now.Stopping || now.PID != info.PID || now.ExitCode != nil {
			return nil
		}
		return fmt.Errorf("signal %d to container %s: %w", sig, info.Name, err)
	}
	if status != 0 {
		return fmt.Errorf("signal %d to container %s: kill exited %d: %s", sig, info.Name, status, strings.TrimSpace(out.String()))
	}
	return nil
}

// Wait blocks until the container has stopped (or, with --rm, its main process has exited, as the
// container is about to go away) and returns the main process's exit status (0 when there was none).
func (r *Runtime) Wait(ctx context.Context, ref string) (int, error) {
	info, err := r.Resolve(ref)
	if err != nil {
		return 125, err
	}
	id, name := info.ID, info.Name
	var settled time.Time
	for {
		info, err := r.load(id)
		if errors.Is(err, ErrNotFound) {
			if status, ok := r.removedExit(id); ok {
				return status, nil
			}
			return 125, fmt.Errorf("container %s was removed before its exit status was read", name)
		}
		if err != nil {
			return 125, err
		}
		if info.ExitCode != nil && (info.State != engine.StateRunning || info.AutoRemove) {
			return *info.ExitCode, nil
		}
		if info.State != engine.StateRunning {
			// The owner records the exit status as the VM goes down; give it a moment.
			if !r.OwnerAlive(id) {
				if settled.IsZero() {
					settled = time.Now()
				} else if time.Since(settled) > 200*time.Millisecond {
					if info.ExitCode != nil {
						return *info.ExitCode, nil
					}
					return 0, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return 125, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// RecordExit stores the exit status of the main process that ran in the VM vmPID, and its error if
// any; it leaves the state alone when a newer boot runs. The status of a --rm container is also
// kept outside its directory, for a kido wait that reads it after the removal.
func (r *Runtime) RecordExit(id string, vmPID, status int, execErr error) error {
	autoRemove := false
	err := r.Update(id, func(i *Info) {
		if i.PID != 0 && i.PID != vmPID {
			return
		}
		i.ExitCode = &status
		if execErr != nil {
			i.Error = execErr.Error()
		}
		autoRemove = i.AutoRemove
	})
	if err == nil && autoRemove {
		return r.recordRemovedExit(id, status)
	}
	return err
}

// StoppedByOthers reports whether the container's boot that runs in VM vmPID ended (or is ending)
// through someone else: kido stop, kill, rm -f, or the container being gone. A VM that is killed
// closes its sockets before it is reaped, so it looks alive for a moment: settle is how long to
// keep looking.
func (r *Runtime) StoppedByOthers(id string, vmPID int, settle time.Duration) bool {
	deadline := time.Now().Add(settle)
	for {
		info, err := r.load(id)
		if err != nil || info.Stopping || info.PID != vmPID || info.State != engine.StateRunning {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// VMPID is the host pid of the running VM of the container, 0 when it is not running.
func (r *Runtime) VMPID(id string) int {
	info, err := r.load(id)
	if err != nil || info.State != engine.StateRunning {
		return 0
	}
	return info.PID
}

// Pruned is what a prune removed.
type Pruned struct {
	Names []string
	Bytes int64
}

// PruneContainers removes every container that is not running.
func (r *Runtime) PruneContainers(ctx context.Context) (Pruned, error) {
	var p Pruned
	infos, err := r.all()
	if err != nil {
		return p, err
	}
	var errs []error
	for _, info := range infos {
		if info.State == engine.StateRunning || r.OwnerAlive(info.ID) {
			continue
		}
		n := diskUsage(r.dir(info.ID))
		if err := r.Remove(ctx, info.ID, false); err != nil {
			errs = append(errs, err)
			continue
		}
		p.Names = append(p.Names, info.ID)
		p.Bytes += n
	}
	return p, errors.Join(errs...)
}

// PruneImages removes every image that no container uses.
func (r *Runtime) PruneImages(ctx context.Context) (Pruned, error) {
	var p Pruned
	imgs, err := r.Images(ctx)
	if err != nil {
		return p, err
	}
	var errs []error
	for _, img := range imgs {
		ref := imageref.Ref{Name: img.Name, Tag: img.Tag}
		if len(r.imageUsers(ref)) > 0 {
			continue
		}
		n := diskUsage(ref.Dir(r.Home))
		if err := r.RemoveImage(ctx, ref.String()); err != nil {
			errs = append(errs, err)
			continue
		}
		p.Names = append(p.Names, ref.String())
		p.Bytes += n
	}
	return p, errors.Join(errs...)
}

// PruneTemp removes what interrupted image operations left behind: images/.tmp-* directories and
// entries of tmp/ in which nothing changed for an hour, so a long save, load or pull keeps its files.
func (r *Runtime) PruneTemp() (Pruned, error) {
	var p Pruned
	var cands []string
	if entries, err := os.ReadDir(r.imagesDir()); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), ".tmp-") {
				cands = append(cands, filepath.Join(r.imagesDir(), e.Name()))
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(r.Home, "tmp")); err == nil {
		for _, e := range entries {
			cands = append(cands, filepath.Join(r.Home, "tmp", e.Name()))
		}
	}
	unlock, err := lock(filepath.Join(r.imagesDir(), ".lock"))
	if err != nil {
		return p, err
	}
	defer unlock()
	var errs []error
	for _, c := range cands {
		if time.Since(newestModTime(c)) < time.Hour {
			continue
		}
		n := diskUsage(c)
		if err := os.RemoveAll(c); err != nil {
			errs = append(errs, err)
			continue
		}
		p.Names = append(p.Names, c)
		p.Bytes += n
	}
	return p, errors.Join(errs...)
}

// newestModTime is the latest modification time of root or anything under it.
func newestModTime(root string) time.Time {
	var t time.Time
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.ModTime().After(t) {
			t = fi.ModTime()
		}
		return nil
	})
	return t
}

// TagImage makes target another name for the image source: an APFS clone of its files with the
// same creation time and configuration.
func (r *Runtime) TagImage(ctx context.Context, source, target string) (*engine.Image, error) {
	src, err := imageref.Parse(source)
	if err != nil {
		return nil, err
	}
	dst, err := imageref.Parse(target)
	if err != nil {
		return nil, err
	}
	j, err := r.loadImage(src)
	if err != nil {
		return nil, err
	}
	if src == dst {
		return j.image(), nil
	}
	srcDir := src.Dir(r.Home)
	img, err := r.storeImage(dst, &imageJSON{RootUUID: j.RootUUID, Parent: j.Parent, Config: j.Config}, func(tmp string) error {
		for _, f := range []string{"disk.img", "kernel", "initrd.cpio"} {
			if err := cloneFile(filepath.Join(srcDir, f), filepath.Join(tmp, f)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := r.lcSetImageCreated(dst, j.Created); err != nil {
		return nil, err
	}
	img.Created = j.Created
	return img, nil
}

func (r *Runtime) lcSetImageCreated(ref imageref.Ref, t time.Time) error {
	unlock, err := lock(filepath.Join(r.imagesDir(), ".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	j, err := r.loadImage(ref)
	if err != nil {
		return err
	}
	j.Created = t
	return writeJSON(filepath.Join(ref.Dir(r.Home), "image.json"), j)
}

// ImageUsers returns the names of the containers whose disk is an overlay on the image.
func (r *Runtime) ImageUsers(name string) ([]string, error) {
	ref, err := imageref.Parse(name)
	if err != nil {
		return nil, err
	}
	return r.imageUsers(ref), nil
}

// RemoveImageForce removes an image together with the stopped containers made from it, and
// refuses while one of them runs. It returns the removed containers.
func (r *Runtime) RemoveImageForce(ctx context.Context, name string) ([]string, error) {
	ref, err := imageref.Parse(name)
	if err != nil {
		return nil, err
	}
	if _, err := r.loadImage(ref); err != nil {
		return nil, err
	}
	infos, err := r.all()
	if err != nil {
		return nil, err
	}
	var users []*Info
	for _, info := range infos {
		if info.Image != ref.String() {
			continue
		}
		if info.State == engine.StateRunning {
			return nil, fmt.Errorf("image %s is used by running container %s: stop or remove it first", ref, info.Name)
		}
		users = append(users, info)
	}
	var removed []string
	for _, info := range users {
		if err := r.Remove(ctx, info.ID, false); err != nil {
			return removed, err
		}
		removed = append(removed, info.Name)
	}
	return removed, r.RemoveImage(ctx, ref.String())
}

// Usage is the disk use of the store, in allocated bytes (an APFS clone counts in full).
type Usage struct {
	Images, Containers, Running int
	ImageBytes, ContainerBytes  int64
	FreeBytes                   int64
}

func (r *Runtime) DiskUsage(ctx context.Context) (Usage, error) {
	var u Usage
	imgs, err := r.Images(ctx)
	if err != nil {
		return u, err
	}
	u.Images = len(imgs)
	u.ImageBytes = diskUsage(r.imagesDir())
	infos, err := r.all()
	if err != nil {
		return u, err
	}
	u.Containers = len(infos)
	for _, i := range infos {
		if i.State == engine.StateRunning {
			u.Running++
		}
	}
	u.ContainerBytes = diskUsage(r.containersDir())
	var st unix.Statfs_t
	if unix.Statfs(r.Home, &st) == nil {
		u.FreeBytes = int64(st.Bavail) * int64(st.Bsize)
	}
	return u, nil
}

// ContainerDiskUsage is the allocated size of one container's directory (its overlay and logs).
func (r *Runtime) ContainerDiskUsage(id string) int64 { return diskUsage(r.dir(id)) }

func diskUsage(root string) int64 {
	var n int64
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				n += st.Blocks * 512
			}
		}
		return nil
	})
	return n
}

// BackendVersion is the version line of the VM monitor that runs containers.
func (r *Runtime) BackendVersion(ctx context.Context) (string, error) {
	if r.Backend == backendKVM {
		vmm := VMMPath()
		if vmm == "" {
			return "", nil
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, vmm, "--version").Output()
		if err != nil {
			return "", err
		}
		line, _, _ := strings.Cut(string(out), "\n")
		return strings.TrimSpace(line), nil
	}
	if r.Backend != "" && r.Backend != backendQEMU {
		return "", nil
	}
	if r.QEMUErr != nil {
		return "", r.QEMUErr
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.QEMU, "--version").Output()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line), nil
}
