package main

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"slices"
	"time"
	"unsafe"

	"github.com/kido-sh/kido/internal/notify"
	"github.com/kido-sh/kido/internal/proto"

	"golang.org/x/sys/unix"
)

// notifyIdle ends a notify session that sends nothing, not even a keepalive, for this long.
const notifyIdle = 90 * time.Second

// virtiofsNotify is the driver's VIRTIOFS_IOC_NOTIFY, _IOW('V', 1, struct { u64 ptr; u32 len; u32 flags }).
const virtiofsNotify = 0x80105601

var (
	errUnsupported = errors.New("the virtio-fs driver takes no notifications")
	errNoMounts    = errors.New("no share of the session is mounted yet")
)

type mountInfo struct {
	from, typ, on string
	fsid          unix.Fsid
}

// mountTable is what notify needs from the guest's file systems; tests stand in for it.
type mountTable interface {
	list() ([]mountInfo, error)
	open(path string) (int, error)
	info(fd int) (mountInfo, error)
	fsctl(fd int, batch []byte) error
	close(fd int)
}

// notifyRecheck is how often a share's mounts are listed again, to find one mounted since: a change
// it misses before then is seen by that mount within the attribute lifetime, as without notifications.
const notifyRecheck = time.Second

type openMount struct {
	fd   int
	info mountInfo
}

type shareMounts struct {
	open   []openMount
	listed time.Time
}

// notifyMounts finds a share's mounts when a batch needs them: every virtiofs mount of kido:NAME,
// opened and checked with fstatfs, so that the batch goes to the file systems it was meant for.
type notifyMounts struct {
	t      mountTable
	now    func() time.Time
	shares map[string]*shareMounts
}

func newNotifyMounts(t mountTable, names []string) *notifyMounts {
	m := &notifyMounts{t: t, now: time.Now, shares: map[string]*shareMounts{}}
	for _, name := range names {
		m.shares[name] = &shareMounts{}
	}
	return m
}

// find gives the share's open mounts, listing the mounts again at most once a notifyRecheck and
// opening them again when the list is not what is open, so that one that failed to open is tried
// again too.
func (m *notifyMounts) find(share string) ([]openMount, error) {
	sm, named := m.shares[share]
	if !named {
		return nil, fmt.Errorf("share %q is not part of this session", share)
	}
	if sm.open != nil && m.now().Sub(sm.listed) < notifyRecheck {
		return sm.open, nil
	}
	list, err := m.t.list()
	if err != nil {
		return nil, err
	}
	var listing []mountInfo
	for _, e := range list {
		if e.typ == "virtiofs" && e.from == "kido:"+share {
			listing = append(listing, e)
		}
	}
	sm.listed = m.now()
	if sm.open != nil && slices.EqualFunc(listing, sm.open, func(e mountInfo, o openMount) bool { return e == o.info }) {
		return sm.open, nil
	}
	m.forget(share)
	for _, e := range listing {
		fd, err := m.t.open(e.on)
		if err != nil {
			continue
		}
		if got, err := m.t.info(fd); err != nil || got.typ != e.typ || got.from != e.from || got.fsid != e.fsid {
			m.t.close(fd)
			continue
		}
		sm.open = append(sm.open, openMount{fd, e})
	}
	if sm.open == nil {
		return nil, fmt.Errorf("share %q is not mounted", share)
	}
	return sm.open, nil
}

func (m *notifyMounts) forget(share string) {
	sm := m.shares[share]
	for _, o := range sm.open {
		m.t.close(o.fd)
	}
	sm.open = nil
}

// send hands a batch to every mount of the share. A mount that fails it may be gone, unmounted or
// replaced (XNU then answers ENOTTY), so the mounts are found again once and those that have not taken
// the batch get it again; the driver applies a batch whole or not at all.
func (m *notifyMounts) send(share string, batch []byte) error {
	var err error
	took := map[unix.Fsid]bool{}
	for range 2 {
		mounts, ferr := m.find(share)
		if ferr != nil {
			return ferr
		}
		err = nil
		for _, o := range mounts {
			if took[o.info.fsid] {
				continue
			}
			if e := m.t.fsctl(o.fd, batch); e != nil {
				err = e
			} else {
				took[o.info.fsid] = true
			}
		}
		if err == nil {
			return nil
		}
		m.forget(share)
	}
	return err
}

