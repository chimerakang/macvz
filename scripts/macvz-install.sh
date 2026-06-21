#!/usr/bin/env bash
#
# macvz-install.sh — install / upgrade / rollback / uninstall MacVz as a managed
# macOS system component (issue #70).
#
# MacVz has two long-running pieces with different privilege needs:
#   - macvz-netd  : the privileged network helper, a root LaunchDaemon (#38/#40).
#   - macvz-kubelet: the Virtual Kubelet provider, which MUST run as the operator
#                    (apple/container is a per-user service and refuses root), so
#                    it is a per-user LaunchAgent.
# This script packages both, plus the shared config, behind one lifecycle so a
# node can be installed, upgraded in place, rolled back to the previous version,
# and cleanly removed.
#
# Versioned layout enables rollback: binaries live under
#   <prefix>/libexec/macvz/versions/<version>/{macvz-kubelet,macvz-netd}
# and a `current` symlink selects the active one. Upgrade flips the symlink and
# records the prior version; rollback flips it back. Config and PKI live outside
# the versioned tree (<etc>/config.yaml, <etc>/pki) so they survive upgrades and
# are removed only by `uninstall --purge`.
#
# Usage:
#   sudo ./macvz-install.sh install   --from DIR --version V [--config FILE]
#   sudo ./macvz-install.sh upgrade   --from DIR --version V
#   sudo ./macvz-install.sh rollback
#   sudo ./macvz-install.sh uninstall [--purge]
#   ./macvz-install.sh status
#
#   --from DIR   directory holding the built macvz-kubelet + macvz-netd (and,
#                optionally, config.example.yaml). Defaults to the script's dir.
#   --version V  version label for this payload (default: `macvz-kubelet --version`).
#   --config F   seed <etc>/config.yaml from F on a fresh install (never on upgrade;
#                an existing config is always preserved).
#   --purge      (uninstall) also delete <etc> config/PKI/state, not just services.
#
# Environment (also used by the test rehearsal):
#   MACVZ_PREFIX   install root for libexec/bin/var (default: /usr/local).
#   MACVZ_ETC      config/PKI/state root (default: /etc/macvz).
#   MACVZ_SOCKET   helper unix socket (default: /var/run/macvz-netd.sock).
#   MACVZ_USER     operator the kubelet LaunchAgent runs as (default: $SUDO_USER
#                  or the invoking user). The agent is installed in that user's
#                  ~/Library/LaunchAgents and bootstrapped into their gui domain.
#   MACVZ_HOME_DIR home directory for MACVZ_USER (default: resolved via ~user).
#                  The rehearsal sets this to a temp directory.
#   LAUNCHCTL      launchctl binary (default: launchctl; set to ":" to stub).
#   NETD           macvz-netd command for helper service ops (default: the
#                  installed current binary; set to ":" to stub).
#   MACVZ_DRY_RUN  if 1, print mutating actions instead of running them.
#
# Root is required when targeting the default /usr/local prefix; a non-default
# MACVZ_PREFIX (used by the rehearsal) drops that requirement so the version
# layout/rollback logic can be exercised without sudo.
set -euo pipefail

PREFIX="${MACVZ_PREFIX:-/usr/local}"
ETC="${MACVZ_ETC:-/etc/macvz}"
SOCKET="${MACVZ_SOCKET:-/var/run/macvz-netd.sock}"
LAUNCHCTL="${LAUNCHCTL:-launchctl}"
DRY_RUN="${MACVZ_DRY_RUN:-0}"

KUBELET_LABEL="com.github.chimerakang.macvz-kubelet"
HELPER_BIN="macvz-netd"
KUBELET_BIN="macvz-kubelet"

MACVZ_HOME="$PREFIX/libexec/macvz"
VERSIONS_DIR="$MACVZ_HOME/versions"
CURRENT_LINK="$MACVZ_HOME/current"
PREVIOUS_FILE="$MACVZ_HOME/previous"
BIN_LINK="$PREFIX/bin/$KUBELET_BIN"
LOG_DIR="$PREFIX/var/log"
CONFIG_FILE="$ETC/config.yaml"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

c_off=""; c_blue=""; c_green=""; c_red=""; c_yellow=""
if [ -t 1 ]; then
	c_off=$'\033[0m'; c_blue=$'\033[1;34m'; c_green=$'\033[32m'; c_red=$'\033[31m'; c_yellow=$'\033[33m'
