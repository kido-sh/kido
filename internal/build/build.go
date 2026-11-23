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
	Dockerfile []byte // nil: the Dockerfile at the root of the context
	Tag        string // NAME[:TAG] of the result; build-<random>:latest when empty
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
const BuildLabel = "kido.build"

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
// if given, and hands every directory it creates to owner "$2" if given, as Docker does for
// COPY --chown and for WORKDIR under USER. The guest's chown takes no options (no -R, no -h), so
// each new directory is named.
const mkdirScript = `dir=$1; owner=$2; mode=$3; d=$1
case $dir in /*) ;; *) exit 2;; esac
set --
while [ ! -e "$d" ] && [ ! -L "$d" ]; do set -- "$@" "$d"; d=${d%/*}; [ -n "$d" ] || d=/; done
/bin/mkdir -p "$dir" || exit 1
if [ -n "$mode" ] && [ $# -gt 0 ]; then /bin/chmod "$mode" "$dir" || exit 1; fi
if [ -n "$owner" ] && [ $# -gt 0 ]; then /bin/chown "$owner" "$@"; fi`

// Build runs the Dockerfile in a build container created from its FROM image and commits the
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
	df := opts.Dockerfile
	if df == nil {
		if df, err = cx.ReadFile("Dockerfile"); err != nil {
			return nil, fmt.Errorf("Dockerfile: %w", err)
		}
	}
	insts, err := Parse(bytes.NewReader(df))
	if err != nil {
		return nil, err
	}
	steps, err := plan(insts)
	if err != nil {
		return nil, err
	}
	tag := opts.Tag
	if tag == "" {
		var id [6]byte
		rand.Read(id[:])
		tag = "build-" + hex.EncodeToString(id[:])
	}
	target, err := imageref.Parse(tag)
	if err != nil {
		return nil, err
	}
	b := &builder{
		eng: eng, opts: opts, cx: cx, out: opts.Stdout, errw: opts.Stderr, target: target, steps: steps,
		global: map[string]*string{}, args: map[string]*string{}, usedArgs: map[string]bool{},
	}
	for k, v := range platformArgs {
		b.global[k] = &v
	}
	return b.build(ctx)
}

type step struct {
	Instruction
	argv  []string          // RUN, CMD, ENTRYPOINT, with the shell form wrapped in /bin/sh -c
	words []string          // FROM: the image; COPY, ADD: sources, then the destination; EXPOSE: ports
	pairs []kv              // ENV, LABEL, ARG
	flags map[string]string // validated --name=value flags
}

