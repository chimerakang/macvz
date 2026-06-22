#!/usr/bin/env bash
#
# run.sh — CRI-P8 k3s/kubelet compatibility suite for the experimental MacVz CRI
# adapter (issue #80).
#
# It exercises the adapter the way a k3s kubelet would: it drives the CRI socket
# with crictl through a realistic single-container Pod lifecycle (run/create/
# start/logs/exec/stop/remove), restarts the adapter to prove state recovery, and
# verifies that cleanup leaves no stale socket, state, or workload. When a
# Kubernetes cluster is wired, it checks API reachability and records that the
# kubectl-driven fixture deployment remains operator-driven for CRI-P9.
#
# This is an isolated `develop`-track feasibility probe, NOT the shipped Virtual
# Kubelet e2e (test/e2e/e2e.sh). It must not be used to gate the VK release path.
#
# Gating: the live suite mutates host state (boots micro-VMs, writes the CRI
# socket), so it runs only when MACVZ_INTEGRATION=1. Without it the script prints
# the plan it WOULD run and exits 0, so it is safe in `go test`-style CI.
#
# Environment:
#   MACVZ_INTEGRATION     set to 1 to run the live suite (otherwise plan-only).
#   MACVZ_CRI_BIN         path to the macvz-cri binary (default: ./bin/macvz-cri).
#   MACVZ_CRI_SOCKET      CRI socket to serve/probe (default: a per-run temp socket).
#   MACVZ_CRI_STATE_DIR   adapter state dir (default: a per-run temp dir).
#   MACVZ_CRI_IMAGE       arm64 image providing sh + httpd + wget (default: busybox:1.36.1).
#   MACVZ_CRI_MANAGE      1 (default) start/stop the adapter; 0 expect it already serving.
#   MACVZ_CRI_EXTRA       extra adapter flags (e.g. Pod networking).
#   CRICTL                crictl binary (default: crictl).
#   KUBECONFIG            if set and reachable, the kubectl/k3s phase runs too.
#   MACVZ_CRI_OUT_DIR     results/diagnostics dir (default: a mktemp dir).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"

INTEGRATION="${MACVZ_INTEGRATION:-0}"
CRI_BIN="${MACVZ_CRI_BIN:-$ROOT/bin/macvz-cri}"
SOCKET="${MACVZ_CRI_SOCKET:-}"
STATE_DIR="${MACVZ_CRI_STATE_DIR:-}"
IMAGE="${MACVZ_CRI_IMAGE:-busybox:1.36.1}"
MANAGE="${MACVZ_CRI_MANAGE:-1}"
CRICTL="${CRICTL:-crictl}"
# crictl defaults to a 2s per-RPC timeout, but booting a real apple/container
# micro-VM (StartContainer) routinely takes longer, which surfaces as a spurious
# DeadlineExceeded. Use a kubelet-like runtime-request timeout instead.
CRICTL_TIMEOUT="${CRICTL_TIMEOUT:-2m}"
OUT_DIR="${MACVZ_CRI_OUT_DIR:-}"

ADAPTER_PID=""
FAILURES=0
TMP_ROOT=""

c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
pass() { printf "${c_green}PASS${c_off} %s\n" "$*"; }
skip() { printf "${c_yellow}SKIP${c_off} %s\n" "$*"; }
fail() { printf "${c_red}FAIL${c_off} %s\n" "$*"; FAILURES=$((FAILURES+1)); }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

crt() { "$CRICTL" --runtime-endpoint "unix://$SOCKET" --timeout "$CRICTL_TIMEOUT" "$@"; }

