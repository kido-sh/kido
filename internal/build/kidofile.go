// Package build runs a Kidofile against an engine.Engine.
//
// Supported: multi-stage FROM [AS NAME], RUN, COPY [--from=STAGE|INDEX|IMAGE], ADD, ENV, ARG,
// WORKDIR, USER, CMD, ENTRYPOINT, LABEL and EXPOSE (recorded in the kido.expose label). ADD takes
// local sources only: URLs are refused and a local tar archive is copied as a file, not unpacked.
// Host COPY and ADD read from the build context only, and .kidoignore is honoured.
package build

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
)

// Instruction is one Kidofile instruction with its continuation lines joined.
type Instruction struct {
	Line     int      // 1-based line the instruction starts on
	Cmd      string   // upper-case keyword
	Flags    []string // leading --name[=value] words
	Args     string   // the rest of the instruction, trimmed
	Original string   // the instruction as written, continuation lines joined
}

var (
	continuationRE = regexp.MustCompile(`\\[ \t]*$`)
	directiveRE    = regexp.MustCompile(`^#\s*([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$`)
)

// Parse splits a Kidofile into instructions. It knows nothing about what each one means.
func Parse(r io.Reader) ([]Instruction, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	var out []Instruction
	lineNo := 0
	directives := true
	next := func() (string, bool) {
		if !sc.Scan() {
			return "", false
		}
		lineNo++
		l := strings.TrimRight(sc.Text(), "\r")
		if lineNo == 1 {
			l = strings.TrimPrefix(l, "\uFEFF")
		}
		return l, true
	}
	for {
		line, ok := next()
		if !ok {
			break
		}
		trimmed := strings.TrimLeft(line, " \t")
		if directives {
			// An unknown directive is a comment and, as in BuildKit, ends the directives.
			if m := directiveRE.FindStringSubmatch(trimmed); m != nil {
				switch strings.ToLower(m[1]) {
				case "escape":
					if m[2] != `\` {
						return nil, fmt.Errorf("line %d: escape=%s is not supported, only the default backslash", lineNo, m[2])
					}
					continue
				case "syntax", "check":
					continue
				}
			}
			directives = false
		}
		if trimmed == "" || trimmed[0] == '#' {
			continue
		}
		start := lineNo
		text, cont := trimContinuation(trimmed)
		var b strings.Builder
		b.WriteString(text)
		for cont {
			l, ok := next()
			if !ok {
				break
			}
			if t := strings.TrimLeft(l, " \t"); t == "" || t[0] == '#' {
				continue
			}
			text, cont = trimContinuation(l)
			b.WriteString(text)
		}
		inst, err := splitInstruction(b.String(), start)
		if err != nil {
			return nil, err
		}
		if inst.Cmd != "" {
			out = append(out, inst)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
	}
	return out, nil
}

func trimContinuation(s string) (string, bool) {
	if loc := continuationRE.FindStringIndex(s); loc != nil {
		return s[:loc[0]], true
	}
	return s, false
}

func splitInstruction(line string, lineNo int) (Instruction, error) {
	s := strings.TrimSpace(line)
	inst := Instruction{Line: lineNo, Original: s}
	if s == "" {
		return inst, nil
	}
	kw, rest := s, ""
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
		kw, rest = s[:i], s[i:]
	}
	inst.Cmd = strings.ToUpper(kw)
	inst.Flags, inst.Args = extractFlags(rest)
	return inst, nil
}

// extractFlags takes the leading words that start with "--"; a bare "--" ends them.
func extractFlags(s string) ([]string, string) {
	var flags []string
	for {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if !strings.HasPrefix(s, "--") {
			return flags, strings.TrimSpace(s)
		}
		word, rest := s, ""
		if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
			word, rest = s[:i], s[i:]
		}
		s = rest
		if word == "--" {
			return flags, strings.TrimSpace(s)
		}
		flags = append(flags, word)
	}
}

var errNotStringArray = errors.New("JSON arrays in a Kidofile must contain only strings")

// parseJSONArray reports whether s is the exec (JSON) form; a JSON array of non-strings is an error.
func parseJSONArray(s string) ([]string, bool, error) {
	if !strings.HasPrefix(s, "[") {
		return nil, false, nil
	}
	var raw []any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, false, nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		str, ok := v.(string)
		if !ok {
			return nil, false, errNotStringArray
		}
		out = append(out, str)
	}
	return out, true, nil
}

// splitWords splits on unquoted, unescaped whitespace, keeping quotes and escapes in the words
// (they are removed later by expandWord). This is how ENV, LABEL and ARG are split.
func splitWords(s string) []string {
	var words []string
	var word strings.Builder
	inWord, blankOK := false, false
	var quote rune
	rs := []rune(s)
	flush := func() {
		if blankOK || word.Len() > 0 {
			words = append(words, word.String())
		}
		word.Reset()
		inWord, blankOK = false, false
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == '\\' && quote != '\'' && i+1 < len(rs) {
				word.WriteRune(r)
				i++
				r = rs[i]
			} else if r == quote {
				quote = 0
			}
			word.WriteRune(r)
		case unicode.IsSpace(r):
			if inWord {
				flush()
			}
		default:
			inWord = true
			switch r {
			case '\'', '"':
				quote, blankOK = r, true
			case '\\':
				if i+1 == len(rs) {
					continue
				}
				word.WriteRune(r)
				i++
				r = rs[i]
			}
			word.WriteRune(r)
		}
	}
	if inWord {
		flush()
	}
	return words
}

type kv struct {
	key, value string
	hasValue   bool
}

// parseKeyValues parses ENV and LABEL arguments: "k=v k2=v2 ..." or the legacy "k value".
func parseKeyValues(cmd, args string) ([]kv, error) {
	words := splitWords(args)
	if len(words) == 0 {
		return nil, fmt.Errorf("%s requires at least one argument", cmd)
	}
	if !strings.Contains(words[0], "=") {
		parts := regexp.MustCompile(`\s+`).Split(args, 2)
		if len(parts) < 2 {
			return nil, fmt.Errorf("%s must have two arguments", cmd)
		}
		return []kv{{key: parts[0], value: parts[1], hasValue: true}}, nil
	}
	out := make([]kv, 0, len(words))
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if !ok {
			return nil, fmt.Errorf("syntax error: can't find = in %q; must be of the form name=value", w)
		}
		if k == "" {
			return nil, fmt.Errorf("%s: empty name in %q", cmd, w)
		}
		out = append(out, kv{key: k, value: v, hasValue: true})
	}
	return out, nil
}

var argNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseArgs parses ARG arguments: "name", "name=default", several per line.
func parseArgs(args string) ([]kv, error) {
	words := splitWords(args)
	if len(words) == 0 {
		return nil, errors.New("ARG requires at least one argument")
	}
	out := make([]kv, 0, len(words))
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if !argNameRE.MatchString(k) {
			return nil, fmt.Errorf("invalid ARG name %q", k)
		}
		out = append(out, kv{key: k, value: v, hasValue: ok})
	}
	return out, nil
}
