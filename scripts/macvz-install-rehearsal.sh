#!/usr/bin/env bash
#
# macvz-install-rehearsal.sh — exercise the macvz-install.sh lifecycle (install →
# upgrade → rollback → uninstall) in a throwaway temp prefix, with launchctl and
# macvz-netd stubbed, so the versioned-layout and rollback logic can be verified
# without root or a real Mac (issue #70).
#
# It builds fake binaries that respond to `--version` and `status`/`install`/
# `uninstall`, runs each lifecycle step against MACVZ_PREFIX/MACVZ_ETC in a temp
# dir, and asserts the `current`/`previous` symlinks and config preservation
# behave as documented. A live install/upgrade/rollback/uninstall on a real Mac
# is still required for service-level acceptance (see docs/PACKAGING.md).
#
# Usage: ./macvz-install-rehearsal.sh   (no args, exits non-zero on any failure)
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="$HERE/macvz-install.sh"

ROOT="$(mktemp -d)"
trap 'rm -rf "$ROOT"' EXIT

PREFIX="$ROOT/prefix"
ETC="$ROOT/etc"
PAYLOAD_A="$ROOT/payload-a"
PAYLOAD_B="$ROOT/payload-b"
HOME_DIR="$ROOT/home"
mkdir -p "$PAYLOAD_A" "$PAYLOAD_B" "$HOME_DIR/Library/LaunchAgents"

FAILURES=0
pass() { printf "PASS %s\n" "$*"; }
fail() { printf "FAIL %s\n" "$*"; FAILURES=$((FAILURES+1)); }

# make_payload DIR VERSION — write fake macvz-kubelet/macvz-netd that satisfy the
# installer's `--version`, plus a config template for the kubelet.
make_payload() {
	local dir="$1" ver="$2"
	cat >"$dir/macvz-kubelet" <<EOF
#!/bin/sh
[ "\$1" = "--version" ] && echo "macvz-kubelet $ver (commit x, built y, darwin/arm64, go)" && exit 0
exit 0
EOF
	cat >"$dir/macvz-netd" <<'EOF'
#!/bin/sh
# Stub helper: accept install/uninstall/status without doing anything real.
echo "macvz-netd stub: $*" >&2
exit 0
EOF
	chmod +x "$dir/macvz-kubelet" "$dir/macvz-netd"
	printf 'nodeName: rehearsal\n' >"$dir/config.example.yaml"
}

make_payload "$PAYLOAD_A" "1.0.0"
make_payload "$PAYLOAD_B" "1.1.0"

# Run the installer with system-mutating side effects neutralised: launchctl and
# the helper command are stubbed (":"), and a fake HOME hosts the LaunchAgent.
inst() {
	HOME="$HOME_DIR" \
	MACVZ_PREFIX="$PREFIX" MACVZ_ETC="$ETC" \
	MACVZ_HOME_DIR="$HOME_DIR" \
	MACVZ_USER="$(id -un)" \
	LAUNCHCTL=":" NETD=":" \
	bash "$INSTALLER" "$@"
}

current_version() { basename "$(readlink "$PREFIX/libexec/macvz/current" 2>/dev/null)" 2>/dev/null; }
previous_version() { cat "$PREFIX/libexec/macvz/previous" 2>/dev/null || echo ""; }
check() {
	local msg="$1"
	shift
	if "$@"; then
		pass "$msg"
	else
		fail "$msg"
	fi
}
check_eq() {
	local msg="$1" got="$2" want="$3"
	if [ "$got" = "$want" ]; then
		pass "$msg"
	else
		fail "$msg: got ${got:-<empty>}, want $want"
	fi
}

echo "### install 1.0.0"
inst install --from "$PAYLOAD_A" >/dev/null || fail "install exited non-zero"
check_eq "current is 1.0.0 after install" "$(current_version)" "1.0.0"
check "bin symlink created" test -L "$PREFIX/bin/macvz-kubelet"
check "config seeded from template" test -f "$ETC/config.yaml"

echo "### edit config, then upgrade to 1.1.0"
printf 'nodeName: edited-by-operator\n' >"$ETC/config.yaml"
inst upgrade --from "$PAYLOAD_B" >/dev/null || fail "upgrade exited non-zero"
check_eq "current is 1.1.0 after upgrade" "$(current_version)" "1.1.0"
check_eq "previous recorded as 1.0.0" "$(previous_version)" "1.0.0"
check "config preserved across upgrade" grep -q "edited-by-operator" "$ETC/config.yaml"

echo "### rollback to 1.0.0"
inst rollback >/dev/null || fail "rollback exited non-zero"
check_eq "current is 1.0.0 after rollback" "$(current_version)" "1.0.0"
check_eq "rollback is reversible (previous=1.1.0)" "$(previous_version)" "1.1.0"
check "config preserved across rollback" grep -q "edited-by-operator" "$ETC/config.yaml"

echo "### roll forward again (reversible)"
inst rollback >/dev/null || fail "second rollback exited non-zero"
check_eq "rolled forward to 1.1.0" "$(current_version)" "1.1.0"

echo "### status"
inst status >/dev/null || fail "status exited non-zero"

echo "### uninstall (keep config)"
inst uninstall >/dev/null || fail "uninstall exited non-zero"
check "versioned tree removed on uninstall" test ! -d "$PREFIX/libexec/macvz"
check "bin symlink removed on uninstall" test ! -L "$PREFIX/bin/macvz-kubelet"
check "config retained without --purge" test -f "$ETC/config.yaml"

echo "### reinstall + uninstall --purge"
inst install --from "$PAYLOAD_A" >/dev/null || fail "reinstall exited non-zero"
inst uninstall --purge >/dev/null || fail "purge uninstall exited non-zero"
check "config purged with --purge" test ! -d "$ETC"

echo
if [ "$FAILURES" -eq 0 ]; then
	echo "PASS macvz-install rehearsal: all checks passed"
	exit 0
fi
echo "FAIL macvz-install rehearsal: $FAILURES check(s) failed"
exit 1
