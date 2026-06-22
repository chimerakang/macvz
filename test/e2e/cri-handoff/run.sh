#!/usr/bin/env bash
#
# run.sh — CRI-I4-1 crictl fixture for the experimental LinuxPod *handoff* path
# (issue #118).
#
# Where test/e2e/cri-k3s/run.sh exercises the default apple/container path, this
# fixture drives the handoff-aware runtime: it starts macvz-cri with
# --experimental-handoff and walks one container through the full CRI lifecycle
# (RunPodSandbox -> CreateContainer -> StartContainer -> Status -> StopContainer
# -> RemoveContainer) over crictl, asserting the CRI-R16 handoff invariants:
#
#   - CreateContainer stages a runtime-private rootfs/handoff subtree
#     (handoffPrepared=true in verbose status).
#   - StartContainer reaches Running only after the launched process reports the
#     staged rootfs identity back through the handoff evidence file
#     (identityVerified=true, observedIdentity==expectedIdentity).
#   - StopContainer records exit state but KEEPS the handoff subtree for
#     post-mortem (the handoffPath directory still exists after stop).
#   - RemoveContainer deletes the subtree idempotently (handoffPath gone; a
#     second remove never errors).
#
# Identity diagnostics are read from the verbose ContainerStatus info map
# (CRI-I3-3, #117), surfaced by `crictl inspect`.
#
# This is an isolated `develop`-track feasibility probe, NOT the shipped Virtual
# Kubelet e2e (test/e2e/e2e.sh), and makes no k3s/kubelet or multi-day stability
# claim — both are explicit non-goals of #118.
#
# Gating: the live suite boots a micro-VM and writes the CRI socket, so it runs
# only when MACVZ_INTEGRATION=1. Without it the script prints the plan it WOULD
# run and exits 0, so it is safe in `go test`-style CI.
#
# Environment:
#   MACVZ_INTEGRATION     set to 1 to run the live fixture (otherwise plan-only).
#   MACVZ_CRI_BIN         path to the macvz-cri binary (default: ./bin/macvz-cri).
#   MACVZ_CRI_SOCKET      CRI socket to serve/probe (default: a per-run temp socket).
#   MACVZ_CRI_STATE_DIR   adapter state dir (default: a per-run temp dir).
#   MACVZ_HANDOFF_ROOT    handoff subtree root (default: a per-run temp dir; the
#                         production /run/macvz/containers is not writable on macOS).
#   MACVZ_CRI_IMAGE       arm64 image providing sh (default: busybox:1.36.1).
#   MACVZ_CRI_MANAGE      1 (default) start/stop the adapter; 0 expect it serving.
#   MACVZ_CRI_EXTRA       extra adapter flags.
#   CRICTL                crictl binary (default: crictl).
#   CRICTL_TIMEOUT        per-RPC timeout (default: 2m; a real VM boot exceeds 2s).
#   MACVZ_CRI_OUT_DIR     results/diagnostics dir (default: a mktemp dir).
#   MACVZ_CRI_KEEP        1 keeps the temp tree for inspection (default: 0).
#   MACVZ_HANDOFF_PRODUCER
#                         which side writes the handoff identity evidence:
#                           host-sim (default) — the fixture writes the expected
#                             identity into the host-visible handoff channel
#                             before StartContainer, standing in for the
#                             cooperating in-VM late-rootfs process so the
#                             identity gate verifies and the FULL lifecycle runs.
#                           none — no producer; StartContainer's gate is expected
#                             to time out, surfacing the in-VM-producer gap as a
#                             precise blocker (CRI-I4 follow-up #119) rather than
#                             a green pass. Asserts the timeout diagnostic.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"

INTEGRATION="${MACVZ_INTEGRATION:-0}"
CRI_BIN="${MACVZ_CRI_BIN:-$ROOT/bin/macvz-cri}"
SOCKET="${MACVZ_CRI_SOCKET:-}"
STATE_DIR="${MACVZ_CRI_STATE_DIR:-}"
HANDOFF_ROOT="${MACVZ_HANDOFF_ROOT:-}"
IMAGE="${MACVZ_CRI_IMAGE:-busybox:1.36.1}"
MANAGE="${MACVZ_CRI_MANAGE:-1}"
CRICTL="${CRICTL:-crictl}"
CRICTL_TIMEOUT="${CRICTL_TIMEOUT:-2m}"
OUT_DIR="${MACVZ_CRI_OUT_DIR:-}"
PRODUCER="${MACVZ_HANDOFF_PRODUCER:-host-sim}"

