package build

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
	"github.com/kido-sh/kido/internal/runtime"
)

// Options configures one build.
type Options struct {
	ContextDir string
	Kidofile   []byte // nil: the Kidofile at the root of the context
	Tag        string // NAME[:TAG] of the result; build-<random>:latest when empty
	Target     string // optional stage name to stop at
	BuildArgs  map[string]string
	NoRm       bool // keep the build container
	CPUs       int
	MemoryMB   int
	Stdout     io.Writer
	Stderr     io.Writer
	Pull       func(ctx context.Context, name string) error // nil: FROM uses local images only
}

// StepError is a RUN step that exited with a non-zero status.
type StepError struct {
	Step, Steps int
	Line        int
	Instruction string
	ExitStatus  int
	Msg         string // why the guest could not run the program, if it could not
}

func (e *StepError) Error() string {
	s := fmt.Sprintf("step %d/%d (line %d) %s: returned a non-zero code: %d", e.Step, e.Steps, e.Line, e.Instruction, e.ExitStatus)
	if e.Msg != "" {
		s += " (" + e.Msg + ")"
	}
	return s
}

// ExposeLabel holds the EXPOSE ports, space-separated, e.g. "80/tcp 53/udp".
const ExposeLabel = "kido.expose"

// BuildLabel marks a build container; its value is the image being built.
const BuildLabel = runtime.BuildLabel

// defaultPath is the guest agent's default PATH. Like BuildKit, a build gives the image this PATH
// when its base sets none, so that ENV PATH=/x:$PATH keeps the system directories.
const defaultPath = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

var platformArgs = map[string]string{
	"TARGETPLATFORM": "darwin/arm64", "TARGETOS": "darwin", "TARGETARCH": "arm64",
	"BUILDPLATFORM": "darwin/arm64", "BUILDOS": "darwin", "BUILDARCH": "arm64",
}

