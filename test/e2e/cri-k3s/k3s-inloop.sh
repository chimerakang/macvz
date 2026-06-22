#!/usr/bin/env bash
#
# k3s-inloop.sh — CRI-P9 follow-up (#85) real kubelet/k3s in-loop validation of
# the experimental MacVz CRI adapter.
#
# Unlike run.sh/soak.sh (which drive the CRI socket directly with `crictl`), this
# harness puts a real Linux **kubelet/k3s control plane** in the loop: it
# schedules a single-container fixture onto the macOS MacVz CRI node, then proves
# the user-facing Kubernetes flows the route-two decision requires — scheduling
# and Pod events, `kubectl rollout status`, `kubectl logs`, `kubectl exec`,
# `kubectl port-forward`, ClusterIP/Service reachability — plus restart recovery
# (macvz-cri and k3s/kubelet, independently) and a sustained soak.
#
# This is the missing CRI-P9 evidence #83 explicitly did not collect (crictl is
# not a control-plane loop). It is an isolated `develop`-track feasibility probe,
# NOT the shipped Virtual Kubelet e2e (test/e2e/e2e.sh), and must not gate the VK
# release path. The CRI route stays no-go for replacement until #82 also clears.
#
# Gating: the live suite mutates a real cluster and a real macOS CRI node, so it
# runs only when MACVZ_INTEGRATION=1 *and* a reachable KUBECONFIG is provided.
# Without both it prints the runbook plan and exits 0, so it is safe in
# `go test`-style CI and `bash -n` validation.
#
# Topology (operator-provided):
#   - A Linux k3s server / control plane.
#   - A macOS host running `macvz-cri` as the external CRI endpoint for a
#     k3s/kubelet node, registered with the #84 labels/taint
#     (see test/e2e/cri-k3s/README.md "Pointing k3s at macvz-cri").
#
# Environment:
#   MACVZ_INTEGRATION       set to 1 to run live (otherwise plan-only).
#   KUBECONFIG              kubeconfig for the k3s control plane (required live).
#   MACVZ_NODE              name of the MacVz CRI node. Default: auto-detected as
#                           the (single) node carrying node.macvz.io/runtime=apple-container.
#   MACVZ_CRI_IMAGE         arm64 image for the fixture (default: busybox:1.36.1).
#                           Substituted into fixtures/workload.yaml at apply time.
#   MACVZ_INLOOP_SOAK_ITERATIONS  soak sampling iterations (default: 30). Each
#                           samples adapter RSS, Pod restartCount, and host
#                           workload counts. Raise for a multi-day operator run.
#   MACVZ_INLOOP_SOAK_INTERVAL    seconds between soak samples (default: 10).
#   MACVZ_INLOOP_RSS_GROWTH_KB    allowed adapter RSS growth before flagging a
#                           leak (default: 65536 = 64 MiB). Needs MACVZ_ADAPTER_RSS_CMD.
#   MACVZ_CRI_OUT_DIR       results/diagnostics dir (default: a mktemp dir).
#
# Operator hooks (commands run via `sh -c`; the harness cannot reach the remote
# macOS node itself). A phase whose required hook is unset is SKIPped *loudly* —
# never silently passed:
#   MACVZ_RESTART_CRI_CMD   restart the macvz-cri service on the CRI node, e.g.
#                           "ssh mac 'launchctl kickstart -k gui/$(id -u)/io.macvz.cri'".
#   MACVZ_RESTART_K3S_CMD   restart the k3s agent / kubelet for the MacVz node.
#   MACVZ_ADAPTER_RSS_CMD   print the adapter's resident memory in KB (one integer),
#                           e.g. "ssh mac 'ps -o rss= -p $(pgrep -x macvz-cri)'".
#   MACVZ_HOST_AUDIT_CMD    print apple/container workloads (for orphan/dup audit),
#                           e.g. "ssh mac 'container list --all'". Used to assert
#                           no stale macvz-cri-* workloads remain.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
FIXTURE="$HERE/fixtures/workload.yaml"
NS="macvz-cri-e2e"
DEPLOY="inloop"
SVC="inloop"
MARKER="macvz-cri-p9-inloop-ok"
BOOT_MARKER="macvz-cri-p9-inloop-boot"
SECRET_MARKER="macvz-cri-p9-secret-ok"

