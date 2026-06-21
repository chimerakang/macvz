#!/usr/bin/env bash
#
# run.sh — hello-http minimal public HTTP demo (issue #61).
#
# Deploys a stock public nginx image onto one or more MacVz virtual nodes, fronts
# it with a ClusterIP Service, and proves it serves real HTTP traffic over the
# exact path a human uses from a browser: `kubectl port-forward svc/hello`, then
# an HTTP GET against http://127.0.0.1:<port>/.
#
# It is the automated half of the demo: the README documents the manual browser
# smoke, and this script asserts the same access path non-interactively so the
# example can gate CI. On any failure it captures diagnostics and exits non-zero.
#
# Usage:
#   KUBECONFIG=... ./run.sh                      # apply, verify over port-forward, tear down
#   KUBECONFIG=... MACVZ_HELLO_KEEP=1 ./run.sh   # leave namespace for manual browser smoke
#   KUBECONFIG=... MACVZ_HELLO_REPLICAS=3 ./run.sh  # multi-node spread
#
# Environment:
#   KUBECONFIG              Cluster credentials (standard kubectl resolution).
#   KUBECTL                 kubectl binary (default: kubectl).
#   MACVZ_HELLO_NAMESPACE   Namespace (default: macvz-hello, as in the manifests).
#   MACVZ_HELLO_IMAGE       arm64 nginx image (default: nginx:1.27-alpine).
#   MACVZ_HELLO_REPLICAS    Deployment replicas (default: 1; raise for multi-node).
#   MACVZ_HELLO_PORT        Local port for the browser/port-forward (default: 8080).
#   MACVZ_HELLO_TIMEOUT     Per-wait timeout in seconds (default: 180).
#   MACVZ_HELLO_DIAG_DIR    Directory for failure diagnostics (default: a mktemp dir).
#   MACVZ_HELLO_KEEP        If 1, skip teardown and print a manual port-forward hint.
set -uo pipefail

KUBECTL="${KUBECTL:-kubectl}"
NS="${MACVZ_HELLO_NAMESPACE:-macvz-hello}"
IMAGE="${MACVZ_HELLO_IMAGE:-nginx:1.27-alpine}"
REPLICAS="${MACVZ_HELLO_REPLICAS:-1}"
PORT="${MACVZ_HELLO_PORT:-8080}"
TIMEOUT="${MACVZ_HELLO_TIMEOUT:-180}"
DIAG_DIR="${MACVZ_HELLO_DIAG_DIR:-}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS="$HERE/manifests"
PF_PID=""

FAILURES=0

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
	[ -d "$MANIFESTS" ] || die "manifests directory not found: $MANIFESTS"
	k get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1 || die "cannot reach the Kubernetes API server (check KUBECONFIG)"

	local nodes
	nodes="$(k get nodes -l type=virtual-kubelet -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null)"
	if [ -z "$nodes" ]; then
		log "Warning: no nodes labeled type=virtual-kubelet detected; relying on the manifests' nodeSelector/tolerations"
	else
		log "MacVz virtual nodes: $nodes"
	fi

	[ -z "$DIAG_DIR" ] && DIAG_DIR="$(mktemp -d -t macvz-hello)"
	mkdir -p "$DIAG_DIR"
}

