package main

import (
	"context"
	"fmt"
)

func init() {
	register("rm", "Remove one or more containers", ctrRm)
}

func ctrRm(ctx context.Context, args []string) int {
	fs := ctrFlags("rm", "[-f] CONTAINER [CONTAINER...]")
	force := fs.Bool("f", false, "kill and remove a running container")
	fs.BoolVar(force, "force", false, "kill and remove a running container")
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
		if err := rt.Remove(ctx, ref, *force); err != nil {
			code = ctrFail(err)
			continue
		}
		fmt.Println(ref)
	}
	return code
}
