#!/usr/bin/env bash
#
# macvz-cri-linuxpod-install.sh — managed per-user service lifecycle for the
# LinuxPod-backed MacVz CRI node path (CRI-L9-1 #149, CRI-L9-4 #152).
#
# Installs macvz-cri (with --experimental-linuxpod-backend) and linuxpod-helper
# as a PAIRED, VERSIONED per-user LaunchAgent set, so protocol-coupled upgrades
# are applied and rolled back together. apple/container and Apple
# Containerization refuse to run as root, so both services are per-user
# LaunchAgents — never root LaunchDaemons. macvz-netd keeps its separate root
# LaunchDaemon model (see docs/PRIVILEGED_NETWORKING.md); this script never
# touches it, launchd startup order is: macvz-netd (system) -> linuxpod-helper
# -> macvz-cri -> kubelet/k3s.
#
# Layout (default prefix ~/.macvz/cri-linuxpod):
#   versions/<ver>/{macvz-cri,linuxpod-helper}   immutable installed pairs
#   current -> versions/<ver>                    active pair (symlink)
#   previous                                     prior version label (rollback)
#   install-args.env                             auditable service wiring
#   macvz-cri.sock linuxpod-helper.sock          service sockets
#   state/ helper-work/ log/                     adapter state, helper VMs, logs
#
# Usage:
#   macvz-cri-linuxpod-install.sh install  --from DIR [wiring flags...]
#   macvz-cri-linuxpod-install.sh upgrade  --from DIR
#   macvz-cri-linuxpod-install.sh rollback
#   macvz-cri-linuxpod-install.sh restart
#   macvz-cri-linuxpod-install.sh status
#   macvz-cri-linuxpod-install.sh preflight
#   macvz-cri-linuxpod-install.sh clean [--force]
#   macvz-cri-linuxpod-install.sh uninstall [--purge]
#
# Wiring flags (install only; persisted to install-args.env and reused by
# upgrade/rollback so the pair is always re-rendered with identical wiring):
#   --socket P                    CRI unix socket (default <prefix>/macvz-cri.sock)
#   --state-dir D                 adapter sandbox/container state (default <prefix>/state)
#   --helper-socket P             linuxpod-helper NDJSON socket (default <prefix>/linuxpod-helper.sock)
#   --helper-work-dir D           helper VM/journal work dir (default <prefix>/helper-work)
#   --kernel P                    Linux kernel image for LinuxPod VMs (required)
#   --containerization-root D     Apple Containerization support binaries dir
#   --initfs-reference R          initfs OCI reference (default vminit:latest)
#   --image R                     holder image reference (default docker.io/library/busybox:1.36.1)
#   --vmnet                       attach Pod VMs to vmnet
#   --kubelet-pods-dir D          kubelet pods dir for volume projection
#   --linuxpod-log-root D         log root override for rootless/remote topologies
#   --volume-host-path-allowed D  allowed hostPath root (repeatable)
#   --pod-cidr CIDR               node Pod CIDR (enables Pod networking with iface)
#   --pod-network-interface I     vmnet bridge (e.g. bridge100)
#   --pod-network-helper-socket P macvz-netd socket (default /var/run/macvz-netd.sock)
#   --pod-network-ingress-interface I  extra ingress iface (repeatable)
#   --pod-network-enable-forwarding    ask netd to enable ip forwarding
#   --streaming-addr A            exec/attach/port-forward streaming address
#   --purge                       (uninstall) also delete state/helper-work/logs/versions
#   --force                       (clean) actually delete; default is an audited dry-run
#
# Environment:
#   MACVZ_CRI_LP_PREFIX     install root (default ~/.macvz/cri-linuxpod)
#   MACVZ_CRI_LP_LABEL      adapter LaunchAgent label (default io.macvz.cri.linuxpod)
#   MACVZ_CRI_LP_HELPER_LABEL helper LaunchAgent label (default io.macvz.linuxpod-helper)
#   LAUNCHCTL               launchctl binary (":" to stub in tests)
#   MACVZ_DRY_RUN           if 1, print mutating actions instead of running them
#
# The route guard invariant from the e2e harnesses applies: nothing here reads
# or writes host routes; the host default route must be identical before and
# after every subcommand.
set -uo pipefail

