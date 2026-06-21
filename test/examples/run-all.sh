#!/usr/bin/env bash
#
# run-all.sh — published P8 real-app validation suite (issue #65).
#
# Runs every published MacVz real-app fixture against the cluster named by
# KUBECONFIG, in sequence, and prints a single pass/fail summary. Each fixture
# is a self-contained harness (apply → assert → tear down) that exercises the
# same access path a human uses; this runner just orchestrates them so a
# developer can validate a clean cluster with one command and compare the
# result against the expected output documented in README.md.
#
# Each fixture exits 0 only when all of its checks pass, non-zero otherwise, and
# attempts to tear its namespace down before returning. This runner exits
# non-zero if any fixture failed.
#
# Usage:
#   KUBECONFIG=... ./run-all.sh                  # run every published fixture
#   KUBECONFIG=... ./run-all.sh hello-http p6    # run a selected subset (by id)
#   ./run-all.sh --list                          # list fixtures and exit
#
# Environment (passed through to each fixture; see each run.sh for the rest):
#   KUBECONFIG   Cluster credentials (standard kubectl resolution).
#   KUBECTL      kubectl binary (default: kubectl).
#   MACVZ_E2E_TIMEOUT  Per-wait timeout in seconds, applied to every fixture
#                      (default: each fixture's own default, typically 180).
#   MACVZ_E2E_CONTINUE If 1, keep running after a fixture fails (default: 1).
#                      Set to 0 to stop at the first failure.
set -uo pipefail

KUBECTL="${KUBECTL:-kubectl}"
CONTINUE="${MACVZ_E2E_CONTINUE:-1}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"

# --- registry of published fixtures ------------------------------------------
# id | relative run.sh path | one-line description | timeout-env-var
# Order matters: cheapest/most-foundational first so an early failure is the
# most informative. Each id is what a developer passes on the command line.
FIXTURES=(
	"hello-http|test/examples/hello-http/run.sh|Minimal public nginx over a ClusterIP Service + port-forward (#61)|MACVZ_HELLO_TIMEOUT"
	"p6-compat|test/e2e/p6-compat/run.sh|Multi-Deployment compatibility: ConfigMap/Secret/SA/probes/exec/in-cluster Service (#53)|MACVZ_P6_TIMEOUT"
	"headlamp-ui|test/e2e/headlamp-ui/run.sh|Headlamp management UI: projected SA token, in-cluster API, RBAC boundary (#63)|MACVZ_HEADLAMP_TIMEOUT"
	"guestbook|test/examples/guestbook/run.sh|Multi-tier guestbook: redis leader/follower + frontend, replication, scaling, rollout restart, browser-visible (#62)|MACVZ_GB_TIMEOUT"
)

c_off=""; c_green=""; c_red=""; c_bold=""
if [ -t 1 ]; then
	c_off=$'\033[0m'; c_green=$'\033[32m'; c_red=$'\033[31m'; c_bold=$'\033[1m'
fi
log()  { printf "%s\n" "$*"; }
pass() { printf "%sPASS%s %s\n" "$c_green" "$c_off" "$*"; }
fail() { printf "%sFAIL%s %s\n" "$c_red" "$c_off" "$*"; }
die()  { printf "%sFATAL%s %s\n" "$c_red" "$c_off" "$*" >&2; exit 2; }

fixture_field() { # $1=fixture-line $2=field-index(1-4)
	printf "%s" "$1" | cut -d'|' -f"$2"
}

list_fixtures() {
	log "${c_bold}Published P8 real-app validation fixtures:${c_off}"
	for entry in "${FIXTURES[@]}"; do
		printf "  %-14s %s\n" "$(fixture_field "$entry" 1)" "$(fixture_field "$entry" 3)"
	done
}

# --- argument handling: optional subset of fixture ids -----------------------
SELECTED=()
for arg in "$@"; do
	case "$arg" in
		--list|-l) list_fixtures; exit 0 ;;
		-h|--help) sed -n '2,33p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
		-*) die "unknown flag: $arg (try --list or --help)" ;;
		*)  SELECTED+=("$arg") ;;
	esac
done

# Resolve the requested ids (default: all) into run.sh paths, validating each.
RUN_IDS=()
if [ "${#SELECTED[@]}" -eq 0 ]; then
	for entry in "${FIXTURES[@]}"; do RUN_IDS+=("$(fixture_field "$entry" 1)"); done
else
	for want in "${SELECTED[@]}"; do
		found=0
		for entry in "${FIXTURES[@]}"; do
			[ "$(fixture_field "$entry" 1)" = "$want" ] && found=1 && break
		done
		[ "$found" -eq 1 ] || die "unknown fixture id: '$want' (try --list)"
		RUN_IDS+=("$want")
	done
fi

# --- preflight ---------------------------------------------------------------
command -v "$KUBECTL" >/dev/null 2>&1 || die "kubectl not found (set KUBECTL=...)"
if ! "$KUBECTL" version -o json >/dev/null 2>&1; then
	die "cannot reach the cluster — set KUBECONFIG to your MacVz cluster"
fi
log "${c_bold}MacVz P8 real-app validation suite${c_off} — ${#RUN_IDS[@]} fixture(s)"
log "Cluster: $("$KUBECTL" config current-context 2>/dev/null || echo '?')"
log ""

# --- run ---------------------------------------------------------------------
declare -a RESULTS
overall=0
for id in "${RUN_IDS[@]}"; do
	for entry in "${FIXTURES[@]}"; do
		[ "$(fixture_field "$entry" 1)" = "$id" ] || continue
		rel="$(fixture_field "$entry" 2)"
		desc="$(fixture_field "$entry" 3)"
		tvar="$(fixture_field "$entry" 4)"
		script="$REPO_ROOT/$rel"
		break
	done

	log "${c_bold}── $id ──${c_off} $desc"
	if [ ! -x "$script" ]; then
		fail "$id: run.sh missing or not executable ($rel)"
		RESULTS+=("$id|FAIL (missing)")
		overall=1
		[ "$CONTINUE" = "1" ] || break
		continue
	fi

	# Apply a suite-wide timeout to each fixture unless the caller set the
	# fixture's own timeout var explicitly.
	env_overrides=()
	if [ -n "${MACVZ_E2E_TIMEOUT:-}" ] && [ -z "${!tvar:-}" ]; then
		env_overrides+=("$tvar=$MACVZ_E2E_TIMEOUT")
	fi

	if env "${env_overrides[@]+"${env_overrides[@]}"}" KUBECTL="$KUBECTL" "$script"; then
		pass "$id"
		RESULTS+=("$id|PASS")
	else
		rc=$?
		fail "$id (exit $rc)"
		RESULTS+=("$id|FAIL (exit $rc)")
		overall=1
		[ "$CONTINUE" = "1" ] || { log ""; log "Stopping (MACVZ_E2E_CONTINUE=0)."; break; }
	fi
	log ""
done

# --- summary -----------------------------------------------------------------
log ""
log "${c_bold}Summary${c_off}"
for r in "${RESULTS[@]}"; do
	id="${r%%|*}"; status="${r#*|}"
	case "$status" in
		PASS) printf "  %s%-14s PASS%s\n" "$c_green" "$id" "$c_off" ;;
		*)    printf "  %s%-14s %s%s\n" "$c_red" "$id" "$status" "$c_off" ;;
	esac
done
log ""
if [ "$overall" -eq 0 ]; then
	pass "all published P8 fixtures passed"
else
	fail "one or more P8 fixtures failed (see per-fixture diagnostics above)"
fi
exit "$overall"
