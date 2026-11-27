package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/kido-sh/kido/internal/mount"
	"github.com/kido-sh/kido/internal/notify"
	"github.com/kido-sh/kido/internal/proto"

	"golang.org/x/sys/unix"
)

// NotifyShare is a live share the relay watches: its name in the guest, its root as the Mac spells
// it in FSEvents paths, and a descriptor for that root, opened at boot without following any symlink.
type NotifyShare struct {
	Name string
	Root string
	FD   int
}

const (
	atSymlinkNofollowAny = 0x0800 // AT_SYMLINK_NOFOLLOW_ANY, <sys/fcntl.h>
	atResolveBeneath     = 0x2000 // AT_RESOLVE_BENEATH

	notifyKeepalive  = 30 * time.Second
	notifyFlushGap   = 100 * time.Millisecond
	notifyStable     = 10 * time.Second
	notifyMaxPending = 1 << 20
	notifyMaxDrain   = 4096
	notifyMaxKnown   = 1 << 15
	notifyKnownBytes = 4 << 20
	notifyMaxIDs     = 4
	notifyLogBudget  = 256 << 10
	notifyLogLine    = 512
	notifyLogBurst   = 20
	notifyLogEvery   = 5 * time.Second
)

var errNotifyUnsupported = errors.New("the container takes no notifications")

// statBeneath reads the Mac's state of rel under the root dirfd, and its inode: never through a
// symlink and never outside the root. Absent only when the path does not exist; any other failure is
// unknown, which the guest always treats as a change.
func statBeneath(dirfd int, rel string) (notify.Record, uint64) {
	r := notify.Record{Path: rel}
	var st unix.Stat_t
	switch err := unix.Fstatat(dirfd, rel, &st, atSymlinkNofollowAny|atResolveBeneath); {
	case err == nil:
		r.State = notify.Present
		r.Mode, r.Nlink, r.Size = uint32(st.Mode), uint32(st.Nlink), uint64(st.Size)
		r.MtimeSec, r.MtimeNsec = st.Mtim.Sec, uint32(st.Mtim.Nsec)
		r.CtimeSec, r.CtimeNsec = st.Ctim.Sec, uint32(st.Ctim.Nsec)
		return r, st.Ino
	case errors.Is(err, unix.ENOENT):
		r.State = notify.Absent
	}
	return r, 0
}

// fdPath is the path of fd as F_GETPATH gives it: the case, Unicode form and /private prefix the
// volume stores, which FSEvents paths use too.
func fdPath(fd int) (string, error) {
	buf := make([]byte, unix.PathMax)
	if _, _, e := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		return "", e
	}
	return unix.ByteSliceToString(buf), nil
}

// notifySession is the relay's connection to the agent.
type notifySession interface {
	send(frame []byte) error
	closed() <-chan struct{}
	close()
}

const (
	markName = 1 << iota // the name may have been created, removed or renamed
	markDeep             // cut to notify.MaxDepth components, so its state says nothing
)

type pathState struct {
	present, dir bool
	ino          uint64
}

type relayPending struct {
	flush      bool
	paths      []string
	marks      map[string]uint8
	ids        map[string][]uint64 // file IDs of the pending paths' events
	bytes      int
	lastFlush  time.Time
	known      map[string]pathState // what the last record for each path said
	knownSize  int                  // bytes of known's keys
	pruneScans int
}

// backoff spaces out attempts to reach the agent: each failure doubles the wait up to max, and only
// a session that stayed up for stable starts again from min.
type backoff struct {
	min, max, stable time.Duration
	cur              time.Duration
	up               time.Time
}

func (b *backoff) connected(now time.Time) { b.up = now }

// failed returns the wait before the next attempt, with up to half of it again as jitter.
func (b *backoff) failed(now time.Time) time.Duration {
	if !b.up.IsZero() && now.Sub(b.up) >= b.stable {
		b.cur = 0
	}
	b.up = time.Time{}
	b.cur = min(max(2*b.cur, b.min), b.max)
	return b.cur + time.Duration(rand.Int64N(int64(b.cur)/2+1))
}

// relay turns kido-vmm's events into batches for the agent. A path is read when its batch is
// built, so a record carries the newest state however many events named the path; a share whose
// records pile up, or whose events were lost, gets a flush instead.
type relay struct {
	shares    []NotifyShare
	dial      func(context.Context) (notifySession, error)
	log       *budgetLog
	now       func() time.Time
	backoff   backoff
	keepalive time.Duration
	flushGap  time.Duration
	pend      []relayPending
	strayed   bool
}