PREFIX="${MACVZ_CRI_LP_PREFIX:-$HOME/.macvz/cri-linuxpod}"
LABEL="${MACVZ_CRI_LP_LABEL:-io.macvz.cri.linuxpod}"
HELPER_LABEL="${MACVZ_CRI_LP_HELPER_LABEL:-io.macvz.linuxpod-helper}"
LAUNCHCTL="${LAUNCHCTL:-launchctl}"
DRY_RUN="${MACVZ_DRY_RUN:-0}"

AGENT_DIR="$HOME/Library/LaunchAgents"
PLIST="$AGENT_DIR/$LABEL.plist"
HELPER_PLIST="$AGENT_DIR/$HELPER_LABEL.plist"
VERSIONS_DIR="$PREFIX/versions"
CURRENT_LINK="$PREFIX/current"
PREVIOUS_FILE="$PREFIX/previous"
ARGS_ENV="$PREFIX/install-args.env"
LOG_DIR="$PREFIX/log"

FROM_DIR="./bin"
PURGE=0
FORCE=0

# Wiring (defaults; overridden by flags on install, or restored from ARGS_ENV).
SOCKET="$PREFIX/macvz-cri.sock"
STATE_DIR="$PREFIX/state"
HELPER_SOCKET="$PREFIX/linuxpod-helper.sock"
HELPER_WORK_DIR="$PREFIX/helper-work"
KERNEL=""
CONTAINERIZATION_ROOT=""
INITFS_REFERENCE="vminit:latest"
HOLDER_IMAGE="docker.io/library/busybox:1.36.1"
VMNET=0
KUBELET_PODS_DIR=""
LINUXPOD_LOG_ROOT=""
VOLUME_ALLOWED=()
POD_CIDR=""
POD_NET_IFACE=""
POD_NET_HELPER_SOCKET="/var/run/macvz-netd.sock"
POD_NET_INGRESS=()
POD_NET_FORWARDING=0
STREAMING_ADDR=""

c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
ok()   { printf "${c_green}OK${c_off}   %s\n" "$*"; }
warn() { printf "${c_yellow}WARN${c_off} %s\n" "$*"; }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

run() {
	if [ "$DRY_RUN" = 1 ]; then
		printf '   [dry-run] %s\n' "$*"
		return 0
	fi
	"$@"
}

usage() {
	sed -n '2,72p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

uid() { id -u; }
gui_domain() { echo "gui/$(uid)"; }

xml_escape() {
	local s="$1"
	s="${s//&/&amp;}"
	s="${s//</&lt;}"
	s="${s//>/&gt;}"
	s="${s//\"/&quot;}"
	s="${s//\'/&apos;}"
	printf '%s' "$s"
}

plist_string() { printf '    <string>%s</string>\n' "$(xml_escape "$1")"; }
plist_key_string() { printf '  <key>%s</key><string>%s</string>\n' "$(xml_escape "$1")" "$(xml_escape "$2")"; }

parse_flags() {
	while [ "$#" -gt 0 ]; do
		case "$1" in
			--from) FROM_DIR="$2"; shift 2 ;;
			--socket) SOCKET="$2"; shift 2 ;;
			--state-dir) STATE_DIR="$2"; shift 2 ;;
			--helper-socket) HELPER_SOCKET="$2"; shift 2 ;;
			--helper-work-dir) HELPER_WORK_DIR="$2"; shift 2 ;;
			--kernel) KERNEL="$2"; shift 2 ;;
			--containerization-root) CONTAINERIZATION_ROOT="$2"; shift 2 ;;
			--initfs-reference) INITFS_REFERENCE="$2"; shift 2 ;;
			--image) HOLDER_IMAGE="$2"; shift 2 ;;
			--vmnet) VMNET=1; shift ;;
			--kubelet-pods-dir) KUBELET_PODS_DIR="$2"; shift 2 ;;
			--linuxpod-log-root) LINUXPOD_LOG_ROOT="$2"; shift 2 ;;
			--volume-host-path-allowed) VOLUME_ALLOWED+=("$2"); shift 2 ;;
			--pod-cidr) POD_CIDR="$2"; shift 2 ;;
			--pod-network-interface) POD_NET_IFACE="$2"; shift 2 ;;
			--pod-network-helper-socket) POD_NET_HELPER_SOCKET="$2"; shift 2 ;;
			--pod-network-ingress-interface) POD_NET_INGRESS+=("$2"); shift 2 ;;
			--pod-network-enable-forwarding) POD_NET_FORWARDING=1; shift ;;
			--streaming-addr) STREAMING_ADDR="$2"; shift 2 ;;
			--purge) PURGE=1; shift ;;
			--force) FORCE=1; shift ;;
			-h|--help) usage 0 ;;
			*) die "unknown flag: $1 (see --help)" ;;
		esac
	done
}

