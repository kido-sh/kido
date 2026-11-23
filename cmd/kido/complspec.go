package main

import "sort"

// ctrCompletion describes one command for shell completion scripts.
type ctrCompletion struct {
	Name    string
	Summary string
	Options []ctrOption
	// Args is what positional arguments complete to: "container", "image", "both", "network" or ""
	// (files).
	Args string
	Subs []ctrCompletion
}

// ctrCompletionArgs says what the positional arguments of each command name.
var ctrCompletionArgs = map[string]string{
	"attach": "container", "commit": "container", "cp": "container", "exec": "container",
	"inspect": "both", "kill": "container", "logs": "container", "port": "container",
	"rename": "container", "restart": "container", "rm": "container", "start": "container",
	"stop": "container", "wait": "container", "top": "container", "export": "container",
	"create": "image", "run": "image", "rmi": "image", "tag": "image", "save": "image",
	"push": "image", "history": "image",
}

// ctrCompletionSpecs is the command table as completion scripts need it, sorted by name, without
// internal commands.
func ctrCompletionSpecs() []ctrCompletion {
	var out []ctrCompletion
	for _, name := range ctrCommandNames() {
		c := ctrCompletion{Name: name, Summary: commands[name].summary, Args: ctrCompletionArgs[name]}
		if subs, ok := ctrGroups[name]; ok {
			for _, s := range subs {
				sc := ctrCompletion{Name: s.Name, Summary: s.Summary(), Args: ctrCompletionArgs[s.Name]}
				if s.cmd != "" {
					sc.Args = ctrCompletionArgs[s.cmd]
				}
				if name == "image" && (s.Name == "inspect" || s.Name == "rm") {
					sc.Args = "image"
				}
				if name == "container" && s.Name == "inspect" {
					sc.Args = "container"
				}
				if name == "network" {
					sc.Args = map[string]string{"inspect": "network", "rm": "network"}[s.Name]
				}
				sc.Options = ctrCommandFlags(s.Run, nil)
				c.Subs = append(c.Subs, sc)
			}
			sort.Slice(c.Subs, func(i, j int) bool { return c.Subs[i].Name < c.Subs[j].Name })
		} else {
			c.Options = ctrCommandFlags(commands[name].run, nil)
		}
		out = append(out, c)
	}
	return out
}
