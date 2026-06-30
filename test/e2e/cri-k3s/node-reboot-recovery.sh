#!/usr/bin/env bash
#
# node-reboot-recovery.sh — CRI-L8-5 (#144) node reboot / bootstrap recovery
# validation for the LinuxPod-backed k3s node.
#
# This is the recovery sibling of linuxpod-soak.sh (CRI-L6-1 #135). Where the
# soak loops service-level churn while the rest of the stack stays up, this
# harness proves the test Mac can go through a *full restart of the node stack*
# — a remote reboot or an ordered service-stack restart — and return the
# LinuxPod-backed k3s node to a known-good `Ready` state WITHOUT manual cleanup
# of kubelet/CRI/helper/netd leftovers.
#
# It exercises one or more recovery scenarios (MACVZ_RECOVERY_SCENARIOS):
#
#   services  restart the whole node service stack in the documented startup
#             order via MACVZ_BOOTSTRAP_CMD, without rebooting the host. The node
#             must return Ready and the workload must come back usable.
#   reboot    reboot the remote Mac via MACVZ_REBOOT_CMD (which must block until
#             the host is reachable again), then bring the stack up via
#             MACVZ_BOOTSTRAP_CMD. A reboot does not preserve live Pod VMs, so a
#             *fresh* healthy Pod — not a re-adopted one — is the expected and
#             accepted outcome; what must NOT happen is stale helper sockets,
#             supervisor journals, VM state, or kubelet sandbox records blocking
#             that fresh run.
#
# Documented startup order (the contract MACVZ_BOOTSTRAP_CMD must honor, and the
# order MACVZ_STARTUP_PROBE_CMD is expected to report):
#
#   1. apple/container   per-user container system (`container system start`).
#   2. macvz-netd        privileged network helper (pf/route/wg) — must be up
#                        before the adapter so podNetwork rules can be applied.
#   3. linuxpod-helper   the #139 router; per-Pod supervisors do not survive a
#                        reboot, so journaled-but-dead pods come back as lost and
#                        are recreated, never re-adopted.
#   4. macvz-cri         the adapter (`--experimental-linuxpod-backend`).
#   5. kubelet / k3s     the agent pointed at the adapter endpoint; node Ready.
#   6. kind socket fwd   the test-topology forward the local kind control plane
#                        uses to reach the remote adapter (see README).
#
# RECOVERY GUARANTEES asserted per scenario (#144 acceptance):
#   - the scripted bring-up returns the node to Ready and the workload to a
#     usable Running Pod (exec + logs serve the marker);
#   - no stale helper sockets / supervisor journals / VM state / kubelet sandbox
#     records remain (MACVZ_STALE_STATE_CMD audit settles to zero);
#   - the route guard passes before AND after recovery, and the remote default
#     route stays the expected gateway via the expected interface
#     (MACVZ_EXPECTED_DEFAULT_GW via MACVZ_EXPECTED_DEFAULT_IF, default
#     192.168.1.1 via en0) — the recovery must never mutate it.
#
# HONESTY GATE (inherited from #130/#135). A Pod reaching Running on this node is
# NOT by itself evidence of a LinuxPod-backed Pod: the shipped serving path runs
# on apple/container, and the R17 prototype helper reports simulated=true. So the
# LinuxPod-specific recovery assertions (stale LinuxPod-VM/supervisor audit
# reaching zero) are only enforced once MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
# proves on the node that the Pod's sandbox is served by a genuine, non-simulated
# LinuxPod backend. Absent that proof the harness still runs the kubelet-visible
# recovery (node Ready, workload usable, route guard) but skips the
# LinuxPod-backend stale-state claim loudly with the #127/#128/#129 blocker.
#
# Gating: like linuxpod-soak.sh, the live recovery check reboots/restarts a real
# macOS node and drives a real cluster, so it runs only when MACVZ_INTEGRATION=1
# *and* a reachable KUBECONFIG is provided. Without both it prints the runbook
# plan and exits 0, so it is safe in `go test`-style CI and `bash -n` validation.
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
#   MACVZ_RECOVERY_SCENARIOS  comma-separated, ordered scenarios to run
#                             (default: services,reboot). A scenario whose
#                             required hook is unset is dropped with a loud skip.
#   MACVZ_RECOVER_TRIES       recovery polls per wait (default: 150).
#   MACVZ_RECOVER_INTERVAL    seconds between recovery polls (default: 2).
#   MACVZ_EXPECTED_DEFAULT_GW expected default-route gateway (default 192.168.1.1).
#   MACVZ_EXPECTED_DEFAULT_IF expected default-route interface (default en0).
#   MACVZ_CRI_OUT_DIR         results/diagnostics dir (default: a mktemp dir).
#
# Operator hooks (commands run via `sh -c`; the harness cannot reach the remote
# macOS node itself). A scenario / audit whose required hook is unset is SKIPped
# loudly, never silently passed:
#   MACVZ_BOOTSTRAP_CMD       REQUIRED for any scenario. Brings the node stack up
#                             in the documented startup order, cleaning stale
#                             helper sockets / supervisor journals / stale VM and
#                             kubelet sandbox state FIRST, while preserving the
#                             host default route. `hooks/node-bootstrap.sh` is a
#                             reference implementation.
#   MACVZ_REBOOT_CMD          REQUIRED for the 'reboot' scenario. Reboots the
#                             remote Mac and BLOCKS until it is reachable again
#                             (e.g. SSH answers). Unset drops 'reboot' loudly.
#   MACVZ_STARTUP_PROBE_CMD   optional. Prints per-component readiness in startup
#                             order (one `component: state` line per component);
#                             recorded and, when present, asserted all-ready.
#   MACVZ_STALE_STATE_CMD     optional but REQUIRED to assert the no-leftover
#                             acceptance. Prints residual stale state lines
#                             (helper sockets, supervisor journals, VM state,
#                             kubelet sandbox records) that would block a fresh
#                             run; asserted to settle to zero after bootstrap.
#   MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
#                             REQUIRED to enforce the LinuxPod-backend stale-state
#                             claim. Prints node-side evidence that the named
#                             Pod's sandbox is served by a real, non-simulated
#                             LinuxPod backend. MACVZ_POD is exported for the hook.
#   MACVZ_ROUTE_AUDIT_CMD     prints the node's default route(s). Captured before
#                             the run and after every scenario; the harness
#                             asserts it never changes and matches the expected
#                             gateway/interface (non-goal: never mutate it).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
FIXTURE="$HERE/fixtures/linuxpod-workload.yaml"
NS="macvz-cri-linuxpod-reboot"
DEPLOY="linuxpod-inloop"
MARKER="macvz-cri-l5-inloop-ok"
APP_BOOT_MARKER="macvz-cri-l5-app-boot"

