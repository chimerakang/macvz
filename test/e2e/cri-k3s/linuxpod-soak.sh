#!/usr/bin/env bash
#
# linuxpod-soak.sh — CRI-L6-1 (#135) LinuxPod soak/churn harness for the real
# kubelet/k3s in-loop path.
#
# This is the soak sibling of linuxpod-inloop.sh (CRI-L5 #130). It drives the
# same real kubelet/k3s topology against a macOS MacVz CRI node running
# `macvz-cri --experimental-linuxpod-backend --linuxpod-helper-socket=<sock>`,
# but instead of a single lifecycle pass it LOOPS restarts and lifecycle churn
# long enough to catch recovery regressions (parent #134: LinuxPod production
# recovery and node stability).
#
# Each iteration applies one churn action (round-robin over the enabled modes),
# waits for recovery, and records a per-iteration row: Pod UID, Pod IP,
# restartCount, adapter RSS, helper RSS (if available), residual LinuxPod state
# count, and whether the host default route was preserved. The churn modes:
#
#   rollout  kubectl rollout restart the Deployment; a fresh Pod must come up
#            healthy (new UID is expected — this is the create/delete cycle).
#   cri      restart macvz-cri (MACVZ_RESTART_CRI_CMD); the Pod must stay the
#            SAME UID (re-adopt, no duplicate backend state).
#   helper   restart/crash the LinuxPod helper (MACVZ_RESTART_HELPER_CMD);
#            recovery is live or a bounded kubelet recreate, and exec/logs must
#            work afterward.
#   netd     restart/reload macvz-netd (MACVZ_RESTART_NETD_CMD); the host
#            default route must NOT change and Service/Pod reachability must be
#            preserved or restored.
#
# A mode whose required operator hook is unset is SKIPped loudly and dropped
# from the round-robin — never silently passed.
#
# HONESTY GATE (inherited from #130). A Pod reaching Running on this node is NOT
# by itself evidence of a LinuxPod-backed Pod: the shipped serving path runs on
# apple/container, and the R17 prototype helper reports simulated=true. So the
# LinuxPod-specific soak assertions (helper-restart live/recreate recovery on a
# real backend, residual LinuxPod-VM audit reaching zero) are only enforced once
# MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD proves on the node that the Pod's sandbox
# is served by a genuine, non-simulated LinuxPod backend. Absent that proof the
# soak still runs the kubelet-visible churn (rollout/cri/route) but skips the
# LinuxPod-backend claims loudly with the #127/#128/#129 blocker.
#
# Gating: like linuxpod-inloop.sh, the live soak mutates a real cluster and a
# real macOS CRI node, so it runs only when MACVZ_INTEGRATION=1 *and* a reachable
# KUBECONFIG is provided. Without both it prints the runbook plan and exits 0, so
# it is safe in `go test`-style CI and `bash -n` validation.
#
# It is an isolated `develop`-track feasibility probe, NOT the shipped Virtual
# Kubelet e2e (test/e2e/e2e.sh), and must not gate the VK release path.
#
# Environment:
#   MACVZ_INTEGRATION         set to 1 to run live (otherwise plan-only).
#   KUBECONFIG                kubeconfig for the k3s control plane (required live).
#   MACVZ_NODE                name of the MacVz CRI node. Default: auto-detected as
#                             the (single) node carrying
#                             node.macvz.io/runtime=apple-container.
#   MACVZ_CRI_IMAGE           arm64 image for the fixture (default: busybox:1.36.1).
#   MACVZ_SOAK_ITERATIONS     churn iterations to run (default: 12). Each AC needs
#                             N>=1 iterations of its mode; the default cycles every
#                             enabled mode at least twice.
#   MACVZ_SOAK_CHURN_MODES    comma-separated churn modes to round-robin
#                             (default: rollout,cri,helper,netd). Modes whose hook
#                             is unset are dropped with a loud skip.
#   MACVZ_SOAK_RECOVER_TRIES  recovery polls per iteration (default: 90).
#   MACVZ_SOAK_RECOVER_INTERVAL  seconds between recovery polls (default: 2).
#   MACVZ_SOAK_RSS_GROWTH_KB  allowed adapter RSS growth across the whole soak
#                             before flagging a leak (default: 65536 = 64 MiB).
#                             Needs MACVZ_ADAPTER_RSS_CMD.
#   MACVZ_SOAK_MAX_RESIDUAL   max residual LinuxPod state lines tolerated per
#                             iteration steady state (default: derived from the
#                             post-deploy baseline). Needs MACVZ_LINUXPOD_AUDIT_CMD.
#   MACVZ_CRI_OUT_DIR         results/diagnostics dir (default: a mktemp dir).
#
# Operator hooks (commands run via `sh -c`; the harness cannot reach the remote
# macOS node itself). A churn mode / audit whose required hook is unset is
# SKIPped loudly:
#   MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
#                           REQUIRED to enforce any LinuxPod-backend acceptance.
#                           Prints node-side evidence that the named Pod's sandbox
#                           is served by a real, non-simulated LinuxPod backend.
#                           MACVZ_POD is exported for the hook. simulated=true (or
#                           no LinuxPod sandbox/VM) keeps the LinuxPod claims
#                           skipped with the #127/#128/#129 blocker.
#   MACVZ_RESTART_CRI_CMD   restart the macvz-cri service on the CRI node ('cri').
#   MACVZ_RESTART_HELPER_CMD restart (or crash+restart) the LinuxPod helper ('helper').
#   MACVZ_RESTART_NETD_CMD  restart or reload macvz-netd on the CRI node ('netd').
#   MACVZ_ADAPTER_RSS_CMD   print the adapter's resident memory in KB (one integer).
#   MACVZ_HELPER_RSS_CMD    print the LinuxPod helper's resident memory in KB
#                           (one integer); optional, recorded when present.
#   MACVZ_LINUXPOD_AUDIT_CMD print residual LinuxPod state (VMs, backend
#                           containers, prepared rootfs/handoff subtrees, Pod
#                           network state). Used for per-iteration residual count
#                           and the final zero-residual cleanup audit.
#   MACVZ_ROUTE_AUDIT_CMD   print the node's default route(s). Captured before the
#                           soak and after every iteration; the harness asserts the
#                           default route never changes (non-goal: never mutate it).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
FIXTURE="$HERE/fixtures/linuxpod-workload.yaml"
NS="macvz-cri-linuxpod-soak"
DEPLOY="linuxpod-inloop"
MARKER="macvz-cri-l5-inloop-ok"
APP_BOOT_MARKER="macvz-cri-l5-app-boot"

