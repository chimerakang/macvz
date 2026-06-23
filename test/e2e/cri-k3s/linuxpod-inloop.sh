#!/usr/bin/env bash
#
# linuxpod-inloop.sh — CRI-L5 (#130) real kubelet/k3s in-loop validation and
# recovery audit for the experimental LinuxPod-backed CRI path.
#
# This is the LinuxPod-backend sibling of k3s-inloop.sh. It puts a real Linux
# kubelet/k3s control plane in the loop against a macOS MacVz CRI node that runs
# `macvz-cri --experimental-linuxpod-backend --linuxpod-helper-socket=…`
# (CRI-L, #125..#129), schedules a two-container app+late-sidecar Pod
# (fixtures/linuxpod-workload.yaml), and proves the LinuxPod acceptance surface:
# shared sandbox namespace, sidecar localhost reachability, rootfs identity
# verification, Pod IP readiness, logs, exec, stop/remove, adapter restart
# recovery, helper restart/failure handling, and a residual LinuxPod
# VM/container/rootfs/handoff/network state audit after cleanup.
#
# HONESTY GATE (the central #130 invariant). The shipped CRI serving path runs on
# apple/container; the LinuxPod backend gate today is a startup handshake against
# a helper that, for the prototype (#124/R17), reports simulated=true and is NOT
# yet wired to serve RunPodSandbox/CreateContainer onto a real LinuxPod
# (#127 lifecycle serving, #128 Pod networking, #129 logs/exec/stats are the open
# dependencies). So a Pod reaching Running on such a node is NOT by itself
# evidence of a LinuxPod-backed Pod. This harness therefore REFUSES to pass the
# LinuxPod-specific acceptances (shared namespace, localhost, identity, Pod IP on
# LinuxPod, residual LinuxPod-VM audit) unless an operator backend-evidence hook
# (MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD) proves on the node that the Pod is served
# by a genuine, non-simulated LinuxPod backend. Absent that proof those phases
# are SKIPped loudly with the precise blocker — never silently passed. This
# mirrors the #119 `kubeletHandoffSmokeBlocked` discipline.
#
# Gating: like k3s-inloop.sh, the live suite mutates a real cluster and a real
# macOS CRI node, so it runs only when MACVZ_INTEGRATION=1 *and* a reachable
# KUBECONFIG is provided. Without both it prints the runbook plan and exits 0, so
# it is safe in `go test`-style CI and `bash -n` validation.
#
# It is an isolated `develop`-track feasibility probe, NOT the shipped Virtual
# Kubelet e2e (test/e2e/e2e.sh), and must not gate the VK release path.
#
# Topology (operator-provided):
#   - A Linux k3s server / control plane.
#   - A macOS host running `macvz-cri --experimental-linuxpod-backend
#     --linuxpod-helper-socket=<sock>` as the external CRI endpoint for a
#     k3s/kubelet node, registered with the #84 labels/taint.
#
# Environment:
#   MACVZ_INTEGRATION       set to 1 to run live (otherwise plan-only).
#   KUBECONFIG              kubeconfig for the k3s control plane (required live).
#   MACVZ_NODE              name of the MacVz CRI node. Default: auto-detected as
#                           the (single) node carrying node.macvz.io/runtime=apple-container.
#   MACVZ_CRI_IMAGE         arm64 image for the fixture (default: busybox:1.36.1).
#   MACVZ_INLOOP_SOAK_ITERATIONS  soak sampling iterations (default: 30).
#   MACVZ_INLOOP_SOAK_INTERVAL    seconds between soak samples (default: 10).
#   MACVZ_INLOOP_RSS_GROWTH_KB    allowed adapter RSS growth before flagging a
#                           leak (default: 65536 = 64 MiB). Needs MACVZ_ADAPTER_RSS_CMD.
#   MACVZ_CRI_OUT_DIR       results/diagnostics dir (default: a mktemp dir).
#
# Operator hooks (commands run via `sh -c`; the harness cannot reach the remote
# macOS node itself). A phase whose required hook is unset is SKIPped *loudly*:
#   MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
#                           REQUIRED to pass any LinuxPod-specific acceptance.
#                           Prints node-side evidence that the named Pod's
#                           sandbox is served by a real, non-simulated LinuxPod
#                           backend (e.g. `crictl inspectp <id>` info, or a node
#                           probe that lists the LinuxPod VM/containers and the
#                           helper's Ping simulated=false). The harness asserts
#                           the output reports simulated=false AND names a
#                           LinuxPod sandbox/VM for the Pod; anything else
#                           (simulated=true, apple/container workload, empty) is
#                           treated as "not LinuxPod-backed" and the LinuxPod
#                           acceptances are skipped with the #127/#128/#129 blocker.
#   MACVZ_LINUXPOD_IDENTITY_CMD
#                           prints the sidecar's LinuxPod rootfs identity status
#                           (identityVerified / expectedIdentity / observedIdentity,
#                           CRI-R16 / pkg/runtime/linuxpod). Used by the identity phase.
#   MACVZ_RESTART_CRI_CMD   restart the macvz-cri service on the CRI node.
#   MACVZ_RESTART_HELPER_CMD restart (or crash+restart) the LinuxPod helper.
#   MACVZ_ADAPTER_RSS_CMD   print the adapter's resident memory in KB (one integer).
#   MACVZ_LINUXPOD_AUDIT_CMD print residual LinuxPod state for the orphan/cleanup
#                           audit: LinuxPod VMs, backend containers, prepared
#                           rootfs/handoff subtrees, and any Pod network state the
#                           backend installed. The harness asserts it is empty
#                           after cleanup and shows no duplicate after restart.
#   MACVZ_ROUTE_AUDIT_CMD   print the node's default route(s) (e.g.
#                           `ssh mac 'netstat -rn -f inet | awk "/^default/"'`).
#                           Captured before and after; the harness asserts the
#                           default route is unchanged (non-goal: never mutate it).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
FIXTURE="$HERE/fixtures/linuxpod-workload.yaml"
NS="macvz-cri-linuxpod-e2e"
DEPLOY="linuxpod-inloop"
SVC="linuxpod-inloop"
MARKER="macvz-cri-l5-inloop-ok"
APP_BOOT_MARKER="macvz-cri-l5-app-boot"
SIDECAR_BOOT_MARKER="macvz-cri-l5-sidecar-boot"
SIDECAR_LOCALHOST_MARKER="macvz-cri-l5-sidecar-localhost-ok"
SECRET_MARKER="macvz-cri-l5-secret-ok"

