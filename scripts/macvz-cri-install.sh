#!/usr/bin/env bash
#
# macvz-cri-install.sh — install / uninstall / status the experimental MacVz CRI
# adapter as a per-user macOS LaunchAgent (CRI-P8, issue #80).
#
# The CRI feasibility adapter (cmd/macvz-cri) is an isolated `develop`-track
# spike, NOT the shipped Virtual Kubelet path. It drives apple/container, which is
# a per-user service that refuses to run as root, so the adapter is a per-user
# LaunchAgent — never a root LaunchDaemon. This installer wires one node so a k3s
# kubelet can point at the adapter's CRI socket, and tears it down cleanly so no
# stale socket, state, or launchd job survives an uninstall.
#
# Usage:
#   ./macvz-cri-install.sh install   --from DIR [--socket PATH] [--state-dir DIR]
#   ./macvz-cri-install.sh uninstall [--purge]
#   ./macvz-cri-install.sh status
#   ./macvz-cri-install.sh preflight
#
#   --from DIR    directory holding the built macvz-cri binary (default: ./bin).
#   --socket P    CRI unix socket the adapter serves (default: $MACVZ_CRI_SOCKET
#                 or ~/.macvz/cri/macvz-cri.sock).
#   --state-dir D restart-tolerant sandbox/container state dir
#                 (default: ~/.macvz/cri/state).
#   --purge       (uninstall) also delete the state dir, not just the service.
#
# Environment:
#   MACVZ_CRI_PREFIX   per-user install root (default: ~/.macvz/cri).
#   MACVZ_CRI_SOCKET   CRI socket path (overridden by --socket).
#   MACVZ_CRI_LABEL    LaunchAgent label (default: io.macvz.cri).
#   MACVZ_CRI_EXTRA    extra flags appended to the adapter command (e.g. Pod
#                      networking: "--pod-cidr 10.42.0.0/24 --pod-network-interface bridge100").
#                      Values are split on whitespace.
#   MACVZ_CRI_EXTRA_ARGS_FILE
#                      optional file with one extra adapter argument per line;
#                      use this when an argument contains spaces.
#   LAUNCHCTL          launchctl binary (default: launchctl; set ":" to stub in tests).
#   MACVZ_DRY_RUN      if 1, print mutating actions instead of running them.
#
# This installer intentionally does NOT configure k3s/kubelet itself; see
# test/e2e/cri-k3s/run.sh and docs/CRI_FEASIBILITY.md (CRI-P8) for the kubelet
# wiring and the compatibility suite.
set -uo pipefail

PREFIX="${MACVZ_CRI_PREFIX:-$HOME/.macvz/cri}"
LABEL="${MACVZ_CRI_LABEL:-io.macvz.cri}"
LAUNCHCTL="${LAUNCHCTL:-launchctl}"
DRY_RUN="${MACVZ_DRY_RUN:-0}"

FROM_DIR="./bin"
SOCKET="${MACVZ_CRI_SOCKET:-$PREFIX/macvz-cri.sock}"
STATE_DIR="$PREFIX/state"
PURGE=0

AGENT_DIR="$HOME/Library/LaunchAgents"
PLIST="$AGENT_DIR/$LABEL.plist"
BIN_DST="$PREFIX/macvz-cri"
LOG_DIR="$PREFIX/log"
EXTRA_ARGS=()

c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
ok()   { printf "${c_green}OK${c_off}   %s\n" "$*"; }
warn() { printf "${c_yellow}WARN${c_off} %s\n" "$*"; }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

# run executes a mutating command, honoring MACVZ_DRY_RUN.
run() {
	if [ "$DRY_RUN" = 1 ]; then
		printf '   [dry-run] %s\n' "$*"
		return 0
	fi
	"$@"
}

