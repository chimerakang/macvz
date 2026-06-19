#!/usr/bin/env bash
#
# run.sh — P6 Kubernetes workload compatibility fixture (issue #53).
#
# Deploys the multi-Deployment acceptance workload in test/e2e/p6-compat/manifests
# onto one or more MacVz virtual nodes and validates the rollout end to end:
# Deployment rollout/availability, probe-gated readiness, ConfigMap/Secret/Downward
# API wiring, ServiceAccount projection, logs, exec (with exit-code fidelity), and
# in-cluster Service consumption (cluster DNS + ClusterIP routing into the
# micro-VMs). On any failure it captures redacted diagnostics and exits non-zero,
# so it is suitable for P6 acceptance gating.
#
# This is the documented P6 acceptance workload: it exercises #45-#51 together.
# It requires those features to have landed in the running macvz-kubelet; on a
# kubelet missing one of them the relevant Pod stays Pending/Failed and the
# corresponding check fails with an actionable message.
#
# Usage:
#   KUBECONFIG=... ./run.sh                 # apply, validate, tear down
#   KUBECONFIG=... MACVZ_P6_KEEP=1 ./run.sh # leave the namespace for inspection
#
# Environment:
#   KUBECONFIG            Cluster credentials (standard kubectl resolution).
#   KUBECTL               kubectl binary (default: kubectl).
#   MACVZ_P6_NAMESPACE    Namespace for the fixture (default: macvz-p6, as in the
#                         manifests; overriding it re-targets every object).
#   MACVZ_P6_IMAGE        arm64 image providing sh/httpd/wget (default:
#                         busybox:1.36.1). Substituted into the manifests on apply.
#   MACVZ_P6_TIMEOUT      Per-wait timeout in seconds (default: 180).
#   MACVZ_P6_DIAG_DIR     Directory for failure diagnostics (default: a mktemp dir).
#   MACVZ_P6_KEEP         If set to 1, skip teardown so you can inspect the objects.
set -uo pipefail

KUBECTL="${KUBECTL:-kubectl}"
NS="${MACVZ_P6_NAMESPACE:-macvz-p6}"
IMAGE="${MACVZ_P6_IMAGE:-busybox:1.36.1}"
TIMEOUT="${MACVZ_P6_TIMEOUT:-180}"
DIAG_DIR="${MACVZ_P6_DIAG_DIR:-}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS="$HERE/manifests"

FAILURES=0

# --- logging -----------------------------------------------------------------
c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
pass() { printf "${c_green}PASS${c_off} %s\n" "$*"; }
skip() { printf "${c_yellow}SKIP${c_off} %s\n" "$*"; }
fail() { printf "${c_red}FAIL${c_off} %s\n" "$*"; FAILURES=$((FAILURES+1)); }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

k() { "$KUBECTL" "$@"; }
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

	if [ -z "$DIAG_DIR" ]; then
		DIAG_DIR="$(mktemp -d -t macvz-p6)"
	fi
	mkdir -p "$DIAG_DIR"
}

