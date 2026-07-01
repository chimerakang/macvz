#!/usr/bin/env bash
#
# macvz-cri-linuxpod-rehearsal.sh — exercise the macvz-cri-linuxpod-install.sh
# lifecycle (install → status → upgrade → rollback → clean → uninstall --purge)
# in a throwaway temp prefix with launchctl stubbed, so the paired versioned
# layout, plist rendering, rollback, and audited cleanup can be verified without
# root, a Swift toolchain, or a real Mac (CRI-L9-1 #149, CRI-L9-4 #152,
# CRI-L9-5 #153).
#
# It builds fake macvz-cri/linuxpod-helper binaries that satisfy --version and
# --preflight, then asserts after each step that the current/previous pair,
# rendered ProgramArguments, sockets, journals, and residue behave as
# documented. A live fresh-node rehearsal on real hardware is still required
# for service-level acceptance (see docs/CRI_LINUXPOD_L9_FRESH_NODE_RUNBOOK.md).
#
# Usage: ./macvz-cri-linuxpod-rehearsal.sh   (no args, exits non-zero on failure)
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="$HERE/macvz-cri-linuxpod-install.sh"

ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT

export HOME="$ROOT/home"
export MACVZ_CRI_LP_PREFIX="$ROOT/prefix"
export LAUNCHCTL=":"
PREFIX="$MACVZ_CRI_LP_PREFIX"
PAYLOAD_A="$ROOT/payload-a"
PAYLOAD_B="$ROOT/payload-b"
KERNEL="$ROOT/vmlinux-arm64"
mkdir -p "$HOME/Library/LaunchAgents" "$PAYLOAD_A" "$PAYLOAD_B"
printf 'fake kernel\n' >"$KERNEL"

FAILURES=0
pass() { printf "PASS %s\n" "$*"; }
fail() { printf "FAIL %s\n" "$*"; FAILURES=$((FAILURES+1)); }

# make_payload DIR VERSION — fake macvz-cri (answers --version/--preflight) and
# fake linuxpod-helper (any invocation exits 0).
make_payload() {
	local dir="$1" ver="$2"
	cat >"$dir/macvz-cri" <<EOF
#!/bin/sh
[ "\$1" = "--version" ] && echo "macvz-kubelet $ver (commit x, built y, darwin/arm64, go)" && exit 0
[ "\$1" = "--preflight" ] && echo "preflight ok ($ver)" && exit 0
exit 0
EOF
	cat >"$dir/linuxpod-helper" <<'EOF'
#!/bin/sh
exit 0
EOF
	chmod +x "$dir/macvz-cri" "$dir/linuxpod-helper"
}
make_payload "$PAYLOAD_A" "v1.0.0-a"
make_payload "$PAYLOAD_B" "v1.0.0-b"

ADAPTER_PLIST="$HOME/Library/LaunchAgents/io.macvz.cri.linuxpod.plist"
HELPER_PLIST="$HOME/Library/LaunchAgents/io.macvz.linuxpod-helper.plist"

step() { printf '\n=== %s ===\n' "$*"; }

step "install v1.0.0-a"
"$INSTALLER" install --from "$PAYLOAD_A" --kernel "$KERNEL" \
	--pod-cidr 10.244.102.0/24 --pod-network-interface bridge100 \
	--pod-network-ingress-interface en0 --pod-network-enable-forwarding \
	--volume-host-path-allowed "$ROOT/allowed" \
	--streaming-addr 127.0.0.1:0 --vmnet || fail "install exited non-zero"

[ "$(readlink "$PREFIX/current")" = "$PREFIX/versions/v1.0.0-a" ] \
	&& pass "current -> v1.0.0-a" || fail "current symlink wrong: $(readlink "$PREFIX/current" 2>&1)"
[ -x "$PREFIX/versions/v1.0.0-a/macvz-cri" ] && [ -x "$PREFIX/versions/v1.0.0-a/linuxpod-helper" ] \
	&& pass "pair binaries installed" || fail "pair binaries missing"
[ -f "$ADAPTER_PLIST" ] && [ -f "$HELPER_PLIST" ] \
	&& pass "both plists rendered" || fail "plist(s) missing"
grep -q -- '--experimental-linuxpod-backend' "$ADAPTER_PLIST" \
	&& pass "adapter plist has linuxpod backend flag" || fail "linuxpod backend flag missing from plist"
grep -q "$PREFIX/current/macvz-cri" "$ADAPTER_PLIST" \
	&& pass "adapter plist runs through current symlink" || fail "adapter plist not via current symlink"
grep -q -- '--pod-network-ingress-interface' "$ADAPTER_PLIST" \
	&& pass "repeatable pod-network flags rendered" || fail "ingress interface missing from plist"
