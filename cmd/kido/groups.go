package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"text/tabwriter"
)

// ctrSub is a subcommand of a management command (kido image ls, kido container prune, ...): either
// its own function or an existing top-level command.
type ctrSub struct {
	Name    string
	Aliases []string
	summary string
	cmd     string
	run     func(ctx context.Context, args []string) int
}

func (s *ctrSub) Summary() string {
	if s.summary != "" {
		return s.summary
	}
	return commands[s.cmd].summary
}

func (s *ctrSub) Run(ctx context.Context, args []string) int {
	if s.run != nil {
		return s.run(ctx, args)
	}
	if c, ok := commands[s.cmd]; ok {
		return c.run(ctx, args)
	}
	fmt.Fprintf(os.Stderr, "kido: %s is not available in this build\n", s.cmd)
	return 125
}

var ctrGroups = map[string][]*ctrSub{}

func registerGroup(name, summary string, subs ...*ctrSub) {
	ctrGroups[name] = subs
	register(name, summary, func(ctx context.Context, args []string) int { return ctrGroupMain(ctx, name, args) })
}

func ctrFindSub(group, name string) *ctrSub {
	for _, s := range ctrGroups[group] {
		if s.Name == name || slices.Contains(s.Aliases, name) {
			return s
		}
	}
	return nil
}

func ctrGroupMain(ctx context.Context, group string, args []string) int {
	if len(args) == 0 || ctrIsHelpArg(args[0]) {
		ctrGroupUsage(group)
		return 0
	}
	s := ctrFindSub(group, args[0])
	if s == nil {
		var names []string
		for _, s := range ctrGroups[group] {
			names = append(names, s.Name)
		}
		ctrUnknown(group, args[0], names)
		return 125
	}
	return s.Run(ctx, args[1:])
}

func ctrGroupUsage(group string) {
	fmt.Printf("Usage:  kido %s COMMAND\n\n%s\n\nCommands:\n", group, commands[group].summary)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, s := range ctrGroups[group] {
		fmt.Fprintf(tw, "  %s\t%s\n", s.Name, s.Summary())
	}
	tw.Flush()
	fmt.Printf("\nRun 'kido %s COMMAND --help' for more information on a command.\n", group)
}

func init() {
	inspectAs := func(typ string) func(context.Context, []string) int {
		return func(ctx context.Context, args []string) int {
			return ctrInspect(ctx, append([]string{"--type", typ}, args...))
		}
	}
	registerGroup("image", "Manage images",
		&ctrSub{Name: "build", cmd: "build"},
		&ctrSub{Name: "history", cmd: "history"},
		&ctrSub{Name: "import", summary: "Import a directory holding disk.img, kernel and initrd.cpio as an image", run: ctrImageImport},
		&ctrSub{Name: "inspect", summary: "Display detailed information on one or more images", run: inspectAs("image")},
		&ctrSub{Name: "load", cmd: "load"},
		&ctrSub{Name: "ls", Aliases: []string{"list"}, cmd: "images"},
		&ctrSub{Name: "prune", summary: "Remove unused images", run: ctrImagePrune},
		&ctrSub{Name: "pull", cmd: "pull"},
		&ctrSub{Name: "push", cmd: "push"},
		&ctrSub{Name: "rm", Aliases: []string{"remove"}, cmd: "rmi"},
		&ctrSub{Name: "save", cmd: "save"},
		&ctrSub{Name: "tag", cmd: "tag"},
	)
	registerGroup("container", "Manage containers",
		&ctrSub{Name: "commit", cmd: "commit"},
		&ctrSub{Name: "cp", cmd: "cp"},
		&ctrSub{Name: "create", cmd: "create"},
		&ctrSub{Name: "exec", cmd: "exec"},
		&ctrSub{Name: "export", cmd: "export"},
		&ctrSub{Name: "inspect", summary: "Display detailed information on one or more containers", run: inspectAs("container")},
		&ctrSub{Name: "kill", cmd: "kill"},
		&ctrSub{Name: "logs", cmd: "logs"},
		&ctrSub{Name: "ls", Aliases: []string{"list", "ps"}, cmd: "ps"},
		&ctrSub{Name: "port", cmd: "port"},
		&ctrSub{Name: "prune", summary: "Remove all stopped containers", run: ctrContainerPrune},
		&ctrSub{Name: "rename", cmd: "rename"},
		&ctrSub{Name: "restart", cmd: "restart"},
		&ctrSub{Name: "rm", Aliases: []string{"remove"}, cmd: "rm"},
		&ctrSub{Name: "run", cmd: "run"},
		&ctrSub{Name: "start", cmd: "start"},
		&ctrSub{Name: "stop", cmd: "stop"},
		&ctrSub{Name: "top", cmd: "top"},
		&ctrSub{Name: "wait", cmd: "wait"},
	)
	registerGroup("system", "Manage kido",
		&ctrSub{Name: "df", summary: "Show kido disk usage", run: ctrSystemDf},
		&ctrSub{Name: "info", cmd: "info"},
		&ctrSub{Name: "prune", summary: "Remove stopped containers and, with -a, unused images", run: ctrSystemPrune},
	)
}