// probe asks the first share that is mounted whether its driver takes notifications. A refusal counts
// only from the mount found: one unmounted meanwhile answers ENOTTY too. With none mounted there is no
// driver to ask, and the relay tries again later.
func (m *notifyMounts) probe() error {
	for share := range m.shares {
		mounts, err := m.find(share)
		if err != nil {
			continue
		}
		err = m.t.fsctl(mounts[0].fd, nil)
		if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
			if got, ierr := m.t.info(mounts[0].fd); ierr != nil || got != mounts[0].info {
				m.forget(share)
				continue
			}
			return errUnsupported
		}
		return err
	}
	return errNoMounts
}

func (m *notifyMounts) closeAll() {
	for share := range m.shares {
		m.forget(share)
	}
}

// notify serves the relay: one session at a time, so a new one ends the old one. Each frame carries
// one share's batch, checked here as the driver checks it before it goes to the driver; a batch
// that fails the check may have stood for any change, so the share gets a flush instead.
func (s *server) notify(conn net.Conn, ch *proto.Channel, req *proto.Request, respond func(proto.Response)) {
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	for _, name := range req.Args {
		if !notify.ValidShare(name) {
			respond(proto.Response{Error: fmt.Sprintf("bad share name %q", name)})
			return
		}
	}
	m := newNotifyMounts(s.mounts, req.Args)
	defer m.closeAll()
	s.notifyMu.Lock()
	if err := m.probe(); err != nil {
		s.notifyMu.Unlock()
		respond(proto.Response{Error: err.Error()})
		return
	}
	if s.notifyConn != nil {
		s.notifyConn.Close()
	}
	s.notifyConn = conn
	s.notifyMu.Unlock()
	defer func() {
		s.notifyMu.Lock()
		if s.notifyConn == conn {
			s.notifyConn = nil
		}
		s.notifyMu.Unlock()
	}()
	respond(proto.Response{OK: true, Version: proto.Version})
	conn.SetWriteDeadline(time.Time{})
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(15 * time.Second)
	}
	for {
		conn.SetReadDeadline(time.Now().Add(notifyIdle))
		typ, p, err := ch.ReadFrame()
		if err != nil || typ != proto.FrameNotify {
			return
		}
		share, batch, err := notify.DecodeFrame(p)
		if err != nil {
			s.notifyLog.printf("notify: %v", err)
			return
		}
		if share == "" {
			continue
		}
		if _, err := notify.ParseBatch(batch); err != nil {
			s.notifyLog.printf("notify %s: a malformed batch; flushing the share", share)
			batch, _ = notify.AppendRecord(nil, notify.Record{Flush: true})
		}
		s.notifySend.Lock()
		err = m.send(share, batch)
		s.notifySend.Unlock()
		if err != nil {
			s.notifyLog.printf("notify %s: %v", share, err)
		}
	}
}

type sysMounts struct{}

func statfsInfo(st *unix.Statfs_t) mountInfo {
	return mountInfo{from: unix.ByteSliceToString(st.Mntfromname[:]), typ: unix.ByteSliceToString(st.Fstypename[:]),
		on: unix.ByteSliceToString(st.Mntonname[:]), fsid: st.Fsid}
}

func (sysMounts) list() ([]mountInfo, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	buf := make([]unix.Statfs_t, n+8)
	if n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT); err != nil {
		return nil, err
	}
	out := make([]mountInfo, 0, n)
	for i := range buf[:n] {
		out = append(out, statfsInfo(&buf[i]))
	}
	return out, nil
}

// open takes an event-only descriptor, which does not keep the file system from being unmounted.
func (sysMounts) open(path string) (int, error) {
	return unix.Open(path, unix.O_EVTONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW_ANY|unix.O_CLOEXEC, 0)
}

func (sysMounts) info(fd int) (mountInfo, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return mountInfo{}, err
	}
	return statfsInfo(&st), nil
}

func (sysMounts) fsctl(fd int, batch []byte) error {
	arg := struct {
		ptr        uint64
		len, flags uint32
	}{}
	var pin runtime.Pinner
	defer pin.Unpin()
	if len(batch) > 0 {
		pin.Pin(&batch[0])
		arg.ptr, arg.len = uint64(uintptr(unsafe.Pointer(&batch[0]))), uint32(len(batch))
	}
	_, _, e := unix.Syscall6(unix.SYS_FFSCTL, uintptr(fd), virtiofsNotify, uintptr(unsafe.Pointer(&arg)), 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

func (sysMounts) close(fd int) { unix.Close(fd) }
