package runtime

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/imageref"
	"github.com/kido-sh/kido/internal/mount"
	"github.com/kido-sh/kido/internal/proto"
)

const (
	readyTimeout  = 30 * time.Second
	defaultStop   = 10 * time.Second
	defaultCPUs   = 2
	defaultMemory = 1024
)

type qemuLaunch struct {
	machineArgs  []string
	readyTimeout time.Duration
	start        func(*exec.Cmd) error
}

func (r *Runtime) Create(ctx context.Context, opts engine.CreateOptions) (*engine.Container, error) {
	info, err := r.prepare(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := r.Start(ctx, info.ID); err != nil {
		return info.container(), fmt.Errorf("container %s: %w", info.Name, err)
	}
	info, err = r.load(info.ID)
	if err != nil {
		return nil, err
	}
	return info.container(), nil
}

// prepare validates opts and makes the container directory, its disk and container.json, without
// booting it.
func (r *Runtime) prepare(ctx context.Context, opts engine.CreateOptions) (*Info, error) {
	ref, err := imageref.Parse(opts.Image)
	if err != nil {
		return nil, err
	}
	img, err := r.loadImage(ref)
	if err != nil {
		return nil, err
	}
	if opts.Name != "" && !ValidName(opts.Name) {
		return nil, fmt.Errorf("invalid container name %q: must match %s", opts.Name, nameRE)
	}
	cpus, err := resolveQEMUCPUs(opts.CPUs, r.Backend)
	if err != nil {
		return nil, err
	}
	opts.CPUs = cpus
	if opts.MemoryMB == 0 {
		opts.MemoryMB = defaultMemory
	}
	if opts.MemoryMB < 256 || opts.MemoryMB > 65536 {
		return nil, fmt.Errorf("--memory %d: must be between 256 and 65536 MB", opts.MemoryMB)
	}
	if opts.Network == "default" || opts.Network == "bridge" {
		opts.Network = ""
	}
	if opts.Network != "" && opts.Network != NetworkNone {
		if r.Backend != backendVZ {
			return nil, fmt.Errorf("--network %s: networks need the vz backend (KIDO_BACKEND=vz); QEMU has the default network and none", opts.Network)
		}
		n, err := r.GetNetwork(opts.Network)
		if err != nil {
			return nil, err
		}
		opts.Network = n.Name
	}
	if opts.Network != "" && r.Backend == backendVZ {
		if err := vmnetSupported(); err != nil {
			return nil, fmt.Errorf("--network %s: %w", opts.Network, err)
		}
	}
	ports, err := resolvePorts(opts.Ports)
	if err != nil {
		return nil, err
	}
	if err := mount.Check(opts.Mounts); err != nil {
		return nil, err
	}

	info, err := r.allocate(opts.Name, func(info *Info) error {
		info.Image = ref.String()
		info.RootUUID = img.RootUUID
		info.Ports = ports
		info.Network = opts.Network
		info.CPUs = opts.CPUs
		info.MemoryMB = opts.MemoryMB
		info.Labels = opts.Labels
		info.Backend = r.Backend
		if info.Backend != backendVZ || info.Network == "" {
			return nil
		}
		all, err := r.all()
		if err != nil {
			return err
		}
		return r.vmnetPlace(info, all)
	})
	if err != nil {
		return nil, err
	}
	dir := r.dir(info.ID)
	live := info.Backend == backendVZ && img.Config.Labels[mount.LiveLabel] == "1" && !opts.CopyMounts
	if err := mount.Save(dir, opts.Mounts, live); err != nil {
		if idRE.MatchString(info.ID) {
			os.RemoveAll(dir)
		}
		return nil, err
	}
	backing := filepath.Join(ref.Dir(r.Home), "disk.img")
	if info.Backend == backendVZ {
		return r.vzPrepareDisk(info, backing)
	}
	if r.QEMUErr != nil {
		return nil, r.QEMUErr
	}
	out, err := exec.CommandContext(ctx, r.QEMUImg, "create", "-q", "-f", "qcow2", "-b", backing, "-F", "raw",
		filepath.Join(dir, "disk.qcow2")).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("qemu-img create: %v: %s", err, bytes.TrimSpace(out))
	}
	if err := os.Chmod(filepath.Join(dir, "disk.qcow2"), 0o600); err != nil {
		return nil, err
	}
	if err := r.save(info); err != nil {
		return nil, err
	}
	return info, nil
}

