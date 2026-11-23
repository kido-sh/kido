package mount

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Guest runs argv as root in the container and returns its stdout; a non-zero exit is an error
// carrying the program's stderr. User values are always passed as arguments, never spliced into a
// script.
type Guest func(ctx context.Context, args []string) ([]byte, error)

// prelude starts every script: a fixed PATH (the scripts run as root, and the agent's PATH puts
// /usr/local/bin first), absolute program paths below, and rootchain, which refuses a path whose
// existing components are not all owned by root, so a non-root user cannot redirect root's writes
// through a symlink or a directory of their own.
const prelude = `PATH=/bin:/usr/bin:/sbin:/usr/sbin
export PATH
set -e
set -o pipefail
rootchain() {
  p=
  rest=${1#/}
  while [ -n "$rest" ]; do
    c=${rest%%/*}
    if [ "$c" = "$rest" ]; then rest=; else rest=${rest#*/}; fi
    p=$p/$c
    if [ ! -e "$p" ] && [ ! -L "$p" ]; then return 0; fi
    if [ "$(/bin/stat -c %u "$p")" != 0 ]; then echo "$p is not owned by root in the container" >&2; return 1; fi
  done
}
`

// scanScript prints each whole disk with the start of its first sector (kido's disk header).
const scanScript = prelude + `for d in /dev/disk*; do
  n=${d#/dev/disk}
  case $n in (''|*[!0-9]*) continue;; esac
  h=$(/bin/dd if=/dev/rdisk$n bs=512 count=1 2>/dev/null | /bin/head -c 80 | /bin/tr -cd 'A-Za-z0-9 \n' | /bin/head -n 1) || h=
  printf '%s %s\n' "$d" "$h"
done`

// populateScript: $1 disk, $2 target, $3 dir|file, $4 1 when the target holds kido's previous copy,
// $5 KiB needed. An empty directory or an existing file at the target is replaced by root's own.
const populateScript = prelude + `t=$2
rootchain "$(/bin/dirname "$t")"
if [ -L "$t" ]; then echo "$t is a symbolic link in the container" >&2; exit 1; fi
if [ "$4" = 1 ]; then /bin/rm -rf "$t"; fi
if [ "$3" = dir ]; then
  if [ -e "$t" ] && [ ! -d "$t" ]; then echo "$t exists in the container and is not a directory" >&2; exit 1; fi
  if [ -d "$t" ]; then
    if [ -n "$(/bin/ls -A "$t")" ]; then echo "$t is not empty in the image; a copy mount needs a missing or empty directory" >&2; exit 1; fi
    /bin/rmdir "$t"
  fi
  /bin/mkdir -p "$t"
  dest=$t
else
  if [ -d "$t" ]; then echo "$t is a directory in the container" >&2; exit 1; fi
  dest=$(/bin/dirname "$t")
  /bin/mkdir -p "$dest"
  /bin/rm -f "$t"
fi
avail=$(/bin/df -k "$dest" | /bin/awk 'NR == 2 { print $4 }') || avail=
if [ -n "$avail" ] && [ "$avail" -lt "$5" ]; then
  echo "the copy needs $(( $5 / 1024 + 1 )) MB but the container has $(( avail / 1024 )) MB free" >&2; exit 1
fi
/bin/dd if="$1" bs=65536 skip=1 2>/dev/null | /bin/tar -xpf - -C "$dest"`

// collectScript: $1 disk, $2 target, $3 dir|file. It prints how many files it left out because
// they have more than one link: a hard link can point at a file outside the mount that only root
// may read.
const collectScript = prelude + `rootchain "$(/bin/dirname "$2")"
if [ -L "$2" ]; then echo "$2 is a symbolic link" >&2; exit 1; fi
if [ "$3" = dir ]; then
  cd "$2"
  /bin/find . -type f -links +1 | /bin/wc -l
  /bin/find . ! \( -type f -links +1 \) -print0 | /bin/tar -cf - --null --no-recursion -T - | /bin/dd of="$1" bs=65536 seek=1 2>/dev/null
elif [ "$(/bin/stat -c %h "$2")" -gt 1 ]; then
  echo 1
  /bin/dd if=/dev/zero of="$1" bs=65536 seek=1 count=1 2>/dev/null
else
  echo 0
  cd "$(/bin/dirname "$2")"
  /bin/tar -cf - "./$(/bin/basename "$2")" | /bin/dd of="$1" bs=65536 seek=1 2>/dev/null
fi
/bin/sync`

var diskRE = regexp.MustCompile(`^/dev/disk[0-9]+$`)

