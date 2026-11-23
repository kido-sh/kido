#!/bin/bash
# usage: test/bench.sh [IMAGE]   (default image: darwin; it must already be imported in $KIDO_HOME)
#
# Measures the exec stream throughput of one container: kido cp in and out, kido exec -i stdin,
# kido exec stdout, and with BENCH_RAW=1 raw TCP through a published port (no agent in the path).
# Environment: KIDO (default bin/kido, built from this tree when missing), BENCH_MB (default 64),
# BENCH_RAW=1, BENCH_PORT (host port for the raw test, default 47311).
set -uo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
img=${1:-darwin}
mb=${BENCH_MB:-64}
kido=${KIDO:-$root/bin/kido}
port=${BENCH_PORT:-47311}
name=bench-$$

if [ ! -x "$kido" ]; then
  (cd "$root" && go build -o bin/kido ./cmd/kido) || exit 1
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/kido-bench.XXXXXX")
cleanup() {
  "$kido" rm -f "$name" >/dev/null 2>&1
  rm -rf "$work"
}
trap cleanup EXIT

now() { python3 -c 'import time; print(time.time())'; }
rate() { python3 -c "import sys; s = float(sys.argv[2]) - float(sys.argv[1]); print('%7.2f s %8.1f MB/s' % (s, float(sys.argv[3]) / s))" "$@"; }
row() { printf '| %-40s | %s |\n' "$1" "$2"; }
fail() { row "$1" "FAILED: $2"; }

head -c $((mb << 20)) /dev/urandom >"$work/data"
sum=$(shasum -a 256 "$work/data" | cut -c1-64)

publish=()
[ "${BENCH_RAW:-}" = 1 ] && publish=(-p "127.0.0.1:$port:5001")
"$kido" run -d --name "$name" ${publish[@]+"${publish[@]}"} "$img" >/dev/null || exit 1

echo "| $mb MB, image $img | time, rate |"
echo "|---|---|"

t0=$(now)
if "$kido" cp "$work/data" "$name:/var/root/data" 2>"$work/err"; then
  t1=$(now)
  got=$("$kido" exec "$name" /bin/sha256sum /var/root/data | cut -c1-64)
  if [ "$got" = "$sum" ]; then row "kido cp in" "$(rate "$t0" "$t1" "$mb")"; else fail "kido cp in" "checksum $got"; fi
else
  fail "kido cp in" "$(head -c 200 "$work/err")"
fi

t0=$(now)
if "$kido" cp "$name:/var/root/data" "$work/back" 2>"$work/err"; then
  t1=$(now)
  if cmp -s "$work/data" "$work/back"; then row "kido cp out" "$(rate "$t0" "$t1" "$mb")"; else fail "kido cp out" "content differs"; fi
else
  fail "kido cp out" "$(head -c 200 "$work/err")"
fi

t0=$(now)
got=$("$kido" exec -i "$name" /bin/sh -c 'cat >/dev/null; echo done' <"$work/data" 2>"$work/err")
t1=$(now)
if [ "$got" = done ]; then row "kido exec -i (stdin to cat >/dev/null)" "$(rate "$t0" "$t1" "$mb")"; else fail "kido exec -i" "$(head -c 200 "$work/err")"; fi

t0=$(now)
"$kido" exec "$name" /bin/cat /var/root/data >"$work/out" 2>"$work/err"
t1=$(now)
if cmp -s "$work/data" "$work/out"; then row "kido exec cat (stdout)" "$(rate "$t0" "$t1" "$mb")"; else fail "kido exec cat" "$(head -c 200 "$work/err")"; fi

if [ "${BENCH_RAW:-}" = 1 ]; then
  mkdir "$work/probe"
  cat >"$work/probe/main.go" <<'EOF'
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"
)

func main() {
	switch os.Args[1] {
	case "sink":
		ln, err := net.Listen("tcp", os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		c, err := ln.Accept()
		if err != nil {
			os.Exit(1)
		}
		n, _ := io.Copy(io.Discard, c)
		fmt.Println(n)
	case "src":
		n, _ := strconv.ParseInt(os.Args[3], 10, 64)
		var c net.Conn
		var err error
		for i := 0; i < 50; i++ {
			if c, err = net.Dial("tcp", os.Args[2]); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		start := time.Now()
		buf := make([]byte, 256<<10)
		for n > 0 {
			k := int64(len(buf))
			if k > n {
				k = n
			}
			if _, err := c.Write(buf[:k]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			n -= k
		}
		c.(*net.TCPConn).CloseWrite()
		io.Copy(io.Discard, c)
		fmt.Println(time.Since(start).Seconds())
	}
}
EOF
  (cd "$work/probe" && printf 'module probe\n\ngo 1.21\n' >go.mod &&
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o probe . && go build -o probe-host .) || exit 1
  "$kido" cp "$work/probe/probe" "$name:/var/root/probe" || exit 1
  "$kido" exec "$name" /var/root/probe sink 0.0.0.0:5001 >"$work/raw" 2>&1 &
  sinkpid=$!
  secs=$("$work/probe/probe-host" src "127.0.0.1:$port" $((mb << 20)))
  wait "$sinkpid"
  if [ "$(cat "$work/raw")" = $((mb << 20)) ]; then row "raw TCP host to guest (hostfwd, no agent)" "$(rate 0 "$secs" "$mb")"; else fail "raw TCP" "$(head -c 200 "$work/raw")"; fi
fi