# --- apply -------------------------------------------------------------------
# render streams every manifest with the image, namespace, and replica count
# substituted, so the env overrides apply without editing the files on disk.
render() {
	awk 'FNR==1 && NR>1 {print "---"} {print}' "$MANIFESTS"/*.yaml |
		sed -e "s|nginx:1.27-alpine|${IMAGE}|g" \
		    -e "s|namespace: macvz-hello|namespace: ${NS}|g" \
		    -e "s|name: macvz-hello$|name: ${NS}|g" \
		    -e "s|replicas: 1|replicas: ${REPLICAS}|g"
}

apply_fixture() {
	log "Applying demo to namespace $NS (image $IMAGE, replicas $REPLICAS)"
	render | k apply -f - >/dev/null || die "kubectl apply failed; the manifests or cluster are not ready"
	pass "demo manifests applied"
}

# --- rollout -----------------------------------------------------------------
phase_rollout() {
	log "Phase: Deployment rollout"
	if kn rollout status deploy/hello --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
		pass "deployment hello rolled out"
	else
		fail "deployment hello did not roll out within ${TIMEOUT}s"
	fi

	local want got
	want="$(kn get deploy hello -o jsonpath='{.spec.replicas}' 2>/dev/null)"
	got="$(kn get deploy hello -o jsonpath='{.status.availableReplicas}' 2>/dev/null)"
	if [ -n "$want" ] && [ "${got:-0}" = "$want" ]; then
		pass "hello has $got/$want available replicas (readiness probe passing)"
	else
		fail "hello availableReplicas=${got:-0}, want ${want:-?}"
	fi
}

# --- browser-visible access path: port-forward the Service, then HTTP GET -----
phase_http() {
	log "Phase: HTTP over kubectl port-forward (the browser access path)"

	# Forward a local port to the Service, exactly as a human would before opening
	# a browser. port-forward resolves svc/hello to a backing Pod and proxies into
	# the micro-VM (pkg/provider/portforward.go).
	kn port-forward svc/hello "${PORT}:80" >"$DIAG_DIR/port-forward.log" 2>&1 &
	PF_PID=$!

	# Wait for the local listener to come up (the forward is async).
	local url="http://127.0.0.1:${PORT}/" up=""
	for _ in $(seq 1 30); do
		if ! kill -0 "$PF_PID" 2>/dev/null; then
			fail "kubectl port-forward exited early (see $DIAG_DIR/port-forward.log)"
			return
		fi
		if curl -fsS -o /dev/null --max-time 3 "$url" 2>/dev/null; then up=1; break; fi
		sleep 1
	done
	if [ -z "$up" ]; then
		fail "no HTTP response on $url after port-forward (see $DIAG_DIR/port-forward.log)"
		return
	fi

	# Status line + body assertions: this is what the browser would render.
	local code body
	code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$url" 2>/dev/null)"
	body="$(curl -fsS --max-time 5 "$url" 2>/dev/null)"
	printf '%s' "$body" >"$DIAG_DIR/response.html"

	if [ "$code" = 200 ]; then
		pass "GET $url -> HTTP 200"
	else
		fail "GET $url -> HTTP ${code:-<none>}, want 200"
	fi
	if printf '%s' "$body" | grep -q "It works on MacVz"; then
		pass "response body is the MacVz hello page"
	else
		fail "response body did not contain the expected page marker"
	fi
	# The page is rendered per-Pod via the Downward API; confirm substitution ran
	# (no literal placeholder left behind) and report which node answered.
	# shellcheck disable=SC2016 # this intentionally searches for a literal template placeholder.
	if printf '%s' "$body" | grep -q '\${POD_NAME}'; then
		fail "page still contains an unsubstituted \${POD_NAME} placeholder"
	else
		local served
		served="$(printf '%s' "$body" | grep -A1 'Served by pod' | grep -oE '<dd>[^<]+' | head -1 | sed 's/<dd>//')"
		pass "page rendered for pod ${served:-<unknown>} (Downward API substitution ran)"
	fi
}

# --- logs --------------------------------------------------------------------
phase_logs() {
	log "Phase: kubectl logs"
	if kn logs -l app=hello --tail=20 2>/dev/null | grep -q "macvz-hello nginx starting"; then
		pass "kubectl logs returned the workload's startup banner"
	else
		fail "kubectl logs did not contain the expected startup banner"
	fi
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
		echo "### hello logs"; kn logs -l app=hello --tail=100 2>&1
	} >"$DIAG_DIR/diagnostics.txt" 2>&1
	log "Diagnostics written: $DIAG_DIR/diagnostics.txt"
}

stop_forward() {
	[ -n "$PF_PID" ] || return
	kill "$PF_PID" 2>/dev/null || true
	wait "$PF_PID" 2>/dev/null || true
	PF_PID=""
}

teardown() {
	if [ "${MACVZ_HELLO_KEEP:-}" = 1 ]; then
		log "MACVZ_HELLO_KEEP=1 set; leaving namespace $NS in place"
		stop_forward
		log "Browser smoke: run 'kubectl -n $NS port-forward svc/hello ${PORT}:80' and open http://127.0.0.1:${PORT}/"
		return
	fi
	stop_forward
	log "Teardown: removing namespace $NS"
	k delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
}

cleanup() { [ "${MACVZ_HELLO_KEEP:-}" = 1 ] || stop_forward; }
trap cleanup EXIT

# --- main --------------------------------------------------------------------
main() {
	preflight
	apply_fixture
	phase_rollout
	phase_http
	phase_logs

	[ "$FAILURES" -ne 0 ] && dump_diagnostics
	teardown

	echo
	if [ "$FAILURES" -eq 0 ]; then
		pass "hello-http demo: all checks passed"
		log  "Browser smoke: run 'kubectl -n $NS port-forward svc/hello ${PORT}:80' and open http://127.0.0.1:${PORT}/"
		return 0
	fi
	fail "hello-http demo: $FAILURES check(s) failed (see $DIAG_DIR/diagnostics.txt)"
	return 1
}

main "$@"
