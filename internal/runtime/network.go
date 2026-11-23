package runtime

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// User networks (vz backend, macOS 26+). Each one is a vmnet network with its own subnet and bridge:
// containers on one network reach each other, the host and (unless internal) the outside, and
// nothing on another network. networks/NAME.json holds the definition. A vmnet network dies with
// the process that created it, so while containers use it, `kido-vmm hold-network` owns it and
// writes its serialization to NAME.vmnet; each container's kido-vmm attaches to that and holds a
// shared flock on NAME.users, and the holder exits once none is left. --network none gets the same
// for one container, as a host-only network whose files live in the container's directory.

// NetworkNone runs a vz container on a private host-only network without DNS: the host reaches
// it, nothing else does.
const NetworkNone = "none"

var (
	ErrNoNetwork     = errors.New("no such network")
	reservedNetworks = map[string]bool{NetworkNone: true, "default": true, "bridge": true, "host": true}
	// Automatic subnets: 192.168.128.0/24 to 192.168.254.0/24, away from vmnet's default
	// 192.168.64.0/24.
	poolFirst, poolLast = 128, 254
)

// Network is networks/NAME.json.
type Network struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Subnet   string    `json:"subnet"`
	Gateway  string    `json:"gateway"`
	Internal bool      `json:"internal,omitempty"`
	Created  time.Time `json:"created"`
}

func (r *Runtime) networksDir() string { return filepath.Join(r.Home, "networks") }

// networkBase is the path prefix of a network's files: NAME.json, .vmnet, .users, .lock, .pid, .log.
func (r *Runtime) networkBase(name string) string { return filepath.Join(r.networksDir(), name) }

// vmnetSupported reports whether this macOS has vmnet networks (26 or later).
func vmnetSupported() error {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return err
	}
	major, _, _ := strings.Cut(v, ".")
	if n, err := strconv.Atoi(major); err != nil || n < 26 {
		return fmt.Errorf("networks need macOS 26 or later (this is %s)", v)
	}
	return nil
}

func lastAddr(p netip.Prefix) netip.Addr {
	return addAddr(p.Addr(), uint32(1)<<(32-p.Bits())-1)
}

// hostSubnets are the IPv4 subnets of the host's interfaces, except loopback.
func hostSubnets() []netip.Prefix {
	var out []netip.Prefix
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() {
			continue
		}
		ones, _ := ipn.Mask.Size()
		if addr, ok := netip.AddrFromSlice(ipn.IP.To4()); ok {
			out = append(out, netip.PrefixFrom(addr, ones).Masked())
		}
	}
	return out
}

// takenSubnets are the subnets a new network must not overlap: vmnet's NAT network, every kido
// network, every --network none container's private subnet and the host's interfaces.
func (r *Runtime) takenSubnets(nets []*Network, all []*Info) ([]netip.Prefix, error) {
	nat, err := vzSubnet()
	if err != nil {
		return nil, err
	}
	taken := []netip.Prefix{netip.MustParsePrefix(vzDefaultSubnet), nat}
	for _, n := range nets {
		if p, err := netip.ParsePrefix(n.Subnet); err == nil {
			taken = append(taken, p)
		}
	}
	for _, i := range all {
		if p, err := netip.ParsePrefix(i.Subnet); err == nil {
			taken = append(taken, p)
		}
	}
	return append(taken, hostSubnets()...), nil
}

func freeSubnet(taken []netip.Prefix) (netip.Prefix, error) {
	for third := poolFirst; third <= poolLast; third++ {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 168, byte(third), 0}), 24)
		free := true
		for _, t := range taken {
			if p.Overlaps(t) {
				free = false
				break
			}
		}
		if free {
			return p, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("no free subnet left in 192.168.%d.0/24-192.168.%d.0/24; pass --subnet", poolFirst, poolLast)
}

// parseSubnet checks a --subnet: an IPv4 network address from /16 to /29.
func parseSubnet(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() || p.Bits() < 16 || p.Bits() > 29 {
		return netip.Prefix{}, fmt.Errorf("--subnet %q: want an IPv4 network from /16 to /29, e.g. 192.168.200.0/24", s)
	}
	if p.Masked() != p {
		return netip.Prefix{}, fmt.Errorf("--subnet %s: not a network address (did you mean %s?)", s, p.Masked())
	}
	return p, nil
}