// allocate picks a unique ID and name and creates the container directory holding its container.json.
func (r *Runtime) allocate(name string, fill func(*Info) error) (*Info, error) {
	unlock, err := lock(filepath.Join(r.containersDir(), ".lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	all, err := r.all()
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, c := range all {
		taken[c.Name] = true
		taken[c.ID] = true
	}
	if name != "" && taken[name] {
		return nil, fmt.Errorf("container name %q is already in use", name)
	}
	for name == "" || taken[name] {
		name = generateName()
	}
	var id string
	for {
		id = randomHex(6)
		if _, err := os.Stat(r.dir(id)); errors.Is(err, os.ErrNotExist) && !taken[id] {
			break
		}
	}
	if err := os.Mkdir(r.dir(id), 0o700); err != nil {
		return nil, err
	}
	info := &Info{ID: id, Name: name, Created: time.Now().UTC(), State: engine.StateCreated}
	if err := fill(info); err != nil {
		os.RemoveAll(r.dir(id))
		return nil, err
	}
	if err := r.save(info); err != nil {
		os.RemoveAll(r.dir(id))
		return nil, err
	}
	return info, nil
}

func resolvePorts(in []engine.PortMapping) ([]engine.PortMapping, error) {
	out := make([]engine.PortMapping, 0, len(in))
	for _, p := range in {
		if p.HostIP == "" {
			p.HostIP = "127.0.0.1"
		}
		if ip := net.ParseIP(p.HostIP); ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("port %d: host IP %q is not an IPv4 address", p.GuestPort, p.HostIP)
		}
		if p.Protocol == "" {
			p.Protocol = "tcp"
		}
		if p.Protocol != "tcp" && p.Protocol != "udp" {
			return nil, fmt.Errorf("port %d: protocol %q is not tcp or udp", p.GuestPort, p.Protocol)
		}
		if p.GuestPort < 1 || p.GuestPort > 65535 || p.HostPort < 0 || p.HostPort > 65535 {
			return nil, fmt.Errorf("port mapping %d:%d out of range", p.HostPort, p.GuestPort)
		}
		if p.GuestPort == proto.DefaultPort && p.Protocol == "tcp" {
			return nil, fmt.Errorf("guest port %d/tcp is reserved for kido-agent", proto.DefaultPort)
		}
		if p.HostPort == 0 {
			port, err := freePort(p.HostIP, p.Protocol)
			if err != nil {
				return nil, err
			}
			p.HostPort = port
		}
		out = append(out, p)
	}
	return out, nil
}

func freePort(ip, protocol string) (int, error) {
	addr := net.JoinHostPort(ip, "0")
	if protocol == "udp" {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			return 0, err
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).Port, nil
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (r *Runtime) Start(ctx context.Context, ref string) error {
	info, err := r.Resolve(ref)
	if err != nil {
		return err
	}
	unlock, err := r.lockContainer(info.ID)
	if err != nil {
		return err
	}
	defer unlock()
	if info, err = r.load(info.ID); err != nil {
		return err
	}
	if info.State == engine.StateRunning {
		return nil
	}
	return r.boot(ctx, info)
}

// boot starts QEMU in its own session so the VM outlives the CLI, then waits for the agent.
func (r *Runtime) boot(ctx context.Context, info *Info) error {
	if info.Backend == backendVZ {
		return r.vzBoot(ctx, info)
	}
	ref, err := imageref.Parse(info.Image)
	if err != nil {
		return err
	}
	imgDir := ref.Dir(r.Home)
	if !uuidRE.MatchString(info.RootUUID) {
		return fmt.Errorf("image %s: invalid root UUID %q", info.Image, info.RootUUID)
	}
	if r.QEMUErr != nil {
		return r.QEMUErr
	}
	launch, err := prepareQEMUBoot(info.CPUs)
	if err != nil {
		return err
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
	for _, f := range []string{"qemu.pid", "monitor.sock", "agent.port", "console.log.1", "qemu.cfg"} {
		os.Remove(filepath.Join(dir, f))
	}
	for _, f := range []string{"console.log", "qemu.log"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			return err
		}
	}
	qlog, err := os.OpenFile(filepath.Join(dir, "qemu.log"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer qlog.Close()
	console, err := startConsolePump(dir)
	if err != nil {
		return err
	}
	defer console.Close()

	netdev := fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:%d", agentPort, proto.DefaultPort)
	if info.Network == "none" {
		netdev += ",restrict=on"
	}
	for _, p := range info.Ports {
		netdev += fmt.Sprintf(",hostfwd=%s:%s:%d-:%d", p.Protocol, p.HostIP, p.HostPort, p.GuestPort)
	}
	bootArgs := strings.Join(append([]string{
		"serial=3", "kido_rtc_wait=5", "kido_rtc_delay=0", "kido_quiet_wait=0",
		"ext4-uuid=" + info.RootUUID,
		"kido_ip=10.0.2.15", "kido_gw=10.0.2.2", "kido_dns=10.0.2.3",
		"kido_time=" + strconv.FormatInt(time.Now().Unix(), 10),
	}, authArgs...), " ")
	// The boot-args hold the boot secret: QEMU reads them from a 0600 file, never from its argv.
	if len(bootArgs) > 1023 || strings.ContainsAny(bootArgs, "\"\n") {
		return fmt.Errorf("boot-args cannot go into a QEMU config file: %d bytes", len(bootArgs))
	}
	if err := writeFileAtomic(filepath.Join(dir, "qemu.cfg"), []byte("[machine]\n  append = \""+bootArgs+"\"\n")); err != nil {
		return err
	}
	margs, err := mount.QEMUArgs(dir)
	if err != nil {
		return err
	}
	args := qemuBootArgs(launch, info, imgDir, netdev, margs)
	cmd := exec.Command(r.QEMU, args...)
	cmd.Dir = dir
	cmd.Stdout = console
	cmd.Stderr = qlog
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := launch.start(cmd); err != nil {
		return fmt.Errorf("start %s: %w", r.QEMU, err)
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

	err = r.waitAgentTimeout(ctx, info.ID, agentPort, exited, launch.readyTimeout)
	os.Remove(filepath.Join(dir, "qemu.cfg"))
	if err == nil {
		if err = mount.Populate(ctx, dir, r.mountGuest(info.ID)); err == nil {
			return nil
		}
		cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
		info.PID, info.FinishedAt, info.Error = 0, time.Now().UTC(), err.Error()
		r.save(info)
		return err
	}
	cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
	}
	info.PID = 0
	info.FinishedAt = time.Now().UTC()
	info.Error = Sanitize(err.Error())
	r.save(info)
	if ctx.Err() != nil {
		return fmt.Errorf("boot interrupted: %w", ctx.Err())
	}
	msg := err.Error()
	if t := tail(filepath.Join(dir, "qemu.log"), 10); t != "" {
		msg += "\nqemu.log:\n" + t
	}
	if t := tail(filepath.Join(dir, "console.log"), 20); t != "" {
		msg += "\nconsole.log (last lines):\n" + t
	}
	return errors.New(Sanitize(msg))
}

func qemuBootArgs(launch *qemuLaunch, info *Info, imgDir, netdev string, mountArgs []string) []string {
	args := make([]string, 0, len(launch.machineArgs)+24+len(mountArgs))
	args = append(args, launch.machineArgs...)
	args = append(args,
		"-smp", strconv.Itoa(info.CPUs), "-m", strconv.Itoa(info.MemoryMB),
		"-kernel", filepath.Join(imgDir, "kernel"), "-initrd", filepath.Join(imgDir, "initrd.cpio"),
		"-readconfig", "qemu.cfg",
		"-drive", "if=none,id=root,file=disk.qcow2,format=qcow2", "-device", "virtio-blk-pci,drive=root",
		"-netdev", netdev, "-device", "virtio-net-pci,netdev=net0,romfile=",
		"-display", "none", "-serial", "stdio",
		"-monitor", "unix:monitor.sock,server=on,wait=off",
		"-pidfile", "qemu.pid", "-no-reboot",
	)
	return append(args, mountArgs...)
}

// waitReady pings until an agent proves its identity, which it records in cred.
func waitReady(ctx context.Context, port int, cred *agentCred, exited <-chan struct{}) error {
	return waitReadyTimeout(ctx, port, cred, exited, readyTimeout)
}

func waitReadyTimeout(ctx context.Context, port int, cred *agentCred, exited <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return errors.New("the VM exited during boot")
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		try := &agentCred{key: cred.key}
		if last = ping(ctx, port, try, 500*time.Millisecond); last == nil {
			cred.agent = try.agent
			return nil
		}
		select {
		case <-exited:
			return errors.New("the VM exited during boot")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	if errors.Is(last, io.EOF) || errors.Is(last, syscall.ECONNRESET) {
		return fmt.Errorf("guest agent did not answer within %s: the guest closes every connection to its agent port, so the image has an older kido-agent or none; rebake or re-import the image", timeout)
	}
	return fmt.Errorf("guest agent did not answer within %s (last error: %v)", timeout, last)
}

func tail(path string, lines int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(b) > 64<<10 {
		b = b[len(b)-64<<10:]
	}
	s := strings.TrimRight(strings.ReplaceAll(string(b), "\r", ""), "\n")
	parts := strings.Split(s, "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func (r *Runtime) Stop(ctx context.Context, ref string, timeout time.Duration) error {
	info, err := r.Resolve(ref)
	if err != nil {
		return err
	}
	unlock, err := r.lockContainer(info.ID)
	if err != nil {
		return err
	}
	defer unlock()
	if info, err = r.load(info.ID); err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = defaultStop
	}
	if info.State == engine.StateRunning {
		info.Stopping = true
		if err := r.save(info); err != nil {
			return err
		}
		r.stopMain(ctx, info, timeout)
		mount.Collect(ctx, r.dir(info.ID), r.mountGuest(info.ID), os.Stderr)
		r.shutdownAgent(info.ID, info.AgentPort)
		deadline := time.Now().Add(timeout)
		for processAlive(info.PID, info.PIDStart) && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				deadline = time.Now()
			case <-time.After(50 * time.Millisecond):
			}
		}
		if err := killQEMU(info); err != nil {
			return err
		}
		mount.Apply(r.dir(info.ID), os.Stderr)
	}
	if info.PID != 0 || info.FinishedAt.IsZero() && !info.StartedAt.IsZero() {
		info.PID = 0
		info.FinishedAt = time.Now().UTC()
	}
	info.Stopping = false
	for _, f := range []string{"qemu.pid", "monitor.sock", "agent.port"} {
		os.Remove(r.file(info.ID, f))
	}
	return r.save(info)
}

// Kill stops the VM at once, without a guest shutdown.
func (r *Runtime) Kill(ctx context.Context, ref string) error {
	info, err := r.Resolve(ref)
	if err != nil {
		return err
	}
	unlock, err := r.lockContainer(info.ID)
	if err != nil {
		return err
	}
	defer unlock()
	if info, err = r.load(info.ID); err != nil {
		return err
	}
	info.Stopping = true
	if err := r.save(info); err != nil {
		return err
	}
	if err := killQEMU(info); err != nil {
		return err
	}
	if info.PID != 0 {
		info.PID = 0
		info.FinishedAt = time.Now().UTC()
	}
	info.Stopping = false
	return r.save(info)
}

func killQEMU(info *Info) error {
	if !processAlive(info.PID, info.PIDStart) {
		return nil
	}
	if err := syscall.Kill(info.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill qemu %d: %w", info.PID, err)
	}
	for range 100 {
		if !processAlive(info.PID, info.PIDStart) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("qemu %d did not exit after SIGKILL", info.PID)
}

// monitor runs HMP commands on the VM's monitor socket.
func (r *Runtime) monitor(id string, cmds ...string) error {
	path := r.file(id, "monitor.sock")
	if len(path) >= 104 {
		return fmt.Errorf("monitor socket path %s is too long for a unix socket; use a shorter KIDO_HOME", path)
	}
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return fmt.Errorf("qemu monitor: %w", err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	if err := readPrompt(conn, br); err != nil {
		return err
	}
	for _, c := range cmds {
		if _, err := io.WriteString(conn, c+"\n"); err != nil {
			return fmt.Errorf("qemu monitor %s: %w", c, err)
		}
		if err := readPrompt(conn, br); err != nil {
			return fmt.Errorf("qemu monitor %s: %w", c, err)
		}
	}
	return nil
}

func readPrompt(conn net.Conn, br *bufio.Reader) error {
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	var buf []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			return fmt.Errorf("qemu monitor: %w", err)
		}
		buf = append(buf, b)
		if bytes.HasSuffix(buf, []byte("(qemu) ")) {
			return nil
		}
	}
}

// mountGuest runs internal/mount's guest commands in container id.
func (r *Runtime) mountGuest(id string) mount.Guest {
	return func(ctx context.Context, args []string) ([]byte, error) {
		out := &tailBuffer{max: 64 << 10}
		err := r.guestRun(ctx, id, args, nil, out)
		return out.buf, err
	}
}