fi
log()  { printf "%s==>%s %s\n" "$c_blue" "$c_off" "$*"; }
ok()   { printf "%sOK%s   %s\n" "$c_green" "$c_off" "$*"; }
warn() { printf "%sWARN%s %s\n" "$c_yellow" "$c_off" "$*"; }
die()  { printf "%sERROR%s %s\n" "$c_red" "$c_off" "$*" >&2; exit 1; }

# run executes a mutating command, honouring MACVZ_DRY_RUN.
run() {
	if [ "$DRY_RUN" = 1 ]; then
		printf "   + %s\n" "$*"
		return 0
	fi
	"$@"
}

# operator_user / operator_uid resolve the user the kubelet LaunchAgent runs as.
operator_user() { printf "%s" "${MACVZ_USER:-${SUDO_USER:-$(id -un)}}"; }
operator_uid()  { id -u "$(operator_user)" 2>/dev/null || echo ""; }
operator_gid()  { id -g "$(operator_user)" 2>/dev/null || echo ""; }
operator_home() {
	if [ -n "${MACVZ_HOME_DIR:-}" ]; then
		printf "%s" "$MACVZ_HOME_DIR"
		return
	fi
	local home
	home="$(eval echo "~$(operator_user)")"
	if [ "$home" = "~$(operator_user)" ]; then
		return 1
	fi
	printf "%s" "$home"
}
agent_dir() {
	local home
	home="$(operator_home)" || return 1
	printf "%s/Library/LaunchAgents" "$home"
}
agent_plist() { printf "%s/%s.plist" "$(agent_dir)" "$KUBELET_LABEL"; }

# netd_cmd is the macvz-netd command used for helper service ops. Default to the
# installed current binary; NETD overrides it (":" stubs it for the rehearsal).
netd_cmd() {
	if [ -n "${NETD:-}" ]; then printf "%s" "$NETD"; return; fi
	printf "%s/%s" "$CURRENT_LINK" "$HELPER_BIN"
}

require_root() {
	[ "$PREFIX" != "/usr/local" ] && return 0   # non-default prefix: rehearsal/dev
	[ "$(id -u)" -eq 0 ] || die "this command must run as root: sudo $0 $ACTION"
}

# resolved_version prints the version basename the `current` symlink points at.
resolved_version() {
	[ -L "$CURRENT_LINK" ] || return 1
	basename "$(readlink "$CURRENT_LINK")"
}

# --- argument parsing --------------------------------------------------------
ACTION="${1:-}"
shift || true
FROM_DIR="$HERE"
VERSION=""
SEED_CONFIG=""
PURGE=0
while [ $# -gt 0 ]; do
	case "$1" in
		--from)    FROM_DIR="${2:?--from needs a dir}"; shift 2 ;;
		--version) VERSION="${2:?--version needs a value}"; shift 2 ;;
		--config)  SEED_CONFIG="${2:?--config needs a file}"; shift 2 ;;
		--purge)   PURGE=1; shift ;;
		-h|--help) ACTION="help"; shift ;;
		*) die "unknown argument: $1" ;;
	esac
done

# stage_version copies the payload binaries into versions/<version> and returns
# (via $VERSION) the resolved version label.
stage_version() {
	local kubelet="$FROM_DIR/$KUBELET_BIN" helper="$FROM_DIR/$HELPER_BIN"
	[ -f "$kubelet" ] || die "missing $KUBELET_BIN in payload dir: $FROM_DIR"
	[ -f "$helper" ]  || die "missing $HELPER_BIN in payload dir: $FROM_DIR"
	if [ -z "$VERSION" ]; then
		VERSION="$("$kubelet" --version 2>/dev/null | awk '{print $2}')"
		[ -n "$VERSION" ] || die "could not determine version; pass --version"
	fi
	local dest="$VERSIONS_DIR/$VERSION"
	log "Staging $KUBELET_BIN + $HELPER_BIN $VERSION → $dest"
	run mkdir -p "$dest" "$LOG_DIR"
	run cp "$kubelet" "$dest/$KUBELET_BIN"
	run cp "$helper" "$dest/$HELPER_BIN"
	run chmod 0755 "$dest/$KUBELET_BIN" "$dest/$HELPER_BIN"
}

# point_current repoints current→versions/<version>, recording the prior version
# in PREVIOUS_FILE so rollback can find it. $1 is the version to activate.
point_current() {
	local target="$1" prior=""
	prior="$(resolved_version || true)"
	if [ -n "$prior" ] && [ "$prior" != "$target" ]; then
		run sh -c "printf '%s\n' '$prior' > '$PREVIOUS_FILE'"
	fi
	run ln -sfn "$VERSIONS_DIR/$target" "$CURRENT_LINK"
	run mkdir -p "$(dirname "$BIN_LINK")"
	run ln -sfn "$CURRENT_LINK/$KUBELET_BIN" "$BIN_LINK"
	ok "current → $target${prior:+ (previous: $prior)}"
}

