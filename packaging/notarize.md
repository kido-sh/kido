# Signing and notarizing a kido release

A release is a Developer ID signed, notarized and stapled `.pkg` that installs `bin/kido`,
`libexec/kido/kido-agent`, `libexec/kido/kido-vmm` and the docs under `/usr/local`. The Homebrew
cask template (`homebrew/kido-cask.rb.template`) installs that pkg. Nothing here has been run
against Apple's notary service yet; the commands are the documented ones for Xcode 14 and later.

Replace `NAME (TEAMID)` with the certificate's common name and `TEAMID` with the team ID.

## Once per machine

1. Join the Apple Developer Program and create two certificates: **Developer ID Application**
   (signs the binaries) and **Developer ID Installer** (signs the pkg). Import them into the login
   keychain and check:

       security find-identity -v -p codesigning     # Developer ID Application: NAME (TEAMID)
       security find-identity -v                    # also lists Developer ID Installer: NAME (TEAMID)

2. Store notary credentials in the keychain, never in a file or the shell history. With an
   app-specific password (notarytool prompts for it; do not pass `--password`):

       xcrun notarytool store-credentials kido-notary --apple-id you@example.com --team-id TEAMID

   or with an App Store Connect API key (keep the `.p8` outside the repository):

       xcrun notarytool store-credentials kido-notary --key /secure/AuthKey_KEYID.p8 --key-id KEYID --issuer ISSUER-UUID

## Every release

1. Build and sign from a clean tree, on the release tag so `git describe` gives the version:

       make clean
       make dist DEVELOPER_ID="Developer ID Application: NAME (TEAMID)"

   `make sign` (a prerequisite of `dist`) signs every binary in `BINARIES` with
   `--options runtime --timestamp` (the timestamp needs network access) and with
   `packaging/entitlements/<binary>.entitlements` when that file exists. `bin/kido` and
   `bin/kido-agent` have none; `bin/kido-vmm`, the Virtualization.framework helper, gets
   `com.apple.security.virtualization` from `kido-vmm.entitlements`.

2. Check the signatures. Notarization rejects a Mach-O without the hardened runtime
   (`flags=0x10000(runtime)`), a secure timestamp, or with `com.apple.security.get-task-allow`:

       for b in bin/kido bin/kido-agent bin/kido-vmm; do
         codesign --verify --strict --verbose=2 "$b"
         codesign -dv "$b" 2>&1 | grep -E 'flags|Timestamp|TeamIdentifier'
         codesign -d --entitlements - "$b"
       done

   `kido-agent` runs inside the guest, but it is signed the same way because the notary service
   checks every Mach-O in the pkg. Whether the guest's dyld accepts a hardened-runtime binary has
   not been tested: boot an image baked with the signed agent before publishing.

3. Build the pkg from the tree in the tarball `make dist` wrote, signed with the installer
   certificate:

       V=$(cat bin/.version)
       mkdir "dist/kido-$V-darwin-arm64" && tar -xzf "dist/kido-$V-darwin-arm64.tar.gz" -C "dist/kido-$V-darwin-arm64"
       pkgbuild --root "dist/kido-$V-darwin-arm64" --install-location /usr/local \
         --identifier sh.kido.pkg --version "$V" \
         --sign "Developer ID Installer: NAME (TEAMID)" --timestamp \
         "dist/kido-$V.pkg"
       pkgutil --check-signature "dist/kido-$V.pkg"

4. Notarize and wait for the verdict:

       xcrun notarytool submit "dist/kido-$V.pkg" --keychain-profile kido-notary --wait

   On `Invalid`, read why (the submission ID is in the output above):

       xcrun notarytool log SUBMISSION-ID --keychain-profile kido-notary

5. Staple the ticket to the pkg and check it as Gatekeeper will:

       xcrun stapler staple "dist/kido-$V.pkg"
       xcrun stapler validate "dist/kido-$V.pkg"
       spctl --assess --type install --verbose=2 "dist/kido-$V.pkg"   # source=Notarized Developer ID

   A ticket cannot be stapled to a bare binary or to the `.tar.gz`; Gatekeeper looks those up
   online. Ship the pkg for installs that should work offline.

6. Publish `dist/kido-$V.pkg` on the release, then fill the cask template's version and
   `shasum -a 256 "dist/kido-$V.pkg"`.

## QEMU

QEMU is not part of a release: `kido setup` downloads it, and shipping it would bring GPLv2
obligations. To make a local QEMU usable with HVF, sign it ad-hoc with the hypervisor entitlement:

    codesign --force --sign - --entitlements packaging/entitlements/qemu-hvf.entitlements /path/to/qemu-system-aarch64
