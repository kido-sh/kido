package runtime

import (
	"net/netip"
	"os"
	"testing"
	"time"
)

func TestVZAddress(t *testing.T) {
	t.Setenv("KIDO_VZ_SUBNET", "")
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id string) {
		if err := os.Mkdir(r.dir(id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := r.save(&Info{ID: id, Name: "c" + id, Backend: backendVZ, Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	mk("aaaaaaaaaaaa")
	mk("bbbbbbbbbbbb")
	n1, err := r.vzAddress("aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := netip.MustParseAddr("192.168.64.128"), netip.MustParseAddr("192.168.64.254")
	if n1.Guest.Less(lo) || hi.Less(n1.Guest) || n1.Gateway.String() != "192.168.64.1" {
		t.Fatalf("got %v via %v", n1.Guest, n1.Gateway)
	}
	if again, _ := r.vzAddress("aaaaaaaaaaaa"); again.Guest != n1.Guest || again.MAC != n1.MAC {
		t.Fatalf("address moved: %v then %v", n1, again)
	}
	// b's fresh vz.ip claims a's address, so a must move; b keeps it.
	if err := os.WriteFile(r.file("bbbbbbbbbbbb", "vz.ip"), []byte(n1.Guest.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if n2, _ := r.vzAddress("aaaaaaaaaaaa"); n2.Guest == n1.Guest {
		t.Fatalf("a kept %v although b holds it", n2.Guest)
	}
	if nb, _ := r.vzAddress("bbbbbbbbbbbb"); nb.Guest != n1.Guest {
		t.Fatalf("b lost its address: %v", nb.Guest)
	}

	t.Setenv("KIDO_VZ_SUBNET", "10.33.0.0/29")
	n3, err := r.vzAddress("aaaaaaaaaaaa")
	if err != nil || n3.Gateway.String() != "10.33.0.1" || !netip.MustParsePrefix("10.33.0.0/29").Contains(n3.Guest) {
		t.Fatalf("small subnet: %v, %v", n3, err)
	}
	t.Setenv("KIDO_VZ_SUBNET", "fd00::/64")
	if _, err := r.vzAddress("aaaaaaaaaaaa"); err == nil {
		t.Fatal("IPv6 subnet accepted")
	}
}
