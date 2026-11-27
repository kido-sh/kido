package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/kido-sh/kido/internal/engine"
)

func init() {
	register("commit", "Create a new image from a container's disk", ctrCommit)
}

// ctrInstrArgs parses a Kidofile-style exec form (JSON array) or shell form.
func ctrInstrArgs(s string) []string {
	var arr []string
	if strings.HasPrefix(strings.TrimSpace(s), "[") && json.Unmarshal([]byte(s), &arr) == nil {
		return arr
	}
	return []string{"/bin/sh", "-c", s}
}

// ctrChange applies one --change instruction (CMD, ENTRYPOINT, ENV, WORKDIR, USER, LABEL) to cfg.
func ctrChange(cfg *engine.ImageConfig, change string) error {
	instr, rest, _ := strings.Cut(strings.TrimSpace(change), " ")
	rest = strings.TrimSpace(rest)
	switch strings.ToUpper(instr) {
	case "CMD":
		cfg.Cmd = ctrInstrArgs(rest)
	case "ENTRYPOINT":
		cfg.Entrypoint = ctrInstrArgs(rest)
	case "ENV":
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			k, v, _ = strings.Cut(rest, " ")
		}
		if k == "" {
			return fmt.Errorf("--change %q: ENV needs KEY=VALUE", change)
		}
		cfg.Env = slices.DeleteFunc(slices.Clone(cfg.Env), func(e string) bool { return strings.HasPrefix(e, k+"=") })
		cfg.Env = append(cfg.Env, k+"="+strings.TrimSpace(v))
	case "WORKDIR":
		cfg.WorkingDir = rest
	case "USER":
		cfg.User = rest
	case "LABEL":
		k, v, _ := strings.Cut(rest, "=")
		labels := maps.Clone(cfg.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		labels[k] = strings.Trim(v, `"`)
		cfg.Labels = labels
	default:
		return fmt.Errorf("--change %q: supported instructions are CMD, ENTRYPOINT, ENV, WORKDIR, USER, LABEL", change)
	}
	return nil
}

func ctrCommit(ctx context.Context, args []string) int {
	fs := ctrFlags("commit", "[-c INSTRUCTION] CONTAINER NAME[:TAG]")
	var changes ctrList
	fs.Var(&changes, "c", "apply a Kidofile instruction to the image config (CMD, ENTRYPOINT, ENV, WORKDIR, USER, LABEL)")
	fs.Var(&changes, "change", "same as -c")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 125
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	info, err := rt.Get(fs.Arg(0))
	if err != nil {
		return ctrFail(err)
	}
	var cfg engine.ImageConfig
	if parent, err := rt.Image(ctx, info.Image); err == nil {
		cfg = parent.Config
	}
	for _, c := range changes {
		if err := ctrChange(&cfg, c); err != nil {
			return ctrFail(err)
		}
	}
	img, err := rt.Commit(ctx, info.ID, fs.Arg(1), cfg)
	if err != nil {
		return ctrFail(err)
	}
	fmt.Println(img.Name + ":" + img.Tag)
	return 0
}
