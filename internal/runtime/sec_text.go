package runtime

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Sanitize makes text that came from a guest, an image or a registry safe to print on a terminal:
// C0 and C1 control characters other than newline and tab, DEL, and bytes that are not UTF-8
// become Go escapes. Never use it on a process's own output stream.
func Sanitize(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, unsafeRune) < 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unsafeRune(r) && r < 0x80:
			fmt.Fprintf(&b, `\x%02x`, r)
		case unsafeRune(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func unsafeRune(r rune) bool {
	return r < 0x20 && r != '\n' && r != '\t' || r == 0x7f || r >= 0x80 && r < 0xa0
}

// tailBuffer keeps only the last max bytes written to it, so that a guest cannot grow host memory.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
