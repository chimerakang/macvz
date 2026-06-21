#!/usr/bin/env bash
#
# run.sh - start/stop the local macvz-kubelet for the issue #37 two-Mac e2e.
# Run on EACH Mac (it manages only the local process). The actual suite is
# invoked separately:
#
#   KUBECONFIG=... ./run.sh start a     # on 192.168.1.110
#   KUBECONFIG=... ./run.sh start b     # on 192.168.1.122
#   KUBECONFIG=... ./run.sh agent-start a
#   ./run.sh stop a
#
# Looks for the kubelet binary at ../../../bin/macvz-kubelet (repo build) or on
# PATH. Logs to ./macvz-<node>.log; pidfile ./macvz-<node>.pid. The agent-*
# commands manage a per-user LaunchAgent for restart/recovery soak tests.
set -euo pipefail

export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

CMD="${1:-}"; NODE="${2:-}"
usage() {
	echo "usage: $0 <start|stop|restart|status|agent-install|agent-start|agent-stop|agent-restart|agent-status|agent-uninstall> <a|b>" >&2
}

case "$NODE" in a|b) ;; *) usage; exit 2;; esac

HERE="$(cd "$(dirname "$0")" && pwd)"
CFG="$HERE/macvz-$NODE.yaml"
PIDFILE="$HERE/macvz-$NODE.pid"
LOG="$HERE/macvz-$NODE.log"
LABEL="com.github.chimerakang.macvz-kubelet.two-node.$NODE"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LAUNCH_DOMAIN="gui/$(id -u)"
LAUNCH_SERVICE="$LAUNCH_DOMAIN/$LABEL"
AGENT_DIR="${MACVZ_AGENT_DIR:-$HOME/.macvz/two-node/$NODE}"
AGENT_BIN="$AGENT_DIR/macvz-kubelet"
AGENT_CFG="$AGENT_DIR/macvz-$NODE.yaml"
AGENT_KUBECONFIG="$AGENT_DIR/kubeconfig.yaml"
AGENT_LOG="$AGENT_DIR/macvz-$NODE.log"
AGENT_ERR_LOG="$AGENT_DIR/macvz-$NODE.err.log"

BIN="${MACVZ_KUBELET_BIN:-}"
if [ -z "$BIN" ]; then
	if [ -x "$HERE/bin/macvz-kubelet" ]; then
		BIN="$HERE/bin/macvz-kubelet"
	elif [ -x "$HERE/../../../bin/macvz-kubelet" ]; then
		BIN="$HERE/../../../bin/macvz-kubelet"
	elif command -v macvz-kubelet >/dev/null 2>&1; then
		BIN="$(command -v macvz-kubelet)"
	else
		echo "macvz-kubelet binary not found — run 'make build' or set MACVZ_KUBELET_BIN" >&2
		exit 2
	fi
fi

