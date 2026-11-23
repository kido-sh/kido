package main

import (
	"context"
	"fmt"
	"os"
	goruntime "runtime"
	"runtime/debug"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("version", "Show the kido version information", ctrVersion)
	register("info", "Display system-wide information", ctrInfo)
}

type ctrVersionInfo struct {
	Client struct {
		Version   string
		GoVersion string
		GitCommit string
		Built     string
		Os        string
		Arch      string
	}
	Backend struct {
		Name    string
		Version string
		Path    string
	}
}

func ctrBuildSetting(key string) string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == key {
				return s.Value
			}
		}
	}
	return ""
}

func ctrBackendPath(rt *runtime.Runtime) string {
	if rt.Backend == "" || rt.Backend == "qemu" {
		return rt.QEMU
	}
	return ""
}

func ctrVersion(ctx context.Context, args []string) int {
	fs := ctrFlags("version", "[OPTIONS]")
	format := fs.String("format", "", "format output with a Go template or 'json'")
	fs.StringVar(format, "f", "", "format output with a Go template or 'json'")
	if err := fs.Parse(args); err != nil {
		return 125
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 125
	}
	var v ctrVersionInfo
	v.Client.Version = version
	v.Client.GoVersion = goruntime.Version()
	v.Client.GitCommit = ctrBuildSetting("vcs.revision")
	v.Client.Built = ctrBuildSetting("vcs.time")
	v.Client.Os, v.Client.Arch = goruntime.GOOS, goruntime.GOARCH
	if rt, err := ctrOpen(); err == nil {
		v.Backend.Name = rt.Backend
		v.Backend.Path = ctrBackendPath(rt)
		v.Backend.Version, _ = rt.BackendVersion(ctx)
	}
	if *format != "" {
		f, err := ctrNewFormatter(os.Stdout, *format, nil)
		if err != nil {
			return ctrFail(err)
		}
		if err := f.Row(v); err != nil {
			return ctrFail(err)
		}
		return 0
	}
	fmt.Printf("Client:\n Version:     %s\n Go version:  %s\n", v.Client.Version, v.Client.GoVersion)
	if v.Client.GitCommit != "" {
		fmt.Printf(" Git commit:  %.12s\n", v.Client.GitCommit)
	}
	if v.Client.Built != "" {
		fmt.Printf(" Built:       %s\n", v.Client.Built)
	}
	fmt.Printf(" OS/Arch:     %s/%s\n", v.Client.Os, v.Client.Arch)
	if v.Backend.Name != "" {
		fmt.Printf("\nBackend:\n Name:        %s\n", v.Backend.Name)
		if v.Backend.Version != "" {
			fmt.Printf(" Version:     %s\n", v.Backend.Version)
		}
		if v.Backend.Path != "" {
			fmt.Printf(" Path:        %s\n", v.Backend.Path)
		}
	}
	return 0
}

type ctrInfoData struct {
	Version           string
	Backend           string
	BackendPath       string
	BackendVersion    string
	RootDir           string
	Containers        int
	ContainersRunning int
	ContainersStopped int
	Images            int
	ImagesSize        int64
	ContainersSize    int64
	FreeSpace         int64
	OperatingSystem   string
	KernelVersion     string
	Architecture      string
	NCPU              int
	MemTotal          int64
}

func ctrSysctl(name string) string {
	s, err := unix.Sysctl(name)
	if err != nil {
		return ""
	}
	return s
}