// CreateNetwork defines a user network; with subnet "" it takes the first free /24 of the pool.
func (r *Runtime) CreateNetwork(name, subnet string, internal bool) (*Network, error) {
	if !ValidName(name) || reservedNetworks[name] {
		return nil, fmt.Errorf("invalid network name %q: must match %s and not be none, default, bridge or host", name, nameRE)
	}
	if err := os.MkdirAll(r.networksDir(), 0o700); err != nil {
		return nil, err
	}
	unlock, err := r.lockNetworks()
	if err != nil {
		return nil, err
	}
	defer unlock()
	nets, err := r.Networks()
	if err != nil {
		return nil, err
	}
	for _, n := range nets {
		if n.Name == name {
			return nil, fmt.Errorf("network with name %s already exists", name)
		}
	}
	all, err := r.all()
	if err != nil {
		return nil, err
	}
	taken, err := r.takenSubnets(nets, all)
	if err != nil {
		return nil, err
	}
	var p netip.Prefix
	if subnet == "" {
		if p, err = freeSubnet(taken); err != nil {
			return nil, err
		}
	} else {
		if p, err = parseSubnet(subnet); err != nil {
			return nil, err
		}
		for _, t := range taken {
			if p.Overlaps(t) {
				return nil, fmt.Errorf("--subnet %s overlaps %s, which vmnet's NAT, another network or a host interface uses", p, t)
			}
		}
	}
	n := &Network{
		ID: randomHex(32), Name: name, Subnet: p.String(), Gateway: addAddr(p.Addr(), 1).String(),
		Internal: internal, Created: time.Now().UTC(),
	}
	if err := writeJSON(r.networkBase(name)+".json", n); err != nil {
		return nil, err
	}
	return n, nil
}