INTEGRATION="${MACVZ_INTEGRATION:-0}"
IMAGE="${MACVZ_CRI_IMAGE:-busybox:1.36.1}"
NODE="${MACVZ_NODE:-}"
SCENARIOS="${MACVZ_RECOVERY_SCENARIOS:-services,reboot}"
RECOVER_TRIES="${MACVZ_RECOVER_TRIES:-150}"
RECOVER_INTERVAL="${MACVZ_RECOVER_INTERVAL:-2}"
EXPECT_GW="${MACVZ_EXPECTED_DEFAULT_GW:-192.168.1.1}"
EXPECT_IF="${MACVZ_EXPECTED_DEFAULT_IF:-en0}"
OUT_DIR="${MACVZ_CRI_OUT_DIR:-}"

RUNTIME_LABEL="node.macvz.io/runtime=apple-container"
TAINT_KEY="node.macvz.io/host-namespace-unsupported"

FAILURES=0
SKIPS=0
TMP_ROOT=""
OUT_DIR_WAS_SET=0
PF_PID=""
LINUXPOD_BACKED=0
ENABLED_SCENARIOS=""
CURRENT_PHASE="init"

c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
pass() { printf "${c_green}PASS${c_off} %s\n" "$*"; }
skip() { printf "${c_yellow}SKIP${c_off} %s\n" "$*"; SKIPS=$((SKIPS+1)); }
fail() { printf "${c_red}FAIL${c_off} [%s] %s\n" "${CURRENT_PHASE:-recovery}" "$*"; FAILURES=$((FAILURES+1)); }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

