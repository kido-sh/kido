# kido

kido runs macOS command-line tools in micro-VMs on Apple Silicon Macs, with the commands you know
from Docker. Each container is its own small virtual machine that boots an open-source Darwin guest
(XNU and a small userland, built from PureDarwin) in about half a second, under QEMU with
Hypervisor.framework or under Virtualization.framework. A tool built for macOS arm64 runs
unmodified and isolated from your Mac: `kido run`, `exec`, `cp`, `build` from a Dockerfile, `push`
and `pull` through any OCI registry. No Docker, no root, no Linux VM in between.

    $ kido run --rm darwin uname -sm
    Darwin arm64

Website: <https://kido.sh>. Source and releases: <https://github.com/kido-sh/kido>.

Status: experimental. Commands, the image format and the protocol between kido and its guest agent
may still change.

## Requirements

- An Apple Silicon Mac with macOS 15 or later; `kido doctor` checks this. User networks on the
  Virtualization.framework backend need macOS 26.
- Disk for the images (the base image's root file system is 8 GiB, stored sparse) and for what each
  container writes.
- QEMU 11.1 built with Hypervisor.framework support: `kido setup` installs it (see below).

## Install

With the install script, which puts kido into `~/.local` (no sudo), checks the release's SHA-256 and
then runs `kido setup`:

    curl -fsSL https://kido.sh/install.sh | sh
    curl -fsSL https://kido.sh/install.sh | sh -s -- --prefix ~/opt/kido --version 0.1.0 --no-setup

It downloads the release archive from <https://github.com/kido-sh/kido/releases>; `--image REF`
also pulls a base image during setup. With Homebrew:

    brew install kido-sh/tap/kido

From source, with Go 1.26 and the Xcode command line tools:

    git clone https://github.com/kido-sh/kido.git && cd kido
    make                          # bin/kido, bin/kido-agent and bin/kido-vmm
    make install PREFIX=~/.local  # signs them ad hoc, installs bin/kido and libexec/kido/
    make dist                     # dist/kido-VERSION-darwin-arm64.tar.gz, .sha256 and install.sh

Shell completion: `eval "$(kido completion bash)"` (macOS's bash 3.2 cannot `source <(...)`),
`source <(kido completion zsh)`, or `kido completion fish > ~/.config/fish/completions/kido.fish`.

## Setup: QEMU and a guest image

    kido setup

downloads QEMU 11.1.3 for darwin arm64 (85.5 MB, from github.com/cocoa-xu/qemu-build) over HTTPS,
checks a SHA-256 pinned in kido, unpacks only what kido uses (about 50 MB) into
`~/.kido/qemu/11.1.3`, signs `qemu-system-aarch64` with the `com.apple.security.hypervisor`
entitlement if its signature lacks it, checks that QEMU and qemu-img start, and points
`~/.kido/qemu/current` at that version. Running it again downloads nothing. `$KIDO_QEMU`, an
absolute path to `qemu-system-aarch64` with qemu-img next to it, takes precedence.

The pinned QEMU build loads some libraries from Homebrew (`/opt/homebrew/opt/...`). On a Mac without
them `kido setup` stops and names what is missing, for example
`brew install capstone dtc gnutls snappy vde`; run it again afterwards (nothing is downloaded
twice). `packaging/qemu/bundle-dylibs.sh` makes a QEMU tree self-contained, and
`kido setup --qemu-url URL-or-FILE --qemu-sha256 HEX` installs such an archive (`.tar.xz` with
`bin/`, `lib/`, `share/qemu/`, optionally under `usr/local/`).

kido does not ship a guest image. A base image is a directory with `disk.img` (a raw GPT disk: EFI
partition and ext4 root), `kernel` (xnu-loader built as an arm64 Linux kernel image),
`initrd.cpio` (the kernel collection) and `image.json` (with the root file system's `"rootUUID"`).
Import one, or pull one from an OCI registry:

    kido image import DIR darwin                # becomes darwin:latest; an APFS clone, cheap
    kido setup --image REGISTRY/darwin:latest   # stored as darwin:latest (--image-name to change)
    kido pull REGISTRY/darwin:latest darwin:latest

`tools/bake-image.sh` makes such a directory from a PureDarwin container disk and an xnu-loader
kernel: it writes `kido-agent` and its launchd job into the disk. It edits the disk in a Linux
container, so it needs Docker (see the script for the rest). `kido doctor` then checks the whole
setup.

## Quick start

    $ kido run --rm darwin uname -sm
    Darwin arm64

    $ kido run -d --name box darwin sleep 600
    $ kido exec box id -un
    root
    $ echo hello > hello.txt
    $ kido cp hello.txt box:/var/root/
    $ kido exec box cat /var/root/hello.txt
    hello
    $ kido cp box:/etc/hosts ./hosts.txt
    $ kido exec -it box /bin/sh           # zsh as /bin/sh; Ctrl-D leaves
    $ kido rm -f box

Build an image from a Dockerfile. This one packages Android's `adb`, an unmodified macOS binary;
`adb-image/` holds the Dockerfile and the `adb` binary:

    FROM darwin
    COPY adb /usr/local/bin/adb
    ENV ADB_USB=0
    ENTRYPOINT ["/usr/local/bin/adb"]
    CMD ["version"]

    $ kido build -t adb:37 adb-image/
    ...
    Successfully tagged adb:37
    $ kido run --rm adb:37
    Android Debug Bridge version 1.0.41
    Version 37.0.1-15733141
    Installed as /usr/local/bin/adb
    Running on Darwin 25.5.0 (arm64)

Under QEMU a one-shot `kido run --rm` takes about half a second, and `kido cp` of 64 MB about a
second. Containers keep `/tmp` across stop, start and commit, and `-u` or a Dockerfile `USER` may
name any uid, with or without a passwd entry. `kido push` and `kido pull` share credentials with
Docker (`~/.docker/config.json`).

## Backends

- **QEMU** (the default): `qemu-system-aarch64 -machine virt -accel hvf` boots the guest's kernel
  directly, on a qcow2 overlay of the image's disk, behind QEMU's user-mode NAT.
- **Virtualization.framework** (`KIDO_BACKEND=vz`, experimental): `kido-vmm`, a small helper signed
  with `com.apple.security.virtualization`, runs each VM on an APFS clone of the image's disk, on
  vmnet. `make sign` builds and signs it as `bin/kido-vmm`; `make install`, Homebrew and
  `install.sh` put it in `libexec/kido/`. kido finds it through `$KIDO_VMM`, next to the `kido`
  binary, or in `../libexec/kido`. Streams are much faster than under QEMU (exec around
  500-800 MB/s). It has no copy mounts and no UDP port publishing, and `logs --console` stays
  empty: the guest has no virtio-console driver yet.

## Networks

- **QEMU**: the guest sits behind QEMU's user-mode NAT: outbound TCP, UDP and DNS work, and, like
  Docker Desktop's `host.docker.internal`, the guest reaches every service on the host's loopback
  (127.0.0.1) through 10.0.2.2, including other containers' published ports and local dev servers.
  `--network none` cuts the guest off from both the host and the outside.
- **Virtualization.framework, default network**: every container sits on vmnet's shared NAT
  network (192.168.64.0/24; set `KIDO_VZ_SUBNET` if yours differs). These containers share one
  layer-2 network, so a root process in one of them can intercept another's traffic (not the
  agent channel, which is encrypted and authenticated). Any local process can reach a container's
  ports at its address.
- **Virtualization.framework, user networks** (macOS 26+): each group of containers gets its own
  vmnet network, subnet and bridge:

      kido network create [--subnet CIDR] [--internal] NAME     # also: ls, inspect, rm, prune
      KIDO_BACKEND=vz kido run -d --network NAME darwin
      KIDO_BACKEND=vz kido run -d --network none darwin

  - Containers on a network reach each other, the host (at .1, which also answers DNS) and the
    outside through NAT; `--internal` drops NAT and DNS. Containers on other networks cannot reach
    them. Subnets are picked from 192.168.128.0/24 upward, away from vmnet's default, other
    networks and the host's interfaces.
  - `--network none` gives the container a private host-only network without DNS: only the host
    (kido's agent, published ports) reaches it.
  - A container keeps its network and address for its whole life: across stop and start, and
    after every kido process is gone. Definitions live in `$KIDO_HOME/networks/NAME.json`.
  - A vmnet network ends with the process that created it, so while containers use a network, a
    `kido-vmm hold-network` process owns it and exits about two seconds after the last of them
    stops. If it is killed while containers run, they lose the network until they restart, and
    macOS keeps the subnet reserved until about a minute after the last VM on vmnet (of any tool)
    stops: the next start waits up to two minutes for that.
  - The host reaches every container's address, as Docker does on Linux.
- **Published ports** (`-p`) bind to 127.0.0.1 unless you name another address, on both backends
  and with `--network none`. vmnet's own port forwarding is not used: it would also listen on the
  Mac's LAN addresses.

## Copy mounts: `-v` and `--mount type=bind`

`-v HOST:CONTAINER[:ro]` and `--mount type=bind,source=HOST,target=CONTAINER[,readonly]` copy; they
are not live bind mounts, and they need the QEMU backend:

- The host directory or file is copied in as a snapshot at every boot (create, start).
- Read-write: at `kido stop`, at the end of `kido run` (also with `--rm`) and at `kido rm -f`, new
  and changed files are copied back. Deletions in the container are not propagated (kido reports
  their count). A file that also changed on the host is not overwritten: the container's version is
  kept next to it as `NAME.kido-<id>`. Hard-linked files are not copied back.
- `:ro` (`readonly`): the container may write its copy, and nothing comes back.
- Changes are lost if the VM dies without a stop. `kido commit` includes the copy.
- The target must be absolute and missing or empty in the image; system paths are refused.

## Commands

`kido COMMAND --help` lists every flag; flags follow Docker's where they mean the same thing.

| Command | What it does |
|---|---|
| `run` | Create a container from an image and run a command in it (`-it --rm -d --name -e --env-file -w -u -p -v --mount --network --cpus -m --entrypoint`) |
| `create`, `start`, `stop`, `restart`, `kill`, `wait` | Container lifecycle, as in Docker (`start -a -i`, `kill -s`); `stop` and `restart` send SIGTERM to the main process, wait `-t` seconds (default 10), then SIGKILL, and only then copy `-v` mounts back and power off |
| `exec` | Run a command in a running container (`-it -d -e --env-file -w -u`) |
| `top` | Run `ps` in the container's VM (`ps -ef` by default, or the `ps` options given after the container) |
| `export` | Write a container's file system as a tar archive (`-o FILE`, or stdout); boots a stopped container without its command for it; sockets and device nodes are skipped with a warning |
| `cp` | Copy files between a container and the host (`SRC CONTAINER:DST`, `CONTAINER:SRC DST`) |
| `ps`, `logs`, `inspect`, `port`, `rename`, `rm` | List, read the output (`logs --console`: the serial console; `--tail`), inspect (JSON), ports, rename, remove |
| `build` | Build an image from a Dockerfile: FROM, RUN, COPY, ADD (local files only, archives not unpacked), ENV, ARG, WORKDIR, USER, ENTRYPOINT, CMD, LABEL, EXPOSE (no SHELL, no multi-stage); `-q` prints only the image ID; `--no-cache` and `--pull` are accepted (kido has no build cache; FROM uses the local image) |
| `commit` | Create an image from a container's disk |
| `images`, `rmi`, `tag`, `save`, `load` | List, remove, tag images; save to and load from an OCI-layout tar archive |
| `history` | An image's parent chain: ID, created, created-by and comment when recorded, size (`-H --no-trunc -q --format`) |
| `image import` | Import a base image directory (`disk.img`, `kernel`, `initrd.cpio`, `image.json`) |
| `pull`, `push`, `login` | OCI registries; credentials are shared with Docker's `~/.docker/config.json` |
| `container`, `image`, `system` | Docker-style groups, with `ls`, `prune`, `system df`, `system info` |
| `network` | User networks for the Virtualization.framework backend, macOS 26+ (`create --subnet --internal`, `ls`, `inspect`, `rm`, `prune`) |
| `setup` | Install QEMU into KIDO_HOME, optionally pull a base image (`--image`) |
| `doctor` | Check the Mac, QEMU, KIDO_HOME, images, disk space and that no Docker is needed |
| `info`, `version`, `completion` | System information, version, shell completion (bash, zsh, fish) |

`ps`, `images`, `history`, `inspect`, `version` and `info` take `--format` (a Go template, `table`
or `json`); `inspect` templates use kido's own field names (a container's `{{.ID}}`, `{{.State}}`),
not Docker's (`{{.Id}}`, `{{.State.Running}}`). Not there yet: `attach`, `stats`, `pause`, `diff`,
`logout`, restart policies. Exit status follows Docker: the command's own status, 125 when kido
itself fails, 126/127 when the guest cannot run or find the program; a failed `kido build` exits
with the status of its RUN step.

## Security model

- **One container is one VM.** XNU has no namespaces or cgroups, so the hypervisor is the isolation
  boundary: every container gets its own kernel, memory and disk (a copy-on-write copy of the
  image's). QEMU and `kido-vmm` run as your user; kido needs no sudo.
- **The agent channel.** A small agent in the guest (`kido-agent`, port 1023 inside the VM) runs
  commands for the CLI. For every boot kido makes a new ed25519 key pair and a boot secret, and
  passes only the public key and the secret to the guest in its boot arguments, through a file
  rather than a command line. At the first contact after boot the agent must prove that it knows
  the boot secret, and its identity key is then pinned. Every connection agrees on fresh X25519
  keys and seals the request and every frame with AES-256-GCM, and every request is signed by the
  host key: a process between kido and the agent can break a connection, but cannot read or change
  the commands, environment, files and output that pass through it. At boot, before the first
  contact, kido checks (with `lsof`) that its own VM process is the one listening on the agent
  port, and connections that do not authenticate are evicted, oldest first, so they cannot lock
  kido out.
- **Network.** See [Networks](#networks): on QEMU the guest can reach the host's loopback; on the
  Virtualization.framework default network containers share one layer-2 network, so put containers
  that must not see each other on separate user networks.
- **Host state.** Everything lives in `$KIDO_HOME` (default `~/.kido`) with directories 0700 and
  files 0600; kido refuses a KIDO_HOME that another user owns or that group or others can write. A
  container's console log is capped in size.
- **Files in and out.** `kido cp`, build contexts and copy mounts refuse paths that escape their
  directory (`..`, absolute paths, symlinks pointing out).
- **Downloads.** `kido setup` fetches QEMU over HTTPS only, checks a SHA-256 pinned in the binary
  and unpacks without following links out of its directory, with size limits.
- **The guest's own crypto.** The guest's corecrypto is an interim fork that has not been audited
  (see [Acknowledgements](#acknowledgements)); do not rely on the guest's system crypto library to
  protect anything. The agent uses Go's own cryptographic code, but the random numbers for its keys
  and nonces come from the guest's system random number generator (`arc4random_buf`), which that
  corecrypto provides.

## Limits

- arm64 Mach-O only: the guest has no Rosetta, so x86_64-only tools do not run; universal binaries
  run their arm64 slice. Apple's own binaries (arm64e, not redistributable) are out of scope, as are
  GUI apps and Xcode builds.
- The guest is Darwin, not macOS: a small userland (toybox applets, zsh as `/bin/sh`), no `stty`,
  and `/tmp` and `/var` are symlinks into `/private` (so `-w /tmp` shows `/private/tmp` in `pwd`).
  toybox `id` fails for a uid that has no passwd entry (`id: bad uid 501`); `$UID` works.
- Tools that need a libSystem function or a `/usr/lib` library the guest does not have yet stop at
  load time (`Symbol not found`, `image not loaded`).
- `kido cp` of a tree that contains sockets fails at the end; device nodes are skipped.
- Under QEMU, guest timers fire 20+ ms late when kido is started from a shell that macOS clamps to a
  utility or background QoS (for example a shell started by a launchd agent without
  `ProcessType=Interactive`): QEMU inherits the clamp and macOS wakes its idle vCPUs late.
  `kido doctor` warns about it; from a normal terminal the delay is a few milliseconds.
- Containers do not restart on their own, for example after the Mac reboots.
- No USB passthrough and no GPU.

## Architecture

    kido (Go CLI, runs as you)
     |-- internal/runtime: one VM per container, state in $KIDO_HOME
     |     QEMU: qemu-system-aarch64 -machine virt -accel hvf -kernel kernel -initrd initrd.cpio
     |           -drive disk.qcow2 (overlay on the image's disk.img)  -netdev user (NAT, hostfwd)
     |     VZ:   kido-vmm (Virtualization.framework, APFS clone of disk.img, vmnet)
     |-- internal/build: Dockerfile subset on top of the engine (RUN in a build VM)
     |-- internal/mount: copy mounts (-v) through tar disks
     |-- internal/registry: OCI push/pull (zstd disk layer, image config, Docker credentials)
     '-- internal/setup: QEMU download, verify, unpack, sign; kido doctor
            |
            | 127.0.0.1:<random port> --> guest port 1023, authenticated and encrypted
            v
    Darwin guest: xnu-loader -> XNU 25.5 arm64 -> launchd -> kido-agent (exec, PTYs, signals,
    files as tar streams, shutdown)

The kernel boots directly (no firmware); the boot arguments carry the network configuration, the
clock and the agent's credentials.

## Troubleshooting

Start with `kido doctor`; every failed check prints its fix.

- **QEMU not found**: run `kido setup`, or set `KIDO_QEMU`.
- **QEMU libraries: N missing**: the QEMU build expects Homebrew libraries; install the listed
  formulas or use a self-contained QEMU (see Setup).
- **QEMU entitlement**: HVF refuses to start without `com.apple.security.hypervisor`;
  `kido setup --force` re-signs the QEMU it installed. Sign your own QEMU (`KIDO_QEMU`) with:

      cat > hvf.entitlements <<'EOF'
      <?xml version="1.0" encoding="UTF-8"?>
      <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
      <plist version="1.0"><dict><key>com.apple.security.hypervisor</key><true/></dict></plist>
      EOF
      codesign --force --sign - --entitlements hvf.entitlements "$KIDO_QEMU"

- **KIDO_HOME too long**: `kido commit` of a running QEMU container pauses it through a unix socket
  under `$KIDO_HOME`, which limits `$KIDO_HOME` to 66 characters; other commands work with a longer
  one.
- **A container does not start**: `kido logs --console NAME` shows the guest's serial console
  (QEMU).
- **Docker-free WARN**: registry commands read Docker's `config.json`; a `credsStore` helper that
  is not on PATH makes pulls and pushes anonymous.

## Environment

| Variable | Meaning |
|---|---|
| `KIDO_HOME` | State directory (default `~/.kido`): `qemu/`, `images/`, `containers/`, `networks/` |
| `KIDO_QEMU` | Absolute path of `qemu-system-aarch64`; overrides what `kido setup` installed |
| `KIDO_BACKEND` | `qemu` (default) or `vz` (experimental) |
| `KIDO_VMM` | Path of the `kido-vmm` helper for the vz backend |
| `KIDO_VZ_SUBNET` | vmnet's NAT network for the vz backend when it is not `192.168.64.0/24` (IPv4, /16 to /29) |
| `DOCKER_CONFIG` | Where registry credentials are read and stored (default `~/.docker`) |

## License

kido's code is licensed under the Apache License, Version 2.0; see [LICENSE](LICENSE).

kido does not include a guest image or QEMU. Guest images contain third-party software under its own
licenses: the Apple Public Source License 2.0 (XNU and Apple's other open-source Darwin code), BSD
licenses (PureDarwin's own code, xnu-loader and other tools) and the Mozilla Public License 2.0
(corecrypto). QEMU, which `kido setup` downloads, is licensed under the GNU General Public License,
version 2.

## Acknowledgements

kido's guest builds on the work of these projects, with thanks:

- [PureDarwin](https://github.com/PureDarwin/PureDarwin), the Darwin build the guest system started
  from (BSD-2-Clause).
- [xnu-loader](https://github.com/PureDarwin/xnu-loader) by Vali0004, which boots XNU as an arm64
  Linux kernel image (BSD-3-Clause).
- Apple's [open-source Darwin releases](https://opensource.apple.com/), including XNU
  (Apple Public Source License 2.0).
- NeoDarwin's [corecrypto](https://github.com/NeoDarwinOS/corecrypto) (MPL-2.0). The guest uses
  PureDarwin's interim fork of it; NeoDarwin does not maintain that fork.

kido is an independent project, not affiliated with Apple, PureDarwin or NeoDarwin.