// Networks lists the user networks by name.
func (r *Runtime) Networks() ([]*Network, error) {
	entries, err := os.ReadDir(r.networksDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Network
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() || !ValidName(name) {
			continue
		}
		var n Network
		b, err := os.ReadFile(filepath.Join(r.networksDir(), e.Name()))
		if err != nil || json.Unmarshal(b, &n) != nil || n.Name != name || len(n.ID) != 64 {
			continue
		}
		if p, err := netip.ParsePrefix(n.Subnet); err != nil || !p.Addr().Is4() || p.Masked() != p {
			continue
		}
		out = append(out, &n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetNetwork finds a user network by name, full ID or unique ID prefix.
func (r *Runtime) GetNetwork(ref string) (*Network, error) {
	nets, err := r.Networks()
	if err != nil {
		return nil, err
	}
	for _, n := range nets {
		if n.Name == ref || n.ID == ref {
			return n, nil
		}
	}
	var match *Network
	for _, n := range nets {
		if ref != "" && strings.HasPrefix(n.ID, ref) {
			if match != nil {
				return nil, fmt.Errorf("network reference %q is ambiguous", ref)
			}
			match = n
		}
	}
	if match == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoNetwork, ref)
	}
	return match, nil
}

// NetworkContainers are the containers attached to network name, in any state.
func (r *Runtime) NetworkContainers(name string) ([]*Info, error) {
	all, err := r.all()
	if err != nil {
		return nil, err
	}
	var out []*Info
	for _, i := range all {
		if i.Network == name {
			out = append(out, i)
		}
	}
	return out, nil
}

// lockNetworks takes the networks lock and then the containers lock, which allocate also takes, so
// creating or removing a network and creating a container on one do not interleave.
func (r *Runtime) lockNetworks() (func(), error) {
	unlockNets, err := lock(filepath.Join(r.networksDir(), ".lock"))
	if err != nil {
		return nil, err
	}
	unlockCtrs, err := lock(filepath.Join(r.containersDir(), ".lock"))
	if err != nil {
		unlockNets()
		return nil, err
	}
	return func() { unlockCtrs(); unlockNets() }, nil
}

// RemoveNetwork deletes a network that no container uses.
func (r *Runtime) RemoveNetwork(ref string) (*Network, error) {
	unlock, err := r.lockNetworks()
	if err != nil {
		return nil, err
	}
	defer unlock()
	n, err := r.GetNetwork(ref)
	if err != nil {
		return nil, err
	}
	users, err := r.NetworkContainers(n.Name)
	if err != nil {
		return nil, err
	}
	if len(users) > 0 {
		var names []string
		for _, u := range users {
			names = append(names, u.Name)
		}
		return nil, fmt.Errorf("network %s is used by container(s) %s: remove them first", n.Name, strings.Join(names, ", "))
	}
	base := r.networkBase(n.Name)
	// The holder lingers two seconds after its last container; its files must outlive it.
	for i := 0; ; i++ {
		pid, start, ok := readHolder(base + ".pid")
		if !ok || !processAlive(pid, start) {
			break
		}
		if i == 50 {
			return nil, fmt.Errorf("network %s is still held by kido-vmm %d; try again", n.Name, pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, ext := range []string{".vmnet", ".users", ".lock", ".pid", ".log"} {
		os.Remove(base + ext)
	}
	return n, os.Remove(base + ".json")
}

// vmnetPlace gives a new vz container on a user network or on none its address (and for none its
// private subnet). It runs under the containers lock, so all sees every container.
func (r *Runtime) vmnetPlace(info *Info, all []*Info) error {
	if info.Network == NetworkNone {
		nets, err := r.Networks()
		if err != nil {
			return err
		}
		taken, err := r.takenSubnets(nets, all)
		if err != nil {
			return err
		}
		p, err := freeSubnet(taken)
		if err != nil {
			return err
		}
		info.Subnet = p.String()
		info.IP = addAddr(p.Addr(), 2).String()
		return nil
	}
	n, err := r.GetNetwork(info.Network)
	if err != nil {
		return err
	}
	p, err := netip.ParsePrefix(n.Subnet)
	if err != nil {
		return fmt.Errorf("network %s: %w", n.Name, err)
	}
	used := map[netip.Addr]bool{p.Addr(): true, addAddr(p.Addr(), 1): true, lastAddr(p): true}
	for _, o := range all {
		if o.Network == n.Name && o.ID != info.ID {
			if a, err := netip.ParseAddr(o.IP); err == nil {
				used[a] = true
			}
		}
	}
	size := uint32(1) << (32 - p.Bits())
	sum := sha256.Sum256([]byte(info.ID))
	start := binary.BigEndian.Uint32(sum[:4])
	for i := range size {
		if a := addAddr(p.Addr(), (start+i)%size); !used[a] {
			info.IP = a.String()
			return nil
		}
	}
	return fmt.Errorf("network %s: no free address left in %s", n.Name, p)
}

// vmnetSpec says where a container's network lives: its subnet, whether it is host-only and the
// path prefix of its holder files.
func (r *Runtime) vmnetSpec(info *Info) (p netip.Prefix, internal bool, base string, err error) {
	if info.Network == NetworkNone {
		p, err = netip.ParsePrefix(info.Subnet)
		return p, true, r.file(info.ID, "none"), err
	}
	n, err := r.GetNetwork(info.Network)
	if err != nil {
		return p, false, "", fmt.Errorf("container %s: %w", info.Name, err)
	}
	p, err = netip.ParsePrefix(n.Subnet)
	return p, n.Internal, r.networkBase(n.Name), err
}

// flockCtx takes an flock on f (LOCK_SH or LOCK_EX) and gives up when ctx ends.
func flockCtx(ctx context.Context, f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// vmnetJoin returns the serialization of the container's network and the users file, locked
// shared, for its kido-vmm to inherit; it starts the network's holder when none runs.
func (r *Runtime) vmnetJoin(ctx context.Context, info *Info) (users *os.File, path string, err error) {
	p, internal, base, err := r.vmnetSpec(info)
	if err != nil {
		return nil, "", err
	}
	if users, err = os.OpenFile(base+".users", os.O_CREATE|os.O_RDWR, 0o600); err != nil {
		return nil, "", err
	}
	defer func() {
		if err != nil {
			users.Close()
		}
	}()
	// A holder that saw its last user go holds the users file exclusively until it has exited.
	if err = flockCtx(ctx, users, syscall.LOCK_SH); err != nil {
		return nil, "", err
	}
	setup, err := os.OpenFile(base+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, "", err
	}
	defer setup.Close()
	if err = flockCtx(ctx, setup, syscall.LOCK_EX); err != nil {
		return nil, "", err
	}
	path = base + ".vmnet"
	if pid, start, ok := readHolder(base + ".pid"); ok && processAlive(pid, start) {
		if _, err := os.Stat(path); err == nil {
			return users, path, nil
		}
	}
	return users, path, startHolder(ctx, p, internal, base)
}

func readHolder(path string) (int, int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return 0, 0, false
	}
	pid, err1 := strconv.Atoi(f[0])
	start, err2 := strconv.ParseInt(f[1], 10, 64)
	return pid, start, err1 == nil && err2 == nil
}

var errVmnetBusy = errors.New("vmnet refused the subnet")

// startHolder starts the network's holder. When vmnet refuses the subnet, the usual reason is a
// holder that was killed while VMs used the network: vmnet keeps the subnet until InternetSharing
// exits, about a minute after its last client (any VM on vmnet). A waiting holder would be such a
// client, so kido waits without one and tries once more.
func startHolder(ctx context.Context, p netip.Prefix, internal bool, base string) error {
	err := runHolder(p, internal, base)
	if !errors.Is(err, errVmnetBusy) {
		return err
	}
	fmt.Fprintf(os.Stderr, "kido: network %s: vmnet still holds the subnet, probably for a kido-vmm that was killed while containers used it; macOS frees it about a minute after the last VM on vmnet stops. Waiting up to 2 minutes.\n", p)
	for deadline := time.Now().Add(2 * time.Minute); processRunning("InternetSharing"); {
		if time.Now().After(deadline) {
			return fmt.Errorf("network %s: vmnet still holds the subnet: stop the VMs that use vmnet (kido's, or other tools'), wait a minute and start again", p)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err = runHolder(p, internal, base); errors.Is(err, errVmnetBusy) {
		return fmt.Errorf("network %s: vmnet refuses the subnet: another vmnet network uses it", p)
	}
	return err
}

// processRunning reports whether a process named comm exists, without talking to it.
func processRunning(comm string) bool {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return true
	}
	for _, kp := range procs {
		if name, _, _ := strings.Cut(string(kp.Proc.P_comm[:]), "\x00"); name == comm {
			return true
		}
	}
	return false
}

// runHolder runs `kido-vmm hold-network` in its own session and waits until it has written the
// network's serialization.
func runHolder(p netip.Prefix, internal bool, base string) error {
	vmm := vmmPath()
	if vmm == "" {
		return errors.New("networks need kido-vmm: install it next to kido or in ../libexec/kido (make install does), or set KIDO_VMM")
	}
	os.Remove(base + ".vmnet")
	logf, err := os.OpenFile(base+".log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pr.Close()
	args := []string{"hold-network", "--subnet", p.String(), "--network", base + ".vmnet", "--users", base + ".users"}
	if internal {
		args = append(args, "--host-only")
	}
	cmd := exec.Command(vmm, args...)
	cmd.Dir = filepath.Dir(base)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.ExtraFiles = []*os.File{pw}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	pw.Close()
	if err != nil {
		return fmt.Errorf("start %s: %w", vmm, err)
	}
	go cmd.Wait()
	pid := cmd.Process.Pid
	if err := writeFileAtomic(base+".pid", fmt.Appendf(nil, "%d %d\n", pid, procStart(pid))); err != nil {
		cmd.Process.Kill()
		return err
	}
	pr.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := bufio.NewReader(pr).ReadString('\n')
	line = strings.TrimSpace(line)
	switch {
	case line == "ok":
		return nil
	case strings.HasPrefix(line, "busy "):
		return errVmnetBusy
	case strings.HasPrefix(line, "err "):
		return fmt.Errorf("network %s: %s", p, strings.TrimPrefix(line, "err "))
	}
	cmd.Process.Kill()
	if err != nil && err != io.EOF {
		return fmt.Errorf("network %s: %w", p, err)
	}
	return fmt.Errorf("network %s: kido-vmm hold-network exited: %s", p, tail(base+".log", 5))
}