// findDisks maps "in N" and "out N" to the guest device that carries this boot's header.
func findDisks(ctx context.Context, g Guest, token string) (map[string]string, error) {
	out, err := g(ctx, []string{"/bin/sh", "-c", scanScript})
	if err != nil {
		return nil, fmt.Errorf("list disks: %w", guestErr(err))
	}
	disks := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 5 || !diskRE.MatchString(f[0]) || f[1] != magic || f[2] != token {
			continue
		}
		disks[f[3]+" "+f[4]] = f[0]
	}
	return disks, nil
}

// Populate copies each source packed by Disks to its target; it runs once the agent answers.
func Populate(ctx context.Context, dir string, g Guest) error {
	st, err := load(dir)
	if st == nil || len(st.Mounts) == 0 {
		return err
	}
	disks, err := findDisks(ctx, g, st.Token)
	if err != nil {
		return err
	}
	for i := range st.Mounts {
		m := &st.Mounts[i]
		dev := disks[fmt.Sprintf("in %d", i)]
		if dev == "" {
			return fmt.Errorf("mount %s: its disk is missing in the container", m.Target)
		}
		kind, replace := "dir", "0"
		if m.File {
			kind = "file"
		}
		if m.Populated {
			replace = "1"
		}
		need := strconv.FormatInt(m.Size/1024+1, 10)
		if _, err := g(ctx, []string{"/bin/sh", "-c", populateScript, "sh", dev, m.Target, kind, replace, need}); err != nil {
			return fmt.Errorf("mount %s: %w", m.Target, guestErr(err))
		}
		m.Populated = true
		if err := save(dir, st); err != nil {
			return err
		}
		os.Remove(filepath.Join(dir, diskDir, fmt.Sprintf("in-%d.img", i)))
	}
	return nil
}

// Collect has the guest pack each read-write target onto its out disk; it runs before the guest
// shuts down. Each mount gets its own deadline, so a wedged guest cannot hold up a stop or rm -f.
// Failures are reported to warn: those changes stay in the container only.
func Collect(ctx context.Context, dir string, g Guest, warn io.Writer) {
	st, err := load(dir)
	if err != nil {
		fmt.Fprintf(warn, "kido: mounts: %v\n", err)
		return
	}
	if st == nil {
		return
	}
	var todo []int
	for i, m := range st.Mounts {
		if !m.ReadOnly && m.Populated {
			todo = append(todo, i)
		}
	}
	if len(todo) == 0 {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	disks, err := findDisks(sctx, g, st.Token)
	cancel()
	for _, i := range todo {
		m := &st.Mounts[i]
		dev := disks[fmt.Sprintf("out %d", i)]
		if err == nil && dev == "" {
			err = fmt.Errorf("its disk is missing in the container")
		}
		if err != nil {
			fmt.Fprintf(warn, "kido: mounts: changes in %s were not copied back: %v\n", m.Target, guestErr(err))
			continue
		}
		kind := "dir"
		if m.File {
			kind = "file"
		}
		cctx, cancel := context.WithTimeout(ctx, collectTimeout(m.Size))
		out, cerr := g(cctx, []string{"/bin/sh", "-c", collectScript, "sh", dev, m.Target, kind})
		cancel()
		if cerr != nil {
			fmt.Fprintf(warn, "kido: mounts: changes in %s were not copied back: %v\n", m.Target, guestErr(cerr))
			continue
		}
		if f := strings.Fields(string(out)); len(f) > 0 {
			if n, err := strconv.Atoi(f[0]); err == nil && n > 0 {
				fmt.Fprintf(warn, "kido: mounts: %d files in %s were not copied back: they have more than one hard link\n", n, m.Target)
			}
		}
		m.Collected = true
	}
	if err := save(dir, st); err != nil {
		fmt.Fprintf(warn, "kido: mounts: %v\n", err)
	}
}

// collectTimeout allows 30 s plus 10 MB/s for what was copied in.
func collectTimeout(size int64) time.Duration {
	return 30*time.Second + time.Duration(size/(10<<20))*time.Second
}

// guestErr drops the "/bin/sh: " the runtime puts in front of the script's own message and makes
// the guest's text safe for a terminal.
func guestErr(err error) error {
	s := err.Error()
	s, _ = strings.CutPrefix(s, "/bin/sh: ")
	return errors.New(sanitize(s))
}

// sanitize replaces control characters (other than newline and tab) and invalid UTF-8.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == utf8.RuneError || r < 0x20 && r != '\n' && r != '\t' || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return '?'
		}
		return r
	}, s)
}
