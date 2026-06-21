#!/usr/bin/env bash
#
# run.sh - long-duration MacVz soak harness (issue #71).
#
# It drives the existing e2e/P8 fixtures in a loop while sampling cluster state
# and, when operator-provided hooks are present, restarts kubelet/helper services
# and verifies restart recovery/orphan cleanup.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"

KUBECTL="${KUBECTL:-kubectl}"
NAMESPACE_PREFIX="${MACVZ_SOAK_NAMESPACE_PREFIX:-macvz-soak}"
DURATION="${MACVZ_SOAK_DURATION:-2h}"
ITERATIONS="${MACVZ_SOAK_ITERATIONS:-0}"
TIMEOUT="${MACVZ_SOAK_TIMEOUT:-180}"
SLEEP="${MACVZ_SOAK_SLEEP:-30}"
FIXTURES="${MACVZ_SOAK_FIXTURES:-e2e}"
IMAGE="${MACVZ_SOAK_IMAGE:-busybox:1.36.1}"
OUT_DIR="${MACVZ_SOAK_OUT_DIR:-}"
ORPHAN_WAIT="${MACVZ_SOAK_ORPHAN_WAIT:-720}"
REQUIRE_RESTARTS="${MACVZ_SOAK_REQUIRE_RESTARTS:-0}"
RESULTS_FILE=""
SUMMARY_WRITTEN=0

NODES=()
FAILURES=0
ITERATION=0
START_EPOCH=0
START_TIME=""
END_EPOCH=0

c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
pass() { printf "${c_green}PASS${c_off} %s\n" "$*"; }
skip() { printf "${c_yellow}SKIP${c_off} %s\n" "$*"; }
fail() { printf "${c_red}FAIL${c_off} %s\n" "$*"; FAILURES=$((FAILURES+1)); }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

k() { "$KUBECTL" "$@"; }

on_exit() {
	local rc=$?
	if [ -n "$RESULTS_FILE" ] && [ "$SUMMARY_WRITTEN" = 0 ]; then
		write_run_summary || true
	fi
	return "$rc"
}
trap on_exit EXIT

record_result() {
	local phase="$1" status="$2" evidence="$3"
	[ -n "$RESULTS_FILE" ] || return 0
	printf "%s\t%s\t%s\t%s\n" "$ITERATION" "$phase" "$status" "$evidence" >>"$RESULTS_FILE"
}

usage() {
	cat <<EOF
Usage: $0 [--duration 2h] [--iterations N] [--fixtures e2e,examples]

Environment:
  KUBECONFIG                        cluster credentials
  MACVZ_SOAK_NODES                  comma-separated MacVz node names
  MACVZ_SOAK_FIXTURES               e2e, examples, or e2e,examples (default: e2e)
  MACVZ_SOAK_NAMESPACE_PREFIX       prefix for per-iteration namespaces (default: macvz-soak)
  MACVZ_SOAK_DURATION               wall-clock budget: 30m, 2h, 1d (default: 2h)
  MACVZ_SOAK_ITERATIONS             fixed loop count; 0 means until duration
  MACVZ_SOAK_OUT_DIR                output directory (default: mktemp)
  MACVZ_SOAK_RESTART_KUBELET_CMD    command template; {node} is replaced
  MACVZ_SOAK_RESTART_HELPER_CMD     command template; {node} is replaced
  MACVZ_SOAK_STOP_KUBELET_CMD       command template for orphan-after-downtime
  MACVZ_SOAK_START_KUBELET_CMD      command template for orphan-after-downtime
  MACVZ_SOAK_CLEANUP_CMD            command template; should run cleanup --dry-run
  MACVZ_SOAK_NODE_CMD               command template; appends a shell command
  MACVZ_SOAK_REQUIRE_RESTARTS=1     fail preflight unless restart hooks are set

Examples:
  MACVZ_SOAK_NODES=macvz-a,macvz-b $0 --duration 8h
  MACVZ_SOAK_RESTART_KUBELET_CMD='ssh admin@{node} sudo launchctl kickstart -k gui/501/com.github.chimerakang.macvz-kubelet' \\
    MACVZ_SOAK_RESTART_HELPER_CMD='ssh admin@{node} sudo launchctl kickstart -k system/com.github.chimerakang.macvz-netd' \\
    $0 --iterations 4 --fixtures e2e,examples
EOF
}

