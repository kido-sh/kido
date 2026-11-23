#!/bin/bash
# usage: test/vz-network.sh IMAGE_DIR   (macOS 26+, bin/kido-vmm built and signed: make sign)
# User networks under the vz backend, end to end: containers on one network talk, other networks
# and --network none are cut off, and a container keeps its network and address across stop/start,
# a SIGKILLed kido-vmm, and a SIGKILL of every kido-vmm (the stand-in for a reboot). At most three VMs
# run at once. The throwaway KIDO_HOME under /tmp is left behind; its path is printed at the end.
set -uo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
img=$1
export KIDO_BACKEND=vz
KIDO_HOME=$(mktemp -d /tmp/kidonet.XXXX) || exit 1
export KIDO_HOME
kido="$root/bin/kido"
probe=$KIDO_HOME/netprobe
P=/var/root/netprobe
pass=0; fail=0
check() {
  local name=$1 want=$2; shift 2
  local out rc
  out=$("$@" 2>&1); rc=$?
  if [[ $rc -eq $want ]]; then pass=$((pass + 1)); printf 'ok   %-34s rc=%d %s\n' "$name" "$rc" "$(head -c 110 <<<"$out" | tr '\n' ' ')"
  else fail=$((fail + 1)); printf 'FAIL %-34s rc=%d want %d: %s\n' "$name" "$rc" "$want" "$(head -c 400 <<<"$out" | tr '\n' ' ')"; fi
}
checko() {
  local name=$1 want=$2 needle=$3; shift 3
  local out rc
  out=$("$@" 2>&1); rc=$?
  if [[ $rc -eq $want && $out == *"$needle"* ]]; then pass=$((pass + 1)); printf 'ok   %-34s rc=%d %s\n' "$name" "$rc" "$(head -c 110 <<<"$out" | tr '\n' ' ')"
  else fail=$((fail + 1)); printf 'FAIL %-34s rc=%d want %d and "%s": %s\n' "$name" "$rc" "$want" "$needle" "$(head -c 400 <<<"$out" | tr '\n' ' ')"; fi
}
ip() { "$kido" inspect -f '{{.IP}}' "$1"; }
# vmms lists the kido-vmm processes (VMs and network holders) of this KIDO_HOME.
vmms() { ps -axo pid=,ucomm=,args= | awk -v h="$KIDO_HOME/" '$2 == "kido-vmm" && index($0, h) { print $1 }'; }
nvmms() { vmms | wc -l | tr -d ' '; }
# killvm C: SIGKILL container C's kido-vmm (never pid 0, which would be this process group).
killvm() { local pid; pid=$("$kido" inspect -f '{{.PID}}' "$1") && [[ $pid -gt 0 ]] && kill -9 "$pid"; }
vmms_gone() { for _ in $(seq 20); do [ -z "$(vmms)" ] && return 0; sleep 0.5; done; return 1; }
# A container restarted after a crash keeps its address and, freshly booted, reuses its first
# ephemeral ports (the guest allocates them in order); a peer may still hold such a connection in
# TIME_WAIT for 30 s and drop the new SYN. So a dial from a restarted container gets three tries.
dial3() { local c=$1; shift; for _ in 1 2 3; do "$kido" exec "$c" $P dial "$@" && return 0; done; return 1; }
# setup C: copy the probe in and serve "C" on 8080 and UDP echo on 8081.
setup() {
  check "probe into $1" 0 "$kido" cp "$probe" "$1:/var/root/"
  check "serve in $1" 0 sh -c '"$1" exec -d "$2" '"$P"' serve 8080 "$2" && "$1" exec -d "$2" '"$P"' udpserve 8081' sh "$kido" "$1"
}
(cd "$root" && go build -o bin/kido ./cmd/kido && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o "$probe" ./test/netprobe) || exit 1

check  "image import"                  0 "$kido" image import "$img" darwin
check  "network create red"            0 "$kido" network create red
check  "network create blue"           0 "$kido" network create blue
check  "network create red again"    125 "$kido" network create red
checko "network ls"                    0 "blue" "$kido" network ls
check  "run a1 on red"                 0 "$kido" run -d --name a1 --network red darwin
check  "run a2 on red"                 0 "$kido" run -d --name a2 --network red darwin
check  "run b1 on blue"                0 "$kido" run -d --name b1 --network blue darwin
for c in a1 a2 b1; do setup "$c"; done
A1=$(ip a1) A2=$(ip a2) B1=$(ip b1)
echo "red: a1 $A1, a2 $A2 ($("$kido" network inspect -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}' red)); blue: b1 $B1"
check  "addresses known"               0 test -n "$A1" -a -n "$A2" -a -n "$B1"
checko "network inspect lists a1"      0 "\"a1\"" sh -c '"$1" network inspect red | grep "\"Name\""' sh "$kido"
checko "ps --filter network=red"       0 "a2" "$kido" ps --filter network=red --format '{{.Names}} {{.Networks}}'
check  "network rm while in use"     125 "$kido" network rm red

