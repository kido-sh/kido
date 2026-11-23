package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/proto"
)

// startHostAgent runs the kido-agent binary named by KIDO_TEST_AGENT on the host. Never send it
// "shutdown": on the host that would try to halt the machine.
func startHostAgent(t *testing.T) (int, *agentCred) {
	bin := os.Getenv("KIDO_TEST_AGENT")
	if bin == "" {
		t.Skip("KIDO_TEST_AGENT not set")
	}
	port, err := freePort("127.0.0.1", "tcp")
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--listen", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), "--key", proto.EncodeKey(pub),
		"--secret", proto.EncodeSecret(proto.BootSecret(key)))
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	cred := &agentCred{key: key}
	if err := waitReady(context.Background(), port, cred, make(chan struct{})); err != nil {
		t.Fatal(err)
	}
	return port, cred
}

func TestExecHostAgent(t *testing.T) {
	port, cred := startHostAgent(t)
	ctx := context.Background()
	run := func(p engine.ProcessSpec, stdin io.Reader) (int, string, string, error) {
		var out, errb bytes.Buffer
		status, err := execAt(ctx, port, cred, p, engine.Stdio{Stdin: stdin, Stdout: &out, Stderr: &errb}, nil)
		return status, out.String(), errb.String(), err
	}

	_, other, _ := ed25519.GenerateKey(nil)
	if err := ping(ctx, port, &agentCred{key: other, agent: cred.agent}, time.Second); err == nil {
		t.Fatal("ping with a wrong key succeeded")
	}
	if status, out, _, err := run(engine.ProcessSpec{Args: []string{"/bin/echo", "hi"}}, nil); err != nil || status != 0 || out != "hi\n" {
		t.Fatalf("echo: %d %q %v", status, out, err)
	}
	if status, _, errOut, err := run(engine.ProcessSpec{Args: []string{"/bin/sh", "-c", "echo oops >&2; exit 3"}}, nil); err != nil || status != 3 || errOut != "oops\n" {
		t.Fatalf("exit 3: %d %q %v", status, errOut, err)
	}
	if status, _, _, err := run(engine.ProcessSpec{Args: []string{"/nonexistent"}}, nil); status != 127 || err == nil {
		t.Fatalf("not found: %d %v", status, err)
	}
	if status, _, _, err := run(engine.ProcessSpec{Args: []string{"/etc/hosts"}}, nil); status != 126 || err == nil {
		t.Fatalf("not executable: %d %v", status, err)
	}
	if status, out, _, err := run(engine.ProcessSpec{Args: []string{"/bin/cat"}}, strings.NewReader("in\n")); err != nil || status != 0 || out != "in\n" {
		t.Fatalf("stdin: %d %q %v", status, out, err)
	}
	if status, out, _, err := run(engine.ProcessSpec{Args: []string{"/bin/sh", "-c", "echo $FOO; pwd"}, Env: []string{"FOO=bar"}, Dir: "/tmp"}, nil); err != nil || status != 0 || !strings.HasPrefix(out, "bar\n") || !strings.Contains(out, "tmp") {
		t.Fatalf("env/dir: %d %q %v", status, out, err)
	}

	sig := make(chan int, 1)
	var out bytes.Buffer
	started := make(chan struct{})
	go func() {
		<-started
		time.Sleep(200 * time.Millisecond)
		sig <- int(syscall.SIGTERM)
	}()
	status, err := execAt(ctx, port, cred, engine.ProcessSpec{Args: []string{"/bin/sleep", "10"}},
		engine.Stdio{Stdout: &out, Signal: sig}, func(int) { close(started) })
	if err != nil || status != 128+int(syscall.SIGTERM) {
		t.Fatalf("signal: %d %v", status, err)
	}

	resize := make(chan engine.Size, 2)
	resize <- engine.Size{Rows: 33, Cols: 101}
	out.Reset()
	status, err = execAt(ctx, port, cred, engine.ProcessSpec{Args: []string{"/bin/sh", "-c", "tty; stty size"}, TTY: true},
		engine.Stdio{Stdout: &out, Resize: resize}, nil)
	if err != nil || status != 0 || !strings.Contains(out.String(), "/dev/tty") || !strings.Contains(out.String(), "33 101") {
		t.Fatalf("tty: %d %q %v", status, out.String(), err)
	}
}

func TestHostTarRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "d", "e"), 0o755)
	os.WriteFile(filepath.Join(src, "d", "f.txt"), []byte("hello"), 0o640)
	os.Symlink("f.txt", filepath.Join(src, "d", "link"))
	var buf bytes.Buffer
	if err := TarHostPath(&buf, filepath.Join(src, "d"), "copy"); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := ExtractToHost(bytes.NewReader(buf.Bytes()), dst, "renamed"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "renamed", "f.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("content: %q %v", b, err)
	}
	if l, err := os.Readlink(filepath.Join(dst, "renamed", "link")); err != nil || l != "f.txt" {
		t.Fatalf("symlink: %q %v", l, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "renamed", "e")); err != nil || !fi.IsDir() {
		t.Fatalf("dir: %v", err)
	}
}

func tarOf(t *testing.T, entries ...*tar.Header) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		if h.Typeflag == tar.TypeReg {
			h.Size = 1
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	return buf.Bytes()
}

func TestExtractRefusesEscapes(t *testing.T) {
	outside := t.TempDir()
	cases := map[string][]byte{
		"dotdot":   tarOf(t, &tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644}),
		"absolute": tarOf(t, &tar.Header{Name: filepath.Join(outside, "abs"), Typeflag: tar.TypeReg, Mode: 0o644}),
		"symlink": tarOf(t,
			&tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: outside},
			&tar.Header{Name: "a/pwned", Typeflag: tar.TypeReg, Mode: 0o644}),
		"symlink-file": tarOf(t,
			&tar.Header{Name: "f", Typeflag: tar.TypeSymlink, Linkname: filepath.Join(outside, "target")},
			&tar.Header{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644}),
		"hardlink": tarOf(t, &tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd"}),
	}
	for name, data := range cases {
		dst := t.TempDir()
		err := ExtractToHost(bytes.NewReader(data), dst, "")
		entries, _ := os.ReadDir(outside)
		if len(entries) != 0 {
			t.Fatalf("%s: wrote outside the destination: %v", name, entries)
		}
		if name == "symlink-file" {
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if fi, err := os.Lstat(filepath.Join(dst, "f")); err != nil || !fi.Mode().IsRegular() {
				t.Fatalf("%s: f should be a regular file now", name)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s: extraction succeeded", name)
		}
	}
	if err := filterTar(bytes.NewReader(cases["dotdot"]), io.Discard); err == nil {
		t.Fatal("filterTar accepted ../escape")
	}
}
