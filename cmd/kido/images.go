package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kido-sh/kido/internal/engine"
	"github.com/kido-sh/kido/internal/runtime"
)

func init() {
	register("images", "List images", ctrImages)
	register("rmi", "Remove one or more images", ctrRmi)
	register("tag", "Create a tag TARGET_IMAGE that refers to SOURCE_IMAGE", ctrTag)
}

func ctrSize(n int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	return fmt.Sprintf("%.3g%s", f, units[i])
}

// ctrImageID is a stable identity of an image's content: the sha256 of the config the registry
// code pushes for it (creation time, root UUID, parent, config), so tags of one image share it.
func ctrImageID(i *engine.Image) string {
	b, _ := json.Marshal(struct {
		Created  time.Time          `json:"created"`
		RootUUID string             `json:"rootUUID"`
		Parent   string             `json:"parent,omitempty"`
		Config   engine.ImageConfig `json:"config"`
	}{i.Created.UTC(), i.RootUUID, i.Parent, i.Config})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type ctrImageRow struct {
	Repository   string
	Tag          string
	ID           string `header:"IMAGE ID"`
	Digest       string
	CreatedSince string `header:"CREATED"`
	CreatedAt    string `header:"CREATED AT"`
	Size         string
	Parent       string
	Containers   string
}

func ctrImages(ctx context.Context, args []string) int {
	fs := ctrFlags("images", "[OPTIONS] [REPOSITORY[:TAG]]")
	quiet := fs.Bool("q", false, "only show image IDs")
	fs.BoolVar(quiet, "quiet", false, "only show image IDs")
	all := fs.Bool("a", false, "accepted, but kido has no intermediate images to show")
	fs.BoolVar(all, "all", false, "show all images")
	noTrunc := fs.Bool("no-trunc", false, "don't truncate output")
	format := fs.String("format", "", "format output with a Go template, 'table TEMPLATE' or 'json'")
	if err := fs.Parse(ctrExpand(ctrInterspersed(fs, args), "qa", "format")); err != nil {
		return 125
	}
	if fs.NArg() > 1 {
		fs.Usage()
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
	if fs.NArg() == 1 {
		repo, tag, hasTag := fs.Arg(0), "", false
		if i := strings.LastIndex(repo, ":"); i >= 0 && !strings.Contains(repo[i:], "/") {
			repo, tag, hasTag = repo[:i], repo[i+1:], true
		}
		var keep []*engine.Image
		for _, i := range imgs {
			if i.Name == repo && (!hasTag || i.Tag == tag) {
				keep = append(keep, i)
			}
		}
		imgs = keep
	}
	if *quiet && *format == "" {
		seen := map[string]bool{}
		for _, i := range imgs {
			id := ctrImageID(i)
			if !*noTrunc {
				id = id[:12]
			}
			if !seen[id] {
				seen[id] = true
				fmt.Println(id)
			}
		}
		return 0
	}
	if *format == "" {
		*format = "table {{.Repository}}\t{{.Tag}}\t{{.ID}}\t{{.CreatedSince}}\t{{.Size}}"
	}
	f, err := ctrNewFormatter(os.Stdout, *format, ctrHeader(ctrImageRow{}))
	if err != nil {
		return ctrFail(err)
	}
	defer f.Flush()
	for _, i := range imgs {
		id := ctrImageID(i)
		if !*noTrunc {
			id = id[:12]
		}
		row := ctrImageRow{
			Repository: i.Name, Tag: i.Tag, ID: id, Digest: "<none>",
			CreatedSince: ctrAgo(i.Created) + " ago", CreatedAt: i.Created.Local().Format("2006-01-02 15:04:05 -0700 MST"),
			Size: ctrSize(i.Size), Parent: i.Parent,
		}
		if strings.Contains(*format, ".Containers") {
			users, _ := rt.ImageUsers(i.Name + ":" + i.Tag)
			row.Containers = fmt.Sprint(len(users))
		}
		if err := f.Row(row); err != nil {
			return ctrFail(err)
		}
	}
	return 0
}

// ctrImageRefs resolves an rmi argument: an image reference, or an image ID prefix (4+ hex digits)
// that matches every tag of one image.
func ctrImageRefs(ctx context.Context, rt *runtime.Runtime, arg string) ([]string, error) {
	if _, err := rt.Image(ctx, arg); err == nil {
		return []string{arg}, nil
	} else if len(arg) < 4 || strings.Trim(arg, "0123456789abcdef") != "" {
		return nil, err
	}
	imgs, err := rt.Images(ctx)
	if err != nil {
		return nil, err
	}
	var refs []string
	ids := map[string]bool{}
	for _, i := range imgs {
		if id := ctrImageID(i); strings.HasPrefix(id, arg) {
			refs = append(refs, i.Name+":"+i.Tag)
			ids[id] = true
		}
	}
	switch {
	case len(refs) == 0:
		return nil, fmt.Errorf("no such image: %s", arg)
	case len(ids) > 1:
		return nil, fmt.Errorf("image ID prefix %s is ambiguous", arg)
	}
	return refs, nil
}

func ctrRmi(ctx context.Context, args []string) int {
	fs := ctrFlags("rmi", "[OPTIONS] IMAGE [IMAGE...]")
	force := fs.Bool("f", false, "also remove the stopped containers made from the image, and every tag of an image ID")
	fs.BoolVar(force, "force", false, "also remove the stopped containers made from the image, and every tag of an image ID")
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
	for _, arg := range fs.Args() {
		refs, err := ctrImageRefs(ctx, rt, arg)
		if err != nil {
			code = ctrFail(err)
			continue
		}
		if len(refs) > 1 && !*force {
			code = ctrFail(fmt.Errorf("image %s is tagged %s: remove the tags one by one or use -f", arg, strings.Join(refs, ", ")))
			continue
		}
		for _, ref := range refs {
			if *force {
				removed, err := rt.RemoveImageForce(ctx, ref)
				for _, c := range removed {
					fmt.Println("Deleted container:", c)
				}
				if err != nil {
					code = ctrFail(err)
					continue
				}
			} else if err := rt.RemoveImage(ctx, ref); err != nil {
				code = ctrFail(err)
				continue
			}
			fmt.Println("Untagged:", ref)
		}
	}
	return code
}

func ctrTag(ctx context.Context, args []string) int {
	fs := ctrFlags("tag", "SOURCE_IMAGE[:TAG] TARGET_IMAGE[:TAG]")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 125
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	refs, err := ctrImageRefs(ctx, rt, fs.Arg(0))
	if err != nil {
		return ctrFail(err)
	}
	if _, err := rt.TagImage(ctx, refs[0], fs.Arg(1)); err != nil {
		return ctrFail(err)
	}
	return 0
}

func ctrImagePrune(ctx context.Context, args []string) int {
	fs := ctrFlags("image prune", "[OPTIONS]")
	all := fs.Bool("a", false, "remove all images no container uses (kido images are always tagged, so nothing is dangling)")
	fs.BoolVar(all, "all", false, "remove all images no container uses")
	force := fs.Bool("f", false, "do not prompt for confirmation")
	fs.BoolVar(force, "force", false, "do not prompt for confirmation")
	if err := fs.Parse(ctrExpand(ctrInterspersed(fs, args), "af")); err != nil {
		return 125
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return 125
	}
	if !*all {
		fmt.Println("Total reclaimed space: 0B")
		return 0
	}
	if !*force && !ctrConfirm("WARNING! This will remove all images without at least one container associated to them.") {
		return 0
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	p, err := rt.PruneImages(ctx)
	if len(p.Names) > 0 {
		fmt.Println("Deleted Images:")
		for _, n := range p.Names {
			fmt.Println("untagged:", n)
		}
		fmt.Println()
	}
	fmt.Println("Total reclaimed space:", ctrSize(p.Bytes))
	if err != nil {
		return ctrFail(err)
	}
	return 0
}

// ctrFind returns DIR/name for the first name that exists, else the only file matching glob.
func ctrFind(dir, glob string, names ...string) (string, error) {
	for _, n := range names {
		p := filepath.Join(dir, n)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, glob))
	if len(matches) == 1 {
		return matches[0], nil
	}
	return "", fmt.Errorf("%s: need one of %s (or exactly one %s)", dir, strings.Join(names, ", "), glob)
}

func ctrImageImport(ctx context.Context, args []string) int {
	fs := ctrFlags("image import", "[--uuid UUID] DIR NAME[:TAG]\n\nDIR holds disk.img (raw GPT disk), kernel, initrd.cpio and the root ext4 UUID\n(--uuid, image.json \"rootUUID\", or a rootUUID/uuid/*.uuid file).")
	uuid := fs.String("uuid", "", "root ext4 UUID (default: read from DIR)")
	if err := fs.Parse(ctrInterspersed(fs, args)); err != nil {
		return 125
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 125
	}
	dir, name := fs.Arg(0), fs.Arg(1)
	disk, err := ctrFind(dir, "*.img", "disk.img")
	if err != nil {
		return ctrFail(err)
	}
	kernel, err := ctrFind(dir, "*.Image", "kernel", "xnu-loader.Image")
	if err != nil {
		return ctrFail(err)
	}
	initrd, err := ctrFind(dir, "*.cpio", "initrd.cpio")
	if err != nil {
		return ctrFail(err)
	}
	var meta struct {
		RootUUID string             `json:"rootUUID"`
		Config   engine.ImageConfig `json:"config"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "image.json")); err == nil {
		if err := json.Unmarshal(b, &meta); err != nil {
			return ctrFail(fmt.Errorf("%s/image.json: %w", dir, err))
		}
	}
	if *uuid == "" {
		*uuid = meta.RootUUID
	}
	if *uuid == "" {
		f, err := ctrFind(dir, "*.uuid", "rootUUID", "root.uuid", "uuid")
		if err != nil {
			return ctrFail(errors.New("no root UUID: pass --uuid or put it in DIR/rootUUID"))
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return ctrFail(err)
		}
		*uuid = strings.TrimSpace(string(b))
	}
	rt, err := ctrOpen()
	if err != nil {
		return ctrFail(err)
	}
	img, err := rt.ImportImage(ctx, name, disk, kernel, initrd, *uuid)
	if err != nil {
		return ctrFail(err)
	}
	ref := img.Name + ":" + img.Tag
	if meta.Config.Entrypoint != nil || meta.Config.Cmd != nil || meta.Config.Env != nil ||
		meta.Config.WorkingDir != "" || meta.Config.User != "" || meta.Config.Labels != nil {
		if err := rt.SetImageConfig(ctx, ref, meta.Config); err != nil {
			return ctrFail(err)
		}
	}
	fmt.Println(ref)
	return 0
}
