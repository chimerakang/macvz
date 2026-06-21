# Release: Signing & Notarization

How to build, sign, notarize, and publish the MacVz Apple Silicon macOS
artifacts (issue #29). MacVz is darwin/arm64 only.

> For installing/upgrading/rolling back/removing the built artifacts on a node,
> see [PACKAGING.md](PACKAGING.md) (issue #70). `make release` produces both the
> single-binary tarball and a self-contained install bundle.

## Signing identity & entitlements

- **Identity:** a **Developer ID Application** certificate (from an Apple
  Developer account) is required to distribute and notarize outside the App
  Store. App Store / "Apple Distribution" certs do **not** work for notarized
  direct distribution.
- **Hardened runtime:** release binaries are signed with `--options runtime`,
  which notarization requires.
- **Entitlements:** [build/macvz-kubelet.entitlements](../build/macvz-kubelet.entitlements)
  is intentionally empty. macvz-kubelet is a pure-Go process that drives the
  `apple/container` CLI; it does **not** link Virtualization.framework, so it
  needs no `com.apple.security.virtualization` entitlement — that lives in
  apple/container's own signed binaries. If a future change makes macvz-kubelet
  use VZ directly, add that entitlement here and re-notarize.

## One-command release

```sh
# Developer ID signing + notarization (keychain profile):
CODESIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)" \
NOTARYTOOL_PROFILE="macvz-notary" \
make release

# …or CI-style credentials instead of a keychain profile:
CODESIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)" \
NOTARY_APPLE_ID="you@example.com" NOTARY_TEAM_ID="TEAMID" NOTARY_PASSWORD="app-specific-pw" \
make release
```

`make release` runs [scripts/macos-release.sh](../scripts/macos-release.sh),
which: builds `macvz-kubelet` and `macvz-netd` for darwin/arm64 with version
stamping → codesigns both (hardened runtime + entitlements for Developer ID
builds) → verifies the signatures → packages the legacy
`dist/macvz-kubelet_<version>_darwin_arm64.tar.gz` single-binary tarball plus
the self-contained `dist/macvz_<version>_darwin_arm64.tar.gz` install bundle →
and, when notarization credentials are present, submits the install bundle with
`notarytool --wait`.

Create a notarytool keychain profile once with:

```sh
xcrun notarytool store-credentials macvz-notary \
  --apple-id you@example.com --team-id TEAMID --password <app-specific-password>
```

> Note: a bare CLI binary cannot be *stapled* (stapling targets `.pkg`/`.dmg`/
> `.app`). Notarization still applies — the ticket is served online and
> Gatekeeper checks it on first run. Wrap the binary in a signed `.pkg` if you
> need an offline-staple-able installer.

## Local developer fallback (no Apple identity)

For day-to-day local builds, sign ad-hoc — runs locally, not distributable:

```sh
make sign      # builds bin/macvz-kubelet and ad-hoc codesigns it
# or, full ad-hoc packaging into dist/:
make release   # CODESIGN_IDENTITY defaults to "-" (ad-hoc), notarization skipped
```

## CI release

[.github/workflows/release.yml](../.github/workflows/release.yml) runs on a
`v*` tag push on a macos runner. It imports a Developer ID cert into a temporary
keychain, runs the same release script, and attaches every `dist/*.tar.gz`
artifact plus its checksum to the GitHub Release (single-binary tarball and
install bundle). Signing/notarization activate only when these repository
secrets are set (otherwise the workflow falls back to ad-hoc artifacts, so forks
still build):

| Secret | Purpose |
| --- | --- |
| `MACOS_CERT_P12_BASE64` | base64 of the Developer ID Application `.p12` |
| `MACOS_CERT_PASSWORD` | password for that `.p12` |
| `MACOS_SIGN_IDENTITY` | `Developer ID Application: Name (TEAMID)` |
| `NOTARY_APPLE_ID` / `NOTARY_TEAM_ID` / `NOTARY_PASSWORD` | notarytool credentials |

Cut a release:

```sh
git tag v0.4.0 && git push origin v0.4.0
```

## Install

```sh
# Managed node install bundle (recommended):
tar -xzf macvz_<version>_darwin_arm64.tar.gz
shasum -a 256 -c macvz_<version>_darwin_arm64.tar.gz.sha256
cd macvz_<version>_darwin_arm64
sudo ./macvz-install.sh install --from . --config config.example.yaml

# Legacy single-binary kubelet tarball:
tar -xzf macvz-kubelet_<version>_darwin_arm64.tar.gz
shasum -a 256 -c macvz-kubelet_<version>_darwin_arm64.tar.gz.sha256
sudo install -m 0755 macvz-kubelet /usr/local/bin/macvz-kubelet
macvz-kubelet --version
```

## Verify signature & notarization

```sh
# Signature valid, hardened runtime present, and the signing authority:
codesign --verify --strict --verbose=2 /usr/local/bin/macvz-kubelet
codesign --display --verbose=2 /usr/local/bin/macvz-kubelet   # look for flags=…(runtime)
codesign --verify --strict --verbose=2 /usr/local/sbin/macvz-netd

# Gatekeeper assessment of a notarized executable:
spctl --assess --type execute --verbose=4 /usr/local/bin/macvz-kubelet
spctl --assess --type execute --verbose=4 /usr/local/sbin/macvz-netd

# Notarization history for an artifact (with credentials):
xcrun notarytool history --keychain-profile macvz-notary
```

A correctly signed release shows `flags=0x10000(runtime)`, an
`Authority=Developer ID Application: …` chain, and a set `TeamIdentifier`. An
ad-hoc/dev build shows `flags=…(adhoc)` and `TeamIdentifier=not set` — expected
for local builds, not for distribution.