parse_args() {
	while [ "$#" -gt 0 ]; do
		case "$1" in
			--duration) DURATION="${2:-}"; shift 2 ;;
			--iterations) ITERATIONS="${2:-}"; shift 2 ;;
			--fixtures) FIXTURES="${2:-}"; shift 2 ;;
			--help|-h) usage; exit 0 ;;
			*) die "unknown argument: $1" ;;
		esac
	done
}

duration_seconds() {
	local v="$1" n unit
	case "$v" in
		*[!0-9smhd]*) return 1 ;;
	esac
	n="${v%[smhd]}"
	unit="${v#"$n"}"
	[ -n "$n" ] || return 1
	case "$unit" in
		""|s) printf '%s\n' "$n" ;;
		m) printf '%s\n' "$((n * 60))" ;;
		h) printf '%s\n' "$((n * 3600))" ;;
		d) printf '%s\n' "$((n * 86400))" ;;
		*) return 1 ;;
	esac
}

template_cmd() {
	local template="$1" node="$2"
	printf '%s\n' "${template//\{node\}/$node}"
}

run_template() {
	local label="$1" template="$2" node="$3" out="$4" cmd
	cmd="$(template_cmd "$template" "$node")"
	log "$label on $node: $cmd"
	{
		echo "### $label on $node"
		echo "$cmd"
		bash -c "$cmd"
	} >>"$out" 2>&1
}

node_shell() {
	local node="$1" remote="$2" out="$3" base
	[ -n "${MACVZ_SOAK_NODE_CMD:-}" ] || return 2
	base="$(template_cmd "$MACVZ_SOAK_NODE_CMD" "$node")"
	{
		echo "### node command on $node: $remote"
		bash -c "$base $(printf '%q' "$remote")"
	} >>"$out" 2>&1
}

preflight() {
	log "Preflight: cluster and MacVz nodes"
	command -v "$KUBECTL" >/dev/null 2>&1 || die "kubectl not found in PATH"
	k get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1 || die "cannot reach Kubernetes API"

	if [ -n "${MACVZ_SOAK_NODES:-}" ]; then
		local IFS=','
		# shellcheck disable=SC2206
		NODES=(${MACVZ_SOAK_NODES})
	else
		local detected
		detected="$(k get nodes -l type=virtual-kubelet -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)"
		while IFS= read -r n; do
			[ -n "$n" ] && NODES=("${NODES[@]}" "$n")
		done <<EOF
$detected
EOF
	fi
	[ "${#NODES[@]}" -ge 1 ] || die "no MacVz nodes found; set MACVZ_SOAK_NODES"

	if [ "$REQUIRE_RESTARTS" = 1 ] &&
		{ [ -z "${MACVZ_SOAK_RESTART_KUBELET_CMD:-}" ] || [ -z "${MACVZ_SOAK_RESTART_HELPER_CMD:-}" ]; }; then
		die "restart hooks required but MACVZ_SOAK_RESTART_KUBELET_CMD or MACVZ_SOAK_RESTART_HELPER_CMD is unset"
	fi

	if [ -z "$OUT_DIR" ]; then
		OUT_DIR="$(mktemp -d -t macvz-soak)"
	fi
	mkdir -p "$OUT_DIR"
	RESULTS_FILE="$OUT_DIR/results.tsv"
	printf "iteration\tphase\tstatus\tevidence\n" >"$RESULTS_FILE"
	log "Nodes: ${NODES[*]}"
	log "Output: $OUT_DIR"
}

