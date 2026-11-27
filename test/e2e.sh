#!/bin/bash
# usage: test/e2e.sh IMAGE_DIR   (a baked image directory; uses a throwaway KIDO_HOME, kept with
# KIDO_E2E_KEEP=1; the QEMU backend needs KIDO_QEMU, since that home has no QEMU from kido setup)
set -uo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
img=$1
if [[ ${KIDO_BACKEND:-vz} == qemu && -z ${KIDO_QEMU:-} ]]; then
  echo "e2e: the QEMU backend needs KIDO_QEMU, a qemu-system-aarch64" >&2
  exit 2
fi
export KIDO_HOME=$(mktemp -d "${TMPDIR:-/tmp}/kido-e2e.XXXX")
[[ -n ${KIDO_E2E_KEEP:-} ]] || trap 'rm -rf "$KIDO_HOME"' EXIT
kido="$root/bin/kido"
pass=0; fail=0
check() {
  local name=$1 want=$2; shift 2
  local out rc
  out=$("$@" 2>&1); rc=$?
  if [[ $rc -eq $want ]]; then pass=$((pass + 1)); printf 'ok   %-28s rc=%d %s\n' "$name" "$rc" "$(head -c 120 <<<"$out" | tr '\n' ' ')"
  else fail=$((fail + 1)); printf 'FAIL %-28s rc=%d want %d: %s\n' "$name" "$rc" "$want" "$(head -c 400 <<<"$out" | tr '\n' ' ')"; fi
}
(cd "$root" && go build -o bin/kido ./cmd/kido) || exit 1
check "image import"            0 "$kido" image import "$img" darwin
check "images"                  0 "$kido" images
check "run uname"               0 "$kido" run --rm darwin /bin/uname -a
check "run exit status"         3 "$kido" run --rm darwin /bin/sh -c 'exit 3'
check "run not found"         127 "$kido" run --rm darwin /nope
check "run env and workdir"     0 "$kido" run --rm -e FOO=bar -w /usr/bin darwin /bin/sh -c 'test "$FOO" = bar && test "$(pwd)" = /usr/bin'
check "run tty"                 0 sh -c 'script -q /dev/null "$1" run -it --rm darwin /bin/sh -c tty </dev/null' sh "$kido"
check "run -d"                  0 "$kido" run -d --name e2e darwin
check "ps"                      0 "$kido" ps
check "exec"                    0 "$kido" exec e2e /bin/uname -m
check "cp in"                   0 "$kido" cp "$root/README.md" e2e:/var/root/
check "exec sees copy"          0 "$kido" exec e2e /bin/sh -c 'test -s /var/root/README.md'
check "cp out"                  0 "$kido" cp e2e:/var/root/README.md "$KIDO_HOME/readme-back.md"
check "cp round trip equal"     0 cmp "$root/README.md" "$KIDO_HOME/readme-back.md"
check "commit"                  0 "$kido" commit e2e darwin-e2e:1
check "run committed image"     0 "$kido" run --rm darwin-e2e:1 /bin/sh -c 'test -s /var/root/README.md'
check "stop"                    0 "$kido" stop e2e
check "rm"                      0 "$kido" rm e2e
check "rmi"                     0 "$kido" rmi darwin-e2e:1