resolve_existing_path() {
	local p="$1" dir base
	if [ -z "$p" ]; then
		return 1
	fi
	case "$p" in
		/*) ;;
		*) p="$PWD/$p" ;;
	esac
	dir="$(dirname "$p")"
	base="$(basename "$p")"
	if [ -d "$dir" ]; then
		printf '%s/%s\n' "$(cd "$dir" && pwd)" "$base"
	else
		printf '%s\n' "$p"
	fi
}

xml_escape() {
	local s="$1"
	s="${s//&/&amp;}"
	s="${s//</&lt;}"
	s="${s//>/&gt;}"
	s="${s//\"/&quot;}"
	printf '%s\n' "$s"
}

render_agent_plist() {
	local kubeconfig_abs bin_abs cfg_abs here_abs path_env user_name
	: "${KUBECONFIG:?set KUBECONFIG to the shared cluster credentials}"
	kubeconfig_abs="$(resolve_existing_path "$AGENT_KUBECONFIG")"
	bin_abs="$(resolve_existing_path "$AGENT_BIN")"
	cfg_abs="$(resolve_existing_path "$AGENT_CFG")"
	here_abs="$(resolve_existing_path "$AGENT_DIR")"
	path_env="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	user_name="${USER:-$(id -un)}"
	cat <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$(xml_escape "$LABEL")</string>
  <key>ProgramArguments</key>
  <array>
    <string>$(xml_escape "$bin_abs")</string>
    <string>--config</string>
    <string>$(xml_escape "$cfg_abs")</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key><string>$(xml_escape "$HOME")</string>
    <key>KUBECONFIG</key><string>$(xml_escape "$kubeconfig_abs")</string>
    <key>LOGNAME</key><string>$(xml_escape "$user_name")</string>
    <key>PATH</key><string>$(xml_escape "$path_env")</string>
    <key>USER</key><string>$(xml_escape "$user_name")</string>
  </dict>
  <key>WorkingDirectory</key><string>$(xml_escape "$here_abs")</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key>
  <dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>$(xml_escape "$AGENT_LOG")</string>
  <key>StandardErrorPath</key><string>$(xml_escape "$AGENT_ERR_LOG")</string>
</dict>
</plist>
PLIST
}

agent_install() {
	[ -f "$CFG" ] || { echo "missing $CFG" >&2; exit 2; }
	: "${KUBECONFIG:?set KUBECONFIG to the shared cluster credentials}"
	mkdir -p "$(dirname "$PLIST")" "$AGENT_DIR"
	cp "$BIN" "$AGENT_BIN"
	cp "$CFG" "$AGENT_CFG"
	cp "$(resolve_existing_path "$KUBECONFIG")" "$AGENT_KUBECONFIG"
	chmod 0755 "$AGENT_BIN"
	chmod 0644 "$AGENT_CFG" "$AGENT_KUBECONFIG"
	touch "$AGENT_LOG" "$AGENT_ERR_LOG"
	render_agent_plist > "$PLIST"
	chmod 0644 "$PLIST" "$AGENT_LOG" "$AGENT_ERR_LOG"
	echo "installed LaunchAgent $LABEL at $PLIST (binary: $AGENT_BIN)"
}

agent_bootout() {
	launchctl bootout "$LAUNCH_SERVICE" >/dev/null 2>&1 || true
}

agent_start() {
	agent_install
	agent_bootout
	if ! launchctl bootstrap "$LAUNCH_DOMAIN" "$PLIST"; then
		sleep 2
		launchctl bootstrap "$LAUNCH_DOMAIN" "$PLIST"
	fi
	echo "started LaunchAgent $LABEL; logs: $AGENT_LOG"
}

agent_stop() {
	agent_bootout
	rm -f "$PIDFILE"
	echo "stopped LaunchAgent $LABEL"
}

agent_restart() {
	if [ ! -f "$PLIST" ]; then
		agent_start
		return
	fi
	if ! launchctl print "$LAUNCH_SERVICE" >/dev/null 2>&1; then
		if ! launchctl bootstrap "$LAUNCH_DOMAIN" "$PLIST"; then
			sleep 2
			launchctl bootstrap "$LAUNCH_DOMAIN" "$PLIST"
		fi
	fi
	launchctl kickstart -k "$LAUNCH_SERVICE"
	echo "restarted LaunchAgent $LABEL; logs: $AGENT_LOG"
}

agent_status() {
	echo "LaunchAgent: $LABEL"
	echo "Plist: $PLIST"
	launchctl print "$LAUNCH_SERVICE"
}

stop_processes() {
	local stopped=0 pid
	if [ -f "$PIDFILE" ]; then
		pid="$(cat "$PIDFILE")"
		# kubelet shutdown flushes the macvz/pods anchor and tears the mesh down.
		kill "$pid" 2>/dev/null || true
		rm -f "$PIDFILE"
		echo "stopped macvz-$NODE (pid $pid)"
		stopped=1
	fi
	# If the bundle was replaced or a stale process survived without its
	# pidfile, fall back to matching this node's config path.
	while IFS= read -r pid; do
		[ -n "$pid" ] || continue
		kill "$pid" 2>/dev/null || true
		echo "stopped macvz-$NODE stray process (pid $pid)"
		stopped=1
	done < <(pgrep -f "macvz-kubelet.*--config $CFG" 2>/dev/null || true)
	rm -f "$PIDFILE"
	if [ "$stopped" = 0 ]; then
		echo "no pidfile/process; nothing to stop"
	fi
}

case "$CMD" in
	start)
		[ -f "$CFG" ] || { echo "missing $CFG" >&2; exit 2; }
		if [ "${MACVZ_RUN_USE_LAUNCHD:-0}" = 1 ]; then
			agent_start
			exit 0
		fi
		if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
			echo "already running (pid $(cat "$PIDFILE"))"; exit 0
		fi
		: "${KUBECONFIG:?set KUBECONFIG to the shared cluster credentials}"
		# The kubelet must run as the operator user because apple/container is
		# per-user. Privileged pf/route/wg/sysctl operations go through macvz-netd.
		# nohup keeps the process alive when this script is invoked by ssh or a
		# short-lived test shell.
		echo "starting macvz-$NODE ($BIN --config $CFG)"
		nohup "$BIN" --config "$CFG" >"$LOG" 2>&1 </dev/null &
		echo $! > "$PIDFILE"
		echo "started pid $(cat "$PIDFILE"); logs: $LOG"
		;;
	stop)
		agent_bootout
		stop_processes
		;;
	restart)
		if [ -f "$PLIST" ] || [ "${MACVZ_RUN_USE_LAUNCHD:-0}" = 1 ]; then
			agent_restart
		else
			"$0" stop "$NODE"
			"$0" start "$NODE"
		fi
		;;
	status)
		if [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
			echo "process running (pid $(cat "$PIDFILE"))"
		else
			echo "process pidfile not running"
		fi
		if launchctl print "$LAUNCH_SERVICE" >/dev/null 2>&1; then
			echo "LaunchAgent loaded: $LABEL"
		else
			echo "LaunchAgent not loaded: $LABEL"
		fi
		;;
	agent-install)
		agent_install
		;;
	agent-start)
		agent_start
		;;
	agent-stop)
		agent_stop
		stop_processes
		;;
	agent-restart)
		agent_restart
		;;
	agent-status)
		agent_status
		;;
	agent-uninstall)
		agent_stop
		rm -f "$PLIST"
		echo "removed $PLIST"
		;;
	*) usage; exit 2 ;;
esac
