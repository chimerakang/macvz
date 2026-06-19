#!/usr/bin/env bash
#
# MacVz multi-node end-to-end suite (issue #30). Exercises the beta-critical
# paths against a real cluster with one or more registered macvz-kubelet nodes:
# node registration, Pod lifecycle, logs, exec, a Service load-balanced across
# nodes, port-forward, and cleanup. On any failure it captures diagnostics and
# exits non-zero, so it is suitable for release gating.
#
# Topology: a Kubernetes control plane plus 2+ Apple Silicon Macs each running
# macvz-kubelet (see docs/E2E.md). With a single MacVz node it still runs every
# single-node phase and reports the cross-node Service check as SKIPPED — the
# documented fallback for hardware-limited environments.
#
# Environment:
#   KUBECONFIG            Cluster credentials (standard kubectl resolution).
#   MACVZ_E2E_NODES       Comma-separated MacVz node names. Default: auto-detect
#                         nodes labeled type=virtual-kubelet.
#   MACVZ_E2E_IMAGE       arm64 image providing sh, httpd, and wget.
#                         Default: busybox:1.36.1.
#   MACVZ_E2E_NAMESPACE   Namespace for test objects. Default: macvz-e2e.
#   MACVZ_E2E_DIAG_DIR    Directory for failure diagnostics. Default: a mktemp dir.
#   MACVZ_E2E_TIMEOUT     Per-wait timeout (seconds). Default: 120.
set -uo pipefail

KUBECTL="${KUBECTL:-kubectl}"
NS="${MACVZ_E2E_NAMESPACE:-macvz-e2e}"
IMAGE="${MACVZ_E2E_IMAGE:-busybox:1.36.1}"
TIMEOUT="${MACVZ_E2E_TIMEOUT:-120}"
PROVIDER_TAINT_KEY="virtual-kubelet.io/provider"

NODES=()        # resolved target node names
FAILURES=0
DIAG_DIR="${MACVZ_E2E_DIAG_DIR:-}"

# --- logging -----------------------------------------------------------------
c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
pass() { printf "${c_green}PASS${c_off} %s\n" "$*"; }
skip() { printf "${c_yellow}SKIP${c_off} %s\n" "$*"; }
fail() { printf "${c_red}FAIL${c_off} %s\n" "$*"; FAILURES=$((FAILURES+1)); }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

k() { "$KUBECTL" "$@"; }

# --- preflight ---------------------------------------------------------------
preflight() {
	log "Preflight: cluster reachability and target nodes"
	command -v "$KUBECTL" >/dev/null 2>&1 || die "kubectl not found in PATH"
	k get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1 || die "cannot reach the Kubernetes API server (check KUBECONFIG)"

	if [ -n "${MACVZ_E2E_NODES:-}" ]; then
		local IFS=','
		# shellcheck disable=SC2206
		NODES=(${MACVZ_E2E_NODES})
	else
		# Auto-detect MacVz virtual nodes by their well-known label.
		local detected
		detected="$(k get nodes -l type=virtual-kubelet -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)"
		while IFS= read -r n; do
			[ -n "$n" ] && NODES=("${NODES[@]}" "$n")
		done <<EOF
$detected
EOF
	fi

	[ "${#NODES[@]}" -ge 1 ] || die "no MacVz nodes found (set MACVZ_E2E_NODES or label nodes type=virtual-kubelet)"
	log "Target MacVz nodes (${#NODES[@]}): ${NODES[*]}"

	if [ -z "$DIAG_DIR" ]; then
		DIAG_DIR="$(mktemp -d -t macvz-e2e)"
	fi
	mkdir -p "$DIAG_DIR"

	k create namespace "$NS" >/dev/null 2>&1 || true
}

# --- phase 1: node registration ----------------------------------------------
phase_node_registration() {
	log "Phase: node registration"
	local n ok=1
	for n in "${NODES[@]}"; do
		if ! k wait --for=condition=Ready "node/$n" --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
			fail "node $n is not Ready"; ok=0; continue
		fi
		local arch taint
		arch="$(k get node "$n" -o jsonpath='{.status.nodeInfo.architecture}' 2>/dev/null)"
		[ "$arch" = "arm64" ] || { fail "node $n arch=$arch, want arm64"; ok=0; }
		taint="$(k get node "$n" -o jsonpath="{.spec.taints[?(@.key=='$PROVIDER_TAINT_KEY')].key}" 2>/dev/null)"
		[ "$taint" = "$PROVIDER_TAINT_KEY" ] || { fail "node $n missing provider taint $PROVIDER_TAINT_KEY"; ok=0; }
		# Capacity must advertise cpu/memory/pods.
		local cpu pods
		cpu="$(k get node "$n" -o jsonpath='{.status.capacity.cpu}' 2>/dev/null)"
		pods="$(k get node "$n" -o jsonpath='{.status.capacity.pods}' 2>/dev/null)"
		[ -n "$cpu" ] && [ -n "$pods" ] || { fail "node $n missing capacity (cpu=$cpu pods=$pods)"; ok=0; }
	done
	[ "$ok" = 1 ] && pass "all ${#NODES[@]} node(s) Ready, arm64, tainted, with capacity"
}