# save_args / load_args persist the wiring so upgrade/rollback/status re-render
# the exact same service arguments (auditable at $ARGS_ENV and in the plists).
save_args() {
	local tmp
	tmp="$(mktemp)"
	{
		printf '# macvz-cri-linuxpod wiring, written by install on %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		printf 'SOCKET=%q\n' "$SOCKET"
		printf 'STATE_DIR=%q\n' "$STATE_DIR"
		printf 'HELPER_SOCKET=%q\n' "$HELPER_SOCKET"
		printf 'HELPER_WORK_DIR=%q\n' "$HELPER_WORK_DIR"
		printf 'KERNEL=%q\n' "$KERNEL"
		printf 'CONTAINERIZATION_ROOT=%q\n' "$CONTAINERIZATION_ROOT"
		printf 'INITFS_REFERENCE=%q\n' "$INITFS_REFERENCE"
		printf 'HOLDER_IMAGE=%q\n' "$HOLDER_IMAGE"
		printf 'VMNET=%q\n' "$VMNET"
		printf 'KUBELET_PODS_DIR=%q\n' "$KUBELET_PODS_DIR"
		printf 'LINUXPOD_LOG_ROOT=%q\n' "$LINUXPOD_LOG_ROOT"
		printf 'VOLUME_ALLOWED=(%s)\n' "$(printf '%q ' "${VOLUME_ALLOWED[@]+"${VOLUME_ALLOWED[@]}"}")"
		printf 'POD_CIDR=%q\n' "$POD_CIDR"
		printf 'POD_NET_IFACE=%q\n' "$POD_NET_IFACE"
		printf 'POD_NET_HELPER_SOCKET=%q\n' "$POD_NET_HELPER_SOCKET"
		printf 'POD_NET_INGRESS=(%s)\n' "$(printf '%q ' "${POD_NET_INGRESS[@]+"${POD_NET_INGRESS[@]}"}")"
		printf 'POD_NET_FORWARDING=%q\n' "$POD_NET_FORWARDING"
		printf 'STREAMING_ADDR=%q\n' "$STREAMING_ADDR"
	} >"$tmp"
	if [ "$DRY_RUN" = 1 ]; then
		printf '   [dry-run] write wiring to %s\n' "$ARGS_ENV"
		rm -f "$tmp"
		return 0
	fi
	mv "$tmp" "$ARGS_ENV"
	ok "wiring persisted to $ARGS_ENV"
}

load_args() {
	[ -r "$ARGS_ENV" ] || die "no persisted wiring at $ARGS_ENV (run install first)"
	# shellcheck disable=SC1090
	. "$ARGS_ENV"
}

# adapter_args prints the full macvz-cri argv (one per line), the single source
# of truth shared by the plist renderer and preflight.
adapter_args() {
	printf '%s\n' \
		"--listen" "unix://$SOCKET" \
		"--state-dir" "$STATE_DIR" \
		"--experimental-linuxpod-backend" \
		"--linuxpod-helper-socket" "$HELPER_SOCKET"
	[ -n "$KUBELET_PODS_DIR" ] && printf '%s\n' "--kubelet-pods-dir" "$KUBELET_PODS_DIR"
	[ -n "$LINUXPOD_LOG_ROOT" ] && printf '%s\n' "--linuxpod-log-root" "$LINUXPOD_LOG_ROOT"
	local v
	for v in "${VOLUME_ALLOWED[@]+"${VOLUME_ALLOWED[@]}"}"; do
		printf '%s\n' "--volume-host-path-allowed" "$v"
	done
	if [ -n "$POD_CIDR" ] && [ -n "$POD_NET_IFACE" ]; then
		printf '%s\n' "--pod-cidr" "$POD_CIDR" "--pod-network-interface" "$POD_NET_IFACE" \
			"--pod-network-helper-socket" "$POD_NET_HELPER_SOCKET"
		for v in "${POD_NET_INGRESS[@]+"${POD_NET_INGRESS[@]}"}"; do
			printf '%s\n' "--pod-network-ingress-interface" "$v"
		done
		[ "$POD_NET_FORWARDING" = 1 ] && printf '%s\n' "--pod-network-enable-forwarding"
	fi
	[ -n "$STREAMING_ADDR" ] && printf '%s\n' "--streaming-addr" "$STREAMING_ADDR"
	return 0
}