k() { kubectl "$@"; }
kn() { kubectl -n "$NS" "$@"; }

print_plan() {
	cat <<'PLAN'
CRI-L8-5 node reboot / bootstrap recovery harness (plan; set MACVZ_INTEGRATION=1
and a reachable KUBECONFIG to run live):

  preflight        kubectl reachable; locate the MacVz CRI node by its runtime
                   label; assert #84 labels + NoSchedule taint present; node Ready.
  route-baseline   capture the node default route(s) (MACVZ_ROUTE_AUDIT_CMD) and
                   assert it is the expected gateway via the expected interface
                   (MACVZ_EXPECTED_DEFAULT_GW via MACVZ_EXPECTED_DEFAULT_IF,
                   default 192.168.1.1 via en0). Re-asserted unchanged after every
                   recovery scenario.
  startup-order    document the expected bring-up order (apple/container, netd,
                   linuxpod-helper, macvz-cri, kubelet/k3s, kind socket forward);
                   if MACVZ_STARTUP_PROBE_CMD is set, assert each is ready in order.
  deploy           kubectl apply fixtures/linuxpod-workload.yaml (app + late
                   sidecar in one Pod, ConfigMap + Secret + ClusterIP Service);
                   kubectl rollout status — the known-good baseline to recover to.
  backend-evidence HONESTY GATE: assert via MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
                   that the Pod's sandbox is served by a genuine, non-simulated
                   LinuxPod backend. Without it, the LinuxPod-backend stale-state
                   claim is SKIPPED with the #127/#128/#129 blocker; the
                   kubelet-visible recovery still runs.
  scenarios        MACVZ_RECOVERY_SCENARIOS in order (default: services,reboot):
                     services  restart the node service stack in startup order via
                               MACVZ_BOOTSTRAP_CMD without rebooting; node returns
                               Ready and the workload comes back usable.
                     reboot    reboot the remote Mac via MACVZ_REBOOT_CMD (blocks
                               until reachable), then bootstrap; a FRESH usable Pod
                               is expected (VMs do not survive reboot).
                   each scenario asserts: stale-state audit settles to zero (no
                   leftover helper sockets / supervisor journals / VM / kubelet
                   sandbox state), node Ready, workload Pod usable (exec+logs serve
                   the marker), and the default route unchanged + still expected.
  route-after      re-capture the default route(s); assert unchanged + still the
                   expected gateway/interface end-to-end.
  summary          per-scenario record, failed-phase diagnostics paths.

Gated: the live recovery check reboots/restarts a real macOS node and drives a
real cluster. LinuxPod stale-state claims additionally require a backend-evidence
hook proving the Pod is genuinely LinuxPod-backed; absent that they skip loudly
with the #127/#128/#129 blocker. Recovery needs MACVZ_BOOTSTRAP_CMD (and
MACVZ_REBOOT_CMD for the reboot scenario); an unset required hook drops its
scenario with a loud skip, never a silent pass. See the header for the full env
contract and test/e2e/cri-k3s/README.md for topology.
PLAN
}