INTEGRATION="${MACVZ_INTEGRATION:-0}"
IMAGE="${MACVZ_CRI_IMAGE:-busybox:1.36.1}"
NODE="${MACVZ_NODE:-}"
SOAK_ITERS="${MACVZ_SOAK_ITERATIONS:-12}"
CHURN_MODES="${MACVZ_SOAK_CHURN_MODES:-rollout,cri,helper,netd}"
RECOVER_TRIES="${MACVZ_SOAK_RECOVER_TRIES:-90}"
RECOVER_INTERVAL="${MACVZ_SOAK_RECOVER_INTERVAL:-2}"
RSS_GROWTH_KB="${MACVZ_SOAK_RSS_GROWTH_KB:-65536}"
MAX_RESIDUAL="${MACVZ_SOAK_MAX_RESIDUAL:-}"
OUT_DIR="${MACVZ_CRI_OUT_DIR:-}"

RUNTIME_LABEL="node.macvz.io/runtime=apple-container"
TAINT_KEY="node.macvz.io/host-namespace-unsupported"

FAILURES=0
SKIPS=0
TMP_ROOT=""
OUT_DIR_WAS_SET=0
PF_PID=""
LINUXPOD_BACKED=0
ENABLED_MODES=""
BASELINE_RESIDUAL=0
FIRST_RSS=0
LAST_RSS=0
HAVE_RSS=0

c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
pass() { printf "${c_green}PASS${c_off} %s\n" "$*"; }
skip() { printf "${c_yellow}SKIP${c_off} %s\n" "$*"; SKIPS=$((SKIPS+1)); }
fail() { printf "${c_red}FAIL${c_off} [%s] %s\n" "${CURRENT_PHASE:-soak}" "$*"; FAILURES=$((FAILURES+1)); }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

k() { kubectl "$@"; }
kn() { kubectl -n "$NS" "$@"; }

print_plan() {
	cat <<'PLAN'
CRI-L6-1 LinuxPod soak/churn harness (plan; set MACVZ_INTEGRATION=1 and a
reachable KUBECONFIG to run live):

  preflight        kubectl reachable; locate the MacVz CRI node by its runtime
                   label; assert #84 labels + NoSchedule taint present; node Ready.
  route-baseline   capture the node default route(s) (MACVZ_ROUTE_AUDIT_CMD) once;
                   re-asserted unchanged after every churn iteration.
  deploy           kubectl apply fixtures/linuxpod-workload.yaml (app + late
                   sidecar in one Pod, ConfigMap + Secret + ClusterIP Service);
                   kubectl rollout status; record the LinuxPod residual baseline.
  backend-evidence HONESTY GATE: assert via MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
                   that the Pod's sandbox is served by a genuine, non-simulated
                   LinuxPod backend. Without it, LinuxPod-backend soak claims are
                   SKIPPED with the #127/#128/#129 blocker; kubelet-visible churn
                   (rollout/cri/route) still runs.
  churn loop       MACVZ_SOAK_ITERATIONS iterations, round-robin over the enabled
                   MACVZ_SOAK_CHURN_MODES:
                     rollout  rollout restart; fresh Pod comes up healthy.
                     cri      restart macvz-cri; SAME Pod UID, no duplicate state.
                     helper   restart/crash the helper; live recovery or bounded
                              recreate; exec + logs work afterward.
                     netd     restart/reload macvz-netd; default route unchanged
                              and Service/Pod reachability preserved or restored.
                   each iteration records Pod UID, Pod IP, restartCount, adapter
                   RSS, helper RSS, residual LinuxPod-state count, and re-checks
                   the default route is unchanged.
  cleanup          delete the fixture; assert no residual Pods and (LinuxPod
                   audit) zero residual VM/container/rootfs/handoff/network state.
  route-after      re-capture the default route(s); assert unchanged end-to-end.
  summary          per-iteration CSV, adapter RSS growth bound, failed-phase
                   diagnostics paths.

Gated: the live soak drives a real cluster and a real macOS CRI node. LinuxPod
acceptances additionally require the node to run
`macvz-cri --experimental-linuxpod-backend` AND a backend-evidence hook proving
the Pod is genuinely LinuxPod-backed; absent that they skip loudly with the
#127/#128/#129 blocker. Churn/audit phases need operator hooks
(MACVZ_RESTART_CRI_CMD, MACVZ_RESTART_HELPER_CMD, MACVZ_RESTART_NETD_CMD,
MACVZ_ADAPTER_RSS_CMD, MACVZ_HELPER_RSS_CMD, MACVZ_LINUXPOD_AUDIT_CMD,
MACVZ_ROUTE_AUDIT_CMD); an unset hook drops its mode/audit with a loud skip,
never a silent pass. See the header for the full env contract and
test/e2e/cri-k3s/README.md for topology.
PLAN
}

