# Troubleshooting

Start with `kido doctor`. Every failed check prints its fix.

- **QEMU not found**: run `KIDO_BACKEND=qemu kido setup`, or set `KIDO_QEMU`.
- **QEMU libraries: N missing**: the QEMU build expects some Homebrew libraries. Install the listed
  formulas, or use a self-contained QEMU (see [Setup](usage.md#setup)).
- **QEMU entitlement**: Hypervisor.framework refuses to start QEMU without
  `com.apple.security.hypervisor`. `KIDO_BACKEND=qemu kido setup --force` signs the QEMU it installed
  again. Sign your own QEMU (`KIDO_QEMU`) with:

      cat > hvf.entitlements <<'EOF'
      <?xml version="1.0" encoding="UTF-8"?>
      <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
      <plist version="1.0"><dict><key>com.apple.security.hypervisor</key><true/></dict></plist>
      EOF
      codesign --force --sign - --entitlements hvf.entitlements "$KIDO_QEMU"

- **`Symbol not found` or `image not loaded`**: the tool uses a libSystem function or a `/usr/lib`
  library that KidoDarwin does not have yet. Please open an issue at
  [kido-sh/darwin](https://github.com/kido-sh/darwin/issues) with the tool and the message.
- **A container does not start**: `kido logs --console NAME` shows the guest's serial console
  (QEMU only).
- **`macOS blocked kido from reaching the VM`**: the Virtualization.framework backend reaches each
  VM over vmnet's network, and macOS's Local Network privacy covers that network. Terminal and SSH
  sessions are allowed. Another app that runs kido asks once. If you said no, turn that app on in
  System Settings > Privacy & Security > Local Network. A launchd agent cannot ask, so run kido
  from a terminal instead.
- **KIDO_HOME too long**: `kido commit` of a running QEMU container pauses it through a unix socket
  under `$KIDO_HOME`. This limits `$KIDO_HOME` to 66 characters. Other commands work with a longer
  one.
- **Credentials WARN**: registry commands read `~/.docker/config.json`. A `credsStore` helper that
  is not on PATH makes pulls and pushes anonymous.
- **Timers 20 ms late under QEMU**: macOS may clamp a shell to a utility or background QoS, for
  example a shell started by a launchd agent without `ProcessType=Interactive`. QEMU inherits the
  clamp, and macOS then wakes its idle vCPUs late. `kido doctor` warns about it. From a normal
  terminal the delay is a few milliseconds.
- **`id: bad uid 501`**: toybox `id` fails for a uid that has no passwd entry. `$UID` works.
- **`-w /tmp` shows `/private/tmp`**: `/tmp` and `/var` are symlinks into `/private`, as on macOS.
- **`kido cp` fails at the end**: the tree contains sockets. Device nodes are skipped.
