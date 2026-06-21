#!/usr/bin/env bash
#
# soak.sh — CRI-P8 bounded soak for the experimental MacVz CRI adapter (issue #80).
#
# It repeatedly creates and deletes a single-container Pod through the CRI socket
# (crictl), sampling the adapter's resident memory and the live sandbox/container
# counts each iteration, to surface leaks (growing RSS, orphaned workloads, or
# stale state) across many create/delete cycles. Failure evidence and a per-run
# CSV of resource samples are written to the output dir.
#
# Like run.sh this mutates host state, so it runs only when MACVZ_INTEGRATION=1;
# otherwise it prints its plan and exits 0.
#
# Environment:
#   MACVZ_INTEGRATION       set to 1 to run live (otherwise plan-only).
#   MACVZ_CRI_BIN           macvz-cri binary (default: ./bin/macvz-cri).
#   MACVZ_CRI_SOCKET        CRI socket (default: per-run temp socket).
#   MACVZ_CRI_STATE_DIR     adapter state dir (default: per-run temp dir).
#   MACVZ_CRI_IMAGE         arm64 image (default: busybox:1.36.1).
#   MACVZ_SOAK_ITERATIONS   create/delete cycles to run (default: 50).
#   MACVZ_SOAK_RSS_GROWTH_KB allowed adapter RSS growth before flagging a leak
#                           (default: 65536 = 64 MiB).
#   MACVZ_CRI_OUT_DIR       results dir (default: a mktemp dir).
#   CRICTL                  crictl binary (default: crictl).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"

INTEGRATION="${MACVZ_INTEGRATION:-0}"
CRI_BIN="${MACVZ_CRI_BIN:-$ROOT/bin/macvz-cri}"
SOCKET="${MACVZ_CRI_SOCKET:-}"
STATE_DIR="${MACVZ_CRI_STATE_DIR:-}"
IMAGE="${MACVZ_CRI_IMAGE:-busybox:1.36.1}"
ITERATIONS="${MACVZ_SOAK_ITERATIONS:-50}"
RSS_GROWTH_KB="${MACVZ_SOAK_RSS_GROWTH_KB:-65536}"
OUT_DIR="${MACVZ_CRI_OUT_DIR:-}"
CRICTL="${CRICTL:-crictl}"
# See run.sh: crictl's 2s default RPC timeout is too short for a real micro-VM
# boot; use a kubelet-like runtime-request timeout to avoid spurious failures.
CRICTL_TIMEOUT="${CRICTL_TIMEOUT:-2m}"

ADAPTER_PID=""
TMP_ROOT=""
FAILURES=0
FIRST_RSS=0
LAST_RSS=0

c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
pass() { printf "${c_green}PASS${c_off} %s\n" "$*"; }
fail() { printf "${c_red}FAIL${c_off} %s\n" "$*"; FAILURES=$((FAILURES+1)); }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

crt() { "$CRICTL" --runtime-endpoint "unix://$SOCKET" --timeout "$CRICTL_TIMEOUT" "$@"; }

print_plan() {
	cat <<PLAN
CRI-P8 soak (plan; set MACVZ_INTEGRATION=1 to run live):

  iterations   ${ITERATIONS} create/delete cycles of a single-container Pod
  image        pull ${IMAGE} once through the CRI ImageService before cycling
  sample       per iteration: adapter RSS (KB) + live sandbox/container counts -> samples.csv
  leak guard   flag if adapter RSS grows by more than ${RSS_GROWTH_KB} KB end-to-end
  orphan guard flag if any sandbox/container remains after the final cycle

Gated because it boots micro-VMs repeatedly and writes the CRI socket.
PLAN
}

setup() {
	command -v "$CRICTL" >/dev/null 2>&1 || die "crictl not found (set CRICTL)"
	[ -x "$CRI_BIN" ] || die "macvz-cri not found at $CRI_BIN (run 'make cri')"
	TMP_ROOT="$(mktemp -d -t macvz-cri-soak)"
	[ -n "$SOCKET" ] || SOCKET="$TMP_ROOT/macvz-cri.sock"
	[ -n "$STATE_DIR" ] || STATE_DIR="$TMP_ROOT/state"
	[ -n "$OUT_DIR" ] || OUT_DIR="$TMP_ROOT/out"
	mkdir -p "$STATE_DIR" "$OUT_DIR"
	printf 'iteration,rss_kb,sandboxes,containers\n' >"$OUT_DIR/samples.csv"
	log "socket=$SOCKET state=$STATE_DIR out=$OUT_DIR iterations=$ITERATIONS"
}

start_adapter() {
	"$CRI_BIN" --listen "unix://$SOCKET" --state-dir "$STATE_DIR" ${MACVZ_CRI_EXTRA:-} \
		>"$OUT_DIR/adapter.log" 2>&1 &
	ADAPTER_PID="$!"
	for _ in $(seq 1 50); do
		crt version >/dev/null 2>&1 && return 0
		sleep 0.2
	done
	die "adapter did not start (see $OUT_DIR/adapter.log)"
}