func (r *relay) init() {
	r.pend = make([]relayPending, len(r.shares))
	for i := range r.pend {
		r.pend[i].marks, r.pend[i].ids, r.pend[i].known = map[string]uint8{}, map[string][]uint64{}, map[string]pathState{}
	}
}

// within gives path relative to root when root is path or a whole-component prefix of it.
func within(root, path string) (string, bool) {
	if path == root {
		return "", true
	}
	if rel, ok := strings.CutPrefix(path, strings.TrimSuffix(root, "/")+"/"); ok {
		return rel, true
	}
	return "", false
}

func (r *relay) flushShare(i int) {
	p := &r.pend[i]
	p.flush, p.paths, p.marks, p.ids, p.bytes = true, nil, map[string]uint8{}, map[string][]uint64{}, 0
	clear(p.known)
	p.knownSize = 0
	p.pruneScans = 0
}

func (r *relay) flushAll() {
	for i := range r.pend {
		r.flushShare(i)
	}
}

// add takes an event for every share it falls in. FSEvents reports only what lies under the roots,
// so an event under none of them means its paths are spelled differently, and only a flush is safe.
func (r *relay) add(ev notify.Event) {
	if ev.Flush {
		r.flushAll()
		return
	}
	matched := false
	for i, s := range r.shares {
		if rel, ok := within(s.Root, ev.Path); ok {
			matched = true
			r.addPath(i, rel, ev)
		}
	}
	if !matched {
		if !r.strayed {
			r.strayed = true
			r.log.printf("an event outside every share (%s): flushing them all", ev.Path)
		}
		r.flushAll()
	}
}

func (r *relay) addPath(i int, rel string, ev notify.Event) {
	rel, deep := notify.CutDepth(rel)
	if ev.Flags&notify.FlagsLost != 0 || rel == "" || notify.ValidPath(rel) != nil {
		r.flushShare(i)
		return
	}
	p := &r.pend[i]
	if p.flush {
		return
	}
	var mark uint8
	if ev.Flags&notify.FlagsName != 0 {
		mark |= markName
	}
	if deep {
		mark |= markDeep
	}
	m, seen := p.marks[rel]
	if !seen && p.bytes+notify.RecordSize(rel) > notifyMaxPending {
		r.flushShare(i)
		return
	}
	if !seen {
		p.paths = append(p.paths, rel)
		p.bytes += notify.RecordSize(rel)
	}
	p.marks[rel] = m | mark
	if ids := p.ids[rel]; !deep && ev.FileID != 0 && len(ids) < notifyMaxIDs && !slices.Contains(ids, ev.FileID) {
		p.ids[rel] = append(ids, ev.FileID)
	}
}

// judge sets whether rec's name changed since the guest last heard of it, and whether it now names a
// different file, from what the relay's last record for the path said and from the file IDs of this
// batch's events, and only when the relay has said nothing of the path yet from FSEvents' flags, which
// accumulate per path and so tell of changes that may be long past. A file ID tells of a replace only
// if it is the one the relay last reported there, or the relay reported none; any other belongs to a
// replace already reported. What the relay said of the paths below a directory that went or was
// replaced no longer holds, and a flush forgets everything.
func (p *relayPending) judge(rec *notify.Record, ino uint64, flagged bool) {
	rel := rec.Path
	prev, known := p.known[rel]
	if len(p.known) > 0 && ((known && prev.dir && (rec.State != notify.Present || prev.ino != ino)) || (!known && rec.State == notify.Absent)) {
		if p.pruneScans+len(p.known) > notifyMaxKnown {
			clear(p.known)
			p.knownSize = 0
			known = false
		} else {
			p.pruneScans += len(p.known)
			prefix := rel + "/"
			for k := range p.known {
				if strings.HasPrefix(k, prefix) {
					p.forget(k)
				}
			}
		}
	}
	if rec.State == notify.Unknown {
		p.forget(rel)
		rec.NameChanged = true
		return
	}
	cur := pathState{present: rec.State == notify.Present, dir: rec.Mode&unix.S_IFMT == unix.S_IFDIR, ino: ino}
	other := slices.ContainsFunc(p.ids[rel], func(id uint64) bool {
		return id != ino && (!known || !prev.present || id == prev.ino)
	})
	switch {
	case cur.present && (other || known && prev.present && prev.ino != ino):
		rec.Replaced, rec.NameChanged = true, true
	case known:
		rec.NameChanged = prev.present != cur.present
	default:
		rec.NameChanged = flagged
	}
	if !known && (len(p.known) >= notifyMaxKnown || p.knownSize+len(rel) > notifyKnownBytes) {
		clear(p.known)
		p.knownSize = 0
	}
	if _, ok := p.known[rel]; !ok {
		p.knownSize += len(rel)
	}
	p.known[rel] = cur
}

