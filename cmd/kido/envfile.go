package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ctrReadEnvFiles reads --env-file files in order, in Docker's format (see ctrParseEnvFile).
func ctrReadEnvFiles(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		env, err := ctrParseEnvFile(f, os.LookupEnv)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("env file %s: %w", p, err)
		}
		out = append(out, env...)
	}
	return out, nil
}

// ctrParseEnvFile parses Docker's env file format: one KEY=VALUE per line, the value taken as is
// (no quotes are removed); a bare KEY takes the host's value and is dropped when the host has none;
// blank lines and lines whose first non-blank character is # are skipped.
func ctrParseEnvFile(r io.Reader, lookup func(string) (string, bool)) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var out []string
	for n := 1; sc.Scan(); n++ {
		b := sc.Bytes()
		if n == 1 {
			b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
		}
		if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
			return nil, fmt.Errorf("line %d: invalid UTF-8 or NUL byte", n)
		}
		line := strings.TrimLeftFunc(string(b), unicode.IsSpace)
		if line == "" || line[0] == '#' {
			continue
		}
		key, value, hasValue := strings.Cut(line, "=")
		if key == "" {
			return nil, fmt.Errorf("line %d: no variable name", n)
		}
		if strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("line %d: variable %q contains whitespace", n, key)
		}
		if hasValue {
			out = append(out, key+"="+value)
		} else if v, ok := lookup(key); ok {
			out = append(out, key+"="+v)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
