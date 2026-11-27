package build

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	kf := "# syntax=kido/kidofile:1\n" +
		"# a comment\n" +
		"from darwin AS base\n" +
		"\n" +
		"RUN echo one \\\n" +
		"  two \\  \n" +
		"# comment inside a continuation\n" +
		"\n" +
		"  three\n" +
		"COPY --chown=1:2 --chmod=755 a b /dst/\n" +
		"   # indented comment\n" +
		"CMD [\"/bin/hello\", \"world\"]\n" +
		"RUN echo trailing \\\n"
	insts, err := Parse(strings.NewReader(kf))
	if err != nil {
		t.Fatal(err)
	}
	want := []Instruction{
		{Line: 3, Cmd: "FROM", Args: "darwin AS base", Original: "from darwin AS base"},
		{Line: 5, Cmd: "RUN", Args: "echo one   two   three", Original: "RUN echo one   two   three"},
		{Line: 10, Cmd: "COPY", Flags: []string{"--chown=1:2", "--chmod=755"}, Args: "a b /dst/", Original: "COPY --chown=1:2 --chmod=755 a b /dst/"},
		{Line: 12, Cmd: "CMD", Args: `["/bin/hello", "world"]`, Original: `CMD ["/bin/hello", "world"]`},
		{Line: 13, Cmd: "RUN", Args: "echo trailing", Original: "RUN echo trailing"},
	}
	if !reflect.DeepEqual(insts, want) {
		t.Fatalf("got  %#v\nwant %#v", insts, want)
	}
}

func TestParseEscapeDirective(t *testing.T) {
	if _, err := Parse(strings.NewReader("# escape=`\nFROM darwin\n")); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("want an error on line 1, got %v", err)
	}
	if _, err := Parse(strings.NewReader("# escape=\\\nFROM darwin\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(strings.NewReader("# foo=bar\n# escape=`\nFROM darwin\n")); err != nil {
		t.Fatalf("an unknown directive ends the directives: %v", err)
	}
	insts, err := Parse(strings.NewReader("FROM darwin\n# escape=`\nRUN x\n"))
	if err != nil || len(insts) != 2 {
		t.Fatalf("a directive after the first instruction is a comment: %v %v", insts, err)
	}
}

func TestExtractFlags(t *testing.T) {
	flags, rest := extractFlags("  --a=1 --b  -- --c x y")
	if !reflect.DeepEqual(flags, []string{"--a=1", "--b"}) || rest != "--c x y" {
		t.Fatalf("got %q %q", flags, rest)
	}
}

func TestParseJSONArray(t *testing.T) {
	if v, ok, err := parseJSONArray(`["a", "b c"]`); err != nil || !ok || !reflect.DeepEqual(v, []string{"a", "b c"}) {
		t.Fatalf("got %q %v %v", v, ok, err)
	}
	if _, ok, err := parseJSONArray(`[ "a", 'b' ]`); err != nil || ok {
		t.Fatalf("invalid JSON must fall back to the shell form: %v %v", ok, err)
	}
	if _, _, err := parseJSONArray(`["a", 1]`); err == nil {
		t.Fatal("want an error for a non-string element")
	}
	if _, ok, _ := parseJSONArray(`echo [x]`); ok {
		t.Fatal("not JSON")
	}
}

func TestSplitWords(t *testing.T) {
	got := splitWords(`a=1 b="x y" c='p q' d=e\ f  g="h\"i"`)
	want := []string{`a=1`, `b="x y"`, `c='p q'`, `d=e\ f`, `g="h\"i"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestParseKeyValues(t *testing.T) {
	got, err := parseKeyValues("ENV", `A=1 B="two words"`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []kv{{"A", "1", true}, {"B", `"two words"`, true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	got, err = parseKeyValues("ENV", "PATH /usr/local/bin:/bin  and more")
	if err != nil {
		t.Fatal(err)
	}
	if want := []kv{{"PATH", "/usr/local/bin:/bin  and more", true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy form: got %v want %v", got, want)
	}
	for _, bad := range []string{"", "A", "A=1 B", "=x"} {
		if _, err := parseKeyValues("ENV", bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestParseArgs(t *testing.T) {
	got, err := parseArgs(`A B=2 C="x y"`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []kv{{"A", "", false}, {"B", "2", true}, {"C", `"x y"`, true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, err := parseArgs("1A"); err == nil {
		t.Fatal("want an error for an invalid name")
	}
}

func TestPlanErrors(t *testing.T) {
	for _, tc := range []struct{ df, want string }{
		{"FROM darwin\nFOO bar\n", "line 2: FOO: unknown instruction"},
		{"FROM darwin\nHEALTHCHECK NONE\n", "line 2: HEALTHCHECK: not supported"},
		{"RUN x\nFROM darwin\n", "line 1: RUN before FROM"},
		{"", "the Kidofile has no instructions"},
		{"ARG X\n", "the Kidofile has no FROM instruction"},
		{"FROM a\nFROM b\n", "line 2: FROM: multi-stage"},
		{"FROM darwin\nRUN --mount=type=cache x\n", "line 2: RUN: flag --mount is not supported"},
		{"FROM darwin\nCOPY --from=a x y\n", "--from is not supported"},
		{"FROM darwin\nCOPY x\n", "at least two arguments"},
		{"FROM darwin\nRUN <<EOF\n", "heredocs"},
		{"FROM darwin\nENV A=${B\n", "line 2: ENV: syntax error: missing '}'"},
		{"FROM darwin\nCOPY \"a b\" /x/\n", "matching double-quote"},
		{"FROM darwin\nWORKDIR ${A:?x}\n", "unsupported modifier"},
		{"FROM darwin\nCMD [\"a\", 1]\n", "JSON arrays in a Kidofile must contain only strings"},
		{"FROM a b\n", "want FROM IMAGE"},
		{"FROM darwin\nEXPOSE\n", "at least one argument"},
	} {
		insts, err := Parse(strings.NewReader(tc.df))
		if err == nil {
			_, err = plan(insts)
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.df, err, tc.want)
		}
	}
}
