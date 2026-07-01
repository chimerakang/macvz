#!/usr/bin/env bash
#
# linuxpod-volumes.sh — CRI-L8-3 (#145) k3s volume-projection matrix validation
# for the experimental LinuxPod-backed CRI path.
#
# This is the volume sibling of linuxpod-inloop.sh (#130) and linuxpod-dns.sh
# (#142). Where inloop proves the LinuxPod Pod *lifecycle* and dns proves the
# resolver path, this harness proves the kubelet-managed Kubernetes volume matrix
# an ordinary k3s workload depends on is honored end to end on a LinuxPod-backed
# Pod: configMap, secret, downward API, the projected service-account token,
# emptyDir (disk, shared across containers) and a Memory-medium emptyDir — with
# read-only mounts actually read-only, read-write scratch writable, content
# correct, projected updates propagating, cross-container sharing visible in both
# directions, and a clean teardown that leaves no stale materialized mount/rootfs
# state. It re-runs the core matrix after rollout, macvz-cri restart and LinuxPod
# helper restart.
#
# THE CENTRAL DISCIPLINE: distinguish a *volume* fault from a Pod-networking or
# Service fault, and a *policy* outcome from a *plumbing* outcome. A read-only
# mount that is writable is a translation bug; a missing SA token is a projection
# bug; a shared emptyDir not visible to the sidecar is a shared-namespace bug —
# each is reported as the layer it is, never collapsed into one red failure.
#
# HONESTY GATE (inherited from linuxpod-inloop.sh / #130). The shipped CRI
# serving path runs on apple/container; the LinuxPod backend gate is a startup
# handshake that, for the prototype, reports simulated=true. A Pod reaching
# Running is therefore NOT by itself evidence of a LinuxPod-backed Pod. The
# volume checks still RUN without the proof (volumes are meaningful on either
# backend), but the LinuxPod-specific framing — "the volume matrix works on a
# genuine LinuxPod micro-VM" — is only asserted when
# MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD proves the Pod is a real, non-simulated
# LinuxPod backend. Absent that proof the suite says so loudly and reports the
# volume result against the apple/container path instead of silently claiming a
# LinuxPod result.
#
# Gating: like linuxpod-dns.sh, the live suite mutates a real cluster and a real
# macOS CRI node, so it runs only when MACVZ_INTEGRATION=1 *and* a reachable
# KUBECONFIG is provided. Without both it prints the runbook plan and exits 0, so
# it is safe in `go test`-style CI and `bash -n` / shellcheck validation.
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
#
# Operator hooks (commands run via `sh -c`; the harness cannot reach the remote
# macOS node itself). A phase whose required hook is unset is SKIPped *loudly*,
# never silently passed:
#   MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
#                           proves the named Pod's sandbox is served by a real,
#                           non-simulated LinuxPod backend (see linuxpod-inloop.sh).
#                           Without it the volume checks still run but against the
#                           apple/container path.
#   MACVZ_RESTART_CRI_CMD   restart the macvz-cri service on the CRI node.
#   MACVZ_RESTART_HELPER_CMD restart (or crash+restart) the LinuxPod helper.
#   MACVZ_LINUXPOD_RESIDUAL_CMD
#                           prints residual LinuxPod materialized-mount/rootfs
#                           state for the deleted Pod; the harness asserts it is
#                           empty after cleanup (acceptance: no stale mount/rootfs).
#   MACVZ_ROUTE_AUDIT_CMD   print the node's default route(s); captured before
#                           and after, asserted unchanged (non-goal: never mutate
#                           the host default route, `192.168.1.1` via `en0`).
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# Shared helper functions (see lib.sh header before adding more).
. "$SCRIPT_DIR/lib.sh"
FIXTURE="$SCRIPT_DIR/fixtures/linuxpod-volumes-workload.yaml"
NS="macvz-cri-linuxpod-vol-e2e"
DEPLOY="linuxpod-vol"
CONFIGMAP="vol-app-config"