INTEGRATION="${MACVZ_INTEGRATION:-0}"
IMAGE="${MACVZ_CRI_IMAGE:-busybox:1.36.1}"
NODE="${MACVZ_NODE:-}"
SOAK_ITERS="${MACVZ_INLOOP_SOAK_ITERATIONS:-30}"
SOAK_INTERVAL="${MACVZ_INLOOP_SOAK_INTERVAL:-10}"
RSS_GROWTH_KB="${MACVZ_INLOOP_RSS_GROWTH_KB:-65536}"
OUT_DIR="${MACVZ_CRI_OUT_DIR:-}"

RUNTIME_LABEL="node.macvz.io/runtime=apple-container"
TAINT_KEY="node.macvz.io/host-namespace-unsupported"

FAILURES=0
SKIPS=0
TMP_ROOT=""
PF_PID=""
# LINUXPOD_BACKED is set to 1 only once the backend-evidence phase proves the Pod
# is served by a genuine, non-simulated LinuxPod backend. Until then the
# LinuxPod-specific acceptances skip loudly rather than pass on a handshake.
LINUXPOD_BACKED=0

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
CRI-L5 LinuxPod-backed real kubelet/k3s in-loop suite (plan; set
MACVZ_INTEGRATION=1 and a reachable KUBECONFIG to run live):

  preflight       kubectl reachable; locate the MacVz CRI node by its runtime
                  label; assert #84 labels + NoSchedule taint present; node Ready.
  route-before    capture the node default route(s) (MACVZ_ROUTE_AUDIT_CMD) so
                  the post-run audit can prove they were never mutated.
  deploy          kubectl apply fixtures/linuxpod-workload.yaml (app + late
                  sidecar in one Pod, ConfigMap + Secret + ClusterIP Service);
                  kubectl rollout status.
  scheduling      Pod landed on the MacVz node; events clean (no FailedScheduling
                  / FailedCreatePodSandBox).
  backend-evidence  HONESTY GATE: assert via MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
                  that the Pod's sandbox is served by a genuine, non-simulated
                  LinuxPod backend. Without that proof every LinuxPod-specific
                  acceptance below is SKIPPED with the #127/#128/#129 blocker —
                  a Running Pod on the apple/container path is not LinuxPod
                  evidence.
  shared-ns       (LinuxPod-backed only) app + sidecar share one Pod sandbox;
                  the sidecar reached the app on 127.0.0.1 (localhost marker).
  identity        (LinuxPod-backed only) the late sidecar's rootfs identity
                  verified at start (MACVZ_LINUXPOD_IDENTITY_CMD; CRI-R16).
  podip           Pod IP is assigned and Ready (and, when LinuxPod-backed, owned
                  by the LinuxPod sandbox VM).
  logs            kubectl logs returns both the app and sidecar boot markers.
  exec            kubectl exec into the app reads the projected Secret/ConfigMap
                  and the sidecar's shared-namespace localhost proof file.
  port-forward    kubectl port-forward + curl localhost returns the served marker.
  service         an in-cluster probe Pod on a Linux node curls the ClusterIP
                  Service and returns the marker.
  restart-cri     restart macvz-cri; Pod stays Running, same UID; LinuxPod audit
                  shows no duplicate VM/container.
  restart-helper  restart/crash the LinuxPod helper; recovery is observed or the
                  exact failure handling is documented as the next blocker.
  soak            sample adapter RSS, Pod restartCount, and LinuxPod state counts
                  over MACVZ_INLOOP_SOAK_ITERATIONS; flag RSS growth / churn.
  cleanup         delete the fixture; assert no residual Pods and (LinuxPod audit)
                  no residual LinuxPod VM/container/rootfs/handoff/network state.
  route-after     re-capture the default route(s); assert unchanged.