# --- setup / teardown --------------------------------------------------------
setup() {
	command -v kubectl >/dev/null 2>&1 || die "kubectl not found in PATH"
	[ -f "$FIXTURE" ] || die "fixture not found at $FIXTURE"
	k get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1 \
		|| die "cluster unreachable (KUBECONFIG=${KUBECONFIG:-unset}); set a reachable kubeconfig"

	TMP_ROOT="$(mktemp -d -t macvz-cri-linuxpod-reboot)"
	if [ -n "$OUT_DIR" ]; then
		OUT_DIR_WAS_SET=1
	else
		OUT_DIR="$TMP_ROOT/out"
	fi
	mkdir -p "$OUT_DIR"
	log "out=$OUT_DIR image=$IMAGE scenarios=$SCENARIOS expect_route=$EXPECT_GW via $EXPECT_IF"
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

node_ready() {
	k get node "$NODE" -o jsonpath="{.status.conditions[?(@.type=='Ready')].status}" 2>/dev/null | grep -q True
}

# wait_node_ready <tag> -> 0 once the node reports Ready=True.
wait_node_ready() {
	local tag="$1" _
	for _ in $(seq 1 "$RECOVER_TRIES"); do
		if node_ready; then return 0; fi
		sleep "$RECOVER_INTERVAL"
	done
	k get node "$NODE" -o wide >"$OUT_DIR/$tag-node.log" 2>&1 || true
	return 1
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

# route_capture <label> -> writes the route audit; 0 on hook success.
route_capture() {
	local label="$1"
	[ -n "${MACVZ_ROUTE_AUDIT_CMD:-}" ] || return 0
	run_hook "$MACVZ_ROUTE_AUDIT_CMD" >"$OUT_DIR/route-$label.txt" 2>"$OUT_DIR/route-$label.err"
}

# route_unchanged <label> -> 0 if the default route matches the baseline.
route_unchanged() {
	local label="$1"
	[ -n "${MACVZ_ROUTE_AUDIT_CMD:-}" ] || return 0
	if ! route_capture "$label"; then
		fail "MACVZ_ROUTE_AUDIT_CMD failed at $label (see $OUT_DIR/route-$label.err)"
		return 1
	fi
	if [ -f "$OUT_DIR/route-baseline.txt" ] \
		&& diff -u "$OUT_DIR/route-baseline.txt" "$OUT_DIR/route-$label.txt" >"$OUT_DIR/route-$label.diff" 2>&1; then
		return 0
	fi
	return 1
}

# route_is_expected <file> -> 0 if the captured route names the expected gateway
# AND the expected interface. The audit output is operator-defined text, so this
# is a substring assertion against the documented gw/if (#144 requires the
# default route stay 192.168.1.1 via en0).
route_is_expected() {
	local file="$1"
	[ -f "$file" ] || return 1
	grep -qw "$EXPECT_GW" "$file" || return 1
	grep -qw "$EXPECT_IF" "$file" || return 1
	return 0
}

# stale_state_count <output-file> -> writes raw audit; prints residual line
# count. Return 2 on hook failure, 3 if the hook is unset.
stale_state_count() {
	local out_file="$1" raw
	[ -n "${MACVZ_STALE_STATE_CMD:-}" ] || return 3
	if ! raw="$(run_hook "$MACVZ_STALE_STATE_CMD" 2>"$out_file.err")"; then
		printf '%s\n' "$raw" >"$out_file"
		return 2
	fi
	printf '%s\n' "$raw" >"$out_file"
	printf '%s\n' "$raw" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' '
}

# wait_stale_state_zero <output-file> -> echoes the settled stale count, or -1 if
# the hook is unset, -2 on hook failure. Kubelet may briefly retain a stopped
# sandbox record after recovery before it issues RemovePodSandbox, so allow the
# audit to converge before failing.
wait_stale_state_zero() {
	local out_file="$1" n _
	for _ in $(seq 1 "$RECOVER_TRIES"); do
		if n="$(stale_state_count "$out_file")"; then
			[ "$n" = 0 ] && { printf '0'; return 0; }
		else
			case "$?" in
				3) printf '%s' "-1"; return 0 ;;
				*) printf '%s' "-2"; return 0 ;;
			esac
		fi
		sleep "$RECOVER_INTERVAL"
	done
	printf '%s' "$n"
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
	[ "$rt" = "apple-container" ] && pass "runtime label" || fail "runtime label (got '$rt')"
	[ "$hns" = "unsupported" ] && pass "host-namespace label" || fail "host-namespace label (got '$hns')"
	if k get node "$NODE" -o jsonpath="{.spec.taints[?(@.key=='$TAINT_KEY')].effect}" 2>/dev/null | grep -q NoSchedule; then
		pass "host-namespace NoSchedule taint"
	else
		fail "missing $TAINT_KEY NoSchedule taint (register the node per README #84)"
	fi
	if node_ready; then pass "node Ready"; else fail "node $NODE is not Ready"; fi
	[ "$FAILURES" = "$failures_before" ]
}

