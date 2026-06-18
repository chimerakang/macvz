#!/usr/bin/env bash
#
# macOS release pipeline for macvz-kubelet (Apple Silicon only):
#   build (darwin/arm64) → codesign → package tarball+checksum → optional
#   notarize → verify.
#
# Environment:
#   VERSION             Build version (default: git describe).
#   OUTPUT_DIR          Artifact directory (default: dist).
#   CODESIGN_IDENTITY   "Developer ID Application: …" for distribution, or "-"
#                       (the default) for an ad-hoc signature for local dev.
#                       Ad-hoc binaries run locally but cannot be notarized.
#   NOTARYTOOL_PROFILE  Keychain profile created with
#                       `xcrun notarytool store-credentials`. When set (and a
#                       real identity is used), the artifact is notarized.
#   NOTARY_APPLE_ID / NOTARY_TEAM_ID / NOTARY_PASSWORD
#                       Alternative notarization credentials for CI (Apple ID,
#                       team ID, app-specific password). Used when
#                       NOTARYTOOL_PROFILE is unset.
#
# See docs/RELEASE.md for the full operator guide.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BINARY="macvz-kubelet"
CMD="./cmd/macvz-kubelet"
DIST="${OUTPUT_DIR:-dist}"
ENTITLEMENTS="build/${BINARY}.entitlements"
VPKG="github.com/chimerakang/macvz/internal/version"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
DATE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
CODESIGN_IDENTITY="${CODESIGN_IDENTITY:--}"
NOTARYTOOL_PROFILE="${NOTARYTOOL_PROFILE:-}"
NOTARY_APPLE_ID="${NOTARY_APPLE_ID:-}"
NOTARY_TEAM_ID="${NOTARY_TEAM_ID:-}"
NOTARY_PASSWORD="${NOTARY_PASSWORD:-}"

# notarize_configured reports whether any notarization credential is available.
notarize_configured() {
	[ -n "$NOTARYTOOL_PROFILE" ] || { [ -n "$NOTARY_APPLE_ID" ] && [ -n "$NOTARY_TEAM_ID" ] && [ -n "$NOTARY_PASSWORD" ]; }
}

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

mkdir -p "$DIST"
out="$DIST/$BINARY"

log "Building $BINARY $VERSION (darwin/arm64)"
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build \
	-ldflags "-s -w -X $VPKG.Version=$VERSION -X $VPKG.Commit=$COMMIT -X $VPKG.Date=$DATE" \
	-o "$out" "$CMD"

if [ "$CODESIGN_IDENTITY" = "-" ]; then
	log "Ad-hoc signing (local/dev; NOT notarizable)"
	codesign --force --sign - "$out"
else
	log "Signing with hardened runtime as: $CODESIGN_IDENTITY"
	codesign --force --options runtime --timestamp \
		--entitlements "$ENTITLEMENTS" --sign "$CODESIGN_IDENTITY" "$out"
fi

log "Verifying signature"
codesign --verify --strict --verbose=2 "$out"
codesign --display --verbose=2 "$out" 2>&1 | sed 's/^/    /'

tarball="$DIST/${BINARY}_${VERSION}_darwin_arm64.tar.gz"
log "Packaging $tarball"
tar -C "$DIST" -czf "$tarball" "$BINARY"
( cd "$DIST" && shasum -a 256 "$(basename "$tarball")" > "$(basename "$tarball").sha256" )
log "Checksum: $(cat "$tarball.sha256")"

if notarize_configured; then
	if [ "$CODESIGN_IDENTITY" = "-" ]; then
		echo "error: notarization needs a Developer ID identity; set CODESIGN_IDENTITY" >&2
		exit 1
	fi
	zip="$DIST/${BINARY}_${VERSION}.zip"
	log "Submitting $zip for notarization"
	ditto -c -k --keepParent "$out" "$zip"
	if [ -n "$NOTARYTOOL_PROFILE" ]; then
		xcrun notarytool submit "$zip" --keychain-profile "$NOTARYTOOL_PROFILE" --wait
	else
		xcrun notarytool submit "$zip" \
			--apple-id "$NOTARY_APPLE_ID" --team-id "$NOTARY_TEAM_ID" --password "$NOTARY_PASSWORD" --wait
	fi
	# A bare Mach-O binary cannot be stapled (stapling targets .pkg/.dmg/.app);
	# the notarization ticket is served online and Gatekeeper checks it on first
	# run. Wrap in a .pkg if an offline-staple-able artifact is required.
	log "Notarization accepted (online ticket; bare CLI cannot be stapled)"
else
	log "Skipping notarization (no notarization credentials configured)"
fi

log "Done. Artifacts in $DIST/:"
find "$DIST" -maxdepth 1 -type f -print | sed 's/^/    /'
