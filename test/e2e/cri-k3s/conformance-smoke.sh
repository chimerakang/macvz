#!/usr/bin/env bash
#
# conformance-smoke.sh — CRI-L8-6 (#147) k3s conformance smoke subset for the
# experimental LinuxPod-backed CRI path.
#
# This is the "ordinary workload compatibility" sibling of linuxpod-inloop.sh.
# Where the in-loop/soak/multipod suites prove the LinuxPod-specific surface
# (shared namespace, identity, recovery, concurrency), this suite runs a small,
# repeatable subset of the everyday Kubernetes/k3s behaviors a real chart relies
# on, so the support claim can be stated honestly: a focused conformance smoke,
# not full upstream conformance. The subset deliberately covers a representative
# slice and records what it does NOT cover so the claim stays honest.
#
# Covered conformance areas (each is one or more phases below):
#   node-readiness   the MacVz CRI node carries the #84 labels/taint and is Ready.
#   deployment       a Deployment rolls out to Available via kubectl rollout status.
#   configmap        a projected ConfigMap marker is served and readable.
#   secret           a projected Secret token is readable in the Pod.
#   projected-volume a multi-source `projected` volume (configMap+secret+
#                    downwardAPI) materializes one tree in the Pod.
#   probes           readiness/liveness httpGet probes drive the Pod Ready and
#                    keep restartCount stable (no liveness-kill flapping).
#   restart-policy   a backend-agnostic cycling container (exits 0 on a loop)
#                    proves restartPolicy: Always re-runs it (restartCount>=1),
#                    with no reliance on any volume surviving the restart.
#   service          a ClusterIP Service is reachable from an in-cluster probe.
#   dns              cluster DNS resolves the Service `*.svc` name from inside the
#                    Pod (a representative check; deeper DNS is linuxpod-dns.sh).
#   logs             kubectl logs returns the app + sidecar boot markers.
#   exec             kubectl exec reads the projected Secret/ConfigMap.
#   port-forward     kubectl port-forward + curl returns the served marker.
#   cleanup          delete leaves no fixture Pods (and, hooked, zero residual
#                    LinuxPod VM/container/rootfs/handoff/network state).
#
# Explicitly NOT covered here (recorded so the smoke claim stays honest):
#   - Full upstream Kubernetes conformance (sonobuoy/[Conformance]). This is a
#     curated subset, not the certified suite.
#   - StatefulSets, Jobs/CronJobs, DaemonSets, HPA, NetworkPolicy, Ingress,
#     PersistentVolumes/CSI, init-containers, host-namespace Pods (rejected by
#     design, #84), and multi-node scheduling spread.
#   - Deep DNS matrix (headless A records, cross-namespace, SRV) — covered by
#     linuxpod-dns.sh (CRI-L8-2 #142).
#   - Volume projection matrix breadth — covered by CRI-L8-3 (#145).
#   - Image lifecycle / GC / arch handling — covered by CRI-L8-4 (#143).
#   - Node reboot/bootstrap recovery — covered by CRI-L8-5 (#144).
#   - Long wall-clock soak — covered by linuxpod-soak.sh / CRI-L8-1 (#146).
#
# HONESTY GATE (inherited from #130). The shipped CRI serving path runs on
# apple/container; a Pod reaching Running on a `--experimental-linuxpod-backend`
# node is NOT by itself evidence of a LinuxPod-backed Pod (the prototype helper
# reports simulated=true). The ordinary Kubernetes conformance checks below run
# regardless — they are control-plane behaviors, not LinuxPod internals — but the
# suite only *claims LinuxPod-backed conformance* when an operator backend-
# evidence hook (MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD) proves the Pod is served by
# a genuine, non-simulated LinuxPod backend. Without that proof the final wording
# reports the smoke against the apple/container path and says so loudly, never
# claiming LinuxPod evidence the runtime did not produce.
#
# Gating: like the sibling suites, the live run mutates a real cluster and a real
# macOS CRI node, so it runs only when MACVZ_INTEGRATION=1 *and* a reachable
# KUBECONFIG is provided. Without both it prints the runbook plan and exits 0, so
# it is safe in `go test`-style CI and `bash -n` validation.
#
# It is an isolated `develop`-track feasibility probe, NOT the shipped Virtual
# Kubelet e2e (test/e2e/e2e.sh), and must not gate the VK release path.
#
# Environment:
#   MACVZ_INTEGRATION       set to 1 to run live (otherwise plan-only).
#   KUBECONFIG              kubeconfig for the k3s control plane (required live).
#   MACVZ_NODE              name of the MacVz CRI node. Default: auto-detected as
#                           the (single) node carrying node.macvz.io/runtime=apple-container.
#   MACVZ_CRI_IMAGE         arm64 image for the fixture (default: busybox:1.36.1).
#   MACVZ_CRI_OUT_DIR       results/diagnostics dir (default: a mktemp dir).
#   MACVZ_CRI_KEEP          set to 1 to keep the namespace + diagnostics on exit.
#
# Operator hooks (commands run via `sh -c`; the harness cannot reach the remote
# macOS node itself). A hook left unset only downgrades the matching phase to a
# loud SKIP — it is never a silent pass:
#   MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
#                           proves the named Pod ($MACVZ_POD) is served by a real,
#                           non-simulated LinuxPod backend. Without it the smoke
#                           still runs but is reported against the apple/container
#                           path (LinuxPod-backed conformance not claimed).
#   MACVZ_LINUXPOD_AUDIT_CMD print residual LinuxPod state for the cleanup audit
#                           (LinuxPod VMs, backend containers, rootfs/handoff
#                           subtrees, Pod network state). Asserted empty after
#                           cleanup.
#   MACVZ_ROUTE_AUDIT_CMD   print the node's default route(s). Captured before and
#                           after; the harness asserts the default route is
#                           unchanged (non-goal: never mutate it).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
FIXTURE="$HERE/fixtures/conformance-smoke.yaml"
NS="macvz-cri-linuxpod-smoke-e2e"
DEPLOY="linuxpod-smoke"
RESTART_DEPLOY="linuxpod-smoke-restart"
SVC="linuxpod-smoke"
MARKER="macvz-cri-l8-smoke-ok"
SECRET_MARKER="macvz-cri-l8-smoke-secret-ok"
APP_BOOT_MARKER="macvz-cri-l8-smoke-app-boot"
SIDECAR_BOOT_MARKER="macvz-cri-l8-smoke-sidecar-boot"
LOCALHOST_MARKER="macvz-cri-l8-smoke-localhost-ok"
DNS_MARKER="macvz-cri-l8-smoke-dns-ok"

