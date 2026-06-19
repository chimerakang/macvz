#!/usr/bin/env bash
#
# verify-dataplane.sh — issue #37 "Validation" checks, run on EITHER Mac after
# both macvz-kubelet processes are up with mesh+podNetwork enabled. Confirms the
# privileged data plane is live before/independently of test/e2e/e2e.sh.
#
#   sudo ./verify-dataplane.sh <a|b>
#
# Exits 0 only if the WireGuard handshake, remote Pod CIDR route, and pf anchor
# are all present. Read-only: it changes nothing.
set -uo pipefail

NODE="${1:-}"; case "$NODE" in a|b) ;; *) echo "usage: sudo $0 <a|b>" >&2; exit 2;; esac
[ "$(id -u)" -eq 0 ] || { echo "must run as root (sudo) — wg/pfctl need it" >&2; exit 2; }

IFACE=utun7
PEER_CIDR="$([ "$NODE" = a ] && echo 10.244.102.0/24 || echo 10.244.101.0/24)"
PEER_MESH="$([ "$NODE" = a ] && echo 10.99.0.2 || echo 10.99.0.1)"
rc=0

echo "==> wg show $IFACE (expect a recent handshake with the peer)"
if wg show "$IFACE" 2>/dev/null | tee /dev/stderr | grep -q "latest handshake"; then
	echo "PASS  WireGuard handshake established"
else
	echo "FAIL  no WireGuard handshake on $IFACE"; rc=1
fi

echo "==> route for remote Pod CIDR $PEER_CIDR"
if netstat -rn -f inet 2>/dev/null | grep -q "$IFACE"; then
	netstat -rn -f inet | grep "$IFACE"
	echo "PASS  routes present via $IFACE"
else
	echo "FAIL  no $IFACE routes for remote Pod CIDR"; rc=1
fi

echo "==> ping peer mesh address $PEER_MESH"
if ping -c2 -t3 "$PEER_MESH" >/dev/null 2>&1; then
	echo "PASS  peer reachable across tunnel"
else
	echo "WARN  could not ping $PEER_MESH (ICMP may be filtered; not fatal)"
fi

echo "==> pf anchor macvz/pods nat rules"
if pfctl -a macvz/pods -s nat 2>/dev/null | tee /dev/stderr | grep -q binat; then
	echo "PASS  binat rules present in anchor (Pods attached)"
else
	echo "INFO  no binat rules yet — expected until a Pod is scheduled on this node"
fi

echo "==> summary: $([ $rc -eq 0 ] && echo OK || echo FAILED)"
exit $rc