INTEGRATION="${MACVZ_INTEGRATION:-0}"
IMAGE="${MACVZ_CRI_IMAGE:-busybox:1.36.1}"
NODE="${MACVZ_NODE:-}"
SOAK_ITERS="${MACVZ_INLOOP_SOAK_ITERATIONS:-30}"
SOAK_INTERVAL="${MACVZ_INLOOP_SOAK_INTERVAL:-10}"
RSS_GROWTH_KB="${MACVZ_INLOOP_RSS_GROWTH_KB:-65536}"
OUT_DIR="${MACVZ_CRI_OUT_DIR:-}"
HANDOFF="${MACVZ_HANDOFF:-0}"

RUNTIME_LABEL="node.macvz.io/runtime=apple-container"
TAINT_KEY="node.macvz.io/host-namespace-unsupported"

FAILURES=0
SKIPS=0
TMP_ROOT=""
PF_PID=""

c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
pass() { printf "${c_green}PASS${c_off} %s\n" "$*"; }
skip() { printf "${c_yellow}SKIP${c_off} %s\n" "$*"; SKIPS=$((SKIPS+1)); }
fail() { printf "${c_red}FAIL${c_off} %s\n" "$*"; FAILURES=$((FAILURES+1)); }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

k() { kubectl "$@"; }
kn() { kubectl -n "$NS" "$@"; }