# --- setup / teardown --------------------------------------------------------
setup() {
	command -v kubectl >/dev/null 2>&1 || die "kubectl not found in PATH"
	[ -f "$FIXTURE" ] || die "fixture not found at $FIXTURE"
	k get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1 \
		|| die "cluster unreachable (KUBECONFIG=${KUBECONFIG:-unset}); set a reachable kubeconfig"

	case "$SOAK_ITERS" in (*[!0-9]*|'') die "MACVZ_SOAK_ITERATIONS must be a positive integer (got '$SOAK_ITERS')";; esac
	[ "$SOAK_ITERS" -ge 1 ] || die "MACVZ_SOAK_ITERATIONS must be >= 1 (got '$SOAK_ITERS')"

	TMP_ROOT="$(mktemp -d -t macvz-cri-linuxpod-soak)"
	if [ -n "$OUT_DIR" ]; then
		OUT_DIR_WAS_SET=1
	else
		OUT_DIR="$TMP_ROOT/out"
	fi
	mkdir -p "$OUT_DIR"
	log "out=$OUT_DIR image=$IMAGE iterations=$SOAK_ITERS modes=$CHURN_MODES"
}

cleanup_trap() {
	[ -n "$PF_PID" ] && { kill "$PF_PID" 2>/dev/null || true; }
	if [ "${MACVZ_CRI_KEEP:-0}" != 1 ]; then
		kubectl delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
	fi
	if [ -n "$TMP_ROOT" ] && [ "${MACVZ_CRI_KEEP:-0}" != 1 ] && [ "$OUT_DIR_WAS_SET" = 1 ]; then
		rm -rf "$TMP_ROOT"
	fi
}
trap cleanup_trap EXIT

run_hook() {
	local cmd="$1"; shift
	[ -n "$cmd" ] || return 3
	sh -c "$cmd"
}

run_bounded() {
	local stdout="$1" stderr="$2" timeout="$3"; shift 3
	: >"$stdout"
	: >"$stderr"
	"$@" >"$stdout" 2>"$stderr" &
	local pid=$! deadline=$((SECONDS + timeout)) rc=0
	while kill -0 "$pid" 2>/dev/null; do
		if [ "$SECONDS" -ge "$deadline" ]; then
			kill "$pid" 2>/dev/null || true
			sleep 1
			kill -9 "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
			printf 'command timed out after %ss\n' "$timeout" >>"$stderr"
			return 124
		fi
		sleep 1
	done
	wait "$pid" || rc=$?
	return "$rc"
}

# --- shared helpers ----------------------------------------------------------
pod_name() {
	local pod
	pod="$(kn get pods -l app=linuxpod-inloop --field-selector=status.phase=Running \
		--sort-by=.metadata.creationTimestamp -o name 2>/dev/null | tail -n 1 | sed 's#^pod/##')"
	if [ -z "$pod" ]; then
		pod="$(kn get pods -l app=linuxpod-inloop \
			--sort-by=.metadata.creationTimestamp -o name 2>/dev/null | tail -n 1 | sed 's#^pod/##')"
	fi
	printf '%s' "$pod"
}
pod_uid()   { kn get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null; }
pod_phase() { kn get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
pod_ip()    { kn get pod "$1" -o jsonpath='{.status.podIP}' 2>/dev/null; }
pod_restart_count() {
	local rc; rc="$(kn get pod "$1" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null)"
	[ -n "$rc" ] || rc=0
	printf '%s' "$rc"
}

# wait_running <try-tag> -> echoes the name of a Running Pod, or empty on timeout.
wait_running() {
	local tag="$1" pod="" _
	for _ in $(seq 1 "$RECOVER_TRIES"); do
		pod="$(pod_name)"
		if [ -n "$pod" ] && [ "$(pod_phase "$pod")" = "Running" ]; then
			printf '%s' "$pod"
			return 0
		fi
		sleep "$RECOVER_INTERVAL"
	done
	printf '%s' "$pod"
	return 1
}

# adapter_rss -> echoes adapter RSS in KB (0 if unavailable). Pure reader: the
# caller updates the FIRST/LAST/HAVE_RSS trackers, because this runs inside `$()`
# (a subshell) where global mutations would be lost.
adapter_rss() {
	local raw rss=0
	if [ -n "${MACVZ_ADAPTER_RSS_CMD:-}" ]; then
		if raw="$(run_hook "$MACVZ_ADAPTER_RSS_CMD" 2>/dev/null)"; then
			rss="$(printf '%s' "$raw" | tr -dc '0-9')"
			[ -n "$rss" ] || rss=0
		fi
	fi
	printf '%s' "$rss"
}

# helper_rss -> echoes helper RSS in KB, or -1 when the hook is unset/failed.
helper_rss() {
	local raw rss=-1
	if [ -n "${MACVZ_HELPER_RSS_CMD:-}" ]; then
		if raw="$(run_hook "$MACVZ_HELPER_RSS_CMD" 2>/dev/null)"; then
			rss="$(printf '%s' "$raw" | tr -dc '0-9')"
			[ -n "$rss" ] || rss=-1
		fi
	fi
	printf '%s' "$rss"
}

# linuxpod_state_count <output-file> -> writes raw audit; prints residual-state
# line count. Return 2 on hook failure (distinct from a clean zero), 3 if unset.
linuxpod_state_count() {
	local out_file="$1" raw
	[ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ] || return 3
	if ! raw="$(run_hook "$MACVZ_LINUXPOD_AUDIT_CMD" 2>"$out_file.err")"; then
		printf '%s\n' "$raw" >"$out_file"
		return 2
	fi
	printf '%s\n' "$raw" >"$out_file"
	printf '%s\n' "$raw" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' '
}

# residual_for_iter <output-file> -> echoes residual count, or -1 unset / -2 fail.
residual_for_iter() {
	local out_file="$1" n
	if n="$(linuxpod_state_count "$out_file")"; then
		printf '%s' "$n"
	else
		case "$?" in
			3) printf '%s' "-1" ;;
			*) printf '%s' "-2" ;;
		esac
	fi
}

