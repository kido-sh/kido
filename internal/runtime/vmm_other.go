//go:build !linux

package runtime

import (
	"context"
	"errors"
)

func linuxKVMAvailable() bool {
	return false
}

func (r *Runtime) kvmPrepareDisk(info *Info, backing string) (*Info, error) {
	return r.vzPrepareDisk(info, backing)
}

func (r *Runtime) kvmBoot(_ context.Context, _ *Info) error {
	return errors.New("the kvm backend requires Linux")
}