helper_args() {
	printf '%s\n' \
		"--socket" "$HELPER_SOCKET" \
		"--work-dir" "$HELPER_WORK_DIR" \
		"--kernel" "$KERNEL" \
		"--initfs-reference" "$INITFS_REFERENCE" \
		"--image" "$HOLDER_IMAGE"
	[ -n "$CONTAINERIZATION_ROOT" ] && printf '%s\n' "--containerization-root" "$CONTAINERIZATION_ROOT"
	[ "$VMNET" = 1 ] && printf '%s\n' "--vmnet"
	return 0
}

# write_service_plist LABEL PLIST BINARY ARGS_FN OUT_LOG ERR_LOG
write_service_plist() {
	local label="$1" plist="$2" binary="$3" args_fn="$4" out_log="$5" err_log="$6"
	local tmp
	tmp="$(mktemp)"
	{
		cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
EOF
		plist_key_string "Label" "$label"
		cat <<EOF
  <key>ProgramArguments</key>
  <array>
EOF
		plist_string "$binary"
		local a
		while IFS= read -r a; do
			plist_string "$a"
		done < <("$args_fn")
		cat <<EOF
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
EOF
		plist_key_string "StandardOutPath" "$out_log"
		plist_key_string "StandardErrorPath" "$err_log"
		plist_key_string "ProcessType" "Interactive"
		cat <<EOF
</dict>
</plist>
EOF
	} >"$tmp"

	if [ "$DRY_RUN" = 1 ]; then
		printf '   [dry-run] write LaunchAgent plist to %s:\n' "$plist"
		sed 's/^/      /' "$tmp"
		rm -f "$tmp"
		return 0
	fi
	run mkdir -p "$AGENT_DIR"
	mv "$tmp" "$plist"
	ok "wrote LaunchAgent $plist"
}

# version_label extracts a version string from the adapter binary. version.String()
# prints "<name> <version> (...)" — field 2 is the version, matching macvz-install.sh.
version_label() {
	local bin="$1" v
	v="$("$bin" --version 2>/dev/null | awk 'NR==1{print $2}')"
	[ -n "$v" ] || die "could not read version from $bin --version"
	printf '%s' "$v"
}

current_version() {
	[ -L "$CURRENT_LINK" ] || return 1
	basename "$(readlink "$CURRENT_LINK")"
}

# check_helper_signature warns when linuxpod-helper lacks the virtualization
# entitlement — without it every CreatePod fails at VM boot.
check_helper_signature() {
	local bin="$1"
	if ! command -v codesign >/dev/null 2>&1; then
		return 0
	fi
	if ! codesign -d --entitlements - "$bin" 2>/dev/null | grep -q 'com.apple.security.virtualization'; then
		warn "linuxpod-helper at $bin lacks the com.apple.security.virtualization entitlement; sign it (see test/e2e/cri-linuxpod/linuxpod-helper.entitlements) or Pod VMs will fail to boot"
	fi
}

