package runtime

import (
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

func newNetRuntime(t *testing.T) *Runtime {
	t.Helper()
	t.Setenv("KIDO_VZ_SUBNET", "")
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCreateNetwork(t *testing.T) {
	r := newNetRuntime(t)
	a, err := r.CreateNetwork("a", "", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.CreateNetwork("b", "", true)
	if err != nil {
		t.Fatal(err)
	}
	pa, pb := netip.MustParsePrefix(a.Subnet), netip.MustParsePrefix(b.Subnet)
	pool := netip.MustParsePrefix("192.168.128.0/17")
	if pa.Overlaps(pb) || !pool.Contains(pa.Addr()) || !pool.Contains(pb.Addr()) || pa.Bits() != 24 {
		t.Fatalf("subnets %s and %s", pa, pb)
	}
	if a.Gateway != addAddr(pa.Addr(), 1).String() || !b.Internal || len(a.ID) != 64 {
		t.Fatalf("network a %+v, b %+v", a, b)
	}
	if got, err := r.GetNetwork(a.ID[:12]); err != nil || got.Name != "a" {
		t.Fatalf("by ID prefix: %v, %v", got, err)
	}
	if _, err := r.GetNetwork("nope"); !errors.Is(err, ErrNoNetwork) {
		t.Fatalf("missing network: %v", err)
	}
	for _, c := range []struct{ name, subnet, want string }{
		{"a", "", "already exists"},
		{"none", "", "invalid network name"},
		{"bridge", "", "invalid network name"},
		{"bad/name", "", "invalid network name"},
		{"c", "192.168.200.5/24", "did you mean 192.168.200.0/24"},
		{"c", "10.0.0.0/8", "want an IPv4 network"},
		{"c", "192.168.64.0/24", "overlaps 192.168.64.0/24"},
		{"c", pa.String(), "overlaps " + pa.String()},
	} {
		if _, err := r.CreateNetwork(c.name, c.subnet, false); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("CreateNetwork(%q, %q) = %v, want %q", c.name, c.subnet, err, c.want)
		}
	}
	if n, err := r.CreateNetwork("c", "10.77.0.0/16", false); err != nil || n.Gateway != "10.77.0.1" {
		t.Fatalf("explicit subnet: %+v, %v", n, err)
	}
	nets, _ := r.Networks()
	if len(nets) != 3 || nets[0].Name != "a" || nets[2].Name != "c" {
		t.Fatalf("Networks() = %v", nets)
	}
}

func TestVmnetPlace(t *testing.T) {
	r := newNetRuntime(t)
	n, err := r.CreateNetwork("n", "192.168.250.0/29", false)
	if err != nil {
		t.Fatal(err)
	}
	p := netip.MustParsePrefix(n.Subnet)
	var all []*Info
	seen := map[string]bool{}
	for _, id := range []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb", "cccccccccccc", "dddddddddddd", "eeeeeeeeeeee"} {
		info := &Info{ID: id, Network: "n"}
		if err := r.vmnetPlace(info, all); err != nil {
			t.Fatal(err)
		}
		a := netip.MustParseAddr(info.IP)
		if !p.Contains(a) || a == p.Addr() || a.String() == n.Gateway || a == lastAddr(p) || seen[info.IP] {
			t.Fatalf("%s got %s in %s (taken %v)", id, a, p, seen)
		}
		seen[info.IP] = true
		all = append(all, info)
	}
	// A /29 has 5 addresses for containers; the sixth container finds none.
	if err := r.vmnetPlace(&Info{ID: "ffffffffffff", Network: "n"}, all); err == nil {
		t.Fatal("placed a sixth container in a /29")
	}

	none := &Info{ID: "111111111111", Network: NetworkNone}
	if err := r.vmnetPlace(none, all); err != nil {
		t.Fatal(err)
	}
	ps := netip.MustParsePrefix(none.Subnet)
	if ps.Overlaps(p) || none.IP != addAddr(ps.Addr(), 2).String() {
		t.Fatalf("none: subnet %s, ip %s", none.Subnet, none.IP)
	}
	// The next network skips the subnet that the none container holds.
	all = append(all, none)
	if err := os.Mkdir(r.dir(none.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	none.Name, none.Created = "lonely", time.Now()
	if err := r.save(none); err != nil {
		t.Fatal(err)
	}
	m, err := r.CreateNetwork("m", "", false)
	if err != nil || netip.MustParsePrefix(m.Subnet).Overlaps(ps) {
		t.Fatalf("network m %v overlaps the none subnet %s: %v", m, ps, err)
	}
}

func TestVmnetAddress(t *testing.T) {
	r := newNetRuntime(t)
	if _, err := r.CreateNetwork("n", "10.9.8.0/23", false); err != nil {
		t.Fatal(err)
	}
	nw, err := r.vmnetAddress(&Info{Name: "c", Network: "n", IP: "10.9.9.7"})
	if err != nil {
		t.Fatal(err)
	}
	if nw.Gateway.String() != "10.9.8.1" || nw.Mask != "255.255.254.0" || nw.MAC != "02:dc:72:09:09:07" {
		t.Fatalf("got %+v", nw)
	}
	if _, err := r.vmnetAddress(&Info{Name: "c", Network: "n", IP: "10.9.10.7"}); err == nil {
		t.Fatal("accepted an address outside the subnet")
	}
}

func TestRemoveNetworkInUse(t *testing.T) {
	r := newNetRuntime(t)
	if _, err := r.CreateNetwork("n", "", false); err != nil {
		t.Fatal(err)
	}
	info := &Info{ID: "aaaaaaaaaaaa", Name: "web", Network: "n", Created: time.Now()}
	if err := os.Mkdir(r.dir(info.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.save(info); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RemoveNetwork("n"); err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("removed a network in use: %v", err)
	}
	if err := os.RemoveAll(r.dir(info.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RemoveNetwork("n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.networkBase("n") + ".json"); !os.IsNotExist(err) {
		t.Fatalf("definition left behind: %v", err)
	}
}

func TestGetNetworkNameBeforePrefix(t *testing.T) {
	r := newNetRuntime(t)
	for i, n := range []Network{
		{ID: "ab" + strings.Repeat("1", 62), Name: "a1", Subnet: "10.1.0.0/24"},
		{ID: "ab" + strings.Repeat("2", 62), Name: "a2", Subnet: "10.2.0.0/24"},
		{ID: "cd" + strings.Repeat("3", 62), Name: "ab", Subnet: "10.3.0.0/24"},
		{ID: "short", Name: "broken", Subnet: "10.4.0.0/24"},
	} {
		n.Created = time.Now()
		if err := writeJSON(r.networkBase(n.Name)+".json", &n); err != nil {
			t.Fatal(i, err)
		}
	}
	if n, err := r.GetNetwork("ab"); err != nil || n.Name != "ab" {
		t.Fatalf(`GetNetwork("ab") = %v, %v`, n, err)
	}
	if _, err := r.GetNetwork("ab1"); err != nil {
		t.Fatalf("unique ID prefix: %v", err)
	}
	if _, err := r.GetNetwork("broken"); !errors.Is(err, ErrNoNetwork) {
		t.Fatalf("a definition with a bad ID was used: %v", err)
	}
}