validate_settings() {
	[ -n "$FIXTURES" ] || die "MACVZ_SOAK_FIXTURES must not be empty"
	case "$NAMESPACE_PREFIX" in
		''|*[!a-z0-9-]*|-*|*-) die "MACVZ_SOAK_NAMESPACE_PREFIX must contain only lowercase letters, digits, and hyphens, and must not start/end with a hyphen" ;;
	esac
	if [ "${#NAMESPACE_PREFIX}" -gt 40 ]; then
		die "MACVZ_SOAK_NAMESPACE_PREFIX must be 40 characters or shorter"
	fi
	case "$ITERATIONS" in
		''|*[!0-9]*) die "MACVZ_SOAK_ITERATIONS must be a non-negative integer" ;;
	esac
	case "$TIMEOUT" in
		''|*[!0-9]*) die "MACVZ_SOAK_TIMEOUT must be a positive integer" ;;
		0) die "MACVZ_SOAK_TIMEOUT must be greater than zero" ;;
	esac
	case "$SLEEP" in
		''|*[!0-9]*) die "MACVZ_SOAK_SLEEP must be a non-negative integer" ;;
	esac
	case "$ORPHAN_WAIT" in
		''|*[!0-9]*) die "MACVZ_SOAK_ORPHAN_WAIT must be a non-negative integer" ;;
	esac
	local IFS=',' fixture
	for fixture in $FIXTURES; do
		case "$fixture" in
			e2e|examples) ;;
			*) die "unknown MACVZ_SOAK_FIXTURES entry: $fixture (use e2e, examples, or e2e,examples)" ;;
		esac
	done
}

wait_nodes_ready() {
	local n ok=1
	for n in "${NODES[@]}"; do
		if ! k wait --for=condition=Ready "node/$n" --timeout="${TIMEOUT}s" >/dev/null 2>&1; then
			fail "node $n did not become Ready"
			ok=0
		fi
	done
	[ "$ok" = 1 ]
}

sample_cluster() {
	local dir="$1" n
	mkdir -p "$dir"
	{
		echo "### timestamp"; date -u '+%Y-%m-%dT%H:%M:%SZ'
		echo "### nodes"; k get nodes -o wide
		echo "### macvz node resources"
		for n in "${NODES[@]}"; do
			echo "## $n"
			k get node "$n" -o jsonpath='capacity.cpu={.status.capacity.cpu} capacity.memory={.status.capacity.memory} capacity.ephemeral-storage={.status.capacity.ephemeral-storage} allocatable.cpu={.status.allocatable.cpu} allocatable.memory={.status.allocatable.memory} allocatable.ephemeral-storage={.status.allocatable.ephemeral-storage} podCIDR={.spec.podCIDR}{"\n"}' 2>/dev/null
		done
		echo "### pods on MacVz nodes"; k get pods -A -o wide --field-selector=status.phase!=Succeeded,status.phase!=Failed 2>/dev/null | grep -E "(${NODES[*]// /|})" || true
		echo "### events"; k get events -A --sort-by=.lastTimestamp 2>/dev/null | tail -100
	} >"$dir/cluster.txt" 2>&1

	for n in "${NODES[@]}"; do
		k get --raw "/api/v1/nodes/${n}/proxy/stats/summary" >"$dir/stats-$n.json" 2>"$dir/stats-$n.err" || true
		# shellcheck disable=SC2016 # expand HOME on the target node, not here.
		node_shell "$n" 'container list --all; df -k "${HOME:-/tmp}"' "$dir/node-$n.txt" || true
	done
}

run_e2e_fixture() {
	local ns="$NAMESPACE_PREFIX-e2e-$ITERATION" dir="$1/e2e"
	mkdir -p "$dir"
	log "Iteration $ITERATION: running multi-node e2e"
	MACVZ_E2E_NAMESPACE="$ns" \
	MACVZ_E2E_NODES="$(IFS=,; echo "${NODES[*]}")" \
	MACVZ_E2E_TIMEOUT="$TIMEOUT" \
	MACVZ_E2E_IMAGE="$IMAGE" \
	MACVZ_E2E_DIAG_DIR="$dir/diag" \
	MACVZ_E2E_TEARDOWN_WAIT=1 \
		"$ROOT/test/e2e/e2e.sh" >"$dir/output.log" 2>&1
	local rc=$?
	if [ "$rc" = 0 ]; then
		pass "iteration $ITERATION e2e passed"
	else
		fail "iteration $ITERATION e2e failed (see $dir/output.log)"
	fi
	return "$rc"
}

