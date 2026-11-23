package main

import (
	"context"
	"fmt"
	"strings"
)

func init() {
	register("completion", "Generate the autocompletion script for bash, zsh or fish", ctrCompletionCmd)
}

func ctrCompletionCmd(ctx context.Context, args []string) int {
	fs := ctrFlags("completion", "bash|zsh|fish\n\n"+
		"  bash:  eval \"$(kido completion bash)\"    (macOS's bash 3.2 cannot source <(...))\n"+
		"  zsh:   source <(kido completion zsh)      (e.g. in ~/.zshrc)\n"+
		"  fish:  kido completion fish > ~/.config/fish/completions/kido.fish")
	if err := fs.Parse(args); err != nil {
		return 125
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 125
	}
	s, err := ctrCompletionScript(fs.Arg(0), ctrCompletionSpecs())
	if err != nil {
		return ctrFail(err)
	}
	fmt.Print(s)
	return 0
}

func ctrCompletionScript(shell string, specs []ctrCompletion) (string, error) {
	switch shell {
	case "bash":
		return ctrBashCompletion(specs), nil
	case "zsh":
		return "# zsh completion for kido: source <(kido completion zsh)\n" +
			"(( $+functions[compdef] )) || { autoload -Uz compinit && compinit }\n" +
			"autoload -U +X bashcompinit && bashcompinit\n" + ctrBashCompletion(specs), nil
	case "fish":
		return ctrFishCompletion(specs), nil
	}
	return "", fmt.Errorf("completion: unsupported shell %q (bash, zsh or fish)", shell)
}

func ctrNames(cs []ctrCompletion) string {
	var n []string
	for _, c := range cs {
		n = append(n, c.Name)
	}
	return strings.Join(n, " ")
}

// ctrOptWords returns every option name and the ones that take a value.
func ctrOptWords(opts []ctrOption) (all, valued string) {
	var a, v []string
	for _, o := range opts {
		a = append(a, o.Names()...)
		if o.Type != "" {
			v = append(v, o.Names()...)
		}
	}
	return strings.Join(a, " "), strings.Join(v, " ")
}

func ctrBashSpec(b *strings.Builder, key string, c ctrCompletion) {
	all, valued := ctrOptWords(c.Options)
	fmt.Fprintf(b, "    %s) opts='%s' vopts='%s' args='%s' subs='%s' ;;\n", key, all, valued, c.Args, ctrNames(c.Subs))
}

func ctrBashCompletion(specs []ctrCompletion) string {
	var b strings.Builder
	b.WriteString(`# bash completion for kido: eval "$(kido completion bash)"
_kido_spec() {
  opts= vopts= args= subs=
  case "$1/$2" in
`)
	for _, c := range specs {
		ctrBashSpec(&b, c.Name+"/", c)
		for _, s := range c.Subs {
			ctrBashSpec(&b, c.Name+"/"+s.Name, s)
		}
	}
	fmt.Fprintf(&b, `  esac
}
_kido() {
  local kido=${COMP_WORDS[0]} cur=${COMP_WORDS[COMP_CWORD]} prev=${COMP_WORDS[COMP_CWORD-1]}
  local cmd= sub= npos=0 skip=0 i w opts vopts args subs
  local word=${COMP_LINE:0:COMP_POINT}
  word=${word##* }
  for ((i = 1; i < COMP_CWORD; i++)); do
    w=${COMP_WORDS[i]}
    if [ $skip = 1 ]; then skip=0; continue; fi
    case $w in
    :) continue ;;
    -*) case " $vopts " in *" $w "*) skip=1 ;; esac; continue ;;
    esac
    [ "${COMP_WORDS[i-1]}" = : ] && continue
    if [ -z "$cmd" ]; then cmd=$w; _kido_spec "$cmd" ""
    elif [ -n "$subs" ] && [ -z "$sub" ]; then sub=$w; _kido_spec "$cmd" "$sub"
    else npos=$((npos + 1)); fi
  done
  if [ "$cur" = : ] || [ "$prev" = : ]; then npos=$((npos - 1)); fi
  COMPREPLY=()
  if [ -z "$cmd" ]; then
    COMPREPLY=($(compgen -W '%s' -- "$cur"))
    return
  fi
  if [ $skip = 1 ]; then
    case "$prev" in
    -s|--signal) [ "$cmd$sub" = kill ] || [ "$cmd$sub" = containerkill ] && COMPREPLY=($(compgen -W 'KILL TERM INT HUP QUIT USR1 USR2' -- "$cur")) ;;
    --type) COMPREPLY=($(compgen -W 'container image' -- "$cur")) ;;
    esac
    return
  fi
  if [ -n "$subs" ] && [ -z "$sub" ]; then
    COMPREPLY=($(compgen -W "$subs" -- "$cur"))
    return
  fi
  case $cur in -*) COMPREPLY=($(compgen -W "$opts" -- "$cur")); return ;; esac
  case "$cmd$sub" in
  completion) COMPREPLY=($(compgen -W 'bash zsh fish' -- "$cur")); return ;;
  run|create|exec|containerrun|containercreate|containerexec) [ $npos -ge 1 ] && return ;;
  esac
  local names=
  case $args in container|both) names=$("$kido" ps -a --format '{{.Names}}' 2>/dev/null) ;; esac
  case $args in image|both) names="$names $("$kido" images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null)" ;; esac
  case $args in network) names=$("$kido" network ls --format '{{.Name}}' 2>/dev/null) ;; esac
  [ -n "$args" ] || return
  COMPREPLY=($(compgen -W "$names" -- "$word"))
  if [[ $word == *:* && $COMP_WORDBREAKS == *:* ]]; then
    local pre=${word%%"${word##*:}"}
    COMPREPLY=("${COMPREPLY[@]#"$pre"}")
  fi
}
complete -o default -F _kido kido
`, ctrNames(specs))
	return b.String()
}

func ctrFishQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func ctrFishOptions(b *strings.Builder, cond string, opts []ctrOption) {
	for _, o := range opts {
		fmt.Fprintf(b, "complete -c kido -n %s", ctrFishQuote(cond))
		for _, s := range o.Short {
			fmt.Fprintf(b, " -s %s", s)
		}
		for _, l := range o.Long {
			fmt.Fprintf(b, " -l %s", l)
		}
		if o.Type != "" {
			b.WriteString(" -r")
		}
		fmt.Fprintf(b, " -d %s\n", ctrFishQuote(ctrCapitalize(o.Usage)))
	}
}

func ctrFishArgs(b *strings.Builder, cond, args string) {
	if args == "container" || args == "both" {
		fmt.Fprintf(b, "complete -c kido -n %s -f -a %s\n", ctrFishQuote(cond), ctrFishQuote(`(kido ps -a --format "{{.Names}}" 2>/dev/null)`))
	}
	if args == "image" || args == "both" {
		fmt.Fprintf(b, "complete -c kido -n %s -f -a %s\n", ctrFishQuote(cond), ctrFishQuote(`(kido images --format "{{.Repository}}:{{.Tag}}" 2>/dev/null)`))
	}
	if args == "network" {
		fmt.Fprintf(b, "complete -c kido -n %s -f -a %s\n", ctrFishQuote(cond), ctrFishQuote(`(kido network ls --format "{{.Name}}" 2>/dev/null)`))
	}
}

func ctrFishCompletion(specs []ctrCompletion) string {
	var b strings.Builder
	b.WriteString("# fish completion for kido: kido completion fish > ~/.config/fish/completions/kido.fish\n")
	for _, c := range specs {
		fmt.Fprintf(&b, "complete -c kido -n __fish_use_subcommand -f -a %s -d %s\n", c.Name, ctrFishQuote(c.Summary))
	}
	for _, c := range specs {
		cond := "__fish_seen_subcommand_from " + c.Name
		if len(c.Subs) > 0 {
			subs := ctrNames(c.Subs)
			for _, s := range c.Subs {
				fmt.Fprintf(&b, "complete -c kido -n %s -f -a %s -d %s\n",
					ctrFishQuote(cond+"; and not __fish_seen_subcommand_from "+subs), s.Name, ctrFishQuote(s.Summary))
				sc := cond + "; and __fish_seen_subcommand_from " + s.Name
				ctrFishOptions(&b, sc, s.Options)
				ctrFishArgs(&b, sc, s.Args)
			}
			continue
		}
		ctrFishOptions(&b, cond, c.Options)
		ctrFishArgs(&b, cond, c.Args)
		if c.Name == "completion" {
			fmt.Fprintf(&b, "complete -c kido -n %s -f -a 'bash zsh fish'\n", ctrFishQuote(cond))
		}
	}
	return b.String()
}
