#!/usr/bin/env bash
#
# run.sh — start/stop the local macvz-kubelet for the issue #37 two-Mac e2e.
# Run on EACH Mac (it manages only the local process). The actual suite is
# invoked separately:
#
#   KUBECONFIG=... ./run.sh start a     # on 192.168.1.110
#   KUBECONFIG=... ./run.sh start b     # on 192.168.1.122
#   ./run.sh stop a
#
# Looks for the kubelet binary at ../../../bin/macvz-kubelet (repo build) or on
# PATH. Logs to ./macvz-<node>.log; pidfile ./macvz-<node>.pid.
set -euo pipefail

CMD="${1:-}"; NODE="${2:-}"
case "$NODE" in a|b) ;; *) echo "usage: $0 <start|stop> <a|b>" >&2; exit 2;; esac

HERE="$(cd "$(dirname "$0")" && pwd)"
CFG="$HERE/macvz-$NODE.yaml"
PIDFILE="$HERE/macvz-$NODE.pid"
LOG="$HERE/macvz-$NODE.log"

BIN="${MACVZ_KUBELET_BIN:-}"
if [ -z "$BIN" ]; then
	if [ -x "$HERE/../../../bin/macvz-kubelet" ]; then
		BIN="$HERE/../../../bin/macvz-kubelet"
	elif command -v macvz-kubelet >/dev/null 2>&1; then
		BIN="$(command -v macvz-kubelet)"
	else
		echo "macvz-kubelet binary not found — run 'make build' or set MACVZ_KUBELET_BIN" >&2
		exit 2
	fi
fi

case "$CMD" in
	start)
		[ -f "$CFG" ] || { echo "missing $CFG" >&2; exit 2; }
		if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
			echo "already running (pid $(cat "$PIDFILE"))"; exit 0
		fi
		: "${KUBECONFIG:?set KUBECONFIG to the shared cluster credentials}"
		# mesh + podNetwork program WireGuard/pf, which need root; the kubelet must
		# therefore run privileged on the host.
		echo "starting macvz-$NODE ($BIN --config $CFG)"
		sudo -E "$BIN" --config "$CFG" >"$LOG" 2>&1 &
		echo $! > "$PIDFILE"
		echo "started pid $(cat "$PIDFILE"); logs: $LOG"
		;;
	stop)
		if [ -f "$PIDFILE" ]; then
			PID="$(cat "$PIDFILE")"
			# kubelet shutdown flushes the macvz/pods anchor and tears the mesh down.
			sudo kill "$PID" 2>/dev/null || true
			rm -f "$PIDFILE"
			echo "stopped macvz-$NODE (pid $PID)"
		else
			echo "no pidfile; nothing to stop"
		fi
		;;
	*) echo "usage: $0 <start|stop> <a|b>" >&2; exit 2 ;;
esac