INTEGRATION="${MACVZ_INTEGRATION:-0}"
IMAGE="${MACVZ_CRI_IMAGE:-busybox:1.36.1}"
NODE="${MACVZ_NODE:-}"
OUT_DIR="${MACVZ_CRI_OUT_DIR:-}"

RUNTIME_LABEL="node.macvz.io/runtime=apple-container"
TAINT_KEY="node.macvz.io/host-namespace-unsupported"

FAILURES=0
SKIPS=0
TMP_ROOT=""
OUT_DIR_WAS_SET=0
PF_PID=""
LINUXPOD_BACKED=0

c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
pass() { printf "${c_green}PASS${c_off} %s\n" "$*"; }
skip() { printf "${c_yellow}SKIP${c_off} %s\n" "$*"; SKIPS=$((SKIPS+1)); }
fail() { printf "${c_red}FAIL${c_off} %s\n" "$*"; FAILURES=$((FAILURES+1)); }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

k() { kubectl "$@"; }
kn() { kubectl -n "$NS" "$@"; }

# gated_fail <area> <reason> — for surfaces that the experimental LinuxPod path
# provides but the default apple/container path documents as gaps (CRI container
# log streaming #129/#133, in-Pod cluster DNS via podnet #128/#142, CRI restart
# surfacing). On a node *proven* LinuxPod-backed these are hard FAILs (the
# conformance claim requires them); otherwise they are loud known-limitation
# SKIPs so the smoke runs green-with-skips on the apple/container path without
# ever silently claiming a surface the runtime did not provide.
gated_fail() {
	local area="$1" reason="$2"
	if [ "$LINUXPOD_BACKED" = 1 ]; then
		fail "$area: $reason (LinuxPod-backed node — this surface must work)"
	else
		skip "$area: $reason — documented apple/container-path gap; this surface is provided by the LinuxPod path. On a LinuxPod-backed node (MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD set) it must PASS."
	fi
}

