# Using kido

## Requirements

- An Apple Silicon Mac with macOS 15 or later. `kido doctor` checks this. User networks on the
  Virtualization.framework backend need macOS 26.
- Disk space for images and for what each container writes. The Darwin image has an 8 GiB root
  file system, but it is stored sparse and takes about 130 MB.
- QEMU 11.1 with Hypervisor.framework support, only for the QEMU backend.
  `KIDO_BACKEND=qemu kido setup` installs it.

## Install

    curl -fsSL https://kido.sh/install.sh | sh
    curl -fsSL https://kido.sh/install.sh | sh -s -- --prefix ~/opt/kido --version 0.1.0 --no-setup
    brew install kido-sh/tap/kido

The script puts kido into `~/.local` (no sudo), checks the release's signature and SHA-256 and then
runs `kido setup`, which pulls the Darwin image. `--image REF` pulls another base image, and
`--no-setup` skips `kido setup`. After `brew install`, run `kido setup` yourself.

From source, with Go 1.26 and the Xcode command line tools:

    git clone https://github.com/kido-sh/kido.git
    cd kido
    make                          # bin/kido, bin/kido-agent and bin/kido-vmm
    make install PREFIX=~/.local  # signs them ad hoc, installs bin/kido and libexec/kido/
    make dist                     # dist/kido-VERSION-darwin-arm64.tar.gz, install.sh and SHA256SUMS

`kido upgrade` upgrades a kido from the install script or a release tarball. It downloads the
latest release from GitHub and checks its signature and SHA-256. Then it replaces kido and its
helpers. `kido upgrade --check` only tells you whether there is a newer release. A kido from
Homebrew upgrades with `brew upgrade kido`.