func (p *relayPending) forget(rel string) {
	if _, ok := p.known[rel]; ok {
		delete(p.known, rel)
		p.knownSize -= len(rel)
	}
}

// frames takes what is pending as frames of at most notify.MaxBatch bytes of records each. A flush
// goes alone and at most once per flushGap per share; wait says when a held-back flush is due.
func (r *relay) frames() (out [][]byte, wait time.Duration) {
	now := r.now()
	for i := range r.pend {
		p := &r.pend[i]
		if !p.flush {
			f, err := r.batches(i)
			if err == nil {
				out = append(out, f...)
				p.paths, p.marks, p.ids, p.bytes = nil, map[string]uint8{}, map[string][]uint64{}, 0
				continue
			}
			r.flushShare(i)
		}
		if gap := p.lastFlush.Add(r.flushGap).Sub(now); gap > 0 {
			if wait == 0 || gap < wait {
				wait = gap
			}
			continue
		}
		b, _ := notify.AppendRecord(nil, notify.Record{Flush: true})
		f, err := notify.EncodeFrame(r.shares[i].Name, b)
		if err != nil {
			continue
		}
		out = append(out, f)
		p.flush, p.lastFlush = false, now
	}
	return out, wait
}

// batches reads the state of each of share i's pending paths and encodes the records.
func (r *relay) batches(i int) ([][]byte, error) {
	p, s := &r.pend[i], r.shares[i]
	p.pruneScans = 0
	var out [][]byte
	var batch []byte
	emit := func() error {
		f, err := notify.EncodeFrame(s.Name, batch)
		if err != nil {
			return err
		}
		out, batch = append(out, f), nil
		return nil
	}
	for _, rel := range p.paths {
		if len(batch)+notify.RecordSize(rel) > notify.MaxBatch {
			if err := emit(); err != nil {
				return nil, err
			}
		}
		rec, mark := notify.Record{Path: rel, NameChanged: true}, p.marks[rel]
		if mark&markDeep == 0 {
			var ino uint64
			rec, ino = statBeneath(s.FD, rel)
			p.judge(&rec, ino, mark&markName != 0)
		}
		var err error
		if batch, err = notify.AppendRecord(batch, rec); err != nil {
			return nil, err
		}
	}
	if len(batch) > 0 {
		if err := emit(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// drain takes the events already waiting, up to notifyMaxDrain, so that a burst goes out in few
// batches without holding back the session.
func (r *relay) drain(events <-chan notify.Event) {
	for range notifyMaxDrain {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			r.add(ev)
		default:
			return
		}
	}
}

// run forwards events until they end. A session that breaks may have lost what it carried, so
// every share gets a flush once a new session is up.
func (r *relay) run(ctx context.Context, events <-chan notify.Event, readErr <-chan error) error {
	var sess notifySession
	retry := time.NewTimer(0)
	defer retry.Stop()
	due := time.NewTimer(time.Hour)
	defer due.Stop()
	keep := time.NewTicker(r.keepalive)
	defer keep.Stop()
	drop := func(err error) {
		r.log.printf("session to the agent ended: %v", err)
		sess.close()
		sess = nil
		r.flushAll()
		retry.Reset(r.backoff.failed(r.now()))
	}
	for {
		if sess != nil {
			frames, wait := r.frames()
			for _, f := range frames {
				if err := sess.send(f); err != nil {
					drop(err)
					break
				}
			}
			if sess != nil && wait > 0 {
				due.Reset(wait)
			}
		}
		var closed <-chan struct{}
		var retryC <-chan time.Time
		if sess != nil {
			closed = sess.closed()
		} else {
			retryC = retry.C
		}
		select {
		case <-ctx.Done():
			if sess != nil {
				sess.close()
			}
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				if sess != nil {
					sess.close()
				}
				return <-readErr
			}
			r.add(ev)
			r.drain(events)
		case <-due.C:
		case <-keep.C:
			if sess != nil {
				if err := sess.send(nil); err != nil {
					drop(err)
				}
			}
		case <-closed:
			drop(errors.New("closed by the agent"))
		case <-retryC:
			s, err := r.dial(ctx)
			switch {
			case errors.Is(err, errNotifyUnsupported):
				r.log.printf("%v; notifications stay off", err)
				return nil
			case err != nil:
				r.log.printf("connecting to the agent: %v", err)
				retry.Reset(r.backoff.failed(r.now()))
			default:
				sess = s
				r.backoff.connected(r.now())
				r.log.printf("notifications on")
			}
		}
	}
}

// readEvents feeds kido-vmm's records to out until the pipe closes. A malformed record leaves the
// stream unusable, so it ends with a flush of every share.
func readEvents(in io.Reader, out chan<- notify.Event) error {
	defer close(out)
	br := bufio.NewReaderSize(in, 64<<10)
	for {
		ev, err := notify.ReadEvent(br)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			out <- notify.Event{Flush: true}
			return fmt.Errorf("records from kido-vmm: %w", err)
		}
		out <- ev
	}
}

