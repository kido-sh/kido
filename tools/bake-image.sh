#!/bin/bash
# usage: bake-image.sh SRC_DISK OUT_DIR NAME TAG
#
# Makes a kido image directory from a PureDarwin disk: an APFS clone of SRC_DISK with kido-agent and
# its launchd job written into the ext4 root, the xnu-loader kernel, a cpio holding only the disk's
# kernel collection, and image.json. The disk is edited in a Linux container, so this needs Docker
# and an image with e2fsprogs (debugfs, e2fsck), sfdisk, mtools and cpio, for example Alpine with
# `apk add e2fsprogs e2fsprogs-extra sfdisk mtools`.
#
# Environment: KIDO_LOADER (required: the xnu-loader kernel, an arm64 Linux Image),
# KIDO_BAKE_DOCKER_IMAGE (required: that Docker image), KIDO_AGENT (a prebuilt darwin/arm64
# kido-agent; default: build ./agent), KIDO_BAKE_FORCE=1 (replace an existing image).
set -euo pipefail

die() { echo "bake-image: $*" >&2; exit 1; }

[ $# -eq 4 ] || die "usage: bake-image.sh SRC_DISK OUT_DIR NAME TAG"
src=$1 out=$2 name=$3 tag=$4

kido=$(cd "$(dirname "$0")/.." && pwd)
loader=${KIDO_LOADER:-}
docker_image=${KIDO_BAKE_DOCKER_IMAGE:-}
[ -n "$loader" ] || die "set KIDO_LOADER to the xnu-loader kernel (an arm64 Linux Image)"
[ -n "$docker_image" ] || die "set KIDO_BAKE_DOCKER_IMAGE to a Docker image with e2fsprogs, sfdisk, mtools and cpio"

[[ $name =~ ^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$ ]] || die "invalid name: $name"
[[ $tag =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || die "invalid tag: $tag"
[ -f "$src" ] || die "no such disk: $src"
[ -f "$loader" ] || die "no xnu-loader kernel at $loader"

umask 077
mkdir -p "$out"
chmod 700 "$out"
out=$(cd "$out" && pwd)
src=$(cd "$(dirname "$src")" && pwd)/$(basename "$src")
artifacts=(disk.img kernel initrd.cpio image.json)
for f in "${artifacts[@]}"; do
  if [ -e "$out/$f" ]; then
    [ "${KIDO_BAKE_FORCE:-}" = 1 ] || die "$out/$f exists (KIDO_BAKE_FORCE=1 replaces it)"
  fi
done

work=$(mktemp -d "${TMPDIR:-/tmp}/kido-bake.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir "$work/in" "$work/out"

if [ -n "${KIDO_AGENT:-}" ]; then
  cp "$KIDO_AGENT" "$work/in/kido-agent"
else
  (cd "$kido" && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o "$work/in/kido-agent" ./agent)
fi
file "$work/in/kido-agent" | grep -q 'Mach-O 64-bit executable arm64' || die "kido-agent is not a darwin/arm64 executable"

cat > "$work/in/sh.kido.agent.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>sh.kido.agent</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/libexec/kido-agent</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/var/log/kido-agent.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/kido-agent.log</string>
</dict>
</plist>
PLIST

rm -f "$out/disk.img.partial"
cp -c "$src" "$out/disk.img.partial"
chmod 600 "$out/disk.img.partial"

# Inside the container: write and check the two files, then extract the kernel collection.
docker run --rm -i -v "$out:/out" -v "$work/in:/in:ro" -v "$work/out:/res" "$docker_image" sh -s <<'SH'
set -eu
img=/out/disk.img.partial
part() { sfdisk -d "$img" | awk -F'[=,]' -v p="$img$1 :" 'index($0, p) == 1 {gsub(/ /, "", $2); print $2}'; }
esp=$(part 1); root=$(part 2)
[ -n "$esp" ] && [ -n "$root" ] || { echo "no ESP or root partition in the disk" >&2; exit 1; }
fs="$img?offset=$((root * 512))"

cat > /tmp/cmds <<CMDS
rm /usr/libexec/kido-agent
write /in/kido-agent /usr/libexec/kido-agent
sif /usr/libexec/kido-agent mode 0100755
sif /usr/libexec/kido-agent uid 0
sif /usr/libexec/kido-agent gid 0
rm /System/Library/LaunchDaemons/sh.kido.agent.plist
write /in/sh.kido.agent.plist /System/Library/LaunchDaemons/sh.kido.agent.plist
sif /System/Library/LaunchDaemons/sh.kido.agent.plist mode 0100644
sif /System/Library/LaunchDaemons/sh.kido.agent.plist uid 0
sif /System/Library/LaunchDaemons/sh.kido.agent.plist gid 0
CMDS
debugfs -w -f /tmp/cmds "$fs" >/tmp/debugfs.log 2>&1 || { cat /tmp/debugfs.log >&2; exit 1; }

check() {
  debugfs -R "dump $1 /tmp/check" "$fs" 2>/dev/null
  [ "$(sha256sum < /tmp/check)" = "$(sha256sum < "$2")" ] || { echo "$1: content differs" >&2; exit 1; }
  st=$(debugfs -R "stat $1" "$fs" 2>/dev/null)
  echo "$st" | grep -q "Mode:  0$3 " || { echo "$1: mode is not $3" >&2; echo "$st" | head -3 >&2; exit 1; }
  echo "$st" | grep -qE 'User: +0 +Group: +0 ' || { echo "$1: not root:wheel" >&2; exit 1; }
  echo "$1: 0$3 root:wheel $(wc -c < "$2") bytes"
}
check /usr/libexec/kido-agent /in/kido-agent 755
check /System/Library/LaunchDaemons/sh.kido.agent.plist /in/sh.kido.agent.plist 644
e2fsck -fn "$fs" >/tmp/fsck.log 2>&1 || { cat /tmp/fsck.log >&2; exit 1; }

debugfs -R stats "$fs" 2>/dev/null | awk '/^Filesystem UUID:/ {print $3}' > /res/uuid
mkdir -p /tmp/kc/EFI/BOOT
mcopy -n -i "$img@@$((esp * 512))" ::/EFI/BOOT/kernel /tmp/kc/EFI/BOOT/kernel
(cd /tmp/kc && find EFI | sort | cpio -o -H newc -R 0:0 2>/dev/null) > /res/initrd.cpio
SH

uuid=$(cat "$work/out/uuid")
[[ $uuid =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || die "bad root UUID: $uuid"
[ -s "$work/out/initrd.cpio" ] || die "no kernel collection extracted"

cp "$work/out/initrd.cpio" "$out/initrd.cpio"
cp "$loader" "$out/kernel"
mv -f "$out/disk.img.partial" "$out/disk.img"
size=$(stat -f %z "$out/disk.img")
created=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '{"name":"%s","tag":"%s","created":"%s","rootUUID":"%s","size":%s,"parent":"","config":{}}\n' \
  "$name" "$tag" "$created" "$uuid" "$size" > "$out/image.json"
chmod 600 "$out"/disk.img "$out"/kernel "$out"/initrd.cpio "$out"/image.json
echo "baked $name:$tag in $out (root $uuid)"
