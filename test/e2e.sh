#!/bin/bash
# usage: test/e2e.sh IMAGE_DIR   (a baked image directory; uses a throwaway KIDO_HOME, kept with
# KIDO_E2E_KEEP=1; the QEMU backend needs KIDO_QEMU, since that home has no QEMU from kido setup)
set -uo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
img=$1
if [[ ${KIDO_BACKEND:-qemu} == qemu && -z ${KIDO_QEMU:-} ]]; then
  echo "e2e: set KIDO_QEMU to a qemu-system-aarch64 for the QEMU backend, or KIDO_BACKEND=vz" >&2
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

# Docker parity: help, lifecycle, images, prune.
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
mkdir -p "$KIDO_HOME/vol"
check  "run -d -v TERM writes"  0 "$kido" run -d --name e2e-v -v "$KIDO_HOME/vol:/data" darwin /bin/sh -c 'trap "echo bye > /data/bye; exit 0" TERM; while :; do sleep 0.1; done'
check  "stop collects after TERM" 0 "$kido" stop e2e-v
checko "-v has the TERM write"  0 "bye" cat "$KIDO_HOME/vol/bye"
check  "rm -v container"        0 "$kido" rm e2e-v
echo hi > "$KIDO_HOME/vol/in"
checko "run --rm -v :ro reads"  0 "hi" "$kido" run --rm -v "$KIDO_HOME/vol:/data:ro" darwin cat /data/in
check  "run --rm -v writes"     0 "$kido" run --rm -v "$KIDO_HOME/vol:/data" darwin /bin/sh -c 'echo new > /data/new'
checko "-v copied the write back" 0 "new" cat "$KIDO_HOME/vol/new"
checko "run without IMAGE"    125 "\"kido run\" requires at least 1 argument." "$kido" run
check  "no full help for it"    0 sh -c '! "$1" run 2>&1 | grep -q Options:' sh "$kido"
checko "build without PATH"   125 "requires exactly 1 argument" "$kido" build -t x
checko "build --help"           0 "--no-cache" "$kido" build --help
checko "pull unknown flag"    125 "unknown flag: --bogus" "$kido" pull --bogus x
mkdir -p "$KIDO_HOME/ctx" && printf 'FROM darwin\nRUN echo hi > /hi\n' > "$KIDO_HOME/ctx/Dockerfile"
checko "build tags like Docker" 0 "Successfully tagged e2e-b:1" "$kido" build -t e2e-b:1 "$KIDO_HOME/ctx"
check  "build -q prints the ID" 0 sh -c 'id=$("$1" build -q -t e2e-b:2 "$2") && test "$id" = "sha256:$("$1" images -q --no-trunc e2e-b:2)"' sh "$kido" "$KIDO_HOME/ctx"
check  "rmi built images"       0 "$kido" rmi e2e-b:1 e2e-b:2
checko "completion help eval"   0 'eval "$(kido completion bash)"' "$kido" completion --help
start=$(python3 -c 'import time; print(time.time())')
"$kido" run --rm darwin /bin/true >/dev/null 2>&1
end=$(python3 -c 'import time; print(time.time())')
printf 'time run --rm true: %.2f s\n' "$(python3 -c "print($end - $start)")"
echo "passed $pass, failed $fail"
[[ $fail -eq 0 ]]
