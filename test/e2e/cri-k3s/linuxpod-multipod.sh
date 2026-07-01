#!/usr/bin/env bash
#
# linuxpod-multipod.sh — CRI-L6-3 (#137) multi-Pod concurrency and bounded
# resource-growth validation for the experimental LinuxPod-backed CRI path.
#
# This is the multi-Pod sibling of linuxpod-inloop.sh (CRI-L5 #130). Where that
# harness proves a *single* app+late-sidecar Pod on the LinuxPod backend, this
# one proves *concurrency*: a Deployment of N (default 3) replicas — each an
# app + late sidecar sharing one Pod sandbox — scheduled by a real Linux
# k3s/kubelet control plane onto the single macOS MacVz CRI node that runs
# `macvz-cri --experimental-linuxpod-backend --linuxpod-helper-socket=…`
# (CRI-L, #125..#129). Because the node is a single MacVz host, every replica
# lands on it, so several concurrent LinuxPod micro-VMs run side by side under
# one adapter and one linuxpod-helper.
#
# It proves the #137 acceptance surface:
#   - At least 3 concurrent app+sidecar Pods reach Ready on the LinuxPod node.
#   - Each Pod receives a UNIQUE Pod IP from the node PodCIDR and stays reachable
#     through the ClusterIP Service AND a direct Pod-IP probe.
#   - Per-Pod logs, exec, and port-forward all work concurrently.
#   - Restarting macvz-cri mid-run does not duplicate sandboxes or lose Pods
#     (every Pod UID is unchanged; the residual-state count does not double).
#   - Restarting linuxpod-helper causes a BOUNDED recreate for affected Pods and
#     every Pod recovers or fails with an explicit diagnostic.
#   - No duplicate pf/binat or helper-work state is left behind.
#   - Cleanup leaves zero residual CRI/helper/network state; default route
#     unchanged.
#   - Adapter RSS and helper/process counts stay within documented bounds across
#     concurrent Pods and repeated scale churn (a pass/fail threshold).
#
# HONESTY GATE (the central #130/#137 invariant). The shipped CRI serving path
# runs on apple/container; the LinuxPod backend gate is a startup handshake
# against a helper that, for the prototype (#124/R17), reports simulated=true.
# A Pod reaching Running on such a node is NOT by itself evidence of a
# LinuxPod-backed Pod. This harness therefore REFUSES to pass the LinuxPod-
# specific acceptances unless an operator backend-evidence hook
# (MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD) proves, FOR EVERY Pod, that it is served
# by a genuine, non-simulated LinuxPod backend. Absent that proof those phases
# are SKIPped loudly with the precise #127/#128/#129 blocker — never silently
# passed. This mirrors linuxpod-inloop.sh / the #119 handoff discipline.
#
# Gating: like linuxpod-inloop.sh, the live suite mutates a real cluster and a
# real macOS CRI node, so it runs only when MACVZ_INTEGRATION=1 *and* a reachable
# KUBECONFIG is provided. Without both it prints the runbook plan and exits 0, so
# it is safe in `go test`-style CI and `bash -n` validation.
#
# It is an isolated `develop`-track feasibility probe, NOT the shipped Virtual
# Kubelet e2e (test/e2e/e2e.sh), and must not gate the VK release path.
#
# Topology (operator-provided): identical to linuxpod-inloop.sh — a Linux k3s
# control plane plus a macOS host running `macvz-cri --experimental-linuxpod-
# backend` as the external CRI endpoint for a kubelet node registered with the
# #84 labels/taint.
#
# Environment:
#   MACVZ_INTEGRATION       set to 1 to run live (otherwise plan-only).
#   KUBECONFIG              kubeconfig for the k3s control plane (required live).
#   MACVZ_NODE              name of the MacVz CRI node. Default: auto-detected as
#                           the (single) node carrying node.macvz.io/runtime=apple-container.
#   MACVZ_CRI_IMAGE         arm64 image for the fixture (default: busybox:1.36.1).
#   MACVZ_MULTIPOD_REPLICAS number of concurrent Pods (default: 3; min enforced 3).
#   MACVZ_MULTIPOD_CHURN_CYCLES  scale up/down churn cycles (default: 2).
#   MACVZ_MULTIPOD_CHURN_HIGH    replica count at the top of each churn cycle
#                           (default: replicas+2).
#   MACVZ_EXPECTED_POD_CIDRS optional space-separated Pod CIDR override for
#                           topologies where the CRI node is registered with a
#                           kubelet-assigned PodCIDR but macvz-cri is explicitly
#                           launched with a different --pod-cidr. Default: read
#                           .spec.podCIDRs/.spec.podCIDR from the node.
#   MACVZ_INLOOP_SOAK_ITERATIONS  soak sampling iterations (default: 30).
#   MACVZ_INLOOP_SOAK_INTERVAL    seconds between soak samples (default: 10).
#   MACVZ_INLOOP_RSS_GROWTH_KB    allowed adapter RSS growth before flagging a
#                           leak (default: 98304 = 96 MiB; needs MACVZ_ADAPTER_RSS_CMD).
#   MACVZ_MULTIPOD_PROC_GROWTH    allowed helper/process-count growth from the
#                           post-deploy baseline back to the same replica count
#                           after churn (default: 0; needs MACVZ_LINUXPOD_HELPER_PROC_CMD).
#   MACVZ_CRI_OUT_DIR       results/diagnostics dir (default: a mktemp dir).
#
# Operator hooks (commands run via `sh -c`; the harness cannot reach the remote
# macOS node itself). A phase whose required hook is unset is SKIPped *loudly*.
# All hooks from linuxpod-inloop.sh apply identically; per-Pod hooks receive the
# Pod name in MACVZ_POD:
#   MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
#                           REQUIRED to pass any LinuxPod-specific acceptance.
#                           Run once per Pod (MACVZ_POD set); must report
#                           simulated=false AND name a LinuxPod sandbox/VM for
#                           that Pod. Any simulated=true / apple-container /
#                           empty output keeps the LinuxPod acceptances skipped.
#   MACVZ_RESTART_CRI_CMD   restart the macvz-cri service on the CRI node.
#   MACVZ_RESTART_HELPER_CMD restart (or crash+restart) the LinuxPod helper.
#   MACVZ_ADAPTER_RSS_CMD   print the adapter's resident memory in KB (one int).
#   MACVZ_LINUXPOD_HELPER_PROC_CMD
#                           print a single integer: the number of LinuxPod-backend
#                           processes/VMs the helper is running (e.g. count of
#                           live LinuxPod micro-VMs / helper worker procs). Used
#                           to prove per-Pod process growth is bounded and returns
#                           to baseline after churn.
#   MACVZ_LINUXPOD_AUDIT_CMD print residual LinuxPod state for the orphan/cleanup
#                           audit: LinuxPod VMs, backend containers, prepared
#                           rootfs/handoff subtrees, and Pod network state. The
#                           harness asserts it is empty after cleanup, scales with
#                           (not faster than) the Pod count, and shows no
#                           duplicate after restart.
#   MACVZ_LINUXPOD_DUP_AUDIT_CMD
#                           OPTIONAL. Print one line per installed pf/binat anchor
#                           rule and per helper-work entry (e.g. each Pod's
#                           rdr/binat anchor name, each helper-work job id). The
#                           harness asserts NO line is duplicated — a duplicate is
#                           a leaked pf/binat or helper-work record. If unset the
#                           harness falls back to MACVZ_LINUXPOD_AUDIT_CMD line
#                           uniqueness, which is weaker.
#   MACVZ_ROUTE_AUDIT_CMD   print the node's default route(s). Captured before and
#                           after; the harness asserts it is unchanged.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# Shared helper functions (see lib.sh header before adding more).
. "$HERE/lib.sh"
FIXTURE="$HERE/fixtures/linuxpod-multipod-workload.yaml"
NS="macvz-cri-linuxpod-mp-e2e"
DEPLOY="linuxpod-multipod"
SVC="linuxpod-multipod"
APP_LABEL="app=linuxpod-multipod"
MARKER="macvz-cri-l6-multipod-ok"
APP_BOOT_MARKER="macvz-cri-l6-app-boot"
SIDECAR_BOOT_MARKER="macvz-cri-l6-sidecar-boot"
SIDECAR_LOCALHOST_MARKER="macvz-cri-l6-sidecar-localhost-ok"
SECRET_MARKER="macvz-cri-l6-secret-ok"
WHOAMI_MARKER="macvz-cri-l6-whoami"