# seed_config installs <etc>/config.yaml on a fresh install only; an existing
# config is always preserved (upgrade/rollback never touch it).
seed_config() {
	run mkdir -p "$ETC" "$ETC/pki"
	if [ -f "$CONFIG_FILE" ]; then
		ok "config preserved: $CONFIG_FILE"
		return
	fi
	local src="$SEED_CONFIG"
	[ -z "$src" ] && [ -f "$FROM_DIR/config.example.yaml" ] && src="$FROM_DIR/config.example.yaml"
	if [ -n "$src" ] && [ -f "$src" ]; then
		run cp "$src" "$CONFIG_FILE"
		warn "seeded $CONFIG_FILE from $(basename "$src") — edit it (nodeName, kubeconfig, mesh) before the node joins"
	else
		warn "no config at $CONFIG_FILE and no template provided; create it with: macvz-kubelet bootstrap ... --out $CONFIG_FILE"
	fi
}

# --- kubelet LaunchAgent ------------------------------------------------------
render_kubelet_plist() {
	cat <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$KUBELET_LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BIN_LINK</string>
    <string>--config</string>
    <string>$CONFIG_FILE</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key>
  <dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>$LOG_DIR/macvz-kubelet.log</string>
  <key>StandardErrorPath</key><string>$LOG_DIR/macvz-kubelet.err.log</string>
</dict>
</plist>
PLIST
}

install_kubelet_agent() {
	local dir plist uid gid out_log err_log
	uid="$(operator_uid)"
	gid="$(operator_gid)"
	[ -n "$uid" ] || die "operator user $(operator_user) does not exist; set MACVZ_USER to the user that runs apple/container"
	[ -n "$gid" ] || die "operator group for $(operator_user) does not exist"
	dir="$(agent_dir)"; plist="$(agent_plist)"
	out_log="$LOG_DIR/macvz-kubelet.log"
	err_log="$LOG_DIR/macvz-kubelet.err.log"
	log "Installing kubelet LaunchAgent for user $(operator_user) → $plist"
	run mkdir -p "$dir" "$LOG_DIR"
	if [ "$DRY_RUN" = 1 ]; then
		printf "   + write %s\n" "$plist"
	else
		render_kubelet_plist > "$plist"
	fi
	# The installer normally runs under sudo, but the kubelet runs as the
	# operator. Give that user ownership of its LaunchAgent and pre-created log
	# files so launchd can start it and open stdout/stderr without root-owned-file
	# surprises.
	run touch "$out_log" "$err_log"
	run chown "$uid:$gid" "$dir" "$plist" "$out_log" "$err_log"
	run chmod 0644 "$plist" "$out_log" "$err_log"
	# Reload: boot out an existing instance, then bootstrap the new plist into the
	# operator's gui domain so it runs in their session (where apple/container is).
	run "$LAUNCHCTL" bootout "gui/$uid/$KUBELET_LABEL" 2>/dev/null || true
	run "$LAUNCHCTL" bootstrap "gui/$uid" "$plist" || warn "launchctl bootstrap failed; start it in the operator's session"
	ok "kubelet LaunchAgent loaded (gui/$uid)"
}

uninstall_kubelet_agent() {
	local plist uid; uid="$(operator_uid)"
	if [ -z "$uid" ]; then
		warn "could not resolve uid for $(operator_user); skipping kubelet LaunchAgent bootout"
	else
		run "$LAUNCHCTL" bootout "gui/$uid/$KUBELET_LABEL" 2>/dev/null || true
	fi
	if plist="$(agent_plist 2>/dev/null)"; then
		[ -e "$plist" ] && run rm -f "$plist"
	else
		warn "could not resolve home for $(operator_user); skipping kubelet LaunchAgent plist removal"
	fi
	ok "kubelet LaunchAgent removed"
}

# --- helper LaunchDaemon (delegated to macvz-netd) ---------------------------
install_helper() {
	log "Installing $HELPER_BIN LaunchDaemon (socket $SOCKET, config $CONFIG_FILE)"
	if [ -f "$CONFIG_FILE" ]; then
		run "$(netd_cmd)" install --socket "$SOCKET" --config "$CONFIG_FILE" \
			|| die "$HELPER_BIN install failed"
	else
		warn "no config yet; installing helper without per-request policy (dev only)"
		run "$(netd_cmd)" install --socket "$SOCKET" --allow-unsafe-no-config \
			|| die "$HELPER_BIN install failed"
	fi
	ok "$HELPER_BIN installed and started"
}