# Markers the fixture writes; the harness asserts exact content so a wrong value
# (stale projection, wrong volume bound) is caught, not just presence.
CONFIGMAP_MARKER="macvz-cri-l8-vol-ok"
CONFIGMAP_CONF_MARKER="macvz-cri-l8-vol-configmap-v1"
CONFIGMAP_CONF_MARKER_V2="macvz-cri-l8-vol-configmap-v2"
SECRET_MARKER="macvz-cri-l8-vol-secret-ok"
APP_SHARED_MARKER="macvz-cri-l8-vol-app-shared"
SIDECAR_SHARED_MARKER="macvz-cri-l8-vol-sidecar-shared"

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
CRI-L8-3 LinuxPod k3s volume-projection matrix suite (plan; set
MACVZ_INTEGRATION=1 and a reachable KUBECONFIG to run live):

  preflight       kubectl reachable; locate the MacVz CRI node by its runtime
                  label; assert #84 labels + NoSchedule taint present; node Ready.
  route-before    capture the node default route(s) (MACVZ_ROUTE_AUDIT_CMD) so
                  the post-run audit can prove they were never mutated.
  deploy          kubectl apply fixtures/linuxpod-volumes-workload.yaml (app +
                  late sidecar Pod; configMap/secret/downwardAPI; shared +
                  Memory emptyDir); rollout.
  scheduling      Pod landed on the MacVz node; events clean.
  backend-evidence  HONESTY GATE: assert via MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
                  that the Pod is served by a genuine, non-simulated LinuxPod
                  backend. The volume checks run either way; without the proof
                  they are reported against the apple/container path, not claimed
                  as a LinuxPod result.
  volume-matrix   exec inside the app: configMap content + read-only enforced;
                  secret content + read-only enforced; downward API content;
                  Memory emptyDir writable. Each read-only mount is proven
                  read-only by a failed write probe (writable RO = translation bug).
  sa-token        the default service-account token projection is present at
                  /var/run/secrets/kubernetes.io/serviceaccount (token, ca.crt,
                  namespace) — the in-cluster API access volume.
  shared-volume   the disk emptyDir is shared both ways: the app marker is
                  readable from the sidecar and the sidecar marker is readable
                  from the app (shared-namespace materialized volume).
  projected-update update the ConfigMap; assert the projected file content updates
                  inside the running Pod (kubelet projected-volume update behavior).
  hostpath-policy  the macOS default mount policy denies arbitrary hostPath; this
                  is covered hermetically (pkg/criserver volume matrix test). A
                  live allowlisted-hostPath probe runs only if a hook is provided.
  vol-after-rollout      rollout-restart the Deployment; re-run the core matrix
                  on the fresh Pod.
  vol-after-cri-restart  restart macvz-cri (MACVZ_RESTART_CRI_CMD); same Pod UID;
                  re-run the core matrix.
  vol-after-helper-restart restart the LinuxPod helper (MACVZ_RESTART_HELPER_CMD,
                  LinuxPod-backed only); re-run the core matrix.
  cleanup         delete the fixture; assert no residual Pods, and (with
                  MACVZ_LINUXPOD_RESIDUAL_CMD) no residual materialized
                  mount/rootfs state.
  route-after     re-capture the default route(s); assert unchanged.

Gated: the live suite drives a real cluster and a real macOS CRI node. The
LinuxPod framing additionally requires a backend-evidence hook; the churn phases
need operator hooks (MACVZ_RESTART_CRI_CMD, MACVZ_RESTART_HELPER_CMD,
MACVZ_LINUXPOD_RESIDUAL_CMD, MACVZ_ROUTE_AUDIT_CMD). An unset hook is skipped
loudly, never silently passed. See the header for the env contract and
test/e2e/cri-k3s/README.md for topology.
PLAN
}

# --- setup / teardown --------------------------------------------------------
setup() {
	command -v kubectl >/dev/null 2>&1 || die "kubectl not found in PATH"
	[ -f "$FIXTURE" ] || die "fixture not found at $FIXTURE"
	k get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1 \
		|| die "cluster unreachable (KUBECONFIG=${KUBECONFIG:-unset}); set a reachable kubeconfig"

	TMP_ROOT="$(mktemp -d -t macvz-cri-linuxpod-vol)"
	if [ -n "$OUT_DIR" ]; then
		OUT_DIR_WAS_SET=1
	else
		OUT_DIR="$TMP_ROOT/out"
	fi
	mkdir -p "$OUT_DIR"
	log "out=$OUT_DIR image=$IMAGE"
}