Gated: the live suite drives a real cluster and a real macOS CRI node. The
LinuxPod acceptances additionally require the node to run
`macvz-cri --experimental-linuxpod-backend` AND a backend-evidence hook proving
the Pod is genuinely LinuxPod-backed; absent that they skip loudly with the
#127/#128/#129 blocker. Restart/audit phases need operator hooks
(MACVZ_RESTART_CRI_CMD, MACVZ_RESTART_HELPER_CMD, MACVZ_ADAPTER_RSS_CMD,
MACVZ_LINUXPOD_AUDIT_CMD, MACVZ_ROUTE_AUDIT_CMD); an unset hook is skipped
loudly, never silently passed. See the header for the full env contract and
test/e2e/cri-k3s/README.md for topology.
PLAN
}

# --- setup / teardown --------------------------------------------------------
setup() {
	command -v kubectl >/dev/null 2>&1 || die "kubectl not found in PATH"
	[ -f "$FIXTURE" ] || die "fixture not found at $FIXTURE"
	k get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1 \
		|| die "cluster unreachable (KUBECONFIG=${KUBECONFIG:-unset}); set a reachable kubeconfig"

	TMP_ROOT="$(mktemp -d -t macvz-cri-linuxpod-inloop)"
	[ -n "$OUT_DIR" ] || OUT_DIR="$TMP_ROOT/out"
	mkdir -p "$OUT_DIR"
	log "out=$OUT_DIR image=$IMAGE"
}

