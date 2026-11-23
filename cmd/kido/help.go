package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"
)

// ctrFlags returns a flag set in which --help/-h print the usage to stdout and exit 0,
// a bad flag prints "kido: ..." and a pointer to --help, and fs.Usage() prints the usage to stderr.
func ctrFlags(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	errs := &ctrFlagErrors{}
	fs.SetOutput(errs)
	help := &ctrHelpFlag{fs: fs, usage: usage}
	fs.Var(help, "help", "Print usage")
	fs.Var(help, "h", "Print usage")
	fs.Usage = func() {
		if errs.failed {
			fmt.Fprintf(os.Stderr, "See 'kido %s --help'.\n", name)
			return
		}
		if ctrArgsError(os.Stderr, name, usage, fs.NArg()) {
			return
		}
		ctrPrintUsage(os.Stderr, fs, usage)
	}
	return fs
}

// ctrFlagErrors receives the flag package's error messages.
type ctrFlagErrors struct{ failed bool }

func (e *ctrFlagErrors) Write(p []byte) (int, error) {
	e.failed = true
	msg := strings.TrimSpace(string(p))
	msg = strings.Replace(msg, "flag provided but not defined: -", "unknown flag: --", 1)
	fmt.Fprintln(os.Stderr, "kido: "+msg)
	return len(p), nil
}

// ctrIntrospecting makes --help hand its flag set to ctrCommandFlags instead of printing it.
var ctrIntrospecting bool

type ctrIntrospection struct{ fs *flag.FlagSet }

type ctrHelpFlag struct {
	fs    *flag.FlagSet
	usage string
}

func (h *ctrHelpFlag) String() string   { return "" }
func (h *ctrHelpFlag) IsBoolFlag() bool { return true }
func (h *ctrHelpFlag) Set(string) error {
	if ctrIntrospecting {
		panic(ctrIntrospection{h.fs})
	}
	ctrPrintUsage(os.Stdout, h.fs, h.usage)
	os.Exit(0)
	return nil
}

func ctrIsHelpArg(a string) bool { return a == "-h" || a == "-help" || a == "--help" }

// ctrInterspersed moves flags that follow positional arguments in front of them
// (`kido save IMAGE -o FILE`), so fs.Parse sees them; "--" still ends the flags. Not for commands
// whose arguments are a command line (run, create, exec).
func ctrInterspersed(fs *flag.FlagSet, args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if f := fs.Lookup(name); f != nil && i+1 < len(args) {
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
				i++
				flags = append(flags, args[i])
			}
		}
	}
	return append(append(flags, "--"), pos...)
}

// ctrOption is one option as help and completion show it: its aliases share one value.
type ctrOption struct {
	Short, Long []string
	Type        string // empty for a boolean option
	Usage       string
	Default     string
}

func (o ctrOption) Names() []string {
	var out []string
	for _, s := range o.Short {
		out = append(out, "-"+s)
	}
	for _, l := range o.Long {
		out = append(out, "--"+l)
	}
	return out
}

// ctrOptions groups the flags of fs that share a value, without the help flags.
func ctrOptions(fs *flag.FlagSet) []ctrOption {
	var opts []*ctrOption
	byValue := map[uintptr]*ctrOption{}
	fs.VisitAll(func(f *flag.Flag) {
		if _, ok := f.Value.(*ctrHelpFlag); ok {
			return
		}
		var o *ctrOption
		v := reflect.ValueOf(f.Value)
		if v.Kind() == reflect.Pointer {
			o = byValue[v.Pointer()]
		}
		if o == nil {
			typ, use := flag.UnquoteUsage(f)
			switch f.Value.(type) {
			case *ctrList:
				typ = "list"
			case *ctrOptString:
				typ = "string"
			}
			o = &ctrOption{Type: typ, Usage: use}
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
				o.Type = ""
			}
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" && f.DefValue != "[]" {
				o.Default = f.DefValue
			}
			opts = append(opts, o)
			if v.Kind() == reflect.Pointer {
				byValue[v.Pointer()] = o
			}
		} else if _, use := flag.UnquoteUsage(f); !strings.HasPrefix(use, "same as") &&
			(len(o.Long) == 0 && len(f.Name) > 1 || strings.HasPrefix(o.Usage, "same as")) {
			o.Usage = use
		}
		if len(f.Name) == 1 {
			o.Short = append(o.Short, f.Name)
		} else {
			o.Long = append(o.Long, f.Name)
		}
	})
	out := make([]ctrOption, 0, len(opts))
	for _, o := range opts {
		out = append(out, *o)
	}
	key := func(o ctrOption) string {
		if len(o.Long) > 0 {
			return o.Long[0]
		}
		return o.Short[0]
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}

func ctrCapitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

