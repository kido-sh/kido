// Command kqprobe is a guest side of test/e2e.sh: it waits for a kqueue vnode event on a file.
//
//	kqprobe PATH SECONDS
//
// It prints "ready" once the watch is set, then the first event's flags, and exits 1 if none comes
// in time.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(2)
}

func main() {
	if len(os.Args) != 3 {
		die("usage: kqprobe PATH SECONDS")
	}
	secs, err := strconv.ParseFloat(os.Args[2], 64)
	if err != nil {
		die("kqprobe: %v", err)
	}
	fd, err := unix.Open(os.Args[1], unix.O_EVTONLY, 0)
	if err != nil {
		die("kqprobe: %s: %v", os.Args[1], err)
	}
	kq, err := unix.Kqueue()
	if err != nil {
		die("kqprobe: %v", err)
	}
	var ev unix.Kevent_t
	unix.SetKevent(&ev, fd, unix.EVFILT_VNODE, unix.EV_ADD|unix.EV_CLEAR)
	ev.Fflags = unix.NOTE_WRITE | unix.NOTE_EXTEND | unix.NOTE_ATTRIB | unix.NOTE_DELETE | unix.NOTE_RENAME
	if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		die("kqprobe: %v", err)
	}
	fmt.Println("ready")
	ts := unix.NsecToTimespec(int64(secs * 1e9))
	got := make([]unix.Kevent_t, 1)
	if n, err := unix.Kevent(kq, nil, got, &ts); err != nil || n == 0 {
		fmt.Println("timeout")
		os.Exit(1)
	}
	var names []string
	for _, f := range []struct {
		bit  uint32
		name string
	}{
		{unix.NOTE_WRITE, "write"}, {unix.NOTE_EXTEND, "extend"}, {unix.NOTE_ATTRIB, "attrib"},
		{unix.NOTE_DELETE, "delete"}, {unix.NOTE_RENAME, "rename"},
	} {
		if got[0].Fflags&f.bit != 0 {
			names = append(names, f.name)
		}
	}
	fmt.Printf("fflags %#x %s\n", got[0].Fflags, strings.Join(names, " "))
}