run_examples_fixture() {
	local dir="$1/examples"
	mkdir -p "$dir"
	log "Iteration $ITERATION: running P8 example catalog"
	MACVZ_E2E_TIMEOUT="$TIMEOUT" \
	MACVZ_E2E_CONTINUE=1 \
	MACVZ_HELLO_NAMESPACE="$NAMESPACE_PREFIX-hello-$ITERATION" \
	MACVZ_P6_NAMESPACE="$NAMESPACE_PREFIX-p6-$ITERATION" \
	MACVZ_HEADLAMP_NAMESPACE="$NAMESPACE_PREFIX-headlamp-$ITERATION" \
	MACVZ_GB_NAMESPACE="$NAMESPACE_PREFIX-gb-$ITERATION" \
		"$ROOT/test/examples/run-all.sh" >"$dir/output.log" 2>&1
	local rc=$?
	if [ "$rc" = 0 ]; then
		pass "iteration $ITERATION examples passed"
	else
		fail "iteration $ITERATION examples failed (see $dir/output.log)"
	fi
	return "$rc"
}

restart_phase() {
	local dir="$1/restarts" n
	mkdir -p "$dir"
	if [ -z "${MACVZ_SOAK_RESTART_KUBELET_CMD:-}" ] && [ -z "${MACVZ_SOAK_RESTART_HELPER_CMD:-}" ]; then
		skip "iteration $ITERATION restarts skipped; set MACVZ_SOAK_RESTART_* hooks"
		return 0
	fi
	for n in "${NODES[@]}"; do
		if [ -n "${MACVZ_SOAK_RESTART_HELPER_CMD:-}" ] &&
			! run_template "restart helper" "$MACVZ_SOAK_RESTART_HELPER_CMD" "$n" "$dir/$n.log"; then
			fail "helper restart hook failed on $n"
			return 1
		fi
		if [ -n "${MACVZ_SOAK_RESTART_KUBELET_CMD:-}" ] &&
			! run_template "restart kubelet" "$MACVZ_SOAK_RESTART_KUBELET_CMD" "$n" "$dir/$n.log"; then
			fail "kubelet restart hook failed on $n"
			return 1
		fi
	done
	wait_nodes_ready || return 1
	pass "iteration $ITERATION restarted configured services and nodes are Ready"
}

recovery_phase() {
	local node="${NODES[0]}" ns="$NAMESPACE_PREFIX-recovery-$ITERATION" pod="recovery" dir="$1/recovery"
	mkdir -p "$dir"
	if [ -z "${MACVZ_SOAK_RESTART_KUBELET_CMD:-}" ]; then
		skip "iteration $ITERATION restart recovery skipped; set MACVZ_SOAK_RESTART_KUBELET_CMD"
		return 0
	fi

	log "Iteration $ITERATION: restart recovery smoke on $node"
	k create namespace "$ns" >/dev/null 2>&1 || true
	k apply -f - >"$dir/apply.log" 2>&1 <<EOF
apiVersion: v1
kind: Pod
metadata: { name: $pod, namespace: $ns }
spec:
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: $node }
  tolerations:
    - key: virtual-kubelet.io/provider
      operator: Exists
      effect: NoSchedule
  containers:
    - name: c
      image: $IMAGE
      command: ["sh", "-c", "echo recovery-ready; sleep 86400"]
      resources:
        requests: { cpu: "100m", memory: 256Mi }
        limits:   { cpu: "250m", memory: 256Mi }
