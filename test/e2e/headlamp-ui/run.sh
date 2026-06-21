#!/usr/bin/env bash
#
# run.sh — Headlamp management-UI compatibility fixture (issue #63).
#
# Deploys Headlamp (a Kubernetes-native management UI) onto a MacVz virtual node
# and validates it end to end:
#   - Deployment rollout / availability (#45) with healthy probes (#50)
#   - the Pod lands on a virtual-kubelet node and does not restart-loop
#   - the projected ServiceAccount token is mounted (#51)
#   - the UI is reachable in a browser via `kubectl port-forward` (#28) and
#     serves its SPA + /config — the documented browser access path
#   - RBAC-limited interaction: the bound `view` role can list but not mutate
#
# A MacVz virtual node runs no kube-proxy, so browser access is via port-forward,
# not NodePort. That is the supported path; see README.md and docs/MANAGEMENT_UI.md.
#
# Usage:
#   KUBECONFIG=... ./run.sh                      # apply, validate, tear down
#   KUBECONFIG=... MACVZ_HEADLAMP_KEEP=1 ./run.sh # leave the namespace + a
#                                                  # port-forward hint for manual use
#
# Environment:
#   KUBECONFIG               Cluster credentials (standard kubectl resolution).
#   KUBECTL                  kubectl binary (default: kubectl).
#   MACVZ_HEADLAMP_NAMESPACE Namespace (default: macvz-headlamp, as in manifests).
#   MACVZ_HEADLAMP_IMAGE     Override the Headlamp image (default: the pinned
#                            multi-arch tag in the manifest).
#   MACVZ_HEADLAMP_PORT      Local port for the port-forward smoke (default: 4466).
#   MACVZ_HEADLAMP_TIMEOUT   Per-wait timeout in seconds (default: 180).
#   MACVZ_HEADLAMP_DIAG_DIR  Directory for failure diagnostics (default: mktemp).
#   MACVZ_HEADLAMP_KEEP      If 1, skip teardown for manual inspection.
set -uo pipefail

KUBECTL="${KUBECTL:-kubectl}"
NS="${MACVZ_HEADLAMP_NAMESPACE:-macvz-headlamp}"
IMAGE="${MACVZ_HEADLAMP_IMAGE:-}"
PORT="${MACVZ_HEADLAMP_PORT:-4466}"
TIMEOUT="${MACVZ_HEADLAMP_TIMEOUT:-180}"
DIAG_DIR="${MACVZ_HEADLAMP_DIAG_DIR:-}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS="$HERE/manifests"
DEFAULT_IMAGE="ghcr.io/headlamp-k8s/headlamp:v0.30.0"

FAILURES=0
PF_PID=""

# --- logging -----------------------------------------------------------------
c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
pass() { printf "${c_green}PASS${c_off} %s\n" "$*"; }
skip() { printf "${c_yellow}SKIP${c_off} %s\n" "$*"; }
fail() { printf "${c_red}FAIL${c_off} %s\n" "$*"; FAILURES=$((FAILURES+1)); }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

k()  { "$KUBECTL" "$@"; }
kn() { "$KUBECTL" -n "$NS" "$@"; }

# --- preflight ---------------------------------------------------------------
preflight() {
	log "Preflight: cluster reachability, manifests, target nodes"
	command -v "$KUBECTL" >/dev/null 2>&1 || die "kubectl not found in PATH"
	command -v curl >/dev/null 2>&1 || die "curl not found in PATH (needed for the browser-visibility check)"
	[ -d "$MANIFESTS" ] || die "manifests directory not found: $MANIFESTS"
	k get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1 || die "cannot reach the Kubernetes API server (check KUBECONFIG)"

	local nodes
	nodes="$(k get nodes -l type=virtual-kubelet -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null)"
	if [ -z "$nodes" ]; then
		log "Warning: no nodes labeled type=virtual-kubelet detected; relying on the manifests' nodeSelector/tolerations"
	else
		log "MacVz virtual nodes: $nodes"
	fi

	if [ -z "$DIAG_DIR" ]; then
		DIAG_DIR="$(mktemp -d -t macvz-headlamp)"
	fi
	mkdir -p "$DIAG_DIR"
}

