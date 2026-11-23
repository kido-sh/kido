package main

import (
	"context"
	"fmt"
	"time"
)

func init() {
	register("stop", "Stop one or more running containers", ctrStop)
}

func ctrStop(ctx context.Context, args []string) int {
	fs := ctrFlags("stop", "[-t SECONDS] CONTAINER [CONTAINER...]")
	secs := fs.Int("t", 10, "seconds to wait for a clean shutdown before killing the VM")
	fs.IntVar(secs, "time", 10, "seconds to wait for a clean shutdown before killing the VM")
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
	timeout := time.Duration(*secs) * time.Second
	code := 0
	for _, ref := range fs.Args() {
		var err error
		if *secs <= 0 {
			err = rt.Kill(ctx, ref)
		} else {
			err = rt.Stop(ctx, ref, timeout)
		}
		if err != nil {
			code = ctrFail(err)
			continue
		}
		fmt.Println(ref)
	}
	return code
}