# E1: one network talks, two networks do not, each reaches the host's agent and the outside. The
# probe exits 1 only on a timeout, so "blocked" means dropped, not refused.
checko "a1 -> a2 tcp"                  0 "a2 from $A1" "$kido" exec a1 $P dial "$A2:8080"
checko "a2 -> a1 udp"                  0 "echo from $A2" "$kido" exec a2 $P udp "$A1:8081" hi
checko "b1 serves on $B1"              0 "b1 from" "$kido" exec b1 $P dial "$B1:8080"
check  "a1 -> b1 tcp blocked"          1 "$kido" exec a1 $P dial "$B1:8080" 2s
check  "b1 -> a2 tcp blocked"          1 "$kido" exec b1 $P dial "$A2:8080" 2s
check  "b1 -> a1 udp blocked"          1 "$kido" exec b1 $P udp "$A1:8081" hi 2s
for c in a1 b1; do
  checko "$c dns"                      0 "example.com ->" "$kido" exec "$c" $P lookup example.com
  checko "$c internet tcp"             0 "connected" "$kido" exec "$c" $P dial 1.1.1.1:80 3s
done
check  "stop b1"                       0 "$kido" stop b1

# E5: --network none keeps the agent and published ports, nothing else.
check  "run n1 --network none -p"      0 "$kido" run -d --name n1 --network none -p 127.0.0.1::8080 darwin
setup n1
N1=$(ip n1)
check  "n1 address known"              0 test -n "$N1"
checko "host -> n1 published port"     0 "n1 from" bash -c 'port=$("$1" port n1 8080/tcp | sed "s/.*://"); exec 3<>/dev/tcp/127.0.0.1/$port && head -1 <&3' bash "$kido"
check  "n1 dns fails"                  1 "$kido" exec n1 $P lookup example.com 5s
check  "n1 internet tcp fails"         1 "$kido" exec n1 $P dial 1.1.1.1:80 3s
check  "n1 -> a1 blocked"              1 "$kido" exec n1 $P dial "$A1:8080" 2s
check  "a1 -> n1 blocked"              1 "$kido" exec a1 $P dial "$N1:8080" 2s
check  "rm -f n1"                      0 "$kido" rm -f n1

# E6: stop and start; a SIGKILLed kido-vmm; every kido-vmm SIGKILLed, then start from disk.
check  "stop a1"                       0 "$kido" stop a1
checko "red lives on for a2"           0 "example.com ->" "$kido" exec a2 $P lookup example.com
check  "start a1"                      0 "$kido" start a1
setup a1
checko "a1 keeps $A1"                  0 "$A1" ip a1
checko "a2 -> a1 after restart"        0 "a1 from $A2" "$kido" exec a2 $P dial "$A1:8080"
check  "SIGKILL a1's kido-vmm"          0 killvm a1
check  "start a1 at once"              0 "$kido" start a1
setup a1
checko "a1 -> a2 after SIGKILL"        0 "a2 from $A1" dial3 a1 "$A2:8080" 2s
check  "start b1"                      0 "$kido" start b1
check  "3 VMs + 2 holders run"          0 test "$(nvmms)" -eq 5
pids=$(vmms)
check  "SIGKILL every kido-vmm"         0 test -n "$pids"
[[ -n $pids ]] && kill -9 $pids
echo "     (vmnet keeps the subnets of killed holders until InternetSharing exits: the next start waits about a minute)"
check  "start a1 a2 b1 again"          0 "$kido" start a1 a2 b1
for c in a1 a2 b1; do setup "$c"; done
checko "a1 keeps $A1"                  0 "$A1" ip a1
checko "a2 keeps $A2"                  0 "$A2" ip a2
checko "b1 keeps $B1"                  0 "$B1" ip b1
checko "a2 -> a1 after kill-all"       0 "a1 from $A2" "$kido" exec a2 $P dial "$A1:8080"
check  "a1 -> b1 still blocked"        1 "$kido" exec a1 $P dial "$B1:8080" 2s
checko "b1 internet after kill-all"    0 "connected" "$kido" exec b1 $P dial 1.1.1.1:80 3s

check  "rm -f all"                     0 "$kido" rm -f a1 a2 b1
check  "network rm"                    0 "$kido" network rm red blue
check  "no kido-vmm left"               0 vmms_gone
check  "rmi"                           0 "$kido" rmi darwin
echo "passed $pass, failed $fail (KIDO_HOME $KIDO_HOME is left for inspection)"
[[ $fail -eq 0 ]]
