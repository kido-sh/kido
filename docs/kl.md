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
| `kl which PATH` | Names the package a file belongs to. For a library that is not installed yet, it names the package that provides it. This answers `Library not loaded`. |
| `kl verify [NAME]` | Checks installed files against their hashes and lists any that changed or went missing. |
| `kl update` | Fetches and checks the newest package index. |

- `--root DIR` installs into another root, such as an image being built.
- `--channel latest` is the default. `--channel nightly` gets new versions first.
- `--arch arm64e` or `--arch arm64` picks the build. kl picks arm64e on CPUs with pointer
  authentication, as every Apple Silicon Mac has.
- `--repo URL` and `--keys FILE` point kl at another repository and the keys that sign it.

## What a package can do

- A package can only install into `/usr/lib`, `/usr/libexec`, `/usr/bin`, `/usr/share`, and the
  Frameworks and PrivateFrameworks folders in `/System/Library`.
- Packages carry no scripts. Installing one only adds files.
- kl never overwrites a file that no package owns, and never installs through a symlinked folder.
- When kl removes a package, it keeps any file you changed and tells you.

## How kl checks what it downloads

- The repository signs a small timestamp file. kl refuses it if the signature is not from a trusted
  key, if it has expired, or if it is older than one kl has already seen. A mirror can therefore
  neither freeze kl on old packages nor roll it back.
- The timestamp names the package index by its sha256, and the index names every package by its
  sha256.
- kl checks each package in 4 MiB chunks before it decompresses any of them. A package that was
  changed stops at the first bad chunk.
- kl unpacks into a staging folder, checks every file's hash, and only then moves the files into
  place. If kl is interrupted, the next run finishes the move.

## Building a repository

kl-repo builds a repository that kl can install from.

```sh
kl-repo --repo ./repo pack --arch arm64e --from ./tree --name libfoo --version 1.0 \
  --license MIT --summary "the foo library" usr/lib/libfoo.dylib
kl-repo --repo ./repo index --arch arm64e --version 1
kl-repo --repo ./repo timestamp --version 1
kl-repo --repo ./repo sign --key ./signing-key
```

`pack` reads what each Mach-O file provides and needs from its load commands, so a package's
dependencies on libraries come for free. It compresses with zstd at level 22. `sign` runs
`ssh-keygen -Y sign`, so the private key never passes through kl-repo.