# --- phase 2: pod lifecycle + logs + exec ------------------------------------
phase_pod_lifecycle() {
	log "Phase: Pod lifecycle, logs, exec (node ${NODES[0]})"
	local pod="e2e-lifecycle"
	apply_pod "$pod" "${NODES[0]}" 'echo macvz-e2e-log; sleep 600'

	if ! k -n "$NS" wait --for=condition=Ready "pod/$pod" --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
		fail "pod $pod did not become Ready"; return
	fi
	pass "pod scheduled and Ready on ${NODES[0]}"

	# logs
	if k -n "$NS" logs "$pod" 2>/dev/null | grep -q "macvz-e2e-log"; then
		pass "kubectl logs returned workload output"
	else
		fail "kubectl logs did not return expected output"
	fi

	# exec (and exit-code fidelity)
	local arch rc
	arch="$(k -n "$NS" exec "$pod" -- uname -m 2>/dev/null)"
	if [ "$arch" = "aarch64" ]; then pass "kubectl exec ran in guest (uname -m = aarch64)"; else fail "exec arch=$arch, want aarch64"; fi
	k -n "$NS" exec "$pod" -- sh -c 'exit 7' >/dev/null 2>&1
	rc=$?
	if [ "$rc" = 7 ]; then pass "exec propagated non-zero exit code (7)"; else fail "exec exit code=$rc, want 7"; fi

	# delete
	k -n "$NS" delete pod "$pod" --wait=true --timeout="${TIMEOUT}s" >/dev/null 2>&1
	if k -n "$NS" get pod "$pod" >/dev/null 2>&1; then
		fail "pod $pod still present after delete"
	else
		pass "pod deleted and micro-VM torn down"
	fi
}

# --- phase 3: cross-node Service ---------------------------------------------
phase_cross_node_service() {
	log "Phase: Service across nodes"
	if [ "${#NODES[@]}" -lt 2 ]; then
		skip "cross-node Service needs >=2 MacVz nodes (have ${#NODES[@]}); single-node fallback"
		phase_single_node_service
		return
	fi

	local n1="${NODES[0]}" n2="${NODES[1]}"
	apply_backend "e2e-be-1" "$n1" "NODE:$n1"
	apply_backend "e2e-be-2" "$n2" "NODE:$n2"
	apply_service "e2e-hello" "e2e-hello"

	k -n "$NS" wait --for=condition=Ready pod/e2e-be-1 pod/e2e-be-2 --timeout="${TIMEOUT}s" >/dev/null 2>&1
	if ! wait_endpoints "e2e-hello" 2; then
		fail "Service e2e-hello did not get 2 ready endpoints"; return
	fi
	pass "Service has 2 ready endpoints (one per node)"

	# Curl the Service repeatedly from a client Pod; expect responses from BOTH
	# nodes, proving cross-node load-balancing over the mesh.
	local out
	# Single quotes intentional: the loop must run in the client Pod's shell.
	# shellcheck disable=SC2016
	out="$(run_client 'for i in $(seq 20); do wget -qO- http://e2e-hello 2>/dev/null; done')"
	if printf '%s' "$out" | grep -q "NODE:$n1" && printf '%s' "$out" | grep -q "NODE:$n2"; then
		pass "Service load-balanced across both Macs ($n1 and $n2 both reached)"
	else
		fail "Service did not reach both nodes; responses: $(printf '%s' "$out" | tr '\n' ' ')"
	fi
}

# Single-node fallback: a Service with one backend is reachable from a client.
phase_single_node_service() {
	local n1="${NODES[0]}"
	apply_backend "e2e-be-1" "$n1" "NODE:$n1"
	apply_service "e2e-hello" "e2e-hello"
	k -n "$NS" wait --for=condition=Ready pod/e2e-be-1 --timeout="${TIMEOUT}s" >/dev/null 2>&1
	if ! wait_endpoints "e2e-hello" 1; then
		fail "Service e2e-hello has no ready endpoint"; return
	fi
	local out
	out="$(run_client 'wget -qO- http://e2e-hello 2>/dev/null')"
	if printf '%s' "$out" | grep -q "NODE:$n1"; then
		pass "single-node Service reachable from client Pod"
	else
		fail "single-node Service not reachable; got: $out"
	fi
}