# wait_residual_at_most <output-file> <max> -> echoes the settled residual count.
# Kubelet can leave a stopped sandbox JSON around briefly after rollout/delete
# before it issues RemovePodSandbox. That is not duplicate live backend state, so
# duplicate checks wait for the CRI state to converge before failing.
wait_residual_at_most() {
	local out_file="$1" max="$2" n _
	for _ in $(seq 1 "$RECOVER_TRIES"); do
		n="$(residual_for_iter "$out_file")"
		case "$n" in
			-2|-1)
				printf '%s' "$n"
				return 0 ;;
			*)
				if [ "$n" -le "$max" ]; then
					printf '%s' "$n"
					return 0
				fi ;;
		esac
		sleep "$RECOVER_INTERVAL"
	done
	printf '%s' "$n"
	return 1
}

# route_unchanged <label> -> 0 if the default route matches the baseline.
route_unchanged() {
	local label="$1"
	[ -n "${MACVZ_ROUTE_AUDIT_CMD:-}" ] || return 0
	if ! run_hook "$MACVZ_ROUTE_AUDIT_CMD" >"$OUT_DIR/route-$label.txt" 2>"$OUT_DIR/route-$label.err"; then
		fail "MACVZ_ROUTE_AUDIT_CMD failed at $label (see $OUT_DIR/route-$label.err)"
		return 1
	fi
	if [ -f "$OUT_DIR/route-baseline.txt" ] \
		&& diff -u "$OUT_DIR/route-baseline.txt" "$OUT_DIR/route-$label.txt" >"$OUT_DIR/route-$label.diff" 2>&1; then
		return 0
	fi
	return 1
}

# reachable -> 0 if the served marker is reachable via port-forward.
reachable() {
	local pod lport=18091 ok=0 _; pod="$(pod_name)"
	[ -n "$pod" ] || return 1
	kn port-forward "pod/$pod" "$lport:8080" >"$OUT_DIR/pf-$1.log" 2>&1 &
	PF_PID="$!"
	for _ in $(seq 1 30); do
		if curl -fsS "http://127.0.0.1:$lport/index.html" 2>/dev/null | grep -q "$MARKER"; then ok=1; break; fi
		sleep 0.5
	done
	kill "$PF_PID" 2>/dev/null || true; wait "$PF_PID" 2>/dev/null || true; PF_PID=""
	[ "$ok" = 1 ]
}

# exec_logs_ok <pod> <tag> -> 0 if exec serves the marker and logs show the boot
# markers (proves the Pod is genuinely usable, not Running-but-dead).
exec_logs_ok() {
	local pod="$1" tag="$2" out
	run_bounded "$OUT_DIR/$tag-exec.out" "$OUT_DIR/$tag-exec.err" 20 \
		kn exec "$pod" -c app -- sh -c 'cat /www/index.html 2>/dev/null' || return 1
	out="$(cat "$OUT_DIR/$tag-exec.out")"
	printf '%s' "$out" | grep -q "$MARKER" || return 1
	run_bounded "$OUT_DIR/$tag-logs.out" "$OUT_DIR/$tag-logs.err" 20 \
		kn logs "$pod" -c app || return 1
	grep -q "$APP_BOOT_MARKER" "$OUT_DIR/$tag-logs.out" || return 1
	return 0
}

# wait_exec_logs_ok <tag> -> echoes the usable Pod name once exec/logs recover.
# Helper restart can leave the old Pod phase as Running while kubelet is still
# reconciling a recreated sandbox, so phase alone is not a readiness signal here.
wait_exec_logs_ok() {
	local tag="$1" pod="" _
	for _ in $(seq 1 "$RECOVER_TRIES"); do
		pod="$(pod_name)"
		if [ -n "$pod" ] && [ "$(pod_phase "$pod")" = "Running" ] && exec_logs_ok "$pod" "$tag"; then
			printf '%s' "$pod"
			return 0
		fi
		sleep "$RECOVER_INTERVAL"
	done
	printf '%s' "$pod"
	return 1
}

