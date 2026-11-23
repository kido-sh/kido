package runtime

import (
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"plain text\n\tok":       "plain text\n\tok",
		"\x1b]52;c;cHduZWQ=\x07": `\x1b]52;c;cHduZWQ=\x07`,
		"red \x1b[31mX\r":        `red \x1b[31mX\x0d`,
		"c1 \u009b31m":           `c1 \u009b31m`,
		"raw \x9b byte":          `raw \x9b byte`,
		"del \x7f":               `del \x7f`,
		"unicode é 日本":           "unicode é 日本",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if ok := "U+FFFD " + string(rune(0xfffd)); Sanitize(ok) != ok {
		t.Errorf("Sanitize(%q) = %q", ok, Sanitize(ok))
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 8}
	b.Write([]byte("abc"))
	b.Write([]byte("defgh"))
	b.Write([]byte("ij"))
	if b.String() != "cdefghij" {
		t.Fatalf("got %q", b.String())
	}
	b.Write([]byte(strings.Repeat("x", 20) + "12345678"))
	if b.String() != "12345678" || cap(b.buf) > 64 {
		t.Fatalf("got %q (cap %d)", b.String(), cap(b.buf))
	}
}