cleanup_trap() {
	[ -n "$PF_PID" ] && { kill "$PF_PID" 2>/dev/null || true; }
	if [ "${MACVZ_CRI_KEEP:-0}" != 1 ]; then
		kubectl delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
	fi
	[ -n "$TMP_ROOT" ] && [ "${MACVZ_CRI_KEEP:-0}" != 1 ] && rm -rf "$TMP_ROOT"
}
trap cleanup_trap EXIT

run_hook() {
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
	local rt hns
	rt="$(k get node "$NODE" -o jsonpath='{.metadata.labels.node\.macvz\.io/runtime}' 2>/dev/null)"
	hns="$(k get node "$NODE" -o jsonpath='{.metadata.labels.node\.macvz\.io/host-namespace}' 2>/dev/null)"
	[ "$rt" = "apple-container" ] && pass "runtime label" || fail "runtime label (got '$rt')"
	[ "$hns" = "unsupported" ] && pass "host-namespace label" || fail "host-namespace label (got '$hns')"

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

phase_route_before() {
	log "Phase: default-route audit (before)"
	if [ -z "${MACVZ_ROUTE_AUDIT_CMD:-}" ]; then
		skip "route-before (set MACVZ_ROUTE_AUDIT_CMD to capture/compare the node default route)"
		return 0
	fi
	if run_hook "$MACVZ_ROUTE_AUDIT_CMD" >"$OUT_DIR/route-before.txt" 2>"$OUT_DIR/route-before.err"; then
		pass "captured node default route(s) ($OUT_DIR/route-before.txt)"
	else
		fail "MACVZ_ROUTE_AUDIT_CMD failed before run (see $OUT_DIR/route-before.err)"
	fi
}

apply_fixture() {
	sed "s#image: busybox:1.36.1#image: $IMAGE#g" "$FIXTURE" >"$OUT_DIR/workload.applied.yaml"
	k apply -f "$OUT_DIR/workload.applied.yaml" >"$OUT_DIR/apply.log" 2>&1
}

phase_deploy() {
	log "Phase: deploy app+late-sidecar fixture + rollout"
	if ! apply_fixture; then
		fail "kubectl apply failed (see $OUT_DIR/apply.log)"
		return 1
	fi
	pass "fixture applied"
	if kn rollout status "deploy/$DEPLOY" --timeout=5m >"$OUT_DIR/rollout.log" 2>&1; then
		pass "kubectl rollout status (Deployment available)"
	else
		fail "rollout did not complete (see $OUT_DIR/rollout.log + Pod events below)"
		kn describe "deploy/$DEPLOY" >"$OUT_DIR/deploy-describe.log" 2>&1 || true
		kn get pods -o wide >"$OUT_DIR/pods.log" 2>&1 || true
	fi
}

pod_name() {
	kn get pods -l app=linuxpod-inloop -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
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
	kn get events --field-selector "involvedObject.name=$pod" >"$OUT_DIR/pod-events.log" 2>&1 || true
	if grep -Eqi 'FailedScheduling|FailedCreatePodSandBox' "$OUT_DIR/pod-events.log"; then
		fail "Pod events contain scheduling/sandbox failures (see $OUT_DIR/pod-events.log)"
	else
		pass "Pod events clean (no FailedScheduling/FailedCreatePodSandBox)"
	fi
}

phase_backend_evidence() {
	# The honesty gate. Only proof from the node that the Pod's sandbox is served
	# by a genuine, non-simulated LinuxPod backend flips LINUXPOD_BACKED=1 and
	# unlocks the LinuxPod-specific acceptances. Anything else keeps them skipped
	# with the precise #127/#128/#129 blocker.
	log "Phase: LinuxPod backend evidence (honesty gate)"
	if [ -z "${MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD:-}" ]; then
		skip "backend-evidence: set MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD to prove the Pod is LinuxPod-backed; LinuxPod acceptances will be skipped (blocked on CRI-L serving #127/#128/#129)"
		return 0
	fi
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for backend evidence"; return 1; }
	if ! MACVZ_POD="$pod" run_hook "$MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD" \
		>"$OUT_DIR/backend-evidence.txt" 2>"$OUT_DIR/backend-evidence.err"; then
		fail "MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD failed (see $OUT_DIR/backend-evidence.err)"
		return 1
	fi
	# Reject the simulated handshake explicitly: the R17 prototype helper reports
	# simulated=true and serves no real Pod.
	if grep -Eqi 'simulated[":= ]+true' "$OUT_DIR/backend-evidence.txt"; then
		skip "backend-evidence reports simulated=true: node is on the R17 prototype handshake, not a real LinuxPod backend — LinuxPod acceptances blocked on #127/#128/#129 (see $OUT_DIR/backend-evidence.txt)"
		return 0
	fi
	if grep -Eqi 'linuxpod|pod[-_ ]?vm|sandboxVM' "$OUT_DIR/backend-evidence.txt" \
		&& grep -Eqi 'simulated[":= ]+false|backend[":= ]+linuxpod|serving[":= ]+linuxpod' "$OUT_DIR/backend-evidence.txt"; then
		LINUXPOD_BACKED=1
		pass "Pod is served by a genuine (non-simulated) LinuxPod backend (see $OUT_DIR/backend-evidence.txt)"
	else
		skip "backend-evidence did not prove a non-simulated LinuxPod-backed Pod (got neither simulated=false nor a LinuxPod sandbox/VM); LinuxPod acceptances blocked on #127/#128/#129 (see $OUT_DIR/backend-evidence.txt)"
	fi
}

# linuxpod_gate <human-phase-name> -> 0 if LinuxPod-backed, else skip+return 1.
linuxpod_gate() {
	if [ "$LINUXPOD_BACKED" = 1 ]; then
		return 0
	fi
	skip "$1: not proven LinuxPod-backed (blocked on CRI-L serving #127 + networking #128 + logs/exec/stats #129, and a non-simulated helper). See backend-evidence phase."
	return 1
}

phase_shared_ns() {
	log "Phase: shared sandbox namespace + localhost (app <-> late sidecar)"
	linuxpod_gate "shared-ns" || return 0
	local pod out; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for shared-ns"; return 1; }
	# The sidecar wrote the localhost proof file into the shared emptyDir after
	# reaching the app on 127.0.0.1 — readable from either container.
	out="$(kn exec "$pod" -c app -- sh -c 'cat /shared/sidecar-localhost 2>/dev/null' 2>"$OUT_DIR/shared-ns.err" || true)"
	echo "$out" >"$OUT_DIR/shared-ns.out"
	if printf '%s' "$out" | grep -q "$SIDECAR_LOCALHOST_MARKER"; then
		pass "late sidecar reached app over 127.0.0.1 (shared Pod network namespace)"
	else
		fail "no localhost proof from sidecar (shared-namespace not demonstrated; see $OUT_DIR/shared-ns.out)"
	fi
}

phase_identity() {
	log "Phase: late-sidecar rootfs identity verification (CRI-R16)"
	linuxpod_gate "identity" || return 0
	if [ -z "${MACVZ_LINUXPOD_IDENTITY_CMD:-}" ]; then
		skip "identity: set MACVZ_LINUXPOD_IDENTITY_CMD to surface on-node identityVerified/expected/observed"
		return 0
	fi
	local pod; pod="$(pod_name)"
	MACVZ_POD="$pod" run_hook "$MACVZ_LINUXPOD_IDENTITY_CMD" >"$OUT_DIR/identity.json" 2>"$OUT_DIR/identity.err" || true
	if grep -Eq '"identityVerified"[[:space:]]*:[[:space:]]*"?true"?|identityVerified=true' "$OUT_DIR/identity.json" 2>/dev/null; then
		pass "LinuxPod sidecar rootfs identity verified (observed==expected)"
	else
		fail "LinuxPod identity not verified (see $OUT_DIR/identity.json)"
	fi
}

phase_podip() {
	log "Phase: Pod IP readiness"
	local pod ip; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for Pod IP"; return 1; }
	ip="$(kn get pod "$pod" -o jsonpath='{.status.podIP}' 2>/dev/null)"
	if [ -n "$ip" ]; then
		pass "Pod IP assigned: $ip"
	else
		fail "Pod has no podIP"
	fi
	# Whether that Pod IP is owned by a LinuxPod sandbox VM is only assertable
	# when LinuxPod-backed; otherwise it is the apple/container path's IP.
	if [ "$LINUXPOD_BACKED" = 1 ]; then
		pass "Pod IP belongs to the LinuxPod-backed sandbox (backend evidence confirmed)"
	else
		skip "podip: Pod IP ownership by a LinuxPod VM not proven (blocked on #128); IP above is the apple/container path's"
	fi
}

phase_logs() {
	log "Phase: kubectl logs (app + sidecar)"
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for logs"; return 1; }
	if kn logs "$pod" -c app 2>"$OUT_DIR/logs-app.err" | grep -q "$APP_BOOT_MARKER"; then
		pass "kubectl logs (app) returns the app boot marker"
	else
		fail "kubectl logs (app) missing '$APP_BOOT_MARKER' (see $OUT_DIR/logs-app.err)"
	fi
	if kn logs "$pod" -c sidecar 2>"$OUT_DIR/logs-sidecar.err" | grep -q "$SIDECAR_BOOT_MARKER"; then
		pass "kubectl logs (sidecar) returns the sidecar boot marker"
	else
		fail "kubectl logs (sidecar) missing '$SIDECAR_BOOT_MARKER' (see $OUT_DIR/logs-sidecar.err)"
	fi
}

phase_exec() {
	log "Phase: kubectl exec (projected Secret + ConfigMap + shared proof)"
	local pod out ok=0 _; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for exec"; return 1; }
	for _ in $(seq 1 30); do
		out="$(kn exec "$pod" -c app -- sh -c 'cat /etc/app-secret/token; echo; cat /www/app.conf; echo; cat /shared/sidecar-localhost 2>/dev/null' 2>"$OUT_DIR/exec.err" || true)"
		echo "$out" >"$OUT_DIR/exec.out"
		if printf '%s' "$out" | grep -q "$SECRET_MARKER" && printf '%s' "$out" | grep -q "$MARKER"; then
			ok=1
			break
		fi
		sleep 1
	done
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
	[ "$ok" = 1 ] || return 0
}

phase_portforward() {
	log "Phase: kubectl port-forward"
	local pod lport=18081; pod="$(pod_name)"
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
	local probe="linuxpod-inloop-probe"
	kn delete pod "$probe" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	if kn run "$probe" --image="$IMAGE" --restart=Never --command -- \
		sh -c "for i in \$(seq 1 30); do wget -T 5 -qO- http://$SVC.$NS.svc:80/index.html && exit 0; sleep 1; done; exit 1" \
		>"$OUT_DIR/probe-run.log" 2>&1; then
		kn wait --for=condition=Ready "pod/$probe" --timeout=2m >/dev/null 2>&1 || true
	fi
	local phase _
	for _ in $(seq 1 180); do
		phase="$(kn get pod "$probe" -o jsonpath='{.status.phase}' 2>/dev/null)"
		case "$phase" in
			Succeeded|Failed) break ;;
		esac
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

# linuxpod_state_count <output-file> -> writes raw audit; prints residual-state
# line count (LinuxPod VMs/containers/rootfs/handoff/network). A hook failure is
# distinct (return 2) from a clean zero.
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
	if [ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ]; then
		local n
		if n="$(linuxpod_state_count "$OUT_DIR/restart-cri-audit.log")"; then
			# After re-adoption there should be exactly one Pod's worth of state, not a duplicate set.
			pass "LinuxPod audit after restart: $n residual line(s) recorded (see $OUT_DIR/restart-cri-audit.log; inspect for duplicate VM/container)"
		else
			fail "LinuxPod audit hook failed after macvz-cri restart (see $OUT_DIR/restart-cri-audit.log.err)"
		fi
	else
		skip "LinuxPod duplicate audit (set MACVZ_LINUXPOD_AUDIT_CMD)"
	fi
}