Shell completion: `eval "$(kido completion bash)"` (macOS's bash 3.2 cannot `source <(...)`),
`source <(kido completion zsh)`, or `kido completion fish > ~/.config/fish/completions/kido.fish`.

## Setup

    kido setup

This pulls the Darwin image `ghcr.io/kido-sh/darwin` as `darwin:latest`. `--image` names another
base image, and `--image-name` another local name. Running it again downloads nothing new.
`kido setup --force` pulls the image again, which needs every container of the old one removed.

The QEMU backend also needs QEMU. `KIDO_BACKEND=qemu kido setup` downloads QEMU 11.1.3 for darwin
arm64 (85.5 MB, from github.com/cocoa-xu/qemu-build) over HTTPS and checks it against a SHA-256
pinned in kido. It unpacks only what kido uses (about 50 MB) into `~/.kido/qemu/11.1.3`. If the
signature of `qemu-system-aarch64` lacks the `com.apple.security.hypervisor` entitlement, it signs
it again. Then it checks that QEMU and qemu-img start, and points `~/.kido/qemu/current` at that
version. `$KIDO_QEMU` takes precedence: an absolute path to `qemu-system-aarch64`, with qemu-img
next to it.

The pinned QEMU build loads some libraries from Homebrew (`/opt/homebrew/opt/...`). If they are
missing, `kido setup` stops and names them, for example `brew install capstone dtc gnutls snappy
vde`. Install them and run it again. `packaging/qemu/bundle-dylibs.sh` makes a QEMU tree
self-contained, and `kido setup --qemu-url URL-or-FILE --qemu-sha256 HEX` installs such an archive
(`.tar.xz` with `bin/`, `lib/` and `share/qemu/`, optionally under `usr/local/`).

## Images

The Darwin image comes from [kido-sh/darwin](https://github.com/kido-sh/darwin). Your own images
build on it with `FROM darwin`.

    kido pull ghcr.io/kido-sh/darwin:26.5             # stored as ghcr.io/kido-sh/darwin:26.5
    kido pull ghcr.io/kido-sh/darwin:26.5 darwin:26.5 # stored as darwin:26.5

Like Docker, `kido run`, `kido create` and `kido build` pull an image first if it is not here.
`--pull never` turns this off for `run` and `create`.

A base image can also be a directory. It holds `disk.img` (a raw GPT disk with an EFI partition
and an ext4 root), `kernel` (the loader, built as an arm64 Linux kernel image), `initrd.cpio` (the
kernel collection) and `image.json` (with the root file system's `"rootUUID"`).

    kido image import DIR darwin    # becomes darwin:latest; an APFS clone, so it is cheap

`kido doctor` checks the whole setup.

## A few more examples

    $ kido run -it --rm darwin /bin/sh    # a shell; Ctrl-D leaves
    $ kido run -d --name box darwin sleep 600
    $ kido cp box:/etc/hosts ./hosts.txt
    $ kido top box
    $ kido export -o box.tar box
    $ kido rm -f box

Containers keep `/tmp` across stop, start and commit. `-u` and a Dockerfile `USER` may name any
uid, with or without a passwd entry. `kido push` and `kido pull` share credentials with Docker
(`~/.docker/config.json`).

## Backends

- **Virtualization.framework** (the default): `kido-vmm` runs each VM on an APFS clone of the
  image's disk, on vmnet. It is a small helper signed with `com.apple.security.virtualization`.
  `make sign` builds and signs it as `bin/kido-vmm`, and `make install`, Homebrew and `install.sh`
  put it in `libexec/kido/`. kido looks for it in `$KIDO_VMM`, next to the `kido` binary, and in
  `../libexec/kido`. Streams are much faster than under QEMU (exec runs at about 500-800 MB/s).
  This backend has no UDP port publishing yet, and `logs --console` stays empty, because the guest
  has no virtio-console driver yet. macOS's Local Network privacy covers vmnet's network. Terminal
  and SSH sessions are allowed, and any other app that runs kido asks once.
- **QEMU** (`KIDO_BACKEND=qemu`): `qemu-system-aarch64 -machine virt -accel hvf` boots the guest's
  kernel directly. It runs on a qcow2 overlay of the image's disk, behind QEMU's user-mode NAT.

## Networks

- **QEMU**: the guest sits behind QEMU's user-mode NAT. Outbound TCP, UDP and DNS work. Like
  Docker Desktop's `host.docker.internal`, the guest reaches every service on the host's loopback
  (127.0.0.1) at 10.0.2.2. That includes other containers' published ports and local dev servers.
  `--network none` cuts the guest off from both the host and the outside.
- **Virtualization.framework, default network**: every container sits on vmnet's shared NAT
  network (192.168.64.0/24; set `KIDO_VZ_SUBNET` if yours differs). These containers share one
  layer-2 network, so a root process in one of them can intercept another's traffic. The agent
  channel is safe from this: it is encrypted and authenticated. Any local process can reach a
  container's ports at its address.
- **Virtualization.framework, user networks** (macOS 26+): each group of containers gets its own
  vmnet network, subnet and bridge:

      kido network create [--subnet CIDR] [--internal] NAME     # also: ls, inspect, rm, prune
      kido run -d --network NAME darwin
      kido run -d --network none darwin

  - Containers on a network reach each other, the host (at .1, which also answers DNS) and the
    outside through NAT. `--internal` drops NAT and DNS. Containers on other networks cannot reach
    them. Subnets are picked from 192.168.128.0/24 upward, away from vmnet's default, other
    networks and the host's interfaces.
  - `--network none` gives the container a private host-only network without DNS. Only the host
    (kido's agent, published ports) reaches it.
  - A container keeps its network and address for its whole life: across stop and start, and
    after every kido process is gone. Definitions live in `$KIDO_HOME/networks/NAME.json`.
  - A vmnet network ends with the process that created it. So while containers use a network, a
    `kido-vmm hold-network` process owns it. It exits about two seconds after the last of them
    stops. If it is killed while containers run, they lose the network until they restart. macOS
    then keeps the subnet reserved until about a minute after the last VM on vmnet stops (from
    any tool), and the next start waits up to two minutes for it.
  - The host reaches every container's address, as Docker does on Linux.
- **Published ports** (`-p`) bind to 127.0.0.1 unless you name another address, on both backends
  and with `--network none`. kido does not use vmnet's own port forwarding, because it would also
  listen on the Mac's LAN addresses.

## Copy mounts: `-v` and `--mount type=bind`

`-v HOST:CONTAINER[:ro]` and `--mount type=bind,source=HOST,target=CONTAINER[,readonly]` copy
files. They are not live bind mounts.

- The host directory or file is copied in as a snapshot at every boot (create, start).
- Read-write mounts copy new and changed files back at `kido stop`, at the end of `kido run` (also
  with `--rm`) and at `kido rm -f`. Deletions in the container are not copied back; kido reports
  how many there were. If a file also changed on the host, kido does not overwrite it: it keeps the
  container's version next to it as `NAME.kido-<id>`. Hard-linked files are not copied back.
- With `:ro` (`readonly`), the container may change its copy, and nothing comes back.
- Changes are lost if the VM dies without a stop. `kido commit` includes the copy.
- The target must be absolute, and missing or empty in the image. System paths are refused.

## Commands

`kido COMMAND --help` lists every flag. Flags follow Docker's where they mean the same thing.

| Command | What it does |
|---|---|
| `run` | Create a container from an image and run a command in it (`-it --rm -d --name -e --env-file -w -u -p -v --mount --network --cpus -m --entrypoint --pull`) |
| `create`, `start`, `stop`, `restart`, `kill`, `wait` | Container lifecycle, as in Docker (`start -a -i`, `kill -s`). `stop` and `restart` send SIGTERM to the main process, wait `-t` seconds (default 10), then send SIGKILL. Only then do they copy `-v` mounts back and power off |
| `exec` | Run a command in a running container (`-it -d -e --env-file -w -u`) |
| `top` | Run `ps` in the container's VM (`ps -ef` by default, or the `ps` options given after the container) |
| `export` | Write a container's file system as a tar archive (`-o FILE`, or stdout). A stopped container boots without its command for this. Sockets and device nodes are skipped with a warning |
| `cp` | Copy files between a container and the host (`SRC CONTAINER:DST`, `CONTAINER:SRC DST`) |
| `ps`, `logs`, `inspect`, `port`, `rename`, `rm` | List, read the output (`logs --console`: the serial console; `--tail`), inspect (JSON), ports, rename, remove |
| `build` | Build an image from a Dockerfile: FROM, RUN, COPY, ADD (local files only, archives not unpacked), ENV, ARG, WORKDIR, USER, ENTRYPOINT, CMD, LABEL, EXPOSE. No SHELL and no multi-stage builds. `-q` prints only the image ID. `--no-cache` and `--pull` are accepted, but kido has no build cache and does not look for a newer FROM image |
| `commit` | Create an image from a container's disk |
| `images`, `rmi`, `tag`, `save`, `load` | List, remove and tag images; save them to and load them from an OCI-layout tar archive |
| `history` | An image's parent chain: ID, created, created-by and comment when recorded, size (`-H --no-trunc -q --format`) |
| `image import` | Import a base image directory (`disk.img`, `kernel`, `initrd.cpio`, `image.json`) |
| `pull`, `push`, `login` | OCI registries; credentials are shared with Docker's `~/.docker/config.json` |
| `container`, `image`, `system` | Docker-style groups, with `ls`, `prune`, `system df` and `system info` |
| `network` | User networks for the Virtualization.framework backend, macOS 26+ (`create --subnet --internal`, `ls`, `inspect`, `rm`, `prune`) |
| `setup` | Pull the Darwin image, and install QEMU for the QEMU backend |
| `doctor` | Check the Mac, the VM backend, KIDO_HOME, images, disk space and that no Docker is needed |
| `upgrade` | Replace this kido with the latest signed release (`--check`, `--version`) |
| `info`, `version`, `completion` | System information, version, shell completion (bash, zsh, fish) |

`ps`, `images`, `history`, `inspect`, `version` and `info` take `--format` (a Go template, `table`
or `json`). `inspect` templates use kido's own field names (a container's `{{.ID}}`,
`{{.State}}`), not Docker's (`{{.Id}}`, `{{.State.Running}}`).

Not there yet: `attach`, `stats`, `pause`, `diff`, `logout` and restart policies.

The exit status follows Docker: the command's own status, 125 when kido itself fails, and 126 or
127 when the guest cannot run or find the program. A failed `kido build` exits with the status of
its RUN step.

## Environment

| Variable | Meaning |
|---|---|
| `KIDO_HOME` | State directory (default `~/.kido`): `qemu/`, `images/`, `containers/`, `networks/` |
| `KIDO_QEMU` | Absolute path of `qemu-system-aarch64`; overrides what `kido setup` installed |
| `KIDO_BACKEND` | `vz` (default) or `qemu` |
| `KIDO_VMM` | Path of the `kido-vmm` helper for the vz backend |
| `KIDO_VZ_SUBNET` | vmnet's NAT network for the vz backend when it is not `192.168.64.0/24` (IPv4, /16 to /29) |
| `DOCKER_CONFIG` | Where registry credentials are read and stored (default `~/.docker`) |

## How it works

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
    KidoDarwin guest: loader -> XNU 25.5 arm64 -> launchd -> kido-agent (exec, PTYs, signals,
    files as tar streams, shutdown)

The kernel boots directly, without firmware. The boot arguments carry the network configuration,
the clock and the agent's credentials.