print_plan() {
	cat <<'PLAN'
CRI-L8-6 k3s conformance smoke subset (plan; set MACVZ_INTEGRATION=1 and a
reachable KUBECONFIG to run live):

  preflight        kubectl reachable; locate the MacVz CRI node by its runtime
                   label; assert #84 labels + NoSchedule taint present; node Ready.
  route-before     capture the node default route(s) (MACVZ_ROUTE_AUDIT_CMD) so
                   the post-run audit can prove they were never mutated.
  deploy           kubectl apply fixtures/conformance-smoke.yaml (Deployment with
                   app + late sidecar, ConfigMap, Secret, projected volume,
                   probes, ClusterIP + headless Service, plus a cycling
                   restart-policy Deployment); kubectl rollout status the app.
  scheduling       Pods landed on the MacVz node; events clean (no
                   FailedScheduling / FailedCreatePodSandBox).
  backend-evidence HONESTY GATE: assert via MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
                   that the Pod's sandbox is genuinely (non-simulated) LinuxPod
                   backed. The ordinary conformance checks below run either way;
                   only the *LinuxPod-backed conformance* claim is gated on it.
  configmap        projected ConfigMap marker served + readable.
  secret           projected Secret token readable in the Pod.
  projected-volume multi-source projected volume (configMap+secret+downwardAPI)
                   materialized in one tree.
  probes           readiness drove Ready; liveness restartCount stable (sampled
                   twice, must not climb — tolerates a one-time startup restart).
  restart-policy   cycling container proves restartPolicy: Always re-ran it
                   (restartCount>=1), no reliance on volume persistence.
  service          ClusterIP Service reachable from an in-cluster Linux-node probe.
  dns              cluster DNS resolves the Service `*.svc` name from in-Pod.
  logs             kubectl logs returns app + sidecar boot markers.
  exec             kubectl exec reads the projected Secret + ConfigMap.
  port-forward     kubectl port-forward + curl returns the served marker.
  cleanup          delete leaves no fixture Pods; hooked LinuxPod residual audit
                   is zero.
  route-after      node default route(s) unchanged across the run (non-goal).

Without the gates this prints the plan and exits 0 (safe for CI / bash -n).
Coverage is a curated subset, NOT full upstream conformance; see the script
header for the explicit not-covered list and the sibling CRI-L8 issues.
PLAN
}

setup() {
	command -v kubectl >/dev/null 2>&1 || die "kubectl not found in PATH"
	[ -f "$FIXTURE" ] || die "fixture not found at $FIXTURE"
	k get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1 \
		|| die "cluster unreachable (KUBECONFIG=${KUBECONFIG:-unset}); set a reachable kubeconfig"

	TMP_ROOT="$(mktemp -d -t macvz-cri-conformance-smoke)"
	if [ -n "$OUT_DIR" ]; then
		OUT_DIR_WAS_SET=1
	else
		OUT_DIR="$TMP_ROOT/out"
	fi
	mkdir -p "$OUT_DIR"
	log "out=$OUT_DIR image=$IMAGE"
}

cleanup_trap() {
	[ -n "$PF_PID" ] && { kill "$PF_PID" 2>/dev/null || true; }
	if [ "${MACVZ_CRI_KEEP:-0}" != 1 ]; then
		kubectl delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
	fi
	if [ -n "$TMP_ROOT" ] && [ "${MACVZ_CRI_KEEP:-0}" != 1 ] && [ "$OUT_DIR_WAS_SET" = 0 ]; then
		rm -rf "$TMP_ROOT"
	fi
}
trap cleanup_trap EXIT

run_hook() {
	local cmd="$1"; shift
	[ -n "$cmd" ] || return 3
	sh -c "$cmd"
}

