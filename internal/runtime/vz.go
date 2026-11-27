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
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
	"github.com/kido-sh/kido/internal/mount"
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

func BackendFromEnv() (string, error) {
	return backendFromEnv(goruntime.GOOS, os.Getenv("KIDO_BACKEND"))
}

func backendFromEnv(goos, b string) (string, error) {
	switch b {
	case "":
		if goos == "darwin" {
			return backendVZ, nil
		}
		return backendQEMU, nil
	case backendVZ:
		if goos != "darwin" {
			return "", fmt.Errorf("KIDO_BACKEND=%s requires macOS (Virtualization.framework is not available on %s); use %s", backendVZ, goos, backendQEMU)
		}
		return backendVZ, nil
	case backendQEMU:
		return backendQEMU, nil
	default:
		if goos != "darwin" {
			return "", fmt.Errorf("KIDO_BACKEND=%q: must be %s on %s", b, backendQEMU, goos)
		}
		return "", fmt.Errorf("KIDO_BACKEND=%q: must be %s or %s", b, backendVZ, backendQEMU)
	}
}

// DevVMM is a development build of kido-vmm, the last place VMMPath looks. It is empty unless the
// build sets it with -X, as `make DEV_VMM=PATH` does.
var DevVMM string

// VMMPath is $KIDO_VMM, kido-vmm next to the kido executable or in ../libexec/kido (the installed
// layout), or DevVMM; "" when none exists.
func VMMPath() string {
	if p := os.Getenv("KIDO_VMM"); p != "" {
		return p
	}
	exe, _ := os.Executable()
	for _, p := range vmmCandidates(exe) {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// vmmCandidates looks next to the real kido, not next to a symlink to it such as Homebrew's.
func vmmCandidates(exe string) []string {
	var out []string
	if exe != "" {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dir := filepath.Dir(exe)
		out = append(out, filepath.Join(dir, vmmComm), filepath.Join(dir, "..", "libexec", "kido", vmmComm))
	}
	if DevVMM != "" {
		out = append(out, DevVMM)
	}
	return out
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
	if err := cloneFile(backing, r.file(info.ID, "disk.img")); err != nil {
		if idRE.MatchString(info.ID) {
			os.RemoveAll(r.dir(info.ID))
		}
		return nil, err
	}
	return info, nil
}

type vzNetwork struct {
	Guest, Gateway netip.Addr
	MAC            string
	Mask           string // kido_mask; empty on vmnet's NAT network
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

// vzAddress gives the container a static NAT address, since the guest takes it from kido_ip. A
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
	var dev []string
	if info.Labels[BuildLabel] == "" {
		if dev, err = devShares(os.Getenv("KIDO_DEV_VIRTIOFS")); err != nil {
			return err
		}
	}
	if !uuidRE.MatchString(info.RootUUID) {
		return fmt.Errorf("image %s: invalid root UUID %q", info.Image, info.RootUUID)
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
	margs, shares, err := mount.VZArgs(dir)
	if err != nil {
		return err
	}
	margs = append(margs, dev...)
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
	nb, err := openNotify(shares, vlog)
	if err != nil {
		return err
	}
	defer nb.close()

	netArgs := []string{"kido_ip=" + nw.Guest.String(), "kido_gw=" + nw.Gateway.String(), "kido_dns=" + nw.Gateway.String()}
	if nw.Mask != "" {
		netArgs = append(netArgs, "kido_mask="+nw.Mask)
	}
	bootArgs := strings.Join(slices.Concat([]string{
		"serial=3", "kido_rtc_wait=5", "kido_rtc_delay=0", "kido_quiet_wait=0",
		"ext4-uuid=" + info.RootUUID,
		"kido_time=" + strconv.FormatInt(time.Now().Unix(), 10),
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
		args = append(args, "--forward", fmt.Sprintf("%s:%s:%d:%s:%d", p.Protocol, p.HostIP, p.HostPort, nw.Guest, p.GuestPort))
	}
	args = append(args, margs...)
	vmm := VMMPath()
	if vmm == "" {
		os.Remove(filepath.Join(dir, "boot-args"))
		return errors.New("the vz backend needs kido-vmm: install it next to kido or in ../libexec/kido (make install does), or set KIDO_VMM")
	}
	var extra []*os.File
	if users != nil {
		args = append(args, "--network", netPath)
		extra = append(extra, users)
	}
	if nb != nil {
		args = append(args, nb.vmmArgs(3+len(extra))...)
		extra = append(extra, nb.w)
	}
	cmd := exec.Command(vmm, args...)
	cmd.Dir = dir
	cmd.Stdout = vlog
	cmd.Stderr = vlog
	cmd.ExtraFiles = extra
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		os.Remove(filepath.Join(dir, "boot-args"))
		return fmt.Errorf("start %s: %w", vmm, err)
	}
	if nb != nil {
		nb.w.Close()
		nb.w = nil
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
		if err = mount.Populate(ctx, dir, r.mountGuest(info.ID)); err == nil {
			if nb != nil {
				r.startRelay(info.ID, dir, nb, vlog)
			}
			return nil
		}
		cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
		info.PID, info.FinishedAt, info.Error = 0, time.Now().UTC(), Sanitize(err.Error())
		r.save(info)
		return err
	}
	blocked := localNetworkBlocked(filepath.Join(dir, "vmm.log"), nw.Guest)
	if blocked {
		err = fmt.Errorf("macOS blocked kido from reaching the VM at %s. Allow Local Network access for the app you run kido from, in System Settings > Privacy & Security > Local Network. Terminal and SSH sessions have it already", nw.Guest)
	}
	cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
	}
	os.Remove(filepath.Join(dir, "boot-args"))
	info.PID = 0
	info.FinishedAt = time.Now().UTC()
	info.Error = Sanitize(err.Error())
	r.save(info)
	if ctx.Err() != nil {
		return fmt.Errorf("boot interrupted: %w", ctx.Err())
	}
	if blocked {
		return err
	}
	msg := fmt.Sprintf("%v (guest %s; if vmnet's NAT network is not %s, set KIDO_VZ_SUBNET)", err, nw.Guest, vzDefaultSubnet)
	if info.Network != "" {
		msg = fmt.Sprintf("%v (guest %s on network %s)", err, nw.Guest, info.Network)
	}
	return vzFailure(vmm, dir, msg)
}

// vzFailure is a boot error with the end of kido-vmm's log, which can carry text from the guest,
// made safe to print.
func vzFailure(vmm, dir, msg string) error {
	if t := tail(filepath.Join(dir, "vmm.log"), 10); t != "" {
		msg = "kido-vmm (" + vmm + ") said:\n" + t + "\n" + msg
	}
	return errors.New(Sanitize(msg))
}

// localNetworkBlocked tells macOS's Local Network privacy apart from a wrong subnet: the host has an
// address next to the guest, yet every dial failed at once.
func localNetworkBlocked(vmmLog string, guest netip.Addr) bool {
	if !strings.Contains(tail(vmmLog, 10), "No route to host after 0 ms") {
		return false
	}
	for _, p := range hostSubnets() {
		if p.Contains(guest) {
			return true
		}
	}
	return false
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

// devShares turns KIDO_DEV_VIRTIOFS, comma-separated NAME=DIR, NAME=DIR:ro or NAME=DIR:rw entries
// for developing the guest's virtio-fs client, into kido-vmm arguments; the guest mounts them itself.
func devShares(spec string) ([]string, error) {
	var args []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(spec, ",") {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		flag := "--share"
		if v, ok := strings.CutSuffix(e, ":ro"); ok {
			flag, e = "--share-ro", v
		} else {
			e = strings.TrimSuffix(e, ":rw")
		}
		name, dir, _ := strings.Cut(e, "=")
		badName := name == "" || name == "." || name == ".." || len(name) > 255 || strings.ContainsAny(name, "/\x00") ||
			seen[name] || len(name) > 1 && name[0] == 'm' && strings.Trim(name[1:], "0123456789") == ""
		if badName || !filepath.IsAbs(dir) || !isDir(dir) {
			return nil, fmt.Errorf("KIDO_DEV_VIRTIOFS: bad entry %q: want NAME=DIR[:ro|:rw] with a unique NAME other than m0, m1, ... and DIR an existing directory", strings.TrimSpace(raw))
		}
		seen[name] = true
		args = append(args, flag, e)
	}
	return args, nil
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