usage() {
	sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

uid() { id -u; }

# gui_domain is the launchctl domain target for the current user's GUI session.
gui_domain() { echo "gui/$(uid)"; }

parse_extra_args() {
	EXTRA_ARGS=()
	if [ -n "${MACVZ_CRI_EXTRA_ARGS_FILE:-}" ]; then
		[ -r "$MACVZ_CRI_EXTRA_ARGS_FILE" ] || die "MACVZ_CRI_EXTRA_ARGS_FILE is not readable: $MACVZ_CRI_EXTRA_ARGS_FILE"
		local line
		while IFS= read -r line || [ -n "$line" ]; do
			[ -z "$line" ] && continue
			EXTRA_ARGS+=("$line")
		done <"$MACVZ_CRI_EXTRA_ARGS_FILE"
	elif [ -n "${MACVZ_CRI_EXTRA:-}" ]; then
		read -r -a EXTRA_ARGS <<<"$MACVZ_CRI_EXTRA"
	fi
}

xml_escape() {
	local s="$1"
	s="${s//&/&amp;}"
	s="${s//</&lt;}"
	s="${s//>/&gt;}"
	s="${s//\"/&quot;}"
	s="${s//\'/&apos;}"
	printf '%s' "$s"
}

plist_string() {
	printf '    <string>%s</string>\n' "$(xml_escape "$1")"
}

plist_key_string() {
	printf '  <key>%s</key><string>%s</string>\n' "$(xml_escape "$1")" "$(xml_escape "$2")"
}

parse_common_flags() {
	while [ "$#" -gt 0 ]; do
		case "$1" in
			--from) FROM_DIR="$2"; shift 2 ;;
			--socket) SOCKET="$2"; shift 2 ;;
			--state-dir) STATE_DIR="$2"; shift 2 ;;
			--purge) PURGE=1; shift ;;
			-h|--help) usage 0 ;;
			*) die "unknown flag: $1 (see --help)" ;;
		esac
	done
}

write_plist() {
	# Build the ProgramArguments array. KeepAlive restarts the adapter if it
	# crashes — restart tolerance is a CRI-P8 acceptance item, and the adapter's
	# restart-recovery (RecoverContainers/RecoverNetwork) makes a relaunch safe.
	local tmp
	tmp="$(mktemp)"
	{
		cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
EOF
			plist_key_string "Label" "$LABEL"
			cat <<EOF
  <key>ProgramArguments</key>
  <array>
EOF
			plist_string "$BIN_DST"
			plist_string "--listen"
			plist_string "unix://$SOCKET"
			plist_string "--state-dir"
			plist_string "$STATE_DIR"
			# Append any extra adapter flags (e.g. Pod networking), one <string> each.
			for a in "${EXTRA_ARGS[@]}"; do
				plist_string "$a"
			done
			cat <<EOF
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
EOF
			plist_key_string "StandardOutPath" "$LOG_DIR/macvz-cri.out.log"
			plist_key_string "StandardErrorPath" "$LOG_DIR/macvz-cri.err.log"
			plist_key_string "ProcessType" "Interactive"
			cat <<EOF
</dict>
</plist>
EOF
	} >"$tmp"

	if [ "$DRY_RUN" = 1 ]; then
		printf '   [dry-run] write LaunchAgent plist to %s:\n' "$PLIST"
		sed 's/^/      /' "$tmp"
		rm -f "$tmp"
		return 0
	fi
	run mkdir -p "$AGENT_DIR"
	mv "$tmp" "$PLIST"
	ok "wrote LaunchAgent $PLIST"
}

