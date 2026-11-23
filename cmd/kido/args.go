package main

import (
	"fmt"
	"io"
	"strings"
)

// ctrArgCounts reads how many positional arguments the first line of a usage string takes: words
// outside brackets are required, each bracketed group is optional, and "..." or a group of
// options for another program ([ps OPTIONS]) makes the count open ended (hi < 0). [OPTIONS] is not
// an argument.
func ctrArgCounts(usage string) (lo, hi int) {
	line, _, _ := strings.Cut(usage, "\n")
	optional, variadic, depth := 0, false, 0
	for _, w := range strings.Fields(line) {
		if strings.Contains(w, "...") || w != "[OPTIONS]" && strings.Contains(w, "OPTIONS") {
			variadic = true
		}
		start := depth
		depth += strings.Count(w, "[") - strings.Count(w, "]")
		switch {
		case start > 0 || w == "[OPTIONS]":
		case strings.HasPrefix(w, "["):
			optional++
		default:
			lo++
		}
	}
	if variadic {
		return lo, -1
	}
	return lo, lo + optional
}

func ctrPlural(n int) string {
	if n == 1 {
		return "argument"
	}
	return "arguments"
}

// ctrArgsMessage is the complaint about n positional arguments for usage, or "" when n fits.
func ctrArgsMessage(usage string, n int) string {
	lo, hi := ctrArgCounts(usage)
	if n >= lo && (hi < 0 || n <= hi) {
		return ""
	}
	switch {
	case hi == 0:
		return "accepts no arguments"
	case lo == hi:
		return fmt.Sprintf("requires exactly %d %s", lo, ctrPlural(lo))
	case hi < 0:
		return fmt.Sprintf("requires at least %d %s", lo, ctrPlural(lo))
	case lo == 0:
		return fmt.Sprintf("requires at most %d %s", hi, ctrPlural(hi))
	}
	return fmt.Sprintf("requires at least %d and at most %d %s", lo, hi, ctrPlural(hi))
}

// ctrArgsError prints the short error for a wrong number of arguments and reports whether it
// did; otherwise the caller prints the whole usage.
func ctrArgsError(w io.Writer, name, usage string, n int) bool {
	msg := ctrArgsMessage(usage, n)
	if msg == "" {
		return false
	}
	line, _, _ := strings.Cut(usage, "\n")
	fmt.Fprintf(w, "\"kido %s\" %s.\nSee 'kido %s --help'.\n\nUsage:  kido %s %s\n", name, msg, name, name, line)
	if s := ctrSummary(name); s != "" {
		fmt.Fprintf(w, "\n%s\n", s)
	}
	return true
}