# --- phases ------------------------------------------------------------------
phase_preflight() {
	CURRENT_PHASE="preflight"
	log "Phase: preflight (locate + validate the MacVz CRI node)"
	if [ -z "$NODE" ]; then
		local nodes count
		nodes="$(k get nodes -l "$RUNTIME_LABEL" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)"
		count="$(printf '%s\n' "$nodes" | sed '/^$/d' | wc -l | tr -d ' ')"
		if [ "$count" -gt 1 ]; then
			fail "multiple nodes carry label $RUNTIME_LABEL; set MACVZ_NODE explicitly"
			return 1
		fi
		NODE="$(printf '%s\n' "$nodes" | sed '/^$/d' | head -n 1)"
	fi
	if [ -z "$NODE" ]; then
		fail "no node carries label $RUNTIME_LABEL (register the MacVz node per README #84)"
		return 1
	fi
	pass "MacVz CRI node: $NODE"

	local failures_before="$FAILURES" rt hns
	rt="$(k get node "$NODE" -o jsonpath='{.metadata.labels.node\.macvz\.io/runtime}' 2>/dev/null)"
	hns="$(k get node "$NODE" -o jsonpath='{.metadata.labels.node\.macvz\.io/host-namespace}' 2>/dev/null)"
	if [ "$rt" = "apple-container" ]; then
		pass "runtime label"
	else
		fail "runtime label (got '$rt')"
	fi
	if [ "$hns" = "unsupported" ]; then
		pass "host-namespace label"
	else
		fail "host-namespace label (got '$hns')"
	fi

	if k get node "$NODE" -o jsonpath="{.spec.taints[?(@.key=='$TAINT_KEY')].effect}" 2>/dev/null | grep -q NoSchedule; then
		pass "host-namespace NoSchedule taint"
	else
		fail "missing $TAINT_KEY NoSchedule taint (register the node per README #84)"
	fi

	if k get node "$NODE" -o jsonpath="{.status.conditions[?(@.type=='Ready')].status}" 2>/dev/null | grep -q True; then
		pass "node Ready"
	else
		fail "node $NODE is not Ready"
	fi
	[ "$FAILURES" = "$failures_before" ]
}

phase_route_baseline() {
	CURRENT_PHASE="route-baseline"
	log "Phase: default-route baseline"
	if [ -z "${MACVZ_ROUTE_AUDIT_CMD:-}" ]; then
		skip "route-baseline (set MACVZ_ROUTE_AUDIT_CMD to capture/compare the node default route every iteration)"
		return 0
	fi
	if run_hook "$MACVZ_ROUTE_AUDIT_CMD" >"$OUT_DIR/route-baseline.txt" 2>"$OUT_DIR/route-baseline.err"; then
		pass "captured node default route baseline ($OUT_DIR/route-baseline.txt)"
	else
		fail "MACVZ_ROUTE_AUDIT_CMD failed at baseline (see $OUT_DIR/route-baseline.err)"
	fi
}

apply_fixture() {
	sed -e "s#image: busybox:1.36.1#image: $IMAGE#g" \
		-e "s#macvz-cri-linuxpod-e2e#$NS#g" \
		"$FIXTURE" >"$OUT_DIR/workload.applied.yaml"
	k apply -f "$OUT_DIR/workload.applied.yaml" >"$OUT_DIR/apply.log" 2>&1
}

phase_deploy() {
	CURRENT_PHASE="deploy"
	log "Phase: deploy app+late-sidecar fixture + rollout"
	if ! apply_fixture; then
		fail "kubectl apply failed (see $OUT_DIR/apply.log)"
		return 1
	fi
	pass "fixture applied"
	if kn rollout status "deploy/$DEPLOY" --timeout=5m >"$OUT_DIR/rollout.log" 2>&1; then
		pass "kubectl rollout status (Deployment available)"
	else
		fail "rollout did not complete (see $OUT_DIR/rollout.log)"
		kn describe "deploy/$DEPLOY" >"$OUT_DIR/deploy-describe.log" 2>&1 || true
		kn get pods -o wide >"$OUT_DIR/pods.log" 2>&1 || true
		return 1
	fi
	local pod node; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no fixture Pod after rollout"; return 1; }
	node="$(kn get pod "$pod" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
	if [ "$node" = "$NODE" ]; then
		pass "Pod $pod scheduled onto MacVz node $NODE"
	else
		fail "Pod scheduled onto '$node', expected MacVz node '$NODE'"
	fi

	# Establish the steady-state LinuxPod residual baseline (one Pod's worth of
	# state); per-iteration counts above this flag a leak/duplicate.
	if [ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ]; then
		local n
		if n="$(linuxpod_state_count "$OUT_DIR/residual-baseline.log")"; then
			BASELINE_RESIDUAL="$n"
			[ -n "$MAX_RESIDUAL" ] || MAX_RESIDUAL="$n"
			pass "LinuxPod residual baseline: $BASELINE_RESIDUAL line(s) (per-iter ceiling $MAX_RESIDUAL)"
		else
			fail "LinuxPod audit hook failed at baseline (see $OUT_DIR/residual-baseline.log.err)"
		fi
	fi
}

