#!/usr/bin/env bash
#
# run.sh — guestbook real-app validation (issue #62, milestone P8).
#
# Deploys the multi-component guestbook application in
# test/examples/guestbook/manifests onto one or more MacVz virtual nodes and
# validates it end to end as a realistic Kubernetes app:
#
#   - three Deployments (redis-leader, redis-follower x2, frontend x3) roll out
#     through normal controllers and reach availability;
#   - Redis replication flows leader -> follower across two Deployments;
#   - the frontend Service is browser-visible via `kubectl port-forward`, serves
#     the guestbook page, accepts a new entry (write -> leader) and shows it back
#     (read <- follower), proving the full data path across all three tiers;
#   - logs, exec (with exit-code fidelity), scaling, and rollout restart all work
#     through Kubernetes;
#   - teardown removes the namespace and leaves no Pods/VMs behind.
#
# On any failure it captures a redacted diagnostics bundle and exits non-zero, so
# it is suitable for P8 acceptance gating.
#
# Usage:
#   KUBECONFIG=... ./run.sh                  # apply, validate, tear down
#   KUBECONFIG=... MACVZ_GB_KEEP=1 ./run.sh  # leave the namespace for inspection
#
# Environment:
#   KUBECONFIG             Cluster credentials (standard kubectl resolution).
#   KUBECTL                kubectl binary (default: kubectl).
#   MACVZ_GB_NAMESPACE     Namespace for the app (default: macvz-guestbook).
#   MACVZ_GB_BUSYBOX_IMAGE arm64 image with sh/httpd/nc (default: busybox:1.36.1).
#   MACVZ_GB_REDIS_IMAGE   arm64 redis image (default: redis:7-alpine).
#   MACVZ_GB_TIMEOUT       Per-wait timeout in seconds (default: 240).
#   MACVZ_GB_PF_PORT       Local port for the browser-visibility check (default: 18080).
#   MACVZ_GB_DIAG_DIR      Directory for failure diagnostics (default: a mktemp dir).
#   MACVZ_GB_KEEP          If set to 1, skip teardown so you can inspect the objects.
set -uo pipefail

KUBECTL="${KUBECTL:-kubectl}"
NS="${MACVZ_GB_NAMESPACE:-macvz-guestbook}"
BUSYBOX_IMAGE="${MACVZ_GB_BUSYBOX_IMAGE:-busybox:1.36.1}"
REDIS_IMAGE="${MACVZ_GB_REDIS_IMAGE:-redis:7-alpine}"
TIMEOUT="${MACVZ_GB_TIMEOUT:-240}"
PF_PORT="${MACVZ_GB_PF_PORT:-18080}"
DIAG_DIR="${MACVZ_GB_DIAG_DIR:-}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS="$HERE/manifests"

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
		DIAG_DIR="$(mktemp -d -t macvz-guestbook)"
	fi
	mkdir -p "$DIAG_DIR"
}