func ctrPrintUsage(w io.Writer, fs *flag.FlagSet, usage string) {
	name := fs.Name()
	usage, more, _ := strings.Cut(usage, "\n\n")
	fmt.Fprintf(w, "Usage:  kido %s %s\n", name, usage)
	if s := ctrSummary(name); s != "" {
		fmt.Fprintf(w, "\n%s\n", s)
	}
	if more != "" {
		fmt.Fprintf(w, "\n%s\n", more)
	}
	opts := ctrOptions(fs)
	if len(opts) == 0 {
		return
	}
	fmt.Fprintln(w, "\nOptions:")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	for _, o := range opts {
		var short []string
		for _, s := range o.Short {
			short = append(short, "-"+s)
		}
		var long []string
		for _, l := range o.Long {
			long = append(long, "--"+l)
		}
		left := strings.Join(short, ", ")
		switch {
		case left == "":
			left = "    " + strings.Join(long, ", ")
		case len(long) > 0:
			left += ", " + strings.Join(long, ", ")
		}
		if o.Type != "" {
			left += " " + o.Type
		}
		use := ctrCapitalize(o.Usage)
		if o.Default != "" && !strings.Contains(use, "(default") {
			use += fmt.Sprintf(" (default %s)", o.Default)
		}
		fmt.Fprintf(tw, "  %s\t%s\n", left, use)
	}
	tw.Flush()
}

// ctrSummary finds the one-line description of "name" or of "group sub".
func ctrSummary(name string) string {
	if c, ok := commands[name]; ok {
		return c.summary
	}
	if group, sub, ok := strings.Cut(name, " "); ok {
		if s := ctrFindSub(group, sub); s != nil {
			return s.Summary()
		}
	}
	return ""
}

// ctrHelp implements `kido help [COMMAND [SUBCOMMAND]]` and `kido COMMAND --help`.
func ctrHelp(args []string) int {
	if len(args) == 0 {
		usage(os.Stdout)
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok || hiddenCommand(args[0]) {
		ctrUnknown("", args[0], ctrCommandNames())
		return 125
	}
	// Commands that build their own flag.FlagSet print their usage to stderr and may return a
	// non-zero status for -h; help is asked for, so it goes to stdout with status 0.
	stderr := os.Stderr
	os.Stderr = os.Stdout
	defer func() { os.Stderr = stderr }()
	cmd.run(context.Background(), append(args[1:], "--help"))
	return 0
}

// ctrUnknown reports an unknown command or subcommand with the closest known names.
func ctrUnknown(group, name string, known []string) {
	full, see := "kido", "kido --help"
	if group != "" {
		full, see = "kido "+group, "kido "+group+" --help"
	}
	fmt.Fprintf(os.Stderr, "kido: '%s' is not a %s command.\n", name, full)
	if s := ctrSuggest(name, known); len(s) > 0 {
		fmt.Fprintf(os.Stderr, "\nDid you mean this?\n")
		for _, n := range s {
			fmt.Fprintf(os.Stderr, "\t%s\n", n)
		}
		fmt.Fprintln(os.Stderr)
	}
	fmt.Fprintf(os.Stderr, "See '%s'.\n", see)
}

func ctrCommandNames() []string {
	var out []string
	for n := range commands {
		if !hiddenCommand(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// ctrSuggest returns the known names within a small edit distance of name, or starting with it.
func ctrSuggest(name string, known []string) []string {
	type cand struct {
		name string
		dist int
	}
	var cs []cand
	for _, k := range known {
		d := ctrDistance(strings.ToLower(name), k)
		switch {
		case d <= 2 && d < len(k):
			cs = append(cs, cand{k, d})
		case len(name) >= 2 && strings.HasPrefix(k, strings.ToLower(name)):
			cs = append(cs, cand{k, 3})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].dist < cs[j].dist })
	var out []string
	for _, c := range cs {
		out = append(out, c.name)
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// ctrDistance is the Damerau-Levenshtein distance (optimal string alignment) between a and b.
func ctrDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	d := make([][]int, len(ra)+1)
	for i := range d {
		d[i] = make([]int, len(rb)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(ra)][len(rb)]
}

// ctrCommandFlags returns the options of a command (or "group sub") by running it with --help in
// introspection mode: a ctrFlags command hands over its flag set before it does anything; other
// commands print Go's default usage, which is parsed instead. Output is discarded.
func ctrCommandFlags(run func(context.Context, []string) int, args []string) (opts []ctrOption) {
	capture, err := os.CreateTemp("", "kido-usage-")
	if err != nil {
		return nil
	}
	defer os.Remove(capture.Name())
	defer capture.Close()
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = capture, capture
	ctrIntrospecting = true
	defer func() {
		ctrIntrospecting = false
		os.Stdout, os.Stderr = stdout, stderr
		if r := recover(); r != nil {
			in, ok := r.(ctrIntrospection)
			if !ok {
				panic(r)
			}
			opts = ctrOptions(in.fs)
		}
	}()
	run(context.Background(), append(args, "--help"))
	capture.Seek(0, io.SeekStart)
	b, _ := io.ReadAll(io.LimitReader(capture, 1<<20))
	return ctrParseGoUsage(string(b))
}

// ctrParseGoUsage reads the options out of flag.PrintDefaults output.
func ctrParseGoUsage(s string) []ctrOption {
	var out []ctrOption
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "  -") {
			continue
		}
		head, _, _ := strings.Cut(line, "\t")
		f := strings.Fields(head)
		name := strings.TrimLeft(f[0], "-")
		if name == "" || name == "h" || name == "help" {
			continue
		}
		o := ctrOption{}
		if len(f) > 1 {
			o.Type = f[1]
		}
		if len(name) == 1 {
			o.Short = []string{name}
		} else {
			o.Long = []string{name}
		}
		out = append(out, o)
	}
	return out
}