phase_backend_evidence() {
	CURRENT_PHASE="backend-evidence"
	log "Phase: LinuxPod backend evidence (honesty gate)"
	if [ -z "${MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD:-}" ]; then
		skip "backend-evidence: set MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD to prove the Pod is LinuxPod-backed; LinuxPod-backend soak claims will be skipped (blocked on CRI-L serving #127/#128/#129)"
		return 0
	fi
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for backend evidence"; return 1; }
	if ! MACVZ_POD="$pod" run_hook "$MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD" \
		>"$OUT_DIR/backend-evidence.txt" 2>"$OUT_DIR/backend-evidence.err"; then
		fail "MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD failed (see $OUT_DIR/backend-evidence.err)"
		return 1
	fi
	if grep -Eqi 'simulated[":= ]+true' "$OUT_DIR/backend-evidence.txt"; then
		skip "backend-evidence reports simulated=true: node is on the R17 prototype handshake, not a real LinuxPod backend — LinuxPod claims blocked on #127/#128/#129 (see $OUT_DIR/backend-evidence.txt)"
		return 0
	fi
	if grep -Eqi 'linuxpod|pod[-_ ]?vm|sandboxVM' "$OUT_DIR/backend-evidence.txt" \
		&& grep -Eqi 'simulated[":= ]+false|backend[":= ]+linuxpod|serving[":= ]+linuxpod' "$OUT_DIR/backend-evidence.txt"; then
		LINUXPOD_BACKED=1
		pass "Pod is served by a genuine (non-simulated) LinuxPod backend (see $OUT_DIR/backend-evidence.txt)"
	else
		skip "backend-evidence did not prove a non-simulated LinuxPod-backed Pod; LinuxPod claims blocked on #127/#128/#129 (see $OUT_DIR/backend-evidence.txt)"
	fi
}

# resolve_modes filters MACVZ_SOAK_CHURN_MODES down to the modes whose required
# hook is present, dropping the rest with a loud skip.
resolve_modes() {
	CURRENT_PHASE="modes"
	local mode
	local IFS=','
	for mode in $CHURN_MODES; do
		mode="$(printf '%s' "$mode" | tr -d '[:space:]')"
		[ -n "$mode" ] || continue
		case "$mode" in
			rollout)
				ENABLED_MODES="$ENABLED_MODES rollout" ;;
			cri)
				if [ -n "${MACVZ_RESTART_CRI_CMD:-}" ]; then ENABLED_MODES="$ENABLED_MODES cri"
				else skip "churn mode 'cri' dropped (set MACVZ_RESTART_CRI_CMD); AC 'cri restart preserves UID' not exercised"; fi ;;
			helper)
				if [ -n "${MACVZ_RESTART_HELPER_CMD:-}" ]; then ENABLED_MODES="$ENABLED_MODES helper"
				else skip "churn mode 'helper' dropped (set MACVZ_RESTART_HELPER_CMD); AC 'helper restart recovers' not exercised"; fi ;;
			netd)
				if [ -n "${MACVZ_RESTART_NETD_CMD:-}" ]; then ENABLED_MODES="$ENABLED_MODES netd"
				else skip "churn mode 'netd' dropped (set MACVZ_RESTART_NETD_CMD); AC 'netd reload preserves default route' not exercised"; fi ;;
			*)
				skip "unknown churn mode '$mode' ignored (valid: rollout,cri,helper,netd)" ;;
		esac
	done
	ENABLED_MODES="$(printf '%s' "$ENABLED_MODES" | sed 's/^ *//')"
	if [ -z "$ENABLED_MODES" ]; then
		fail "no usable churn modes (set at least one restart hook or keep 'rollout')"
		return 1
	fi
	log "enabled churn modes: $ENABLED_MODES"
}

# --- per-iteration churn actions ---------------------------------------------
# Each returns 0 on success and records its own pass/fail; the loop handles the
# common per-iteration record (UID/IP/RSS/residual/route).

churn_rollout() {
	local pod; pod="$(pod_name)"
	kn rollout restart "deploy/$DEPLOY" >"$OUT_DIR/iter-$ITER-rollout.log" 2>&1 \
		|| { fail "rollout restart returned non-zero (see $OUT_DIR/iter-$ITER-rollout.log)"; return 1; }
	if ! kn rollout status "deploy/$DEPLOY" --timeout=5m >>"$OUT_DIR/iter-$ITER-rollout.log" 2>&1; then
		fail "rollout did not complete (see $OUT_DIR/iter-$ITER-rollout.log)"
		return 1
	fi
	local pod_after
	if pod_after="$(wait_exec_logs_ok "iter-$ITER-rollout")" && [ -n "$pod_after" ]; then
		pass "rollout: fresh Pod $pod_after healthy (exec+logs serve the marker)"
	else
		fail "rollout: new Pod not usable after restart (see $OUT_DIR/iter-$ITER-rollout-exec.err and $OUT_DIR/iter-$ITER-rollout-logs.err)"
	fi
}

