package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitReportsExitOfAutoRemovedContainer(t *testing.T) {
	r := lcRuntime(t)
	c := lcContainer(t, r, "gone", func(i *Info) {
		i.AutoRemove = true
		i.PID, i.PIDStart = os.Getpid(), hostProcStart(os.Getpid())
		i.StartedAt = time.Now()
	})
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		s, err := r.Wait(context.Background(), c.ID)
		done <- result{s, err}
	}()
	time.Sleep(120 * time.Millisecond)
	if err := r.RecordExit(c.ID, os.Getpid(), 7, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(r.dir(c.ID)); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.err != nil || res.status != 7 {
			t.Fatalf("Wait = %d, %v; want 7", res.status, res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return")
	}
	if s, ok := r.removedExit(c.ID); !ok || s != 7 {
		t.Fatalf("removedExit = %d, %v; want 7", s, ok)
	}
}

func TestRemovedExitExpires(t *testing.T) {
	r := lcRuntime(t)
	if err := r.recordRemovedExit("0123456789ab", 3); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * removedExitTTL)
	path := filepath.Join(r.exitsDir(), "0123456789ab")
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.removedExit("0123456789ab"); ok {
		t.Fatal("an expired exit status was reported")
	}
	if err := r.recordRemovedExit("ba9876543210", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired record not pruned: %v", err)
	}
	if _, ok := r.removedExit("../escape"); ok {
		t.Fatal("a bad id was accepted")
	}
}