EOF
	if ! k -n "$ns" wait --for=condition=Ready "pod/$pod" --timeout="${TIMEOUT}s" >"$dir/wait-before.log" 2>&1; then
		fail "restart recovery pod did not become Ready before restart"
		k delete namespace "$ns" --wait=false >/dev/null 2>&1 || true
		return 1
	fi
	local before after
	before="$(k -n "$ns" get pod "$pod" -o jsonpath='{.status.podIP}' 2>/dev/null)"
	if ! run_template "restart kubelet" "$MACVZ_SOAK_RESTART_KUBELET_CMD" "$node" "$dir/restart.log"; then
		fail "restart recovery kubelet restart hook failed on $node"
		k delete namespace "$ns" --wait=false >/dev/null 2>&1 || true
		return 1
	fi
	wait_nodes_ready >/dev/null 2>&1 || true
	if ! k -n "$ns" wait --for=condition=Ready "pod/$pod" --timeout="${TIMEOUT}s" >"$dir/wait-after.log" 2>&1; then
		fail "restart recovery pod did not return Ready after kubelet restart"
		k delete namespace "$ns" --wait=false >/dev/null 2>&1 || true
		return 1
	fi
	after="$(k -n "$ns" get pod "$pod" -o jsonpath='{.status.podIP}' 2>/dev/null)"
	k -n "$ns" logs "$pod" >"$dir/logs.txt" 2>&1 || true
	k delete namespace "$ns" --wait=true --timeout="${TIMEOUT}s" >/dev/null 2>&1 ||
		k delete namespace "$ns" --wait=false >/dev/null 2>&1 || true
	if [ -n "$before" ] && [ "$before" = "$after" ]; then
		pass "iteration $ITERATION restart recovery kept PodIP $after"
	else
		fail "restart recovery PodIP changed before=$before after=$after"
		return 1
	fi
}

orphan_phase() {
	local node="${NODES[0]}" ns="$NAMESPACE_PREFIX-orphan-$ITERATION" pod="orphan" dir="$1/orphan" workload
	mkdir -p "$dir"
	if [ -z "${MACVZ_SOAK_STOP_KUBELET_CMD:-}" ] || [ -z "${MACVZ_SOAK_START_KUBELET_CMD:-}" ] || [ -z "${MACVZ_SOAK_NODE_CMD:-}" ]; then
		skip "iteration $ITERATION orphan-after-downtime skipped; set STOP/START/NODE hooks"
		return 0
	fi

	log "Iteration $ITERATION: orphan cleanup smoke on $node"
	workload="macvz-${ns}-${pod}-c"
	k create namespace "$ns" >/dev/null 2>&1 || true
	k apply -f - >"$dir/apply.log" 2>&1 <<EOF
apiVersion: v1
kind: Pod
metadata: { name: $pod, namespace: $ns }
spec:
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: $node }
  tolerations:
    - key: virtual-kubelet.io/provider
      operator: Exists
      effect: NoSchedule
  containers:
    - name: c
      image: $IMAGE
      command: ["sh", "-c", "sleep 86400"]
      resources:
        requests: { cpu: "100m", memory: 256Mi }
        limits:   { cpu: "250m", memory: 256Mi }
EOF
	if ! k -n "$ns" wait --for=condition=Ready "pod/$pod" --timeout="${TIMEOUT}s" >"$dir/wait-before.log" 2>&1; then
		fail "orphan test pod did not become Ready"
		k delete namespace "$ns" --wait=false >/dev/null 2>&1 || true
		return 1
	fi
	if ! run_template "stop kubelet" "$MACVZ_SOAK_STOP_KUBELET_CMD" "$node" "$dir/control.log"; then
		fail "orphan test failed to stop kubelet on $node"
		k delete namespace "$ns" --wait=false >/dev/null 2>&1 || true
		return 1
	fi
	k delete namespace "$ns" --wait=false >"$dir/delete-while-down.log" 2>&1 || true
	if ! run_template "start kubelet" "$MACVZ_SOAK_START_KUBELET_CMD" "$node" "$dir/control.log"; then
		fail "orphan test failed to start kubelet on $node"
		return 1
	fi
	wait_nodes_ready >/dev/null 2>&1 || true
	log "Waiting ${ORPHAN_WAIT}s for orphan grace/reap window"
	sleep "$ORPHAN_WAIT"
	node_shell "$node" "if container list --all | grep -F '$workload'; then exit 10; fi" "$dir/node-check.log"
	local rc=$?
	if [ "$rc" = 10 ]; then
		fail "orphan workload still present after grace window: $workload"
		return 1
	fi
	if [ "$rc" != 0 ]; then
		fail "could not verify orphan workload absence on $node (exit $rc)"
		return 1
	fi
	pass "iteration $ITERATION orphan workload reaped or absent: $workload"
}

