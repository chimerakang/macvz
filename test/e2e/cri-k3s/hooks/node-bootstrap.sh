#!/usr/bin/env bash
set -euo pipefail

# Reference MACVZ_BOOTSTRAP_CMD hook for node-reboot-recovery.sh (CRI-L8-5 #144).
#
# It brings the LinuxPod-backed k3s node stack up in the documented startup
# order, cleaning stale state FIRST so a fresh run is never blocked by leftovers,
# while preserving the host default route. It does NOT use sudo, route, or pfctl,
# and never touches default routes — that is the #144 non-goal.
#
# Startup order honored:
#   1. apple/container   `container system start` (per-user service).
#   2. macvz-netd        verified reachable on its socket (a privileged system
#                        service that comes back on boot via launchd; this hook
#                        only waits for it, it does not (re)launch it).
#   3. linuxpod-helper   the #139 router — reuses hooks/linuxpod-helper-restart.sh.
#   4. macvz-cri         the adapter — reuses hooks/linuxpod-cri-restart.sh.
#   5. kubelet / k3s     left to its own KeepAlive/agent supervision; the caller
#                        asserts node Ready through kubectl.
#   6. kind socket fwd   verified only (the local kind topology owns it).
#
# Stale-state cleanup (safe, scoped): before bring-up it removes leftover
# macvz-cri / linuxpod-helper sockets and stale per-Pod supervisor journals that
# a crash or reboot can leave behind. It NEVER kills live `supervise-pod`
# children (post-reboot there are none; after a soft restart they own live VMs
# and the helper hook re-adopts them).
#
# Most behaviour is delegated to the sibling restart hooks so the startup
# topology stays single-sourced. Environment passthrough mirrors those hooks
# (MACVZ_CRI_SSH_TARGET / MACVZ_HELPER_SSH_TARGET / MACVZ_LINUXPOD_* / etc.).

HERE="$(cd "$(dirname "$0")" && pwd)"
target="${MACVZ_BOOTSTRAP_SSH_TARGET:-${MACVZ_CRI_SSH_TARGET:-${MACVZ_HELPER_SSH_TARGET:-${1:-}}}}"
netd_socket="${MACVZ_NETD_SOCKET:-/var/run/macvz-netd.sock}"
helper_socket="${MACVZ_LINUXPOD_HELPER_SOCKET:-}"
cri_socket="${MACVZ_CRI_SOCKET:-}"
clean_stale="${MACVZ_BOOTSTRAP_CLEAN_STALE:-1}"
container_start="${MACVZ_BOOTSTRAP_CONTAINER_START:-1}"
netd_wait="${MACVZ_BOOTSTRAP_NETD_WAIT:-60}"

log() { printf '==> [node-bootstrap] %s\n' "$*"; }

# remote_or_local runs a `/bin/sh -s` snippet either over SSH or locally.
remote_or_local() {
	if [ -n "$target" ]; then
		ssh "$target" /bin/sh -s -- "$@"
	else
		/bin/sh -s -- "$@"
	fi
}

# --- 1. apple/container ------------------------------------------------------
if [ "$container_start" = 1 ] || [ "$container_start" = true ]; then
	log "step 1/6: ensure apple/container system is started"
	printf '%s\n' '
set -eu
if command -v container >/dev/null 2>&1; then
	container system start >/dev/null 2>&1 || true
	# Best-effort readiness wait; do not fail bring-up if status is terse.
	i=0
	while [ "$i" -lt 30 ]; do
		i=$((i + 1))
		if container system status >/dev/null 2>&1; then
			printf "apple/container: ready\n"
			exit 0
		fi
		sleep 1
	done
	printf "apple/container: status-unconfirmed (continuing)\n"
else
	printf "apple/container: container CLI not found (skipping start)\n"
fi
' | remote_or_local
else
	log "step 1/6: apple/container start skipped (MACVZ_BOOTSTRAP_CONTAINER_START=0)"
fi

# --- 1b. stale-state cleanup -------------------------------------------------
if [ "$clean_stale" = 1 ] || [ "$clean_stale" = true ]; then
	log "step 1b: clean stale sockets / supervisor journals (route-preserving)"
	printf '%s\n' '
set -eu
helper_socket="$1"
cri_socket="$2"

# Remove a stale unix socket only if no live process is bound to it. We never
# touch default routes, pf, or live supervise-pod children here.
remove_stale_socket() {
	sock="$1"
	[ -n "$sock" ] || return 0
	[ -e "$sock" ] || return 0
	if command -v lsof >/dev/null 2>&1 && lsof -- "$sock" >/dev/null 2>&1; then
		printf "stale-clean: %s still bound by a live process; leaving it\n" "$sock"
		return 0
	fi
	rm -f "$sock" && printf "stale-clean: removed stale socket %s\n" "$sock"
}

remove_stale_socket "$helper_socket"
remove_stale_socket "$cri_socket"

# Drop supervisor-journal entries whose supervise-pod process is gone, so a
# restarted router does not advertise dead pods as adoptable. Keep entries whose
# pid is still alive (a soft restart that preserved the VM).
for jr in "$HOME"/macvz-cri-live-*/service-linuxpod/supervisor-journal.json; do
	[ -f "$jr" ] || continue
	if ! ps -axo command= | grep -q "[s]upervise-pod"; then
		: >"$jr"
		printf "stale-clean: cleared dead supervisor journal %s\n" "$jr"
	fi
done
printf "stale-clean: done\n"
' | remote_or_local "$helper_socket" "$cri_socket"
else
	log "step 1b: stale-state cleanup skipped (MACVZ_BOOTSTRAP_CLEAN_STALE=0)"
fi

# --- 2. macvz-netd -----------------------------------------------------------
log "step 2/6: wait for macvz-netd socket ($netd_socket)"
printf '%s\n' '
set -eu
socket="$1"
deadline="$2"
i=0
while [ "$i" -lt "$deadline" ]; do
	i=$((i + 1))
	if [ -S "$socket" ]; then
		printf "macvz-netd: ready (%s)\n" "$socket"
		exit 0
	fi
	sleep 1
done
printf "macvz-netd: socket %s not present after %ss\n" "$socket" "$deadline" >&2
exit 1
' | remote_or_local "$netd_socket" "$netd_wait"

# --- 3. linuxpod-helper ------------------------------------------------------
log "step 3/6: (re)start linuxpod-helper router"
"$HERE/linuxpod-helper-restart.sh"

# --- 4. macvz-cri ------------------------------------------------------------
log "step 4/6: (re)start macvz-cri adapter"
"$HERE/linuxpod-cri-restart.sh"

# --- 5. kubelet / k3s --------------------------------------------------------
log "step 5/6: kubelet/k3s left to its own supervision; caller asserts node Ready"

# --- 6. kind socket forward --------------------------------------------------
if [ -n "${MACVZ_KIND_FORWARD_PROBE_CMD:-}" ]; then
	log "step 6/6: verify kind socket forward (MACVZ_KIND_FORWARD_PROBE_CMD)"
	sh -c "$MACVZ_KIND_FORWARD_PROBE_CMD"
else
	log "step 6/6: kind socket forward probe not set (MACVZ_KIND_FORWARD_PROBE_CMD); skipping"
fi

log "node stack bootstrap complete"