# --- apply -------------------------------------------------------------------
render() {
	# Concatenate manifests with document separators; substitute the namespace
	# and (when overridden) the image without editing the files on disk.
	if [ -n "$IMAGE" ]; then
		awk 'FNR==1 && NR>1 {print "---"} {print}' "$MANIFESTS"/*.yaml |
			sed -e "s|namespace: macvz-headlamp|namespace: ${NS}|g" \
			    -e "s|name: macvz-headlamp$|name: ${NS}|g" \
			    -e "s|name: macvz-headlamp-view$|name: ${NS}-view|g" \
			    -e "s|${DEFAULT_IMAGE}|${IMAGE}|g"
	else
		awk 'FNR==1 && NR>1 {print "---"} {print}' "$MANIFESTS"/*.yaml |
			sed -e "s|namespace: macvz-headlamp|namespace: ${NS}|g" \
			    -e "s|name: macvz-headlamp$|name: ${NS}|g" \
			    -e "s|name: macvz-headlamp-view$|name: ${NS}-view|g"
	fi
}

apply_fixture() {
	log "Applying Headlamp fixture to namespace $NS${IMAGE:+ (image $IMAGE)}"
	if ! render | k apply -f - >/dev/null; then
		die "kubectl apply failed; the manifests or cluster are not ready"
	fi
	pass "fixture manifests applied"
}

# --- rollout + placement -----------------------------------------------------
phase_rollout() {
	log "Phase: Deployment rollout and placement"
	if kn rollout status deploy/headlamp --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
		pass "headlamp Deployment rolled out (#45)"
	else
		fail "headlamp Deployment did not roll out within ${TIMEOUT}s"
	fi

	local node restarts ready
	node="$(kn get pods -l app.kubernetes.io/name=headlamp -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null)"
	if [ -n "$node" ]; then
		pass "headlamp Pod scheduled onto node $node"
	else
		fail "headlamp Pod has no node assigned"
	fi

	ready="$(kn get pods -l app.kubernetes.io/name=headlamp -o jsonpath='{range .items[*]}{range .status.conditions[?(@.type=="Ready")]}{.status}{"\n"}{end}{end}' 2>/dev/null | grep -c True)"
	if [ "${ready:-0}" -ge 1 ]; then
		pass "headlamp Pod is Ready (readiness probe passing) (#50)"
	else
		fail "headlamp Pod is not Ready; readiness probe may be failing"
	fi

	restarts="$(kn get pods -l app.kubernetes.io/name=headlamp -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.restartCount}{"\n"}{end}{end}' 2>/dev/null | awk '{s+=$1} END{print s+0}')"
	if [ "${restarts:-0}" = 0 ]; then
		pass "no liveness-driven restarts (server stable) (#50)"
	else
		fail "headlamp restarted $restarts time(s); liveness may be flapping"
	fi
}

hlPod() {
	kn get pods -l app.kubernetes.io/name=headlamp \
		-o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1
}

# --- ServiceAccount projection (#51) -----------------------------------------
phase_sa_token() {
	log "Phase: ServiceAccount projected token (in-cluster API access)"
	local pod
	pod="$(hlPod)"
	[ -n "$pod" ] || { fail "no Running headlamp Pod to inspect"; return; }

	if kn exec "$pod" -c headlamp -- test -s /var/run/secrets/kubernetes.io/serviceaccount/token 2>/dev/null; then
		pass "projected token mounted at the standard path (#51)"
	else
		# Headlamp's image is distroless-ish; fall back to asserting the projected
		# volume is present in the Pod spec if exec has no `test`.
		if kn get pod "$pod" -o jsonpath='{.spec.volumes[*].projected.sources[*].serviceAccountToken.path}' 2>/dev/null | grep -q token; then
			pass "projected kube-api-access token volume present in Pod spec (#51)"
		else
			fail "ServiceAccount projected token not found (#51)"
		fi
	fi
}

# --- browser access path: port-forward + HTTP smoke (#28) --------------------
phase_http_smoke() {
	log "Phase: browser access via kubectl port-forward (#28)"
	# Start a background port-forward to the ClusterIP Service, exactly as a user
	# would to open the UI in a browser.
	kn port-forward svc/headlamp "${PORT}:80" >"$DIAG_DIR/port-forward.log" 2>&1 &
	PF_PID=$!

	# Wait for the local listener to come up.
	local up=0
	for _ in $(seq 1 30); do
		if ! kill -0 "$PF_PID" 2>/dev/null; then
			fail "kubectl port-forward exited early (see $DIAG_DIR/port-forward.log)"
			return
		fi
		if curl -fsS -o /dev/null "http://127.0.0.1:${PORT}/config" 2>/dev/null \
			|| curl -fsS -o /dev/null "http://127.0.0.1:${PORT}/" 2>/dev/null; then
			up=1; break
		fi
		sleep 1
	done

	if [ "$up" != 1 ]; then
		fail "port-forward to svc/headlamp never served a response on 127.0.0.1:${PORT}"
		return
	fi
	pass "kubectl port-forward established to svc/headlamp (browser path) (#28)"

	# /config returns JSON unconditionally from the server — a deterministic proof
	# the UI backend is live and routed through the Service.
	if curl -fsS "http://127.0.0.1:${PORT}/config" 2>/dev/null | grep -qi 'cluster\|inCluster\|"clusters"'; then
		pass "Headlamp /config served valid backend JSON"
	else
		# Some versions gate /config; fall back to the SPA shell at /.
		if curl -fsS "http://127.0.0.1:${PORT}/" 2>/dev/null | grep -qi 'headlamp\|<div id="root"\|<div id="main"'; then
			pass "Headlamp SPA served at / (UI reachable in a browser)"
		else
			fail "Headlamp responded but neither /config nor the SPA shell looked valid"
		fi
	fi
}

# --- RBAC-limited interaction ------------------------------------------------
phase_rbac() {
	log "Phase: RBAC-limited interaction (bound to the read-only 'view' role)"
	local sa="system:serviceaccount:${NS}:headlamp"

	# The view binding must allow reads cluster-wide...
	if k auth can-i list pods --as="$sa" -A >/dev/null 2>&1; then
		pass "view role can list pods cluster-wide (UI can browse resources)"
	else
		fail "view role cannot list pods; the UI would show nothing"
	fi

	# ...and must deny every mutation, proving the RBAC boundary holds.
	local denied=1 rule verb resource
	for rule in "delete|pods" "create|deployments" "update|configmaps"; do
		verb="${rule%%|*}"
		resource="${rule#*|}"
		if k auth can-i "$verb" "$resource" --as="$sa" -A >/dev/null 2>&1; then
			fail "view role unexpectedly permitted: $verb $resource"
			denied=0
		fi
	done
	[ "$denied" = 1 ] && pass "view role denied delete/create/update (read-only boundary holds)"
}

# --- diagnostics + teardown --------------------------------------------------
dump_diagnostics() {
	log "Capturing diagnostics to $DIAG_DIR"
	{
		echo "### nodes"; k get nodes -o wide
		echo "### deployment"; kn get deploy -o wide
		echo "### pods"; kn get pods -o wide
		echo "### describe pods"; kn describe pods
		echo "### service"; kn get svc -o wide
		echo "### endpoints"; kn get endpoints -o wide
		echo "### events"; kn get events --sort-by=.lastTimestamp
		echo "### headlamp logs"; kn logs -l app.kubernetes.io/name=headlamp --tail=100 2>&1
		echo "### port-forward log"; cat "$DIAG_DIR/port-forward.log" 2>/dev/null
	} >"$DIAG_DIR/diagnostics.txt" 2>&1
	log "Diagnostics written: $DIAG_DIR/diagnostics.txt"
}

cleanup_pf() {
	if [ -n "$PF_PID" ]; then
		kill "$PF_PID" >/dev/null 2>&1 || true
		wait "$PF_PID" 2>/dev/null || true
		PF_PID=""
	fi
}

teardown() {
	cleanup_pf
	if [ "${MACVZ_HEADLAMP_KEEP:-}" = 1 ]; then
		log "MACVZ_HEADLAMP_KEEP=1 set; leaving namespace $NS in place"
		log "Open the UI:   kubectl -n $NS port-forward svc/headlamp ${PORT}:80   then browse http://127.0.0.1:${PORT}"
		log "Login token:   kubectl -n $NS create token headlamp"
		return
	fi
	log "Teardown: removing namespace $NS and the ClusterRoleBinding"
	k delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
	k delete clusterrolebinding "${NS}-view" --wait=false >/dev/null 2>&1 || true
}

trap cleanup_pf EXIT

# --- main --------------------------------------------------------------------
main() {
	preflight
	apply_fixture
	phase_rollout
	phase_sa_token
	phase_http_smoke
	phase_rbac

	if [ "$FAILURES" -ne 0 ]; then
		dump_diagnostics
	fi
	teardown

	echo
	if [ "$FAILURES" -eq 0 ]; then
		pass "Headlamp management-UI fixture: all checks passed"
		return 0
	fi
	fail "Headlamp management-UI fixture: $FAILURES check(s) failed (see $DIAG_DIR/diagnostics.txt)"
	return 1
}

main "$@"
