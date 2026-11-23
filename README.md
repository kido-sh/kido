# kido

kido runs macOS command-line tools in small Darwin VMs on Apple Silicon Macs. You use it like
Docker: `kido run`, `kido build`, `kido push`. Each container boots a real Darwin kernel (XNU) in
about half a second.

```console
$ kido run --rm darwin sw_vers
ProductName:	KidoDarwin
ProductVersion:	26.5
BuildVersion:	25F74
$ kido run --rm darwin uname -a
Darwin kido 25.5.0 Darwin Kernel Version 25.5.0: Mon Nov 23 13:26:52 UTC 2026; nixbld1:xnu-12377.121.6~1/RELEASE_ARM64_VIRT arm64
```

## Quick start

You need an Apple Silicon Mac with macOS 15 or later. kido needs neither Docker nor root.

Until the first release is out, build kido from source. This needs Go and the Xcode command line
tools.

```sh
git clone https://github.com/kido-sh/kido.git
cd kido
make install PREFIX=~/.local
export PATH="$HOME/.local/bin:$PATH"
kido setup --image ghcr.io/kido-sh/darwin
kido run --rm darwin sw_vers
```

`kido setup` downloads QEMU and the Darwin image, about 130 MB in all. If QEMU needs libraries
from Homebrew, `kido setup` tells you which ones to install.

## Examples

Open a shell:

```console
$ kido run -it --rm darwin /bin/sh
```

Keep a container running, run commands in it and copy files into it:

```console
$ kido run -d --name box darwin sleep 600
$ kido exec box sw_vers -productName
KidoDarwin
$ echo hello > hello.txt
$ kido cp hello.txt box:/tmp/
$ kido exec box cat /tmp/hello.txt
hello
$ kido rm -f box
```

Put your own macOS tool in an image. This Dockerfile adds yq, from its official macOS arm64
release:

```dockerfile
FROM darwin
COPY yq /usr/local/bin/yq
ENTRYPOINT ["/usr/local/bin/yq"]
```

```console
$ curl -fsSL -o yq https://github.com/mikefarah/yq/releases/latest/download/yq_darwin_arm64
$ chmod +x yq
$ kido build -t yq .
$ echo 'kernel: xnu' | kido run --rm -i yq .kernel
xnu
```

Share images through any OCI registry, such as GitHub's:

```console
$ kido login ghcr.io
$ kido push yq ghcr.io/YOUR-NAME/yq
$ kido pull ghcr.io/YOUR-NAME/yq
```

[docs/usage.md](docs/usage.md) covers the rest: every command, networks, ports and `-v`.

## How it works

Each container is its own small VM, with its own kernel, memory and disk. kido starts it with QEMU
and Hypervisor.framework, or with Apple's Virtualization.framework (`KIDO_BACKEND=vz`,
experimental). The system inside is [KidoDarwin](https://github.com/kido-sh/darwin): Apple's
open-source XNU kernel, launchd and a small set of Unix tools. A small agent in the VM runs your
commands. kido talks to it over an encrypted and authenticated channel; see
[docs/security.md](docs/security.md).

## What runs

- macOS arm64 command-line tools run unchanged, if KidoDarwin has the functions and libraries they
  use. Universal binaries run their arm64 part.
- Some tools still stop at launch with `Symbol not found` or `image not loaded`. Please
  [open an issue](https://github.com/kido-sh/darwin/issues) with the tool's name.
- Apple's own binaries do not run: they are built for arm64e. Tools built only for x86_64 do not
  run either (there is no Rosetta), and neither do GUI apps.
- `-v` copies files in and out. It is not a live mount.
- There is no USB passthrough and no GPU.

kido is experimental. Commands and the image format may still change.

## Troubleshooting

Run `kido doctor`. Every failed check prints its fix.
[docs/troubleshooting.md](docs/troubleshooting.md) covers the rest.

## License

kido is licensed under the Apache License 2.0; see [LICENSE](LICENSE). The Darwin image and QEMU
are separate downloads with their own licenses. The image's licenses are listed in
[KidoDarwin's LICENSING.md](https://github.com/kido-sh/darwin/blob/main/LICENSING.md). QEMU is
licensed under the GNU General Public License, version 2.

## Acknowledgements

kido's Darwin system builds on [PureDarwin](https://github.com/PureDarwin/PureDarwin), on
[xnu-loader](https://github.com/PureDarwin/xnu-loader) by Vali0004, on Apple's
[open-source releases](https://opensource.apple.com/) and on NeoDarwin's
[corecrypto](https://github.com/NeoDarwinOS/corecrypto). Thank you all.

kido is an independent project. It is not affiliated with Apple, Docker, PureDarwin or NeoDarwin.
