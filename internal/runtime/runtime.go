// Package runtime implements engine.Engine with one QEMU micro-VM per container.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/mount"
	"github.com/kido-sh/kido/internal/setup"
)

var (
	nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
	idRE   = regexp.MustCompile(`^[0-9a-f]{12}$`)
	uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

var (
	ErrNotFound   = errors.New("no such container")
	ErrNotRunning = errors.New("container is not running")
)

// Runtime is the QEMU-backed engine.Engine. All state lives under Home.
type Runtime struct {
	Home    string
	QEMU    string
	QEMUImg string
	// QEMUErr is why setup.FindQEMU found no usable QEMU; QEMU work returns it.
	QEMUErr error
	// Backend ($KIDO_BACKEND, qemu or vz) runs the containers this Runtime creates.
	Backend string
}

var _ engine.Engine = (*Runtime)(nil)

// BuildLabel marks a build container; its value is the image being built.
const BuildLabel = "kido.build"

// DefaultHome is $KIDO_HOME, or ~/.kido.
func DefaultHome() string {
	if h := os.Getenv("KIDO_HOME"); h != "" {
		return h
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".kido")
	}
	return ".kido"
}

// New prepares the state directories under home (0700) and locates QEMU (setup.FindQEMU).
func New(home string) (*Runtime, error) {
	abs, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	for i, d := range []string{abs, filepath.Join(abs, "images"), filepath.Join(abs, "containers"), filepath.Join(abs, "networks")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
		if err := checkPrivateDir(d, i == 0); err != nil {
			return nil, err
		}
	}
	backend, err := BackendFromEnv()
	if err != nil {
		return nil, err
	}
	q, qerr := setup.FindQEMU(abs)
	return &Runtime{Home: abs, QEMU: q.Path, QEMUImg: q.Img, QEMUErr: qerr, Backend: backend}, nil
}

