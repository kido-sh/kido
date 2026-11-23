package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kido-sh/kido/internal/setup"
)

// checkUnpackedQEMU explains a QEMU that kido setup unpacked into $KIDO_HOME/qemu/<version> but did
// not make current, which is what a setup that stopped at missing libraries leaves behind. It
// returns nil when there is no such QEMU.
func checkUnpackedQEMU(home string) []checkResult {
	qdir := setup.QEMUDir(home)
	entries, err := os.ReadDir(qdir)
	if err != nil {
		return nil
	}
	var q setup.QEMU
	version := ""
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "current" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		bin := filepath.Join(qdir, e.Name(), "bin")
		if isExecutableFile(filepath.Join(bin, "qemu-system-aarch64")) != nil {
			continue
		}
		if version == "" || e.Name() == setup.PinnedQEMU.Version {
			version = e.Name()
			q = setup.QEMU{Path: filepath.Join(bin, "qemu-system-aarch64"), Img: filepath.Join(bin, "qemu-img"), Source: setup.SourceSetup}
		}
	}
	if version == "" {
		return nil
	}
	where := filepath.Join(qdir, version)
	libs := checkQEMULibraries(q)
	if libs.status == checkFail {
		return []checkResult{
			fail("QEMU", fmt.Sprintf("QEMU %s is unpacked in %s, but kido setup stopped there: libraries it needs are missing (below)", version, where), libs.fix),
			libs,
		}
	}
	return []checkResult{fail("QEMU", fmt.Sprintf("QEMU %s is unpacked in %s, but kido setup did not finish installing it", version, where),
		"run `KIDO_BACKEND=qemu kido setup` again and read the error it ends with")}
}
