# kl, the system package manager

kl installs the system libraries and frameworks that KidoDarwin's base image leaves out. It puts each
one where macOS keeps it, under the same install name, so tools built for macOS find it. kl runs
inside a container.

kl is still being built. Its package repository at kl.kido.sh is not online yet, and the image does
not include kl yet.

```console
$ kl add libsqlite3
$ kl which /usr/lib/libsqlite3.dylib
/usr/lib/libsqlite3.dylib: libsqlite3
$ kl list
libsqlite3 3.51.0
```

## Commands

| Command | What it does |
|---|---|
| `kl add NAME...` | Installs packages and the packages they need. |
| `kl rm NAME...` | Removes packages, and the packages nothing else needs any more. |
| `kl list` | Lists installed packages. A package installed only for another one says so. |
| `kl search TERM` | Finds packages by name or summary. |
| `kl info NAME` | Shows a package's version, size, license, and what it provides and needs. |
| `kl which PATH` | Names the package a file belongs to, whether installed or available in the index. Works for both dylibs (`/usr/lib/libffi.dylib`) and other files (`/usr/bin/python3`). |
| `kl verify [NAME]` | Checks installed files against their hashes and lists any that changed or went missing. |
| `kl update` | Fetches and checks the newest package index. |

### Options

- `--root DIR` installs into another root, such as an image being built.
- `--repo URL` sets the primary repository URL (default `https://kl.kido.sh`).
- `--mirror URL` sets an untrusted mirror URL tried before `--repo`. Repeat `--mirror` to try
  several mirrors in order.
- `--kl-version VER` sets the repository schema version (default `v1`).
- `--os-version X.Y[.Z]` and `--os-build BUILD` select the target OS version and build. Two-part
  versions such as `26.5` or `27.0` are padded to three numeric segments (`26/5/0`, `27/0/0`).
- `--channel latest` is the default. `--channel nightly` gets new versions first.
- `--arch arm64e` or `--arch arm64` picks the build. kl picks arm64e on CPUs with pointer
  authentication, as every Apple Silicon Mac has.
- `--keys FILE` overrides the trusted SSH Ed25519 signing keys.

## Repository layout (v1)

Every kl v1 repository uses the permanent layout below:

```text
https://kl.kido.sh/<kl_version>/<major>/<minor>/<patch>/<build>/<channel>/
  timestamp.json
  timestamp.json.sig
  <arch>/
    index.<sha256>.json

https://kl.kido.sh/<kl_version>/blobs/sha256/<digest>
```

Examples:

- `https://kl.kido.sh/v1/26/1/1/24B52/latest/` (macOS 26.1.1 beta build `24B52`)
- `https://kl.kido.sh/v1/26/1/1/24B56/latest/` (macOS 26.1.1 release build `24B56`)
- `https://kl.kido.sh/v1/26/5/0/25F79/latest/` (macOS 26.5 padded to three segments)
- `https://kl.kido.sh/v1/27/0/0/26B118/latest/` (macOS 27.0 padded to three segments)
- `https://kl.kido.sh/v1/blobs/sha256/<digest>` (shared content-addressed blobs across all OS
  versions, builds, and channels under `v1`)

## Configuration and OS discovery

kl reads defaults from `<root>/etc/kl.conf` if it exists. CLI flags override `/etc/kl.conf`. When
`os_version` or `os_build` is not set by CLI flags or `/etc/kl.conf`, kl reads `ProductVersion` and
`ProductBuildVersion` from `<root>/System/Library/CoreServices/SystemVersion.plist`.

Example `/etc/kl.conf`:

```ini
repo = https://kl.kido.sh
mirror = https://kl-cache.corp.example/kl
mirror = http://10.0.1.10:8080
kl_version = v1
os_version = 26.5
os_build = 25F79
channel = latest
arch = arm64e
```

Instead of `os_version`, `/etc/kl.conf` also accepts `major`, `minor`, and optional `patch`
(default `0`). `mirror` may be repeated to configure multiple mirrors in priority order.

## Mirrors and transport policy

Mirrors are plain static HTTP or HTTPS servers (such as Nginx, Caddy, S3, or R2) holding the same
`v1/` directory layout. kl treats every mirror as untrusted transport:

