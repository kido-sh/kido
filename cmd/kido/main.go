package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
)

var version = "0.0.0-dev"

type command struct {
	run     func(ctx context.Context, args []string) int
	summary string
}

var commands = map[string]command{}

// register is called from each command file's init. Names starting with "__" are internal.
func register(name, summary string, run func(ctx context.Context, args []string) int) {
	commands[name] = command{run: run, summary: summary}
}

func hiddenCommand(name string) bool { return strings.HasPrefix(name, "__") }

func usage(w io.Writer) {
	fmt.Fprint(w, "Usage:  kido [OPTIONS] COMMAND\n\n"+
		"Run macOS arm64 command-line tools in Darwin micro-VMs\n\n"+
		"Options:\n"+
		"  -h, --help       Print usage\n"+
		"  -v, --version    Print version information and quit\n")
	var plain, groups []string
	for n := range commands {
		switch {
		case hiddenCommand(n):
		case ctrGroups[n] != nil:
			groups = append(groups, n)
		default:
			plain = append(plain, n)
		}
	}
	sort.Strings(plain)
	sort.Strings(groups)
	for _, sec := range []struct {
		title string
		names []string
	}{{"Management Commands", groups}, {"Commands", plain}} {
		if len(sec.names) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s:\n", sec.title)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, n := range sec.names {
			fmt.Fprintf(tw, "  %s\t%s\n", n, commands[n].summary)
		}
		tw.Flush()
	}
	fmt.Fprint(w, "\nRun 'kido COMMAND --help' for more information on a command.\n")
}

func main() {
	os.Exit(kidoMain(os.Args[1:]))
}

func kidoMain(args []string) int {
	if len(args) == 0 {
		usage(os.Stdout)
		return 0
	}
	switch args[0] {
	case "-h", "-help", "--help":
		usage(os.Stdout)
		return 0
	case "-v", "-version", "--version":
		fmt.Printf("kido version %s\n", version)
		return 0
	case "help":
		return ctrHelp(args[1:])
	}
	if strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(os.Stderr, "kido: unknown flag: %s\nSee 'kido --help'.\n", args[0])
		return 125
	}
	cmd, ok := commands[args[0]]
	if !ok {
		ctrUnknown("", args[0], ctrCommandNames())
		return 125
	}
	if len(args) > 1 && ctrIsHelpArg(args[1]) {
		return ctrHelp(args[:1])
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	return cmd.run(ctx, args[1:])
}