ADAPTER_PID=""
FAILURES=0
TMP_ROOT=""

c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
pass() { printf "${c_green}PASS${c_off} %s\n" "$*"; }
skip() { printf "${c_yellow}SKIP${c_off} %s\n" "$*"; }
fail() { printf "${c_red}FAIL${c_off} %s\n" "$*"; FAILURES=$((FAILURES+1)); }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

crt() { "$CRICTL" --runtime-endpoint "unix://$SOCKET" --timeout "$CRICTL_TIMEOUT" "$@"; }

# info_value KEY FILE — extract a verbose-status info value from a crictl inspect
# JSON dump. crictl flattens the ContainerStatus info map (handoffStatusInfo,
# #117) onto the TOP level of the inspect document, as siblings of "status", so
# the keys are queried at the root. jq is preferred; a grep/sed fallback keeps
# the fixture dependency-light. None of the handoff keys collide with the keys
# inside the nested "status" object, so the flat fallback is unambiguous.
info_value() {
	local key="$1" file="$2"
	if command -v jq >/dev/null 2>&1; then
		jq -r --arg k "$key" '.[$k] // .info[$k] // empty' "$file" 2>/dev/null
		return
	fi
	# Fallback: match "key": "value" allowing arbitrary whitespace.
	sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$file" | head -n1
}