cleanup_trap() {
	if [ "${MACVZ_CRI_KEEP:-0}" != 1 ]; then
		kubectl delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
	fi
	if [ -n "$TMP_ROOT" ] && [ "${MACVZ_CRI_KEEP:-0}" != 1 ] && [ "$OUT_DIR_WAS_SET" = 0 ]; then
		rm -rf "$TMP_ROOT"
	fi
}
trap cleanup_trap EXIT

run_hook() {
	local cmd="$1"
	[ -n "$cmd" ] || return 3
	sh -c "$cmd"
}

# --- generic helpers ---------------------------------------------------------
pod_name() {
	kn get pods -l app=linuxpod-vol --field-selector=status.phase=Running \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}
pod_uid()   { kn get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null; }

# exec_in <pod> <container> <sh-command> <outfile>; never fails the exec itself so
# the caller classifies the captured output. The trailing `true` guarantees exit 0.
exec_in() {
	local pod="$1" c="$2" cmd="$3" outfile="$4"
	kn exec "$pod" -c "$c" -- sh -c "$cmd 2>&1; true" >"$outfile" 2>&1 || true
}

# assert_file_contains <pod> <container> <path> <expected> <label>
assert_file_contains() {
	local pod="$1" c="$2" path="$3" want="$4" label="$5" out
	out="$OUT_DIR/${label//\//_}.txt"
	exec_in "$pod" "$c" "cat $path" "$out"
	if grep -q "$want" "$out"; then
		pass "$label: $path contains '$want'"
		return 0
	fi
	fail "$label: $path missing '$want' (see $out)"
	return 1
}

# assert_read_only <pod> <container> <dir> <label>: a write into a read-only mount
# must be refused. A successful write is a translation bug (RO downgraded to RW).
assert_read_only() {
	local pod="$1" c="$2" dir="$3" label="$4" out
	out="$OUT_DIR/${label//\//_}-ro.txt"
	exec_in "$pod" "$c" "echo probe > $dir/.macvz-ro-probe && echo WROTE || echo READONLY" "$out"
	if grep -q READONLY "$out" && ! grep -q WROTE "$out"; then
		pass "$label: $dir is read-only (write refused)"
		return 0
	fi
	fail "$label: $dir accepted a write but should be read-only (see $out)"
	return 1
}

# assert_writable <pod> <container> <dir> <label>
assert_writable() {
	local pod="$1" c="$2" dir="$3" label="$4" out
	out="$OUT_DIR/${label//\//_}-rw.txt"
	exec_in "$pod" "$c" "echo rw > $dir/.macvz-rw-probe && cat $dir/.macvz-rw-probe" "$out"
	if grep -q '^rw$' "$out"; then
		pass "$label: $dir is writable"
		return 0
	fi
	fail "$label: $dir is not writable (see $out)"
	return 1
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
	if k get node "$NODE" -o jsonpath="{.status.conditions[?(@.type=='Ready')].status}" 2>/dev/null | grep -q True; then
		pass "node Ready"
	else
		fail "node $NODE is not Ready"
	fi
	[ "$FAILURES" = "$failures_before" ]
}

apply_fixture() {
	sed "s#image: busybox:1.36.1#image: $IMAGE#g" "$FIXTURE" >"$OUT_DIR/workload.applied.yaml"
	k apply -f "$OUT_DIR/workload.applied.yaml" >"$OUT_DIR/apply.log" 2>&1
}

phase_deploy() {
	log "Phase: deploy volume fixture + rollout"
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
	fi
}

phase_scheduling() {
	log "Phase: scheduling + Pod events"
	local pod node; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no fixture Pod found"; return 1; }
	node="$(kn get pod "$pod" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
	if [ "$node" = "$NODE" ]; then
		pass "Pod $pod scheduled onto MacVz node $NODE"
	else
		fail "Pod scheduled onto '$node', expected MacVz node '$NODE'"
	fi
	kn get events --field-selector "involvedObject.name=$pod" >"$OUT_DIR/pod-events.log" 2>&1 || true
	if grep -Eqi 'FailedScheduling|FailedCreatePodSandBox|FailedMount' "$OUT_DIR/pod-events.log"; then
		fail "Pod events contain scheduling/sandbox/mount failures (see $OUT_DIR/pod-events.log)"
	else
		pass "Pod events clean (no FailedScheduling/FailedCreatePodSandBox/FailedMount)"
	fi
}

