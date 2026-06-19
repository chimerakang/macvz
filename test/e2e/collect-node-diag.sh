#!/usr/bin/env bash
#
# collect-node-diag.sh — node-level network diagnostics for MacVz cross-host
# failures (issue #43). Run ON a Mac that hosts a macvz-kubelet to capture the
# privileged data-plane state needed to tell apart mesh, route, pf, forwarding,
# and bridge problems:
#
#   sudo ./collect-node-diag.sh                 # gather + redact, print to stdout
#   sudo ./collect-node-diag.sh > node-a.txt    # save a bundle
#   ./collect-node-diag.sh redact < raw.txt     # filter mode: redact stdin only
#
# It is read-only and self-contained (no external files), so test/e2e/e2e.sh can
# pipe it to a remote node over ssh:
#
#   ssh admin@mac-a sudo bash -s < collect-node-diag.sh > node-a.txt
#
# Secrets are masked before output (WireGuard private/preshared keys, tokens,
# client-key-data, passwords) so a bundle is safe to attach to an issue. Public
# keys, addresses, routes, and pf rules are kept — those are what you diagnose
# with.
#
# Tunables (env):
#   MACVZ_DIAG_ANCHOR       pf anchor to dump. Default: macvz/pods.
#   MACVZ_DIAG_MESH_IFACE   WireGuard interface(s), space-separated. Default:
#                           auto-detected via `wg show interfaces`.
#   MACVZ_DIAG_VMNET_IFACE  Pod bridge interface. Default: bridge100.
set -uo pipefail

# --- redaction ---------------------------------------------------------------
# redact_filter masks secret values while preserving their labels and all
# non-secret context. Label-based (not pattern-based) so public keys, which look
# like private ones, are NOT redacted — they are needed to correlate peers.
redact_filter() {
	awk '
	{
		low = tolower($0)
		if (low ~ /private[ ]?key/ || low ~ /preshared[ ]?key/ || \
		    low ~ /client-key-data/ || low ~ /token:/ || low ~ /password/ || \
		    low ~ /authorization: bearer/ || low ~ /secret-data/) {
			# Keep up to and including the first : or =, mask the value.
			if (match($0, /[:=]/)) {
				print substr($0, 1, RSTART) " [REDACTED]"
			} else {
				print "[REDACTED]"
			}
			next
		}
		print
	}'
}

# Filter mode: redact stdin and exit (used internally and by the unit test).
if [ "${1:-}" = "redact" ]; then
	redact_filter
	exit 0
fi

ANCHOR="${MACVZ_DIAG_ANCHOR:-macvz/pods}"
VMNET_IFACE="${MACVZ_DIAG_VMNET_IFACE:-bridge100}"

# Resolve WireGuard interfaces: explicit override, else ask wg, else fall back to
# the utun7 the two-node fixture uses.
mesh_ifaces() {
	if [ -n "${MACVZ_DIAG_MESH_IFACE:-}" ]; then
		printf '%s\n' "$MACVZ_DIAG_MESH_IFACE"
		return
	fi
	local got
	got="$(wg show interfaces 2>/dev/null)"
	[ -n "$got" ] && { printf '%s\n' "$got"; return; }
	printf 'utun7\n'
}

section() { printf '\n### %s\n' "$*"; }

# run_cmd prints the command then its combined output; never fails the script.
run_cmd() {
	printf '$ %s\n' "$*"
	"$@" 2>&1 || printf '(exit %d)\n' "$?"
}

gather() {
	section "host"
	run_cmd hostname
	run_cmd date
	printf 'euid: %s%s\n' "$(id -u)" "$([ "$(id -u)" -eq 0 ] || echo '  (NOT root — wg/pfctl output will be incomplete; re-run with sudo)')"

	section "WireGuard interfaces"
	run_cmd wg show interfaces

	local iface
	for iface in $(mesh_ifaces); do
		section "WireGuard: wg show $iface (handshakes, transfer, endpoints)"
		run_cmd wg show "$iface"
		section "WireGuard config: wg showconf $iface (keys masked)"
		run_cmd wg showconf "$iface"
		section "mesh interface: ifconfig $iface"
		run_cmd ifconfig "$iface"
	done

	section "IPv4 routing table"
	run_cmd netstat -rn -f inet

	section "IPv4 forwarding sysctl"
	run_cmd sysctl net.inet.ip.forwarding

	section "pf status"
	run_cmd pfctl -s info

	section "pf anchor $ANCHOR — nat/binat rules"
	run_cmd pfctl -a "$ANCHOR" -s nat

	section "pf anchor $ANCHOR — filter rules"
	run_cmd pfctl -a "$ANCHOR" -s rules

	section "Pod bridge: ifconfig $VMNET_IFACE"
	run_cmd ifconfig "$VMNET_IFACE"
}

gather | redact_filter
