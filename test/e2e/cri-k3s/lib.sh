# shellcheck shell=bash
#
# lib.sh -- shared helper functions for the CRI k3s e2e harnesses in this
# directory (linuxpod-*.sh, node-reboot-recovery.sh, conformance-smoke.sh).
#
# This file is sourced, not executed:
#
#   HERE="$(cd "$(dirname "$0")" && pwd)"
#   . "$HERE/lib.sh"
#
# How to add a function here:
#   - Only move a function here when its body is byte-identical in every
#     harness that defines it. If one harness needs different behavior, keep
#     (or redefine) the function locally in that harness *after* sourcing this
#     file; the local definition overrides the shared one.
#   - Functions here may reference globals (NS, OUT_DIR, MARKER, RECOVER_*,
#     LINUXPOD_BACKED, MACVZ_* env, ...) and helpers (log/pass/skip/fail, kn,
#     pod_name, ...) that the sourcing harness defines. Bash resolves them at
#     call time, but every harness that *calls* the function must provide
#     everything it references.

# Guard against double-sourcing.
if [ -n "${MACVZ_CRI_E2E_LIB_SOURCED:-}" ]; then
	return 0
fi
MACVZ_CRI_E2E_LIB_SOURCED=1

# Run a user-supplied hook command string via sh -c. Returns 3 when the
# hook is unset so callers can distinguish "not configured" from failure.
# NOTE: linuxpod-dns.sh and linuxpod-volumes.sh keep a local variant that
# overrides this one.
run_hook() {
	local cmd="$1"; shift
	[ -n "$cmd" ] || return 3
	sh -c "$cmd"
}

# Pod status accessor (uses the harness's kn/NS).
pod_phase() { kn get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }

# linuxpod_gate <human-phase-name> -> 0 if LinuxPod-backed, else skip+return 1.
# LINUXPOD_BACKED is set by the sourcing harness's backend-evidence phase.
linuxpod_gate() {
	if [ "$LINUXPOD_BACKED" = 1 ]; then
		return 0
	fi
	skip "$1: not proven LinuxPod-backed (blocked on CRI-L serving #127 + networking #128 + logs/exec/stats #129, and a non-simulated helper). See backend-evidence phase."
	return 1
}

# linuxpod_state_count <output-file> -> writes raw audit; prints residual-state
# line count (LinuxPod VMs/containers/rootfs/handoff/network). Return 2 on hook
# failure (distinct from a clean zero), 3 if the audit hook is unset.
linuxpod_state_count() {
	local out_file="$1" raw
	[ -n "${MACVZ_LINUXPOD_AUDIT_CMD:-}" ] || return 3
	if ! raw="$(run_hook "$MACVZ_LINUXPOD_AUDIT_CMD" 2>"$out_file.err")"; then
		printf '%s\n' "$raw" >"$out_file"
		return 2
	fi
	printf '%s\n' "$raw" >"$out_file"
	printf '%s\n' "$raw" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' '
}

# Run a command with stdout/stderr redirected to files and a hard timeout in
# seconds; returns 124 on timeout, else the command's exit code.
run_bounded() {
	local stdout="$1" stderr="$2" timeout="$3"; shift 3
	: >"$stdout"
	: >"$stderr"
	"$@" >"$stdout" 2>"$stderr" &
	local pid=$! deadline=$((SECONDS + timeout)) rc=0
	while kill -0 "$pid" 2>/dev/null; do
		if [ "$SECONDS" -ge "$deadline" ]; then
			kill "$pid" 2>/dev/null || true
			sleep 1
			kill -9 "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
			printf 'command timed out after %ss\n' "$timeout" >>"$stderr"
			return 124
		fi
		sleep 1
	done
	wait "$pid" || rc=$?
	return "$rc"
}

# exec_logs_ok <pod> <tag> -> 0 if exec serves the marker and logs show the boot
# markers (proves the Pod is genuinely usable, not Running-but-dead). Uses the
# sourcing harness's OUT_DIR/MARKER/APP_BOOT_MARKER globals.
exec_logs_ok() {
	local pod="$1" tag="$2" out
	run_bounded "$OUT_DIR/$tag-exec.out" "$OUT_DIR/$tag-exec.err" 20 \
		kn exec "$pod" -c app -- sh -c 'cat /www/index.html 2>/dev/null' || return 1
	out="$(cat "$OUT_DIR/$tag-exec.out")"
	printf '%s' "$out" | grep -q "$MARKER" || return 1
	run_bounded "$OUT_DIR/$tag-logs.out" "$OUT_DIR/$tag-logs.err" 20 \
		kn logs "$pod" -c app || return 1
	grep -q "$APP_BOOT_MARKER" "$OUT_DIR/$tag-logs.out" || return 1
	return 0
}

# wait_exec_logs_ok <tag> -> echoes the usable Pod name once exec/logs recover.
# Helper restart can leave the old Pod phase as Running while kubelet is still
# reconciling a recreated sandbox, so phase alone is not a readiness signal
# here. Polls RECOVER_TRIES times, RECOVER_INTERVAL seconds apart.
wait_exec_logs_ok() {
	local tag="$1" pod="" _
	for _ in $(seq 1 "$RECOVER_TRIES"); do
		pod="$(pod_name)"
		if [ -n "$pod" ] && [ "$(pod_phase "$pod")" = "Running" ] && exec_logs_ok "$pod" "$tag"; then
			printf '%s' "$pod"
			return 0
		fi
		sleep "$RECOVER_INTERVAL"
	done
	printf '%s' "$pod"
	return 1
}

# Capture the node default route before the run via MACVZ_ROUTE_AUDIT_CMD
# (skips when the hook is unset).
phase_route_before() {
	log "Phase: default-route audit (before)"
	if [ -z "${MACVZ_ROUTE_AUDIT_CMD:-}" ]; then
		skip "route-before (set MACVZ_ROUTE_AUDIT_CMD to capture/compare the node default route)"
		return 0
	fi
	if run_hook "$MACVZ_ROUTE_AUDIT_CMD" >"$OUT_DIR/route-before.txt" 2>"$OUT_DIR/route-before.err"; then
		pass "captured node default route(s) ($OUT_DIR/route-before.txt)"
	else
		fail "MACVZ_ROUTE_AUDIT_CMD failed before run (see $OUT_DIR/route-before.err)"
	fi
}