// Process is the container's main process, run by `kido run`.
type Process struct {
	Args []string `json:"args"`
	Env  []string `json:"env,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	User string   `json:"user,omitempty"`
	TTY  bool     `json:"tty,omitempty"`
}

// Info is container.json: everything kido knows about a container.
type Info struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	Image      string               `json:"image"`
	Created    time.Time            `json:"created"`
	Ports      []engine.PortMapping `json:"ports,omitempty"`
	Network    string               `json:"network,omitempty"`
	IP         string               `json:"ip,omitempty"`     // vz on a network: the container's address
	Subnet     string               `json:"subnet,omitempty"` // vz with --network none: its private subnet
	CPUs       int                  `json:"cpus"`
	MemoryMB   int                  `json:"memoryMB"`
	Labels     map[string]string    `json:"labels,omitempty"`
	RootUUID   string               `json:"rootUUID"`
	Main       *Process             `json:"main,omitempty"`
	Detached   bool                 `json:"detached,omitempty"`
	AutoRemove bool                 `json:"autoRemove,omitempty"`
	Restart    string               `json:"restart,omitempty"` // policy: always, unless-stopped or on-failure[:N]
	Restarts   int                  `json:"restartCount,omitempty"`
	UserStop   bool                 `json:"userStopped,omitempty"` // kido stop or kill: no restart until a kido start
	Backend    string               `json:"backend,omitempty"`
	AgentPort  int                  `json:"agentPort,omitempty"`
	PID        int                  `json:"pid,omitempty"`
	PIDStart   int64                `json:"pidStart,omitempty"`
	StartedAt  time.Time            `json:"startedAt,omitzero"`
	FinishedAt time.Time            `json:"finishedAt,omitzero"`
	ExitCode   *int                 `json:"exitCode,omitempty"`
	Stopping   bool                 `json:"stopping,omitempty"`
	Error      string               `json:"error,omitempty"`

	State engine.State `json:"state"`
}

func (i *Info) container() *engine.Container {
	return &engine.Container{
		ID: i.ID, Name: i.Name, Image: i.Image, State: i.State, Created: i.Created,
		Ports: i.Ports, CPUs: i.CPUs, MemoryMB: i.MemoryMB, Labels: i.Labels,
	}
}

func (r *Runtime) containersDir() string    { return filepath.Join(r.Home, "containers") }
func (r *Runtime) dir(id string) string     { return filepath.Join(r.containersDir(), id) }
func (r *Runtime) file(id, f string) string { return filepath.Join(r.dir(id), f) }

// ContainerDir is containers/<id>, where console.log and main.log live.
func (r *Runtime) ContainerDir(id string) string { return r.dir(id) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ValidName reports whether s is an acceptable container name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'))
}

func (r *Runtime) load(id string) (*Info, error) {
	b, err := os.ReadFile(r.file(id, "container.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, err
	}
	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, fmt.Errorf("container %s: %w", id, err)
	}
	if info.ID != id {
		return nil, fmt.Errorf("container %s: container.json names %q", id, info.ID)
	}
	if !ValidName(info.Name) {
		return nil, fmt.Errorf("container %s: container.json has an invalid name %q", id, info.Name)
	}
	info.State = r.state(&info)
	return &info, nil
}

func (r *Runtime) save(info *Info) error {
	return writeJSON(r.file(info.ID, "container.json"), info)
}

func (r *Runtime) state(info *Info) engine.State {
	if info.PID > 0 && processAlive(info.PID, info.PIDStart) {
		return engine.StateRunning
	}
	if info.StartedAt.IsZero() {
		return engine.StateCreated
	}
	return engine.StateStopped
}

func (r *Runtime) all() ([]*Info, error) {
	entries, err := os.ReadDir(r.containersDir())
	if err != nil {
		return nil, err
	}
	var out []*Info
	for _, e := range entries {
		if !e.IsDir() || !idRE.MatchString(e.Name()) {
			continue
		}
		info, err := r.load(e.Name())
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Resolve finds a container by full ID, name, or unique ID prefix.
func (r *Runtime) Resolve(ref string) (*Info, error) {
	if ref == "" {
		return nil, fmt.Errorf("%w: empty reference", ErrNotFound)
	}
	if idRE.MatchString(ref) {
		if info, err := r.load(ref); err == nil {
			return info, nil
		}
	}
	all, err := r.all()
	if err != nil {
		return nil, err
	}
	for _, info := range all {
		if info.Name == ref {
			return info, nil
		}
	}
	var match *Info
	for _, info := range all {
		if strings.HasPrefix(info.ID, ref) {
			if match != nil {
				return nil, fmt.Errorf("container reference %q is ambiguous", ref)
			}
			match = info
		}
	}
	if match == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	return match, nil
}

// lock takes an exclusive flock on path; the returned func releases it.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
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

func (r *Runtime) lockContainer(id string) (func(), error) {
	return lock(r.file(id, "lock"))
}

// Update applies fn to the container's state under its lock.
func (r *Runtime) Update(id string, fn func(*Info)) error {
	unlock, err := r.lockContainer(id)
	if err != nil {
		return err
	}
	defer unlock()
	info, err := r.load(id)
	if err != nil {
		return err
	}
	fn(info)
	return r.save(info)
}

// Get returns the full state of a container.
func (r *Runtime) Get(ref string) (*Info, error) { return r.Resolve(ref) }

// Mounts returns the mounts recorded for container id.
func (r *Runtime) Mounts(id string) ([]mount.Mounted, error) { return mount.List(r.dir(id)) }

func (r *Runtime) List(ctx context.Context, all bool) ([]*engine.Container, error) {
	infos, err := r.all()
	if err != nil {
		return nil, err
	}
	var out []*engine.Container
	for _, info := range infos {
		if all || info.State == engine.StateRunning {
			out = append(out, info.container())
		}
	}
	return out, nil
}

// ListInfo is List with the full state of each container.
func (r *Runtime) ListInfo(all bool) ([]*Info, error) {
	infos, err := r.all()
	if err != nil {
		return nil, err
	}
	var out []*Info
	for _, info := range infos {
		if all || info.State == engine.StateRunning {
			out = append(out, info)
		}
	}
	return out, nil
}

func (r *Runtime) Inspect(ctx context.Context, ref string) (*engine.Container, error) {
	info, err := r.Resolve(ref)
	if err != nil {
		return nil, err
	}
	return info.container(), nil
}

func (r *Runtime) Remove(ctx context.Context, ref string, force bool) error {
	info, err := r.Resolve(ref)
	if err != nil {
		return err
	}
	unlock, err := r.lockContainer(info.ID)
	if err != nil {
		return err
	}
	defer unlock()
	if info, err = r.load(info.ID); err != nil {
		return err
	}
	if info.State == engine.StateRunning {
		if !force {
			return fmt.Errorf("container %s is running: stop it first or use -f", info.Name)
		}
		mount.Collect(ctx, r.dir(info.ID), r.mountGuest(info.ID), os.Stderr)
		if err := killQEMU(info); err != nil {
			return err
		}
		mount.Apply(r.dir(info.ID), os.Stderr)
	}
	dir := r.dir(info.ID)
	if filepath.Dir(dir) != r.containersDir() || !idRE.MatchString(info.ID) {
		return fmt.Errorf("refusing to remove %s", dir)
	}
	// A supervisor that saw its VM die may still lock or save into dir; after the rename its
	// paths fail instead of recreating files inside a directory that is being removed.
	tomb := filepath.Join(r.containersDir(), ".rm-"+info.ID)
	if err := os.RemoveAll(tomb); err != nil {
		return err
	}
	if err := os.Rename(dir, tomb); err != nil {
		return err
	}
	return os.RemoveAll(tomb)
}