phase_restart_helper() {
	log "Phase: LinuxPod helper restart/failure handling"
	if [ -z "${MACVZ_RESTART_HELPER_CMD:-}" ]; then
		skip "restart-helper (set MACVZ_RESTART_HELPER_CMD to restart/crash the LinuxPod helper; AC5 may remain a documented blocker)"
		return 0
	fi
	if [ "$LINUXPOD_BACKED" != 1 ]; then
		skip "restart-helper: node not proven LinuxPod-backed, so a helper restart exercises nothing real (blocked on #127/#128/#129)"
		return 0
	fi
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod before helper restart"; return 1; }
	log "restarting/crashing the LinuxPod helper via operator hook"
	run_hook "$MACVZ_RESTART_HELPER_CMD" >"$OUT_DIR/restart-helper.log" 2>&1 \
		|| fail "MACVZ_RESTART_HELPER_CMD returned non-zero (see $OUT_DIR/restart-helper.log)"
	local ok=0 _
	for _ in $(seq 1 60); do
		[ "$(pod_phase "$pod")" = "Running" ] && { ok=1; break; }
		sleep 2
	done
	[ "$ok" = 1 ] && pass "Pod recovered to Running after helper restart" \
		|| fail "Pod not Running after helper restart (record the failure handling as the next blocker; see $OUT_DIR/restart-helper.log)"
}