# --- apply -------------------------------------------------------------------
# render streams every manifest with the image substituted, so MACVZ_P6_IMAGE and
# MACVZ_P6_NAMESPACE overrides apply without editing the files on disk.
render() {
	# Concatenate manifests with document separators, then substitute the image
	# and (when overridden) the namespace.
	awk 'FNR==1 && NR>1 {print "---"} {print}' "$MANIFESTS"/*.yaml |
		sed -e "s|busybox:1.36.1|${IMAGE}|g" -e "s|namespace: macvz-p6|namespace: ${NS}|g" -e "s|name: macvz-p6$|name: ${NS}|g"
}

apply_fixture() {
	log "Applying fixture to namespace $NS (image $IMAGE)"
	if ! render | k apply -f - >/dev/null; then
		die "kubectl apply failed; the manifests or cluster are not ready"
	fi
	pass "fixture manifests applied"
}

# --- rollout + availability --------------------------------------------------
phase_rollout() {
	log "Phase: Deployment rollout"
	local d ok=1
	for d in web checker; do
		if ! kn rollout status "deploy/$d" --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
			fail "deployment $d did not roll out within ${TIMEOUT}s"; ok=0
		fi
	done
	[ "$ok" = 1 ] && pass "deployments web and checker rolled out"

	# web must have all replicas available; checker its single replica.
	local want got
	want="$(kn get deploy web -o jsonpath='{.spec.replicas}' 2>/dev/null)"
	got="$(kn get deploy web -o jsonpath='{.status.availableReplicas}' 2>/dev/null)"
	if [ -n "$want" ] && [ "${got:-0}" = "$want" ]; then
		pass "web has $got/$want available replicas (probes passing)"
	else
		fail "web availableReplicas=${got:-0}, want ${want:-?}"
	fi
}

# webPod prints the name of a Ready web Pod, or empty.
webPod() {
	kn get pods -l app=web \
		-o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1
}

# --- config / secret / downward wiring ---------------------------------------
phase_env_wiring() {
	log "Phase: ConfigMap / Secret / Downward API wiring (exec into a web Pod)"
	local pod page
	pod="$(webPod)"
	[ -n "$pod" ] || { fail "no Running web Pod to inspect"; return; }

	# The rendered status page reflects every input source at once. Reading it via
	# exec also proves exec works against a Deployment-managed Pod.
	page="$(kn exec "$pod" -c web -- cat /www/index.html 2>/dev/null)"
	if [ -z "$page" ]; then fail "could not read rendered status page from $pod"; return; fi

	check_kv() { # key  expected-substring  human-label
		if printf '%s\n' "$page" | grep -q "^$1=$2"; then
			pass "$3"
		else
			fail "$3 — got: $(printf '%s\n' "$page" | grep "^$1=" || echo "<missing $1>")"
		fi
	}
	check_kv greeting            hello-from-configmap "configMapKeyRef -> GREETING (#46)"
	check_kv cfg_app_greeting    hello-from-configmap "envFrom configMap prefix CFG_ (#46)"
	check_kv cfg_app_mode        production           "envFrom configMap second key (#46)"
	check_kv token_present       yes                  "secretKeyRef -> API_TOKEN present (#47)"
	check_kv configfile_present  yes                  "configMap volume file /etc/web/app.conf (#46)"
	check_kv secretfile_present  yes                  "secret volume file /etc/web-secret/api-token (#47)"
	check_kv mem_limit_mb        512                  "resourceFieldRef limits.memory -> 512 MiB (#48)"

	# Downward fieldRef: POD_NAME must equal the actual Pod name; NODE_NAME non-empty.
	if printf '%s\n' "$page" | grep -q "^pod=$pod$"; then
		pass "fieldRef metadata.name -> POD_NAME matches $pod (#48)"
	else
		fail "fieldRef POD_NAME mismatch — got: $(printf '%s\n' "$page" | grep '^pod=')"
	fi
	if printf '%s\n' "$page" | grep -q "^node=.\+"; then
		pass "fieldRef spec.nodeName -> NODE_NAME populated (#48)"
	else
		fail "fieldRef NODE_NAME empty"
	fi

	# ServiceAccount projection (#51): the projected kube-api-access token must be
	# mounted at the standard path.
	if kn exec "$pod" -c web -- test -s /var/run/secrets/kubernetes.io/serviceaccount/token 2>/dev/null; then
		pass "ServiceAccount projected token mounted (#51)"
	else
		fail "ServiceAccount projected token missing at /var/run/secrets/kubernetes.io/serviceaccount/token (#51)"
	fi
}

# --- logs + exec fidelity ----------------------------------------------------
phase_logs_exec() {
	log "Phase: logs and exec"
	local pod
	pod="$(webPod)"
	[ -n "$pod" ] || { fail "no Running web Pod for logs/exec"; return; }

	if kn logs "$pod" -c web 2>/dev/null | grep -q "macvz-p6 web starting"; then
		pass "kubectl logs returned the workload's startup banner"
	else
		fail "kubectl logs did not contain the expected startup banner"
	fi

	# Exit-code fidelity: a non-zero exit must propagate through exec.
	kn exec "$pod" -c web -- sh -c 'exit 9' >/dev/null 2>&1
	local rc=$?
	if [ "$rc" = 9 ]; then
		pass "kubectl exec propagated a non-zero exit code (9)"
	else
		fail "kubectl exec exit code=$rc, want 9"
	fi
}

# --- service consumption (DNS + ClusterIP routing) ---------------------------
phase_service() {
	log "Phase: in-cluster Service consumption (checker -> web)"

	# The checker Pod becomes Ready only after it successfully fetches the web
	# Service, so waiting for its readiness proves DNS + ClusterIP routing end to end.
	if kn wait --for=condition=Available deploy/checker --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
		pass "checker is Available (web Service reached via cluster DNS + ClusterIP)"
	else
		fail "checker never became Available — it could not reach the web Service"
	fi

	local cpod body
	cpod="$(kn get pods -l app=checker -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
	if [ -n "$cpod" ]; then
		body="$(kn exec "$cpod" -c checker -- cat /tmp/last 2>/dev/null)"
		if printf '%s\n' "$body" | grep -q "greeting=hello-from-configmap"; then
			pass "checker fetched web's rendered page over the Service"
		else
			fail "checker's last fetch did not contain web's page; got: $(printf '%s' "$body" | tr '\n' ' ')"
		fi
	fi
}

# --- probe / liveness stability ----------------------------------------------
phase_probes() {
	log "Phase: probe-gated readiness and liveness stability"
	# Every web Pod should be Ready (readiness/startup passed) with restartCount 0
	# (liveness has not tripped) — i.e. the probes are healthy, not flapping.
	local ready total restarts
	ready="$(kn get pods -l app=web -o jsonpath='{range .items[*]}{range .status.conditions[?(@.type=="Ready")]}{.status}{"\n"}{end}{end}' 2>/dev/null | grep -c True)"
	total="$(kn get pods -l app=web --no-headers 2>/dev/null | wc -l | tr -d ' ')"
	restarts="$(kn get pods -l app=web -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.restartCount}{"\n"}{end}{end}' 2>/dev/null | awk '{s+=$1} END{print s+0}')"
	if [ "${ready:-0}" = "${total:-0}" ] && [ "${total:-0}" -ge 1 ]; then
		pass "all $ready/$total web Pods Ready (readiness + startup probes passed) (#50)"
	else
		fail "web Pods Ready=${ready:-0}/${total:-0}"
	fi
	if [ "${restarts:-0}" = 0 ]; then
		pass "no liveness-driven restarts across web Pods (#50)"
	else
		fail "web Pods have $restarts restart(s); liveness may be flapping"
	fi
}

# --- diagnostics + teardown --------------------------------------------------
# redact masks Secret material so the bundle is safe to attach to an issue.
redact() {
	sed -E \
		-e 's/(api-token|db-password|password|token)([": ]+)[^",}]+/\1\2<redacted>/Ig' \
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
		echo "### web logs"; kn logs -l app=web --all-containers --tail=100 2>&1
		echo "### checker logs"; kn logs -l app=checker --all-containers --tail=100 2>&1
	} 2>&1 | redact >"$DIAG_DIR/diagnostics.txt"
	log "Diagnostics written: $DIAG_DIR/diagnostics.txt"
}

teardown() {
	if [ "${MACVZ_P6_KEEP:-}" = 1 ]; then
		log "MACVZ_P6_KEEP=1 set; leaving namespace $NS in place for inspection"
		return
	fi
	log "Teardown: removing namespace $NS"
	k delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
}

# --- main --------------------------------------------------------------------
main() {
	preflight
	apply_fixture
	phase_rollout
	phase_env_wiring
	phase_logs_exec
	phase_service
	phase_probes

	if [ "$FAILURES" -ne 0 ]; then
		dump_diagnostics
	fi
	teardown

	echo
	if [ "$FAILURES" -eq 0 ]; then
		pass "P6 compatibility fixture: all checks passed"
		return 0
	fi
	fail "P6 compatibility fixture: $FAILURES check(s) failed (see $DIAG_DIR/diagnostics.txt)"
	return 1
}

main "$@"