# install_pair copies the adapter+helper pair into versions/<ver> and flips the
# current symlink; the installed version label is returned in INSTALLED_VER (not
# stdout, which carries the human log).
INSTALLED_VER=""
install_pair() {
	local src_cri="$FROM_DIR/macvz-cri" src_helper="$FROM_DIR/linuxpod-helper"
	[ -x "$src_cri" ] || die "macvz-cri binary not found or not executable at $src_cri (run 'make cri' or pass --from)"
	[ -x "$src_helper" ] || die "linuxpod-helper binary not found or not executable at $src_helper (build test/e2e/cri-linuxpod and copy/sign it into --from)"

	local ver
	ver="$(version_label "$src_cri")"
	local dest="$VERSIONS_DIR/$ver"
	log "Installing pair version $ver -> $dest"
	run mkdir -p "$dest"
	run cp "$src_cri" "$dest/macvz-cri"
	run cp "$src_helper" "$dest/linuxpod-helper"
	run chmod 0755 "$dest/macvz-cri" "$dest/linuxpod-helper"
	check_helper_signature "$dest/linuxpod-helper"

	# Record the outgoing version for rollback, then flip the symlink.
	local cur
	if cur="$(current_version)" && [ "$cur" != "$ver" ]; then
		if [ "$DRY_RUN" = 1 ]; then
			printf '   [dry-run] record previous=%s\n' "$cur"
		else
			printf '%s\n' "$cur" >"$PREVIOUS_FILE"
		fi
		log "previous version recorded: $cur"
	fi
	run ln -sfn "$dest" "$CURRENT_LINK"
	INSTALLED_VER="$ver"
}

bootstrap_services() {
	# Helper first: the adapter handshakes the helper socket at startup and
	# fails fast on a mismatch/absence (KeepAlive retries until the helper is up).
	run "$LAUNCHCTL" bootout "$(gui_domain)/$HELPER_LABEL" 2>/dev/null || true
	run "$LAUNCHCTL" bootstrap "$(gui_domain)" "$HELPER_PLIST" || \
		warn "launchctl bootstrap($HELPER_LABEL) failed; check $LOG_DIR"
	run "$LAUNCHCTL" bootout "$(gui_domain)/$LABEL" 2>/dev/null || true
	run "$LAUNCHCTL" bootstrap "$(gui_domain)" "$PLIST" || \
		warn "launchctl bootstrap($LABEL) failed; check $LOG_DIR"
}

render_and_start() {
	write_service_plist "$HELPER_LABEL" "$HELPER_PLIST" "$CURRENT_LINK/linuxpod-helper" helper_args \
		"$LOG_DIR/linuxpod-helper.out.log" "$LOG_DIR/linuxpod-helper.err.log"
	write_service_plist "$LABEL" "$PLIST" "$CURRENT_LINK/macvz-cri" adapter_args \
		"$LOG_DIR/macvz-cri.out.log" "$LOG_DIR/macvz-cri.err.log"
	bootstrap_services
}

run_preflight() {
	local bin="$CURRENT_LINK/macvz-cri"
	[ -x "$bin" ] || return 0
	log "Preflight"
	local args=()
	while IFS= read -r a; do args+=("$a"); done < <(adapter_args)
	"$bin" --preflight "${args[@]}" || \
		warn "preflight reported FAIL items above; services are installed but may not serve until they are resolved (protocol mismatches fail loudly at startup — see $LOG_DIR/macvz-cri.err.log)"
}

cmd_install() {
	parse_flags "$@"
	[ -n "$KERNEL" ] || die "--kernel is required: the LinuxPod helper cannot boot Pod micro-VMs without a kernel image"
	log "Installing LinuxPod CRI node services ($HELPER_LABEL + $LABEL) for user $(id -un)"
	run mkdir -p "$PREFIX" "$VERSIONS_DIR" "$STATE_DIR" "$HELPER_WORK_DIR" "$LOG_DIR" \
		"$(dirname "$SOCKET")" "$(dirname "$HELPER_SOCKET")"
	install_pair
	save_args
	[ "$DRY_RUN" = 1 ] || run_preflight
	render_and_start
	ok "installed pair $INSTALLED_VER; CRI endpoint: unix://$SOCKET"
	log "Point the k3s agent at it: --container-runtime-endpoint unix://$SOCKET (see docs/CRI_NODE_OPERATIONS.md)"
}

cmd_upgrade() {
	parse_flags "$@"
	load_args
	current_version >/dev/null || die "nothing installed at $CURRENT_LINK (run install first)"
	install_pair
	[ "$DRY_RUN" = 1 ] || run_preflight
	render_and_start
	ok "upgraded to pair $INSTALLED_VER (rollback with: $0 rollback)"
}