print_plan() {
	cat <<'PLAN'
CRI-P8 k3s compatibility suite (plan; set MACVZ_INTEGRATION=1 to run live):

  preflight    macvz-cri --preflight (runtime deps: apple/container, socket, state)
  adapter      start macvz-cri on the CRI socket; crictl version + info
  image        pull the workload image through the CRI ImageService
  lifecycle    runp -> create -> start -> ps -> inspect (single-container Pod)
  logs         crictl logs returns the workload's stdout
  exec         crictl exec runs a command and returns its real exit code (probes)
  config       a projected config/secret mount is bound read-only into the VM
  unsupported  a hostNetwork Pod is rejected with a clear diagnostic (not booted)
  restart      stop + restart macvz-cri; crictl ps shows the recovered container
  cleanup      stop/rm container + sandbox; assert no stale socket/state/workload
  kubectl      (only when KUBECONFIG is set) check API reachability; fixtures are CRI-P9

Gated because the live suite boots micro-VMs and writes the CRI socket.
PLAN
}

# --- setup / teardown --------------------------------------------------------
setup() {
	command -v "$CRICTL" >/dev/null 2>&1 || die "crictl not found in PATH (set CRICTL)"
	[ -x "$CRI_BIN" ] || die "macvz-cri not found at $CRI_BIN (run 'make cri' or set MACVZ_CRI_BIN)"

	TMP_ROOT="$(mktemp -d -t macvz-cri-k3s)"
	[ -n "$SOCKET" ] || SOCKET="$TMP_ROOT/macvz-cri.sock"
	[ -n "$STATE_DIR" ] || STATE_DIR="$TMP_ROOT/state"
	[ -n "$OUT_DIR" ] || OUT_DIR="$TMP_ROOT/out"
	mkdir -p "$STATE_DIR" "$OUT_DIR"
	log "socket=$SOCKET state=$STATE_DIR out=$OUT_DIR"
}

start_adapter() {
	[ "$MANAGE" = 1 ] || { log "MACVZ_CRI_MANAGE=0: expecting an already-serving adapter"; return 0; }
	log "Starting macvz-cri"
	# The lifecycle phase binds a projected config dir under $OUT_DIR as a hostPath
	# to stand in for a kubelet-projected volume (no real kubelet here). The adapter
	# rejects arbitrary hostPaths by default (safe macOS posture), so explicitly
	# allow this run's output tree — exactly what its diagnostic instructs.
	# shellcheck disable=SC2086
	"$CRI_BIN" --listen "unix://$SOCKET" --state-dir "$STATE_DIR" \
		--volume-host-path-allowed "$OUT_DIR" ${MACVZ_CRI_EXTRA:-} \
		>"$OUT_DIR/adapter.log" 2>&1 &
	ADAPTER_PID="$!"
	wait_socket || die "adapter did not start serving on $SOCKET (see $OUT_DIR/adapter.log)"
}

stop_adapter() {
	[ "$MANAGE" = 1 ] || return 0
	[ -n "$ADAPTER_PID" ] || return 0
	kill "$ADAPTER_PID" 2>/dev/null || true
	wait "$ADAPTER_PID" 2>/dev/null || true
	ADAPTER_PID=""
}

wait_socket() {
	for _ in $(seq 1 50); do
		if crt version >/dev/null 2>&1; then return 0; fi
		sleep 0.2
	done
	return 1
}