# --- phase 4: port-forward ---------------------------------------------------
phase_port_forward() {
	log "Phase: port-forward"
	if ! k -n "$NS" get pod e2e-be-1 >/dev/null 2>&1; then
		skip "port-forward needs a backend Pod (service phase did not run)"; return
	fi
	local lport=18080 pid out
	k -n "$NS" port-forward pod/e2e-be-1 "${lport}:8080" >/dev/null 2>&1 &
	pid=$!
	# Give the forward a moment to establish.
	local i=0
	while [ "$i" -lt 10 ]; do
		out="$(curl -s --max-time 3 "http://127.0.0.1:${lport}" 2>/dev/null)"
		[ -n "$out" ] && break
		i=$((i+1)); sleep 1
	done
	kill "$pid" >/dev/null 2>&1
	wait "$pid" 2>/dev/null
	if printf '%s' "$out" | grep -q "NODE:"; then
		pass "port-forward reached the in-Pod HTTP server"
	else
		fail "port-forward did not return a response"
	fi
}

# --- manifests ---------------------------------------------------------------
# tolerations shared by every MacVz workload.
TOLERATION='{"key":"virtual-kubelet.io/provider","operator":"Exists","effect":"NoSchedule"}'

apply_pod() {
	local name="$1" node="$2" cmd="$3"
	k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: { name: $name, namespace: $NS }
spec:
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: $node }
  tolerations: [ $TOLERATION ]
  containers:
    - name: c
      image: $IMAGE
      command: ["sh", "-c", "$cmd"]
      resources:
        requests: { cpu: "250m", memory: 256Mi }
        limits:   { cpu: "500m", memory: 256Mi }
EOF
}

apply_backend() {
	local name="$1" node="$2" marker="$3"
	k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $NS
  labels: { app: e2e-hello }
spec:
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: $node }
  tolerations: [ $TOLERATION ]
  containers:
    - name: c
      image: $IMAGE
      command: ["sh", "-c", "mkdir -p /w && echo $marker > /w/index.html && httpd -f -p 8080 -h /w"]
      ports: [ { containerPort: 8080 } ]
      resources:
        requests: { cpu: "250m", memory: 256Mi }
        limits:   { cpu: "500m", memory: 256Mi }
EOF
}

apply_service() {
	local name="$1" selector_app="$2"
	k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata: { name: $name, namespace: $NS }
spec:
  selector: { app: $selector_app }
  ports: [ { port: 80, targetPort: 8080 } ]
EOF
}

# run_client runs a throwaway alpine Pod that tolerates the provider taint and
# executes the given shell, printing its stdout.
run_client() {
	local cmd="$1"
	k -n "$NS" run "e2e-client-$$" --image="$IMAGE" --restart=Never --rm -i \
		--overrides="{\"spec\":{\"tolerations\":[$TOLERATION],\"nodeSelector\":{\"type\":\"virtual-kubelet\"}}}" \
		--command -- sh -c "$cmd" 2>/dev/null
}

# wait_endpoints waits until a Service has at least n ready endpoint addresses.
wait_endpoints() {
	local svc="$1" want="$2" i=0 got
	while [ "$i" -lt "$TIMEOUT" ]; do
		got="$(k -n "$NS" get endpoints "$svc" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null | wc -w | tr -d ' ')"
		[ "${got:-0}" -ge "$want" ] && return 0
		i=$((i+2)); sleep 2
	done
	return 1
}

# --- diagnostics + teardown --------------------------------------------------
dump_diagnostics() {
	log "Capturing diagnostics to $DIAG_DIR"
	{
		echo "### nodes"; k get nodes -o wide
		local n; for n in "${NODES[@]}"; do echo "### describe node $n"; k describe "node/$n"; done
		echo "### pods"; k -n "$NS" get pods -o wide
		echo "### describe pods"; k -n "$NS" describe pods
		echo "### endpoints"; k -n "$NS" get endpoints,endpointslices -o wide
		echo "### events"; k -n "$NS" get events --sort-by=.lastTimestamp
	} >"$DIAG_DIR/diagnostics.txt" 2>&1
	log "Diagnostics written: $DIAG_DIR/diagnostics.txt"
}

teardown() {
	log "Teardown: removing namespace $NS"
	k delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
}

# --- main --------------------------------------------------------------------
main() {
	preflight
	phase_node_registration
	phase_pod_lifecycle
	phase_cross_node_service
	phase_port_forward

	if [ "$FAILURES" -ne 0 ]; then
		dump_diagnostics
	fi
	teardown

	echo
	if [ "$FAILURES" -eq 0 ]; then
		pass "MacVz multi-node e2e suite: all checks passed (${#NODES[@]} node(s))"
		return 0
	fi
	fail "MacVz multi-node e2e suite: $FAILURES check(s) failed"
	return 1
}

main "$@"