cmd_rollback() {
	parse_flags "$@"
	load_args
	[ -r "$PREVIOUS_FILE" ] || die "no previous version recorded at $PREVIOUS_FILE"
	local prev cur
	prev="$(cat "$PREVIOUS_FILE")"
	[ -d "$VERSIONS_DIR/$prev" ] || die "previous version $prev is missing from $VERSIONS_DIR"
	cur="$(current_version)" || die "no current version to roll back from"
	log "Rolling back $cur -> $prev"
	run ln -sfn "$VERSIONS_DIR/$prev" "$CURRENT_LINK"
	if [ "$DRY_RUN" = 1 ]; then
		printf '   [dry-run] record previous=%s\n' "$cur"
	else
		printf '%s\n' "$cur" >"$PREVIOUS_FILE"
	fi
	render_and_start
	ok "rolled back to pair $prev (previous is now $cur)"
}

cmd_restart() {
	parse_flags "$@"
	load_args
	log "Restarting LinuxPod CRI node services"
	bootstrap_services
	ok "restarted"
}

cmd_status() {
	parse_flags "$@"
	log "LinuxPod CRI node status"
	local cur prev
	cur="$(current_version 2>/dev/null || echo '(none)')"
	prev="$([ -r "$PREVIOUS_FILE" ] && cat "$PREVIOUS_FILE" || echo '(none)')"
	printf '  current pair:  %s\n' "$cur"
	printf '  previous pair: %s\n' "$prev"
	printf '  adapter plist: %s%s\n' "$PLIST" "$([ -f "$PLIST" ] && echo ' (present)' || echo ' (absent)')"
	printf '  helper plist:  %s%s\n' "$HELPER_PLIST" "$([ -f "$HELPER_PLIST" ] && echo ' (present)' || echo ' (absent)')"
	if [ -r "$ARGS_ENV" ]; then
		load_args
		printf '  CRI socket:    %s%s\n' "$SOCKET" "$([ -S "$SOCKET" ] && echo ' (live)' || echo ' (absent)')"
		printf '  helper socket: %s%s\n' "$HELPER_SOCKET" "$([ -S "$HELPER_SOCKET" ] && echo ' (live)' || echo ' (absent)')"
		printf '  state dir:     %s%s\n' "$STATE_DIR" "$([ -d "$STATE_DIR" ] && echo ' (present)' || echo ' (absent)')"
		printf '  helper work:   %s%s\n' "$HELPER_WORK_DIR" "$([ -d "$HELPER_WORK_DIR" ] && echo ' (present)' || echo ' (absent)')"
		printf '  wiring:        %s\n' "$ARGS_ENV"
		log "adapter arguments (auditable):"
		adapter_args | sed 's/^/    /'
	else
		warn "no persisted wiring at $ARGS_ENV"
	fi
	local l
	for l in "$HELPER_LABEL" "$LABEL"; do
		if "$LAUNCHCTL" print "$(gui_domain)/$l" >/dev/null 2>&1; then
			ok "LaunchAgent $l is loaded"
		else
			warn "LaunchAgent $l is not loaded"
		fi
	done
}

cmd_preflight() {
	parse_flags "$@"
	load_args
	local bin="$CURRENT_LINK/macvz-cri"
	[ -x "$bin" ] || bin="$FROM_DIR/macvz-cri"
	[ -x "$bin" ] || die "macvz-cri binary not found (install first or pass --from)"
	local args=()
	while IFS= read -r a; do args+=("$a"); done < <(adapter_args)
	exec "$bin" --preflight "${args[@]}"
}

services_loaded() {
	# Test seam: the ":" stub exits 0 for anything, which would read as
	# "always loaded" — treat it as "nothing loaded" instead.
	[ "$LAUNCHCTL" = ":" ] && return 1
	"$LAUNCHCTL" print "$(gui_domain)/$LABEL" >/dev/null 2>&1 || \
		"$LAUNCHCTL" print "$(gui_domain)/$HELPER_LABEL" >/dev/null 2>&1
}