# Help, lifecycle, images, prune.
checko() {
  local name=$1 want=$2 needle=$3; shift 3
  local out rc
  out=$("$@" 2>&1); rc=$?
  if [[ $rc -eq $want && $out == *"$needle"* ]]; then pass=$((pass + 1)); printf 'ok   %-28s rc=%d %s\n' "$name" "$rc" "$(head -c 120 <<<"$out" | tr '\n' ' ')"
  else fail=$((fail + 1)); printf 'FAIL %-28s rc=%d want %d and "%s": %s\n' "$name" "$rc" "$want" "$needle" "$(head -c 400 <<<"$out" | tr '\n' ' ')"; fi
}
checko "help"                   0 "Management Commands" "$kido" --help
checko "run --help"             0 "--entrypoint" "$kido" run --help
checko "help ps"                0 "Usage:  kido ps" "$kido" help ps
checko "did you mean"         125 "start" "$kido" statr
checko "unknown flag"         125 "unknown flag" "$kido" ps --bogus
checko "version"                0 "Version:" "$kido" version
checko "info"                   0 "Images: 1" "$kido" info
check  "tag"                    0 "$kido" tag darwin darwin:e2e
checko "images --format"        0 "darwin:e2e" "$kido" images --format '{{.Repository}}:{{.Tag}}'
checko "run --entrypoint"       0 "via-entrypoint" "$kido" run --rm --entrypoint /bin/echo darwin via-entrypoint
check  "create"                 0 "$kido" create --name e2e-c darwin /bin/sh -c 'echo created; exit 4'
checko "ps -a created"          0 "Created" "$kido" ps -a --filter name=e2e-c
checko "start -a"               4 "created" "$kido" start -a e2e-c
check  "run -d sleeper"         0 "$kido" run -d --name e2e-k -p 127.0.0.1::8080 darwin /bin/sleep 60
checko "port"                   0 "8080/tcp -> 127.0.0.1:" "$kido" port e2e-k
check  "rename"                 0 "$kido" rename e2e-k e2e-k2
check  "exec -d"                0 "$kido" exec -d e2e-k2 /bin/sh -c 'echo bg > /var/root/bg.txt'
checko "exec -d ran"            0 "bg" sh -c 'sleep 1; "$1" exec e2e-k2 /bin/cat /var/root/bg.txt' sh "$kido"
check  "restart"                0 "$kido" restart e2e-k2
checko "running after restart"  0 "e2e-k2" "$kido" ps --format '{{.Names}}'
check  "kill -s TERM"           0 "$kido" kill -s TERM e2e-k2
checko "wait"                   0 "143" "$kido" wait e2e-k2
check  "run -d short"           0 "$kido" run -d --name e2e-l darwin /bin/sh -c 'echo one; sleep 1; echo two'
checko "logs -f"                0 "two" "$kido" logs -f e2e-l
check  "run -d for rm -f"       0 "$kido" run -d --name e2e-f darwin /bin/sleep 60
check  "rm -f running"          0 "$kido" rm -f e2e-f
check  "container prune -f"     0 "$kido" container prune -f
check  "no containers left"     0 sh -c 'test -z "$("$1" ps -aq)"' sh "$kido"
check  "rmi -f with a user"     0 sh -c '"$1" create --name e2e-u darwin:e2e /bin/true >/dev/null && "$1" rmi -f darwin:e2e' sh "$kido"
check  "ctrl-c run --rm"      130 sh -c '"$1" run --rm darwin /bin/sleep 30 & p=$!; sleep 2; kill -INT $p; wait $p' sh "$kido"
check  "nothing left after ^C"  0 sh -c 'test -z "$("$1" ps -aq)"' sh "$kido"
check  "save"                   0 "$kido" save darwin -o "$KIDO_HOME/darwin.tar"
checko "load"                   0 "Loaded image: darwin:latest" "$kido" load -i "$KIDO_HOME/darwin.tar"
checko "run loaded image"       0 "Darwin" "$kido" run --rm darwin /bin/uname -s
check  "run -d for cp"          0 "$kido" run -d --name e2e-cp darwin
checko "cp into missing dir/" 125 "does not exist" "$kido" cp "$root/README.md" e2e-cp:/usr/local/nope/
check  "rm -f cp container"     0 "$kido" rm -f e2e-cp
check  "completion bash"        0 sh -c '"$1" completion bash | bash -n' sh "$kido"
check  "completion zsh"         0 sh -c '"$1" completion zsh > "$2/c.zsh" && zsh -n "$2/c.zsh"' sh "$kido" "$KIDO_HOME"