type agentNotify struct {
	conn net.Conn
	ch   *proto.Channel
	done chan struct{}
	once sync.Once
}

func (s *agentNotify) send(frame []byte) error {
	s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return s.ch.WriteFrame(proto.FrameNotify, frame)
}

func (s *agentNotify) closed() <-chan struct{} { return s.done }

func (s *agentNotify) close() {
	s.once.Do(func() {
		s.conn.Close()
		close(s.done)
	})
}

// notifyDial opens a notify session with the pinned agent identity (agentCred fails without one), never
// a first contact. The host key is read from its file for each handshake and cleared after it.
func (r *Runtime) notifyDial(id string, port int, names []string) func(context.Context) (notifySession, error) {
	return func(ctx context.Context) (notifySession, error) {
		cred, err := r.agentCred(id)
		if err != nil {
			return nil, err
		}
		defer clear(cred.key)
		conn, ch, resp, err := request(ctx, port, cred, proto.Request{Op: proto.OpNotify, Args: names}, 10*time.Second)
		if err != nil {
			return nil, err
		}
		if !resp.OK {
			conn.Close()
			if strings.Contains(resp.Error, "unknown op") || strings.Contains(resp.Error, "no notifications") {
				return nil, errNotifyUnsupported
			}
			return nil, fmt.Errorf("agent: %s", Sanitize(resp.Error))
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(15 * time.Second)
		}
		s := &agentNotify{conn: conn, ch: ch, done: make(chan struct{})}
		go func() {
			s.ch.ReadFrame()
			s.close()
		}()
		return s, nil
	}
}

// RunNotifyRelay forwards kido-vmm's records for container id to its agent until events ends.
func (r *Runtime) RunNotifyRelay(ctx context.Context, id string, events io.Reader, shares []NotifyShare, logw io.Writer) error {
	info, err := r.Get(id)
	if err != nil {
		return err
	}
	names := make([]string, len(shares))
	for i, s := range shares {
		names[i] = s.Name
	}
	rl := &relay{shares: shares, dial: r.notifyDial(info.ID, info.AgentPort, names), log: newBudgetLog(logw, notifyLogBudget),
		now:       time.Now,
		backoff:   backoff{min: 100 * time.Millisecond, max: 5 * time.Second, stable: notifyStable},
		keepalive: notifyKeepalive, flushGap: notifyFlushGap}
	rl.init()
	ch := make(chan notify.Event, 1024)
	errc := make(chan error, 1)
	go func() { errc <- readEvents(events, ch) }()
	return rl.run(ctx, ch, errc)
}

// budgetLog writes sanitized lines, each message cut to notifyLogLine bytes: notifyLogBurst at once and
// one more every notifyLogEvery, saying how many it dropped, until it has written max bytes, then one
// last line saying so.
type budgetLog struct {
	mu      sync.Mutex
	w       io.Writer
	left    int
	now     func() time.Time
	tokens  float64
	last    time.Time
	dropped int
}

func newBudgetLog(w io.Writer, max int) *budgetLog {
	return &budgetLog{w: w, left: max, now: time.Now, tokens: notifyLogBurst, last: time.Now()}
}

