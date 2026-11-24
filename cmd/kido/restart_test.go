package main

import (
	"testing"
	"time"
)

func TestCtrRestartPolicy(t *testing.T) {
	for in, want := range map[string]string{
		"no": "", "always": "always", "unless-stopped": "unless-stopped",
		"on-failure": "on-failure", "on-failure:3": "on-failure:3", "on-failure:03": "on-failure:3",
	} {
		if got, err := ctrRestartPolicy(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"sometimes", "always:2", "on-failure:-1", "on-failure:x", "no:1"} {
		if _, err := ctrRestartPolicy(in); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}

func TestCtrShouldRestart(t *testing.T) {
	for _, c := range []struct {
		policy           string
		status, restarts int
		want             bool
	}{
		{"", 1, 0, false},
		{"always", 0, 9, true},
		{"unless-stopped", 0, 0, true},
		{"on-failure", 0, 0, false},
		{"on-failure", 2, 100, true},
		{"on-failure:2", 1, 1, true},
		{"on-failure:2", 1, 2, false},
	} {
		if got := ctrShouldRestart(c.policy, c.status, c.restarts); got != c.want {
			t.Errorf("%q status %d after %d: got %v", c.policy, c.status, c.restarts, got)
		}
	}
}

func TestCtrRestartDelay(t *testing.T) {
	d := time.Duration(0)
	var got []time.Duration
	for range 8 {
		d = ctrRestartDelay(d, time.Second)
		got = append(got, d)
	}
	if got[0] != time.Second || got[1] != 2*time.Second || got[6] != time.Minute || got[7] != time.Minute {
		t.Errorf("delays %v", got)
	}
	if d := ctrRestartDelay(time.Minute, 10*time.Second); d != time.Second {
		t.Errorf("after a long run: %v", d)
	}
}