# --env-file, top, export, history, wait on a --rm container.
printf '# comment\n\nFROM_FILE=file value\nE2E_HOST_VAR\nOVERRIDE=file\n' > "$KIDO_HOME/e2e.env"
checko "run --env-file"         0 "file value|host|cli" env E2E_HOST_VAR=host "$kido" run --rm --env-file "$KIDO_HOME/e2e.env" -e OVERRIDE=cli darwin /bin/sh -c 'echo "$FROM_FILE|$E2E_HOST_VAR|$OVERRIDE"'
checko "--env-file missing"   125 "no such file" "$kido" run --rm --env-file "$KIDO_HOME/nope.env" darwin /bin/true
check  "run -d for top"         0 "$kido" run -d --name e2e-t darwin /bin/sleep 60
checko "exec --env-file"        0 "file value" "$kido" exec --env-file "$KIDO_HOME/e2e.env" e2e-t /bin/sh -c 'echo "$FROM_FILE"'
checko "top"                    0 "/bin/sleep 60" "$kido" top e2e-t
checko "top ps options"         0 "COMM" "$kido" top e2e-t -A -o pid,comm
checko "top bad ps option"    125 "ps failed" "$kido" top e2e-t --bogus
check  "export running"         0 "$kido" export -o "$KIDO_HOME/e2e-t.tar" e2e-t
check  "export has bin/sh"      0 sh -c 'tar -tf "$1" | grep -qx bin/sh' sh "$KIDO_HOME/e2e-t.tar"
check  "export no dev/sockets"  0 sh -c '! tar -tvf "$1" | grep -q "^[cbs]"' sh "$KIDO_HOME/e2e-t.tar"
check  "rm -f top container"    0 "$kido" rm -f e2e-t
check  "create for export"      0 "$kido" create --name e2e-x darwin /bin/true
checko "export created"         0 "etc/" sh -c '"$1" export e2e-x | tar -t | grep -x etc/' sh "$kido"
checko "still Created"          0 "Created" "$kido" ps -a --filter name=e2e-x --format '{{.Status}}'
check  "rm export container"    0 "$kido" rm e2e-x
check  "run -d for history"     0 "$kido" run -d --name e2e-h darwin
check  "commit for history"     0 "$kido" commit e2e-h darwin-h:1
check  "rm -f history source"   0 "$kido" rm -f e2e-h
check  "history chain"          0 sh -c 'test "$("$1" history -q darwin-h:1 | wc -l)" -eq 2' sh "$kido"
checko "history"                0 "CREATED BY" "$kido" history darwin-h:1
checko "history no image"     125 "no such image" "$kido" history nope:1
check  "rmi history image"      0 "$kido" rmi darwin-h:1
check  "run -d --rm for wait"   0 "$kido" run -d --rm --name e2e-w darwin /bin/sh -c 'sleep 1; exit 7'
check  "wait --rm exit status"  0 sh -c 'test "$("$1" wait e2e-w)" = 7' sh "$kido"
check  "nothing left (ux2)"     0 sh -c 'sleep 2; test -z "$("$1" ps -aq)"' sh "$kido"

# stop and restart send TERM to the main process first; argument errors; build -q; help.
check  "run -d TERM trap"       0 "$kido" run -d --name e2e-s darwin /bin/sh -c 'trap "echo got TERM; exit 7" TERM; while :; do sleep 0.1; done'
check  "restart sends TERM"     0 "$kido" restart e2e-s
check  "stop sends TERM"        0 "$kido" stop e2e-s
check  "exit status from trap"  0 sh -c 'test "$("$1" wait e2e-s)" = 7' sh "$kido"
check  "trap ran twice"         0 sh -c 'test "$("$1" logs e2e-s | grep -c "got TERM")" -eq 2' sh "$kido"
check  "run -d ignoring TERM"   0 "$kido" run -d --name e2e-i darwin /bin/sh -c 'trap "" TERM; while :; do sleep 0.1; done'
check  "stop -t 1 then KILL"    0 "$kido" stop -t 1 e2e-i
check  "137 after the timeout"  0 sh -c 'test "$("$1" wait e2e-i)" = 137' sh "$kido"
check  "rm stopped containers"  0 "$kido" rm e2e-s e2e-i
check  "run -d --restart"       0 "$kido" run -d --name e2e-r --restart on-failure:1 darwin /bin/sh -c 'exit 3'
check  "one restart, then stop" 0 sh -c 'for i in $(seq 40); do test "$("$1" inspect -f "{{.Restarts}} {{.State}}" e2e-r)" = "1 stopped" && exit 0; sleep 1; done; exit 1' sh "$kido"
check  "rm restarted container" 0 "$kido" rm e2e-r
checko "--restart needs -d"   125 "--restart needs -d" "$kido" run --restart always darwin true
mkdir -p "$KIDO_HOME/vol"
check  "run -d -v TERM writes"  0 "$kido" run -d --name e2e-v -v "$KIDO_HOME/vol:/data" darwin /bin/sh -c 'trap "echo bye > /data/bye; exit 0" TERM; while :; do sleep 0.1; done'
check  "stop collects after TERM" 0 "$kido" stop e2e-v
checko "-v has the TERM write"  0 "bye" cat "$KIDO_HOME/vol/bye"
check  "rm -v container"        0 "$kido" rm e2e-v
echo hi > "$KIDO_HOME/vol/in"
checko "run --rm -v :ro reads"  0 "hi" "$kido" run --rm -v "$KIDO_HOME/vol:/data:ro" darwin cat /data/in
check  "run --rm -v writes"     0 "$kido" run --rm -v "$KIDO_HOME/vol:/data" darwin /bin/sh -c 'echo new > /data/new'
checko "-v copied the write back" 0 "new" cat "$KIDO_HOME/vol/new"
live_image=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1])).get("config", {}).get("labels", {}).get("sh.kido.virtiofs", ""))' "$img/image.json" 2>/dev/null)
if [[ ${KIDO_BACKEND:-vz} != vz || $live_image != 1 ]]; then
  echo "skip live mount checks: they need the vz backend and an image labelled sh.kido.virtiofs=1"