# --- apply -------------------------------------------------------------------
# render streams every manifest with the images/namespace substituted, so the
# overrides apply without editing the files on disk.
render() {
	awk 'FNR==1 && NR>1 {print "---"} {print}' "$MANIFESTS"/*.yaml |
		sed -e "s|busybox:1.36.1|${BUSYBOX_IMAGE}|g" \
		    -e "s|redis:7-alpine|${REDIS_IMAGE}|g" \
		    -e "s|namespace: macvz-guestbook|namespace: ${NS}|g" \
		    -e "s|name: macvz-guestbook$|name: ${NS}|g"
}

apply_app() {
	log "Applying guestbook to namespace $NS (busybox=$BUSYBOX_IMAGE redis=$REDIS_IMAGE)"
	if ! render | k apply -f - >/dev/null; then
		die "kubectl apply failed; the manifests or cluster are not ready"
	fi
	pass "guestbook manifests applied"
}

# --- rollout + availability --------------------------------------------------
phase_rollout() {
	log "Phase: Deployment rollout"
	local d ok=1
	for d in redis-leader redis-follower frontend; do
		if ! kn rollout status "deploy/$d" --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
			fail "deployment $d did not roll out within ${TIMEOUT}s"; ok=0
		fi
	done
	[ "$ok" = 1 ] && pass "deployments redis-leader, redis-follower, and frontend rolled out"

	local d want got
	for d in redis-follower frontend; do
		want="$(kn get deploy "$d" -o jsonpath='{.spec.replicas}' 2>/dev/null)"
		got="$(kn get deploy "$d" -o jsonpath='{.status.availableReplicas}' 2>/dev/null)"
		if [ -n "$want" ] && [ "${got:-0}" = "$want" ]; then
			pass "$d has $got/$want available replicas"
		else
			fail "$d availableReplicas=${got:-0}, want ${want:-?}"
		fi
	done
}

# frontendPod / followerPod print the name of a Running Pod, or empty.
frontendPod() { kn get pods -l app=frontend -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1; }
followerPod() { kn get pods -l role=follower -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1; }

# --- redis replication across Deployments ------------------------------------
phase_replication() {
	log "Phase: Redis replication (leader -> follower across two Deployments)"
	local fp
	fp="$(followerPod)"
	[ -n "$fp" ] || { fail "no Running redis-follower Pod to inspect"; return; }

	# A follower reports master_link_status:up only once it has synced from the
	# leader Service — proving cross-Deployment, in-cluster Service connectivity.
	# shellcheck disable=SC2016 # REDIS_PASSWORD expands inside the container.
	if kn exec "$fp" -c redis -- sh -c 'redis-cli -a "$REDIS_PASSWORD" --no-auth-warning info replication | tr -d "\r" | grep -q "master_link_status:up"' >/dev/null 2>&1; then
		pass "redis-follower is synced to redis-leader (master_link_status:up)"
	else
		fail "redis-follower never reported master_link_status:up"
	fi
}

# --- browser-visible Service + functional guestbook --------------------------
start_port_forward() {
	# Forward the frontend Service to a local port, the documented browser path.
	kn port-forward "svc/frontend" "${PF_PORT}:80" >"$DIAG_DIR/port-forward.log" 2>&1 &
	PF_PID=$!
	for _ in $(seq 1 40); do
		if curl -fsS "http://localhost:${PF_PORT}/cgi-bin/api" >/dev/null 2>&1; then return 0; fi
		kill -0 "$PF_PID" 2>/dev/null || { PF_PID=""; return 1; }
		sleep 0.5
	done
	return 1
}

stop_port_forward() {
	[ -n "$PF_PID" ] || return
	kill "$PF_PID" 2>/dev/null || true
	wait "$PF_PID" 2>/dev/null || true
	PF_PID=""
}

phase_browser() {
	log "Phase: browser-visible Service + functional guestbook (port-forward)"
	if ! start_port_forward; then
		fail "could not reach the frontend Service through kubectl port-forward"
		return
	fi
	local base="http://localhost:${PF_PORT}"

	# 1. Landing page is served (the browser entry point).
	if curl -fsS "${base}/" | grep -q 'url=cgi-bin/api'; then
		pass "frontend Service serves the landing page (browser-visible)"
	else
		fail "landing page not served at ${base}/"
	fi

	# 2. The guestbook page renders.
	if curl -fsS "${base}/cgi-bin/api" | grep -qi 'guestbook'; then
		pass "guestbook page renders through the Service"
	else
		fail "guestbook page did not render"
	fi

	# 3. Submit an entry (write -> leader) and see it back (read <- follower).
	local marker
	marker="macvz-p8-$$-$(date +%s 2>/dev/null || echo now)"
	curl -fsS -o /dev/null -X POST --data-urlencode "entry=${marker}" "${base}/cgi-bin/api" 2>/dev/null
	local found=0
	for _ in $(seq 1 20); do
		if curl -fsS "${base}/cgi-bin/api" | grep -q "$marker"; then found=1; break; fi
		sleep 0.5
	done
	if [ "$found" = 1 ]; then
		pass "submitted entry written to redis-leader and read back from redis-follower"
	else
		fail "submitted guestbook entry never appeared (write/replication/read path broken)"
	fi

	# 4. HTML escaping (no stored-XSS through an entry).
	curl -fsS -o /dev/null -X POST --data-urlencode 'entry=<script>x</script>' "${base}/cgi-bin/api" 2>/dev/null
	local page; page="$(curl -fsS "${base}/cgi-bin/api")"
	if printf '%s' "$page" | grep -q '&lt;script&gt;' && ! printf '%s' "$page" | grep -q '<script>x'; then
		pass "entries are HTML-escaped (no markup injection)"
	else
		fail "entry was not HTML-escaped"
	fi

	stop_port_forward
}

# --- logs + exec fidelity ----------------------------------------------------
phase_logs_exec() {
	log "Phase: logs and exec"
	local pod
	pod="$(frontendPod)"
	[ -n "$pod" ] || { fail "no Running frontend Pod for logs/exec"; return; }

	if kn logs "$pod" -c frontend 2>/dev/null | grep -q "macvz-guestbook frontend starting"; then
		pass "kubectl logs returned the frontend startup banner"
	else
		fail "kubectl logs did not contain the expected startup banner"
	fi

	# The CGI must be staged and executable in the served tree.
	if kn exec "$pod" -c frontend -- test -x /www/cgi-bin/api >/dev/null 2>&1; then
		pass "CGI staged and executable at /www/cgi-bin/api (exec works)"
	else
		fail "CGI not present/executable in the frontend Pod"
	fi

	# Exit-code fidelity: a non-zero exit must propagate through exec.
	kn exec "$pod" -c frontend -- sh -c 'exit 9' >/dev/null 2>&1
	local rc=$?
	if [ "$rc" = 9 ]; then
		pass "kubectl exec propagated a non-zero exit code (9)"
	else
		fail "kubectl exec exit code=$rc, want 9"
	fi
}

# --- scaling -----------------------------------------------------------------
phase_scaling() {
	log "Phase: scaling the frontend through the controller"
	if ! kn scale deploy/frontend --replicas=5 >/dev/null 2>&1; then
		fail "kubectl scale deploy/frontend --replicas=5 failed"; return
	fi
	if kn rollout status deploy/frontend --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
		local got
		got="$(kn get deploy frontend -o jsonpath='{.status.availableReplicas}' 2>/dev/null)"
		if [ "${got:-0}" = 5 ]; then
			pass "frontend scaled out to 5/5 available replicas"
		else
			fail "frontend scaled but availableReplicas=${got:-0}, want 5"
		fi
	else
		fail "frontend did not reach 5 replicas within ${TIMEOUT}s"
	fi

	# Scale back to the manifest default so the rest of the run is deterministic.
	if kn scale deploy/frontend --replicas=3 >/dev/null 2>&1 &&
		kn rollout status deploy/frontend --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
		pass "frontend scaled back in to 3 replicas"
	else
		fail "frontend did not scale back to 3 replicas"
	fi
}

# --- rollout restart ---------------------------------------------------------
phase_rollout_restart() {
	log "Phase: rollout restart (rolling update through the controller)"
	if ! kn rollout restart deploy/frontend >/dev/null 2>&1; then
		fail "kubectl rollout restart deploy/frontend failed"; return
	fi
	if kn rollout status deploy/frontend --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
		pass "frontend rolling restart completed"
	else
		fail "frontend rollout restart did not complete within ${TIMEOUT}s"
		return
	fi

	# Data lives in Redis, not the frontend: it must survive a frontend restart.
	if start_port_forward; then
		if curl -fsS "http://localhost:${PF_PORT}/cgi-bin/api" | grep -q 'macvz-p8-'; then
			pass "guestbook entries survived the frontend restart (state is in Redis)"
		else
			fail "guestbook entries lost after frontend restart"
		fi
		stop_port_forward
	else
		fail "could not re-reach the frontend Service after restart"
	fi
}

# --- diagnostics + teardown --------------------------------------------------
# redact masks Secret material so the bundle is safe to attach to an issue.
redact() {
	sed -E \
		-e 's/(redis-password|password|masterauth|requirepass|token)([": ]+)[^",}]+/\1\2<redacted>/Ig' \
		-e 's/([A-Za-z0-9+/]{40,}={0,2})/<redacted-b64>/g'
}

dump_diagnostics() {
	log "Capturing diagnostics to $DIAG_DIR"
	{
		echo "### nodes"; k get nodes -o wide
		echo "### deployments"; kn get deploy -o wide
		echo "### replicasets"; kn get rs -o wide
		echo "### pods"; kn get pods -o wide
		echo "### describe deployments"; kn describe deploy
		echo "### describe pods"; kn describe pods
		echo "### endpoints"; kn get endpoints -o wide
		echo "### services"; kn get svc -o wide
		echo "### configmaps (metadata only)"; kn get configmaps
		echo "### secrets (metadata only, values intentionally omitted)"; kn get secrets
		echo "### events"; kn get events --sort-by=.lastTimestamp
		echo "### frontend logs"; kn logs -l app=frontend --all-containers --tail=100 2>&1
		echo "### redis-leader logs"; kn logs -l role=leader --all-containers --tail=100 2>&1
		echo "### redis-follower logs"; kn logs -l role=follower --all-containers --tail=100 2>&1
		echo "### port-forward log"; cat "$DIAG_DIR/port-forward.log" 2>/dev/null
	} 2>&1 | redact >"$DIAG_DIR/diagnostics.txt"
	log "Diagnostics written: $DIAG_DIR/diagnostics.txt"
}

teardown() {
	stop_port_forward
	if [ "${MACVZ_GB_KEEP:-}" = 1 ]; then
		log "MACVZ_GB_KEEP=1 set; leaving namespace $NS in place for inspection"
		return
	fi
	log "Teardown: removing namespace $NS and verifying no Pods are left behind"
	k delete namespace "$NS" --wait=true --timeout="${TIMEOUT}s" >/dev/null 2>&1
	# Acceptance: cleanup leaves no Pods/VMs. The namespace delete blocks until
	# every Pod (and its micro-VM) is gone; confirm the namespace is actually gone.
	if k get namespace "$NS" >/dev/null 2>&1; then
		fail "namespace $NS still present after delete; Pods/VMs may be orphaned"
	else
		pass "namespace $NS fully removed (no Pods/VMs left behind)"
	fi
}

# --- main --------------------------------------------------------------------
main() {
	trap 'stop_port_forward' EXIT
	preflight
	apply_app
	phase_rollout
	phase_replication
	phase_browser
	phase_logs_exec
	phase_scaling
	phase_rollout_restart

	if [ "$FAILURES" -ne 0 ]; then
		dump_diagnostics
	fi
	teardown

	echo
	if [ "$FAILURES" -eq 0 ]; then
		pass "guestbook real-app validation: all checks passed"
		return 0
	fi
	fail "guestbook real-app validation: $FAILURES check(s) failed (see $DIAG_DIR/diagnostics.txt)"
	return 1
}

main "$@"