cmd_install() {
	parse_common_flags "$@"
	parse_extra_args
	local src="$FROM_DIR/macvz-cri"
	[ -x "$src" ] || die "macvz-cri binary not found or not executable at $src (run 'make cri' or pass --from)"

	log "Installing macvz-cri LaunchAgent ($LABEL) for user $(id -un)"
	run mkdir -p "$PREFIX" "$STATE_DIR" "$LOG_DIR" "$(dirname "$SOCKET")"
	run cp "$src" "$BIN_DST"
	run chmod 0755 "$BIN_DST"

	# Preflight before loading the job so a missing dependency is reported once,
	# clearly, instead of as a launchd crash-loop in the logs.
	if [ "$DRY_RUN" != 1 ]; then
		log "Preflight"
		"$BIN_DST" --preflight --listen "unix://$SOCKET" --state-dir "$STATE_DIR" "${EXTRA_ARGS[@]}" || \
			warn "preflight reported FAIL items above; the LaunchAgent will be installed but may not serve until they are resolved"
	fi

	write_plist

	# Reload idempotently: boot out any prior job before bootstrapping the new one.
	run "$LAUNCHCTL" bootout "$(gui_domain)/$LABEL" 2>/dev/null || true
	run "$LAUNCHCTL" bootstrap "$(gui_domain)" "$PLIST" || \
		warn "launchctl bootstrap failed; check $LOG_DIR for details"
	ok "installed; CRI endpoint: unix://$SOCKET"
	log "Point kubelet/k3s at it with --container-runtime-endpoint unix://$SOCKET"
}

cmd_uninstall() {
	parse_common_flags "$@"
	log "Uninstalling macvz-cri LaunchAgent ($LABEL)"
	run "$LAUNCHCTL" bootout "$(gui_domain)/$LABEL" 2>/dev/null || true
	[ -f "$PLIST" ] && run rm -f "$PLIST"

	# Leave no stale socket behind — a dangling socket file would make a fresh
	# install or a crictl probe believe an endpoint exists.
	[ -S "$SOCKET" ] && run rm -f "$SOCKET"
	[ -f "$BIN_DST" ] && run rm -f "$BIN_DST"

	if [ "$PURGE" = 1 ]; then
		warn "purging state dir $STATE_DIR"
		run rm -rf "$STATE_DIR"
		run rm -rf "$LOG_DIR"
	else
		log "state dir preserved at $STATE_DIR (use --purge to remove)"
	fi
	ok "uninstalled"
}

cmd_status() {
	parse_common_flags "$@"
	log "macvz-cri status"
	printf '  label:     %s\n' "$LABEL"
	printf '  binary:    %s%s\n' "$BIN_DST" "$([ -x "$BIN_DST" ] && echo ' (present)' || echo ' (absent)')"
	printf '  plist:     %s%s\n' "$PLIST" "$([ -f "$PLIST" ] && echo ' (present)' || echo ' (absent)')"
	printf '  socket:    %s%s\n' "$SOCKET" "$([ -S "$SOCKET" ] && echo ' (present)' || echo ' (absent)')"
	printf '  state dir: %s%s\n' "$STATE_DIR" "$([ -d "$STATE_DIR" ] && echo ' (present)' || echo ' (absent)')"
	if "$LAUNCHCTL" print "$(gui_domain)/$LABEL" >/dev/null 2>&1; then
		ok "LaunchAgent is loaded"
	else
		warn "LaunchAgent is not loaded"
	fi
}

cmd_preflight() {
	parse_common_flags "$@"
	parse_extra_args
	local bin="$BIN_DST"
	[ -x "$bin" ] || bin="$FROM_DIR/macvz-cri"
	[ -x "$bin" ] || die "macvz-cri binary not found (install first or run 'make cri')"
	exec "$bin" --preflight --listen "unix://$SOCKET" --state-dir "$STATE_DIR" "${EXTRA_ARGS[@]}"
}

main() {
	[ "$#" -ge 1 ] || usage 1
	local sub="$1"; shift
	case "$sub" in
		install)   cmd_install "$@" ;;
		uninstall) cmd_uninstall "$@" ;;
		status)    cmd_status "$@" ;;
		preflight) cmd_preflight "$@" ;;
		-h|--help|help) usage 0 ;;
		*) die "unknown subcommand: $sub (install|uninstall|status|preflight)" ;;
	esac
}

main "$@"
