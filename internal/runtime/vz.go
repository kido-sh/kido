package runtime

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
	"github.com/kido-sh/kido/internal/proto"
)

// The vz backend runs each container under Virtualization.framework through kido-vmm, a small
// helper signed with com.apple.security.virtualization (vmm/kido-vmm.m, `make sign`). The helper
// proxies 127.0.0.1:AgentPort and the published ports to the guest's address on vmnet's NAT network
// or on its user network (network.go), so the agent client is the same as with QEMU.
const (
	backendQEMU = "qemu"
	backendVZ   = "vz"
	vmmComm     = "kido-vmm"
	// vmnet's shared (NAT) network, unless /Library/Preferences/SystemConfiguration/com.apple.vmnet.plist
	// (readable by root only) overrides it; KIDO_VZ_SUBNET then has to say so.
	vzDefaultSubnet = "192.168.64.0/24"
	dhcpLeases      = "/var/db/dhcpd_leases"
)

func backendFromEnv() (string, error) {
	switch b := os.Getenv("KIDO_BACKEND"); b {
	case "", backendQEMU:
		return backendQEMU, nil
	case backendVZ:
		return backendVZ, nil
	default:
		return "", fmt.Errorf("KIDO_BACKEND=%q: must be %s or %s", b, backendQEMU, backendVZ)
	}
}

// DevVMM is a development build of kido-vmm, the last place vmmPath looks. It is empty unless the
// build sets it with -X, as `make DEV_VMM=PATH` does.
var DevVMM string

// vmmPath is $KIDO_VMM, kido-vmm next to the kido executable or in ../libexec/kido (the installed
// layout), or DevVMM; "" when none exists.
func vmmPath() string {
	if p := os.Getenv("KIDO_VMM"); p != "" {
		return p
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, vmmComm), filepath.Join(dir, "..", "libexec", "kido", vmmComm))
	}
	if DevVMM != "" {
		candidates = append(candidates, DevVMM)
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// vzBootFiles are the image's kernel and initrd, or $KIDO_VZ_KERNEL and $KIDO_VZ_INITRD when set:
// other boot files for an image whose own do not boot under VZ.
func vzBootFiles(imgDir string) (kernel, initrd string) {
	kernel, initrd = filepath.Join(imgDir, "kernel"), filepath.Join(imgDir, "initrd.cpio")
	if k := os.Getenv("KIDO_VZ_KERNEL"); k != "" {
		kernel = k
	}
	if i := os.Getenv("KIDO_VZ_INITRD"); i != "" {
		initrd = i
	}
	return kernel, initrd
}

// vzPrepareDisk ends prepare for the vz backend: VZ has no qcow2, so the container's disk is an
// APFS clone of the image's raw disk.
// On failure it removes the container directory that prepare has just allocated.
func (r *Runtime) vzPrepareDisk(info *Info, backing string) (*Info, error) {
	err := vzNetworkSupported(info)
	if err == nil {
		err = cloneFile(backing, r.file(info.ID, "disk.img"))
	}
	if err != nil {
		if idRE.MatchString(info.ID) {
			os.RemoveAll(r.dir(info.ID))
		}
		return nil, err
	}
	return info, nil
}

// vzNetworkSupported refuses udp ports: kido-vmm proxies tcp only, and vmnet's own forwarding
// would also listen on the Mac's LAN addresses.
func vzNetworkSupported(info *Info) error {
	for _, p := range info.Ports {
		if p.Protocol != "tcp" {
			return fmt.Errorf("port %d/%s: the vz backend publishes tcp ports only", p.GuestPort, p.Protocol)
		}
	}
	return nil
}

type vzNetwork struct {
	Guest, Gateway netip.Addr
	MAC            string
	Mask           string // pd_mask; empty on vmnet's NAT network
}

func vzMAC(a netip.Addr) string {
	b := a.As4()
	return fmt.Sprintf("02:dc:72:%02x:%02x:%02x", b[1], b[2], b[3])
}

// vmnetAddress is the container's fixed place on its user network or on none.
func (r *Runtime) vmnetAddress(info *Info) (*vzNetwork, error) {
	p, _, _, err := r.vmnetSpec(info)
	if err != nil {
		return nil, err
	}
	guest, err := netip.ParseAddr(info.IP)
	if err != nil || !p.Contains(guest) {
		return nil, fmt.Errorf("container %s has no address on network %s; recreate it", info.Name, info.Network)
	}
	m := net.CIDRMask(p.Bits(), 32)
	return &vzNetwork{
		Guest: guest, Gateway: addAddr(p.Addr(), 1), MAC: vzMAC(guest),
		Mask: fmt.Sprintf("%d.%d.%d.%d", m[0], m[1], m[2], m[3]),
	}, nil
}

func vzSubnet() (netip.Prefix, error) {
	s := os.Getenv("KIDO_VZ_SUBNET")
	if s == "" {
		s = vzDefaultSubnet
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() || p.Bits() < 16 || p.Bits() > 29 {
		return netip.Prefix{}, fmt.Errorf("KIDO_VZ_SUBNET=%q: want an IPv4 network from /16 to /29", s)
	}
	return p.Masked(), nil
}

func readAddr(path string) (netip.Addr, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return netip.Addr{}, err
	}
	return netip.ParseAddr(strings.TrimSpace(string(b)))
}

func leasedAddrs() map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	f, err := os.Open(dhcpLeases)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "ip_address="); ok {
			if a, err := netip.ParseAddr(v); err == nil {
				out[a] = true
			}
		}
	}
	return out
}

