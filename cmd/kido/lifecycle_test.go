package main

import (
	"bytes"
	"flag"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/runtime"
)

func TestSuggest(t *testing.T) {
	known := []string{"start", "stop", "images", "image", "inspect", "ps", "rm", "rmi"}
	for in, want := range map[string]string{"statr": "start", "imgaes": "images", "isnpect": "inspect", "rmii": "rmi"} {
		if got := ctrSuggest(in, known); len(got) == 0 || got[0] != want {
			t.Errorf("ctrSuggest(%q) = %v, want %s first", in, got, want)
		}
	}
	if got := ctrSuggest("zzzzzz", known); len(got) != 0 {
		t.Errorf("ctrSuggest(zzzzzz) = %v, want none", got)
	}
}

func TestOptionsGroupAliases(t *testing.T) {
	fs := ctrFlags("x", "")
	d := fs.Bool("d", false, "run in the background")
	fs.BoolVar(d, "detach", false, "run in the background")
	var env ctrList
	fs.Var(&env, "e", "same as --env")
	fs.Var(&env, "env", "set a variable")
	fs.Int("time", 10, "seconds")
	opts := ctrOptions(fs)
	if len(opts) != 3 {
		t.Fatalf("got %d options, want 3: %+v", len(opts), opts)
	}
	byName := map[string]ctrOption{}
	for _, o := range opts {
		byName[strings.Join(o.Names(), ",")] = o
	}
	if o, ok := byName["-d,--detach"]; !ok || o.Type != "" {
		t.Errorf("detach option: %+v", opts)
	}
	if o, ok := byName["-e,--env"]; !ok || o.Type != "list" || o.Usage != "set a variable" {
		t.Errorf("env option: %+v", o)
	}
	if o := byName["--time"]; o.Type != "int" || o.Default != "10" {
		t.Errorf("time option: %+v", o)
	}
}

func TestParseGoUsage(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.Bool("q", false, "quiet")
	fs.String("tag", "", "image tag")
	fs.PrintDefaults()
	got := ctrParseGoUsage(buf.String())
	if len(got) != 2 || got[0].Short[0] != "q" || got[0].Type != "" || got[1].Long[0] != "tag" || got[1].Type != "string" {
		t.Fatalf("ctrParseGoUsage = %+v", got)
	}
}

func TestInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	out := fs.String("o", "", "")
	quiet := fs.Bool("q", false, "")
	if err := fs.Parse(ctrInterspersed(fs, []string{"img1", "-o", "f.tar", "img2", "-q", "--", "-dash"})); err != nil {
		t.Fatal(err)
	}
	if *out != "f.tar" || !*quiet || !slices.Equal(fs.Args(), []string{"img1", "img2", "-dash"}) {
		t.Fatalf("o=%q q=%v args=%v", *out, *quiet, fs.Args())
	}
}

func TestMainArgs(t *testing.T) {
	cfg := engine.ImageConfig{Entrypoint: []string{"/bin/adb"}, Cmd: []string{"version"}}
	unset := &ctrOptString{}
	cases := []struct {
		ep   *ctrOptString
		args []string
		want []string
	}{
		{unset, nil, []string{"/bin/adb", "version"}},
		{unset, []string{"devices"}, []string{"/bin/adb", "devices"}},
		{&ctrOptString{value: "/bin/sh", set: true}, nil, []string{"/bin/sh"}},
		{&ctrOptString{value: "/bin/sh", set: true}, []string{"-c", "true"}, []string{"/bin/sh", "-c", "true"}},
		{&ctrOptString{set: true}, []string{"/bin/echo", "hi"}, []string{"/bin/echo", "hi"}},
		{&ctrOptString{set: true}, nil, nil},
	}
	for _, c := range cases {
		if got := ctrMainArgs(cfg, c.ep, c.args); !slices.Equal(got, c.want) {
			t.Errorf("ctrMainArgs(%+v, %v) = %v, want %v", *c.ep, c.args, got, c.want)
		}
	}
}

func TestSignal(t *testing.T) {
	for in, want := range map[string]syscall.Signal{"KILL": 9, "sigterm": 15, "9": 9, "usr1": syscall.SIGUSR1, "HUP": 1} {
		if got, err := ctrSignal(in); err != nil || got != want {
			t.Errorf("ctrSignal(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"BOGUS", "0", "64", ""} {
		if _, err := ctrSignal(in); err == nil {
			t.Errorf("ctrSignal(%q) accepted", in)
		}
	}
}

func TestFormatter(t *testing.T) {
	row := ctrPsRow{ID: "abc", Names: "web", Status: "Up"}
	var buf bytes.Buffer
	f, err := ctrNewFormatter(&buf, `table {{.ID}}\t{{.Names}}`, ctrHeader(ctrPsRow{}))
	if err != nil {
		t.Fatal(err)
	}
	f.Row(row)
	f.Flush()
	if got := buf.String(); !strings.HasPrefix(got, "CONTAINER ID") || !strings.Contains(got, "abc") || !strings.Contains(got, "NAMES") {
		t.Errorf("table output %q", got)
	}
	buf.Reset()
	f, _ = ctrNewFormatter(&buf, "json", nil)
	f.Row(row)
	if !strings.Contains(buf.String(), `"Names":"web"`) {
		t.Errorf("json output %q", buf.String())
	}
	buf.Reset()
	f, _ = ctrNewFormatter(&buf, `{{.Names}}:{{upper .Status}}`, nil)
	f.Row(row)
	if buf.String() != "web:UP\n" {
		t.Errorf("template output %q", buf.String())
	}
	if _, err := ctrNewFormatter(&buf, "{{.Nope", nil); err == nil {
		t.Error("bad template accepted")
	}
}

func TestPsFilter(t *testing.T) {
	code := 3
	i := &runtime.Info{ID: "0123456789ab", Name: "web-1", Image: "darwin:latest", State: engine.StateStopped,
		ExitCode: &code, Labels: map[string]string{"tier": "front"}}
	for f, want := range map[string]bool{
		"id=0123": true, "name=web": true, "name=web-*": true, "name=db": false, "status=exited": true,
		"status=running": false, "label=tier": true, "label=tier=front": true, "label=tier=back": false,
		"ancestor=darwin": true, "exited=3": true, "exited=0": false,
	} {
		if got, err := ctrPsFilter(i, f); err != nil || got != want {
			t.Errorf("filter %s = %v, %v; want %v", f, got, err, want)
		}
	}
	if _, err := ctrPsFilter(i, "volume=x"); err == nil {
		t.Error("unknown filter accepted")
	}
}

func TestImageIDSharedByTags(t *testing.T) {
	created := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	a := &engine.Image{Name: "darwin", Tag: "latest", Created: created, RootUUID: "c42a6242-de93-4b00-9b82-af06e5178cf5"}
	b := *a
	b.Name, b.Tag = "other/darwin", "v1"
	if ctrImageID(a) != ctrImageID(&b) {
		t.Error("two tags of one image have different IDs")
	}
	b.Config.Cmd = []string{"/bin/sh"}
	if ctrImageID(a) == ctrImageID(&b) {
		t.Error("a different config kept the same ID")
	}
}