phase_route_baseline() {
	CURRENT_PHASE="route-baseline"
	log "Phase: default-route baseline (expect $EXPECT_GW via $EXPECT_IF)"
	if [ -z "${MACVZ_ROUTE_AUDIT_CMD:-}" ]; then
		skip "route-baseline (set MACVZ_ROUTE_AUDIT_CMD to capture/compare the node default route across recovery)"
		return 0
	fi
	if ! route_capture baseline; then
		fail "MACVZ_ROUTE_AUDIT_CMD failed at baseline (see $OUT_DIR/route-baseline.err)"
		return 0
	fi
	pass "captured node default route baseline ($OUT_DIR/route-baseline.txt)"
	if route_is_expected "$OUT_DIR/route-baseline.txt"; then
		pass "baseline default route is the expected $EXPECT_GW via $EXPECT_IF"
	else
		fail "baseline default route is NOT $EXPECT_GW via $EXPECT_IF (see $OUT_DIR/route-baseline.txt)"
	fi
}

phase_startup_order() {
	CURRENT_PHASE="startup-order"
	log "Phase: documented startup order (container -> netd -> helper -> cri -> kubelet/k3s -> kind-fwd)"
	if [ -z "${MACVZ_STARTUP_PROBE_CMD:-}" ]; then
		skip "startup-order probe (set MACVZ_STARTUP_PROBE_CMD to assert each component's readiness in order)"
		return 0
	fi
	if ! run_hook "$MACVZ_STARTUP_PROBE_CMD" >"$OUT_DIR/startup-order.txt" 2>"$OUT_DIR/startup-order.err"; then
		fail "MACVZ_STARTUP_PROBE_CMD failed (see $OUT_DIR/startup-order.err)"
		return 0
	fi
	# Each non-empty line is `component: state`; any state that is not a ready
	# token is a failure. Tokens treated as ready: ready/ok/up/running/active/true.
	local bad
	bad="$(sed '/^[[:space:]]*$/d' "$OUT_DIR/startup-order.txt" \
		| grep -viE ':[[:space:]]*(ready|ok|up|running|active|true)[[:space:]]*$' || true)"
	if [ -z "$bad" ]; then
		pass "all startup-order components report ready ($OUT_DIR/startup-order.txt)"
	else
		fail "startup-order components not ready: $(printf '%s' "$bad" | tr '\n' ';') (see $OUT_DIR/startup-order.txt)"
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
	log "Phase: deploy app+late-sidecar fixture + rollout (known-good baseline)"
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
}

phase_backend_evidence() {
	CURRENT_PHASE="backend-evidence"
	log "Phase: LinuxPod backend evidence (honesty gate)"
	if [ -z "${MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD:-}" ]; then
		skip "backend-evidence: set MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD to prove the Pod is LinuxPod-backed; the LinuxPod stale-state claim will be skipped (blocked on CRI-L serving #127/#128/#129)"
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

# resolve_scenarios filters MACVZ_RECOVERY_SCENARIOS down to those whose required
# hooks are present, dropping the rest with a loud skip.
resolve_scenarios() {
	CURRENT_PHASE="scenarios"
	local s
	local IFS=','
	for s in $SCENARIOS; do
		s="$(printf '%s' "$s" | tr -d '[:space:]')"
		[ -n "$s" ] || continue
		case "$s" in
			services)
				if [ -n "${MACVZ_BOOTSTRAP_CMD:-}" ]; then ENABLED_SCENARIOS="$ENABLED_SCENARIOS services"
				else skip "scenario 'services' dropped (set MACVZ_BOOTSTRAP_CMD); service-stack recovery not exercised"; fi ;;
			reboot)
				if [ -z "${MACVZ_BOOTSTRAP_CMD:-}" ]; then
					skip "scenario 'reboot' dropped (set MACVZ_BOOTSTRAP_CMD); reboot recovery not exercised"
				elif [ -z "${MACVZ_REBOOT_CMD:-}" ]; then
					skip "scenario 'reboot' dropped (set MACVZ_REBOOT_CMD to reboot the remote Mac and block until reachable); reboot recovery not exercised"
				else
					ENABLED_SCENARIOS="$ENABLED_SCENARIOS reboot"
				fi ;;
			*)
				skip "unknown recovery scenario '$s' ignored (valid: services,reboot)" ;;
		esac
	done
	ENABLED_SCENARIOS="$(printf '%s' "$ENABLED_SCENARIOS" | sed 's/^ *//')"
	if [ -z "$ENABLED_SCENARIOS" ]; then
		skip "no usable recovery scenarios (set MACVZ_BOOTSTRAP_CMD and, for reboot, MACVZ_REBOOT_CMD)"
		return 1
	fi
	log "enabled recovery scenarios: $ENABLED_SCENARIOS"
}