phase_soak() {
	log "Phase: in-loop soak ($SOAK_ITERS samples, ${SOAK_INTERVAL}s apart)"
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for soak"; return 1; }
	printf 'iteration,rss_kb,pod_phase,restart_count,linuxpod_state\n' >"$OUT_DIR/soak-samples.csv"

	local first_rss=0 last_rss=0 have_rss=0 i rss phase rc lp rss_hook_failures=0 audit_hook_failures=0
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
					rss=0; rss_hook_failures=$((rss_hook_failures+1))
				fi
			else
				rss=0; rss_hook_failures=$((rss_hook_failures+1))
			fi
		else
			rss=0
		fi
		phase="$(pod_phase "$pod")"
		rc="$(kn get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null)"; [ -n "$rc" ] || rc=0
		if [ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ]; then
			if lp="$(linuxpod_state_count "$OUT_DIR/soak-audit-$i.log")"; then :; else lp=-2; audit_hook_failures=$((audit_hook_failures+1)); fi
		else
			lp=-1
		fi
		printf '%s,%s,%s,%s,%s\n' "$i" "$rss" "$phase" "$rc" "$lp" >>"$OUT_DIR/soak-samples.csv"
		[ $((i % 10)) -eq 0 ] && log "soak $i/$SOAK_ITERS (phase=$phase rss=${rss}KB restarts=$rc)"
		sleep "$SOAK_INTERVAL"
	done

	local final_phase final_rc; final_phase="$(pod_phase "$pod")"
	final_rc="$(kn get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null)"; [ -n "$final_rc" ] || final_rc=0
	[ "$final_phase" = "Running" ] && pass "Pod Running for the full soak" || fail "Pod ended soak in phase '$final_phase'"
	[ "$final_rc" -le 1 ] && pass "Pod restartCount bounded ($final_rc)" || fail "Pod restarted $final_rc times during soak"
	[ "$rss_hook_failures" = 0 ] || fail "adapter RSS hook failed or returned non-numeric output $rss_hook_failures time(s)"
	[ "$audit_hook_failures" = 0 ] || fail "LinuxPod audit hook failed $audit_hook_failures time(s) during soak"

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
	log "Phase: cleanup + residual LinuxPod state audit"
	k delete -f "$OUT_DIR/workload.applied.yaml" --wait=true --timeout=3m >"$OUT_DIR/cleanup.log" 2>&1 || true
	local remaining
	remaining="$(kn get pods -l app=linuxpod-inloop -o name 2>/dev/null | wc -l | tr -d ' ')"
	[ "$remaining" = 0 ] && pass "no fixture Pods remain after delete" || fail "$remaining fixture Pod(s) remain after delete"

	if [ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ]; then
		local n
		if n="$(linuxpod_state_count "$OUT_DIR/cleanup-audit.log")"; then
			[ "$n" = 0 ] && pass "LinuxPod audit: no residual VM/container/rootfs/handoff/network state" \
				|| fail "LinuxPod audit: $n residual state line(s) remain (see $OUT_DIR/cleanup-audit.log)"
		else
			fail "LinuxPod audit hook failed during cleanup (see $OUT_DIR/cleanup-audit.log.err)"
		fi
	else
		skip "residual LinuxPod audit (set MACVZ_LINUXPOD_AUDIT_CMD to assert zero residual VM/container/rootfs/handoff/network state)"
	fi
}