cleanup() {
	[ -n "$ADAPTER_PID" ] && { kill "$ADAPTER_PID" 2>/dev/null || true; wait "$ADAPTER_PID" 2>/dev/null || true; }
	[ -n "$TMP_ROOT" ] && [ "${MACVZ_CRI_KEEP:-0}" != 1 ] && rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

adapter_rss_kb() {
	[ -n "$ADAPTER_PID" ] || { echo 0; return; }
	ps -o rss= -p "$ADAPTER_PID" 2>/dev/null | tr -d ' ' || echo 0
}

write_fixtures() {
	cat >"$OUT_DIR/sandbox.json" <<EOF
{"metadata": {"name": "soak", "namespace": "macvz-cri-soak", "uid": "soak-uid", "attempt": 0}, "linux": {}}
EOF
	cat >"$OUT_DIR/container.json" <<EOF
{"metadata": {"name": "app", "attempt": 0}, "image": {"image": "$IMAGE"}, "command": ["sh", "-c", "sleep 1"], "log_path": "app.log"}
EOF
}

run_cycle() {
	local i="$1" sb cid
	sb="$(crt runp "$OUT_DIR/sandbox.json" 2>>"$OUT_DIR/soak.log")" || { fail "iter $i: runp"; return 1; }
	cid="$(crt create "$sb" "$OUT_DIR/container.json" "$OUT_DIR/sandbox.json" 2>>"$OUT_DIR/soak.log")" || { fail "iter $i: create"; crt rmp "$sb" >/dev/null 2>&1; return 1; }
	crt start "$cid" >/dev/null 2>>"$OUT_DIR/soak.log" || fail "iter $i: start"
	crt stop "$cid" >/dev/null 2>&1 || true
	crt rm "$cid" >/dev/null 2>&1 || true
	crt stopp "$sb" >/dev/null 2>&1 || true
	crt rmp "$sb" >/dev/null 2>&1 || true
}

pull_image() {
	log "Pulling soak image ($IMAGE)"
	if crt pull "$IMAGE" >"$OUT_DIR/pull.txt" 2>&1; then
		pass "image pulled"
	else
		fail "image pull failed (see $OUT_DIR/pull.txt)"
		return 1
	fi
}

sample() {
	local i="$1" rss sandboxes containers
	rss="$(adapter_rss_kb)"
	# wc -l, not `grep -c . || echo 0` (which double-counts on an empty list and
	# writes a multiline value into the CSV — see the orphan guard below).
	sandboxes="$(crt pods -q 2>/dev/null | wc -l | tr -d ' ')"
	containers="$(crt ps -a -q 2>/dev/null | wc -l | tr -d ' ')"
	printf '%s,%s,%s,%s\n' "$i" "$rss" "$sandboxes" "$containers" >>"$OUT_DIR/samples.csv"
	[ "$FIRST_RSS" = 0 ] && FIRST_RSS="$rss"
	LAST_RSS="$rss"
}

main_live() {
	setup
	start_adapter
	write_fixtures
	pull_image || { fail "cannot run soak without a pulled image"; exit 1; }

	local i
	for i in $(seq 1 "$ITERATIONS"); do
		run_cycle "$i" || true
		sample "$i"
		[ $((i % 10)) -eq 0 ] && log "iteration $i/$ITERATIONS (rss=${LAST_RSS}KB)"
	done

	# Orphan guard: nothing should remain in the CRI view after the last cycle.
	# Count with `wc -l`, not `grep -c . || echo 0`: grep exits non-zero on an empty
	# (zero-pod) list, so the `|| echo 0` fires *in addition* to grep's own "0",
	# yielding a multiline "0\n0" that spuriously fails the [ = 0 ] comparison.
	local rem_c rem_p
	rem_c="$(crt ps -a -q 2>/dev/null | wc -l | tr -d ' ')"
	rem_p="$(crt pods -q 2>/dev/null | wc -l | tr -d ' ')"
	if [ "$rem_c" = 0 ] && [ "$rem_p" = 0 ]; then
		pass "no orphaned containers/sandboxes after $ITERATIONS cycles"
	else
		fail "orphans remain: $rem_c container(s), $rem_p sandbox(es)"
	fi

	# Leak guard: bounded adapter RSS growth.
	local growth=$((LAST_RSS - FIRST_RSS))
	log "adapter RSS: first=${FIRST_RSS}KB last=${LAST_RSS}KB growth=${growth}KB (limit ${RSS_GROWTH_KB}KB)"
	if [ "$growth" -le "$RSS_GROWTH_KB" ]; then
		pass "adapter RSS growth within bound"
	else
		fail "adapter RSS grew ${growth}KB (> ${RSS_GROWTH_KB}KB): possible leak"
	fi

	echo
	if [ "$FAILURES" -eq 0 ]; then
		pass "CRI-P8 soak passed; samples in $OUT_DIR/samples.csv"
		exit 0
	fi
	fail "CRI-P8 soak: $FAILURES failure(s); evidence in $OUT_DIR"
	exit 1
}

if [ "$INTEGRATION" != 1 ]; then
	print_plan
	exit 0
fi
main_live
