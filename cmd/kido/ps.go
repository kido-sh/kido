package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("ps", "List containers", ctrPs)
}

func ctrAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Second:
		return "Less than a second"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < 2*time.Minute:
		return "About a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 2*time.Hour:
		return "About an hour"
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

func ctrExitCode(i *runtime.Info) int {
	if i.ExitCode != nil {
		return *i.ExitCode
	}
	if i.Error != "" {
		return 125
	}
	return 0
}

func ctrStatus(i *runtime.Info) string {
	switch i.State {
	case engine.StateRunning:
		return "Up " + ctrAgo(i.StartedAt)
	case engine.StateStopped:
		if i.FinishedAt.IsZero() {
			return fmt.Sprintf("Exited (%d)", ctrExitCode(i))
		}
		return fmt.Sprintf("Exited (%d) %s ago", ctrExitCode(i), ctrAgo(i.FinishedAt))
	}
	return "Created"
}

// ctrState is the one-word state: created, running or exited.
func ctrState(i *runtime.Info) string {
	if i.State == engine.StateStopped {
		return "exited"
	}
	return string(i.State)
}

func ctrPorts(ports []engine.PortMapping) string {
	var out []string
	for _, p := range ports {
		out = append(out, fmt.Sprintf("%s:%d->%d/%s", p.HostIP, p.HostPort, p.GuestPort, p.Protocol))
	}
	return strings.Join(out, ", ")
}

func ctrCommand(i *runtime.Info, trunc bool) string {
	if i.Main == nil || len(i.Main.Args) == 0 {
		return `""`
	}
	s := strings.Join(i.Main.Args, " ")
	if r := []rune(s); trunc && len(r) > 20 {
		s = string(r[:19]) + "…"
	}
	return `"` + s + `"`
}

func ctrLabels(m map[string]string) string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

type ctrPsRow struct {
	ID         string `header:"CONTAINER ID"`
	Image      string
	Command    string
	CreatedAt  string `header:"CREATED AT"`
	RunningFor string `header:"CREATED"`
	Status     string
	Ports      string
	Names      string
	State      string
	Labels     string
	Size       string
	Backend    string
	Networks   string

	labels map[string]string
}

// Label is the value of one label, for templates such as {{.Label "key"}}.
func (r ctrPsRow) Label(k string) string { return r.labels[k] }

// ctrNetworkName is the container's network: a user network, none, or default.
func ctrNetworkName(i *runtime.Info) string {
	if i.Network == "" {
		return "default"
	}
	return i.Network
}

// ctrPsFilter is one --filter KEY=VALUE.
func ctrPsFilter(i *runtime.Info, f string) (bool, error) {
	k, v, ok := strings.Cut(f, "=")
	if !ok {
		return false, fmt.Errorf("bad filter %q: want KEY=VALUE", f)
	}
	switch k {
	case "id":
		return strings.HasPrefix(i.ID, v), nil
	case "name":
		if m, err := path.Match(v, i.Name); err == nil && m {
			return true, nil
		}
		return strings.Contains(i.Name, v), nil
	case "status":
		switch v {
		case "created", "running", "exited":
			return ctrState(i) == v, nil
		}
		return false, fmt.Errorf("bad status filter %q: want created, running or exited", v)
	case "label":
		lk, lv, hasValue := strings.Cut(v, "=")
		got, ok := i.Labels[lk]
		return ok && (!hasValue || got == lv), nil
	case "ancestor":
		return i.Image == v || i.Image == v+":latest", nil
	case "exited":
		return i.State == engine.StateStopped && fmt.Sprint(ctrExitCode(i)) == v, nil
	case "network":
		return ctrNetworkName(i) == v || v == "bridge" && i.Network == "", nil
	}
	return false, fmt.Errorf("unsupported filter %q (id, name, status, label, ancestor, exited, network)", k)
}

func ctrPs(ctx context.Context, args []string) int {
	fs := ctrFlags("ps", "[OPTIONS]")
	all := fs.Bool("a", false, "show all containers (default shows just running)")
	fs.BoolVar(all, "all", false, "show all containers (default shows just running)")
	quiet := fs.Bool("q", false, "only display container IDs")
	fs.BoolVar(quiet, "quiet", false, "only display container IDs")
	format := fs.String("format", "", "format output with a Go template, 'table TEMPLATE' or 'json'")
	last := fs.Int("n", -1, "show the n last created containers (includes all states)")
	fs.IntVar(last, "last", -1, "show the n last created containers (includes all states)")
	latest := fs.Bool("l", false, "show the latest created container (includes all states)")
	fs.BoolVar(latest, "latest", false, "show the latest created container (includes all states)")
	noTrunc := fs.Bool("no-trunc", false, "don't truncate output")
	size := fs.Bool("s", false, "display total file sizes")
	fs.BoolVar(size, "size", false, "display total file sizes")
	var filters ctrList
	fs.Var(&filters, "f", "filter output (id=, name=, status=, label=, ancestor=, exited=, network=)")
	fs.Var(&filters, "filter", "filter output (id=, name=, status=, label=, ancestor=, exited=, network=)")
	if err := fs.Parse(ctrExpand(args, "aqls", "format", "n", "last", "f", "filter")); err != nil {
		return 125
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 125
	}
	if *latest {
		*last = 1
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	infos, err := rt.ListInfo(*all || *last > 0 || len(filters) > 0)
	if err != nil {
		return ctrFail(err)
	}
	var shown []*runtime.Info
	for _, i := range infos {
		ok := true
		for _, f := range filters {
			m, err := ctrPsFilter(i, f)
			if err != nil {
				return ctrFail(err)
			}
			ok = ok && m
		}
		if ok && (*all || *last > 0 || i.State == engine.StateRunning || ctrHasStatusFilter(filters)) {
			shown = append(shown, i)
		}
	}
	if *last > 0 && len(shown) > *last {
		shown = shown[:*last]
	}
	if *quiet && *format == "" {
		for _, i := range shown {
			fmt.Println(i.ID)
		}
		return 0
	}
	if *format == "" {
		*format = "table {{.ID}}\t{{.Image}}\t{{.Command}}\t{{.RunningFor}}\t{{.Status}}\t{{.Ports}}\t{{.Names}}"
		if *size {
			*format += "\t{{.Size}}"
		}
	}
	f, err := ctrNewFormatter(os.Stdout, *format, ctrHeader(ctrPsRow{}))
	if err != nil {
		return ctrFail(err)
	}
	defer f.Flush()
	for _, i := range shown {
		row := ctrPsRow{
			ID: i.ID, Image: i.Image, Command: runtime.Sanitize(ctrCommand(i, !*noTrunc)),
			CreatedAt: i.Created.Local().Format("2006-01-02 15:04:05 -0700 MST"), RunningFor: ctrAgo(i.Created) + " ago",
			Status: ctrStatus(i), Ports: ctrPorts(i.Ports), Names: i.Name, State: ctrState(i),
			Labels: runtime.Sanitize(ctrLabels(i.Labels)), Backend: i.Backend, Networks: ctrNetworkName(i), labels: i.Labels,
		}
		if *size || strings.Contains(*format, ".Size") {
			row.Size = ctrSize(rt.ContainerDiskUsage(i.ID))
		}
		if err := f.Row(row); err != nil {
			return ctrFail(err)
		}
	}
	return 0
}

func ctrHasStatusFilter(filters []string) bool {
	for _, f := range filters {
		if strings.HasPrefix(f, "status=") || strings.HasPrefix(f, "exited=") {
			return true
		}
	}
	return false
}