cleanup() {
	stop_adapter
	# Best-effort: never leave a child adapter or temp tree behind.
	[ -n "$TMP_ROOT" ] && [ "${MACVZ_CRI_KEEP:-0}" != 1 ] && rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

# --- phases ------------------------------------------------------------------
phase_preflight() {
	log "Phase: preflight"
	local preflight_socket="$SOCKET"
	# With MACVZ_CRI_MANAGE=0 the target socket is supposed to be serving
	# already, so run dependency preflight against an unused socket. The adapter
	# handshake phase proves the external endpoint itself is reachable.
	[ "$MANAGE" = 1 ] || preflight_socket="$TMP_ROOT/preflight.sock"
	if "$CRI_BIN" --preflight --listen "unix://$preflight_socket" --state-dir "$STATE_DIR" \
		--volume-host-path-allowed "$OUT_DIR" ${MACVZ_CRI_EXTRA:-} >"$OUT_DIR/preflight.log" 2>&1; then
		pass "preflight reported no hard-dependency failures"
	else
		# A FAIL here is usually a missing apple/container; surface it but continue so
		# the suite still records which phases are reachable.
		fail "preflight reported FAIL items (see $OUT_DIR/preflight.log)"
	fi
}

phase_adapter() {
	log "Phase: adapter handshake"
	if crt version >"$OUT_DIR/version.txt" 2>&1; then pass "crictl version handshake"; else fail "crictl version"; return; fi
	crt info >"$OUT_DIR/info.json" 2>&1 && pass "crictl info" || fail "crictl info"
}

phase_image() {
	log "Phase: image pull"
	if crt pull "$IMAGE" >"$OUT_DIR/pull.txt" 2>&1; then
		pass "image pulled ($IMAGE)"
	else
		fail "image pull ($IMAGE; see $OUT_DIR/pull.txt)"
	fi
}

# write_fixtures emits the crictl sandbox/container JSON for the single-container
# compatibility Pod, including a read-only projected config mount.
write_fixtures() {
	local cfgdir="$OUT_DIR/cfg"
	mkdir -p "$cfgdir"
	printf 'macvz-cri-p8\n' >"$cfgdir/app.conf"

	cat >"$OUT_DIR/sandbox.json" <<EOF
{
  "metadata": {"name": "compat", "namespace": "macvz-cri-e2e", "uid": "compat-uid-1", "attempt": 0},
  "log_directory": "$OUT_DIR",
  "linux": {}
}
EOF

	cat >"$OUT_DIR/container.json" <<EOF
{
  "metadata": {"name": "app", "attempt": 0},
  "image": {"image": "$IMAGE"},
  "command": ["sh", "-c", "cat /etc/app/app.conf; httpd -f -p 8080"],
  "log_path": "app.log",
  "mounts": [
    {"host_path": "$cfgdir", "container_path": "/etc/app", "readonly": true}
  ]
}
EOF
}

phase_lifecycle() {
	log "Phase: single-container lifecycle + config mount"
	write_fixtures
	local sb cid
	sb="$(crt runp "$OUT_DIR/sandbox.json" 2>>"$OUT_DIR/lifecycle.log")" || { fail "runp"; return; }
	pass "runp ($sb)"
	# Record the sandbox id immediately so cleanup can always reclaim it, even if a
	# later step in this phase fails and returns early (otherwise the sandbox leaks).
	echo "$sb" >"$OUT_DIR/.sandbox-id"
	cid="$(crt create "$sb" "$OUT_DIR/container.json" "$OUT_DIR/sandbox.json" 2>>"$OUT_DIR/lifecycle.log")" || { fail "create"; return; }
	pass "create ($cid)"
	crt start "$cid" 2>>"$OUT_DIR/lifecycle.log" && pass "start" || fail "start"
	# Match against `ps -q` (full ids), not the `ps` table, whose CONTAINER column
	# truncates ids to 13 chars and would never match the full id we hold.
	crt ps -a -q 2>>"$OUT_DIR/lifecycle.log" | grep -q "$cid" && pass "ps shows container" || fail "ps"

	# logs: the workload cats the projected config on boot, proving the read-only
	# mount was bound (config/secrets compat) and that crictl logs streams stdout.
	if crt logs "$cid" 2>>"$OUT_DIR/lifecycle.log" | grep -q "macvz-cri-p8"; then
		pass "logs + projected config mount"
	else
		fail "logs/config mount (expected marker not found)"
	fi

	# exec: an exec probe returns its real exit code (liveness/readiness backing).
	if crt exec "$cid" sh -c 'exit 0' 2>>"$OUT_DIR/lifecycle.log"; then
		pass "exec probe (exit 0)"
	else
		fail "exec probe"
	fi

	echo "$cid" >"$OUT_DIR/.container-id"
}

phase_unsupported() {
	log "Phase: unsupported Pod shape diagnostic"
	cat >"$OUT_DIR/hostnet.json" <<EOF
{
  "metadata": {"name": "hostnet", "namespace": "macvz-cri-e2e", "uid": "hostnet-uid-1", "attempt": 0},
  "linux": {"security_context": {"namespace_options": {"network": 2}}}
}
EOF
	# NamespaceMode NODE = 2. The adapter must reject this with a clear diagnostic
	# rather than booting a Pod that silently ignores hostNetwork.
	if crt runp "$OUT_DIR/hostnet.json" >"$OUT_DIR/hostnet.out" 2>&1; then
		fail "hostNetwork Pod was accepted (expected a clear rejection)"
	elif grep -qi "hostNetwork" "$OUT_DIR/hostnet.out"; then
		pass "hostNetwork Pod rejected with a clear diagnostic"
	else
		fail "hostNetwork rejected but without a recognizable diagnostic (see hostnet.out)"
	fi
}

phase_restart() {
	log "Phase: adapter restart recovery"
	[ "$MANAGE" = 1 ] || { skip "restart (MACVZ_CRI_MANAGE=0; adapter not managed here)"; return; }
	local cid
	cid="$(cat "$OUT_DIR/.container-id" 2>/dev/null || true)"
	[ -n "$cid" ] || { skip "restart (no container from lifecycle phase)"; return; }

	stop_adapter
	start_adapter
	if crt ps -a -q 2>>"$OUT_DIR/restart.log" | grep -q "$cid"; then
		pass "container survived adapter restart (state recovered)"
	else
		fail "container missing after restart (state not recovered)"
	fi
}

phase_cleanup() {
	log "Phase: cleanup verification"
	local sb cid
	sb="$(cat "$OUT_DIR/.sandbox-id" 2>/dev/null || true)"
	cid="$(cat "$OUT_DIR/.container-id" 2>/dev/null || true)"
	[ -n "$cid" ] && { crt stop "$cid" >/dev/null 2>&1 || true; crt rm "$cid" >/dev/null 2>&1 || true; }
	[ -n "$sb" ] && { crt stopp "$sb" >/dev/null 2>&1 || true; crt rmp "$sb" >/dev/null 2>&1 || true; }

	# No workload left behind in the CRI view.
	if [ "$(crt ps -a -q 2>/dev/null | wc -l | tr -d ' ')" = "0" ]; then
		pass "no containers remain after cleanup"
	else
		fail "containers remain after cleanup"
	fi
	if [ "$(crt pods -q 2>/dev/null | wc -l | tr -d ' ')" = "0" ]; then
		pass "no sandboxes remain after cleanup"
	else
		fail "sandboxes remain after cleanup"
	fi

	# After stopping the adapter, the socket must not linger.
	stop_adapter
	if [ "$MANAGE" = 1 ]; then
		if [ -S "$SOCKET" ]; then
			fail "stale CRI socket left at $SOCKET after adapter stop"
		else
			pass "no stale CRI socket after adapter stop"
		fi
	elif crt version >/dev/null 2>&1; then
		pass "external CRI socket still serving (MACVZ_CRI_MANAGE=0)"
	else
		fail "external CRI socket stopped serving unexpectedly"
	fi
}

phase_kubectl() {
	if [ -z "${KUBECONFIG:-}" ]; then
		skip "kubectl/k3s phase (KUBECONFIG not set; crictl suite covers the CRI contract)"
		return
	fi
	if ! kubectl get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1; then
		skip "kubectl/k3s phase (cluster unreachable)"
		return
	fi
	log "Phase: kubectl/k3s fixtures"
	skip "kubectl fixture deployment is not automated in CRI-P8; CRI-P9 owns real-cluster Service reachability"
}

# --- main --------------------------------------------------------------------
if [ "$INTEGRATION" != 1 ]; then
	print_plan
	exit 0
fi

setup
# Preflight runs BEFORE the adapter starts: one of its checks is that the CRI
# socket is not already serving, so probing it while our own adapter holds the
# socket would always (falsely) FAIL with "another macvz-cri is running".
phase_preflight
start_adapter

phase_adapter
phase_image
phase_lifecycle
phase_unsupported
phase_restart
phase_cleanup
phase_kubectl

echo
if [ "$FAILURES" -eq 0 ]; then
	pass "CRI-P8 compatibility suite: all phases passed"
	exit 0
fi
fail "CRI-P8 compatibility suite: $FAILURES check(s) failed (diagnostics in $OUT_DIR)"
exit 1
