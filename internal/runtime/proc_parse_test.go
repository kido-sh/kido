package runtime

import (
	"fmt"
	"testing"
)

func makeProcStatLine(pid int, comm string, state byte, start int64) string {
	return fmt.Sprintf("%d (%s) %c 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 %d 100 200", pid, comm, state, start)
}

func TestParseProcStat(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantOK    bool
		wantPID   int
		wantComm  string
		wantState byte
		wantStart int64
	}{
		{
			name:      "qemu truncated comm",
			line:      makeProcStatLine(4242, "qemu-system-aar", 'S', 987654321),
			wantOK:    true,
			wantPID:   4242,
			wantComm:  "qemu-system-aar",
			wantState: 'S',
			wantStart: 987654321,
		},
		{
			name:      "comm with spaces and nested parens",
			line:      makeProcStatLine(77, "my (odd) proc ) name", 'R', 12345),
			wantOK:    true,
			wantPID:   77,
			wantComm:  "my (odd) proc ) name",
			wantState: 'R',
			wantStart: 12345,
		},
		{
			name:      "zombie process",
			line:      makeProcStatLine(99, "qemu-system-aar", 'Z', 555),
			wantOK:    true,
			wantPID:   99,
			wantComm:  "qemu-system-aar",
			wantState: 'Z',
			wantStart: 555,
		},
		{
			name:   "missing parens",
			line:   "42 qemu S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 999",
			wantOK: false,
		},
		{
			name:   "too few fields",
			line:   "42 (qemu) S 1 2 3",
			wantOK: false,
		},
		{
			name:   "bad pid",
			line:   "abc (qemu) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 999",
			wantOK: false,
		},
		{
			name:   "bad start ticks",
			line:   "42 (qemu) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 notanumber",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, ok := parseProcStat(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("parseProcStat ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if st.pid != tc.wantPID || st.comm != tc.wantComm || st.state != tc.wantState || st.start != tc.wantStart {
				t.Fatalf("got %+v, want pid=%d comm=%q state=%c start=%d", st, tc.wantPID, tc.wantComm, tc.wantState, tc.wantStart)
			}
		})
	}
}

func TestProcStatAlive(t *testing.T) {
	cases := []struct {
		name   string
		st     procStat
		start  int64
		vmOnly bool
		want   bool
	}{
		{
			name:   "live truncated qemu matching start",
			st:     procStat{pid: 10, comm: "qemu-system-aar", state: 'S', start: 400},
			start:  400,
			vmOnly: true,
			want:   true,
		},
		{
			name:   "live full qemu",
			st:     procStat{pid: 10, comm: "qemu-system-aarch64", state: 'R', start: 400},
			start:  0,
			vmOnly: true,
			want:   true,
		},
		{
			name:   "live kido-vmm",
			st:     procStat{pid: 10, comm: vmmComm, state: 'S', start: 400},
			start:  400,
			vmOnly: true,
			want:   true,
		},
		{
			name:   "zombie qemu treated as dead",
			st:     procStat{pid: 10, comm: "qemu-system-aar", state: 'Z', start: 400},
			start:  400,
			vmOnly: true,
			want:   false,
		},
		{
			name:   "dead X state",
			st:     procStat{pid: 10, comm: "qemu-system-aar", state: 'X', start: 400},
			start:  400,
			vmOnly: false,
			want:   false,
		},
		{
			name:   "recycled pid start mismatch",
			st:     procStat{pid: 10, comm: "qemu-system-aar", state: 'S', start: 401},
			start:  400,
			vmOnly: true,
			want:   false,
		},
		{
			name:   "short unrelated comm rejected for vm",
			st:     procStat{pid: 10, comm: "qemu", state: 'S', start: 400},
			start:  400,
			vmOnly: true,
			want:   false,
		},
		{
			name:   "non-vm host process allowed when vmOnly false",
			st:     procStat{pid: 10, comm: "kido", state: 'S', start: 400},
			start:  400,
			vmOnly: false,
			want:   true,
		},
		{
			name:   "zombie host process rejected",
			st:     procStat{pid: 10, comm: "kido", state: 'Z', start: 400},
			start:  400,
			vmOnly: false,
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := procStatAlive(tc.st, tc.start, tc.vmOnly); got != tc.want {
				t.Fatalf("procStatAlive(%+v, %d, %v) = %v, want %v", tc.st, tc.start, tc.vmOnly, got, tc.want)
			}
		})
	}
}