cleanup_phase() {
	local dir="$1/cleanup" n ok=1
	mkdir -p "$dir"
	if [ -z "${MACVZ_SOAK_CLEANUP_CMD:-}" ]; then
		skip "iteration $ITERATION cleanup dry-run skipped; set MACVZ_SOAK_CLEANUP_CMD"
		return 0
	fi
	for n in "${NODES[@]}"; do
		if ! run_template "cleanup dry-run" "$MACVZ_SOAK_CLEANUP_CMD" "$n" "$dir/$n.log"; then
			fail "cleanup dry-run hook failed on $n"
			ok=0
			continue
		fi
		if grep -Eq 'Would reap [1-9][0-9]* orphan' "$dir/$n.log"; then
			fail "cleanup dry-run found orphan VMs on $n"
			ok=0
		fi
	done
	[ "$ok" = 1 ]
}

churn_phase() {
	local dir="$1/churn" n="${NODES[$((ITERATION % ${#NODES[@]}))]}"
	mkdir -p "$dir"
	log "Iteration $ITERATION: cordon/uncordon churn on $n"
	{
		k cordon "$n"
		k get node "$n" -o wide
		k uncordon "$n"
		k wait --for=condition=Ready "node/$n" --timeout="${TIMEOUT}s"
	} >"$dir/$n.log" 2>&1
	local rc=$?
	if [ "$rc" = 0 ]; then
		pass "iteration $ITERATION node churn completed on $n"
	else
		fail "iteration $ITERATION node churn failed on $n"
	fi
	return "$rc"
}