churn_cri() {
	local pod uid_before uid_after; pod="$(pod_name)"
	uid_before="$(pod_uid "$pod")"
	run_hook "$MACVZ_RESTART_CRI_CMD" >"$OUT_DIR/iter-$ITER-cri.log" 2>&1 \
		|| { fail "MACVZ_RESTART_CRI_CMD returned non-zero (see $OUT_DIR/iter-$ITER-cri.log)"; return 1; }
	local pod_after; pod_after="$(wait_running "cri-$ITER")"
	[ -n "$pod_after" ] || { fail "Pod not Running after macvz-cri restart"; return 1; }
	uid_after="$(pod_uid "$pod_after")"
	if [ -n "$uid_before" ] && [ "$uid_before" = "$uid_after" ]; then
		pass "cri: Pod UID unchanged after restart (re-adopted, no recreate)"
	else
		fail "cri: Pod UID changed ($uid_before -> $uid_after): possible duplicate/lost workload"
	fi
	# No duplicate backend state: residual must not exceed one Pod's baseline.
	if [ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ]; then
		local n; n="$(wait_residual_at_most "$OUT_DIR/iter-$ITER-cri-audit.log" "$MAX_RESIDUAL")"
		case "$n" in
			-2) fail "cri: LinuxPod audit hook failed (see $OUT_DIR/iter-$ITER-cri-audit.log.err)" ;;
			-1) : ;;
			*)
				if [ "$n" -le "$MAX_RESIDUAL" ]; then
					pass "cri: no duplicate backend state ($n <= $MAX_RESIDUAL residual line(s))"
				else
					fail "cri: residual LinuxPod state $n > baseline ceiling $MAX_RESIDUAL (duplicate backend state; see $OUT_DIR/iter-$ITER-cri-audit.log)"
				fi ;;
		esac
	fi
}

churn_helper() {
	if [ "$LINUXPOD_BACKED" != 1 ]; then
		skip "helper (iter $ITER): node not proven LinuxPod-backed, a helper restart exercises nothing real (blocked on #127/#128/#129)"
		return 0
	fi
	local pod uid_before; pod="$(pod_name)"
	uid_before="$(pod_uid "$pod")"
	run_hook "$MACVZ_RESTART_HELPER_CMD" >"$OUT_DIR/iter-$ITER-helper.log" 2>&1 \
		|| { fail "MACVZ_RESTART_HELPER_CMD returned non-zero (see $OUT_DIR/iter-$ITER-helper.log)"; return 1; }
	local pod_after; pod_after="$(wait_running "helper-$ITER")"
	[ -n "$pod_after" ] || { fail "helper: Pod not Running after helper restart (record failure handling as next blocker; see $OUT_DIR/iter-$ITER-helper.log)"; return 1; }
	local uid_after; uid_after="$(pod_uid "$pod_after")"
	if [ "$uid_before" = "$uid_after" ]; then
		log "helper: recovery was live (same Pod UID $uid_after)"
	else
		log "helper: recovery was a bounded kubelet recreate ($uid_before -> $uid_after)"
	fi
	if pod_after="$(wait_exec_logs_ok "iter-$ITER-helper")"; then
		pass "helper: exec + logs work after restart (live recovery or bounded recreate)"
	else
		fail "helper: exec/logs did not recover after restart (Running-but-unusable; see $OUT_DIR/iter-$ITER-helper-exec.err)"
	fi
}

churn_netd() {
	run_hook "$MACVZ_RESTART_NETD_CMD" >"$OUT_DIR/iter-$ITER-netd.log" 2>&1 \
		|| { fail "MACVZ_RESTART_NETD_CMD returned non-zero (see $OUT_DIR/iter-$ITER-netd.log)"; return 1; }
	# The non-goal: a netd restart/reload must never mutate the host default route.
	if [ -n "${MACVZ_ROUTE_AUDIT_CMD:-}" ]; then
		if route_unchanged "iter-$ITER-netd"; then
			pass "netd: default route unchanged across restart/reload"
		else
			fail "netd: default route changed across restart/reload (see $OUT_DIR/route-iter-$ITER-netd.diff) — netd must never mutate default routes"
		fi
	else
		skip "netd: default-route guard skipped (set MACVZ_ROUTE_AUDIT_CMD)"
	fi
	# Reachability must be preserved or restored.
	if reachable "iter-$ITER-netd"; then
		pass "netd: Service/Pod reachability restored after restart/reload"
	else
		fail "netd: reachability not restored after restart/reload (see $OUT_DIR/pf-iter-$ITER-netd.log)"
	fi
}

