package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/kido-sh/kido/internal/mount"
	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("inspect", "Return low-level information on containers or images (JSON)", ctrInspect)
}

// ctrInspected is a container as inspect shows it: its state and its mounts.
type ctrInspected struct {
	*runtime.Info
	Mounts []mount.Mounted `json:"mounts,omitempty"`
}

func ctrInspect(ctx context.Context, args []string) int {
	fs := ctrFlags("inspect", "[OPTIONS] NAME|ID [NAME|ID...]")
	typ := fs.String("type", "", "only look for a container or an image")
	format := fs.String("f", "", "format the output using a Go template, or 'json'")
	fs.StringVar(format, "format", "", "format the output using a Go template, or 'json'")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 125
	}
	if *typ != "" && *typ != "container" && *typ != "image" {
		return ctrFail(fmt.Errorf("--type %q: want container or image", *typ))
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	out := []any{}
	code := 0
	for _, ref := range fs.Args() {
		if *typ != "image" {
			if info, err := rt.Get(ref); err == nil {
				ms, err := rt.Mounts(info.ID)
				if err != nil {
					code = ctrFail(err)
					continue
				}
				out = append(out, ctrInspected{info, ms})
				continue
			} else if *typ == "container" {
				code = ctrFail(err)
				continue
			}
		}
		img, err := rt.Image(ctx, ref)
		if err != nil {
			code = ctrFail(fmt.Errorf("no such object: %s", ref))
			continue
		}
		out = append(out, img)
	}
	if *format != "" {
		f, err := ctrNewFormatter(os.Stdout, *format, nil)
		if err != nil {
			return ctrFail(err)
		}
		for _, o := range out {
			if err := f.Row(o); err != nil {
				return ctrFail(err)
			}
		}
		return code
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "    ")
	enc.Encode(out)
	return code
}