func ctrInfo(ctx context.Context, args []string) int {
	fs := ctrFlags("info", "[OPTIONS]")
	format := fs.String("format", "", "format output with a Go template or 'json'")
	fs.StringVar(format, "f", "", "format output with a Go template or 'json'")
	if err := fs.Parse(args); err != nil {
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
	u, err := rt.DiskUsage(ctx)
	if err != nil {
		return ctrFail(err)
	}
	d := ctrInfoData{
		Version: version, Backend: rt.Backend, BackendPath: ctrBackendPath(rt), RootDir: rt.Home,
		Containers: u.Containers, ContainersRunning: u.Running, ContainersStopped: u.Containers - u.Running,
		Images: u.Images, ImagesSize: u.ImageBytes, ContainersSize: u.ContainerBytes, FreeSpace: u.FreeBytes,
		OperatingSystem: strings.TrimSpace("macOS " + ctrSysctl("kern.osproductversion")),
		KernelVersion:   ctrSysctl("kern.osrelease"), Architecture: goruntime.GOARCH, NCPU: goruntime.NumCPU(),
	}
	if mem, err := unix.SysctlUint64("hw.memsize"); err == nil {
		d.MemTotal = int64(mem)
	}
	d.BackendVersion, _ = rt.BackendVersion(ctx)
	if *format != "" {
		f, err := ctrNewFormatter(os.Stdout, *format, nil)
		if err != nil {
			return ctrFail(err)
		}
		if err := f.Row(d); err != nil {
			return ctrFail(err)
		}
		return 0
	}
	fmt.Printf("Client:\n Version:    %s\n\n", d.Version)
	fmt.Printf("Server:\n Containers: %d\n  Running: %d\n  Stopped: %d\n Images: %d\n", d.Containers, d.ContainersRunning, d.ContainersStopped, d.Images)
	fmt.Printf(" Backend: %s\n", d.Backend)
	if d.BackendPath != "" {
		fmt.Printf(" Backend Path: %s\n", d.BackendPath)
	}
	if d.BackendVersion != "" {
		fmt.Printf(" Backend Version: %s\n", d.BackendVersion)
	}
	fmt.Printf(" Root Dir (KIDO_HOME): %s\n", d.RootDir)
	fmt.Printf(" Disk Use: %s (images %s, containers %s; clones counted in full)\n",
		ctrSize(d.ImagesSize+d.ContainersSize), ctrSize(d.ImagesSize), ctrSize(d.ContainersSize))
	fmt.Printf(" Free Space: %s\n", ctrSize(d.FreeSpace))
	fmt.Printf(" Operating System: %s\n Kernel Version: %s\n Architecture: %s\n CPUs: %d\n Total Memory: %.4gGiB\n",
		d.OperatingSystem, d.KernelVersion, d.Architecture, d.NCPU, float64(d.MemTotal)/(1<<30))
	return 0
}

func ctrSystemDf(ctx context.Context, args []string) int {
	fs := ctrFlags("system df", "[OPTIONS]")
	if err := fs.Parse(args); err != nil {
		return 125
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	imgs, err := rt.Images(ctx)
	if err != nil {
		return ctrFail(err)
	}
	var imgSize, imgFree int64
	active := 0
	for _, i := range imgs {
		imgSize += i.Size
		if users, _ := rt.ImageUsers(i.Name + ":" + i.Tag); len(users) > 0 {
			active++
		} else {
			imgFree += i.Size
		}
	}
	infos, err := rt.ListInfo(true)
	if err != nil {
		return ctrFail(err)
	}
	var ctrBytes, ctrFree int64
	running := 0
	for _, i := range infos {
		n := rt.ContainerDiskUsage(i.ID)
		ctrBytes += n
		if i.State == "running" {
			running++
		} else {
			ctrFree += n
		}
	}
	pct := func(part, whole int64) string {
		if whole == 0 {
			return "0%"
		}
		return fmt.Sprintf("%d%%", part*100/whole)
	}
	f, _ := ctrNewFormatter(os.Stdout, "table {{.Type}}\t{{.Total}}\t{{.Active}}\t{{.Size}}\t{{.Reclaimable}}", map[string]string{
		"Type": "TYPE", "Total": "TOTAL", "Active": "ACTIVE", "Size": "SIZE", "Reclaimable": "RECLAIMABLE",
	})
	f.Row(map[string]string{"Type": "Images", "Total": fmt.Sprint(len(imgs)), "Active": fmt.Sprint(active),
		"Size": ctrSize(imgSize), "Reclaimable": fmt.Sprintf("%s (%s)", ctrSize(imgFree), pct(imgFree, imgSize))})
	f.Row(map[string]string{"Type": "Containers", "Total": fmt.Sprint(len(infos)), "Active": fmt.Sprint(running),
		"Size": ctrSize(ctrBytes), "Reclaimable": fmt.Sprintf("%s (%s)", ctrSize(ctrFree), pct(ctrFree, ctrBytes))})
	f.Flush()
	return 0
}
