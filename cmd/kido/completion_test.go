package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionBash(t *testing.T) {
	dir := t.TempDir()
	script, err := ctrCompletionScript("bash", ctrCompletionSpecs())
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "c.bash"), []byte(script), 0o600)
	fake := "#!/bin/sh\ncase $1 in ps) printf 'web\\ndb\\n' ;; images) printf 'darwin:latest\\nalpine:3\\n' ;; esac\n"
	os.WriteFile(filepath.Join(dir, "kido"), []byte(fake), 0o700)
	cases := map[string]string{
		"kido st":           "start stop",
		"kido image l":      "load ls",
		"kido run --na":     "--name",
		"kido build --tar":  "--target",
		"kido run --name ":  "",
		"kido stop ":        "web db",
		"kido rmi ":         "darwin:latest alpine:3",
		"kido rmi darwin:l": "latest",
		"kido kill -s T":    "TERM",
		"kido completion ":  "bash zsh fish",
		"kido run darwin ":  "",
	}
	for line, want := range cases {
		cmd := exec.Command("/bin/bash", "--norc", "--noprofile", "-c", `source "$1/c.bash"
COMP_LINE=$2; COMP_POINT=${#COMP_LINE}
read -ra COMP_WORDS <<<"$COMP_LINE"
[[ $COMP_LINE == *" " ]] && COMP_WORDS+=("")
if [[ $COMP_LINE == *:* ]]; then w=${COMP_WORDS[${#COMP_WORDS[@]}-1]}; unset 'COMP_WORDS[${#COMP_WORDS[@]}-1]'; COMP_WORDS+=("${w%%:*}" ":" "${w#*:}"); fi
COMP_CWORD=$((${#COMP_WORDS[@]} - 1))
_kido; echo "${COMPREPLY[*]}"`, "bash", dir, line)
		cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%q: %v: %s", line, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Errorf("complete %q = %q, want %q", line, got, want)
		}
	}
}

func TestCompletionScriptsParse(t *testing.T) {
	dir := t.TempDir()
	for shell, check := range map[string][]string{"bash": {"/bin/bash", "-n"}, "zsh": {"/bin/zsh", "-n"}} {
		t.Run(shell, func(t *testing.T) {
			s, err := ctrCompletionScript(shell, ctrCompletionSpecs())
			if err != nil {
				t.Fatal(err)
			}
			f := filepath.Join(dir, shell)
			if err := os.WriteFile(f, []byte(s), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(check[0]); err != nil {
				t.Skipf("%s not installed", check[0])
			}
			if out, err := exec.Command(check[0], check[1], f).CombinedOutput(); err != nil {
				t.Errorf("%s -n: %v: %s", shell, err, out)
			}
		})
	}
	fish, _ := ctrCompletionScript("fish", ctrCompletionSpecs())
	for _, want := range []string{
		"complete -c kido -n __fish_use_subcommand -f -a run -d",
		"complete -c kido -n '__fish_seen_subcommand_from run' -s d -l detach -d",
		`complete -c kido -n '__fish_seen_subcommand_from stop' -f -a '(kido ps -a --format "{{.Names}}" 2>/dev/null)'`,
	} {
		if !strings.Contains(fish, want) {
			t.Errorf("fish script lacks %q", want)
		}
	}
	if _, err := ctrCompletionScript("tcsh", nil); err == nil {
		t.Error("tcsh accepted")
	}
}
