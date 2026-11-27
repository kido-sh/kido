package runtime

import (
	"reflect"
	"strings"
	"testing"
)

func TestDevShares(t *testing.T) {
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	got, err := devShares(" data=" + a + ":ro, ,rw=" + b + ":rw,plain=" + c)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--share-ro", "data=" + a, "--share", "rw=" + b, "--share", "plain=" + c}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("devShares = %q, want %q", got, want)
	}
	if got, err := devShares(""); got != nil || err != nil {
		t.Fatalf("devShares(\"\") = %q, %v", got, err)
	}
	long := strings.Repeat("\u4e2d", 86)
	for _, bad := range []string{"m0=" + a, "m12=" + a + ":ro", "noequals", "=" + a, "x=relative", "x=", "x=" + a + "/missing", "x=" + a + ",x=" + b, ".=" + a, "..=" + a, "a/b=" + a, long + "=" + a} {
		if _, err := devShares(bad); err == nil {
			t.Fatalf("devShares(%q): no error", bad)
		}
	}
	if _, err := devShares(strings.Repeat("\u4e2d", 85) + "=" + a); err != nil {
		t.Fatalf("a 255-byte name: %v", err)
	}
	if _, err := devShares("m=" + a + ",mx1=" + b); err != nil {
		t.Fatalf("names that only start with m: %v", err)
	}
}