# cmd_clean removes stale LinuxPod runtime state (CRI-L9-4 #152): dangling
# sockets, supervisor/adoption journals, sup-* residue, persisted sandbox and
# container records. Default is an audited dry-run listing every path with its
# size; --force performs the deletions. Refuses to run while the services are
# loaded, because live state would be corrupted, and never touches routes, pf,
# or macvz-netd.
cmd_clean() {
	parse_flags "$@"
	load_args
	if services_loaded; then
		die "services are loaded; run '$0 uninstall' (or launchctl bootout) before clean — cleaning live state corrupts running Pods"
	fi
	log "Stale-state scan (mode: $([ "$FORCE" = 1 ] && echo DESTRUCTIVE || echo dry-run))"
	local targets=() t
	[ -S "$SOCKET" ] || [ -e "$SOCKET" ] && targets+=("$SOCKET")
	[ -S "$HELPER_SOCKET" ] || [ -e "$HELPER_SOCKET" ] && targets+=("$HELPER_SOCKET")
	[ -f "$HELPER_WORK_DIR/supervisor-journal.json" ] && targets+=("$HELPER_WORK_DIR/supervisor-journal.json")
	[ -f "$HELPER_WORK_DIR/adoption-journal.json" ] && targets+=("$HELPER_WORK_DIR/adoption-journal.json")
	while IFS= read -r t; do
		targets+=("$t")
	done < <(find "$HELPER_WORK_DIR" -mindepth 1 -maxdepth 1 \( -name 'sup-*' -o -name 's-*.sock' \) 2>/dev/null)
	[ -d "$STATE_DIR" ] && while IFS= read -r t; do
		targets+=("$t")
	done < <(find "$STATE_DIR" -mindepth 1 -maxdepth 2 -name '*.json' 2>/dev/null)

	if [ "${#targets[@]}" -eq 0 ]; then
		ok "no stale state found"
		return 0
	fi
	local total=0 size
	for t in "${targets[@]}"; do
		size="$(du -sk "$t" 2>/dev/null | awk '{print $1}')"
		printf '  %8sKB  %s\n' "${size:-0}" "$t"
		total=$((total + ${size:-0}))
	done
	log "total: ${total}KB in ${#targets[@]} paths"
	if [ "$FORCE" != 1 ]; then
		log "dry-run only; re-run with --force to delete (audit log: $LOG_DIR/clean.log)"
		return 0
	fi
	run mkdir -p "$LOG_DIR"
	for t in "${targets[@]}"; do
		if [ "$DRY_RUN" = 1 ]; then
			printf '   [dry-run] rm -rf %s\n' "$t"
		else
			printf '%s clean rm -rf %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$t" >>"$LOG_DIR/clean.log"
			rm -rf "$t"
		fi
	done
	ok "cleaned ${#targets[@]} paths (audited in $LOG_DIR/clean.log)"
}

cmd_uninstall() {
	parse_flags "$@"
	[ -r "$ARGS_ENV" ] && load_args
	log "Uninstalling LinuxPod CRI node services"
	run "$LAUNCHCTL" bootout "$(gui_domain)/$LABEL" 2>/dev/null || true
	run "$LAUNCHCTL" bootout "$(gui_domain)/$HELPER_LABEL" 2>/dev/null || true
	[ -f "$PLIST" ] && run rm -f "$PLIST"
	[ -f "$HELPER_PLIST" ] && run rm -f "$HELPER_PLIST"
	# Leave no stale sockets behind — a dangling socket makes crictl/kubelet
	# believe an endpoint exists.
	[ -S "$SOCKET" ] && run rm -f "$SOCKET"
	[ -S "$HELPER_SOCKET" ] && run rm -f "$HELPER_SOCKET"

	if [ "$PURGE" = 1 ]; then
		warn "purging state, helper work dir, logs, wiring, and all installed versions"
		run rm -rf "$STATE_DIR" "$HELPER_WORK_DIR" "$LOG_DIR" "$VERSIONS_DIR"
		run rm -f "$CURRENT_LINK" "$PREVIOUS_FILE" "$ARGS_ENV"
	else
		log "state/versions preserved under $PREFIX (use --purge to remove)"
	fi
	ok "uninstalled"
}

main() {
	[ "$#" -ge 1 ] || usage 1
	local sub="$1"; shift
	case "$sub" in
		install)   cmd_install "$@" ;;
		upgrade)   cmd_upgrade "$@" ;;
		rollback)  cmd_rollback "$@" ;;
		restart)   cmd_restart "$@" ;;
		status)    cmd_status "$@" ;;
		preflight) cmd_preflight "$@" ;;
		clean)     cmd_clean "$@" ;;
		uninstall) cmd_uninstall "$@" ;;
		-h|--help|help) usage 0 ;;
		*) die "unknown subcommand: $sub (install|upgrade|rollback|restart|status|preflight|clean|uninstall)" ;;
	esac
}

main "$@"
