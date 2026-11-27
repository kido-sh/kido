package main

import (
	"flag"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/mount"
)

const ctrMountNote = "a directory is mounted live (beta) on the vz backend when the image supports it, " +
	"unless --copy-mounts; anything else is a copy: copied in at every start; without ro, new and changed " +
	"files are copied back when the container stops (never deletions, never over a file that also changed " +
	"on the host; lost if the VM dies)"

// ctrMounts holds the -v/--volume, --mount and --copy-mounts flags of run and create.
type ctrMounts struct {
	vols, mounts ctrList
	copyDirs     bool
}

func ctrMountFlags(fs *flag.FlagSet) *ctrMounts {
	m := &ctrMounts{}
	fs.Var(&m.vols, "v", "mount a host path in the container: HOST:CONTAINER[:ro]; "+ctrMountNote)
	fs.Var(&m.vols, "volume", "same as -v")
	fs.Var(&m.mounts, "mount", "bind mount: type=bind,source=HOST,target=CONTAINER[,readonly] (same semantics as -v)")
	fs.BoolVar(&m.copyDirs, "copy-mounts", false, "copy directories in and back instead of mounting them live")
	return m
}

func (m *ctrMounts) parse() ([]engine.Mount, error) {
	var out []engine.Mount
	add := func(parse func(string) (engine.Mount, error), specs []string) error {
		for _, s := range specs {
			mt, err := parse(s)
			if err == nil {
				mt, err = mount.Resolve(mt)
			}
			if err != nil {
				return err
			}
			out = append(out, mt)
		}
		return nil
	}
	if err := add(mount.ParseVolume, m.vols); err != nil {
		return nil, err
	}
	if err := add(mount.ParseMount, m.mounts); err != nil {
		return nil, err
	}
	return out, mount.Check(out)
}