var stepFlags = map[string][]string{
	"FROM": {"platform"},
	"COPY": {"chown", "chmod", "link"},
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

// plan checks the whole Dockerfile before any VM starts.
func plan(insts []Instruction) ([]*step, error) {
	if len(insts) == 0 {
		return nil, errors.New("the Dockerfile has no instructions")
	}
	steps := make([]*step, 0, len(insts))
	from := false
	for _, in := range insts {
		s := &step{Instruction: in, flags: map[string]string{}}
		if err := s.parse(); err != nil {
			return nil, fmt.Errorf("line %d: %s: %w", in.Line, in.Cmd, err)
		}
		switch {
		case s.Cmd == "FROM" && from:
			return nil, fmt.Errorf("line %d: FROM: multi-stage builds are not supported", in.Line)
		case s.Cmd == "FROM":
			from = true
		case !from && s.Cmd != "ARG":
			return nil, fmt.Errorf("line %d: %s before FROM: only ARG may come first", in.Line, s.Cmd)
		}
		steps = append(steps, s)
	}
	if !from {
		return nil, errors.New("the Dockerfile has no FROM instruction")
	}
	return steps, nil
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
		case name == "from" && (s.Cmd == "COPY" || s.Cmd == "ADD"):
			return errors.New("--from is not supported: kido build has no multi-stage builds")
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
		s.words = strings.Fields(args)
		switch {
		case len(s.words) == 1:
		case len(s.words) == 3 && strings.EqualFold(s.words[1], "AS"):
			s.words = s.words[:1]
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
		for _, w := range words[:len(words)-1] {
			if strings.Contains(w, "$") {
				continue
			}
			v, err := expandWord(w, unset)
			if err != nil {
				return err
			}
			if err := checkSource(s.Cmd, v); err != nil {
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
	steps  []*step

	global   map[string]*string // ARGs declared before FROM; nil: declared without a value
	args     map[string]*string // ARGs declared after FROM
	argOrder []string
	usedArgs map[string]bool
	inStage  bool

	cfg    engine.ImageConfig
	cmdSet bool
	id     string // build container
}

func (b *builder) build(ctx context.Context) (img *engine.Image, err error) {
	defer b.cleanup(ctx)
	n := len(b.steps)
	for i, s := range b.steps {
		fmt.Fprintf(b.out, "Step %d/%d : %s\n", i+1, n, s.Original)
		if err := b.dispatch(ctx, s); err != nil {
			if se, ok := errors.AsType[*StepError](err); ok {
				se.Step, se.Steps, se.Line, se.Instruction = i+1, n, s.Line, s.Original
				return nil, se
			}
			return nil, fmt.Errorf("step %d/%d (line %d) %s: %w", i+1, n, s.Line, s.Cmd, err)
		}
	}
	if img, err = b.eng.Commit(ctx, b.id, b.target.String(), b.cfg); err != nil {
		return nil, fmt.Errorf("commit %s: %w", b.target, err)
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

func (b *builder) cleanup(ctx context.Context) {
	if b.id == "" {
		return
	}
	if b.opts.NoRm {
		fmt.Fprintf(b.errw, "Keeping build container %s\n", b.id)
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := b.eng.Remove(cctx, b.id, true); err != nil {
		fmt.Fprintf(b.errw, "kido build: removing build container %s: %v\n", b.id, err)
	}
}

func (b *builder) dispatch(ctx context.Context, s *step) error {
	switch s.Cmd {
	case "ARG":
		return b.arg(s)
	case "FROM":
		return b.from(ctx, s)
	case "RUN":
		return b.run(ctx, s)
	case "COPY", "ADD":
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

func (b *builder) from(ctx context.Context, s *step) error {
	b.inStage = true
	if p, ok := s.flags["platform"]; ok {
		v, err := expandWord(p, b.globalLookup)
		if err != nil {
			return err
		}
		if !strings.EqualFold(v, "darwin/arm64") {
			return fmt.Errorf("--platform=%s: kido builds darwin/arm64 images only", v)
		}
	}
	name, err := expandWord(s.words[0], b.globalLookup)
	if err != nil {
		return err
	}
	if base, _, _ := strings.Cut(name, ":"); base == "scratch" {
		return errors.New("FROM scratch is not supported: a kido image needs a Darwin userland, start from a local image such as darwin")
	}
	ref, err := imageref.Parse(name)
	if err != nil {
		return err
	}
	if ref == b.target {
		return fmt.Errorf("the image being built, %s, is also its base image: choose another -t", ref)
	}
	base, err := b.localImage(ctx, ref)
	if err != nil {
		return err
	}
	if base == nil && b.opts.Pull != nil {
		if err := b.opts.Pull(ctx, ref.String()); err != nil {
			return err
		}
		if base, err = b.localImage(ctx, ref); err != nil {
			return err
		}
	}
	if base == nil {
		return fmt.Errorf("image %s not found", ref)
	}
	// Other tools write image configs too; a bad user or directory must not reach the guest helpers.
	if u := base.Config.User; u != "" && !ownerRE.MatchString(u) {
		return fmt.Errorf("image %s has an invalid user %q", ref, u)
	}
	if wd := base.Config.WorkingDir; strings.ContainsRune(wd, 0) {
		return fmt.Errorf("image %s has an invalid working directory %q", ref, wd)
	}
	b.cfg = engine.ImageConfig{
		Entrypoint: slices.Clone(base.Config.Entrypoint),
		Cmd:        slices.Clone(base.Config.Cmd),
		Env:        slices.Clone(base.Config.Env),
		User:       base.Config.User,
		Labels:     map[string]string{},
	}
	if wd := base.Config.WorkingDir; wd != "" {
		b.cfg.WorkingDir = path.Join("/", wd)
	}
	for k, v := range base.Config.Labels {
		b.cfg.Labels[k] = v
	}
	if _, ok := envLookup(b.cfg.Env, "PATH"); !ok {
		b.cfg.Env = append(b.cfg.Env, "PATH="+defaultPath)
	}
	c, err := b.eng.Create(ctx, engine.CreateOptions{
		Image: ref.String(), CPUs: b.opts.CPUs, MemoryMB: b.opts.MemoryMB,
		Labels: map[string]string{BuildLabel: b.target.String()},
	})
	// Create can fail after the container exists (a boot that times out or is interrupted).
	if c != nil {
		b.id = c.ID
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

// owner expands and validates --chown. A bare UID also sets the GID, as in Docker; a bare user
// name changes the owner only.
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

// ownerSpec gives a bare UID the same GID, as Docker does.
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
	for _, e := range entries {
		if e.info.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		p := path.Join(dir, e.name)
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
	status, err := b.eng.Exec(ctx, b.id, engine.ProcessSpec{Args: []string{"/bin/test", "-d", p}}, engine.Stdio{})
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