INTEGRATION="${MACVZ_INTEGRATION:-0}"
IMAGE="${MACVZ_CRI_IMAGE:-busybox:1.36.1}"
NODE="${MACVZ_NODE:-}"
REPLICAS="${MACVZ_MULTIPOD_REPLICAS:-3}"
[ "$REPLICAS" -ge 3 ] 2>/dev/null || REPLICAS=3
CHURN_CYCLES="${MACVZ_MULTIPOD_CHURN_CYCLES:-2}"
CHURN_HIGH="${MACVZ_MULTIPOD_CHURN_HIGH:-$((REPLICAS + 2))}"
EXPECTED_POD_CIDRS="${MACVZ_EXPECTED_POD_CIDRS:-}"
SOAK_ITERS="${MACVZ_INLOOP_SOAK_ITERATIONS:-30}"
SOAK_INTERVAL="${MACVZ_INLOOP_SOAK_INTERVAL:-10}"
RSS_GROWTH_KB="${MACVZ_INLOOP_RSS_GROWTH_KB:-98304}"
PROC_GROWTH="${MACVZ_MULTIPOD_PROC_GROWTH:-0}"
OUT_DIR="${MACVZ_CRI_OUT_DIR:-}"

RUNTIME_LABEL="node.macvz.io/runtime=apple-container"
TAINT_KEY="node.macvz.io/host-namespace-unsupported"

FAILURES=0
SKIPS=0
TMP_ROOT=""
OUT_DIR_WAS_SET=0
PF_PID=""
# LINUXPOD_BACKED is set to 1 only once the backend-evidence phase proves EVERY
# Pod is served by a genuine, non-simulated LinuxPod backend. Until then the
# LinuxPod-specific acceptances skip loudly rather than pass on a handshake.
LINUXPOD_BACKED=0
# Helper/process count captured right after the initial rollout, used as the
# churn baseline for the per-Pod process-growth bound.
PROC_BASELINE=""

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
CRI-L6-3 LinuxPod-backed multi-Pod concurrency suite (plan; set
MACVZ_INTEGRATION=1 and a reachable KUBECONFIG to run live):

  preflight       kubectl reachable; locate the MacVz CRI node by its runtime
                  label; assert #84 labels + NoSchedule taint present; node Ready.
  route-before    capture the node default route(s) (MACVZ_ROUTE_AUDIT_CMD) so
                  the post-run audit can prove they were never mutated.
  deploy          kubectl apply fixtures/linuxpod-multipod-workload.yaml with
                  replicas=MACVZ_MULTIPOD_REPLICAS (>=3); rollout status; assert
                  >=3 app+sidecar Pods reach Ready on the MacVz node.
  scheduling      every replica landed on the MacVz node; events clean (no
                  FailedScheduling / FailedCreatePodSandBox) for all Pods.
  backend-evidence  HONESTY GATE: assert via MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
                  that EVERY Pod's sandbox is served by a genuine, non-simulated
                  LinuxPod backend. Without that proof every LinuxPod-specific
                  acceptance below is SKIPPED with the #127/#128/#129 blocker.
  unique-podips   each Pod has a Pod IP from the node PodCIDR and all Pod IPs are
                  distinct (no IPAM collision across concurrent LinuxPod VMs).
  per-pod         (LinuxPod-backed only) for every Pod: shared-namespace
                  localhost proof, logs (app+sidecar), exec (Secret+ConfigMap+
                  shared proof), and port-forward all work concurrently.
  service         curl the ClusterIP Service many times; assert it load-balanced
                  across >=REPLICAS DISTINCT Pod backends (distinct /whoami).
  direct-podip    an in-cluster probe curls each Pod IP directly and gets that
                  Pod's marker (proves per-Pod reachability, not just the VIP).
  restart-cri     restart macvz-cri; every Pod stays Running with the same UID;
                  Pod count unchanged; LinuxPod residual state does not double.
  restart-helper  restart/crash the LinuxPod helper; every Pod recovers (bounded
                  recreate) or fails with an explicit diagnostic; no stale
                  Running-but-unusable Pod remains.
  dup-audit       assert no duplicate pf/binat anchor or helper-work record is
                  left behind (MACVZ_LINUXPOD_DUP_AUDIT_CMD; falls back to audit
                  line uniqueness).
  churn           scale the Deployment up to MACVZ_MULTIPOD_CHURN_HIGH and back
                  to REPLICAS MACVZ_MULTIPOD_CHURN_CYCLES times; after each cycle
                  Pod IPs stay unique and residual state returns to the baseline
                  count; helper/process count returns to baseline (+/- bound).
  soak            sample adapter RSS, helper/process count, aggregate Pod
                  restartCount, and LinuxPod state over MACVZ_INLOOP_SOAK_
                  ITERATIONS; flag RSS growth / process growth / churn.
  cleanup         delete the fixture; assert no residual Pods and (LinuxPod
                  audit) zero residual VM/container/rootfs/handoff/network state.
  route-after     re-capture the default route(s); assert unchanged.