else
  live=$KIDO_HOME/live
  mkdir -p "$live"
  check  "run -d live -v"         0 "$kido" run -d --name e2e-l -v "$live:/data" darwin sleep 600
  echo host > "$live/host"
  checko "live: sees a Mac write" 0 "host" "$kido" exec e2e-l cat /data/host
  check  "live: writes"           0 "$kido" exec e2e-l /bin/sh -c 'echo guest > /data/guest'
  checko "live: on the Mac at once" 0 "guest" cat "$live/guest"
  check  "live: removes"          0 "$kido" exec e2e-l rm /data/host
  check  "live: gone on the Mac"  1 test -e "$live/host"
  check  "live: restart"          0 "$kido" restart e2e-l
  checko "live: mounted again"    0 "guest" "$kido" exec e2e-l cat /data/guest
  check  "rm -f live container"   0 "$kido" rm -f e2e-l
  checko "live :ro reads"         0 "guest" "$kido" run --rm -v "$live:/data:ro" darwin cat /data/guest
  check  "live :ro refuses writes" 0 sh -c 'out=$("$1" run --rm -v "$2:/data:ro" darwin /bin/sh -c "echo x > /data/x" 2>&1) && exit 1; printf "%s" "$out" | grep -qi "read-only file system"' sh "$kido" "$live"
  check  "live :ro wrote nothing" 1 test -e "$live/x"
  check  "live --rm writes"       0 "$kido" run --rm -v "$live:/data" darwin /bin/sh -c 'echo last > /data/last'
  checko "live --rm write landed" 0 "last" cat "$live/last"
  check  "run -d file -v"         0 "$kido" run -d --name e2e-f -v "$live/guest:/etc/guest:ro" darwin sleep 600
  echo changed > "$live/guest"
  checko "file -v is a copy"      0 "guest" "$kido" exec e2e-f cat /etc/guest
  check  "rm -f file container"   0 "$kido" rm -f e2e-f
fi
probe=$KIDO_HOME/netprobe
(cd "$root" && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o "$probe" ./test/netprobe) || exit 1
udp_port=$(python3 -c 'import socket; s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
udp_echo() { for _ in 1 2 3 4 5; do "$probe" udp "127.0.0.1:$udp_port" ping 2s && return 0; done; return 1; }
check  "run -d -p udp"          0 "$kido" run -d --name e2e-u -p "127.0.0.1:$udp_port:9999/udp" darwin sleep 600
check  "cp netprobe in"         0 "$kido" cp "$probe" e2e-u:/var/root/netprobe
check  "exec -d udp echo"       0 "$kido" exec -d e2e-u /var/root/netprobe udpserve 9999
checko "-p udp answers"         0 ": ping" udp_echo
check  "rm -f udp container"    0 "$kido" rm -f e2e-u
checko "run without IMAGE"    125 "\"kido run\" requires at least 1 argument." "$kido" run
check  "no full help for it"    0 sh -c '! "$1" run 2>&1 | grep -q Options:' sh "$kido"
checko "build without PATH"   125 "requires exactly 1 argument" "$kido" build -t x
checko "build --help"           0 "--no-cache" "$kido" build --help
checko "pull unknown flag"    125 "unknown flag: --bogus" "$kido" pull --bogus x
mkdir -p "$KIDO_HOME/ctx" && printf 'FROM darwin\nRUN echo hi > /hi\n' > "$KIDO_HOME/ctx/Dockerfile"
checko "build prints the tag"   0 "Successfully tagged e2e-b:1" "$kido" build -t e2e-b:1 "$KIDO_HOME/ctx"
check  "build -q prints the ID" 0 sh -c 'id=$("$1" build -q -t e2e-b:2 "$2") && test "$id" = "sha256:$("$1" images -q --no-trunc e2e-b:2)"' sh "$kido" "$KIDO_HOME/ctx"
check  "rmi built images"       0 "$kido" rmi e2e-b:1 e2e-b:2
checko "completion help eval"   0 'eval "$(kido completion bash)"' "$kido" completion --help
start=$(python3 -c 'import time; print(time.time())')
"$kido" run --rm darwin /bin/true >/dev/null 2>&1
end=$(python3 -c 'import time; print(time.time())')
printf 'time run --rm true: %.2f s\n' "$(python3 -c "print($end - $start)")"
echo "passed $pass, failed $fail"
[[ $fail -eq 0 ]]