# bootstrap_stack <tag> -> run the documented ordered bring-up; 0 on success.
bootstrap_stack() {
	local tag="$1"
	log "$tag: bootstrap node stack in startup order (MACVZ_BOOTSTRAP_CMD)"
	if run_hook "$MACVZ_BOOTSTRAP_CMD" >"$OUT_DIR/$tag-bootstrap.log" 2>&1; then
		pass "$tag: bootstrap completed (see $OUT_DIR/$tag-bootstrap.log)"
		return 0
	fi
	fail "$tag: MACVZ_BOOTSTRAP_CMD returned non-zero (see $OUT_DIR/$tag-bootstrap.log)"
	return 1
}

# assert_recovered <tag> <expect-fresh-pod> -> shared post-bootstrap assertions:
# node Ready, workload usable, stale-state zero, route unchanged + expected.
assert_recovered() {
	local tag="$1" expect_fresh="$2" uid_before="${3:-}"

	if wait_node_ready "$tag"; then
		pass "$tag: node $NODE returned Ready"
	else
		fail "$tag: node $NODE did not return Ready (see $OUT_DIR/$tag-node.log)"
	fi

	local pod_after
	if pod_after="$(wait_exec_logs_ok "$tag")" && [ -n "$pod_after" ]; then
		pass "$tag: workload Pod $pod_after usable (exec+logs serve the marker)"
		if [ "$expect_fresh" = 1 ] && [ -n "$uid_before" ]; then
			local uid_after; uid_after="$(pod_uid "$pod_after")"
			if [ "$uid_before" = "$uid_after" ]; then
				log "$tag: same Pod UID survived ($uid_after) — stronger than required"
			else
				log "$tag: fresh Pod after recovery ($uid_before -> $uid_after) — expected; VMs do not survive reboot"
			fi
		fi
	else
		fail "$tag: workload did not return to a usable Pod (see $OUT_DIR/$tag-exec.err and $OUT_DIR/$tag-logs.err)"
	fi

	# No stale leftovers may block a fresh run. The socket/journal/kubelet-sandbox
	# portion is kubelet/CRI-visible regardless of backend; the LinuxPod-VM /
	# supervisor portion is only a proven claim once the honesty gate confirms a
	# genuine non-simulated backend (#127/#128/#129).
	local n; n="$(wait_stale_state_zero "$OUT_DIR/$tag-stale.log")"
	case "$n" in
		-1) skip "$tag: stale-state audit skipped (set MACVZ_STALE_STATE_CMD to assert no leftover sockets/journals/VM/sandbox state)" ;;
		-2) fail "$tag: MACVZ_STALE_STATE_CMD failed (see $OUT_DIR/$tag-stale.log.err)" ;;
		0)
			if [ "$LINUXPOD_BACKED" = 1 ]; then
				pass "$tag: zero stale helper-socket/supervisor-journal/LinuxPod-VM/kubelet-sandbox leftovers"
			else
				pass "$tag: zero stale helper-socket/kubelet-sandbox leftovers (LinuxPod-VM claim not enforced — backend not proven non-simulated, #127/#128/#129)"
			fi ;;
		*)  fail "$tag: $n stale leftover line(s) remain after bootstrap (see $OUT_DIR/$tag-stale.log)" ;;
	esac

	# Route guard: unchanged and still the expected gateway/interface.
	if [ -n "${MACVZ_ROUTE_AUDIT_CMD:-}" ]; then
		if route_unchanged "$tag"; then
			pass "$tag: default route unchanged across recovery"
		else
			fail "$tag: default route changed across recovery (see $OUT_DIR/route-$tag.diff) — recovery must never mutate it"
		fi
		if route_is_expected "$OUT_DIR/route-$tag.txt"; then
			pass "$tag: default route still $EXPECT_GW via $EXPECT_IF"
		else
			fail "$tag: default route no longer $EXPECT_GW via $EXPECT_IF (see $OUT_DIR/route-$tag.txt)"
		fi
	else
		skip "$tag: route guard skipped (set MACVZ_ROUTE_AUDIT_CMD)"
	fi
}