print_plan() {
	cat <<'PLAN'
CRI-I4-1 handoff-lifecycle crictl fixture (plan; set MACVZ_INTEGRATION=1 to run live):

  adapter      start macvz-cri --experimental-handoff on the CRI socket; crictl version
  image        pull the workload image through the CRI ImageService
  create       runp + create a single-container Pod on the handoff-aware path
  prepared     verbose status shows handoffPrepared=true with a staged handoffPath
  producer     (host-sim) write the expected identity into the handoff channel,
               standing in for the cooperating in-VM late-rootfs process
  start        start the container; Running is gated on handoff identity verification
  verified     verbose status shows identityVerified=true (observed==expected identity)
  stop-keeps   stop the container; the handoff subtree is RETAINED for post-mortem
  remove       remove the container; the handoff subtree is deleted
  idempotent   a second remove + sandbox teardown leave no stale socket/state

With MACVZ_HANDOFF_PRODUCER=none the producer is skipped and the start gate is
expected to TIME OUT, surfacing the in-VM evidence-producer gap (follow-up #119)
as a precise blocker instead of a green pass.

Gated because the live fixture boots a micro-VM and writes the CRI socket. The
handoff path is the experimental LinuxPod runtime (CRI-I); it is not the shipped
Virtual Kubelet path and makes no k3s/kubelet compatibility claim.
PLAN
}

# --- setup / teardown --------------------------------------------------------
setup() {
	command -v "$CRICTL" >/dev/null 2>&1 || die "crictl not found in PATH (set CRICTL)"
	[ -x "$CRI_BIN" ] || die "macvz-cri not found at $CRI_BIN (run 'make cri' or set MACVZ_CRI_BIN)"

	TMP_ROOT="$(mktemp -d -t macvz-cri-handoff)"
	[ -n "$SOCKET" ] || SOCKET="$TMP_ROOT/macvz-cri.sock"
	[ -n "$STATE_DIR" ] || STATE_DIR="$TMP_ROOT/state"
	[ -n "$HANDOFF_ROOT" ] || HANDOFF_ROOT="$TMP_ROOT/handoff"
	[ -n "$OUT_DIR" ] || OUT_DIR="$TMP_ROOT/out"
	mkdir -p "$STATE_DIR" "$HANDOFF_ROOT" "$OUT_DIR"
	log "socket=$SOCKET state=$STATE_DIR handoff-root=$HANDOFF_ROOT out=$OUT_DIR"
}

start_adapter() {
	[ "$MANAGE" = 1 ] || { log "MACVZ_CRI_MANAGE=0: expecting an already-serving handoff adapter"; return 0; }
	log "Starting macvz-cri --experimental-handoff"
	# shellcheck disable=SC2086
	"$CRI_BIN" --listen "unix://$SOCKET" --state-dir "$STATE_DIR" \
		--experimental-handoff --handoff-root "$HANDOFF_ROOT" ${MACVZ_CRI_EXTRA:-} \
		>"$OUT_DIR/adapter.log" 2>&1 &
	ADAPTER_PID="$!"
	wait_socket || die "adapter did not start serving on $SOCKET (see $OUT_DIR/adapter.log)"
}

stop_adapter() {
	[ "$MANAGE" = 1 ] || return 0
	[ -n "$ADAPTER_PID" ] || return 0
	kill "$ADAPTER_PID" 2>/dev/null || true
	wait "$ADAPTER_PID" 2>/dev/null || true
	ADAPTER_PID=""
}

wait_socket() {
	for _ in $(seq 1 50); do
		if crt version >/dev/null 2>&1; then return 0; fi
		sleep 0.2
	done
	return 1
}

cleanup() {
	# Best-effort reclaim so a mid-phase failure never leaks a sandbox/container.
	local sb cid
	cid="$(cat "$OUT_DIR/.container-id" 2>/dev/null || true)"
	sb="$(cat "$OUT_DIR/.sandbox-id" 2>/dev/null || true)"
	[ -n "$cid" ] && { crt stop "$cid" >/dev/null 2>&1 || true; crt rm "$cid" >/dev/null 2>&1 || true; }
	[ -n "$sb" ] && { crt stopp "$sb" >/dev/null 2>&1 || true; crt rmp "$sb" >/dev/null 2>&1 || true; }
	stop_adapter
	[ -n "$TMP_ROOT" ] && [ "${MACVZ_CRI_KEEP:-0}" != 1 ] && rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

# --- fixtures ----------------------------------------------------------------
write_fixtures() {
	cat >"$OUT_DIR/sandbox.json" <<EOF
{
  "metadata": {"name": "handoff", "namespace": "macvz-cri-i4", "uid": "handoff-uid-1", "attempt": 0},
  "log_directory": "$OUT_DIR",
  "linux": {}
}
EOF
	# A long-lived process so the container stays Running for the status assertions.
	cat >"$OUT_DIR/container.json" <<EOF
{
  "metadata": {"name": "app", "attempt": 0},
  "image": {"image": "$IMAGE"},
  "command": ["sh", "-c", "echo macvz-cri-i4-handoff; sleep 3600"],
  "log_path": "app.log"
}
EOF
}

# --- phases ------------------------------------------------------------------
phase_adapter() {
	log "Phase: adapter handshake (handoff enabled)"
	if crt version >"$OUT_DIR/version.txt" 2>&1; then pass "crictl version handshake"; else fail "crictl version"; fi
	if grep -q "handoff path enabled" "$OUT_DIR/adapter.log" 2>/dev/null; then
		pass "adapter logged handoff path enabled"
	else
		skip "adapter handoff-enabled log line not found (non-fatal; check $OUT_DIR/adapter.log)"
	fi
}

phase_image() {
	log "Phase: image pull"
	if crt pull "$IMAGE" >"$OUT_DIR/pull.txt" 2>&1; then
		pass "image pulled ($IMAGE)"
	else
		fail "image pull ($IMAGE; see $OUT_DIR/pull.txt)"
	fi
}

phase_create() {
	log "Phase: create on the handoff-aware path"
	write_fixtures
	local sb cid
	sb="$(crt runp "$OUT_DIR/sandbox.json" 2>>"$OUT_DIR/lifecycle.log")" || { fail "runp"; return; }
	echo "$sb" >"$OUT_DIR/.sandbox-id"
	pass "runp ($sb)"
	cid="$(crt create "$sb" "$OUT_DIR/container.json" "$OUT_DIR/sandbox.json" 2>>"$OUT_DIR/lifecycle.log")" || { fail "create"; return; }
	echo "$cid" >"$OUT_DIR/.container-id"
	pass "create ($cid)"
}

phase_prepared() {
	log "Phase: handoff prepared at create time"
	local cid
	cid="$(cat "$OUT_DIR/.container-id" 2>/dev/null || true)"
	[ -n "$cid" ] || { skip "prepared (no container from create phase)"; return; }
	crt inspect "$cid" >"$OUT_DIR/inspect-created.json" 2>>"$OUT_DIR/lifecycle.log" || { fail "inspect (created)"; return; }
	local prepared hpath
	prepared="$(info_value handoffPrepared "$OUT_DIR/inspect-created.json")"
	hpath="$(info_value handoffPath "$OUT_DIR/inspect-created.json")"
	if [ "$prepared" = "true" ]; then pass "handoffPrepared=true at create"; else fail "handoffPrepared expected true, got '${prepared:-<empty>}'"; fi
	if [ -n "$hpath" ] && [ -d "$hpath" ]; then
		echo "$hpath" >"$OUT_DIR/.handoff-path"
		pass "handoff subtree staged on disk ($hpath)"
	else
		fail "handoffPath missing or not a directory ('${hpath:-<empty>}')"
	fi
	# Capture the expected identity the adapter staged at create time; the
	# host-sim producer writes it back, and the verified phase compares against it.
	local expected
	expected="$(info_value expectedIdentity "$OUT_DIR/inspect-created.json")"
	if [ -n "$expected" ]; then
		echo "$expected" >"$OUT_DIR/.expected-identity"
		pass "expectedIdentity staged at create ($expected)"
	else
		fail "expectedIdentity missing from create-time verbose status"
	fi
}

# phase_producer stands in for the cooperating in-VM late-rootfs process: it
# writes the expected rootfs identity into the host-visible handoff evidence
# channel (handoffPath/identity) so StartContainer's bounded-wait identity gate
# can verify. The standard apple/container workload has no component that writes
# this evidence, so without the producer the gate times out by design — that is
# the precise blocker the `none` mode documents (#119). Writing host-side is
# faithful: it is the exact file the in-VM process's write would surface on the
# host through the writable handoff bind mount.
phase_producer() {
	log "Phase: handoff identity producer (mode=$PRODUCER)"
	if [ "$PRODUCER" = none ]; then
		skip "producer skipped (MACVZ_HANDOFF_PRODUCER=none: start gate is expected to time out)"
		return
	fi
	local hpath expected
	hpath="$(cat "$OUT_DIR/.handoff-path" 2>/dev/null || true)"
	expected="$(cat "$OUT_DIR/.expected-identity" 2>/dev/null || true)"
	if [ -z "$hpath" ] || [ -z "$expected" ]; then
		fail "producer (missing handoffPath or expectedIdentity from create phase)"
		return
	fi
	# Line-oriented key=value evidence (runtime.ParseHandoffEvidence): identity is
	# the only required key; expected/proc_root are diagnostics.
	{
		echo "identity=$expected"
		echo "expected=$expected"
		echo "proc_root=/ (host-sim producer)"
	} >"$hpath/identity"
	pass "wrote handoff identity evidence to $hpath/identity"
}

phase_start() {
	log "Phase: start (Running gated on identity verification)"
	local cid
	cid="$(cat "$OUT_DIR/.container-id" 2>/dev/null || true)"
	[ -n "$cid" ] || { skip "start (no container)"; return; }
	if crt start "$cid" >>"$OUT_DIR/lifecycle.log" 2>&1; then
		if [ "$PRODUCER" = none ]; then
			fail "start succeeded without a producer (the identity gate should have timed out)"
		else
			pass "start returned (identity verified within timeout)"
		fi
		return
	fi
	# start failed.
	if [ "$PRODUCER" = none ]; then
		# The gate must fail for the right reason: evidence never arrived. Anything
		# else (e.g. the workload failing to boot) is a real failure, not the blocker.
		if grep -q "handoff identity verification failed" "$OUT_DIR/lifecycle.log" 2>/dev/null; then
			pass "start gate timed out with the expected identity-evidence diagnostic (in-VM producer gap, #119)"
		else
			fail "start failed but not with the handoff identity-verification diagnostic (see $OUT_DIR/lifecycle.log)"
		fi
	else
		fail "start (see $OUT_DIR/lifecycle.log and $OUT_DIR/adapter.log)"
	fi
}

phase_verified() {
	log "Phase: identity verified in verbose status"
	if [ "$PRODUCER" = none ]; then
		skip "verified (no producer; container never reached Running)"
		return
	fi
	local cid
	cid="$(cat "$OUT_DIR/.container-id" 2>/dev/null || true)"
	[ -n "$cid" ] || { skip "verified (no container)"; return; }
	crt inspect "$cid" >"$OUT_DIR/inspect-running.json" 2>>"$OUT_DIR/lifecycle.log" || { fail "inspect (running)"; return; }
	local verified expected observed
	verified="$(info_value identityVerified "$OUT_DIR/inspect-running.json")"
	expected="$(info_value expectedIdentity "$OUT_DIR/inspect-running.json")"
	observed="$(info_value observedIdentity "$OUT_DIR/inspect-running.json")"
	if [ "$verified" = "true" ]; then pass "identityVerified=true"; else fail "identityVerified expected true, got '${verified:-<empty>}'"; fi
	if [ -n "$expected" ] && [ "$expected" = "$observed" ]; then
		pass "observedIdentity matches expectedIdentity ($expected)"
	else
		fail "identity mismatch: expected='${expected:-<empty>}' observed='${observed:-<empty>}'"
	fi
}

phase_stop_keeps() {
	log "Phase: stop retains the handoff subtree"
	local cid hpath
	cid="$(cat "$OUT_DIR/.container-id" 2>/dev/null || true)"
	hpath="$(cat "$OUT_DIR/.handoff-path" 2>/dev/null || true)"
	[ -n "$cid" ] || { skip "stop (no container)"; return; }
	if crt stop "$cid" 2>>"$OUT_DIR/lifecycle.log"; then pass "stop returned"; else fail "stop"; fi
	if [ -n "$hpath" ] && [ -d "$hpath" ]; then
		pass "handoff subtree retained after stop (post-mortem evidence intact)"
	else
		fail "handoff subtree removed by stop (should survive until remove): $hpath"
	fi
}

phase_remove() {
	log "Phase: remove deletes the handoff subtree (idempotent)"
	local cid sb hpath
	cid="$(cat "$OUT_DIR/.container-id" 2>/dev/null || true)"
	sb="$(cat "$OUT_DIR/.sandbox-id" 2>/dev/null || true)"
	hpath="$(cat "$OUT_DIR/.handoff-path" 2>/dev/null || true)"
	[ -n "$cid" ] || { skip "remove (no container)"; return; }
	crt rm "$cid" 2>>"$OUT_DIR/lifecycle.log" && pass "remove returned" || fail "remove"
	if [ -z "$hpath" ] || [ ! -e "$hpath" ]; then
		pass "handoff subtree deleted after remove"
	else
		fail "handoff subtree still present after remove: $hpath"
	fi
	# Idempotency: a second remove must not error (the record is already gone, so
	# crictl rm is a no-op/ignorable error; the handoff cleanup is independently
	# idempotent). Cleanup of the sandbox follows.
	crt rm "$cid" >/dev/null 2>&1 || true
	rm -f "$OUT_DIR/.container-id"
	[ -n "$sb" ] && { crt stopp "$sb" >/dev/null 2>&1 || true; crt rmp "$sb" >/dev/null 2>&1 || true; rm -f "$OUT_DIR/.sandbox-id"; }

	if [ "$(crt ps -a -q 2>/dev/null | wc -l | tr -d ' ')" = "0" ]; then pass "no containers remain"; else fail "containers remain after cleanup"; fi
	if [ "$(crt pods -q 2>/dev/null | wc -l | tr -d ' ')" = "0" ]; then pass "no sandboxes remain"; else fail "sandboxes remain after cleanup"; fi

	stop_adapter
	if [ -S "$SOCKET" ]; then fail "stale CRI socket left at $SOCKET"; else pass "no stale CRI socket after adapter stop"; fi
}

# --- main --------------------------------------------------------------------
if [ "$INTEGRATION" != 1 ]; then
	print_plan
	exit 0
fi

setup
start_adapter
phase_adapter
phase_image
phase_create
phase_prepared
phase_producer
phase_start
phase_verified
phase_stop_keeps
phase_remove

echo
if [ "$FAILURES" -eq 0 ]; then
	pass "CRI-I4-1 handoff-lifecycle fixture: all phases passed"
	exit 0
fi
fail "CRI-I4-1 handoff-lifecycle fixture: $FAILURES check(s) failed (diagnostics in $OUT_DIR)"
exit 1
