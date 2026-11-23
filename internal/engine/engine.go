// Package engine is the contract between the CLI commands (run, exec, build, ...) and the code that
// manages containers. internal/runtime implements it; internal/build only consumes it.
package engine

import (
	"context"
	"io"
	"time"
)

type PortMapping struct {
	HostIP    string // default 127.0.0.1; never 0.0.0.0 unless the user asks for it explicitly
	HostPort  int
	GuestPort int
	Protocol  string // "tcp" or "udp"
}

// Mount copies a host path into the container at every boot and, unless ReadOnly, copies the
// container's changes back when it stops (see internal/mount).
type Mount struct {
	Source   string `json:"source"` // absolute host path
	Target   string `json:"target"` // absolute guest path
	ReadOnly bool   `json:"readOnly,omitempty"`
}

type CreateOptions struct {
	Name     string // optional; generated when empty
	Image    string // local image name, e.g. "darwin" or "darwin:latest"
	Ports    []PortMapping
	CPUs     int // default 2
	MemoryMB int // default 1024
	Labels   map[string]string
	Mounts   []Mount
	Network  string // "": user-mode NAT; "none": the guest reaches neither the host nor the outside
}

type ProcessSpec struct {
	Args []string
	Env  []string // KEY=VALUE, added to the guest's default environment
	Dir  string   // working directory, default /
	User string   // empty: root
	TTY  bool
}

type Size struct{ Rows, Cols uint16 }

type Stdio struct {
	Stdin  io.Reader // nil: no stdin (the process sees EOF at once)
	Stdout io.Writer
	Stderr io.Writer   // ignored when TTY
	Resize <-chan Size // optional, TTY only
	Signal <-chan int  // optional: signal numbers to forward
}

type State string

const (
	StateCreated State = "created"
	StateRunning State = "running"
	StateStopped State = "stopped"
)

type Container struct {
	ID       string
	Name     string
	Image    string
	State    State
	Created  time.Time
	Ports    []PortMapping
	CPUs     int
	MemoryMB int
	Labels   map[string]string
}

type ImageConfig struct {
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        []string          `json:"env,omitempty"`
	WorkingDir string            `json:"workingDir,omitempty"`
	User       string            `json:"user,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

type Image struct {
	Name     string // repository, e.g. "darwin" or "ghcr.io/x/darwin"
	Tag      string // "latest" when not given
	Created  time.Time
	Size     int64 // bytes the disk image takes on disk
	RootUUID string
	Parent   string
	Config   ImageConfig
}

type Engine interface {
	// Create makes the container's disk and starts its VM, returning once the guest agent answers.
	Create(ctx context.Context, opts CreateOptions) (*Container, error)
	Exec(ctx context.Context, ref string, p ProcessSpec, stdio Stdio) (exitStatus int, err error)
	// CopyIn unpacks a tar stream into dstDir in the container.
	CopyIn(ctx context.Context, ref string, tarStream io.Reader, dstDir string) error
	// CopyOut writes a tar stream of srcPath in the container.
	CopyOut(ctx context.Context, ref string, srcPath string, w io.Writer) error
	Stop(ctx context.Context, ref string, timeout time.Duration) error
	Start(ctx context.Context, ref string) error
	Remove(ctx context.Context, ref string, force bool) error
	// Commit snapshots the container's disk into a new local image (pausing a running VM while it
	// copies) and records cfg as the image's default configuration.
	Commit(ctx context.Context, ref string, image string, cfg ImageConfig) (*Image, error)
	List(ctx context.Context, all bool) ([]*Container, error)
	Inspect(ctx context.Context, ref string) (*Container, error)
	Images(ctx context.Context) ([]*Image, error)
	ImportImage(ctx context.Context, name string, disk, kernel, initrd string, rootUUID string) (*Image, error)
	RemoveImage(ctx context.Context, name string) error
}