func addAddr(a netip.Addr, n uint32) netip.Addr {
	b := a.As4()
	return netip.AddrFrom4([4]byte(binary.BigEndian.AppendUint32(nil, binary.BigEndian.Uint32(b[:])+n)))
}

// vzAddress gives the container a static NAT address, since the guest takes it from pd_ip. A
// container keeps its address while no running container holds it. A new one starts at a hash of
// the ID in the upper half of the subnet, away from vmnet's DHCP pool (it grows from .2), and
// skips addresses of running vz containers and of the host's DHCP leases.
func (r *Runtime) vzAddress(id string) (*vzNetwork, error) {
	prefix, err := vzSubnet()
	if err != nil {
		return nil, err
	}
	unlock, err := lock(filepath.Join(r.containersDir(), ".vz-net.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	all, err := r.all()
	if err != nil {
		return nil, err
	}
	// A container that is still booting is not running yet, so a fresh vz.ip counts as taken too.
	busy := map[netip.Addr]bool{}
	for _, o := range all {
		if o.ID == id || o.Backend != backendVZ {
			continue
		}
		path := r.file(o.ID, "vz.ip")
		st, err := os.Stat(path)
		if err != nil || o.State != engine.StateRunning && time.Since(st.ModTime()) > time.Minute {
			continue
		}
		if a, err := readAddr(path); err == nil {
			busy[a] = true
		}
	}
	leased := leasedAddrs()
	network := prefix.Addr()
	size := uint32(1) << (32 - prefix.Bits())
	gateway := addAddr(network, 1)
	free := func(a netip.Addr, avoidLeases bool) bool {
		return prefix.Contains(a) && a != network && a != gateway && a != addAddr(network, size-1) &&
			!busy[a] && !(avoidLeases && leased[a])
	}

	guest, err := readAddr(r.file(id, "vz.ip"))
	if err != nil || !free(guest, false) {
		guest = netip.Addr{}
		sum := sha256.Sum256([]byte(id))
		start := binary.BigEndian.Uint32(sum[:4])
		half := size / 2
		for _, try := range []struct {
			lo, n       uint32
			avoidLeases bool
		}{{half, half - 1, true}, {2, size - 3, true}, {2, size - 3, false}} {
			for i := range try.n {
				if a := addAddr(network, try.lo+(start+i)%try.n); free(a, try.avoidLeases) {
					guest = a
					break
				}
			}
			if guest.IsValid() {
				break
			}
		}
		if !guest.IsValid() {
			return nil, fmt.Errorf("no free address in %s", prefix)
		}
	}
	if err := os.WriteFile(r.file(id, "vz.ip"), []byte(guest.String()+"\n"), 0o600); err != nil {
		return nil, err
	}
	return &vzNetwork{Guest: guest, Gateway: gateway, MAC: vzMAC(guest)}, nil
}

// vzBoot is boot for the vz backend: it starts kido-vmm in its own session so the VM outlives the
// CLI, then waits for the agent through the helper's forward.
func (r *Runtime) vzBoot(ctx context.Context, info *Info) error {
	ref, err := imageref.Parse(info.Image)
	if err != nil {
		return err
	}
	if !uuidRE.MatchString(info.RootUUID) {
		return fmt.Errorf("image %s: invalid root UUID %q", info.Image, info.RootUUID)
	}
	if err := vzNetworkSupported(info); err != nil {
		return err
	}
	var nw *vzNetwork
	if info.Network == "" {
		nw, err = r.vzAddress(info.ID)
	} else {
		nw, err = r.vmnetAddress(info)
	}
	if err != nil {
		return err
	}
	// kido-vmm inherits the network's users file, locked shared, so the holder keeps the network
	// while the VM runs. Joining can wait for vmnet, so it comes before the boot-args file exists.
	var users *os.File
	var netPath string
	if info.Network != "" {
		if users, netPath, err = r.vmnetJoin(ctx, info); err != nil {
			return err
		}
		defer users.Close()
	}
	authArgs, err := r.authBootArgs(info.ID)
	if err != nil {
		return err
	}
	agentPort, err := freePort("127.0.0.1", "tcp")
	if err != nil {
		return err
	}
	dir := r.dir(info.ID)
	for _, f := range []string{"agent.port", "vmm.state", "boot-args"} {
		os.Remove(filepath.Join(dir, f))
	}
	for _, f := range []string{"console.log", "vmm.log"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			return err
		}
	}
	vlog, err := os.OpenFile(filepath.Join(dir, "vmm.log"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer vlog.Close()

	netArgs := []string{"pd_ip=" + nw.Guest.String(), "pd_gw=" + nw.Gateway.String(), "pd_dns=" + nw.Gateway.String()}
	if nw.Mask != "" {
		netArgs = append(netArgs, "pd_mask="+nw.Mask)
	}
	bootArgs := strings.Join(slices.Concat([]string{
		"serial=3", "pd_rtc_wait=5", "pd_rtc_delay=0", "pd_quiet_wait=0",
		"ext4-uuid=" + info.RootUUID,
		"pdtime=" + strconv.FormatInt(time.Now().Unix(), 10),
	}, netArgs, authArgs), " ")
	// kido-vmm reads and deletes this file, which keeps the credential out of its argv.
	if err := os.WriteFile(filepath.Join(dir, "boot-args"), []byte(bootArgs), 0o600); err != nil {
		return err
	}
	kernel, initrd := vzBootFiles(ref.Dir(r.Home))
	args := []string{
		"--kernel", kernel, "--initrd", initrd, "--disk", "disk.img", "--cmdline-file", "boot-args",
		"--cpus", strconv.Itoa(info.CPUs), "--memory", strconv.Itoa(info.MemoryMB), "--mac", nw.MAC,
		"--console", "console.log", "--state-file", "vmm.state",
		"--forward", fmt.Sprintf("tcp:127.0.0.1:%d:%s:%d", agentPort, nw.Guest, proto.DefaultPort),
	}
	for _, p := range info.Ports {
		args = append(args, "--forward", fmt.Sprintf("tcp:%s:%d:%s:%d", p.HostIP, p.HostPort, nw.Guest, p.GuestPort))
	}
	vmm := vmmPath()
	if vmm == "" {
		os.Remove(filepath.Join(dir, "boot-args"))
		return errors.New("the vz backend needs kido-vmm: install it next to kido or in ../libexec/kido (make install does), or set KIDO_VMM")
	}
	if users != nil {
		args = append(args, "--network", netPath)
	}
	cmd := exec.Command(vmm, args...)
	cmd.Dir = dir
	cmd.Stdout = vlog
	cmd.Stderr = vlog
	if users != nil {
		cmd.ExtraFiles = []*os.File{users}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		os.Remove(filepath.Join(dir, "boot-args"))
		return fmt.Errorf("start %s: %w", vmm, err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	info.PID = cmd.Process.Pid
	info.PIDStart = procStart(info.PID)
	info.AgentPort = agentPort
	info.StartedAt = time.Now().UTC()
	info.FinishedAt = time.Time{}
	info.ExitCode = nil
	info.Error = ""
	info.Stopping = false
	if err := os.WriteFile(filepath.Join(dir, "agent.port"), []byte(strconv.Itoa(agentPort)), 0o600); err != nil {
		cmd.Process.Kill()
		return err
	}
	if err := r.save(info); err != nil {
		cmd.Process.Kill()
		return err
	}

	err = r.waitAgent(ctx, info.ID, agentPort, exited)
	if err == nil {
		return nil
	}
	cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
	}
	os.Remove(filepath.Join(dir, "boot-args"))
	info.PID = 0
	info.FinishedAt = time.Now().UTC()
	info.Error = err.Error()
	r.save(info)
	if ctx.Err() != nil {
		return fmt.Errorf("boot interrupted: %w", ctx.Err())
	}
	msg := fmt.Sprintf("%v (guest %s; if vmnet's NAT network is not %s, set KIDO_VZ_SUBNET)", err, nw.Guest, vzDefaultSubnet)
	if info.Network != "" {
		msg = fmt.Sprintf("%v (guest %s on network %s)", err, nw.Guest, info.Network)
	}
	if t := tail(filepath.Join(dir, "vmm.log"), 10); t != "" {
		msg = "kido-vmm (" + vmm + ") said:\n" + t + "\n" + msg
	}
	return errors.New(msg)
}

// vzSetPaused pauses or resumes the VM through kido-vmm's SIGUSR1/SIGUSR2 and waits for the state.
func vzSetPaused(dir string, info *Info, pause bool) error {
	sig, want := syscall.SIGUSR2, "running"
	if pause {
		sig, want = syscall.SIGUSR1, "paused"
	}
	if !processAlive(info.PID, info.PIDStart) {
		return ErrNotRunning
	}
	if err := syscall.Kill(info.PID, sig); err != nil {
		return err
	}
	for range 500 {
		if b, err := os.ReadFile(filepath.Join(dir, "vmm.state")); err == nil && strings.TrimSpace(string(b)) == want {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("VM did not reach state %s", want)
}

// vzCommit is Commit for the vz backend: the guest syncs, the VM pauses, and the image's disk is an
// APFS clone of the container's raw disk.
func (r *Runtime) vzCommit(ctx context.Context, info *Info, target, parent imageref.Ref, cfg engine.ImageConfig) (*engine.Image, error) {
	dir := r.dir(info.ID)
	if info.State == engine.StateRunning {
		syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		r.ExecNotify(syncCtx, info.ID, engine.ProcessSpec{Args: []string{"/bin/sync"}}, engine.Stdio{}, nil)
		cancel()
		if err := vzSetPaused(dir, info, true); err != nil {
			return nil, fmt.Errorf("pause: %w", err)
		}
		defer vzSetPaused(dir, info, false)
	}
	parentDir := parent.Dir(r.Home)
	return r.storeImage(target, &imageJSON{RootUUID: info.RootUUID, Parent: parent.String(), Config: cfg}, func(tmp string) error {
		if err := cloneFile(filepath.Join(dir, "disk.img"), filepath.Join(tmp, "disk.img")); err != nil {
			return err
		}
		for _, f := range []string{"kernel", "initrd.cpio"} {
			if err := cloneFile(filepath.Join(parentDir, f), filepath.Join(tmp, f)); err != nil {
				return err
			}
		}
		return nil
	})
}
