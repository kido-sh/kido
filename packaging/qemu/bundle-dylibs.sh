#!/bin/bash
# Make an installed macOS QEMU tree self-contained, so it runs on a Mac without Homebrew: copy every
# library outside /usr/lib and /System that its programs load (directly or through other libraries)
# into PREFIX/lib/dylibs, point the load commands there with @loader_path, and re-sign.
#
#   bundle-dylibs.sh PREFIX ENTITLEMENTS [PROGRAM...]
#
# PREFIX is the install tree (PREFIX/bin/qemu-system-aarch64, PREFIX/share/qemu, ...), e.g. the
# usr/local of `make install DESTDIR=...` on the CI runner that built QEMU against Homebrew.
# ENTITLEMENTS is qemu-hvf.entitlements; qemu-system-* programs are signed with it. PROGRAMs are
# relative to PREFIX (default: bin/qemu-system-aarch64 bin/qemu-img); pass lib/qemu/*.dylib modules
# too to bundle what they load. Needs otool, install_name_tool and codesign (Xcode command line
# tools). Written for the bash 3.2 that ships with macOS.

set -euo pipefail

die() { echo "bundle-dylibs: $*" >&2; exit 1; }

[ $# -ge 2 ] || die "usage: bundle-dylibs.sh PREFIX ENTITLEMENTS [PROGRAM...]"
prefix=$(cd "$1" && pwd -P)
entitlements=$(cd "$(dirname "$2")" && pwd -P)/$(basename "$2")
[ -f "$entitlements" ] || die "no entitlements file: $2"
shift 2
[ $# -gt 0 ] || set -- bin/qemu-system-aarch64 bin/qemu-img
dest=$prefix/lib/dylibs
mkdir -p "$dest"

own_id() { otool -D "$1" | awk 'NR == 2 { print $1 }'; }

load_commands() {
	local id
	id=$(own_id "$1")
	otool -L "$1" | awk 'NR > 1 { print $1 }' | while read -r dep; do
		[ "$dep" = "$id" ] || echo "$dep"
	done
}

rpaths() {
	otool -l "$1" | awk '$1 == "cmd" && $2 == "LC_RPATH" { want = 1 } want && $1 == "path" { print $2; want = 0 }'
}

# relpath FROM TO: the relative path from directory FROM to directory TO (both absolute).
relpath() {
	local from=$1 to=$2 up=""
	while [ "$to" != "$from" ] && [ "${to#"$from"/}" = "$to" ]; do
		from=$(dirname "$from")
		up="../$up"
	done
	local rest=${to#"$from"}
	rest=${rest#/}
	local out=$up$rest
	echo "${out%/}"
}

# resolve DEP ORIGIN FILE: the file that DEP names when loaded by FILE, whose original copy lived in
# directory ORIGIN (so @loader_path and @rpath of a library copied out of Homebrew still resolve).
resolve() {
	local dep=$1 origin=$2 file=$3 rp
	case $dep in
	@loader_path/*) echo "$origin/${dep#@loader_path/}" ;;
	@executable_path/*) echo "$prefix/bin/${dep#@executable_path/}" ;;
	@rpath/*)
		for rp in $(rpaths "$file"); do
			rp=${rp/@loader_path/$origin}
			rp=${rp/@executable_path/$prefix/bin}
			if [ -e "$rp/${dep#@rpath/}" ]; then
				echo "$rp/${dep#@rpath/}"
				return
			fi
		done
		echo "$origin/${dep#@rpath/}"
		;;
	*) echo "$dep" ;;
	esac
}

queue=()
origins=()
modified=()
bnames=()
bsrcs=()
for p in "$@"; do
	[ -f "$prefix/$p" ] || die "no such program: $prefix/$p"
	queue+=("$prefix/$p")
	origins+=("$(dirname "$prefix/$p")")
done

i=0
while [ $i -lt ${#queue[@]} ]; do
	file=${queue[$i]}
	origin=${origins[$i]}
	i=$((i + 1))
	changes=()
	for dep in $(load_commands "$file"); do
		case $dep in /usr/lib/* | /System/*) continue ;; esac
		src=$(resolve "$dep" "$origin" "$file")
		[ -e "$src" ] || die "$file loads $dep, which is not on this machine ($src)"
		real=$(cd "$(dirname "$src")" && pwd -P)/$(basename "$src")
		while [ -L "$real" ]; do
			link=$(readlink "$real")
			case $link in /*) real=$link ;; *) real=$(cd "$(dirname "$real")" && cd "$(dirname "$link")" && pwd -P)/$(basename "$link") ;; esac
		done
		case $real in
		"$prefix"/*)
			# Already inside the tree: scan it once, keep its load command.
			seen=0
			for q in "${queue[@]}"; do [ "$q" != "$real" ] || seen=1; done
			if [ $seen = 0 ]; then
				queue+=("$real")
				origins+=("$(dirname "$real")")
			fi
			continue
			;;
		esac
		base=$(basename "$dep")
		known=""
		for ((j = 0; j < ${#bnames[@]}; j++)); do
			[ "${bnames[$j]}" != "$base" ] || known=${bsrcs[$j]}
		done
		if [ -n "$known" ]; then
			[ "$known" = "$real" ] || cmp -s "$known" "$real" || die "two different libraries are both named $base: $known, $real"
		else
			[ ! -e "$dest/$base" ] || die "$dest/$base already exists"
			bnames+=("$base")
			bsrcs+=("$real")
			cp "$real" "$dest/$base"
			chmod 0644 "$dest/$base"
			install_name_tool -id "@rpath/$base" "$dest/$base" 2>/dev/null
			modified+=("$dest/$base")
			queue+=("$dest/$base")
			origins+=("$(dirname "$real")")
		fi
		rel=$(relpath "$(dirname "$file")" "$dest")
		changes+=(-change "$dep" "@loader_path/${rel:+$rel/}$base")
	done
	if [ ${#changes[@]} -gt 0 ]; then
		chmod u+w "$file"
		install_name_tool "${changes[@]}" "$file" 2>/dev/null
		modified+=("$file")
	fi
done

for f in $(printf '%s\n' "${modified[@]+"${modified[@]}"}" | sort -u); do
	case $(basename "$f") in
	qemu-system-*) codesign --force --sign - --entitlements "$entitlements" "$f" 2>/dev/null ;;
	*) codesign --force --sign - "$f" 2>/dev/null ;;
	esac
done

leftover=0
for f in "${queue[@]}"; do
	if load_commands "$f" | grep -Ev '^(/usr/lib/|/System/|@loader_path/|@executable_path/|@rpath/)'; then
		echo "bundle-dylibs: $f still loads the libraries above" >&2
		leftover=1
	fi
	codesign --verify --strict "$f" || leftover=1
done
[ $leftover = 0 ] || exit 1
echo "bundle-dylibs: $(ls "$dest" | wc -l | tr -d ' ') libraries in $dest; $(printf '%s\n' "${modified[@]+"${modified[@]}"}" | sort -u | wc -l | tr -d ' ') files rewritten and re-signed"