- To prevent a stale or malicious mirror from freezing or downgrading a client, kl fetches the
  small signed `timestamp.json` from the primary `--repo` first whenever it is reachable, then
  downloads the content-addressed index and package blobs from mirrors in configured order before
  falling back to the primary repo.
- If the primary repository is unreachable, kl warns on stderr and falls back to mirror timestamps,
  verifying every mirror with the same Ed25519 SSHSIG signature, target-binding, expiry, and
  monotonic rollback checks and picking the highest verified timestamp version across mirrors.
- If any mirror is down, times out, returns a redirect or non-200 HTTP status, serves stale or
  wrong-build metadata, or serves a tampered blob chunk, kl warns on stderr, discards any staged
  data from that mirror, and falls back to the next mirror and finally to the primary `--repo`.
- Primary repositories require `https://`, `file://`, or loopback `http://` (`127.0.0.1`, `[::1]`,
  `localhost`).
- Explicitly configured mirrors (`--mirror` or `mirror =` in `/etc/kl.conf`) allow `https://`,
  `file://`, and `http://` on loopback or private LAN hosts (RFC 1918 IPv4, IPv6 unique-local or
  link-local, `.local`, `.internal`, or single-label internal hostnames). Resolved IP addresses are
  checked after DNS resolution before connecting, external HTTP proxies are disabled for cleartext
  mirrors, public cleartext `http://` URLs and HTTP redirects are refused.

## What a package can do

- A package can only install into `/usr/lib`, `/usr/libexec`, `/usr/bin`, `/usr/share`, and the
  Frameworks and PrivateFrameworks folders in `/System/Library`.
- Packages carry no scripts. Installing one only adds files.
- kl never overwrites a file that no package owns, and never installs or removes through a symlinked
  folder.
- When kl removes a package, it keeps any file you changed and tells you.

## How kl checks what it downloads

- The repository signs a small `timestamp.json` that cryptographically binds the exact target
  (`kl_version`, `major`, `minor`, `patch`, `build`, `channel`), expiry time, monotonic version, and
  per-architecture index hashes.
- kl refuses a timestamp if the signature is not from a trusted Ed25519 key, if it targets a
  different OS version, build, schema version, or channel (preventing cross-build replay and
  mix-and-match attacks), if it has expired, or if it is older than the highest version seen for
  that target.
- Rollback state in `<root>/var/db/kl/state.json` is keyed per target
  (`<kl_version>/<major>/<minor>/<patch>/<build>/<channel>`), so switching channels or builds never
  resets rollback protection.
- The timestamp names the package index by its SHA-256, and the index names every package blob and
  file list by SHA-256.
- kl checks each package in 4 MiB chunks before any byte reaches the zstd or tar decoder. A package
  that was changed stops at the first bad chunk.
- kl unpacks into a staging folder, checks every file's hash, and only then moves the files into
  place. If kl is interrupted, the next run validates and finishes the journal.

## Building a repository

kl-repo builds and signs a v1 repository that kl can install from.

```sh
kl-repo --repo ./repo --os-version 26.1.1 --os-build 24B56 --channel latest \
  pack --arch arm64e --from ./tree --name libfoo --version 1.0 \
  --license MIT --summary "the foo library" usr/lib/libfoo.dylib
kl-repo --repo ./repo --os-version 26.1.1 --os-build 24B56 --channel latest \
  index --arch arm64e --version 1
kl-repo --repo ./repo --os-version 26.1.1 --os-build 24B56 --channel latest \
  timestamp --version 1
kl-repo --repo ./repo --os-version 26.1.1 --os-build 24B56 --channel latest \
  sign --key ./signing-key
```

- `pack` reads what each Mach-O file provides and needs from its load commands, compresses with zstd
  at level 22, and writes blobs into `<repo>/<kl_version>/blobs/sha256/<digest>`. Publishing the
  same package across multiple OS builds automatically deduplicates blobs on disk.
- `index` supports incremental publishing: it loads any existing published index for that target and
  architecture, overlays newly packed entries from `packages.d/`, and reuses existing blobs without
  re-packing unchanged packages.
- `sign` verifies that `timestamp.json` matches the requested target and invokes
  `ssh-keygen -Y sign`, so the private key never passes through kl-repo.