grep -q -- '--vmnet' "$HELPER_PLIST" \
	&& pass "helper plist has vmnet flag" || fail "vmnet flag missing from helper plist"
[ -r "$PREFIX/install-args.env" ] \
	&& pass "wiring persisted (auditable)" || fail "install-args.env missing"

step "install is idempotent for the same version"
"$INSTALLER" install --from "$PAYLOAD_A" --kernel "$KERNEL" >/dev/null || fail "re-install exited non-zero"
[ ! -e "$PREFIX/previous" ] \
	&& pass "same-version reinstall records no previous" || fail "same-version reinstall wrote previous"

step "status renders auditable arguments"
STATUS_OUT="$("$INSTALLER" status)"
printf '%s\n' "$STATUS_OUT" | grep -q -- '--linuxpod-helper-socket' \
	&& pass "status prints adapter arguments" || fail "status does not print adapter args"
printf '%s\n' "$STATUS_OUT" | grep -q 'current pair:  v1.0.0-a' \
	&& pass "status shows current pair" || fail "status current pair wrong"

step "upgrade to v1.0.0-b"
"$INSTALLER" upgrade --from "$PAYLOAD_B" >/dev/null || fail "upgrade exited non-zero"
[ "$(readlink "$PREFIX/current")" = "$PREFIX/versions/v1.0.0-b" ] \
	&& pass "current -> v1.0.0-b" || fail "upgrade did not flip current"
[ "$(cat "$PREFIX/previous" 2>/dev/null)" = "v1.0.0-a" ] \
	&& pass "previous recorded as v1.0.0-a" || fail "previous not recorded"
grep -q -- '--experimental-linuxpod-backend' "$ADAPTER_PLIST" \
	&& pass "upgrade re-rendered plist with persisted wiring" || fail "upgrade lost wiring"

step "rollback to v1.0.0-a"
"$INSTALLER" rollback >/dev/null || fail "rollback exited non-zero"
[ "$(readlink "$PREFIX/current")" = "$PREFIX/versions/v1.0.0-a" ] \
	&& pass "rollback flipped current to v1.0.0-a" || fail "rollback did not flip current"
[ "$(cat "$PREFIX/previous" 2>/dev/null)" = "v1.0.0-b" ] \
	&& pass "previous now v1.0.0-b (rollback is reversible)" || fail "previous not updated on rollback"

step "clean: dry-run audits, --force deletes"
# Fabricate stale runtime state the way a crashed node leaves it.
mkdir -p "$PREFIX/helper-work/sup-deadbeef" "$PREFIX/state"
printf 'x' >"$PREFIX/helper-work/sup-deadbeef/holder.ext4"
printf '{}' >"$PREFIX/helper-work/supervisor-journal.json"
printf '{}' >"$PREFIX/state/sandbox-1.json"
touch "$PREFIX/macvz-cri.sock" "$PREFIX/linuxpod-helper.sock"
CLEAN_OUT="$("$INSTALLER" clean)"
printf '%s\n' "$CLEAN_OUT" | grep -q 'sup-deadbeef' \
	&& pass "clean dry-run lists sup-* residue" || fail "clean dry-run missed sup-* residue"
[ -f "$PREFIX/helper-work/supervisor-journal.json" ] \
	&& pass "clean dry-run deletes nothing" || fail "clean dry-run deleted state"
"$INSTALLER" clean --force >/dev/null || fail "clean --force exited non-zero"
[ ! -e "$PREFIX/helper-work/sup-deadbeef" ] && [ ! -e "$PREFIX/helper-work/supervisor-journal.json" ] \
	&& pass "clean --force removed residue" || fail "clean --force left residue"
grep -q 'sup-deadbeef' "$PREFIX/log/clean.log" 2>/dev/null \
	&& pass "clean --force audited to clean.log" || fail "clean.log missing audit entry"

step "uninstall --purge leaves nothing"
"$INSTALLER" uninstall --purge >/dev/null || fail "uninstall exited non-zero"
[ ! -f "$ADAPTER_PLIST" ] && [ ! -f "$HELPER_PLIST" ] \
	&& pass "plists removed" || fail "plists survived uninstall"
[ ! -e "$PREFIX/versions" ] && [ ! -e "$PREFIX/current" ] && [ ! -e "$PREFIX/install-args.env" ] \
	&& pass "purge removed versions/current/wiring" || fail "purge left files under $PREFIX"

printf '\n'
if [ "$FAILURES" -gt 0 ]; then
	printf 'REHEARSAL FAILED: %d assertion(s)\n' "$FAILURES"
	exit 1
fi
printf 'REHEARSAL PASSED\n'