Gated: the live suite drives a real cluster and a real macOS CRI node. The
LinuxPod acceptances additionally require the node to run
`macvz-cri --experimental-linuxpod-backend` AND a backend-evidence hook proving
EVERY Pod is genuinely LinuxPod-backed; absent that they skip loudly with the
#127/#128/#129 blocker. Restart/audit/resource phases need operator hooks
(MACVZ_RESTART_CRI_CMD, MACVZ_RESTART_HELPER_CMD, MACVZ_ADAPTER_RSS_CMD,
MACVZ_LINUXPOD_HELPER_PROC_CMD, MACVZ_LINUXPOD_AUDIT_CMD,
MACVZ_LINUXPOD_DUP_AUDIT_CMD, MACVZ_ROUTE_AUDIT_CMD); an unset hook is skipped
loudly, never silently passed. See the header for the full env contract and
test/e2e/cri-k3s/README.md for topology.
PLAN
}

# --- setup / teardown --------------------------------------------------------
setup() {
	command -v kubectl >/dev/null 2>&1 || die "kubectl not found in PATH"
	command -v python3 >/dev/null 2>&1 || die "python3 not found in PATH (needed for PodCIDR membership checks)"
	[ -f "$FIXTURE" ] || die "fixture not found at $FIXTURE"
	k get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1 \
		|| die "cluster unreachable (KUBECONFIG=${KUBECONFIG:-unset}); set a reachable kubeconfig"

	TMP_ROOT="$(mktemp -d -t macvz-cri-linuxpod-multipod)"
	if [ -n "$OUT_DIR" ]; then
		OUT_DIR_WAS_SET=1
	else
		OUT_DIR="$TMP_ROOT/out"
	fi
	mkdir -p "$OUT_DIR"
	log "out=$OUT_DIR image=$IMAGE replicas=$REPLICAS churn=${CHURN_CYCLES}x(up to $CHURN_HIGH)"
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

# pod_names -> newline-separated Running/known Pod names for the fixture.
pod_names() {
	kn get pods -l "$APP_LABEL" -o name 2>/dev/null | sed -n 's#^pod/##p'
}
pod_count() { pod_names | wc -l | tr -d ' '; }
pod_uid() { kn get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null; }
pod_ip() { kn get pod "$1" -o jsonpath='{.status.podIP}' 2>/dev/null; }
pod_app_container_id() {
	kn get pod "$1" -o jsonpath='{.status.containerStatuses[?(@.name=="app")].containerID}' 2>/dev/null
}
ready_count() {
	kn get pods -l "$APP_LABEL" \
		-o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null \
		| grep -c '^True$'
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

apply_fixture() {
	sed -e "s#image: busybox:1.36.1#image: $IMAGE#g" \
		-e "s#^  replicas: 3#  replicas: $REPLICAS#" \
		"$FIXTURE" >"$OUT_DIR/workload.applied.yaml"
	k apply -f "$OUT_DIR/workload.applied.yaml" >"$OUT_DIR/apply.log" 2>&1
}

phase_deploy() {
	log "Phase: deploy $REPLICAS-replica app+late-sidecar fixture + rollout"
	if ! apply_fixture; then
		fail "kubectl apply failed (see $OUT_DIR/apply.log)"
		return 1
	fi
	pass "fixture applied (replicas=$REPLICAS)"
	if kn rollout status "deploy/$DEPLOY" --timeout=8m >"$OUT_DIR/rollout.log" 2>&1; then
		pass "kubectl rollout status (Deployment available)"
	else
		fail "rollout did not complete (see $OUT_DIR/rollout.log + Pod events below)"
		kn describe "deploy/$DEPLOY" >"$OUT_DIR/deploy-describe.log" 2>&1 || true
		kn get pods -o wide >"$OUT_DIR/pods.log" 2>&1 || true
	fi
	local ready; ready="$(ready_count)"
	if [ "$ready" -ge 3 ] && [ "$ready" -ge "$REPLICAS" ]; then
		pass "$ready concurrent app+sidecar Pods Ready (>=3, >=replicas)"
	else
		fail "only $ready/$REPLICAS Pods Ready (need >=3 concurrent Ready Pods)"
	fi
}

phase_scheduling() {
	log "Phase: scheduling + Pod events (all replicas on the MacVz node)"
	local pods; pods="$(pod_names)"
	[ -n "$pods" ] || { fail "no fixture Pods found"; return 1; }
	local off=0 bad=0 pod node
	while IFS= read -r pod; do
		[ -n "$pod" ] || continue
		node="$(kn get pod "$pod" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
		[ "$node" = "$NODE" ] || { off=$((off+1)); echo "$pod -> $node" >>"$OUT_DIR/offnode.txt"; }
		kn get events --field-selector "involvedObject.name=$pod" >>"$OUT_DIR/pod-events.log" 2>&1 || true
		if kn get events --field-selector "involvedObject.name=$pod" 2>/dev/null \
			| grep -Eqi 'FailedScheduling|FailedCreatePodSandBox'; then
			bad=$((bad+1)); echo "$pod" >>"$OUT_DIR/badevents.txt"
		fi
	done <<EOF
$pods
EOF
	[ "$off" = 0 ] && pass "all Pods scheduled onto MacVz node $NODE" \
		|| fail "$off Pod(s) not on $NODE (see $OUT_DIR/offnode.txt)"
	[ "$bad" = 0 ] && pass "Pod events clean for all Pods (no FailedScheduling/FailedCreatePodSandBox)" \
		|| fail "$bad Pod(s) had scheduling/sandbox failures (see $OUT_DIR/pod-events.log)"
}

phase_backend_evidence() {
	# The honesty gate. Only proof that EVERY Pod's sandbox is served by a
	# genuine, non-simulated LinuxPod backend flips LINUXPOD_BACKED=1 and unlocks
	# the LinuxPod-specific acceptances.
	log "Phase: LinuxPod backend evidence for every Pod (honesty gate)"
	if [ -z "${MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD:-}" ]; then
		skip "backend-evidence: set MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD to prove each Pod is LinuxPod-backed; LinuxPod acceptances will be skipped (blocked on CRI-L serving #127/#128/#129)"
		return 0
	fi
	local count=0 ready=0 _
	for _ in $(seq 1 60); do
		count="$(pod_count)"
		ready="$(ready_count)"
		[ "$count" -ge "$REPLICAS" ] && [ "$ready" -ge "$REPLICAS" ] && break
		sleep 1
	done
	local pods; pods="$(pod_names)"
	[ -n "$pods" ] || { fail "no Pods for backend evidence"; return 1; }
	local total=0 backed=0 simulated=0 pod ev
	while IFS= read -r pod; do
		[ -n "$pod" ] || continue
		total=$((total+1))
		ev="$OUT_DIR/backend-evidence-$pod.txt"
		if ! MACVZ_POD="$pod" run_hook "$MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD" \
			</dev/null >"$ev" 2>"$ev.err"; then
			fail "MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD failed for $pod (see $ev.err)"
			continue
		fi
		if grep -Eqi 'simulated[":= ]+true' "$ev"; then
			simulated=$((simulated+1)); continue
		fi
		if grep -Eqi 'linuxpod|pod[-_ ]?vm|sandboxVM' "$ev" \
			&& grep -Eqi 'simulated[":= ]+false|backend[":= ]+linuxpod|serving[":= ]+linuxpod' "$ev"; then
			backed=$((backed+1))
		fi
	done <<EOF
$pods
EOF
	if [ "$simulated" -gt 0 ]; then
		skip "backend-evidence: $simulated/$total Pod(s) report simulated=true (R17 prototype handshake, not a real LinuxPod backend) — LinuxPod acceptances blocked on #127/#128/#129 (see $OUT_DIR/backend-evidence-*.txt)"
		return 0
	fi
	if [ "$backed" -eq "$total" ] && [ "$total" -ge 3 ]; then
		LINUXPOD_BACKED=1
		pass "all $total Pods served by a genuine (non-simulated) LinuxPod backend"
	else
		skip "backend-evidence proved only $backed/$total Pods LinuxPod-backed (need every Pod, >=3); LinuxPod acceptances blocked on #127/#128/#129 (see $OUT_DIR/backend-evidence-*.txt)"
	fi
}

phase_unique_podips() {
	log "Phase: unique Pod IPs from the node PodCIDR"
	local pods; pods="$(pod_names)"
	[ -n "$pods" ] || { fail "no Pods for Pod IP check"; return 1; }
	: >"$OUT_DIR/podips.txt"
	local missing=0 pod ip
	while IFS= read -r pod; do
		[ -n "$pod" ] || continue
		ip="$(pod_ip "$pod")"
		if [ -n "$ip" ]; then
			printf '%s %s\n' "$ip" "$pod" >>"$OUT_DIR/podips.txt"
		else
			missing=$((missing+1)); echo "$pod (no IP)" >>"$OUT_DIR/podips.txt"
		fi
	done <<EOF
$pods
EOF
	[ "$missing" = 0 ] && pass "every Pod has a Pod IP" || fail "$missing Pod(s) have no podIP (see $OUT_DIR/podips.txt)"

	local total uniq
	total="$(awk 'NF>=2 && $1 ~ /[0-9]/' "$OUT_DIR/podips.txt" | wc -l | tr -d ' ')"
	uniq="$(awk 'NF>=2 && $1 ~ /[0-9]/ {print $1}' "$OUT_DIR/podips.txt" | sort -u | wc -l | tr -d ' ')"
	if [ "$total" -ge 3 ] && [ "$total" = "$uniq" ]; then
		pass "all $total Pod IPs are unique (no IPAM collision across concurrent LinuxPod VMs)"
	else
		fail "Pod IP collision: $total IPs, $uniq distinct (see $OUT_DIR/podips.txt)"
	fi
	# PodCIDR membership: every Pod IP must fall inside the node's PodCIDR(s).
	local cidrs="$EXPECTED_POD_CIDRS"
	if [ -z "$cidrs" ]; then
		cidrs="$(k get node "$NODE" -o jsonpath='{.spec.podCIDRs[*]}{" "}{.spec.podCIDR}' 2>/dev/null)"
	fi
	if [ -n "$cidrs" ]; then
		echo "$cidrs" >"$OUT_DIR/podcidrs.txt"
		local out_of=0 ip hit
		while read -r ip _; do
			[ -n "$ip" ] || continue
			printf '%s' "$ip" | grep -Eq '^[0-9]' || continue
			hit=0
			for cidr in $cidrs; do
				if cidr_contains_ipv4 "$cidr" "$ip"; then
					hit=1
					break
				fi
			done
			[ "$hit" = 1 ] || { out_of=$((out_of+1)); echo "$ip not in [$cidrs]" >>"$OUT_DIR/podcidr-misses.txt"; }
		done <"$OUT_DIR/podips.txt"
		[ "$out_of" = 0 ] && pass "all Pod IPs fall within expected Pod CIDR(s) ($cidrs)" \
			|| fail "$out_of Pod IP(s) outside expected Pod CIDR(s) $cidrs (see $OUT_DIR/podcidr-misses.txt)"
	else
		skip "PodCIDR membership: node $NODE exposes no .spec.podCIDR and MACVZ_EXPECTED_POD_CIDRS is unset (uniqueness still asserted above)"
	fi
}

cidr_contains_ipv4() {
	local cidr="$1" ip="$2"
	python3 - "$cidr" "$ip" <<'PY' 2>/dev/null
import ipaddress
import sys
try:
    net = ipaddress.ip_network(sys.argv[1], strict=False)
    addr = ipaddress.ip_address(sys.argv[2])
except ValueError:
    sys.exit(1)
sys.exit(0 if addr in net else 1)
PY
}

phase_per_pod() {
	log "Phase: per-Pod shared-ns / logs / exec / port-forward (concurrent)"
	linuxpod_gate "per-pod" || return 0
	local pods; pods="$(pod_names)"
	[ -n "$pods" ] || { fail "no Pods for per-Pod checks"; return 1; }
	local sns_bad=0 logs_bad=0 exec_bad=0 pf_bad=0 lport=18091 pod out
	while IFS= read -r pod; do
		[ -n "$pod" ] || continue
		# shared-namespace localhost proof
		out="$(kn exec "$pod" -c app -- sh -c 'cat /shared/sidecar-localhost 2>/dev/null' 2>>"$OUT_DIR/per-pod.err" || true)"
		printf '%s' "$out" | grep -q "$SIDECAR_LOCALHOST_MARKER" || { sns_bad=$((sns_bad+1)); echo "$pod sns" >>"$OUT_DIR/per-pod-fail.txt"; }
		# logs: app + sidecar markers
		kn logs "$pod" -c app 2>/dev/null | grep -q "$APP_BOOT_MARKER" || { logs_bad=$((logs_bad+1)); echo "$pod logs-app" >>"$OUT_DIR/per-pod-fail.txt"; }
		kn logs "$pod" -c sidecar 2>/dev/null | grep -q "$SIDECAR_BOOT_MARKER" || { logs_bad=$((logs_bad+1)); echo "$pod logs-sidecar" >>"$OUT_DIR/per-pod-fail.txt"; }
		# exec: projected Secret + ConfigMap + shared proof
		out="$(kn exec "$pod" -c app -- sh -c 'cat /etc/app-secret/token; echo; cat /www/app.conf; echo; cat /shared/sidecar-localhost 2>/dev/null' 2>>"$OUT_DIR/per-pod.err" || true)"
		{ printf '%s' "$out" | grep -q "$SECRET_MARKER" && printf '%s' "$out" | grep -q "$MARKER"; } \
			|| { exec_bad=$((exec_bad+1)); echo "$pod exec" >>"$OUT_DIR/per-pod-fail.txt"; }
		# port-forward: curl the served marker
		lport=$((lport+1))
		kn port-forward "pod/$pod" "$lport:8080" >"$OUT_DIR/pf-$pod.log" 2>&1 &
		PF_PID="$!"
		local ok=0 _
		for _ in $(seq 1 30); do
			if curl -fsS "http://127.0.0.1:$lport/index.html" 2>/dev/null | grep -q "$MARKER"; then ok=1; break; fi
			sleep 0.5
		done
		kill "$PF_PID" 2>/dev/null || true; wait "$PF_PID" 2>/dev/null || true; PF_PID=""
		[ "$ok" = 1 ] || { pf_bad=$((pf_bad+1)); echo "$pod pf" >>"$OUT_DIR/per-pod-fail.txt"; }
	done <<EOF
$pods
EOF
	[ "$sns_bad" = 0 ]  && pass "shared-namespace localhost proof on every Pod"      || fail "$sns_bad Pod(s) missing shared-ns localhost proof (see $OUT_DIR/per-pod-fail.txt)"
	[ "$logs_bad" = 0 ] && pass "kubectl logs (app+sidecar) on every Pod"            || fail "$logs_bad log check(s) failed (see $OUT_DIR/per-pod-fail.txt)"
	[ "$exec_bad" = 0 ] && pass "kubectl exec (Secret+ConfigMap+shared) on every Pod" || fail "$exec_bad Pod(s) failed exec checks (see $OUT_DIR/per-pod-fail.txt)"
	[ "$pf_bad" = 0 ]   && pass "kubectl port-forward on every Pod"                  || fail "$pf_bad Pod(s) failed port-forward (see $OUT_DIR/per-pod-fail.txt)"
}

phase_service() {
	log "Phase: ClusterIP Service load-balances across distinct Pods"
	local probe="linuxpod-multipod-probe"
	local want; want="$(pod_count)"
	[ "$want" -ge 3 ] || want=3
	kn delete pod "$probe" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	# Curl the Service many times and collect distinct /whoami pod identities.
	if kn run "$probe" --image="$IMAGE" --restart=Never --command -- \
		sh -c "for i in \$(seq 1 $((want * 12))); do wget -T 5 -qO- http://$SVC.$NS.svc:80/whoami 2>/dev/null; sleep 0.3; done" \
		>"$OUT_DIR/svc-probe-run.log" 2>&1; then
		:
	fi
	local phase _
	for _ in $(seq 1 240); do
		phase="$(kn get pod "$probe" -o jsonpath='{.status.phase}' 2>/dev/null)"
		case "$phase" in Succeeded|Failed) break ;; esac
		sleep 1
	done
	kn logs "$probe" >"$OUT_DIR/svc-probe.log" 2>&1 || true
	local distinct
	distinct="$(grep "$WHOAMI_MARKER" "$OUT_DIR/svc-probe.log" 2>/dev/null | sed -n 's/.*pod=\([^ ]*\).*/\1/p' | sort -u | wc -l | tr -d ' ')"
	if [ "$distinct" -ge "$want" ]; then
		pass "ClusterIP Service reached $distinct distinct Pod backends (>= $want)"
	elif [ "$distinct" -ge 3 ]; then
		pass "ClusterIP Service reached $distinct distinct Pod backends (>=3; fewer than $want replicas hit within sampling)"
	else
		fail "Service load-balanced across only $distinct distinct Pod(s) (phase=$phase; see $OUT_DIR/svc-probe.log)"
	fi
	kn delete pod "$probe" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

phase_direct_podip() {
	log "Phase: direct Pod-IP reachability (per Pod, in-cluster)"
	local pods; pods="$(pod_names)"
	[ -n "$pods" ] || { fail "no Pods for direct Pod-IP probe"; return 1; }
	local probe="linuxpod-multipod-ipprobe" bad=0 total=0 pod ip
	kn delete pod "$probe" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	# Build a single probe that curls every Pod IP and prints PASS/FAIL per IP.
	local script="set -e; rc=0;"
	while IFS= read -r pod; do
		[ -n "$pod" ] || continue
		ip="$(pod_ip "$pod")"
		[ -n "$ip" ] || { bad=$((bad+1)); continue; }
		total=$((total+1))
		script="$script if wget -T 5 -qO- http://$ip:8080/index.html | grep -q $MARKER; then echo OK $ip; else echo MISS $ip; rc=1; fi;"
	done <<EOF
$pods
EOF
	script="$script exit \$rc"
	if [ "$total" = 0 ]; then
		fail "no Pod IPs to probe directly"
		return 1
	fi
	kn run "$probe" --image="$IMAGE" --restart=Never --command -- sh -c "$script" >/dev/null 2>&1 || true
	local phase _
	for _ in $(seq 1 180); do
		phase="$(kn get pod "$probe" -o jsonpath='{.status.phase}' 2>/dev/null)"
		case "$phase" in Succeeded|Failed) break ;; esac
		sleep 1
	done
	kn logs "$probe" >"$OUT_DIR/direct-podip.log" 2>&1 || true
	local ok; ok="$(grep -c '^OK ' "$OUT_DIR/direct-podip.log" 2>/dev/null || echo 0)"
	if [ "$ok" -ge "$total" ] && [ "$bad" = 0 ]; then
		pass "every Pod ($ok/$total) reachable directly on its Pod IP:8080"
	else
		fail "direct Pod-IP reachability: $ok/$total OK, $bad without IP (see $OUT_DIR/direct-podip.log)"
	fi
	kn delete pod "$probe" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}

# helper_proc_count -> prints the single integer the helper-process hook reports,
# or empty if the hook is unset/failed.
helper_proc_count() {
	[ -n "${MACVZ_LINUXPOD_HELPER_PROC_CMD:-}" ] || return 3
	local raw; raw="$(run_hook "$MACVZ_LINUXPOD_HELPER_PROC_CMD" 2>/dev/null)" || return 2
	printf '%s' "$raw" | tr -dc '0-9'
}

phase_restart_cri() {
	log "Phase: macvz-cri restart recovery (no duplicate/lost sandboxes)"
	if [ -z "${MACVZ_RESTART_CRI_CMD:-}" ]; then
		skip "restart-cri (set MACVZ_RESTART_CRI_CMD to restart the adapter on the CRI node)"
		return 0
	fi
	local pods; pods="$(pod_names)"
	[ -n "$pods" ] || { fail "no Pods before macvz-cri restart"; return 1; }
	# Snapshot every Pod UID before the restart.
	: >"$OUT_DIR/uids-before.txt"
	local pod
	while IFS= read -r pod; do
		[ -n "$pod" ] || continue
		printf '%s %s\n' "$pod" "$(pod_uid "$pod")" >>"$OUT_DIR/uids-before.txt"
	done <<EOF
$pods
EOF
	local count_before; count_before="$(pod_count)"
	local audit_before=""
	[ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ] && audit_before="$(linuxpod_state_count "$OUT_DIR/restart-cri-audit-before.log" || echo "")"

	log "restarting macvz-cri via operator hook"
	run_hook "$MACVZ_RESTART_CRI_CMD" >"$OUT_DIR/restart-cri.log" 2>&1 \
		|| { fail "MACVZ_RESTART_CRI_CMD returned non-zero (see $OUT_DIR/restart-cri.log)"; return 1; }

	# Wait for every snapshotted Pod to be Running again.
	local running=0 _
	for _ in $(seq 1 90); do
		running=0
		while read -r pod _; do
			[ -n "$pod" ] || continue
			[ "$(pod_phase "$pod")" = "Running" ] && running=$((running+1))
		done <"$OUT_DIR/uids-before.txt"
		[ "$running" -ge "$count_before" ] && break
		sleep 2
	done
	[ "$running" -ge "$count_before" ] && pass "all $count_before Pods Running after macvz-cri restart" \
		|| fail "only $running/$count_before Pods Running after macvz-cri restart"

	# UID stability: no Pod was recreated (no lost/duplicate workload).
	local changed=0 uid_now pod_b uid_b
	while read -r pod_b uid_b; do
		[ -n "$pod_b" ] || continue
		uid_now="$(pod_uid "$pod_b")"
		[ "$uid_b" = "$uid_now" ] || { changed=$((changed+1)); echo "$pod_b $uid_b -> $uid_now" >>"$OUT_DIR/uid-changes.txt"; }
	done <"$OUT_DIR/uids-before.txt"
	[ "$changed" = 0 ] && pass "all Pod UIDs unchanged (no duplicate/lost CRI state)" \
		|| fail "$changed Pod UID(s) changed across macvz-cri restart (see $OUT_DIR/uid-changes.txt)"

	# Residual-state count must not balloon (no duplicate sandbox set).
	if [ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ]; then
		local audit_after
		if audit_after="$(linuxpod_state_count "$OUT_DIR/restart-cri-audit-after.log")"; then
			if [ -n "$audit_before" ] && [ "$audit_before" -gt 0 ] 2>/dev/null && [ "$audit_after" -ge $((audit_before * 2)) ] 2>/dev/null; then
				fail "LinuxPod residual state grew from $audit_before to $audit_after lines after restart (possible duplicate sandboxes; see $OUT_DIR/restart-cri-audit-after.log)"
			else
				pass "LinuxPod residual state did not double after restart ($audit_before -> $audit_after lines)"
			fi
		else
			fail "LinuxPod audit hook failed after macvz-cri restart (see $OUT_DIR/restart-cri-audit-after.log.err)"
		fi
	else
		skip "LinuxPod duplicate audit (set MACVZ_LINUXPOD_AUDIT_CMD)"
	fi
}

phase_restart_helper() {
	log "Phase: LinuxPod helper restart/failure handling (bounded recreate)"
	if [ -z "${MACVZ_RESTART_HELPER_CMD:-}" ]; then
		skip "restart-helper (set MACVZ_RESTART_HELPER_CMD to restart/crash the LinuxPod helper)"
		return 0
	fi
	if [ "$LINUXPOD_BACKED" != 1 ]; then
		skip "restart-helper: node not proven LinuxPod-backed, so a helper restart exercises nothing real (blocked on #127/#128/#129)"
		return 0
	fi
	local count_before; count_before="$(pod_count)"
	: >"$OUT_DIR/restart-helper-containerids-before.txt"
	local before_pod
	while IFS= read -r before_pod; do
		[ -n "$before_pod" ] || continue
		printf '%s %s\n' "$before_pod" "$(pod_app_container_id "$before_pod")" >>"$OUT_DIR/restart-helper-containerids-before.txt"
	done <<EOF
$(pod_names)
EOF
	log "restarting/crashing the LinuxPod helper via operator hook"
	run_hook "$MACVZ_RESTART_HELPER_CMD" >"$OUT_DIR/restart-helper.log" 2>&1 \
		|| { fail "MACVZ_RESTART_HELPER_CMD returned non-zero (see $OUT_DIR/restart-helper.log)"; return 1; }

	# Bounded recreate: kubelet may recreate affected Pods. Wait for the
	# Deployment to converge back to a full Ready set, then assert exec works on
	# every current Pod (no stale Running-but-unusable Pod remains).
	local ready=0 _
	for _ in $(seq 1 120); do
		ready="$(ready_count)"
		[ "$ready" -ge "$count_before" ] && break
		sleep 2
	done
	[ "$ready" -ge "$count_before" ] \
		&& pass "all $count_before Pods recovered to Ready after helper restart (bounded recreate)" \
		|| fail "only $ready/$count_before Pods Ready after helper restart (record the failure handling as the next blocker; see $OUT_DIR/restart-helper.log)"

	# Every current Pod must be genuinely usable, not stale Running-but-dead. A
	# helper restart drives bounded recreate, so give kubelet time to publish the
	# replacement container IDs before asking the streaming server to exec.
	: >"$OUT_DIR/restart-helper-exec.err"
	: >"$OUT_DIR/restart-helper-exec-fail.txt"
	: >"$OUT_DIR/restart-helper-containerids-after.txt"
	local usable=0 exec_bad=0 _ pod out cid pods
	for _ in $(seq 1 120); do
		pods="$(pod_names)"
		usable=0
		exec_bad=0
		: >"$OUT_DIR/restart-helper-exec-fail.txt.tmp"
		: >"$OUT_DIR/restart-helper-containerids-after.txt.tmp"
		while IFS= read -r pod; do
			[ -n "$pod" ] || continue
			cid="$(pod_app_container_id "$pod")"
			printf '%s %s\n' "$pod" "$cid" >>"$OUT_DIR/restart-helper-containerids-after.txt.tmp"
			if [ -z "$cid" ]; then
				exec_bad=$((exec_bad+1))
				printf '%s missing-app-container-id\n' "$pod" >>"$OUT_DIR/restart-helper-exec-fail.txt.tmp"
				continue
			fi
			out="$(kn exec "$pod" -c app -- sh -c 'cat /www-rw/index.html 2>/dev/null; echo; cat /shared/sidecar-localhost 2>/dev/null' 2>>"$OUT_DIR/restart-helper-exec.err" || true)"
			if printf '%s' "$out" | grep -q "$MARKER" && printf '%s' "$out" | grep -q "$SIDECAR_LOCALHOST_MARKER"; then
				usable=$((usable+1))
			else
				exec_bad=$((exec_bad+1))
				printf '%s exec-proof-failed\n' "$pod" >>"$OUT_DIR/restart-helper-exec-fail.txt.tmp"
			fi
		done <<EOF
$pods
EOF
		mv "$OUT_DIR/restart-helper-exec-fail.txt.tmp" "$OUT_DIR/restart-helper-exec-fail.txt"
		mv "$OUT_DIR/restart-helper-containerids-after.txt.tmp" "$OUT_DIR/restart-helper-containerids-after.txt"
		if [ "$usable" -ge "$count_before" ] && [ "$exec_bad" = 0 ]; then
			break
		fi
		sleep 2
	done
	if [ "$usable" -ge "$count_before" ] && [ "$exec_bad" = 0 ]; then
		pass "exec + shared-ns proof works on every Pod after helper restart (no stale Running-but-unusable Pod)"
	else
		fail "$exec_bad Pod(s) did not recover usable state after helper restart (see $OUT_DIR/restart-helper-exec-fail.txt)"
	fi
}

phase_dup_audit() {
	log "Phase: no duplicate pf/binat or helper-work state"
	local src="" raw
	if [ -n "${MACVZ_LINUXPOD_DUP_AUDIT_CMD:-}" ]; then
		src="dup"
		if ! raw="$(run_hook "$MACVZ_LINUXPOD_DUP_AUDIT_CMD" 2>"$OUT_DIR/dup-audit.err")"; then
			fail "MACVZ_LINUXPOD_DUP_AUDIT_CMD failed (see $OUT_DIR/dup-audit.err)"
			return 1
		fi
	elif [ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ]; then
		src="audit"
		if ! raw="$(run_hook "$MACVZ_LINUXPOD_AUDIT_CMD" 2>"$OUT_DIR/dup-audit.err")"; then
			fail "MACVZ_LINUXPOD_AUDIT_CMD failed for dup check (see $OUT_DIR/dup-audit.err)"
			return 1
		fi
	else
		skip "dup-audit (set MACVZ_LINUXPOD_DUP_AUDIT_CMD or MACVZ_LINUXPOD_AUDIT_CMD to assert no duplicate pf/binat/helper-work record)"
		return 0
	fi
	printf '%s\n' "$raw" >"$OUT_DIR/dup-audit.txt"
	# Any non-blank line appearing more than once is a leaked pf/binat or
	# helper-work record.
	local dups
	dups="$(printf '%s\n' "$raw" | sed '/^[[:space:]]*$/d' | sort | uniq -d)"
	if [ -z "$dups" ]; then
		pass "no duplicate pf/binat/helper-work record (source: $src)"
	else
		printf '%s\n' "$dups" >"$OUT_DIR/dup-audit-dups.txt"
		fail "duplicate pf/binat/helper-work record(s) found (source: $src; see $OUT_DIR/dup-audit-dups.txt)"
	fi
}

phase_churn() {
	log "Phase: scale churn ($CHURN_CYCLES cycle(s) up to $CHURN_HIGH, back to $REPLICAS)"
	local baseline_audit=""
	[ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ] && baseline_audit="$(linuxpod_state_count "$OUT_DIR/churn-audit-baseline.log" || echo "")"
	local cycle ok=1
	for cycle in $(seq 1 "$CHURN_CYCLES"); do
		log "churn cycle $cycle/$CHURN_CYCLES: scale -> $CHURN_HIGH"
		if ! kn scale "deploy/$DEPLOY" --replicas="$CHURN_HIGH" >"$OUT_DIR/churn-up-$cycle.log" 2>&1; then
			fail "churn cycle $cycle: scale up to $CHURN_HIGH failed (see $OUT_DIR/churn-up-$cycle.log)"
			ok=0
			continue
		fi
		if ! kn rollout status "deploy/$DEPLOY" --timeout=6m >>"$OUT_DIR/churn-up-$cycle.log" 2>&1; then
			fail "churn cycle $cycle: rollout after scale up did not complete (see $OUT_DIR/churn-up-$cycle.log)"
			ok=0
			continue
		fi
		log "churn cycle $cycle/$CHURN_CYCLES: scale -> $REPLICAS"
		if ! kn scale "deploy/$DEPLOY" --replicas="$REPLICAS" >"$OUT_DIR/churn-down-$cycle.log" 2>&1; then
			fail "churn cycle $cycle: scale down to $REPLICAS failed (see $OUT_DIR/churn-down-$cycle.log)"
			ok=0
			continue
		fi
		if ! kn rollout status "deploy/$DEPLOY" --timeout=6m >>"$OUT_DIR/churn-down-$cycle.log" 2>&1; then
			fail "churn cycle $cycle: rollout after scale down did not complete (see $OUT_DIR/churn-down-$cycle.log)"
			ok=0
			continue
		fi
		# Wait for the Pod set to settle back to REPLICAS.
		local n ready _
		for _ in $(seq 1 60); do
			n="$(pod_count)"
			ready="$(ready_count)"
			[ "$n" = "$REPLICAS" ] && [ "$ready" = "$REPLICAS" ] && break
			sleep 2
		done
		# Pod IPs unique after each cycle.
		local ip_file="$OUT_DIR/churn-podips-$cycle.txt" total uniq p ip
		: >"$ip_file"
		while IFS= read -r p; do
			[ -n "$p" ] || continue
			ip="$(pod_ip "$p")"
			[ -n "$ip" ] && printf '%s %s\n' "$ip" "$p" >>"$ip_file"
		done <<EOF
$(pod_names)
EOF
		total="$(awk 'NF>=2 {print $1}' "$ip_file" | wc -l | tr -d ' ')"
		uniq="$(awk 'NF>=2 {print $1}' "$ip_file" | sort -u | wc -l | tr -d ' ')"
		if [ "$total" = "$uniq" ] && [ "$total" -ge 3 ]; then
			pass "churn cycle $cycle: $total Pods, all Pod IPs unique"
		else
			fail "churn cycle $cycle: Pod IP collision or missing IPs ($total IPs, $uniq distinct; see $ip_file)"; ok=0
		fi
	done
	# After churn the residual state must return to (not exceed) the baseline.
	if [ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ]; then
		local after
		if after="$(linuxpod_state_count "$OUT_DIR/churn-audit-after.log")"; then
			if [ -n "$baseline_audit" ] && [ "$after" -le "$baseline_audit" ] 2>/dev/null; then
				pass "post-churn residual state returned to baseline ($baseline_audit -> $after lines; no leak across churn)"
			elif [ -n "$baseline_audit" ]; then
				fail "post-churn residual state $after > baseline $baseline_audit lines (leaked LinuxPod/network state across churn; see $OUT_DIR/churn-audit-after.log)"
			else
				pass "post-churn LinuxPod audit recorded ($after lines; set a baseline for a strict bound)"
			fi
		else
			fail "LinuxPod audit hook failed after churn (see $OUT_DIR/churn-audit-after.log.err)"
		fi
	else
		skip "churn residual-state bound (set MACVZ_LINUXPOD_AUDIT_CMD)"
	fi
	# Helper/process count must return to the post-deploy baseline (+/- bound).
	if [ -n "${MACVZ_LINUXPOD_HELPER_PROC_CMD:-}" ] && [ -n "$PROC_BASELINE" ]; then
		local now; now="$(helper_proc_count || echo "")"
		if [ -n "$now" ]; then
			local growth=$((now - PROC_BASELINE))
			[ "$growth" -lt 0 ] && growth=0
			if [ "$growth" -le "$PROC_GROWTH" ]; then
				pass "helper/process count returned to baseline after churn ($PROC_BASELINE -> $now, growth $growth <= $PROC_GROWTH)"
			else
				fail "helper/process count grew $growth over baseline after churn ($PROC_BASELINE -> $now > $PROC_GROWTH): leaked per-Pod processes"
			fi
		else
			fail "MACVZ_LINUXPOD_HELPER_PROC_CMD returned no integer after churn"
		fi
	else
		skip "helper/process-count churn bound (set MACVZ_LINUXPOD_HELPER_PROC_CMD)"
	fi
	[ "$ok" = 1 ]
}

phase_soak() {
	log "Phase: in-loop soak ($SOAK_ITERS samples, ${SOAK_INTERVAL}s apart)"
	local pods; pods="$(pod_names)"
	[ -n "$pods" ] || { fail "no Pods for soak"; return 1; }
	printf 'iteration,rss_kb,ready,total,helper_procs,restart_sum,linuxpod_state\n' >"$OUT_DIR/soak-samples.csv"

	local first_rss=0 last_rss=0 have_rss=0 i rss ready total procs rc_sum lp
	local rss_hook_failures=0 audit_hook_failures=0 proc_hook_failures=0
	for i in $(seq 1 "$SOAK_ITERS"); do
		if [ -n "${MACVZ_ADAPTER_RSS_CMD:-}" ]; then
			local rss_raw
			if rss_raw="$(run_hook "$MACVZ_ADAPTER_RSS_CMD" 2>/dev/null)"; then
				rss="$(printf '%s' "$rss_raw" | tr -dc '0-9')"
				if [ -n "$rss" ]; then
					have_rss=1; [ "$first_rss" = 0 ] && first_rss="$rss"; last_rss="$rss"
				else rss=0; rss_hook_failures=$((rss_hook_failures+1)); fi
			else rss=0; rss_hook_failures=$((rss_hook_failures+1)); fi
		else rss=0; fi
		ready="$(ready_count)"; total="$(pod_count)"
		if [ -n "${MACVZ_LINUXPOD_HELPER_PROC_CMD:-}" ]; then
			procs="$(helper_proc_count || echo "")"; [ -n "$procs" ] || { procs=-2; proc_hook_failures=$((proc_hook_failures+1)); }
		else procs=-1; fi
		# Aggregate restartCount across all Pods.
		rc_sum=0
		local rc p
		while IFS= read -r p; do
			[ -n "$p" ] || continue
			rc="$(kn get pod "$p" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null)"; [ -n "$rc" ] || rc=0
			rc_sum=$((rc_sum + rc))
		done <<EOF
$(pod_names)
EOF
		if [ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ]; then
			if lp="$(linuxpod_state_count "$OUT_DIR/soak-audit-$i.log")"; then :; else lp=-2; audit_hook_failures=$((audit_hook_failures+1)); fi
		else lp=-1; fi
		printf '%s,%s,%s,%s,%s,%s,%s\n' "$i" "$rss" "$ready" "$total" "$procs" "$rc_sum" "$lp" >>"$OUT_DIR/soak-samples.csv"
		[ $((i % 10)) -eq 0 ] && log "soak $i/$SOAK_ITERS (ready=$ready/$total rss=${rss}KB procs=$procs restarts=$rc_sum)"
		sleep "$SOAK_INTERVAL"
	done

	local final_ready final_total; final_ready="$(ready_count)"; final_total="$(pod_count)"
	[ "$final_ready" -ge 3 ] && [ "$final_ready" = "$final_total" ] \
		&& pass "all $final_ready Pods Ready for the full soak" \
		|| fail "soak ended with $final_ready/$final_total Pods Ready"
	[ "$rss_hook_failures" = 0 ] || fail "adapter RSS hook failed or returned non-numeric output $rss_hook_failures time(s)"
	[ "$audit_hook_failures" = 0 ] || fail "LinuxPod audit hook failed $audit_hook_failures time(s) during soak"
	[ "$proc_hook_failures" = 0 ] || fail "helper/process-count hook failed $proc_hook_failures time(s) during soak"

	if [ "$have_rss" = 1 ]; then
		local growth=$((last_rss - first_rss))
		log "adapter RSS: first=${first_rss}KB last=${last_rss}KB growth=${growth}KB (limit ${RSS_GROWTH_KB}KB across $final_total Pods)"
		if [ "$growth" -le "$RSS_GROWTH_KB" ]; then
			pass "adapter RSS growth within bound"
		else
			fail "adapter RSS grew ${growth}KB (> ${RSS_GROWTH_KB}KB): possible leak under concurrency"
		fi
	else
		skip "adapter RSS trend (set MACVZ_ADAPTER_RSS_CMD); samples recorded with rss=0"
	fi
	log "soak samples: $OUT_DIR/soak-samples.csv"
}

phase_cleanup() {
	log "Phase: cleanup + residual LinuxPod state audit"
	k delete -f "$OUT_DIR/workload.applied.yaml" --wait=true --timeout=5m >"$OUT_DIR/cleanup.log" 2>&1 || true
	local remaining
	remaining="$(kn get pods -l "$APP_LABEL" -o name 2>/dev/null | wc -l | tr -d ' ')"
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
		skip "residual LinuxPod audit (set MACVZ_LINUXPOD_AUDIT_CMD to assert zero residual state)"
	fi
	# Helper/process count must return to zero (or below the post-deploy baseline) after cleanup.
	if [ -n "${MACVZ_LINUXPOD_HELPER_PROC_CMD:-}" ]; then
		local now; now="$(helper_proc_count || echo "")"
		if [ -n "$now" ]; then
			[ "$now" = 0 ] && pass "helper/process count is 0 after cleanup (no leaked per-Pod processes)" \
				|| fail "helper/process count is $now after cleanup (leaked LinuxPod processes/VMs)"
		else
			skip "helper/process count after cleanup (hook returned no integer)"
		fi
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
phase_unique_podips
# Capture the helper/process baseline once the concurrent set is up, for churn.
if [ -n "${MACVZ_LINUXPOD_HELPER_PROC_CMD:-}" ]; then
	PROC_BASELINE="$(helper_proc_count || echo "")"
	[ -n "$PROC_BASELINE" ] && log "helper/process baseline: $PROC_BASELINE (at $REPLICAS Pods)"
fi
phase_per_pod
phase_service
phase_direct_podip
phase_restart_cri
phase_restart_helper
phase_dup_audit
phase_churn
phase_soak
phase_cleanup
phase_route_after

echo
if [ "$FAILURES" -eq 0 ] && [ "$SKIPS" -eq 0 ]; then
	pass "CRI-L6-3 LinuxPod multi-Pod suite: all checks passed (diagnostics in $OUT_DIR)"
	exit 0
fi
if [ "$FAILURES" -eq 0 ]; then
	pass "CRI-L6-3 LinuxPod multi-Pod suite: checks passed with $SKIPS skipped (LinuxPod concurrency acceptance is NOT complete while skips remain; see #127/#128/#129). Diagnostics in $OUT_DIR"
	exit 0
fi
fail "CRI-L6-3 LinuxPod multi-Pod suite: $FAILURES check(s) failed (diagnostics in $OUT_DIR)"
exit 1