phase_backend_evidence() {
	log "Phase: LinuxPod backend evidence (honesty gate)"
	if [ -z "${MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD:-}" ]; then
		skip "backend-evidence: set MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD to prove the Pod is LinuxPod-backed; volume results below are reported against the apple/container path (blocked on CRI-L serving #127/#128/#129)"
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
		skip "backend-evidence reports simulated=true: node is on the R17 prototype handshake — volume results reported against the apple/container path (blocked on #127/#128/#129; see $OUT_DIR/backend-evidence.txt)"
		return 0
	fi
	if grep -Eqi 'linuxpod|pod[-_ ]?vm|sandboxVM' "$OUT_DIR/backend-evidence.txt" \
		&& grep -Eqi 'simulated[":= ]+false|backend[":= ]+linuxpod|serving[":= ]+linuxpod' "$OUT_DIR/backend-evidence.txt"; then
		LINUXPOD_BACKED=1
		pass "Pod is served by a genuine (non-simulated) LinuxPod backend; volume results below are LinuxPod-backed (see $OUT_DIR/backend-evidence.txt)"
	else
		skip "backend-evidence did not prove a non-simulated LinuxPod-backed Pod; volume results reported against the apple/container path (see $OUT_DIR/backend-evidence.txt)"
	fi
}

# core_volume_checks <label-prefix> <pod>
# The reusable volume core, re-run verbatim after each churn event: configMap and
# secret content + read-only enforcement, downward API content, the SA-token
# projection, the shared emptyDir cross-visibility, and the Memory emptyDir being
# writable scratch.
core_volume_checks() {
	local label="$1" pod="$2"

	# configMap: content correct AND the mount is genuinely read-only.
	assert_file_contains "$pod" app /etc/config/app.conf "$CONFIGMAP_CONF_MARKER" "$label/configmap" || true
	assert_read_only "$pod" app /etc/config "$label/configmap" || true

	# secret: content correct AND read-only.
	assert_file_contains "$pod" app /etc/secret/token "$SECRET_MARKER" "$label/secret" || true
	assert_read_only "$pod" app /etc/secret "$label/secret" || true

	# downward API: the projected pod metadata is present.
	assert_file_contains "$pod" app /etc/podinfo/name "$pod" "$label/downward-api" || true

	# SA token projection: the in-cluster API access volume.
	local out="$OUT_DIR/$label-sa-token.txt"
	exec_in "$pod" app 'for f in token ca.crt namespace; do test -s /var/run/secrets/kubernetes.io/serviceaccount/$f && echo have:$f || echo missing:$f; done' "$out"
	if grep -q have:token "$out" && grep -q have:ca.crt "$out" && grep -q have:namespace "$out"; then
		pass "$label/sa-token: token, ca.crt, namespace projected"
	else
		fail "$label/sa-token: projected service-account token incomplete (see $out)"
	fi

	# shared emptyDir: visible both ways across the two containers.
	assert_file_contains "$pod" sidecar /shared/app-marker "$APP_SHARED_MARKER" "$label/shared-app-to-sidecar" || true
	assert_file_contains "$pod" app /shared/sidecar-marker "$SIDECAR_SHARED_MARKER" "$label/shared-sidecar-to-app" || true

	# Memory emptyDir: writable guest-local scratch.
	assert_writable "$pod" app /cache "$label/memory-emptydir" || true
}

phase_volume_matrix() {
	log "Phase: volume matrix (configMap/secret/downwardAPI + RO enforcement, Memory emptyDir)"
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for volume matrix"; return 1; }
	# configMap docroot served over httpd proves the configMap is both mounted and
	# readable as a real workload input, not just present on disk.
	assert_file_contains "$pod" app /www/index.html "$CONFIGMAP_MARKER" "matrix/configmap-docroot" || true
	core_volume_checks "initial" "$pod"
}