run_iteration() {
	ITERATION=$((ITERATION + 1))
	local dir="$OUT_DIR/iteration-$ITERATION"
	mkdir -p "$dir"
	log "Starting soak iteration $ITERATION"
	sample_cluster "$dir/before"

	case ",$FIXTURES," in
		*,e2e,*)
			if run_e2e_fixture "$dir"; then
				record_result "e2e" "PASS" "$dir/e2e/output.log"
			else
				record_result "e2e" "FAIL" "$dir/e2e/output.log"
			fi
			;;
		*) record_result "e2e" "SKIP" "fixture disabled" ;;
	esac
	case ",$FIXTURES," in
		*,examples,*)
			if run_examples_fixture "$dir"; then
				record_result "examples" "PASS" "$dir/examples/output.log"
			else
				record_result "examples" "FAIL" "$dir/examples/output.log"
			fi
			;;
		*) record_result "examples" "SKIP" "fixture disabled" ;;
	esac

	if [ -n "${MACVZ_SOAK_RESTART_KUBELET_CMD:-}" ]; then
		if recovery_phase "$dir"; then
			record_result "restart-recovery" "PASS" "$dir/recovery"
		else
			record_result "restart-recovery" "FAIL" "$dir/recovery"
		fi
	else
		skip "iteration $ITERATION restart recovery skipped; set MACVZ_SOAK_RESTART_KUBELET_CMD"
		record_result "restart-recovery" "SKIP" "MACVZ_SOAK_RESTART_KUBELET_CMD unset"
	fi
	if [ -n "${MACVZ_SOAK_RESTART_KUBELET_CMD:-}" ] || [ -n "${MACVZ_SOAK_RESTART_HELPER_CMD:-}" ]; then
		if restart_phase "$dir"; then
			record_result "service-restarts" "PASS" "$dir/restarts"
		else
			record_result "service-restarts" "FAIL" "$dir/restarts"
		fi
	else
		skip "iteration $ITERATION restarts skipped; set MACVZ_SOAK_RESTART_* hooks"
		record_result "service-restarts" "SKIP" "restart hooks unset"
	fi
	if churn_phase "$dir"; then
		record_result "node-churn" "PASS" "$dir/churn"
	else
		record_result "node-churn" "FAIL" "$dir/churn"
	fi
	if [ -n "${MACVZ_SOAK_STOP_KUBELET_CMD:-}" ] && [ -n "${MACVZ_SOAK_START_KUBELET_CMD:-}" ] && [ -n "${MACVZ_SOAK_NODE_CMD:-}" ]; then
		if orphan_phase "$dir"; then
			record_result "orphan-cleanup" "PASS" "$dir/orphan"
		else
			record_result "orphan-cleanup" "FAIL" "$dir/orphan"
		fi
	else
		skip "iteration $ITERATION orphan-after-downtime skipped; set STOP/START/NODE hooks"
		record_result "orphan-cleanup" "SKIP" "STOP/START/NODE hooks unset"
	fi
	if [ -n "${MACVZ_SOAK_CLEANUP_CMD:-}" ]; then
		if cleanup_phase "$dir"; then
			record_result "cleanup-dry-run" "PASS" "$dir/cleanup"
		else
			record_result "cleanup-dry-run" "FAIL" "$dir/cleanup"
		fi
	else
		skip "iteration $ITERATION cleanup dry-run skipped; set MACVZ_SOAK_CLEANUP_CMD"
		record_result "cleanup-dry-run" "SKIP" "MACVZ_SOAK_CLEANUP_CMD unset"
	fi

	sample_cluster "$dir/after"
	log "Finished soak iteration $ITERATION"
}

write_run_summary() {
	local summary="$OUT_DIR/summary.txt"
	[ -n "$OUT_DIR" ] || return 0
	mkdir -p "$OUT_DIR"
	{
		echo "MacVz soak summary"
		echo "started: $START_TIME"
		echo "finished: $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
		echo "nodes: ${NODES[*]}"
		echo "fixtures: $FIXTURES"
		echo "iterations: $ITERATION"
		echo "failures: $FAILURES"
		echo "results: $RESULTS_FILE"
		echo
		if [ -r "$RESULTS_FILE" ]; then
			cat "$RESULTS_FILE"
		fi
	} >"$summary"
	SUMMARY_WRITTEN=1
	log "Run summary written: $summary"
}

should_continue() {
	if [ "$ITERATIONS" -gt 0 ]; then
		[ "$ITERATION" -lt "$ITERATIONS" ]
	else
		[ "$(date +%s)" -lt "$END_EPOCH" ]
	fi
}

main() {
	parse_args "$@"
	local seconds
	seconds="$(duration_seconds "$DURATION")" || die "invalid duration: $DURATION"
	validate_settings
	if [ "$ITERATIONS" -eq 0 ] && [ "$seconds" -eq 0 ]; then
		die "soak would run zero iterations; set --iterations >0 or --duration >0"
	fi
	START_EPOCH="$(date +%s)"
	START_TIME="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
	END_EPOCH="$((START_EPOCH + seconds))"

	preflight
	wait_nodes_ready || true
	while should_continue; do
		run_iteration
		should_continue || break
		sleep "$SLEEP"
	done
	write_run_summary

	echo
	if [ "$FAILURES" -eq 0 ]; then
		pass "MacVz soak: $ITERATION iteration(s) passed; output in $OUT_DIR"
		return 0
	fi
	fail "MacVz soak: $FAILURES failure(s) across $ITERATION iteration(s); output in $OUT_DIR"
	return 1
}

main "$@"
