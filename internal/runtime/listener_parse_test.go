package runtime

import (
	"slices"
	"testing"
)

func TestParseProcNetTCPInodes(t *testing.T) {
	tcpSample := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 123456 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 0100007F:C350 01 00000000:00000000 00:00000000 00000000  1000        0 222222 1 0000000000000000 20 4 30 10 -1
   2: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 333333 1 0000000000000000 100 0 0 10 0
   3: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 0 1 0000000000000000 100 0 0 10 0
   4: 0100007f:1f90 00000000:0000 0a 00000000:00000000 00:00000000 00000000  1000        0 987654 1 0000000000000000 100 0 0 10 0
   5: 0100007F:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 444444 1 0000000000000000 100 0 0 10 0
`
	got := parseProcNetTCPInodes(tcpSample, 8080, false)
	want := []string{"123456", "987654"}
	if !slices.Equal(got, want) {
		t.Fatalf("ipv4 port 8080: got %v, want %v", got, want)
	}
	if got := parseProcNetTCPInodes(tcpSample, 80, false); !slices.Equal(got, []string{"444444"}) {
		t.Fatalf("ipv4 port 80: got %v", got)
	}
	if got := parseProcNetTCPInodes(tcpSample, 9999, false); len(got) != 0 {
		t.Fatalf("ipv4 missing port: got %v", got)
	}
	if got := parseProcNetTCPInodes(tcpSample, 0, false); len(got) != 0 {
		t.Fatalf("invalid port 0: got %v", got)
	}

	tcp6Sample := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0000000000000000FFFF00000100007F:9C40 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 555666 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:9C40 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 777888 1 0000000000000000 100 0 0 10 0
`
	if got := parseProcNetTCPInodes(tcp6Sample, 40000, true); !slices.Equal(got, []string{"555666"}) {
		t.Fatalf("ipv6-mapped loopback port 40000: got %v, want [555666]", got)
	}
}