uninstall_helper() {
	run "$(netd_cmd)" uninstall 2>/dev/null || warn "$HELPER_BIN uninstall reported an error (it may not have been installed)"
	ok "$HELPER_BIN LaunchDaemon removed"
}

# --- subcommands -------------------------------------------------------------
do_install() {
	require_root
	stage_version
	point_current "$VERSION"
	seed_config
	install_helper
	install_kubelet_agent
	echo
	ok "MacVz $VERSION installed. Verify with: $0 status"
}

do_upgrade() {
	require_root
	[ -L "$CURRENT_LINK" ] || die "nothing installed at $MACVZ_HOME; run 'install' first"
	stage_version
	point_current "$VERSION"        # records the prior version for rollback
	# Config + PKI are untouched, so state is preserved across the upgrade.
	install_helper                  # reinstall from the new binary
	install_kubelet_agent           # reload the agent against the new current
	echo
	ok "Upgraded to $VERSION (config + state preserved). Rollback with: $0 rollback"
}

do_rollback() {
	require_root
	[ -f "$PREVIOUS_FILE" ] || die "no previous version recorded; nothing to roll back to"
	local prev cur; prev="$(cat "$PREVIOUS_FILE")"; cur="$(resolved_version || echo '?')"
	[ -d "$VERSIONS_DIR/$prev" ] || die "previous version $prev is no longer staged at $VERSIONS_DIR/$prev"
	log "Rolling back $cur → $prev"
	run ln -sfn "$VERSIONS_DIR/$prev" "$CURRENT_LINK"
	run ln -sfn "$CURRENT_LINK/$KUBELET_BIN" "$BIN_LINK"
	run sh -c "printf '%s\n' '$cur' > '$PREVIOUS_FILE'"   # swap, so rollback is reversible
	install_helper
	install_kubelet_agent
	echo
	ok "Rolled back to $prev (re-run 'rollback' to return to $cur)"
}

do_uninstall() {
	require_root
	log "Uninstalling MacVz services"
	uninstall_kubelet_agent
	uninstall_helper
	[ -L "$BIN_LINK" ] && run rm -f "$BIN_LINK"
	[ -d "$MACVZ_HOME" ] && run rm -rf "$MACVZ_HOME"
	if [ "$PURGE" = 1 ]; then
		[ -d "$ETC" ] && run rm -rf "$ETC"
		warn "purged config, PKI, and state at $ETC"
	else
		ok "left config + state at $ETC (use --purge to remove)"
	fi
	echo
	ok "MacVz uninstalled"
}

do_status() {
	local ver prev uid
	ver="$(resolved_version || echo '(not installed)')"
	prev="$( [ -f "$PREVIOUS_FILE" ] && cat "$PREVIOUS_FILE" || echo '(none)')"
	uid="$(operator_uid)"
	echo "MacVz install status"
	echo "  prefix:           $PREFIX"
	echo "  current version:  $ver"
	echo "  previous version: $prev"
	echo "  kubelet binary:   $BIN_LINK -> $(readlink "$BIN_LINK" 2>/dev/null || echo '(missing)')"
	echo "  config:           $CONFIG_FILE $( [ -f "$CONFIG_FILE" ] && echo '(present)' || echo '(absent)')"
	echo "  operator user:    $(operator_user) (uid ${uid:-?})"
	if [ -n "$uid" ] && [ "$LAUNCHCTL" != ":" ]; then
		if "$LAUNCHCTL" print "gui/$uid/$KUBELET_LABEL" >/dev/null 2>&1; then
			echo "  kubelet agent:    loaded"
		else
			echo "  kubelet agent:    not loaded"
		fi
	fi
	if [ "${NETD:-}" != ":" ] && [ -x "$CURRENT_LINK/$HELPER_BIN" ]; then
		echo "  --- macvz-netd ---"
		"$(netd_cmd)" status 2>/dev/null | sed 's/^/  /' || true
	fi
}

usage() { sed -n '2,60p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

case "$ACTION" in
	install)   do_install ;;
	upgrade)   do_upgrade ;;
	rollback)  do_rollback ;;
	uninstall) do_uninstall ;;
	status)    do_status ;;
	help|-h|--help|"") usage ;;
	*) die "unknown command: $ACTION (try: install|upgrade|rollback|uninstall|status)" ;;
esac