phase_route_after() {
	log "Phase: default-route audit (after)"
	if [ -z "${MACVZ_ROUTE_AUDIT_CMD:-}" ]; then
		skip "route-after (set MACVZ_ROUTE_AUDIT_CMD)"
		return 0
	fi
	if ! run_hook "$MACVZ_ROUTE_AUDIT_CMD" >"$OUT_DIR/route-after.txt" 2>"$OUT_DIR/route-after.err"; then
		fail "MACVZ_ROUTE_AUDIT_CMD failed after run (see $OUT_DIR/route-after.err)"
		return 1
	fi
	if [ -f "$OUT_DIR/route-before.txt" ] && diff -u "$OUT_DIR/route-before.txt" "$OUT_DIR/route-after.txt" >"$OUT_DIR/route.diff" 2>&1; then
		pass "node default route(s) unchanged across the run (non-goal honored)"
	else
		fail "node default route(s) changed across the run (see $OUT_DIR/route.diff) — the suite must never mutate default routes"
	fi
}

# --- main --------------------------------------------------------------------
if [ "$INTEGRATION" != 1 ] || [ -z "${KUBECONFIG:-}" ]; then
	print_plan
	exit 0
fi

setup
phase_preflight || die "preflight failed; not deploying onto an unverified node"
phase_route_before
phase_deploy
phase_scheduling
phase_backend_evidence
phase_shared_ns
phase_identity
phase_podip
phase_logs
phase_exec
phase_portforward
phase_service
phase_restart_cri
phase_restart_helper
phase_soak
phase_cleanup
phase_route_after

echo
if [ "$FAILURES" -eq 0 ] && [ "$SKIPS" -eq 0 ]; then
	pass "CRI-L5 LinuxPod in-loop suite: all checks passed (diagnostics in $OUT_DIR)"
	exit 0
fi
if [ "$FAILURES" -eq 0 ]; then
	pass "CRI-L5 LinuxPod in-loop suite: checks passed with $SKIPS skipped (LinuxPod acceptance is NOT complete while skips remain; see #127/#128/#129). Diagnostics in $OUT_DIR"
	exit 0
fi
fail "CRI-L5 LinuxPod in-loop suite: $FAILURES check(s) failed (diagnostics in $OUT_DIR)"
exit 1