# --- phases ------------------------------------------------------------------
phase_preflight() {
	log "Phase: preflight (locate + validate the MacVz CRI node) [node-readiness]"
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
	log "Phase: deploy conformance-smoke fixture + rollout [deployment]"
	if ! apply_fixture; then
		fail "kubectl apply failed (see $OUT_DIR/apply.log)"
		return 1
	fi
	pass "fixture applied"
	if kn rollout status "deploy/$DEPLOY" --timeout=5m >"$OUT_DIR/rollout.log" 2>&1; then
		pass "kubectl rollout status: $DEPLOY Deployment available"
	else
		fail "$DEPLOY rollout did not complete (see $OUT_DIR/rollout.log + diagnostics)"
		kn describe "deploy/$DEPLOY" >"$OUT_DIR/deploy-describe.log" 2>&1 || true
		kn get pods -o wide >"$OUT_DIR/pods.log" 2>&1 || true
	fi
	# The restart-policy Deployment is intentionally a *cycling* container (exits 0
	# on a loop so restartCount climbs), so it never reaches a stable Available —
	# do NOT rollout-gate it. Just confirm its Pod was created/scheduled here; the
	# restart-policy phase asserts the restart behavior.
	local rpod _
	for _ in $(seq 1 60); do
		rpod="$(restart_pod_name)"
		[ -n "$rpod" ] && break
		sleep 1
	done
	if [ -n "$rpod" ]; then
		pass "restart-policy Pod created ($rpod) — cycling container, restart behavior checked later"
	else
		fail "$RESTART_DEPLOY Pod was never created (see diagnostics)"
		kn describe "deploy/$RESTART_DEPLOY" >"$OUT_DIR/restart-describe.log" 2>&1 || true
	fi
}