phase_projected_update() {
	log "Phase: projected-update (kubelet propagates a ConfigMap change into the Pod)"
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for projected-update"; return 1; }
	# Patch the ConfigMap's app.conf marker; a full-directory configMap projection
	# (not subPath) is updated in place by the kubelet within its sync period.
	if ! k -n "$NS" patch configmap "$CONFIGMAP" --type merge \
		-p "{\"data\":{\"app.conf\":\"marker=$CONFIGMAP_CONF_MARKER_V2\\n\"}}" >"$OUT_DIR/configmap-patch.log" 2>&1; then
		fail "could not patch ConfigMap (see $OUT_DIR/configmap-patch.log)"
		return 1
	fi
	local out updated=0 _
	out="$OUT_DIR/projected-update.txt"
	for _ in $(seq 1 60); do
		exec_in "$pod" app "cat /etc/config/app.conf" "$out"
		if grep -q "$CONFIGMAP_CONF_MARKER_V2" "$out"; then updated=1; break; fi
		sleep 2
	done
	if [ "$updated" = 1 ]; then
		pass "projected-update: ConfigMap change propagated into the running Pod"
	else
		fail "projected-update: ConfigMap change did NOT propagate within ~120s (see $out)"
	fi
}

phase_hostpath_policy() {
	log "Phase: hostPath policy"
	if [ -z "${MACVZ_VOLUME_HOSTPATH_PROBE_CMD:-}" ]; then
		skip "hostpath-policy: arbitrary hostPath is denied by the macOS default mount policy; allow/deny is covered hermetically (pkg/criserver TestLinuxPodVolumePolicyErrors / volume matrix). Set MACVZ_VOLUME_HOSTPATH_PROBE_CMD to also probe an allowlisted hostPath live."
		return 0
	fi
	if run_hook "$MACVZ_VOLUME_HOSTPATH_PROBE_CMD" >"$OUT_DIR/hostpath-probe.txt" 2>&1; then
		pass "hostpath-policy: allowlisted hostPath probe succeeded (see $OUT_DIR/hostpath-probe.txt)"
	else
		fail "hostpath-policy: allowlisted hostPath probe failed (see $OUT_DIR/hostpath-probe.txt)"
	fi
}

phase_vol_after_rollout() {
	log "Phase: volume matrix after rollout restart"
	if ! kn rollout restart "deploy/$DEPLOY" >"$OUT_DIR/rollout-restart.log" 2>&1; then
		fail "rollout restart failed (see $OUT_DIR/rollout-restart.log)"
		return 1
	fi
	kn rollout status "deploy/$DEPLOY" --timeout=5m >"$OUT_DIR/rollout-restart-status.log" 2>&1 || true
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Running Pod after rollout restart"; return 1; }
	core_volume_checks "after-rollout" "$pod"
}

phase_vol_after_cri_restart() {
	log "Phase: volume matrix after macvz-cri restart"
	if [ -z "${MACVZ_RESTART_CRI_CMD:-}" ]; then
		skip "vol-after-cri-restart (set MACVZ_RESTART_CRI_CMD)"
		return 0
	fi
	local pod uid_before; pod="$(pod_name)"; uid_before="$(pod_uid "$pod")"
	run_hook "$MACVZ_RESTART_CRI_CMD" >"$OUT_DIR/restart-cri.log" 2>&1 \
		|| fail "MACVZ_RESTART_CRI_CMD returned non-zero (see $OUT_DIR/restart-cri.log)"
	local ok=0 _
	for _ in $(seq 1 60); do
		pod="$(pod_name)"
		[ -n "$pod" ] && [ "$(pod_phase "$pod")" = "Running" ] && { ok=1; break; }
		sleep 2
	done
	[ "$ok" = 1 ] || { fail "Pod not Running after macvz-cri restart (see $OUT_DIR/restart-cri.log)"; return 1; }
	if [ -n "$uid_before" ] && [ "$(pod_uid "$pod")" = "$uid_before" ]; then
		pass "Pod UID preserved across macvz-cri restart ($uid_before)"
	else
		fail "Pod UID changed across macvz-cri restart (was '$uid_before', now '$(pod_uid "$pod")')"
	fi
	core_volume_checks "after-cri-restart" "$pod"
}

