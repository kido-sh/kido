package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	registerGroup("network", "Manage networks (vz backend, macOS 26+)",
		&ctrSub{Name: "create", summary: "Create a network", run: ctrNetworkCreate},
		&ctrSub{Name: "inspect", summary: "Display detailed information on one or more networks", run: ctrNetworkInspect},
		&ctrSub{Name: "ls", Aliases: []string{"list"}, summary: "List networks", run: ctrNetworkLs},
		&ctrSub{Name: "prune", summary: "Remove all networks no container uses", run: ctrNetworkPrune},
		&ctrSub{Name: "rm", Aliases: []string{"remove"}, summary: "Remove one or more networks", run: ctrNetworkRm},
	)
}

func ctrNetworkCreate(ctx context.Context, args []string) int {
	fs := ctrFlags("network create", "[OPTIONS] NETWORK")
	subnet := fs.String("subnet", "", "IPv4 subnet, e.g. 192.168.200.0/24 (default: the first free /24 from 192.168.128.0)")
	internal := fs.Bool("internal", false, "host-only: no route to the outside and no DNS; the host still reaches the containers")
	driver := fs.String("d", "vmnet", "driver (vmnet; bridge is accepted as its alias)")
	fs.StringVar(driver, "driver", "vmnet", "driver (vmnet; bridge is accepted as its alias)")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 125
	}
	if *driver != "vmnet" && *driver != "bridge" {
		return ctrFail(fmt.Errorf("driver %q is not supported: kido networks are vmnet networks", *driver))
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	n, err := rt.CreateNetwork(fs.Arg(0), *subnet, *internal)
	if err != nil {
		return ctrFail(err)
	}
	fmt.Println(n.ID)
	return 0
}

type ctrNetworkRow struct {
	ID        string `header:"NETWORK ID"`
	Name      string
	Driver    string
	Scope     string
	Subnet    string
	Gateway   string
	Internal  string
	CreatedAt string `header:"CREATED AT"`
}

func ctrNetworkLs(ctx context.Context, args []string) int {
	fs := ctrFlags("network ls", "[OPTIONS]")
	quiet := fs.Bool("q", false, "only display network IDs")
	fs.BoolVar(quiet, "quiet", false, "only display network IDs")
	noTrunc := fs.Bool("no-trunc", false, "don't truncate the output")
	format := fs.String("format", "", "format output with a Go template, 'table TEMPLATE' or 'json'")
	var filters ctrList
	fs.Var(&filters, "f", "filter output (name=, id=)")
	fs.Var(&filters, "filter", "filter output (name=, id=)")
	if err := fs.Parse(ctrExpand(ctrInterspersed(fs, args), "q", "format", "f", "filter")); err != nil {
		return 125
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 125
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	all, err := rt.Networks()
	if err != nil {
		return ctrFail(err)
	}
	var nets []*runtime.Network
	for _, n := range all {
		ok := true
		for _, f := range filters {
			k, v, _ := strings.Cut(f, "=")
			switch k {
			case "name":
				ok = ok && strings.Contains(n.Name, v)
			case "id":
				ok = ok && strings.HasPrefix(n.ID, v)
			default:
				return ctrFail(fmt.Errorf("unsupported filter %q (name, id)", f))
			}
		}
		if ok {
			nets = append(nets, n)
		}
	}
	id := func(n *runtime.Network) string {
		if *noTrunc {
			return n.ID
		}
		return n.ID[:12]
	}
	if *quiet && *format == "" {
		for _, n := range nets {
			fmt.Println(id(n))
		}
		return 0
	}
	if *format == "" {
		*format = "table {{.ID}}\t{{.Name}}\t{{.Driver}}\t{{.Subnet}}\t{{.Internal}}"
	}
	f, err := ctrNewFormatter(os.Stdout, *format, ctrHeader(ctrNetworkRow{}))
	if err != nil {
		return ctrFail(err)
	}
	defer f.Flush()
	for _, n := range nets {
		row := ctrNetworkRow{
			ID: id(n), Name: n.Name, Driver: "vmnet", Scope: "local", Subnet: n.Subnet, Gateway: n.Gateway,
			Internal: fmt.Sprint(n.Internal), CreatedAt: n.Created.Local().Format("2006-01-02 15:04:05 -0700 MST"),
		}
		if err := f.Row(row); err != nil {
			return ctrFail(err)
		}
	}
	return 0
}

// ctrNetworkView is what network inspect prints, in Docker's shape.
type ctrNetworkView struct {
	Name     string
	ID       string `json:"Id"`
	Created  time.Time
	Driver   string
	Internal bool
	IPAM     struct {
		Config []ctrIPAMConfig
	}
	Containers map[string]ctrNetworkMember
}

type ctrIPAMConfig struct{ Subnet, Gateway string }

type ctrNetworkMember struct {
	Name        string
	IPv4Address string
	State       string
}

func ctrNetworkInspect(ctx context.Context, args []string) int {
	fs := ctrFlags("network inspect", "[OPTIONS] NETWORK [NETWORK...]")
	format := fs.String("f", "", "format the output using a Go template, or 'json'")
	fs.StringVar(format, "format", "", "format the output using a Go template, or 'json'")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 125
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	out := []any{}
	code := 0
	for _, ref := range fs.Args() {
		n, err := rt.GetNetwork(ref)
		if err != nil {
			code = ctrFail(err)
			continue
		}
		v := ctrNetworkView{Name: n.Name, ID: n.ID, Created: n.Created, Driver: "vmnet", Internal: n.Internal,
			Containers: map[string]ctrNetworkMember{}}
		v.IPAM.Config = []ctrIPAMConfig{{Subnet: n.Subnet, Gateway: n.Gateway}}
		users, err := rt.NetworkContainers(n.Name)
		if err != nil {
			return ctrFail(err)
		}
		bits := netip.MustParsePrefix(n.Subnet).Bits()
		for _, u := range users {
			v.Containers[u.ID] = ctrNetworkMember{Name: u.Name, IPv4Address: fmt.Sprintf("%s/%d", u.IP, bits), State: ctrState(u)}
		}
		out = append(out, v)
	}
	if *format != "" {
		f, err := ctrNewFormatter(os.Stdout, *format, nil)
		if err != nil {
			return ctrFail(err)
		}
		for _, o := range out {
			if err := f.Row(o); err != nil {
				return ctrFail(err)
			}
		}
		return code
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "    ")
	enc.Encode(out)
	return code
}

func ctrNetworkRm(ctx context.Context, args []string) int {
	fs := ctrFlags("network rm", "NETWORK [NETWORK...]")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 125
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	code := 0
	for _, ref := range fs.Args() {
		if _, err := rt.RemoveNetwork(ref); err != nil {
			code = ctrFail(err)
			continue
		}
		fmt.Println(ref)
	}
	return code
}

func ctrNetworkPrune(ctx context.Context, args []string) int {
	_, force, ok := ctrPruneFlags("network prune", args, false)
	if !ok {
		return 125
	}
	if !force && !ctrConfirm("WARNING! This will remove all networks not used by at least one container.") {
		return 0
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	nets, err := rt.Networks()
	if err != nil {
		return ctrFail(err)
	}
	var removed []string
	for _, n := range nets {
		if users, err := rt.NetworkContainers(n.Name); err != nil || len(users) > 0 {
			continue
		}
		if _, err := rt.RemoveNetwork(n.Name); err == nil {
			removed = append(removed, n.Name)
		}
	}
	if len(removed) > 0 {
		fmt.Printf("Deleted Networks:\n%s\n", strings.Join(removed, "\n"))
	}
	return 0
}
