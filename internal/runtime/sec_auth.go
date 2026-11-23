package runtime

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/kido-sh/kido/internal/proto"
)

// agentCred authenticates the CLI to a container's agent: the host key made at boot, and the
// agent identity recorded at the first ping after boot (nil while booting, when the first agent
// that proves a key is recorded).
type agentCred struct {
	key   ed25519.PrivateKey
	agent ed25519.PublicKey
}

func (c *agentCred) handshake(conn net.Conn, br *bufio.Reader, req proto.Request) (*proto.Channel, error) {
	agent, ch, err := proto.ClientHandshake(conn, br, c.key, c.agent, req)
	if err == nil && c.agent == nil {
		c.agent = agent
	}
	return ch, err
}

// authBootArgs makes a fresh host key for one boot of container id and returns the boot-args that
// hand its public half and the boot secret to the guest. Every backend calls it before every boot and
// must keep these boot-args out of every argv: the boot secret is what the host's first contact
// checks before it pins the agent identity.
func (r *Runtime) authBootArgs(id string) ([]string, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	for _, f := range []string{"agent.pub", "token"} {
		if err := os.Remove(r.file(id, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if err := writeFileAtomic(r.file(id, "key"), priv.Seed()); err != nil {
		return nil, err
	}
	return []string{"kido_key=" + proto.EncodeKey(pub), "kido_secret=" + proto.EncodeSecret(proto.BootSecret(priv)),
		"kido_port=" + strconv.Itoa(proto.DefaultPort)}, nil
}

func (r *Runtime) hostKey(id string) (ed25519.PrivateKey, error) {
	seed, err := os.ReadFile(r.file(id, "key"))
	if errors.Is(err, fs.ErrNotExist) {
		if _, terr := os.Stat(r.file(id, "token")); terr == nil {
			return nil, fmt.Errorf("container %s was started by an older kido: stop it and start it again", id)
		}
	}
	if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("container %s: malformed key file", id)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// agentCred returns the credential for talking to the agent of a running container.
func (r *Runtime) agentCred(id string) (*agentCred, error) {
	key, err := r.hostKey(id)
	if err != nil {
		return nil, err
	}
	pub, err := os.ReadFile(r.file(id, "agent.pub"))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("container %s: no agent identity was recorded at boot; restart the container", id)
	}
	return &agentCred{key: key, agent: pub}, nil
}

// waitAgent polls the agent of container id through 127.0.0.1:port until it authenticates, for
// up to readyTimeout, gives up early when exited is closed, and records the agent's identity.
// The port is free when the VM process is started, so another local user can bind it first: nothing
// is sent to it until the VM process itself listens there.
func (r *Runtime) waitAgent(ctx context.Context, id string, port int, exited <-chan struct{}) error {
	key, err := r.hostKey(id)
	if err != nil {
		return err
	}
	info, err := r.load(id)
	if err != nil {
		return err
	}
	if err := waitListener(ctx, info.PID, port, exited); err != nil {
		return err
	}
	cred := &agentCred{key: key}
	if err := waitReady(ctx, port, cred, exited); err != nil {
		return err
	}
	if !processAlive(info.PID, info.PIDStart) {
		return errors.New("the VM process exited during boot")
	}
	return writeFileAtomic(r.file(id, "agent.pub"), cred.agent)
}

// waitListener waits until process pid listens on 127.0.0.1:port. Once it does, the kernel keeps
// every other process off that address and port.
func waitListener(ctx context.Context, pid, port int, exited <-chan struct{}) error {
	deadline := time.Now().Add(readyTimeout)
	for {
		out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-a", "-p", strconv.Itoa(pid),
			"-iTCP@127.0.0.1:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
		if err == nil && strings.TrimSpace(string(out)) == strconv.Itoa(pid) {
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("the VM process exited before it listened on the agent port 127.0.0.1:%d; another process may have taken that port", port)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the VM process does not listen on the agent port 127.0.0.1:%d (lsof: %v)", port, err)
		}
	}
}

// shutdownAgent asks the agent of container id to power the guest off.
func (r *Runtime) shutdownAgent(id string, port int) error {
	cred, err := r.agentCred(id)
	if err != nil {
		return err
	}
	return shutdown(port, cred)
}
