#!/usr/bin/env bash
#
# prep-node.sh — privileged per-host setup for the issue #37 two-Mac
# WireGuard + podNetwork e2e. Run ONCE on each Mac, as a user with sudo:
#
#   sudo ./prep-node.sh a   # on 192.168.1.110 (macvz-a)
#   sudo ./prep-node.sh b   # on 192.168.1.122 (macvz-b)
#
# It is idempotent: installs the node's WireGuard private key, ensures serving
# TLS material, hooks the pf anchor into /etc/pf.conf, and enables IPv4
# forwarding. It does NOT start macvz-kubelet (run.sh does) and makes no change
# that survives unless pf/forwarding are explicitly persisted — see Cleanup.
set -euo pipefail

NODE="${1:-}"
case "$NODE" in
	a|b) ;;
	*) echo "usage: sudo $0 <a|b>" >&2; exit 2 ;;
esac

HERE="$(cd "$(dirname "$0")" && pwd)"
KEY_SRC="$HERE/keys/macvz-$NODE.key"
PRIV_DEST="/etc/macvz/wireguard.key"
PKI_DIR="/etc/macvz/pki"
ANCHOR_HOOKS="$HERE/pf-anchor-hooks.conf"
PF_CONF="/etc/pf.conf"

[ "$(id -u)" -eq 0 ] || { echo "must run as root (use sudo)" >&2; exit 2; }
[ -f "$KEY_SRC" ] || { echo "missing $KEY_SRC — generate keys first (see README)" >&2; exit 2; }

echo "==> Installing WireGuard private key -> $PRIV_DEST"
install -d -m 700 /etc/macvz
install -m 600 "$KEY_SRC" "$PRIV_DEST"

echo "==> Ensuring kubelet serving TLS (self-signed) under $PKI_DIR"
install -d -m 700 "$PKI_DIR"
if [ ! -f "$PKI_DIR/kubelet.crt" ] || [ ! -f "$PKI_DIR/kubelet.key" ]; then
	IP="$([ "$NODE" = a ] && echo 192.168.1.110 || echo 192.168.1.122)"
	openssl req -x509 -nodes -newkey rsa:2048 -days 365 \
		-keyout "$PKI_DIR/kubelet.key" -out "$PKI_DIR/kubelet.crt" \
		-subj "/CN=macvz-$NODE" \
		-addext "subjectAltName=IP:$IP" >/dev/null 2>&1
	chmod 600 "$PKI_DIR/kubelet.key"
	echo "    generated kubelet.crt/key for IP $IP"
else
	echo "    serving TLS already present, leaving as-is"
fi

echo "==> Hooking pf anchor into $PF_CONF"
if grep -q 'anchor "macvz/pods/\*"' "$PF_CONF" 2>/dev/null; then
	echo "    anchor hooks already present"
else
	cp "$PF_CONF" "${PF_CONF}.macvz.bak.$(date +%s 2>/dev/null || echo backup)" 2>/dev/null || true
	# pf requires strict ordering: translation anchors (nat/rdr/binat) must
	# precede filter anchors. macOS ships a pf.conf with com.apple anchors, so we
	# insert the macvz translation anchors right after the com.apple rdr-anchor
	# and the macvz filter anchor right after the com.apple filter anchor. If the
	# com.apple markers are absent (non-standard pf.conf) we fall back to
	# prepending translation anchors and appending the filter anchor.
	if grep -q '^rdr-anchor "com\.apple/\*"' "$PF_CONF" && grep -q '^anchor "com\.apple/\*"' "$PF_CONF"; then
		awk '
			/^rdr-anchor "com\.apple\/\*"/ {
				print
				print "nat-anchor \"macvz/pods/*\""
				print "rdr-anchor \"macvz/pods/*\""
				print "binat-anchor \"macvz/pods/*\""
				next
			}
			/^anchor "com\.apple\/\*"/ {
				print
				print "anchor \"macvz/pods/*\""
				next
			}
			{ print }
		' "$PF_CONF" > "${PF_CONF}.macvz.new" && mv "${PF_CONF}.macvz.new" "$PF_CONF"
		echo "    inserted hooks in pf order (backup left next to $PF_CONF)"
	else
		{ echo ""; echo "# --- macvz/pods anchor (issue #37) ---"; cat "$ANCHOR_HOOKS"; } >> "$PF_CONF"
		echo "    appended hooks (no com.apple markers; backup left next to $PF_CONF)"
	fi
fi
echo "==> Validating + (re)loading pf"
pfctl -n -f "$PF_CONF"   # syntax check; fails loud if the append is invalid
pfctl -f "$PF_CONF" 2>/dev/null || true
pfctl -e 2>/dev/null || true   # tolerate "already enabled"

echo "==> Enabling IPv4 forwarding"
sysctl -w net.inet.ip.forwarding=1 >/dev/null

echo "==> vmnet bridge check"
if ifconfig bridge100 >/dev/null 2>&1; then
	echo "    bridge100 present"
else
	echo "    NOTE: bridge100 not up yet — it appears after the first micro-VM"
	echo "          starts. If apple/container uses a different bridge, set"
	echo "          podNetwork.interface accordingly in macvz-$NODE.yaml."
fi

echo "==> Done. Node macvz-$NODE prepared."
echo "    Public key for peer config: $(wg pubkey < "$PRIV_DEST")"