phase_churn_loop() {
	log "Phase: churn loop ($SOAK_ITERS iterations over [$ENABLED_MODES])"
	printf 'iteration,mode,pod_uid,pod_ip,restart_count,adapter_rss_kb,helper_rss_kb,residual,route_ok\n' >"$OUT_DIR/soak-samples.csv"

	# shellcheck disable=SC2086
	set -- $ENABLED_MODES
	local nmodes=$#
	local mode pod uid ip rc rss hrss residual route_ok
	for ITER in $(seq 1 "$SOAK_ITERS"); do
		# round-robin: 1-based index into the enabled-mode list.
		local idx=$(( (ITER - 1) % nmodes + 1 ))
		eval "mode=\${$idx}"
		CURRENT_PHASE="iter-$ITER-$mode"
		log "iteration $ITER/$SOAK_ITERS: churn mode '$mode'"

		case "$mode" in
			rollout) churn_rollout ;;
			cri)     churn_cri ;;
			helper)  churn_helper ;;
			netd)    churn_netd ;;
		esac

		# Per-iteration record (after recovery).
		pod="$(pod_name)"
		uid="$(pod_uid "$pod")"; [ -n "$uid" ] || uid="-"
		ip="$(pod_ip "$pod")"; [ -n "$ip" ] || ip="-"
		rc="$(pod_restart_count "$pod")"
		rss="$(adapter_rss)"
		if [ "$rss" != 0 ]; then
			HAVE_RSS=1
			[ "$FIRST_RSS" = 0 ] && FIRST_RSS="$rss"
			LAST_RSS="$rss"
		fi
		hrss="$(helper_rss)"
		if [ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ]; then
			residual="$(residual_for_iter "$OUT_DIR/iter-$ITER-record-audit.log")"
		else
			residual="-1"
		fi

		# Every iteration re-asserts the default route is unchanged from baseline.
		if [ -n "${MACVZ_ROUTE_AUDIT_CMD:-}" ]; then
			if route_unchanged "iter-$ITER"; then route_ok="yes"
			else route_ok="no"; fail "iter $ITER ($mode): default route changed (see $OUT_DIR/route-iter-$ITER.diff)"; fi
		else
			route_ok="-"
		fi

		printf '%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
			"$ITER" "$mode" "$uid" "$ip" "$rc" "$rss" "$hrss" "$residual" "$route_ok" >>"$OUT_DIR/soak-samples.csv"
		log "iter $ITER recorded: mode=$mode uid=$uid ip=$ip restarts=$rc rss=${rss}KB helper_rss=${hrss}KB residual=$residual route=$route_ok"
	done
	CURRENT_PHASE="churn-loop"

	log "soak samples: $OUT_DIR/soak-samples.csv"
	if [ "$HAVE_RSS" = 1 ]; then
		local growth=$((LAST_RSS - FIRST_RSS))
		log "adapter RSS: first=${FIRST_RSS}KB last=${LAST_RSS}KB growth=${growth}KB (limit ${RSS_GROWTH_KB}KB)"
		if [ "$growth" -le "$RSS_GROWTH_KB" ]; then
			pass "adapter RSS growth within bound across the soak"
		else
			fail "adapter RSS grew ${growth}KB (> ${RSS_GROWTH_KB}KB) across the soak: possible leak"
		fi
	else
		skip "adapter RSS trend (set MACVZ_ADAPTER_RSS_CMD); samples recorded with rss=0"
	fi
}

phase_cleanup() {
	CURRENT_PHASE="cleanup"
	log "Phase: cleanup + residual LinuxPod state audit"
	k delete -f "$OUT_DIR/workload.applied.yaml" --wait=true --timeout=3m >"$OUT_DIR/cleanup.log" 2>&1 || true
	local remaining
	remaining="$(kn get pods -l app=linuxpod-inloop -o name 2>/dev/null | wc -l | tr -d ' ')"
	if [ "$remaining" = 0 ]; then
		pass "no fixture Pods remain after delete"
	else
		fail "$remaining fixture Pod(s) remain after delete"
	fi

	if [ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ]; then
		local n
		if n="$(wait_residual_at_most "$OUT_DIR/cleanup-audit.log" 0)"; then
			if [ "$n" = 0 ]; then
				pass "LinuxPod audit: zero residual VM/container/rootfs/handoff/network state after soak"
			else
				fail "LinuxPod audit: $n residual state line(s) remain after soak (see $OUT_DIR/cleanup-audit.log)"
			fi
		else
			fail "LinuxPod audit hook failed during cleanup (see $OUT_DIR/cleanup-audit.log.err)"
		fi
	else
		skip "residual LinuxPod audit (set MACVZ_LINUXPOD_AUDIT_CMD to assert zero residual state)"
	fi
}

phase_route_after() {
	CURRENT_PHASE="route-after"
	log "Phase: default-route audit (after, end-to-end)"
	if [ -z "${MACVZ_ROUTE_AUDIT_CMD:-}" ]; then
		skip "route-after (set MACVZ_ROUTE_AUDIT_CMD)"
		return 0
	fi
	if route_unchanged "after"; then
		pass "node default route(s) unchanged across the whole soak (non-goal honored)"
	else
		fail "node default route(s) changed across the soak (see $OUT_DIR/route-after.diff) — the soak must never mutate default routes"
	fi
}

# --- main --------------------------------------------------------------------
CURRENT_PHASE="init"
if [ "$INTEGRATION" != 1 ] || [ -z "${KUBECONFIG:-}" ]; then
	print_plan
	exit 0
fi

setup
phase_preflight || die "preflight failed; not deploying onto an unverified node"
phase_route_baseline
phase_deploy || die "deploy failed; nothing to soak"
phase_backend_evidence
resolve_modes || die "no churn modes to run"
phase_churn_loop
phase_cleanup
phase_route_after

echo
if [ "$FAILURES" -eq 0 ] && [ "$SKIPS" -eq 0 ]; then
	pass "CRI-L6-1 LinuxPod soak: all checks passed over $SOAK_ITERS iterations (diagnostics in $OUT_DIR)"
	exit 0
fi
if [ "$FAILURES" -eq 0 ]; then
	pass "CRI-L6-1 LinuxPod soak: checks passed with $SKIPS skipped (LinuxPod recovery acceptance is NOT complete while skips remain; see #127/#128/#129). Diagnostics in $OUT_DIR"
	exit 0
fi
fail "CRI-L6-1 LinuxPod soak: $FAILURES check(s) failed over $SOAK_ITERS iterations (diagnostics in $OUT_DIR)"
exit 1
