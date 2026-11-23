package build

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

type lookupFunc func(name string) (string, bool)

// expandWord substitutes $VAR, ${VAR}, ${VAR:-word}, ${VAR-word}, ${VAR:+word} and ${VAR+word},
// and removes quotes and backslash escapes, in the instructions that expand variables. Unset
// variables expand to the empty string.
func expandWord(word string, lookup lookupFunc) (string, error) {
	l := &lexer{src: []rune(word), lookup: lookup}
	return l.until(eof)
}

const eof = rune(-1)

type lexer struct {
	src    []rune
	pos    int
	lookup lookupFunc
}

func (l *lexer) peek() rune {
	if l.pos >= len(l.src) {
		return eof
	}
	return l.src[l.pos]
}

func (l *lexer) next() rune {
	r := l.peek()
	if r != eof {
		l.pos++
	}
	return r
}

// until expands up to stop, which it consumes; stop == eof reads the whole input.
func (l *lexer) until(stop rune) (string, error) {
	var b strings.Builder
	for {
		r := l.peek()
		switch {
		case r == eof:
			if stop != eof {
				return "", errors.New("syntax error: missing '}'")
			}
			return b.String(), nil
		case r == stop:
			l.next()
			return b.String(), nil
		case r == '\'':
			s, err := l.singleQuoted()
			if err != nil {
				return "", err
			}
			b.WriteString(s)
		case r == '"':
			s, err := l.doubleQuoted()
			if err != nil {
				return "", err
			}
			b.WriteString(s)
		case r == '$':
			s, err := l.dollar()
			if err != nil {
				return "", err
			}
			b.WriteString(s)
		case r == '\\':
			l.next()
			if c := l.next(); c != eof {
				b.WriteRune(c)
			}
		default:
			b.WriteRune(l.next())
		}
	}
}

func (l *lexer) singleQuoted() (string, error) {
	l.next()
	var b strings.Builder
	for {
		r := l.next()
		switch r {
		case eof:
			return "", errors.New("unexpected end of statement while looking for matching single-quote")
		case '\'':
			return b.String(), nil
		}
		b.WriteRune(r)
	}
}

func (l *lexer) doubleQuoted() (string, error) {
	l.next()
	var b strings.Builder
	for {
		switch r := l.peek(); r {
		case eof:
			return "", errors.New("unexpected end of statement while looking for matching double-quote")
		case '"':
			l.next()
			return b.String(), nil
		case '$':
			s, err := l.dollar()
			if err != nil {
				return "", err
			}
			b.WriteString(s)
		case '\\':
			l.next()
			switch c := l.peek(); c {
			case eof:
			case '"', '$', '\\':
				b.WriteRune(l.next())
			default:
				b.WriteRune('\\')
			}
		default:
			b.WriteRune(l.next())
		}
	}
}

func (l *lexer) name() string {
	var b strings.Builder
	if r := l.peek(); unicode.IsDigit(r) {
		for unicode.IsDigit(l.peek()) {
			b.WriteRune(l.next())
		}
		return b.String()
	} else if strings.ContainsRune("@*#?-$!0", r) && r != eof {
		return string(l.next())
	}
	for {
		r := l.peek()
		if r == eof || !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_') {
			return b.String()
		}
		b.WriteRune(l.next())
	}
}

func (l *lexer) dollar() (string, error) {
	l.next()
	if l.peek() != '{' {
		name := l.name()
		if name == "" {
			return "$", nil
		}
		v, _ := l.lookup(name)
		return v, nil
	}
	l.next()
	switch l.peek() {
	case eof:
		return "", errors.New("syntax error: missing '}'")
	case '{', '}', ':':
		return "", errors.New("syntax error: bad substitution")
	}
	name := l.name()
	if name == "" {
		return "", errors.New("syntax error: bad substitution")
	}
	r := l.next()
	switch r {
	case '}':
		v, _ := l.lookup(name)
		return v, nil
	case eof:
		return "", errors.New("syntax error: missing '}'")
	case ':', '-', '+':
	default:
		return "", fmt.Errorf("unsupported modifier (%c) in substitution", r)
	}
	colon := r == ':'
	mod := r
	if colon {
		mod = l.next()
	}
	if mod != '-' && mod != '+' {
		if mod == eof {
			return "", errors.New("syntax error: missing '}'")
		}
		return "", fmt.Errorf("unsupported modifier (%c) in substitution", mod)
	}
	word, err := l.until('}')
	if err != nil {
		return "", err
	}
	v, set := l.lookup(name)
	present := set && (!colon || v != "")
	if mod == '-' {
		if present {
			return v, nil
		}
		return word, nil
	}
	if present {
		return word, nil
	}
	return "", nil
}