phase_vol_after_helper_restart() {
	log "Phase: volume matrix after LinuxPod helper restart"
	if [ -z "${MACVZ_RESTART_HELPER_CMD:-}" ]; then
		skip "vol-after-helper-restart (set MACVZ_RESTART_HELPER_CMD)"
		return 0
	fi
	if [ "$LINUXPOD_BACKED" != 1 ]; then
		skip "vol-after-helper-restart: node not proven LinuxPod-backed, so a helper restart exercises nothing real (blocked on #127/#128/#129)"
		return 0
	fi
	run_hook "$MACVZ_RESTART_HELPER_CMD" >"$OUT_DIR/restart-helper.log" 2>&1 \
		|| fail "MACVZ_RESTART_HELPER_CMD returned non-zero (see $OUT_DIR/restart-helper.log)"
	local pod ok=0 _
	for _ in $(seq 1 60); do
		pod="$(pod_name)"
		[ -n "$pod" ] && [ "$(pod_phase "$pod")" = "Running" ] && { ok=1; break; }
		sleep 2
	done
	[ "$ok" = 1 ] && pass "Pod Running after helper restart" \
		|| { fail "Pod not Running after helper restart (see $OUT_DIR/restart-helper.log)"; return 1; }
	core_volume_checks "after-helper-restart" "$pod"
}

phase_cleanup() {
	log "Phase: cleanup"
	k delete -f "$OUT_DIR/workload.applied.yaml" --wait=true --timeout=3m >"$OUT_DIR/cleanup.log" 2>&1 || true
	local remaining
	remaining="$(kn get pods -l app=linuxpod-vol -o name 2>/dev/null | wc -l | tr -d ' ')"
	[ "$remaining" = 0 ] && pass "no fixture Pods remain after delete" || fail "$remaining fixture Pod(s) remain after delete"

	# Residual materialized mount/rootfs audit (acceptance: cleanup leaves no stale
	# materialized mount/rootfs state). Without the hook this can't be proven from
	# the cluster side, so skip loudly rather than claim it.
	if [ -z "${MACVZ_LINUXPOD_RESIDUAL_CMD:-}" ]; then
		skip "cleanup/residual: set MACVZ_LINUXPOD_RESIDUAL_CMD to assert no residual LinuxPod materialized mount/rootfs state remains on the node"
		return 0
	fi
	if run_hook "$MACVZ_LINUXPOD_RESIDUAL_CMD" >"$OUT_DIR/residual.txt" 2>"$OUT_DIR/residual.err"; then
		# A correct backend prints nothing (no residual). Any non-empty line is stale state.
		if [ -s "$OUT_DIR/residual.txt" ]; then
			fail "cleanup/residual: residual materialized mount/rootfs state remains (see $OUT_DIR/residual.txt)"
		else
			pass "cleanup/residual: no residual materialized mount/rootfs state"
		fi
	else
		fail "MACVZ_LINUXPOD_RESIDUAL_CMD failed (see $OUT_DIR/residual.err)"
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
phase_volume_matrix
phase_projected_update
phase_hostpath_policy
phase_vol_after_rollout
phase_vol_after_cri_restart
phase_vol_after_helper_restart
phase_cleanup
phase_route_after

echo
if [ "$FAILURES" -eq 0 ] && [ "$SKIPS" -eq 0 ]; then
	pass "CRI-L8-3 LinuxPod volume-projection matrix suite: all checks passed (diagnostics in $OUT_DIR)"
	exit 0
fi
if [ "$FAILURES" -eq 0 ]; then
	pass "CRI-L8-3 LinuxPod volume-projection matrix suite: checks passed with $SKIPS skipped (LinuxPod framing or a churn hook was unproven; see #127/#128/#129). Diagnostics in $OUT_DIR"
	exit 0
fi
fail "CRI-L8-3 LinuxPod volume-projection matrix suite: $FAILURES check(s) failed (diagnostics in $OUT_DIR)"
exit 1
