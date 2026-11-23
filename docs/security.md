# Security model

- **One container is one VM.** XNU has no namespaces or cgroups, so the hypervisor is the isolation
  boundary. Every container gets its own kernel, memory and disk (a copy-on-write copy of the
  image's). QEMU and `kido-vmm` run as your user, and kido needs no sudo.
- **The agent channel.** A small agent in the guest (`kido-agent`, port 1023 inside the VM) runs
  commands for the CLI.
  - For every boot, kido makes a new ed25519 key pair and a boot secret. It passes only the public
    key and the secret to the guest, in its boot arguments, through a file rather than a command
    line.
  - At the first contact after boot, the agent must prove that it knows the boot secret. kido then
    pins the agent's identity key.
  - Every connection agrees on fresh X25519 keys and seals the request and every frame with
    AES-256-GCM. The host key signs every request. A process between kido and the agent can break
    a connection, but it cannot read or change the commands, environment, files and output that
    pass through it.
  - At boot, before the first contact, kido checks (with `lsof`) that its own VM process is the
    one listening on the agent port. Connections that do not authenticate are evicted, oldest
    first, so they cannot lock kido out.
- **Network.** On QEMU, the guest can reach the host's loopback. On the Virtualization.framework
  default network, containers share one layer-2 network: put containers that must not see each
  other on separate user networks. See [Networks](usage.md#networks).
- **Host state.** Everything lives in `$KIDO_HOME` (default `~/.kido`), with directories 0700 and
  files 0600. kido refuses a KIDO_HOME that another user owns, or that group or others can write.
  A container's console log is capped in size.
- **Files in and out.** `kido cp`, build contexts and copy mounts refuse paths that escape their
  directory (`..`, absolute paths, symlinks that point out).
- **Downloads.** `kido setup` fetches QEMU over HTTPS only and checks a SHA-256 pinned in the
  binary. It unpacks with size limits and does not follow links out of its directory.
- **The guest's own crypto.** The guest's corecrypto is a fork that nobody has audited (see
  [kido-sh/darwin](https://github.com/kido-sh/darwin#security)). Do not rely on the guest's system
  crypto library to protect anything. The agent uses Go's own cryptographic code, but the random
  numbers for its keys and nonces come from the guest's system random number generator
  (`arc4random_buf`), which that corecrypto provides.