print_plan() {
	cat <<'PLAN'
CRI-P9 real kubelet/k3s in-loop suite (plan; set MACVZ_INTEGRATION=1 and a
reachable KUBECONFIG to run live):

  preflight     kubectl reachable; locate the MacVz CRI node by its runtime
                label; assert #84 labels + NoSchedule taint present; node Ready.
  deploy        kubectl apply fixtures/workload.yaml (single-container Deployment
                + ConfigMap + Secret + ClusterIP Service); kubectl rollout status.
  scheduling    Pod landed on the MacVz node; Pod events show clean scheduling
                (no FailedScheduling / FailedCreatePodSandBox).
  handoff       (MACVZ_HANDOFF=1, node on macvz-cri --experimental-handoff)
                container Running implies StartContainer's identity gate passed
                (#116); MACVZ_HANDOFF_STATUS_CMD surfaces on-node identityVerified.
  logs          kubectl logs returns the workload boot marker.
  exec          kubectl exec reads the projected Secret + ConfigMap markers.
  port-forward  kubectl port-forward + curl localhost returns the served marker.
  service       an in-cluster probe Pod on a Linux node curls the ClusterIP
                Service and returns the marker (documented allowed vantage).
  restart-cri   restart macvz-cri (operator hook); Pod stays Running, same UID;
                host audit shows no duplicate/orphaned apple/container workload.
  restart-k3s   restart k3s/kubelet (operator hook); node returns Ready and the
                Pod is observable/recovered with no orphan.
  soak          sample adapter RSS, Pod restartCount, and host workload counts
                over MACVZ_INLOOP_SOAK_ITERATIONS; flag RSS growth / churn.
  cleanup       delete the fixture namespace; assert no residual Pods and no
                stale macvz-cri-* apple/container workloads.

Gated: the live suite drives a real cluster and a real macOS CRI node. Restart
and audit phases need operator hooks (MACVZ_RESTART_CRI_CMD, MACVZ_RESTART_K3S_CMD,
MACVZ_ADAPTER_RSS_CMD, MACVZ_HOST_AUDIT_CMD); a phase whose hook is unset is
skipped loudly, never silently passed. See the header for the full env contract
and test/e2e/cri-k3s/README.md for topology.
PLAN
}

# --- setup / teardown --------------------------------------------------------
setup() {
	command -v kubectl >/dev/null 2>&1 || die "kubectl not found in PATH"
	[ -f "$FIXTURE" ] || die "fixture not found at $FIXTURE"
	k get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1 \
		|| die "cluster unreachable (KUBECONFIG=${KUBECONFIG:-unset}); set a reachable kubeconfig"

	TMP_ROOT="$(mktemp -d -t macvz-cri-inloop)"
	[ -n "$OUT_DIR" ] || OUT_DIR="$TMP_ROOT/out"
	mkdir -p "$OUT_DIR"
	log "out=$OUT_DIR image=$IMAGE"
}

cleanup_trap() {
	[ -n "$PF_PID" ] && { kill "$PF_PID" 2>/dev/null || true; }
	# Best-effort namespace teardown unless the operator asked to keep evidence.
	if [ "${MACVZ_CRI_KEEP:-0}" != 1 ]; then
		kubectl delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
	fi
	[ -n "$TMP_ROOT" ] && [ "${MACVZ_CRI_KEEP:-0}" != 1 ] && rm -rf "$TMP_ROOT"
}
trap cleanup_trap EXIT

run_hook() {
	# run_hook <env-value> -> runs it via sh -c, capturing stdout. Returns the
	# command's exit status; caller decides how to treat a missing hook.
	local cmd="$1"; shift
	[ -n "$cmd" ] || return 3
	sh -c "$cmd"
}

# --- phases ------------------------------------------------------------------
phase_preflight() {
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

	local failures_before="$FAILURES"
	# #84 labels present.
	local rt hns
	rt="$(k get node "$NODE" -o jsonpath='{.metadata.labels.node\.macvz\.io/runtime}' 2>/dev/null)"
	hns="$(k get node "$NODE" -o jsonpath='{.metadata.labels.node\.macvz\.io/host-namespace}' 2>/dev/null)"
	[ "$rt" = "apple-container" ] && pass "runtime label" || fail "runtime label (got '$rt')"
	[ "$hns" = "unsupported" ] && pass "host-namespace label" || fail "host-namespace label (got '$hns')"

	# #84 NoSchedule taint present.
	if k get node "$NODE" -o jsonpath="{.spec.taints[?(@.key=='$TAINT_KEY')].effect}" 2>/dev/null | grep -q NoSchedule; then
		pass "host-namespace NoSchedule taint"
	else
		fail "missing $TAINT_KEY NoSchedule taint (register the node per README #84)"
	fi

	# Node Ready.
	if k get node "$NODE" -o jsonpath="{.status.conditions[?(@.type=='Ready')].status}" 2>/dev/null | grep -q True; then
		pass "node Ready"
	else
		fail "node $NODE is not Ready"
	fi
	[ "$FAILURES" = "$failures_before" ]
}

apply_fixture() {
	# Substitute the image, then apply. Keep the source file untouched.
	sed "s#image: busybox:1.36.1#image: $IMAGE#g" "$FIXTURE" >"$OUT_DIR/workload.applied.yaml"
	k apply -f "$OUT_DIR/workload.applied.yaml" >"$OUT_DIR/apply.log" 2>&1
}

phase_deploy() {
	log "Phase: deploy fixture + rollout"
	if ! apply_fixture; then
		fail "kubectl apply failed (see $OUT_DIR/apply.log)"
		return 1
	fi
	pass "fixture applied"
	# rollout status is the kubelet/scheduler driving the Pod to Ready — the core
	# difference from the crictl harness.
	if kn rollout status "deploy/$DEPLOY" --timeout=5m >"$OUT_DIR/rollout.log" 2>&1; then
		pass "kubectl rollout status (Deployment available)"
	else
		fail "rollout did not complete (see $OUT_DIR/rollout.log + Pod events below)"
		kn describe "deploy/$DEPLOY" >"$OUT_DIR/deploy-describe.log" 2>&1 || true
		kn get pods -o wide >"$OUT_DIR/pods.log" 2>&1 || true
	fi
}

pod_name() {
	kn get pods -l app=inloop -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

phase_scheduling() {
	log "Phase: scheduling + Pod events"
	local pod node
	pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no fixture Pod found"; return 1; }
	node="$(kn get pod "$pod" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
	if [ "$node" = "$NODE" ]; then
		pass "Pod $pod scheduled onto MacVz node $NODE"
	else
		fail "Pod scheduled onto '$node', expected MacVz node '$NODE'"
	fi
	# Clean scheduling: no scheduler/sandbox failures in the Pod's events.
	kn get events --field-selector "involvedObject.name=$pod" >"$OUT_DIR/pod-events.log" 2>&1 || true
	if grep -Eqi 'FailedScheduling|FailedCreatePodSandBox' "$OUT_DIR/pod-events.log"; then
		fail "Pod events contain scheduling/sandbox failures (see $OUT_DIR/pod-events.log)"
	else
		pass "Pod events clean (no FailedScheduling/FailedCreatePodSandBox)"
	fi
}

phase_handoff() {
	# CRI-I4-2 (#119): when the MacVz CRI node runs with the experimental
	# handoff-aware runtime (macvz-cri --experimental-handoff, see README
	# "Pointing k3s at macvz-cri"), a Pod reaching Running is itself evidence
	# that StartContainer's identity gate passed — the handoff-aware runtime
	# only persists Running after the launched process reports the expected
	# rootfs identity through the runtime-private evidence channel (#116).
	# This phase makes that assertion explicit and, when an operator status
	# hook is provided, surfaces the on-node handoff diagnostics.
	[ "$HANDOFF" = "1" ] || { skip "handoff (set MACVZ_HANDOFF=1 when the node runs macvz-cri --experimental-handoff)"; return 0; }
	log "Phase: handoff identity verification (CRI-I4-2 #119)"
	local pod phase; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for handoff verification"; return 1; }
	phase="$(kn get pod "$pod" -o jsonpath='{.status.containerStatuses[0].state.running}' 2>/dev/null)"
	if [ -n "$phase" ]; then
		pass "container Running through the handoff identity gate (#116: Running implies verified identity)"
	else
		fail "container not Running; handoff identity gate not satisfied (see $OUT_DIR/handoff-*.log)"
		kn get pod "$pod" -o yaml >"$OUT_DIR/handoff-pod.yaml" 2>&1 || true
	fi
	# Optional: surface the runtime-private handoff diagnostics from the node.
	# The hook runs `crictl inspect <id>` (Verbose) on the CRI node and prints
	# the container status JSON; the handoff-aware status exposes
	# handoffPrepared / identityVerified / expectedIdentity / observedIdentity
	# under .info (CRI-I3 #117 handoffStatusInfo).
	if [ -n "${MACVZ_HANDOFF_STATUS_CMD:-}" ]; then
		run_hook "$MACVZ_HANDOFF_STATUS_CMD" >"$OUT_DIR/handoff-status.json" 2>"$OUT_DIR/handoff-status.err" || true
		local verified=""
		if command -v jq >/dev/null 2>&1; then
			verified="$(jq -r '.identityVerified // .info.identityVerified // empty' "$OUT_DIR/handoff-status.json" 2>/dev/null)"
		fi
		if [ "$verified" = "true" ] || grep -Eq '"identityVerified"[[:space:]]*:[[:space:]]*"?true"?|identityVerified=true' "$OUT_DIR/handoff-status.json" 2>/dev/null; then
			pass "node handoff diagnostics report identityVerified=true (see $OUT_DIR/handoff-status.json)"
		elif grep -q 'handoffPrepared' "$OUT_DIR/handoff-status.json" 2>/dev/null; then
			fail "node handoff diagnostics present but identityVerified not true (see $OUT_DIR/handoff-status.json)"
		else
			skip "MACVZ_HANDOFF_STATUS_CMD produced no handoffStatusInfo (node may not be running --experimental-handoff)"
		fi
	else
		skip "handoff diagnostics (set MACVZ_HANDOFF_STATUS_CMD to surface on-node identityVerified/expectedIdentity)"
	fi
}

phase_logs() {
	log "Phase: kubectl logs"
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for logs"; return 1; }
	if kn logs "$pod" 2>"$OUT_DIR/logs.err" | grep -q "$BOOT_MARKER"; then
		pass "kubectl logs returns the boot marker"
	else
		fail "kubectl logs missing '$BOOT_MARKER' (see $OUT_DIR/logs.err)"
	fi
}

phase_exec() {
	log "Phase: kubectl exec (projected Secret + ConfigMap)"
	local pod out; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for exec"; return 1; }
	out="$(kn exec "$pod" -- sh -c 'cat /etc/app-secret/token; echo; cat /www/app.conf' 2>"$OUT_DIR/exec.err")"
	echo "$out" >"$OUT_DIR/exec.out"
	if printf '%s' "$out" | grep -q "$SECRET_MARKER"; then
		pass "exec read projected Secret marker"
	else
		fail "exec missing Secret marker (see $OUT_DIR/exec.out)"
	fi
	if printf '%s' "$out" | grep -q "$MARKER"; then
		pass "exec read projected ConfigMap marker"
	else
		fail "exec missing ConfigMap marker (see $OUT_DIR/exec.out)"
	fi
}

phase_portforward() {
	log "Phase: kubectl port-forward"
	local pod lport=18080; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for port-forward"; return 1; }
	kn port-forward "pod/$pod" "$lport:8080" >"$OUT_DIR/pf.log" 2>&1 &
	PF_PID="$!"
	local ok=0 _
	for _ in $(seq 1 30); do
		if curl -fsS "http://127.0.0.1:$lport/index.html" 2>/dev/null | grep -q "$MARKER"; then ok=1; break; fi
		sleep 0.5
	done
	kill "$PF_PID" 2>/dev/null || true; wait "$PF_PID" 2>/dev/null || true; PF_PID=""
	if [ "$ok" = 1 ]; then
		pass "port-forward + curl returns the served marker"
	else
		fail "port-forward curl did not return '$MARKER' (see $OUT_DIR/pf.log)"
	fi
}

phase_service() {
	log "Phase: ClusterIP Service reachability (in-cluster probe)"
	# The probe Pod does NOT tolerate the MacVz taint, so it lands on a Linux node:
	# a documented, supported vantage point for ClusterIP reachability. It curls
	# the Service by DNS name and must observe the served marker.
	local probe="inloop-probe"
	kn delete pod "$probe" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	if kn run "$probe" --image="$IMAGE" --restart=Never --command -- \
		sh -c "for i in \$(seq 1 30); do wget -qO- http://$SVC.$NS.svc:80/index.html && exit 0; sleep 1; done; exit 1" \
		>"$OUT_DIR/probe-run.log" 2>&1; then
		kn wait --for=condition=Ready "pod/$probe" --timeout=2m >/dev/null 2>&1 || true
	fi
	# Wait for the probe to finish, then read its logs for the marker.
	local phase _
	for _ in $(seq 1 60); do
		phase="$(kn get pod "$probe" -o jsonpath='{.status.phase}' 2>/dev/null)"
		[ "$phase" = "Succeeded" ] || [ "$phase" = "Failed" ] && break
		sleep 1
	done
	kn logs "$probe" >"$OUT_DIR/probe.log" 2>&1 || true
	if grep -q "$MARKER" "$OUT_DIR/probe.log"; then
		pass "ClusterIP Service reachable from a Linux-node probe"
	else
		fail "Service probe did not return '$MARKER' (phase=$phase; see $OUT_DIR/probe.log)"
	fi
	kn delete pod "$probe" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

pod_uid() { kn get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null; }
pod_phase() { kn get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }

host_workload_count() {
	# host_workload_count <output-file>
	#
	# Writes the raw host audit to the output file and prints the number of
	# macvz-cri-* workloads. A hook failure is distinct from a clean zero count.
	local out_file="$1" raw
	[ -n "${MACVZ_HOST_AUDIT_CMD:-}" ] || return 3
	if ! raw="$(run_hook "$MACVZ_HOST_AUDIT_CMD" 2>"$out_file.err")"; then
		printf '%s\n' "$raw" >"$out_file"
		return 2
	fi
	printf '%s\n' "$raw" >"$out_file"
	printf '%s\n' "$raw" | grep -i 'macvz-cri-' | wc -l | tr -d ' '
}

phase_restart_cri() {
	log "Phase: macvz-cri restart recovery"
	if [ -z "${MACVZ_RESTART_CRI_CMD:-}" ]; then
		skip "restart-cri (set MACVZ_RESTART_CRI_CMD to restart the adapter on the CRI node)"
		return 0
	fi
	local pod uid_before uid_after; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod before macvz-cri restart"; return 1; }
	uid_before="$(pod_uid "$pod")"

	log "restarting macvz-cri via operator hook"
	run_hook "$MACVZ_RESTART_CRI_CMD" >"$OUT_DIR/restart-cri.log" 2>&1 \
		|| fail "MACVZ_RESTART_CRI_CMD returned non-zero (see $OUT_DIR/restart-cri.log)"

	# The Pod must remain the same object (adapter restart is not a Pod restart):
	# the micro-VM outlives the adapter, which re-adopts it on recovery.
	local ok=0 _
	for _ in $(seq 1 60); do
		[ "$(pod_phase "$pod")" = "Running" ] && { ok=1; break; }
		sleep 2
	done
	uid_after="$(pod_uid "$pod")"
	[ "$ok" = 1 ] && pass "Pod still Running after macvz-cri restart" || fail "Pod not Running after macvz-cri restart"
	if [ -n "$uid_before" ] && [ "$uid_before" = "$uid_after" ]; then
		pass "Pod UID unchanged (no duplicate CRI state)"
	else
		fail "Pod UID changed ($uid_before -> $uid_after): possible duplicate/lost workload"
	fi
	# No duplicate apple/container workload for this Pod.
	if [ -n "${MACVZ_HOST_AUDIT_CMD:-}" ]; then
		local n
		if n="$(host_workload_count "$OUT_DIR/restart-cri-host-audit.log")"; then
			[ "$n" -le 1 ] && pass "host audit: $n macvz-cri-* workload (no duplicate)" \
				|| fail "host audit shows $n macvz-cri-* workloads (possible duplicate; see $OUT_DIR/restart-cri-host-audit.log)"
		else
			fail "host audit hook failed after macvz-cri restart (see $OUT_DIR/restart-cri-host-audit.log.err)"
		fi
	else
		skip "host duplicate audit (set MACVZ_HOST_AUDIT_CMD)"
	fi
}

phase_restart_k3s() {
	log "Phase: k3s/kubelet restart recovery"
	if [ -z "${MACVZ_RESTART_K3S_CMD:-}" ]; then
		skip "restart-k3s (set MACVZ_RESTART_K3S_CMD to restart the kubelet/k3s agent)"
		return 0
	fi
	local pod uid_before; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod before k3s restart"; return 1; }
	uid_before="$(pod_uid "$pod")"

	log "restarting k3s/kubelet via operator hook"
	run_hook "$MACVZ_RESTART_K3S_CMD" >"$OUT_DIR/restart-k3s.log" 2>&1 \
		|| fail "MACVZ_RESTART_K3S_CMD returned non-zero (see $OUT_DIR/restart-k3s.log)"

	# Node may briefly go NotReady; wait for it to return and the Pod to be observable.
	local ready=0 _
	for _ in $(seq 1 90); do
		if k get node "$NODE" -o jsonpath="{.status.conditions[?(@.type=='Ready')].status}" 2>/dev/null | grep -q True; then ready=1; break; fi
		sleep 2
	done
	[ "$ready" = 1 ] && pass "node Ready again after k3s restart" || fail "node $NODE did not return Ready"

	local running=0 uid_after
	for _ in $(seq 1 60); do
		[ "$(pod_phase "$pod")" = "Running" ] && { running=1; break; }
		sleep 2
	done
	uid_after="$(pod_uid "$pod")"
	[ "$running" = 1 ] && pass "Pod observable/Running after k3s restart" || fail "Pod not Running after k3s restart"
	if [ -n "$uid_before" ] && [ "$uid_before" = "$uid_after" ]; then
		pass "Pod UID unchanged after k3s restart (no orphan/dup)"
	else
		fail "Pod UID changed after k3s restart ($uid_before -> $uid_after)"
	fi
}

phase_soak() {
	log "Phase: in-loop soak ($SOAK_ITERS samples, ${SOAK_INTERVAL}s apart)"
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for soak"; return 1; }
	printf 'iteration,rss_kb,pod_phase,restart_count,host_workloads\n' >"$OUT_DIR/soak-samples.csv"

	local first_rss=0 last_rss=0 have_rss=0 i rss phase rc hw rss_hook_failures=0 audit_hook_failures=0
	for i in $(seq 1 "$SOAK_ITERS"); do
		if [ -n "${MACVZ_ADAPTER_RSS_CMD:-}" ]; then
			local rss_raw
			if rss_raw="$(run_hook "$MACVZ_ADAPTER_RSS_CMD" 2>/dev/null)"; then
				rss="$(printf '%s' "$rss_raw" | tr -dc '0-9')"
				if [ -n "$rss" ]; then
					have_rss=1
					[ "$first_rss" = 0 ] && first_rss="$rss"
					last_rss="$rss"
				else
					rss=0
					rss_hook_failures=$((rss_hook_failures+1))
				fi
			else
				rss=0
				rss_hook_failures=$((rss_hook_failures+1))
			fi
		else
			rss=0
		fi
		phase="$(pod_phase "$pod")"
		rc="$(kn get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null)"
		[ -n "$rc" ] || rc=0
		if [ -n "${MACVZ_HOST_AUDIT_CMD:-}" ]; then
			if hw="$(host_workload_count "$OUT_DIR/soak-host-audit-$i.log")"; then
				:
			else
				hw=-2
				audit_hook_failures=$((audit_hook_failures+1))
			fi
		else
			hw=-1
		fi
		printf '%s,%s,%s,%s,%s\n' "$i" "$rss" "$phase" "$rc" "$hw" >>"$OUT_DIR/soak-samples.csv"
		[ $((i % 10)) -eq 0 ] && log "soak $i/$SOAK_ITERS (phase=$phase rss=${rss}KB restarts=$rc)"
		sleep "$SOAK_INTERVAL"
	done

	# Pod must have stayed up across the soak (no crash loop).
	local final_phase final_rc; final_phase="$(pod_phase "$pod")"
	final_rc="$(kn get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null)"; [ -n "$final_rc" ] || final_rc=0
	[ "$final_phase" = "Running" ] && pass "Pod Running for the full soak" || fail "Pod ended soak in phase '$final_phase'"
	[ "$final_rc" -le 1 ] && pass "Pod restartCount bounded ($final_rc)" || fail "Pod restarted $final_rc times during soak"
	[ "$rss_hook_failures" = 0 ] || fail "adapter RSS hook failed or returned non-numeric output $rss_hook_failures time(s)"
	[ "$audit_hook_failures" = 0 ] || fail "host audit hook failed $audit_hook_failures time(s) during soak"

	if [ "$have_rss" = 1 ]; then
		local growth=$((last_rss - first_rss))
		log "adapter RSS: first=${first_rss}KB last=${last_rss}KB growth=${growth}KB (limit ${RSS_GROWTH_KB}KB)"
		[ "$growth" -le "$RSS_GROWTH_KB" ] && pass "adapter RSS growth within bound" \
			|| fail "adapter RSS grew ${growth}KB (> ${RSS_GROWTH_KB}KB): possible leak"
	else
		skip "adapter RSS trend (set MACVZ_ADAPTER_RSS_CMD); samples recorded with rss=0"
	fi
	log "soak samples: $OUT_DIR/soak-samples.csv"
}

phase_cleanup() {
	log "Phase: cleanup + orphan audit"
	k delete -f "$OUT_DIR/workload.applied.yaml" --wait=true --timeout=3m >"$OUT_DIR/cleanup.log" 2>&1 || true
	# No fixture Pods remain in the namespace.
	local remaining
	remaining="$(kn get pods -l app=inloop -o name 2>/dev/null | wc -l | tr -d ' ')"
	[ "$remaining" = 0 ] && pass "no fixture Pods remain after delete" || fail "$remaining fixture Pod(s) remain after delete"

	# No stale apple/container workloads on the host.
	if [ -n "${MACVZ_HOST_AUDIT_CMD:-}" ]; then
		local n
		if n="$(host_workload_count "$OUT_DIR/cleanup-host-audit.log")"; then
			[ "$n" = 0 ] && pass "host audit: no residual macvz-cri-* workloads" \
				|| fail "host audit: $n stale macvz-cri-* workload(s) remain (see $OUT_DIR/cleanup-host-audit.log)"
		else
			fail "host audit hook failed during cleanup (see $OUT_DIR/cleanup-host-audit.log.err)"
		fi
	else
		skip "host orphan audit (set MACVZ_HOST_AUDIT_CMD to assert zero stale workloads)"
	fi
}

# --- main --------------------------------------------------------------------
if [ "$INTEGRATION" != 1 ] || [ -z "${KUBECONFIG:-}" ]; then
	print_plan
	exit 0
fi

setup
phase_preflight || die "preflight failed; not deploying onto an unverified node"
phase_deploy
phase_scheduling
phase_handoff
phase_logs
phase_exec
phase_portforward
phase_service
phase_restart_cri
phase_restart_k3s
phase_soak
phase_cleanup

echo
if [ "$FAILURES" -eq 0 ] && [ "$SKIPS" -eq 0 ]; then
	pass "CRI-P9 in-loop suite: all checks passed (diagnostics in $OUT_DIR)"
	exit 0
fi
if [ "$FAILURES" -eq 0 ]; then
	pass "CRI-P9 in-loop suite: checks passed with $SKIPS skipped hook-dependent phase(s); live acceptance is not complete (diagnostics in $OUT_DIR)"
	exit 0
fi
fail "CRI-P9 in-loop suite: $FAILURES check(s) failed (diagnostics in $OUT_DIR)"
exit 1