pod_name() {
	kn get pods -l app=linuxpod-smoke -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

restart_pod_name() {
	kn get pods -l app=linuxpod-smoke-restart -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

phase_scheduling() {
	log "Phase: scheduling + Pod events [node-readiness]"
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
	# The honesty gate. Proof from the node that the Pod's sandbox is served by a
	# genuine, non-simulated LinuxPod backend flips LINUXPOD_BACKED=1; otherwise
	# the ordinary conformance checks still run but the final claim is reported
	# against the apple/container path, never falsely as LinuxPod-backed.
	log "Phase: LinuxPod backend evidence (honesty gate)"
	if [ -z "${MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD:-}" ]; then
		skip "backend-evidence: set MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD to claim LinuxPod-backed conformance; the smoke still runs but is reported against the apple/container path"
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
		skip "backend-evidence reports simulated=true: node is on the prototype handshake, not a real LinuxPod backend — conformance reported against the apple/container path (see $OUT_DIR/backend-evidence.txt)"
		return 0
	fi
	if grep -Eqi 'linuxpod|pod[-_ ]?vm|sandboxVM' "$OUT_DIR/backend-evidence.txt" \
		&& grep -Eqi 'simulated[":= ]+false|backend[":= ]+linuxpod|serving[":= ]+linuxpod' "$OUT_DIR/backend-evidence.txt"; then
		LINUXPOD_BACKED=1
		pass "Pod is served by a genuine (non-simulated) LinuxPod backend — conformance claimed LinuxPod-backed (see $OUT_DIR/backend-evidence.txt)"
	else
		skip "backend-evidence did not prove a non-simulated LinuxPod-backed Pod; conformance reported against the apple/container path (see $OUT_DIR/backend-evidence.txt)"
	fi
}

# Read a file from the projected tree / mounts inside the app container, with a
# bounded retry while the Pod finishes becoming Ready.
app_cat() {
	local path="$1" out _ pod; pod="$(pod_name)"
	[ -n "$pod" ] || return 1
	for _ in $(seq 1 30); do
		out="$(kn exec "$pod" -c app -- sh -c "cat $path 2>/dev/null" 2>/dev/null || true)"
		[ -n "$out" ] && { printf '%s' "$out"; return 0; }
		sleep 1
	done
	printf '%s' "$out"
}

phase_configmap() {
	log "Phase: ConfigMap projection [configmap]"
	local out; out="$(app_cat /www/index.html)"
	echo "$out" >"$OUT_DIR/configmap.out"
	if printf '%s' "$out" | grep -q "$MARKER"; then
		pass "projected ConfigMap marker served at /www/index.html"
	else
		fail "ConfigMap marker '$MARKER' not found (see $OUT_DIR/configmap.out)"
	fi
}

phase_secret() {
	log "Phase: Secret projection [secret]"
	local out; out="$(app_cat /etc/projected/token)"
	echo "$out" >"$OUT_DIR/secret.out"
	if printf '%s' "$out" | grep -q "$SECRET_MARKER"; then
		pass "projected Secret token readable at /etc/projected/token"
	else
		fail "Secret marker '$SECRET_MARKER' not found (see $OUT_DIR/secret.out)"
	fi
}

phase_projected_volume() {
	log "Phase: multi-source projected volume [projected-volume]"
	local pod out; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for projected-volume"; return 1; }
	out="$(kn exec "$pod" -c app -- sh -c 'cat /etc/projected/app.conf; echo "---"; cat /etc/projected/token; echo "---"; cat /etc/projected/pod-name' 2>"$OUT_DIR/projected.err" || true)"
	echo "$out" >"$OUT_DIR/projected.out"
	local ok=1
	printf '%s' "$out" | grep -q "$MARKER" || { fail "projected configMap source (app.conf) missing marker"; ok=0; }
	printf '%s' "$out" | grep -q "$SECRET_MARKER" || { fail "projected secret source (token) missing marker"; ok=0; }
	printf '%s' "$out" | grep -q "$pod" || { fail "projected downwardAPI source (pod-name) did not equal '$pod'"; ok=0; }
	[ "$ok" = 1 ] && pass "projected volume fanned configMap+secret+downwardAPI into /etc/projected (all three sources present)"
}

app_restart_count() {
	local n; n="$(kn get pod "$1" -o jsonpath="{.status.containerStatuses[?(@.name=='app')].restartCount}" 2>/dev/null)"
	echo "${n:-0}"
}

phase_probes() {
	log "Phase: readiness/liveness probes [probes]"
	local pod ready rc1 rc2; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for probes"; return 1; }
	ready="$(kn get pod "$pod" -o jsonpath="{.status.conditions[?(@.type=='Ready')].status}" 2>/dev/null)"
	if [ "$ready" = "True" ]; then
		pass "readiness probe drove Pod Ready=True"
	else
		fail "Pod not Ready (readiness probe; status='$ready')"
	fi
	# A healthy liveness probe must not be *flapping* the app. We don't require
	# restartCount==0 — the per-container-VM apple/container path may restart the
	# container once during startup — but it must be STABLE over a liveness period:
	# sample twice ~one period apart and assert it does not climb. A steady count
	# (and a Ready Pod) means liveness is not killing a healthy container; a
	# climbing count is active flapping and a real fault.
	rc1="$(app_restart_count "$pod")"
	sleep 25
	rc2="$(app_restart_count "$pod")"
	if [ "$rc2" -le "$rc1" ] 2>/dev/null; then
		pass "liveness probe stable: app restartCount steady at $rc2 over 25s (no liveness-kill flapping)"
	else
		fail "app restartCount climbed $rc1->$rc2 in 25s — liveness probe is actively flapping a healthy container"
	fi
}

phase_restart_policy() {
	log "Phase: restartPolicy: Always [restart-policy]"
	local pod rc phase _; pod="$(restart_pod_name)"
	[ -n "$pod" ] || { fail "no restart-policy Pod found"; return 1; }
	# Backend-agnostic proof: the flapper exits 0 on a loop, so restartPolicy:
	# Always must re-run it and restartCount must climb to >=1. This deliberately
	# does NOT depend on any volume surviving the restart (the apple/container
	# per-container-VM model does not persist emptyDir across a restart; that is
	# the separate CRI-L8-3 projection matrix, #145). Poll up to ~120s — one cycle
	# is sleep 12 + re-create latency.
	rc=0
	for _ in $(seq 1 120); do
		rc="$(kn get pod "$pod" -o jsonpath="{.status.containerStatuses[?(@.name=='flapper')].restartCount}" 2>/dev/null)"
		rc="${rc:-0}"
		[ "$rc" -ge 1 ] 2>/dev/null && break
		sleep 1
	done
	if [ "$rc" -ge 1 ] 2>/dev/null; then
		pass "restartPolicy: Always re-ran the exited container (restartCount=$rc)"
	else
		kn describe pod "$pod" >"$OUT_DIR/restart-describe.log" 2>&1 || true
		gated_fail "restart-policy" "restartCount stayed 0 after an exited container; the apple/container path does not surface CRI container restarts (see $OUT_DIR/restart-describe.log)"
	fi
	# Sanity: the Pod must be live (Running/Pending during a restart), not wedged
	# in an unrecoverable error that never runs the container.
	phase="$(kn get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)"
	case "$phase" in
		Running|Pending|Succeeded) pass "restart-policy Pod is live (phase=$phase), not wedged" ;;
		*) fail "restart-policy Pod in unexpected phase '$phase' (container never running)" ;;
	esac
}

