package main

import (
	"strings"
	"testing"
)

func TestCtrMemoryFlag(t *testing.T) {
	good := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"64", 64},
		{"512m", 512},
		{"512M", 512},
		{"4g", 4096},
		{"4G", 4096},
	}
	for _, tc := range good {
		got, err := ctrMemoryFlag("--gpu-memory", tc.in)
		if err != nil {
			t.Errorf("ctrMemoryFlag(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ctrMemoryFlag(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"0",
		"-1",
		"-4g",
		"g",
		"m",
		"abc",
		"4tb",
		"18014398509481984g",
		"9007199254740992g",
	}
	for _, in := range bad {
		_, err := ctrMemoryFlag("--gpu-memory", in)
		if err == nil {
			t.Errorf("ctrMemoryFlag(%q) succeeded, want error", in)
			continue
		}
		if !strings.Contains(err.Error(), in) {
			t.Errorf("ctrMemoryFlag(%q) error %q does not include original input", in, err.Error())
		}
	}
}
