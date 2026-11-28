package main

import (
	"context"
	"fmt"
	"os"
)

func init() {
	register("probe", "Probe host CPU, KVM, and GPU capabilities for kido", ctrProbe)
}

func ctrProbe(ctx context.Context, args []string) int {
	fl := ctrFlags("probe", "[OPTIONS]")
	asJSON := fl.Bool("json", false, "Emit a board capability JSON record")
	noCoh := fl.Bool("no-coherency-test", false, "Skip the live GPU DMA coherency test (Linux)")
	forceCoh := fl.Bool("force-coherency-test", false, "Force the live GPU DMA coherency test (Linux)")
	if err := fl.Parse(args); err != nil {
		return 125
	}
	if fl.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "kido: \"probe\" accepts no arguments.\nSee 'kido probe --help'.")
		return 125
	}
	if *noCoh && *forceCoh {
		fmt.Fprintln(os.Stderr, "kido: --no-coherency-test and --force-coherency-test are mutually exclusive.\nSee 'kido probe --help'.")
		return 125
	}
	var passArgs []string
	if *asJSON {
		passArgs = append(passArgs, "--json")
	}
	if *noCoh {
		passArgs = append(passArgs, "--no-coherency-test")
	}
	if *forceCoh {
		passArgs = append(passArgs, "--force-coherency-test")
	}
	return runPlatformProbe(ctx, passArgs)
}