func (l *budgetLog) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.tokens = min(notifyLogBurst, l.tokens+now.Sub(l.last).Seconds()/notifyLogEvery.Seconds())
	l.last = now
	if l.left <= 0 {
		return
	}
	if l.tokens < 1 {
		l.dropped++
		return
	}
	l.tokens--
	msg := strings.NewReplacer("\n", `\n`, "\r", `\r`).Replace(Sanitize(fmt.Sprintf(format, args...)))
	if len(msg) > notifyLogLine {
		msg = strings.ToValidUTF8(msg[:notifyLogLine], "") + "..."
	}
	if l.dropped > 0 {
		msg += fmt.Sprintf(" (%d lines dropped before this one)", l.dropped)
		l.dropped = 0
	}
	line := now.UTC().Format(time.RFC3339) + " kido notify: " + msg + "\n"
	if len(line) > l.left {
		line = "kido notify: log limit reached\n"
		l.left = 0
	} else {
		l.left -= len(line)
	}
	io.WriteString(l.w, line)
}

// NotifyCommand is the hidden kido command that runs the relay.
const NotifyCommand = "__notify"

// notifyBoot is what vzBoot opens for notifications: a descriptor for each live share root and the
// pipe from kido-vmm to the relay.
type notifyBoot struct {
	shares []NotifyShare
	roots  []*os.File
	r, w   *os.File
}

// openNotify prepares notifications for the live shares. It returns nil when there are none or
// KIDO_NO_NOTIFY is 1. A root that no longer opens without a symlink, or is no longer the directory
// mount.Shares checked, fails the boot; any other failure only leaves notifications off.
func openNotify(shares []mount.Share, warn io.Writer) (*notifyBoot, error) {
	if len(shares) == 0 || os.Getenv("KIDO_NO_NOTIFY") == "1" {
		return nil, nil
	}
	nb := &notifyBoot{}
	for _, s := range shares {
		fd, err := unix.Open(s.Source, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW_ANY|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ELOOP) {
			nb.close()
			return nil, fmt.Errorf("mount %s: the path now leads through a symbolic link; refusing to follow it", s.Source)
		}
		var root string
		var st unix.Stat_t
		if err == nil {
			nb.roots = append(nb.roots, os.NewFile(uintptr(fd), s.Source))
			err = unix.Fstat(fd, &st)
		}
		if err == nil && (uint64(st.Dev) != s.Dev || st.Ino != s.Ino) {
			nb.close()
			return nil, fmt.Errorf("mount %s: the path now leads to another directory; refusing to follow it", s.Source)
		}
		if err == nil {
			root, err = fdPath(fd)
		}
		if err != nil {
			nb.close()
			fmt.Fprintf(warn, "kido: notifications off: %s: %v\n", s.Source, err)
			return nil, nil
		}
		nb.shares = append(nb.shares, NotifyShare{Name: s.Name, Root: root})
	}
	var err error
	if nb.r, nb.w, err = os.Pipe(); err != nil {
		nb.close()
		fmt.Fprintf(warn, "kido: notifications off: %v\n", err)
		return nil, nil
	}
	return nb, nil
}

func (nb *notifyBoot) close() {
	if nb == nil {
		return
	}
	for _, f := range nb.roots {
		f.Close()
	}
	for _, f := range []*os.File{nb.r, nb.w} {
		if f != nil {
			f.Close()
		}
	}
	nb.roots, nb.r, nb.w = nil, nil, nil
}

// vmmArgs are kido-vmm's arguments when the pipe's write end is its descriptor fd.
func (nb *notifyBoot) vmmArgs(fd int) []string {
	args := []string{"--notify-fd", strconv.Itoa(fd)}
	for _, s := range nb.shares {
		args = append(args, "--watch", s.Root)
	}
	return args
}

// notifyEnv is all of kido's environment the relay gets.
var notifyEnv = []string{"HOME", "KIDO_HOME", "PATH", "TMPDIR"}

// startRelay starts the relay in its own session, with the pipe as its stdin, its log as stdout
// and stderr, and the roots from descriptor 3, then closes kido's copies. It never fails a boot:
// without the relay the guest keeps the server's attribute lifetime.
func (r *Runtime) startRelay(id, dir string, nb *notifyBoot, warn io.Writer) {
	defer nb.close()
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(warn, "kido: notifications off: %v\n", err)
		return
	}
	logf, err := os.OpenFile(filepath.Join(dir, "notify.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		fmt.Fprintf(warn, "kido: notifications off: %v\n", err)
		return
	}
	defer logf.Close()
	args := []string{NotifyCommand, id}
	for _, s := range nb.shares {
		args = append(args, s.Name+"="+s.Root)
	}
	cmd := exec.Command(self, args...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nb.r, logf, logf
	cmd.ExtraFiles = nb.roots
	cmd.Env = []string{}
	for _, k := range notifyEnv {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(warn, "kido: notifications off: %v\n", err)
		return
	}
	go cmd.Wait()
}