scenario_services() {
	CURRENT_PHASE="services"
	log "Scenario: services (ordered service-stack restart, no host reboot)"
	local pod uid_before; pod="$(pod_name)"; uid_before="$(pod_uid "$pod")"
	bootstrap_stack services || return 1
	assert_recovered services 0 "$uid_before"
}

scenario_reboot() {
	CURRENT_PHASE="reboot"
	log "Scenario: reboot (remote Mac reboot, then ordered bootstrap)"
	local pod uid_before; pod="$(pod_name)"; uid_before="$(pod_uid "$pod")"
	log "reboot: rebooting remote Mac and waiting for reachability (MACVZ_REBOOT_CMD)"
	if run_hook "$MACVZ_REBOOT_CMD" >"$OUT_DIR/reboot.log" 2>&1; then
		pass "reboot: remote Mac rebooted and reachable again (see $OUT_DIR/reboot.log)"
	else
		fail "reboot: MACVZ_REBOOT_CMD did not return the host to reachable (see $OUT_DIR/reboot.log)"
		return 1
	fi
	bootstrap_stack reboot || return 1
	# A reboot does not preserve live Pod VMs, so a fresh usable Pod is expected.
	assert_recovered reboot 1 "$uid_before"
}

phase_scenarios() {
	local s
	# shellcheck disable=SC2086
	for s in $ENABLED_SCENARIOS; do
		case "$s" in
			services) scenario_services ;;
			reboot)   scenario_reboot ;;
		esac
	done
}

phase_route_after() {
	CURRENT_PHASE="route-after"
	log "Phase: default-route audit (after, end-to-end)"
	if [ -z "${MACVZ_ROUTE_AUDIT_CMD:-}" ]; then
		skip "route-after (set MACVZ_ROUTE_AUDIT_CMD)"
		return 0
	fi
	if route_unchanged after; then
		pass "node default route(s) unchanged across the whole recovery run (non-goal honored)"
	else
		fail "node default route(s) changed across the run (see $OUT_DIR/route-after.diff) — recovery must never mutate default routes"
	fi
	if route_is_expected "$OUT_DIR/route-after.txt"; then
		pass "final default route is the expected $EXPECT_GW via $EXPECT_IF"
	else
		fail "final default route is NOT $EXPECT_GW via $EXPECT_IF (see $OUT_DIR/route-after.txt)"
	fi
}

# --- main --------------------------------------------------------------------
CURRENT_PHASE="init"
if [ "$INTEGRATION" != 1 ] || [ -z "${KUBECONFIG:-}" ]; then
	print_plan
	exit 0
fi

setup
phase_preflight || die "preflight failed; not testing recovery on an unverified node"
phase_route_baseline
phase_startup_order
phase_deploy || die "deploy failed; nothing to recover"
phase_backend_evidence
if resolve_scenarios; then
	phase_scenarios
fi
phase_route_after

echo
if [ "$FAILURES" -eq 0 ] && [ "$SKIPS" -eq 0 ]; then
	pass "CRI-L8-5 node reboot/bootstrap recovery: all checks passed (diagnostics in $OUT_DIR)"
	exit 0
fi
if [ "$FAILURES" -eq 0 ]; then
	pass "CRI-L8-5 node reboot/bootstrap recovery: checks passed with $SKIPS skipped (recovery acceptance is NOT complete while skips remain; provide the missing hooks). Diagnostics in $OUT_DIR"
	exit 0
fi
fail "CRI-L8-5 node reboot/bootstrap recovery: $FAILURES check(s) failed (diagnostics in $OUT_DIR)"
exit 1
