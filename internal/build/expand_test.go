package build

import (
	"strings"
	"testing"
)

func TestExpandWord(t *testing.T) {
	vars := map[string]string{"A": "a", "E": "", "P": "/usr/bin"}
	lookup := func(n string) (string, bool) { v, ok := vars[n]; return v, ok }
	for _, tc := range []struct{ in, want string }{
		{"$A", "a"},
		{"${A}", "a"},
		{"x${A}y", "xay"},
		{"$A.$A", "a.a"},
		{"$U", ""},
		{"${U:-def}", "def"},
		{"${E:-def}", "def"},
		{"${E-def}", ""},
		{"${U-def}", "def"},
		{"${A:-def}", "a"},
		{"${A:+alt}", "alt"},
		{"${E:+alt}", ""},
		{"${E+alt}", "alt"},
		{"${U+alt}", ""},
		{"${U:-$P/x}", "/usr/bin/x"},
		{"${U:-\"a b\"}", "a b"},
		{"'$A'", "$A"},
		{`"$A b"`, "a b"},
		{`\$A`, "$A"},
		{`"a\"b\\c\d"`, `a"b\c\d`},
		{`a\ b`, "a b"},
		{"$", "$"},
		{"a$", "a$"},
		{"$1", ""},
		{"${A}}", "a}"},
		{`trailing\`, "trailing"},
	} {
		got, err := expandWord(tc.in, lookup)
		if err != nil || got != tc.want {
			t.Errorf("expandWord(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"${A", "${}", "${:-x}", "${A:?x}", "${A#x}", "'open", `"open`, "${A:-x"} {
		if got, err := expandWord(bad, lookup); err == nil {
			t.Errorf("expandWord(%q) = %q, want an error", bad, got)
		} else if strings.Contains(err.Error(), "%!") {
			t.Errorf("expandWord(%q): malformed error %v", bad, err)
		}
	}
}