var (
	ownerRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(:[A-Za-z0-9_][A-Za-z0-9_.-]*)?$`)
	schemeRE   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
	archiveRE  = regexp.MustCompile(`\.(tar|tgz|tbz2?|txz|tar\.(gz|bz2|xz|zst))$`)
	errCopyEnd = errors.New("copy finished")
)

// mkdirScript is mkdir -p "$1" for an absolute path. When it creates "$1", it sets its mode to "$3"
// if given, and hands every directory it creates to owner "$2" if given, for COPY --chown and for
// WORKDIR under USER. The guest's chown takes no options (no -R, no -h), so
// each new directory is named.
const mkdirScript = `dir=$1; owner=$2; mode=$3; d=$1
case $dir in /*) ;; *) exit 2;; esac
set --
while [ ! -e "$d" ] && [ ! -L "$d" ]; do set -- "$@" "$d"; d=${d%/*}; [ -n "$d" ] || d=/; done
/bin/mkdir -p "$dir" || exit 1
if [ -n "$mode" ] && [ $# -gt 0 ]; then /bin/chmod "$mode" "$dir" || exit 1; fi
if [ -n "$owner" ] && [ $# -gt 0 ]; then /bin/chown "$owner" "$@"; fi`

// Build runs the Kidofile in a build container created from its FROM image and commits the
// result. The build container is removed even when the build fails, unless NoRm is set.
func Build(ctx context.Context, eng engine.Engine, opts Options) (*engine.Image, error) {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	cx, err := OpenContext(opts.ContextDir)
	if err != nil {
		return nil, err
	}
	defer cx.Close()
	kf := opts.Kidofile
	if kf == nil {
		if kf, err = cx.ReadFile("Kidofile"); err != nil {
			return nil, fmt.Errorf("Kidofile: %w", err)
		}
	}
	insts, err := Parse(bytes.NewReader(kf))
	if err != nil {
		return nil, err
	}
	p, err := plan(insts)
	if err != nil {
		return nil, err
	}
	var nonceBytes [6]byte
	rand.Read(nonceBytes[:])
	nonce := hex.EncodeToString(nonceBytes[:])
	tag := opts.Tag
	if tag == "" {
		tag = "build-" + nonce
	}
	target, err := imageref.Parse(tag)
	if err != nil {
		return nil, err
	}
	b := &builder{
		eng: eng, opts: opts, cx: cx, out: opts.Stdout, errw: opts.Stderr, target: target,
		plan: p, nonce: nonce,
		global: map[string]*string{}, args: map[string]*string{}, usedArgs: map[string]bool{},
		stageImages: map[int]string{}, stageConfigs: map[int]engine.ImageConfig{},
		stagedCopies: map[copyKey]stagedCopy{}, extractedImages: map[imageref.Ref]bool{},
	}
	for k, v := range platformArgs {
		v := v
		b.global[k] = &v
	}
	return b.build(ctx)
}

type step struct {
	Instruction
	argv      []string          // RUN, CMD, ENTRYPOINT, with the shell form wrapped in /bin/sh -c
	words     []string          // FROM: the image; COPY, ADD: sources, then the destination; EXPOSE: ports
	pairs     []kv              // ENV, LABEL, ARG
	flags     map[string]string // validated --name=value flags
	stageName string            // FROM: optional AS stage name
}

var stepFlags = map[string][]string{
	"FROM": {"platform"},
	"COPY": {"from", "chown", "chmod", "link"},
	"ADD":  {"chown", "chmod", "link"},
}

func unset(string) (string, bool) { return "", false }

// checkSource refuses what no build context can provide: URLs for ADD, absolute paths and "..".
func checkSource(cmd, src string) error {
	if cmd == "ADD" && (schemeRE.MatchString(src) || strings.HasPrefix(src, "git@")) {
		return fmt.Errorf("%s: ADD from a URL is not supported, download it on the host or in a RUN step", src)
	}
	_, err := cleanSource(src)
	return err
}

// plan checks the whole Kidofile before any VM starts.
func plan(insts []Instruction) (*buildPlan, error) {
	if len(insts) == 0 {
		return nil, errors.New("the Kidofile has no instructions")
	}
	p := &buildPlan{byName: map[string]int{}}
	var cur *stage
	for _, in := range insts {
		s := &step{Instruction: in, flags: map[string]string{}}
		if err := s.parse(); err != nil {
			return nil, fmt.Errorf("line %d: %s: %w", in.Line, in.Cmd, err)
		}
		switch {
		case s.Cmd == "FROM":
			idx := len(p.stages)
			if s.stageName != "" {
				if _, dup := p.byName[s.stageName]; dup {
					return nil, fmt.Errorf("line %d: FROM: duplicate stage name %q", in.Line, s.stageName)
				}
				if isAllDigits(s.stageName) && s.stageName != strconv.Itoa(idx) {
					return nil, fmt.Errorf("line %d: FROM: stage name %q conflicts with stage index", in.Line, s.stageName)
				}
				p.byName[s.stageName] = idx
			}
			cur = &stage{index: idx, name: s.stageName, from: s}
			p.stages = append(p.stages, cur)
		case cur == nil && s.Cmd == "ARG":
			p.globalArgs = append(p.globalArgs, s)
		case cur == nil:
			return nil, fmt.Errorf("line %d: %s before FROM: only ARG may come first", in.Line, s.Cmd)
		default:
			cur.steps = append(cur.steps, s)
		}
	}
	if len(p.stages) == 0 {
		return nil, errors.New("the Kidofile has no FROM instruction")
	}
	for _, st := range p.stages {
		for _, s := range st.steps {
			rawFrom, ok := s.flags["from"]
			if !ok || strings.Contains(rawFrom, "$") {
				continue
			}
			v, err := expandWord(rawFrom, unset)
			if err != nil {
				return nil, fmt.Errorf("line %d: %s: %w", s.Line, s.Cmd, err)
			}
			if _, err := resolveCopyFromRef(p, st.index, v); err != nil {
				return nil, fmt.Errorf("line %d: %s: %w", s.Line, s.Cmd, err)
			}
		}
	}
	return p, nil
}

func (s *step) parse() error {
	switch s.Cmd {
	case "FROM", "RUN", "CMD", "ENTRYPOINT", "COPY", "ADD", "ENV", "ARG", "WORKDIR", "USER", "LABEL", "EXPOSE":
	case "SHELL", "HEALTHCHECK", "VOLUME", "STOPSIGNAL", "ONBUILD", "MAINTAINER":
		return errors.New("not supported by kido build")
	default:
		return errors.New("unknown instruction")
	}
	for _, f := range s.Flags {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(f, "--"), "=")
		switch {
		case !slices.Contains(stepFlags[s.Cmd], name):
			return fmt.Errorf("flag --%s is not supported", name)
		case name == "link":
			if hasValue && value != "true" && value != "false" {
				return fmt.Errorf("--link=%s: want true or false", value)
			}
			continue
		case value == "":
			return fmt.Errorf("flag --%s needs a value", name)
		}
		if _, dup := s.flags[name]; dup {
			return fmt.Errorf("flag --%s given twice", name)
		}
		s.flags[name] = value
	}
	args := s.Args
	var err error
	switch s.Cmd {
	case "FROM":
		fields := strings.Fields(args)
		switch {
		case len(fields) == 1:
			s.words = fields
		case len(fields) == 3 && strings.EqualFold(fields[1], "AS"):
			name := fields[2]
			if name == "scratch" {
				return errors.New(`stage name "scratch" is reserved`)
			}
			if !stageNameRE.MatchString(name) || len(name) > 255 {
				return fmt.Errorf("invalid stage name %q: must start with a lowercase letter or digit and contain only lowercase letters, digits, '.', '_' or '-'", name)
			}
			s.words = fields[:1]
			s.stageName = name
		default:
			return errors.New("want FROM IMAGE [AS NAME]")
		}
	case "RUN", "CMD", "ENTRYPOINT":
		argv, isJSON, err := parseJSONArray(args)
		switch {
		case err != nil:
			return err
		case isJSON && len(argv) == 0 && s.Cmd == "RUN":
			return errors.New("requires at least one argument")
		case isJSON:
			s.argv = argv
		case args == "":
			return errors.New("requires at least one argument")
		case strings.HasPrefix(args, "<<"):
			return errors.New("heredocs are not supported")
		default:
			s.argv = []string{"/bin/sh", "-c", args}
		}
	case "COPY", "ADD":
		words, isJSON, err := parseJSONArray(args)
		if err != nil {
			return err
		}
		if !isJSON {
			if strings.HasPrefix(args, "<<") {
				return errors.New("heredocs are not supported")
			}
			words = strings.Fields(args)
		}
		if len(words) < 2 {
			return errors.New("requires at least two arguments: SOURCE... DEST")
		}
		s.words = words
		_, isFrom := s.flags["from"]
		for _, w := range words[:len(words)-1] {
			if strings.Contains(w, "$") {
				continue
			}
			v, err := expandWord(w, unset)
			if err != nil {
				return err
			}
			if isFrom {
				if _, _, err := cleanStageSource(v); err != nil {
					return err
				}
			} else if err := checkSource(s.Cmd, v); err != nil {
				return err
			}
		}
	case "ENV", "LABEL":
		s.pairs, err = parseKeyValues(s.Cmd, args)
	case "ARG":
		s.pairs, err = parseArgs(args)
	case "WORKDIR", "USER":
		if args == "" {
			return errors.New("requires exactly one argument")
		}
	case "EXPOSE":
		if s.words = strings.Fields(args); len(s.words) == 0 {
			return errors.New("requires at least one argument")
		}
	}
	if err != nil {
		return err
	}
	return s.checkSyntax()
}

// checkSyntax expands every word the step will expand with nothing set, to report substitution
// and quoting errors before the build starts.
func (s *step) checkSyntax() error {
	words := slices.Clone(s.words)
	for _, p := range s.pairs {
		if s.Cmd != "ARG" {
			words = append(words, p.key)
		}
		if p.hasValue {
			words = append(words, p.value)
		}
	}
	if s.Cmd == "WORKDIR" || s.Cmd == "USER" {
		words = append(words, s.Args)
	}
	for _, v := range s.flags {
		words = append(words, v)
	}
	for _, w := range words {
		if _, err := expandWord(w, unset); err != nil {
			return err
		}
	}
	return nil
}

type builder struct {
	eng    engine.Engine
	opts   Options
	cx     *Context
	out    io.Writer
	errw   io.Writer
	target imageref.Ref
	plan   *buildPlan
	nonce  string

	targetStageIdx  int
	selected        []*stage
	fromRefs        map[int]stageRef
	copyFromRefs    map[copyKey]stageRef
	needCommit      map[int]bool
	baseImages      map[imageref.Ref]*engine.Image
	stageCopies     map[int][]copyTask
	imageCopies     map[imageref.Ref][]copyTask
	stagedCopies    map[copyKey]stagedCopy
	extractedImages map[imageref.Ref]bool
	stageImages     map[int]string
	stageConfigs    map[int]engine.ImageConfig
	tempImages      []string
	stageDir        string

	global   map[string]*string // ARGs declared before FROM; nil: declared without a value
	args     map[string]*string // ARGs declared after FROM
	argOrder []string
	usedArgs map[string]bool
	inStage  bool

	cfg     engine.ImageConfig
	cmdSet  bool
	id      string // active build container
	idImage string // image of active build container
}

func (b *builder) build(ctx context.Context) (img *engine.Image, err error) {
	defer b.cleanup(ctx)
	if err := b.resolvePlan(ctx); err != nil {
		return nil, err
	}
	n := len(b.plan.globalArgs)
	for _, st := range b.selected {
		n += 1 + len(st.steps)
	}
	stepNo := 0
	for _, s := range b.plan.globalArgs {
		stepNo++
		fmt.Fprintf(b.out, "Step %d/%d : %s\n", stepNo, n, s.Original)
	}
	for _, st := range b.selected {
		if err := b.extractExternalImagesForStage(ctx, st); err != nil {
			return nil, err
		}
		stepNo++
		fmt.Fprintf(b.out, "Step %d/%d : %s\n", stepNo, n, st.from.Original)
		if err := b.startStage(ctx, st); err != nil {
			return nil, fmt.Errorf("step %d/%d (line %d) FROM: %w", stepNo, n, st.from.Line, err)
		}
		for j, s := range st.steps {
			stepNo++
			fmt.Fprintf(b.out, "Step %d/%d : %s\n", stepNo, n, s.Original)
			if err := b.dispatch(ctx, st.index, j, s); err != nil {
				if se, ok := errors.AsType[*StepError](err); ok {
					se.Step, se.Steps, se.Line, se.Instruction = stepNo, n, s.Line, s.Original
					return nil, se
				}
				return nil, fmt.Errorf("step %d/%d (line %d) %s: %w", stepNo, n, s.Line, s.Cmd, err)
			}
		}
		if st.index != b.targetStageIdx {
			if err := b.finishIntermediateStage(ctx, st); err != nil {
				return nil, err
			}
			continue
		}
		if img, err = b.eng.Commit(ctx, b.id, b.target.String(), b.cfg); err != nil {
			return nil, fmt.Errorf("commit %s: %w", b.target, err)
		}
	}
	var unused []string
	for k := range b.opts.BuildArgs {
		if !b.usedArgs[k] {
			unused = append(unused, k)
		}
	}
	if len(unused) > 0 {
		slices.Sort(unused)
		fmt.Fprintf(b.errw, "[Warning] One or more build-args %v were not consumed\n", unused)
	}
	fmt.Fprintf(b.out, "Successfully built %s\n", b.target)
	return img, nil
}

func (b *builder) finishIntermediateStage(ctx context.Context, st *stage) error {
	if tasks := b.stageCopies[st.index]; len(tasks) > 0 {
		if err := b.extractTasksFromContainer(ctx, b.id, tasks); err != nil {
			return err
		}
	}
	if b.needCommit[st.index] {
		stageImg := fmt.Sprintf("kido-build-stage-%s-%d:latest", b.nonce, st.index)
		if _, err := b.eng.Commit(ctx, b.id, stageImg, b.cfg); err != nil {
			return fmt.Errorf("commit stage %d: %w", st.index, err)
		}
		b.tempImages = append(b.tempImages, stageImg)
		b.stageImages[st.index] = stageImg
		b.stageConfigs[st.index] = cloneImageConfig(b.cfg)
	}
	b.removeActiveContainer(ctx)
	return ctx.Err()
}

func cloneImageConfig(cfg engine.ImageConfig) engine.ImageConfig {
	out := engine.ImageConfig{
		Entrypoint: slices.Clone(cfg.Entrypoint),
		Cmd:        slices.Clone(cfg.Cmd),
		Env:        slices.Clone(cfg.Env),
		WorkingDir: cfg.WorkingDir,
		User:       cfg.User,
		Labels:     map[string]string{},
	}
	for k, v := range cfg.Labels {
		out.Labels[k] = v
	}
	return out
}

func (b *builder) removeActiveContainer(ctx context.Context) {
	if b.id == "" {
		return
	}
	id := b.id
	b.id, b.idImage = "", ""
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := b.eng.Remove(cctx, id, true); err != nil {
		fmt.Fprintf(b.errw, "kido build: removing build container %s: %v\n", id, err)
	}
}

func (b *builder) cleanup(ctx context.Context) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	keptImage := ""
	if b.id != "" {
		if b.opts.NoRm {
			fmt.Fprintf(b.errw, "Keeping build container %s\n", b.id)
			keptImage = b.idImage
		} else {
			if err := b.eng.Remove(cctx, b.id, true); err != nil {
				fmt.Fprintf(b.errw, "kido build: removing build container %s: %v\n", b.id, err)
			}
			b.id, b.idImage = "", ""
		}
	}
	for i := len(b.tempImages) - 1; i >= 0; i-- {
		img := b.tempImages[i]
		if img == keptImage {
			fmt.Fprintf(b.errw, "Keeping stage image %s for build container %s\n", img, b.id)
			continue
		}
		if err := b.eng.RemoveImage(cctx, img); err != nil {
			fmt.Fprintf(b.errw, "kido build: removing stage image %s: %v\n", img, err)
		}
	}
	b.tempImages = nil
	if b.stageDir != "" {
		os.RemoveAll(b.stageDir)
		b.stageDir = ""
	}
}

func (b *builder) dispatch(ctx context.Context, stageIdx, stepIdx int, s *step) error {
	switch s.Cmd {
	case "ARG":
		return b.arg(s)
	case "RUN":
		return b.run(ctx, s)
	case "COPY":
		if _, ok := s.flags["from"]; ok {
			return b.copyFrom(ctx, s, copyKey{stageIdx, stepIdx})
		}
		return b.copy(ctx, s)
	case "ADD":
		return b.copy(ctx, s)
	case "ENV":
		return b.env(s)
	case "LABEL":
		return b.label(s)
	case "WORKDIR":
		return b.workdir(ctx, s)
	case "USER":
		v, err := expandWord(s.Args, b.lookup)
		if err != nil {
			return err
		}
		if !ownerRE.MatchString(v) {
			return fmt.Errorf("invalid user %q: want NAME, UID, NAME:GROUP or UID:GID", v)
		}
		b.cfg.User = v
	case "CMD":
		b.cfg.Cmd, b.cmdSet = slices.Clone(s.argv), true
	case "ENTRYPOINT":
		b.cfg.Entrypoint = slices.Clone(s.argv)
		if !b.cmdSet {
			b.cfg.Cmd = nil
		}
	case "EXPOSE":
		return b.expose(s)
	}
	return nil
}

func (b *builder) globalLookup(name string) (string, bool) {
	if v := b.global[name]; v != nil {
		return *v, true
	}
	return "", false
}

// lookup resolves a variable in the stage: ENV wins over ARG.
func (b *builder) lookup(name string) (string, bool) {
	if v, ok := envLookup(b.cfg.Env, name); ok {
		return v, true
	}
	if v := b.args[name]; v != nil {
		return *v, true
	}
	return "", false
}

func envLookup(env []string, name string) (string, bool) {
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

func envSet(env []string, name, value string) []string {
	for i, e := range env {
		if k, _, _ := strings.Cut(e, "="); k == name {
			env[i] = name + "=" + value
			return env
		}
	}
	return append(env, name+"="+value)
}

// runEnv is the environment of RUN: the image's ENV plus the stage's ARGs that ENV does not set.
func (b *builder) runEnv() []string {
	env := slices.Clone(b.cfg.Env)
	for _, name := range b.argOrder {
		if v := b.args[name]; v != nil {
			if _, ok := envLookup(env, name); !ok {
				env = append(env, name+"="+*v)
			}
		}
	}
	return env
}

func (b *builder) arg(s *step) error {
	scope, lookup := b.global, b.globalLookup
	if b.inStage {
		scope, lookup = b.args, b.lookup
	}
	for _, p := range s.pairs {
		var val *string
		if v, ok := b.opts.BuildArgs[p.key]; ok {
			val = &v
			b.usedArgs[p.key] = true
		} else if p.hasValue {
			v, err := expandWord(p.value, lookup)
			if err != nil {
				return err
			}
			val = &v
		} else if b.inStage {
			val = b.global[p.key]
		}
		if _, seen := scope[p.key]; !seen && b.inStage {
			b.argOrder = append(b.argOrder, p.key)
		}
		scope[p.key] = val
	}
	return nil
}

func (b *builder) localImage(ctx context.Context, ref imageref.Ref) (*engine.Image, error) {
	images, err := b.eng.Images(ctx)
	if err != nil {
		return nil, err
	}
	for _, img := range images {
		if img.Name == ref.Name && img.Tag == ref.Tag {
			return img, nil
		}
	}
	return nil, nil
}

func (b *builder) startStage(ctx context.Context, st *stage) error {
	b.inStage = true
	b.args = map[string]*string{}
	b.argOrder = nil
	b.cmdSet = false

	fref := b.fromRefs[st.index]
	var imageRef string
	var baseCfg engine.ImageConfig
	if fref.kind == refStage {
		imageRef = b.stageImages[fref.stageIndex]
		baseCfg = b.stageConfigs[fref.stageIndex]
	} else {
		imageRef = fref.image.String()
		baseCfg = b.baseImages[fref.image].Config
	}
	b.cfg = cloneImageConfig(baseCfg)
	if b.cfg.WorkingDir != "" {
		b.cfg.WorkingDir = path.Join("/", b.cfg.WorkingDir)
	}
	if _, ok := envLookup(b.cfg.Env, "PATH"); !ok {
		b.cfg.Env = append(b.cfg.Env, "PATH="+defaultPath)
	}
	c, err := b.eng.Create(ctx, engine.CreateOptions{
		Image: imageRef, CPUs: b.opts.CPUs, MemoryMB: b.opts.MemoryMB,
		Labels: map[string]string{BuildLabel: b.target.String()},
	})
	if c != nil {
		b.id = c.ID
		b.idImage = imageRef
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(b.out, " ---> Running in %s\n", c.ID)
	return nil
}

func (b *builder) run(ctx context.Context, s *step) error {
	status, err := b.eng.Exec(ctx, b.id,
		engine.ProcessSpec{Args: s.argv, Env: b.runEnv(), Dir: b.cfg.WorkingDir, User: b.cfg.User},
		engine.Stdio{Stdout: b.out, Stderr: b.errw})
	if err != nil {
		// The engine reports a program the guest cannot find or run as 127 or 126 with an error.
		if status == 126 || status == 127 {
			return &StepError{ExitStatus: status, Msg: err.Error()}
		}
		return err
	}
	if status != 0 {
		return &StepError{ExitStatus: status}
	}
	return nil
}

func (b *builder) env(s *step) error {
	type pair struct{ k, v string }
	pairs := make([]pair, 0, len(s.pairs))
	// Every value is expanded against the environment from before this instruction.
	for _, p := range s.pairs {
		k, err := expandWord(p.key, b.lookup)
		if err != nil {
			return err
		}
		v, err := expandWord(p.value, b.lookup)
		if err != nil {
			return err
		}
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return fmt.Errorf("invalid variable %q", k)
		}
		pairs = append(pairs, pair{k, v})
	}
	for _, p := range pairs {
		b.cfg.Env = envSet(b.cfg.Env, p.k, p.v)
	}
	return nil
}

func (b *builder) label(s *step) error {
	for _, p := range s.pairs {
		k, err := expandWord(p.key, b.lookup)
		if err != nil {
			return err
		}
		v, err := expandWord(p.value, b.lookup)
		if err != nil {
			return err
		}
		if k == "" {
			return errors.New("empty label name")
		}
		b.cfg.Labels[k] = v
	}
	return nil
}

func (b *builder) workdir(ctx context.Context, s *step) error {
	v, err := expandWord(s.Args, b.lookup)
	if err != nil {
		return err
	}
	if v == "" || strings.ContainsRune(v, 0) {
		return fmt.Errorf("invalid directory %q", v)
	}
	if !path.IsAbs(v) {
		v = path.Join(b.workdirOrRoot(), v)
	}
	b.cfg.WorkingDir = path.Clean(v)
	owner := ""
	if b.cfg.User != "" {
		owner = ownerSpec(b.cfg.User)
	}
	return b.mkdir(ctx, b.cfg.WorkingDir, owner, "")
}

func (b *builder) workdirOrRoot() string {
	if b.cfg.WorkingDir == "" {
		return "/"
	}
	return b.cfg.WorkingDir
}

func (b *builder) expose(s *step) error {
	ports := strings.Fields(b.cfg.Labels[ExposeLabel])
	for _, w := range s.words {
		v, err := expandWord(w, b.lookup)
		if err != nil {
			return err
		}
		for _, f := range strings.Fields(v) {
			p, err := normalizePort(f)
			if err != nil {
				return err
			}
			if !slices.Contains(ports, p) {
				ports = append(ports, p)
			}
		}
	}
	b.cfg.Labels[ExposeLabel] = strings.Join(ports, " ")
	return nil
}

// normalizePort accepts PORT[-PORT][/tcp|/udp] and returns it with the protocol spelled out.
func normalizePort(s string) (string, error) {
	ports, proto, _ := strings.Cut(s, "/")
	proto = strings.ToLower(proto)
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return "", fmt.Errorf("EXPOSE %s: protocol must be tcp or udp", s)
	}
	lo, hi, isRange := strings.Cut(ports, "-")
	l, err1 := strconv.Atoi(lo)
	h, err2 := l, error(nil)
	if isRange {
		h, err2 = strconv.Atoi(hi)
	}
	if err1 != nil || err2 != nil || l < 1 || h > 65535 || l > h {
		return "", fmt.Errorf("EXPOSE %s: invalid port", s)
	}
	if isRange {
		return fmt.Sprintf("%d-%d/%s", l, h, proto), nil
	}
	return fmt.Sprintf("%d/%s", l, proto), nil
}

// owner expands and validates --chown. A bare UID also sets the GID, and a bare user name
// changes the owner only.
func (b *builder) owner(s *step) (string, error) {
	raw, ok := s.flags["chown"]
	if !ok {
		return "", nil
	}
	v, err := expandWord(raw, b.lookup)
	if err != nil {
		return "", err
	}
	if !ownerRE.MatchString(v) {
		return "", fmt.Errorf("--chown=%s: want USER[:GROUP] or UID[:GID]", v)
	}
	return ownerSpec(v), nil
}

// ownerSpec gives a bare UID the same GID.
func ownerSpec(v string) string {
	if _, err := strconv.Atoi(v); err == nil {
		return v + ":" + v
	}
	return v
}

func (b *builder) chmod(s *step) (int64, error) {
	raw, ok := s.flags["chmod"]
	if !ok {
		return -1, nil
	}
	v, err := expandWord(raw, b.lookup)
	if err != nil {
		return 0, err
	}
	m, err := strconv.ParseInt(v, 8, 64)
	if err != nil || m < 0 || m > 0o7777 {
		return 0, fmt.Errorf("--chmod=%s: want an octal mode such as 755", v)
	}
	return m, nil
}

func (b *builder) copy(ctx context.Context, s *step) error {
	words := make([]string, len(s.words))
	for i, w := range s.words {
		v, err := expandWord(w, b.lookup)
		if err != nil {
			return err
		}
		words[i] = v
	}
	srcs, dst := words[:len(words)-1], words[len(words)-1]
	owner, err := b.owner(s)
	if err != nil {
		return err
	}
	chmod, err := b.chmod(s)
	if err != nil {
		return err
	}
	var sources []source
	for _, src := range srcs {
		if err := checkSource(s.Cmd, src); err != nil {
			return err
		}
		found, err := b.cx.resolve(src)
		if err != nil {
			return err
		}
		sources = append(sources, found...)
	}
	if len(sources) == 0 {
		return fmt.Errorf("no source files were specified: nothing in the build context matches %s", strings.Join(srcs, " "))
	}
	dirForm := dst == "" || dst == "." || strings.HasSuffix(dst, "/")
	if strings.ContainsRune(dst, 0) {
		return fmt.Errorf("invalid destination %q", dst)
	}
	if !path.IsAbs(dst) {
		dst = path.Join(b.workdirOrRoot(), dst)
	}
	dst = path.Clean(dst)
	if len(sources) > 1 && !dirForm {
		return errors.New("with more than one source the destination must be a directory and end with a /")
	}
	targetDir := dst
	var entries []entry
	for _, src := range sources {
		name := src.base
		if !src.info.IsDir() && !dirForm {
			isDir, err := b.isDir(ctx, dst)
			if err != nil {
				return err
			}
			if !isDir {
				targetDir, name = path.Dir(dst), path.Base(dst)
			}
		}
		if s.Cmd == "ADD" && !src.info.IsDir() && archiveRE.MatchString(src.base) {
			fmt.Fprintf(b.errw, "kido build: ADD copies %s as a file, it does not unpack archives\n", src.base)
		}
		es, err := b.cx.entries(src, name)
		if err != nil {
			return err
		}
		entries = append(entries, es...)
	}
	// A directory this COPY creates for a single source directory takes that directory's mode.
	mode := ""
	if len(sources) == 1 && sources[0].info.IsDir() {
		m := tarMode(sources[0].info.Mode())
		if chmod >= 0 {
			m = chmod
		}
		mode = strconv.FormatInt(m, 8)
	}
	if err := b.mkdir(ctx, targetDir, owner, mode); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	if err := b.copyIn(ctx, entries, targetDir, chmod); err != nil {
		return err
	}
	if owner == "" {
		return nil
	}
	return b.chown(ctx, owner, targetDir, entries)
}

func (b *builder) copyIn(ctx context.Context, entries []entry, dir string, chmod int64) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := b.cx.writeTar(pw, entries, chmod)
		pw.CloseWithError(err)
		done <- err
	}()
	err := b.eng.CopyIn(ctx, b.id, pr, dir)
	pr.CloseWithError(errCopyEnd)
	if werr := <-done; werr != nil && !errors.Is(werr, errCopyEnd) {
		return werr
	}
	return err
}

// chown gives every copied file and directory to owner, in batches that keep each request well
// under the agent's line limit. Symlinks are skipped: the guest's chown has no -h and would change
// what they point to, which may be anything in the image.
func (b *builder) chown(ctx context.Context, owner, dir string, entries []entry) error {
	relPaths := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.info.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		relPaths = append(relPaths, e.name)
	}
	return b.chownPaths(ctx, owner, dir, relPaths)
}

func (b *builder) chownPaths(ctx context.Context, owner, dir string, relPaths []string) error {
	const budget = 48 << 10 // of JSON, against the 64 KiB proto.MaxLine
	var paths []string
	size := 0
	flush := func() error {
		if len(paths) == 0 {
			return nil
		}
		err := b.guest(ctx, append([]string{"/bin/chown", owner}, paths...)...)
		paths, size = nil, 0
		return err
	}
	for _, rel := range relPaths {
		p := path.Join(dir, rel)
		paths = append(paths, p)
		q, _ := json.Marshal(p)
		if size += len(q) + 1; size >= budget {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func (b *builder) mkdir(ctx context.Context, dir, owner, mode string) error {
	if !path.IsAbs(dir) {
		return fmt.Errorf("directory %q is not absolute", dir)
	}
	return b.guest(ctx, "/bin/sh", "-c", mkdirScript, "sh", dir, owner, mode)
}

func (b *builder) isDir(ctx context.Context, p string) (bool, error) {
	return b.isDirIn(ctx, b.id, p)
}

func (b *builder) isDirIn(ctx context.Context, containerID, p string) (bool, error) {
	status, err := b.eng.Exec(ctx, containerID, engine.ProcessSpec{Args: []string{"/bin/test", "-d", p}}, engine.Stdio{})
	if err != nil {
		return false, err
	}
	return status == 0, nil
}

// guest runs a helper command as root in the build container and fails on a non-zero status.
func (b *builder) guest(ctx context.Context, args ...string) error {
	stderr := &tailBuffer{max: 64 << 10}
	status, err := b.eng.Exec(ctx, b.id, engine.ProcessSpec{Args: args}, engine.Stdio{Stdout: io.Discard, Stderr: stderr})
	if err != nil {
		return err
	}
	if status != 0 {
		msg := runtime.Sanitize(strings.TrimSpace(stderr.String()))
		if msg == "" {
			msg = fmt.Sprintf("exit status %d", status)
		}
		return fmt.Errorf("%s: %s", args[0], msg)
	}
	return nil
}

// tailBuffer keeps only the last max bytes written to it, so a guest cannot grow host memory.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
