package build

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
)

var stageNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type buildPlan struct {
	globalArgs []*step
	stages     []*stage
	byName     map[string]int
}

type stage struct {
	index int
	name  string
	from  *step
	steps []*step
}

type stageRefKind int

const (
	refImage stageRefKind = iota
	refStage
)

type stageRef struct {
	kind       stageRefKind
	stageIndex int
	image      imageref.Ref
	raw        string
}

type copyKey struct {
	stageIdx int
	stepIdx  int
}

type copyTask struct {
	key     copyKey
	from    stageRef
	srcs    []string
	prepErr error
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func cleanStageSource(src string) (rel string, trailingSlash bool, err error) {
	if src == "" || strings.ContainsRune(src, 0) {
		return "", false, fmt.Errorf("invalid source path %q", src)
	}
	for _, el := range strings.Split(src, "/") {
		if el == ".." {
			return "", false, fmt.Errorf("%s: \"..\" is not allowed in a source path", src)
		}
	}
	trailingSlash = strings.HasSuffix(src, "/") || strings.HasSuffix(src, "/.") || src == "."
	trimmed := strings.TrimLeft(src, "/")
	if trimmed == "" {
		return ".", true, nil
	}
	return path.Clean(trimmed), trailingSlash, nil
}

func resolveFromRef(p *buildPlan, curIdx int, raw string) (stageRef, error) {
	if raw == "" {
		return stageRef{}, errors.New("empty base image")
	}
	if base, _, _ := strings.Cut(raw, ":"); base == "scratch" {
		return stageRef{}, errors.New("FROM scratch is not supported: a kido image needs a Darwin userland, start from a local image such as darwin")
	}
	if idx, ok := p.byName[raw]; ok && idx < curIdx {
		return stageRef{kind: refStage, stageIndex: idx, raw: raw}, nil
	}
	ref, err := imageref.Parse(raw)
	if err != nil {
		return stageRef{}, err
	}
	return stageRef{kind: refImage, image: ref, raw: raw}, nil
}

func resolveCopyFromRef(p *buildPlan, curIdx int, raw string) (stageRef, error) {
	if raw == "" {
		return stageRef{}, errors.New("flag --from needs a value")
	}
	if base, _, _ := strings.Cut(raw, ":"); base == "scratch" {
		return stageRef{}, errors.New("COPY --from=scratch is not supported")
	}
	if idx, ok := p.byName[raw]; ok {
		switch {
		case idx < curIdx:
			return stageRef{kind: refStage, stageIndex: idx, raw: raw}, nil
		case idx == curIdx:
			return stageRef{}, fmt.Errorf("stage %q cannot copy from itself", raw)
		default:
			return stageRef{}, fmt.Errorf("cannot copy from stage %q: only earlier stages can be referenced", raw)
		}
	}
	if isAllDigits(raw) {
		idx, err := strconv.Atoi(raw)
		if err != nil || raw != strconv.Itoa(idx) || idx < 0 || idx >= len(p.stages) {
			return stageRef{}, fmt.Errorf("invalid stage index %s", raw)
		}
		if idx == curIdx {
			return stageRef{}, fmt.Errorf("stage %d cannot copy from itself", idx)
		}
		if idx > curIdx {
			return stageRef{}, fmt.Errorf("cannot copy from stage %d: only earlier stages can be referenced", idx)
		}
		return stageRef{kind: refStage, stageIndex: idx, raw: raw}, nil
	}
	ref, err := imageref.Parse(raw)
	if err != nil {
		return stageRef{}, err
	}
	return stageRef{kind: refImage, image: ref, raw: raw}, nil
}

func (b *builder) resolvePlan(ctx context.Context) error {
	for _, s := range b.plan.globalArgs {
		if err := b.arg(s); err != nil {
			return fmt.Errorf("line %d: ARG: %w", s.Line, err)
		}
	}

	b.targetStageIdx = len(b.plan.stages) - 1
	if b.opts.Target != "" {
		idx, ok := b.plan.byName[b.opts.Target]
		if !ok {
			return fmt.Errorf("target stage %q not found", b.opts.Target)
		}
		b.targetStageIdx = idx
	}

	b.fromRefs = map[int]stageRef{}
	b.copyFromRefs = map[copyKey]stageRef{}
	b.needCommit = map[int]bool{}
	visited := map[int]bool{}

	var visit func(idx int) error
	visit = func(idx int) error {
		if visited[idx] {
			return nil
		}
		visited[idx] = true
		st := b.plan.stages[idx]

		if p, ok := st.from.flags["platform"]; ok {
			v, err := expandWord(p, b.globalLookup)
			if err != nil {
				return fmt.Errorf("line %d: FROM: %w", st.from.Line, err)
			}
			if !strings.EqualFold(v, "darwin/arm64") {
				return fmt.Errorf("line %d: FROM: --platform=%s: kido builds darwin/arm64 images only", st.from.Line, v)
			}
		}

		fromName, err := expandWord(st.from.words[0], b.globalLookup)
		if err != nil {
			return fmt.Errorf("line %d: FROM: %w", st.from.Line, err)
		}
		fref, err := resolveFromRef(b.plan, idx, fromName)
		if err != nil {
			return fmt.Errorf("line %d: FROM: %w", st.from.Line, err)
		}
		b.fromRefs[idx] = fref
		if fref.kind == refStage {
			b.needCommit[fref.stageIndex] = true
			if err := visit(fref.stageIndex); err != nil {
				return err
			}
		}

		for j, s := range st.steps {
			rawFrom, ok := s.flags["from"]
			if !ok {
				continue
			}
			expanded, err := expandWord(rawFrom, b.globalLookup)
			if err != nil {
				return fmt.Errorf("line %d: COPY: %w", s.Line, err)
			}
			cref, err := resolveCopyFromRef(b.plan, idx, expanded)
			if err != nil {
				return fmt.Errorf("line %d: COPY: %w", s.Line, err)
			}
			b.copyFromRefs[copyKey{idx, j}] = cref
			if cref.kind == refStage {
				if err := visit(cref.stageIndex); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := visit(b.targetStageIdx); err != nil {
		return err
	}

	for _, st := range b.plan.stages {
		if visited[st.index] {
			b.selected = append(b.selected, st)
		}
	}

	finalFrom := b.fromRefs[b.targetStageIdx]
	if finalFrom.kind == refImage && finalFrom.image == b.target {
		return fmt.Errorf("line %d: FROM: the image being built, %s, is also its base image: choose another -t",
			b.plan.stages[b.targetStageIdx].from.Line, finalFrom.image)
	}

	b.baseImages = map[imageref.Ref]*engine.Image{}
	for _, st := range b.selected {
		if fref := b.fromRefs[st.index]; fref.kind == refImage {
			img, err := b.ensureImage(ctx, fref.image)
			if err != nil {
				return fmt.Errorf("line %d: FROM: %w", st.from.Line, err)
			}
			if u := img.Config.User; u != "" && !ownerRE.MatchString(u) {
				return fmt.Errorf("line %d: FROM: image %s has an invalid user %q", st.from.Line, fref.image, u)
			}
			if wd := img.Config.WorkingDir; strings.ContainsRune(wd, 0) {
				return fmt.Errorf("line %d: FROM: image %s has an invalid working directory %q", st.from.Line, fref.image, wd)
			}
		}
		for j, s := range st.steps {
			cref, ok := b.copyFromRefs[copyKey{st.index, j}]
			if !ok || cref.kind != refImage {
				continue
			}
			if _, err := b.ensureImage(ctx, cref.image); err != nil {
				return fmt.Errorf("line %d: COPY: %w", s.Line, err)
			}
		}
	}

	b.planCopyTasks()
	return nil
}

func (b *builder) ensureImage(ctx context.Context, ref imageref.Ref) (*engine.Image, error) {
	if img, ok := b.baseImages[ref]; ok {
		return img, nil
	}
	base, err := b.localImage(ctx, ref)
	if err != nil {
		return nil, err
	}
	if base == nil && b.opts.Pull != nil {
		if err := b.opts.Pull(ctx, ref.String()); err != nil {
			return nil, err
		}
		if base, err = b.localImage(ctx, ref); err != nil {
			return nil, err
		}
	}
	if base == nil {
		return nil, fmt.Errorf("image %s not found", ref)
	}
	b.baseImages[ref] = base
	return base, nil
}

func (b *builder) planCopyTasks() {
	b.stageCopies = map[int][]copyTask{}
	b.imageCopies = map[imageref.Ref][]copyTask{}
	simEnv := map[int][]string{}

	for _, st := range b.selected {
		fref := b.fromRefs[st.index]
		var env []string
		if fref.kind == refStage {
			env = slices.Clone(simEnv[fref.stageIndex])
		} else {
			env = slices.Clone(b.baseImages[fref.image].Config.Env)
		}
		if _, ok := envLookup(env, "PATH"); !ok {
			env = append(env, "PATH="+defaultPath)
		}
		args := map[string]*string{}
		lookup := func(name string) (string, bool) {
			if v, ok := envLookup(env, name); ok {
				return v, true
			}
			if v := args[name]; v != nil {
				return *v, true
			}
			return "", false
		}
		broken := false

		for j, s := range st.steps {
			if broken {
				break
			}
			switch s.Cmd {
			case "ARG":
				for _, p := range s.pairs {
					var val *string
					if v, ok := b.opts.BuildArgs[p.key]; ok {
						v := v
						val = &v
					} else if p.hasValue {
						v, err := expandWord(p.value, lookup)
						if err != nil {
							broken = true
							break
						}
						val = &v
					} else {
						val = b.global[p.key]
					}
					args[p.key] = val
				}
			case "ENV":
				type pair struct{ k, v string }
				pairs := make([]pair, 0, len(s.pairs))
				for _, p := range s.pairs {
					k, err := expandWord(p.key, lookup)
					if err != nil {
						broken = true
						break
					}
					v, err := expandWord(p.value, lookup)
					if err != nil {
						broken = true
						break
					}
					if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
						broken = true
						break
					}
					pairs = append(pairs, pair{k, v})
				}
				if !broken {
					for _, p := range pairs {
						env = envSet(env, p.k, p.v)
					}
				}
			case "COPY":
				key := copyKey{st.index, j}
				cref, ok := b.copyFromRefs[key]
				if !ok {
					continue
				}
				task := copyTask{key: key, from: cref}
				srcs := make([]string, len(s.words)-1)
				for i, w := range s.words[:len(s.words)-1] {
					v, err := expandWord(w, lookup)
					if err != nil {
						task.prepErr = err
						break
					}
					if _, _, err := cleanStageSource(v); err != nil {
						task.prepErr = err
						break
					}
					srcs[i] = v
				}
				task.srcs = srcs
				if cref.kind == refStage {
					b.stageCopies[cref.stageIndex] = append(b.stageCopies[cref.stageIndex], task)
				} else {
					b.imageCopies[cref.image] = append(b.imageCopies[cref.image], task)
				}
			}
		}
		simEnv[st.index] = env
	}
}