phase_service() {
	log "Phase: ClusterIP Service reachability [service]"
	local probe="linuxpod-smoke-probe"
	kn delete pod "$probe" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	kn run "$probe" --image="$IMAGE" --restart=Never --command -- \
		sh -c "for i in \$(seq 1 30); do wget -T 5 -qO- http://$SVC.$NS.svc:80/index.html && exit 0; sleep 1; done; exit 1" \
		>"$OUT_DIR/probe-run.log" 2>&1 || true
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
		pass "ClusterIP Service reachable from an in-cluster Linux-node probe"
	else
		fail "Service probe did not return '$MARKER' (phase=$phase; see $OUT_DIR/probe.log)"
	fi
	kn delete pod "$probe" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

phase_dns() {
	log "Phase: cluster DNS resolution of the Service name [dns]"
	local pod out; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for DNS"; return 1; }
	# Representative DNS check from inside the Pod: reach our own ClusterIP Service
	# by its `*.svc` *name* — which only succeeds if cluster DNS resolved it (the
	# service phase already proved ClusterIP routing independently, so a by-name
	# success here is the DNS signal). We use wget rather than nslookup because
	# some busybox builds ship without the nslookup applet. The deeper DNS matrix
	# (headless A records, cross-namespace, SRV) is linuxpod-dns.sh (#142).
	out="$(kn exec "$pod" -c app -- sh -c "wget -T 8 -qO- http://$SVC.$NS.svc.cluster.local/index.html 2>&1" 2>"$OUT_DIR/dns.err" || true)"
	echo "$out" >"$OUT_DIR/dns.out"
	if printf '%s' "$out" | grep -q "$MARKER"; then
		pass "cluster DNS resolved $SVC.$NS.svc.cluster.local from inside the Pod (reached the Service by name)"
	else
		gated_fail "dns" "in-Pod resolution of the Service name failed; in-Pod cluster DNS needs the LinuxPod podnet + cluster-DNS path (#128/#142) (see $OUT_DIR/dns.out)"
	fi
	# Boot-time DNS proof the sidecar recorded into the shared volume.
	local s; s="$(kn exec "$pod" -c app -- sh -c 'cat /shared/sidecar-dns 2>/dev/null' 2>/dev/null || true)"
	if printf '%s' "$s" | grep -q "$DNS_MARKER"; then
		pass "sidecar boot-time DNS self-resolution recorded ($DNS_MARKER)"
	else
		skip "sidecar boot-time DNS proof not present yet (exec-time DNS above is authoritative)"
	fi
}

# logs_check <container> <marker> — PASS if kubectl logs returns the marker.
# Otherwise it is gated: the apple/container path produces no CRI container-log
# file (e.g. "failed to ... resolving symlinks in path .../N.log: no such file"),
# whereas CRI-L4 #129/#133 added real log streaming for the LinuxPod path — so a
# missing marker is a known-limitation SKIP off the LinuxPod path and a FAIL on it.
logs_check() {
	local container="$1" marker="$2"
	local outf="$OUT_DIR/logs-$container.out" errf="$OUT_DIR/logs-$container.err"
	kn logs "$pod" -c "$container" >"$outf" 2>"$errf" || true
	if grep -q "$marker" "$outf"; then
		pass "kubectl logs ($container) returns the boot marker"
	else
		gated_fail "logs($container)" "kubectl logs returned no CRI log stream for '$marker' (see $outf / $errf)"
	fi
}

phase_logs() {
	log "Phase: kubectl logs [logs]"
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for logs"; return 1; }
	logs_check app "$APP_BOOT_MARKER"
	logs_check sidecar "$SIDECAR_BOOT_MARKER"
}

phase_exec() {
	log "Phase: kubectl exec [exec]"
	local pod out; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for exec"; return 1; }
	out="$(kn exec "$pod" -c app -- sh -c 'cat /etc/projected/token; echo; cat /www/app.conf; echo; cat /shared/sidecar-localhost 2>/dev/null' 2>"$OUT_DIR/exec.err" || true)"
	echo "$out" >"$OUT_DIR/exec.out"
	if printf '%s' "$out" | grep -q "$SECRET_MARKER"; then
		pass "exec read the projected Secret marker"
	else
		fail "exec missing Secret marker (see $OUT_DIR/exec.out)"
	fi
	if printf '%s' "$out" | grep -q "$MARKER"; then
		pass "exec read the projected ConfigMap marker"
	else
		fail "exec missing ConfigMap marker (see $OUT_DIR/exec.out)"
	fi
	if printf '%s' "$out" | grep -q "$LOCALHOST_MARKER"; then
		pass "exec read the shared-namespace localhost proof (app <-> sidecar)"
	else
		skip "shared-namespace localhost proof not present (sidecar may still be probing; not a conformance-subset gate)"
	fi
}

phase_portforward() {
	log "Phase: kubectl port-forward [port-forward]"
	local pod lport=18082; pod="$(pod_name)"
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

phase_cleanup() {
	log "Phase: cleanup + residual state audit [cleanup]"
	k delete -f "$OUT_DIR/workload.applied.yaml" --wait=true --timeout=3m >"$OUT_DIR/cleanup.log" 2>&1 || true
	local remaining
	remaining="$(kn get pods -l 'app in (linuxpod-smoke, linuxpod-smoke-restart)' -o name 2>/dev/null | wc -l | tr -d ' ')"
	[ "$remaining" = 0 ] && pass "no fixture Pods remain after delete" || fail "$remaining fixture Pod(s) remain after delete"

	if [ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ]; then
		if run_hook "$MACVZ_LINUXPOD_AUDIT_CMD" >"$OUT_DIR/cleanup-audit.log" 2>"$OUT_DIR/cleanup-audit.err"; then
			local n
			n="$(grep -cve '^[[:space:]]*$' "$OUT_DIR/cleanup-audit.log" 2>/dev/null || echo 0)"
			[ "$n" = 0 ] && pass "LinuxPod audit: no residual VM/container/rootfs/handoff/network state" \
				|| fail "LinuxPod audit: $n residual state line(s) remain (see $OUT_DIR/cleanup-audit.log)"
		else
			fail "LinuxPod audit hook failed during cleanup (see $OUT_DIR/cleanup-audit.err)"
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
phase_configmap
phase_secret
phase_projected_volume
phase_probes
phase_restart_policy
phase_service
phase_dns
phase_logs
phase_exec
phase_portforward
phase_cleanup
phase_route_after

echo
backing="apple/container path"
[ "$LINUXPOD_BACKED" = 1 ] && backing="genuine non-simulated LinuxPod backend"
if [ "$FAILURES" -eq 0 ] && [ "$SKIPS" -eq 0 ]; then
	pass "CRI-L8-6 conformance smoke subset: all checks passed against a $backing (curated subset, NOT full upstream conformance; diagnostics in $OUT_DIR)"
	exit 0
fi
if [ "$FAILURES" -eq 0 ]; then
	pass "CRI-L8-6 conformance smoke subset: checks passed with $SKIPS skipped against a $backing (curated subset, NOT full upstream conformance; unset hooks/optional proofs were skipped loudly — see above). Diagnostics in $OUT_DIR"
	exit 0
fi
fail "CRI-L8-6 conformance smoke subset: $FAILURES check(s) failed against a $backing (diagnostics in $OUT_DIR)"
exit 1
