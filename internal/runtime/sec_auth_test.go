package runtime

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestWaitListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := waitListener(context.Background(), os.Getpid(), port, nil); err != nil {
		t.Fatalf("own listener: %v", err)
	}

	// A "VM process" that never binds the port while another process (this test) holds it.
	vm := exec.Command("/bin/sleep", "0.3")
	if err := vm.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { vm.Wait(); close(exited) }()
	err = waitListener(context.Background(), vm.Process.Pid, port, exited)
	if err == nil || !strings.Contains(err.Error(), "another process may have taken that port") {
		t.Fatalf("squatted port accepted: %v", err)
	}
}
