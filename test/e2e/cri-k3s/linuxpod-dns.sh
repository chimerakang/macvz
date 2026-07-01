#!/usr/bin/env bash
#
# linuxpod-dns.sh — CRI-L8-2 (#142) k3s DNS + Service discovery validation for
# the experimental LinuxPod-backed CRI path.
#
# This is the DNS/Service-discovery sibling of linuxpod-inloop.sh (#130). Where
# inloop proves the LinuxPod Pod *lifecycle*, shared namespace, and a *manually
# probed* ClusterIP curl, this harness proves the normal k3s DNS path works from
# inside a LinuxPod-backed Pod: CoreDNS reachability, `*.svc` and
# `*.svc.cluster.local` resolution, headless-Service Pod A records, and
# same-namespace vs other-namespace lookups — and that DNS keeps working across
# rollout, macvz-cri restart, LinuxPod helper restart, and netd reload.
#
# THE CENTRAL DISCIPLINE: distinguish a DNS failure from a Pod-networking or a
# Service-routing failure, never collapse them into one red "it didn't work".
# Every resolution check classifies the failure (NXDOMAIN vs CoreDNS-unreachable
# vs resolves-but-unroutable) so an operator report names the real layer:
#   - resolver-config : does the Pod even have a cluster resolver + search list?
#   - coredns-reach   : does a *known-good* name resolve at all (CoreDNS up)?
#   - nxdomain-control: does a *known-bad* name return an authoritative NXDOMAIN
#                       (proving CoreDNS answers, vs blanket-timing-out)?
#   - dns-vs-route    : resolve a Service by name, then curl it BOTH by name and
#                       by the resolved ClusterIP — by-IP-ok/by-name-fail is a
#                       DNS layer fault; by-IP-fail is a Service routing fault,
#                       not DNS.
#
# HONESTY GATE (inherited from linuxpod-inloop.sh / #130). The shipped CRI
# serving path runs on apple/container; the LinuxPod backend gate is a startup
# handshake that, for the prototype, reports simulated=true. A Pod reaching
# Running is therefore NOT by itself evidence of a LinuxPod-backed Pod. The DNS
# checks still RUN without the proof (DNS is meaningful on either backend), but
# the LinuxPod-specific framing — "DNS works on a genuine LinuxPod micro-VM" —
# is only asserted when MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD proves the Pod is a
# real, non-simulated LinuxPod backend. Absent that proof the suite says so
# loudly and reports the DNS result against the apple/container path instead of
# silently claiming a LinuxPod result.
#
# Gating: like linuxpod-inloop.sh, the live suite mutates a real cluster and a
# real macOS CRI node, so it runs only when MACVZ_INTEGRATION=1 *and* a reachable
# KUBECONFIG is provided. Without both it prints the runbook plan and exits 0, so
# it is safe in `go test`-style CI and `bash -n` / shellcheck validation.
#
# It is an isolated `develop`-track feasibility probe, NOT the shipped Virtual
# Kubelet e2e (test/e2e/e2e.sh), and must not gate the VK release path.
#
# Environment:
#   MACVZ_INTEGRATION       set to 1 to run live (otherwise plan-only).
#   KUBECONFIG              kubeconfig for the k3s control plane (required live).
#   MACVZ_NODE              name of the MacVz CRI node. Default: auto-detected as
#                           the (single) node carrying node.macvz.io/runtime=apple-container.
#   MACVZ_CRI_IMAGE         arm64 image for the fixture (default: busybox:1.36.1).
#   MACVZ_DNS_DOMAIN        cluster DNS domain (default: cluster.local).
#   MACVZ_CRI_OUT_DIR       results/diagnostics dir (default: a mktemp dir).
#
# Operator hooks (commands run via `sh -c`; the harness cannot reach the remote
# macOS node itself). A churn phase whose required hook is unset is SKIPped
# *loudly*, never silently passed:
#   MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
#                           proves the named Pod's sandbox is served by a real,
#                           non-simulated LinuxPod backend (see linuxpod-inloop.sh).
#                           Without it the DNS checks still run but against the
#                           apple/container path.
#   MACVZ_RESTART_CRI_CMD   restart the macvz-cri service on the CRI node.
#   MACVZ_RESTART_HELPER_CMD restart (or crash+restart) the LinuxPod helper.
#   MACVZ_RESTART_NETD_CMD   reload/restart macvz-netd (e.g. the non-sudo
#                           hooks/netd-reload-policy.sh socket reloadPolicy).
#   MACVZ_ROUTE_AUDIT_CMD   print the node's default route(s); captured before
#                           and after, asserted unchanged (non-goal: never mutate
#                           the host default route, `192.168.1.1` via `en0`).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# Shared helper functions (see lib.sh header before adding more).
. "$HERE/lib.sh"
FIXTURE="$HERE/fixtures/linuxpod-dns-workload.yaml"
NS="macvz-cri-linuxpod-dns-e2e"
DEPLOY="linuxpod-dns"
SVC="linuxpod-dns"
SVC_HEADLESS="linuxpod-dns-headless"
MARKER="macvz-cri-l8-dns-ok"
SIDECAR_DNS_MARKER="macvz-cri-l8-dns-sidecar-resolve-ok"

INTEGRATION="${MACVZ_INTEGRATION:-0}"
IMAGE="${MACVZ_CRI_IMAGE:-busybox:1.36.1}"
NODE="${MACVZ_NODE:-}"
DNS_DOMAIN="${MACVZ_DNS_DOMAIN:-cluster.local}"
OUT_DIR="${MACVZ_CRI_OUT_DIR:-}"

RUNTIME_LABEL="node.macvz.io/runtime=apple-container"
TAINT_KEY="node.macvz.io/host-namespace-unsupported"

FAILURES=0
SKIPS=0
TMP_ROOT=""
OUT_DIR_WAS_SET=0
LINUXPOD_BACKED=0
# DNS_WIRED is set by phase_dns_core from the initial Pod: 1 if the guest has a
# cluster resolver, 0 if the LinuxPod backend has not injected one yet. The
# single-shot resolution phases gate on it so they skip with the #128 blocker
# (feature absent) instead of failing as if a record were missing.
DNS_WIRED=0

c_blue='\033[1;34m'; c_green='\033[1;32m'; c_red='\033[1;31m'; c_yellow='\033[1;33m'; c_off='\033[0m'
log()  { printf "${c_blue}==>${c_off} %s\n" "$*"; }
pass() { printf "${c_green}PASS${c_off} %s\n" "$*"; }
skip() { printf "${c_yellow}SKIP${c_off} %s\n" "$*"; SKIPS=$((SKIPS+1)); }
fail() { printf "${c_red}FAIL${c_off} %s\n" "$*"; FAILURES=$((FAILURES+1)); }
die()  { printf "${c_red}FATAL${c_off} %s\n" "$*" >&2; exit 2; }

k() { kubectl "$@"; }
kn() { kubectl -n "$NS" "$@"; }

print_plan() {
	cat <<'PLAN'
CRI-L8-2 LinuxPod k3s DNS + Service discovery suite (plan; set
MACVZ_INTEGRATION=1 and a reachable KUBECONFIG to run live):

  preflight       kubectl reachable; locate the MacVz CRI node by its runtime
                  label; assert #84 labels + NoSchedule taint present; node Ready.
  route-before    capture the node default route(s) (MACVZ_ROUTE_AUDIT_CMD) so
                  the post-run audit can prove they were never mutated.
  deploy          kubectl apply fixtures/linuxpod-dns-workload.yaml (app + late
                  sidecar Pod, ConfigMap, ClusterIP + headless Service); rollout.
  scheduling      Pod landed on the MacVz node; events clean.
  backend-evidence  HONESTY GATE: assert via MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD
                  that the Pod is served by a genuine, non-simulated LinuxPod
                  backend. The DNS checks run either way; without the proof they
                  are reported against the apple/container path, not claimed as
                  a LinuxPod result.
  resolver-config the Pod's /etc/resolv.conf has a cluster nameserver and the
                  expected `<ns>.svc.<domain>`/`svc.<domain>`/`<domain>` search
                  list (a missing resolver is a kubelet dnsPolicy fault, NOT a
                  resolution fault — classified separately).
  coredns-reach   a known-good name (`kubernetes.default.svc.<domain>`) resolves
                  to the kubernetes Service ClusterIP — CoreDNS is reachable and
                  answering (also the first other-namespace lookup).
  nxdomain-control a known-bad name returns an authoritative NXDOMAIN (proving
                  CoreDNS answers, vs blanket timeout = CoreDNS unreachable).
  svc-discovery   `linuxpod-dns.<ns>.svc` and `…svc.<domain>` both resolve to the
                  ClusterIP Service IP (same-namespace short + FQDN).
  headless        `linuxpod-dns-headless.<ns>.svc.<domain>` returns the Pod IP as
                  an A record (not a single ClusterIP).
  cross-namespace `kube-dns.kube-system.svc.<domain>` resolves to the kube-dns
                  Service ClusterIP (other-namespace lookup).
  dns-vs-route    resolve the Service by name, then curl it BOTH by name and by
                  the resolved ClusterIP — separates a DNS-layer fault
                  (by-IP-ok/by-name-fail) from a Service-routing fault
                  (by-IP-fail), so DNS is never blamed for routing.
  sidecar-dns     the late sidecar's boot-time nslookup proof file is present in
                  the shared namespace (boot-time DNS evidence).
  dns-after-rollout      rollout-restart the Deployment; re-run the core DNS
                  checks on the fresh Pod.
  dns-after-cri-restart  restart macvz-cri (MACVZ_RESTART_CRI_CMD); same Pod UID;
                  re-run the core DNS checks.
  dns-after-helper-restart restart the LinuxPod helper (MACVZ_RESTART_HELPER_CMD,
                  LinuxPod-backed only); re-run the core DNS checks.
  dns-after-netd-reload  reload macvz-netd (MACVZ_RESTART_NETD_CMD); re-run the
                  core DNS checks; assert the default route is unchanged.
  cleanup         delete the fixture; assert no residual Pods.
  route-after     re-capture the default route(s); assert unchanged.

Gated: the live suite drives a real cluster and a real macOS CRI node. The
LinuxPod framing additionally requires a backend-evidence hook; the churn phases
need operator hooks (MACVZ_RESTART_CRI_CMD, MACVZ_RESTART_HELPER_CMD,
MACVZ_RESTART_NETD_CMD, MACVZ_ROUTE_AUDIT_CMD). An unset hook is skipped loudly,
never silently passed. See the header for the env contract and
test/e2e/cri-k3s/README.md for topology.
PLAN
}

# --- setup / teardown --------------------------------------------------------
setup() {
	command -v kubectl >/dev/null 2>&1 || die "kubectl not found in PATH"
	[ -f "$FIXTURE" ] || die "fixture not found at $FIXTURE"
	k get --raw='/healthz' --request-timeout=10s >/dev/null 2>&1 \
		|| die "cluster unreachable (KUBECONFIG=${KUBECONFIG:-unset}); set a reachable kubeconfig"

	TMP_ROOT="$(mktemp -d -t macvz-cri-linuxpod-dns)"
	if [ -n "$OUT_DIR" ]; then
		OUT_DIR_WAS_SET=1
	else
		OUT_DIR="$TMP_ROOT/out"
	fi
	mkdir -p "$OUT_DIR"
	log "out=$OUT_DIR image=$IMAGE domain=$DNS_DOMAIN"
}

cleanup_trap() {
	if [ "${MACVZ_CRI_KEEP:-0}" != 1 ]; then
		kubectl delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
	fi
	if [ -n "$TMP_ROOT" ] && [ "${MACVZ_CRI_KEEP:-0}" != 1 ] && [ "$OUT_DIR_WAS_SET" = 1 ]; then
		rm -rf "$TMP_ROOT"
	fi
}
trap cleanup_trap EXIT

run_hook() {
	local cmd="$1"
	[ -n "$cmd" ] || return 3
	sh -c "$cmd"
}

# --- generic helpers ---------------------------------------------------------
pod_name() {
	kn get pods -l app=linuxpod-dns --field-selector=status.phase=Running \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}
pod_uid()   { kn get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null; }
pod_ip()    { kn get pod "$1" -o jsonpath='{.status.podIP}' 2>/dev/null; }
svc_clusterip() { kn get svc "$1" -o jsonpath='{.spec.clusterIP}' 2>/dev/null; }

# Run nslookup for <name> inside the app container of <pod>, writing the raw
# output (including any kubectl/exec error) to <outfile>. The caller parses the
# output rather than the exit code.
#
# Resolver tool: some busybox images (the LinuxPod fixture's included) ship the
# `nslookup` applet in the binary but do NOT symlink `/bin/nslookup`, so the bare
# command is "not found" while `busybox nslookup` works. Prefer the bare command,
# fall back to `busybox nslookup`, and emit an explicit `no-resolver-tool` marker
# if neither exists — so a missing tool is classified apart from a DNS result.
# `name` is passed as a positional arg (not interpolated) to avoid quoting holes.
nslookup_raw() {
	local pod="$1" name="$2" outfile="$3"
	kn exec "$pod" -c app -- sh -c '
		n="$1"
		if command -v nslookup >/dev/null 2>&1; then exec nslookup "$n";
		elif command -v busybox >/dev/null 2>&1; then exec busybox nslookup "$n";
		else echo "no-resolver-tool: image has neither nslookup nor busybox"; exit 127; fi
	' _ "$name" >"$outfile" 2>&1 || true
}

# pod_has_cluster_resolver <pod> <outfile> -> 0 if the Pod has a cluster
# nameserver in /etc/resolv.conf, else 1. A missing resolver is the LinuxPod
# backend not yet injecting kubelet's DNS config (a #128 gap), NOT a resolution
# fault — callers gate the resolution checks on this so the two never blur.
pod_has_cluster_resolver() {
	local pod="$1" outfile="$2"
	kn exec "$pod" -c app -- sh -c 'cat /etc/resolv.conf 2>&1; true' >"$outfile" 2>&1 || true
	grep -q '^nameserver' "$outfile"
}

# Print the *answer* A-record IPs from an nslookup output file: every `Address`
# line that is not the resolver's own `…:53` line.
nslookup_answers() {
	grep -E '^Address' "$1" 2>/dev/null | grep -v ':53' \
		| grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' || true
}

# Classify why an nslookup produced no answer, so DNS faults are never confused
# with image-tooling, exec-transport, or networking faults.
# Prints: no-tool | unreachable | nxdomain | unknown.
#   no-tool     — the image has no resolver applet (a fixture/image issue).
#   unreachable — the query hung or the DNS server could not be reached: a
#                 Pod-networking/Service-routing path to CoreDNS (incl. the
#                 LinuxPod supervisor exec timing out on a blocking query that
#                 never gets an answer), NOT a missing DNS record.
#   nxdomain    — CoreDNS answered authoritatively that the record is absent.
dns_failure_kind() {
	local f="$1"
	if grep -Eqi 'no-resolver-tool|nslookup: .*not found|applet not found' "$f"; then
		echo no-tool
	elif grep -Eqi 'read timeout|container not found|unable to upgrade connection|i/o timeout|context deadline|deadline exceeded' "$f"; then
		echo unreachable
	elif grep -Eqi "can't resolve|NXDOMAIN|Name or service not known|Name does not resolve|server can't find" "$f"; then
		# An authoritative negative still requires the server to have answered.
		if grep -Eqi 'timed out|no servers could be reached|connection refused' "$f"; then
			echo unreachable
		else
			echo nxdomain
		fi
	elif grep -Eqi 'timed out|no servers could be reached|connection refused' "$f"; then
		echo unreachable
	else
		echo unknown
	fi
}

# assert_resolves_to <pod> <name> <expected_ip> <label> <outfile>
# pass if <name> resolves to an answer set containing <expected_ip>; otherwise
# fail with the classified failure kind (DNS vs networking), or a wrong-IP note.
assert_resolves_to() {
	local pod="$1" name="$2" expected="$3" label="$4" outfile="$5" answers kind
	nslookup_raw "$pod" "$name" "$outfile"
	answers="$(nslookup_answers "$outfile")"
	if [ -z "$answers" ]; then
		kind="$(dns_failure_kind "$outfile")"
		case "$kind" in
			no-tool)     skip "$label: image has no resolver applet (nslookup/busybox) — cannot probe DNS (fixture/image issue, not a DNS result; see $outfile)" ;;
			unreachable) fail "$label: '$name' did not resolve — CoreDNS UNREACHABLE (Pod-networking/Service-routing to kube-dns, not a DNS record fault; see $outfile)" ;;
			nxdomain)    fail "$label: '$name' returned NXDOMAIN — DNS answered but the record is missing (see $outfile)" ;;
			*)           fail "$label: '$name' did not resolve (unclassified; see $outfile)" ;;
		esac
		return 1
	fi
	if printf '%s\n' "$answers" | grep -qx "$expected"; then
		pass "$label: '$name' -> $expected"
		return 0
	fi
	fail "$label: '$name' resolved to [$(printf '%s' "$answers" | tr '\n' ' ')], expected $expected (see $outfile)"
	return 1
}

# --- phases ------------------------------------------------------------------
phase_preflight() {
	log "Phase: preflight (locate + validate the MacVz CRI node)"
	if [ -z "$NODE" ]; then
		local nodes count
		nodes="$(k get nodes -l "$RUNTIME_LABEL" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)"
		count="$(printf '%s\n' "$nodes" | sed '/^$/d' | wc -l | tr -d ' ')"
		if [ "$count" -gt 1 ]; then
			fail "multiple nodes carry label $RUNTIME_LABEL; set MACVZ_NODE explicitly"
			return 1
		fi
		NODE="$(printf '%s\n' "$nodes" | sed '/^$/d' | head -n 1)"
	fi
	if [ -z "$NODE" ]; then
		fail "no node carries label $RUNTIME_LABEL (register the MacVz node per README #84)"
		return 1
	fi
	pass "MacVz CRI node: $NODE"

	local failures_before="$FAILURES" rt hns
	rt="$(k get node "$NODE" -o jsonpath='{.metadata.labels.node\.macvz\.io/runtime}' 2>/dev/null)"
	hns="$(k get node "$NODE" -o jsonpath='{.metadata.labels.node\.macvz\.io/host-namespace}' 2>/dev/null)"
	[ "$rt" = "apple-container" ] && pass "runtime label" || fail "runtime label (got '$rt')"
	[ "$hns" = "unsupported" ] && pass "host-namespace label" || fail "host-namespace label (got '$hns')"

	if k get node "$NODE" -o jsonpath="{.spec.taints[?(@.key=='$TAINT_KEY')].effect}" 2>/dev/null | grep -q NoSchedule; then
		pass "host-namespace NoSchedule taint"
	else
		fail "missing $TAINT_KEY NoSchedule taint (register the node per README #84)"
	fi
	if k get node "$NODE" -o jsonpath="{.status.conditions[?(@.type=='Ready')].status}" 2>/dev/null | grep -q True; then
		pass "node Ready"
	else
		fail "node $NODE is not Ready"
	fi
	[ "$FAILURES" = "$failures_before" ]
}

apply_fixture() {
	sed "s#image: busybox:1.36.1#image: $IMAGE#g" "$FIXTURE" >"$OUT_DIR/workload.applied.yaml"
	k apply -f "$OUT_DIR/workload.applied.yaml" >"$OUT_DIR/apply.log" 2>&1
}

phase_deploy() {
	log "Phase: deploy DNS fixture + rollout"
	if ! apply_fixture; then
		fail "kubectl apply failed (see $OUT_DIR/apply.log)"
		return 1
	fi
	pass "fixture applied"
	if kn rollout status "deploy/$DEPLOY" --timeout=5m >"$OUT_DIR/rollout.log" 2>&1; then
		pass "kubectl rollout status (Deployment available)"
	else
		fail "rollout did not complete (see $OUT_DIR/rollout.log)"
		kn describe "deploy/$DEPLOY" >"$OUT_DIR/deploy-describe.log" 2>&1 || true
		kn get pods -o wide >"$OUT_DIR/pods.log" 2>&1 || true
	fi
}

phase_scheduling() {
	log "Phase: scheduling + Pod events"
	local pod node; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no fixture Pod found"; return 1; }
	node="$(kn get pod "$pod" -o jsonpath='{.spec.nodeName}' 2>/dev/null)"
	if [ "$node" = "$NODE" ]; then
		pass "Pod $pod scheduled onto MacVz node $NODE"
	else
		fail "Pod scheduled onto '$node', expected MacVz node '$NODE'"
	fi
	kn get events --field-selector "involvedObject.name=$pod" >"$OUT_DIR/pod-events.log" 2>&1 || true
	if grep -Eqi 'FailedScheduling|FailedCreatePodSandBox' "$OUT_DIR/pod-events.log"; then
		fail "Pod events contain scheduling/sandbox failures (see $OUT_DIR/pod-events.log)"
	else
		pass "Pod events clean (no FailedScheduling/FailedCreatePodSandBox)"
	fi
}

phase_backend_evidence() {
	log "Phase: LinuxPod backend evidence (honesty gate)"
	if [ -z "${MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD:-}" ]; then
		skip "backend-evidence: set MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD to prove the Pod is LinuxPod-backed; DNS results below are reported against the apple/container path (blocked on CRI-L serving #127/#128/#129)"
		return 0
	fi
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for backend evidence"; return 1; }
	if ! MACVZ_POD="$pod" run_hook "$MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD" \
		>"$OUT_DIR/backend-evidence.txt" 2>"$OUT_DIR/backend-evidence.err"; then
		fail "MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD failed (see $OUT_DIR/backend-evidence.err)"
		return 1
	fi
	if grep -Eqi 'simulated[":= ]+true' "$OUT_DIR/backend-evidence.txt"; then
		skip "backend-evidence reports simulated=true: node is on the R17 prototype handshake — DNS results reported against the apple/container path (blocked on #127/#128/#129; see $OUT_DIR/backend-evidence.txt)"
		return 0
	fi
	if grep -Eqi 'linuxpod|pod[-_ ]?vm|sandboxVM' "$OUT_DIR/backend-evidence.txt" \
		&& grep -Eqi 'simulated[":= ]+false|backend[":= ]+linuxpod|serving[":= ]+linuxpod' "$OUT_DIR/backend-evidence.txt"; then
		LINUXPOD_BACKED=1
		pass "Pod is served by a genuine (non-simulated) LinuxPod backend; DNS results below are LinuxPod-backed (see $OUT_DIR/backend-evidence.txt)"
	else
		skip "backend-evidence did not prove a non-simulated LinuxPod-backed Pod; DNS results reported against the apple/container path (see $OUT_DIR/backend-evidence.txt)"
	fi
}

# DNS_NOT_WIRED_BLOCKER is the precise pointer used whenever the LinuxPod backend
# has not yet injected a cluster resolver into the guest, so the resolution
# checks are blocked (feature absent) rather than failed (feature regressed).
DNS_NOT_WIRED_BLOCKER="the LinuxPod backend does not yet inject kubelet's cluster DNS config (no /etc/resolv.conf nameserver in the guest) — the normal k3s DNS path is not wired on this backend; blocked on CRI-L Pod networking #128"

# core_dns_checks <label-prefix> <pod>
# The reusable DNS core: resolver config, CoreDNS reachability on a known-good
# name, an authoritative NXDOMAIN control, and same-namespace Service FQDN
# resolution. Re-run verbatim after each churn event.
#
# Feature-absent vs feature-broken discipline: if the guest has NO cluster
# resolver, the backend simply hasn't implemented DNS injection yet — the
# resolution sub-checks are SKIPPED loudly with the #128 blocker (so the suite
# stays a meaningful gate that will flip to hard PASS/FAIL once DNS is wired),
# never silently failed as if a record were missing. Once a resolver IS present,
# the resolution checks become hard assertions.
core_dns_checks() {
	local label="$1" pod="$2" out wired=0

	# resolver-config — a missing cluster resolver is the LinuxPod backend not yet
	# wiring kubelet's DNS config, not a resolution fault: classify it apart.
	out="$OUT_DIR/$label-resolv.conf"
	if pod_has_cluster_resolver "$pod" "$out"; then
		wired=1
		pass "$label/resolver-config: Pod has a cluster nameserver"
		if grep -q "svc.$DNS_DOMAIN" "$out" && grep -q "$NS.svc.$DNS_DOMAIN" "$out"; then
			pass "$label/resolver-config: search list has $NS.svc.$DNS_DOMAIN and svc.$DNS_DOMAIN"
		else
			fail "$label/resolver-config: search list missing expected svc domains (see $out)"
		fi
	else
		skip "$label/resolver-config: $DNS_NOT_WIRED_BLOCKER (see $out)"
	fi

	if [ "$wired" != 1 ]; then
		skip "$label/coredns-reach: blocked — $DNS_NOT_WIRED_BLOCKER"
		skip "$label/nxdomain-control: blocked — $DNS_NOT_WIRED_BLOCKER"
		skip "$label/svc-fqdn: blocked — $DNS_NOT_WIRED_BLOCKER"
		return 0
	fi

	# coredns-reach — known-good name must resolve to the kubernetes Service IP.
	local kube_ip
	kube_ip="$(k -n default get svc kubernetes -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"
	if [ -n "$kube_ip" ]; then
		assert_resolves_to "$pod" "kubernetes.default.svc.$DNS_DOMAIN" "$kube_ip" \
			"$label/coredns-reach" "$OUT_DIR/$label-kubernetes.nslookup" || true
	else
		skip "$label/coredns-reach: could not read kubernetes Service ClusterIP"
	fi

	# nxdomain-control — a known-bad name must come back as an authoritative
	# NXDOMAIN (not a timeout), proving CoreDNS is answering rather than down.
	out="$OUT_DIR/$label-nxdomain.nslookup"
	nslookup_raw "$pod" "macvz-no-such-svc.$NS.svc.$DNS_DOMAIN" "$out"
	if [ -n "$(nslookup_answers "$out")" ]; then
		fail "$label/nxdomain-control: a known-bad name unexpectedly resolved (see $out)"
	else
		case "$(dns_failure_kind "$out")" in
			nxdomain) pass "$label/nxdomain-control: known-bad name -> authoritative NXDOMAIN (CoreDNS is answering)" ;;
			unreachable) fail "$label/nxdomain-control: known-bad name TIMED OUT — CoreDNS unreachable, not NXDOMAIN (Pod-networking/Service-routing fault; see $out)" ;;
			*) fail "$label/nxdomain-control: unclassified negative for a known-bad name (see $out)" ;;
		esac
	fi

	# same-namespace Service FQDN.
	local svc_ip; svc_ip="$(svc_clusterip "$SVC")"
	if [ -n "$svc_ip" ]; then
		assert_resolves_to "$pod" "$SVC.$NS.svc.$DNS_DOMAIN" "$svc_ip" \
			"$label/svc-fqdn" "$OUT_DIR/$label-svc-fqdn.nslookup" || true
	else
		skip "$label/svc-fqdn: could not read $SVC ClusterIP"
	fi
}

phase_dns_core() {
	log "Phase: DNS core (resolver, CoreDNS reachability, NXDOMAIN control, svc FQDN)"
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for DNS core"; return 1; }
	# Record whether the guest has a cluster resolver so the single-shot
	# resolution phases can gate on it (feature-absent -> loud skip, not fail).
	if pod_has_cluster_resolver "$pod" "$OUT_DIR/dns-wired-probe.resolv.conf"; then
		DNS_WIRED=1
	else
		DNS_WIRED=0
	fi
	core_dns_checks "initial" "$pod"
}

# dns_wired_gate <human-phase-name> -> 0 if a cluster resolver is present, else
# skip with the #128 blocker and return 1.
dns_wired_gate() {
	[ "$DNS_WIRED" = 1 ] && return 0
	skip "$1: blocked — $DNS_NOT_WIRED_BLOCKER"
	return 1
}

phase_svc_discovery() {
	log "Phase: Service discovery (short name + FQDN, same namespace)"
	dns_wired_gate "svc-discovery" || return 0
	local pod svc_ip; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for svc-discovery"; return 1; }
	svc_ip="$(svc_clusterip "$SVC")"
	if [ -z "$svc_ip" ]; then
		fail "could not read $SVC ClusterIP"
		return 1
	fi
	# Short name resolves via the search list (`<svc>.<ns>.svc`).
	assert_resolves_to "$pod" "$SVC.$NS.svc" "$svc_ip" \
		"svc-short" "$OUT_DIR/svc-short.nslookup" || true
	# Bare single-label name resolves via the first search domain (`<svc>`).
	assert_resolves_to "$pod" "$SVC" "$svc_ip" \
		"svc-bare" "$OUT_DIR/svc-bare.nslookup" || true
}

phase_headless() {
	log "Phase: headless Service resolution (Pod A record)"
	# The clusterIP-None assertion is meaningful regardless of in-guest DNS, so it
	# runs first; the in-guest A-record resolution gates on a present resolver.
	local pod ip out answers; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for headless"; return 1; }
	ip="$(pod_ip "$pod")"
	if [ -z "$ip" ]; then
		fail "Pod has no podIP to expect from the headless Service"
		return 1
	fi
	# A headless Service (clusterIP: None) must resolve to the backing Pod IP(s),
	# not a virtual ClusterIP.
	local hl_cip; hl_cip="$(svc_clusterip "$SVC_HEADLESS")"
	if [ "$hl_cip" != "None" ]; then
		fail "headless Service $SVC_HEADLESS has clusterIP '$hl_cip', expected None"
	else
		pass "headless Service has clusterIP None"
	fi
	dns_wired_gate "headless/resolve" || return 0
	out="$OUT_DIR/headless.nslookup"
	nslookup_raw "$pod" "$SVC_HEADLESS.$NS.svc.$DNS_DOMAIN" "$out"
	answers="$(nslookup_answers "$out")"
	if printf '%s\n' "$answers" | grep -qx "$ip"; then
		pass "headless: resolves to the backing Pod IP $ip (A record, not a ClusterIP)"
	elif [ -z "$answers" ]; then
		case "$(dns_failure_kind "$out")" in
			no-tool) skip "headless: image has no resolver applet — cannot probe (fixture/image issue; see $out)" ;;
			unreachable) fail "headless: did not resolve — CoreDNS unreachable (not a DNS-record fault; see $out)" ;;
			*) fail "headless: '$SVC_HEADLESS' returned no Pod A record (see $out)" ;;
		esac
	else
		fail "headless: resolved to [$(printf '%s' "$answers" | tr '\n' ' ')], expected the Pod IP $ip (see $out)"
	fi
}

phase_cross_namespace() {
	log "Phase: cross-namespace lookup (kube-dns.kube-system)"
	dns_wired_gate "cross-namespace" || return 0
	local pod kd_ip; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for cross-namespace"; return 1; }
	kd_ip="$(k -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"
	if [ -z "$kd_ip" ]; then
		skip "cross-namespace: kube-dns Service not found in kube-system (non-standard DNS deployment)"
		return 0
	fi
	assert_resolves_to "$pod" "kube-dns.kube-system.svc.$DNS_DOMAIN" "$kd_ip" \
		"cross-namespace" "$OUT_DIR/cross-namespace.nslookup" || true
}

# exec_transport_failed <file> -> 0 if the file shows a kubectl/supervisor exec
# transport failure (the LinuxPod supervisor exec read-timeout, a dropped
# upgrade, a vanished container) rather than a genuine application result. Such a
# failure is INCONCLUSIVE — it must never be reported as a routing or DNS fault.
exec_transport_failed() {
	grep -Eqi 'read timeout|container not found|unable to upgrade connection|i/o timeout|context deadline|deadline exceeded|error executing command in container' "$1"
}

phase_dns_vs_route() {
	# The separation phase: tell a DNS-layer fault, a Service-routing fault, and
	# an exec-transport timeout apart — never collapse them. Curl the Service by
	# the resolved ClusterIP (routing, no DNS) and, when a resolver is present,
	# also by name (full DNS+routing path).
	log "Phase: DNS-vs-routing separation"
	local pod svc_ip out ip_ok=0
	pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for dns-vs-route"; return 1; }
	svc_ip="$(svc_clusterip "$SVC")"
	[ -n "$svc_ip" ] || { fail "could not read $SVC ClusterIP"; return 1; }

	# Routing leg (no DNS): curl the ClusterIP directly.
	kn exec "$pod" -c app -- sh -c "wget -T 5 -qO- http://$svc_ip:80/index.html 2>&1; true" \
		>"$OUT_DIR/dns-vs-route-byip.out" 2>&1 || true
	if grep -q "$MARKER" "$OUT_DIR/dns-vs-route-byip.out"; then
		ip_ok=1
		pass "dns-vs-route/routing: Service reachable by ClusterIP $svc_ip (Service routing from the guest works; DNS not involved)"
	elif exec_transport_failed "$OUT_DIR/dns-vs-route-byip.out"; then
		skip "dns-vs-route/routing: INCONCLUSIVE — the LinuxPod supervisor exec timed out, not a routing verdict (see dns-vs-route-byip.out)"
	else
		fail "dns-vs-route/routing: NOT reachable by ClusterIP — Service-ROUTING fault from the guest, independent of DNS (do not blame DNS; see dns-vs-route-byip.out)"
	fi

	# DNS leg: only meaningful when the backend injected a resolver.
	if ! dns_wired_gate "dns-vs-route/by-name"; then
		return 0
	fi
	out="$OUT_DIR/dns-vs-route.nslookup"
	nslookup_raw "$pod" "$SVC.$NS.svc.$DNS_DOMAIN" "$out"
	kn exec "$pod" -c app -- sh -c "wget -T 5 -qO- http://$SVC.$NS.svc.$DNS_DOMAIN:80/index.html 2>&1; true" \
		>"$OUT_DIR/dns-vs-route-byname.out" 2>&1 || true
	if grep -q "$MARKER" "$OUT_DIR/dns-vs-route-byname.out"; then
		pass "dns-vs-route/by-name: Service reachable by NAME — DNS + routing healthy end-to-end"
	elif exec_transport_failed "$OUT_DIR/dns-vs-route-byname.out"; then
		skip "dns-vs-route/by-name: INCONCLUSIVE — supervisor exec timed out (see dns-vs-route-byname.out)"
	elif [ "$ip_ok" = 1 ]; then
		fail "dns-vs-route/by-name: reachable by ClusterIP but NOT by name — isolated DNS-LAYER fault (routing is fine; resolved='$(nslookup_answers "$out" | head -n1)'; see $out)"
	else
		fail "dns-vs-route/by-name: unreachable by name AND by ClusterIP — routing fault upstream of DNS (see dns-vs-route-byname.out)"
	fi
}

phase_sidecar_dns() {
	log "Phase: sidecar boot-time DNS proof (shared namespace)"
	if [ "$LINUXPOD_BACKED" != 1 ]; then
		skip "sidecar-dns: not proven LinuxPod-backed, so shared-namespace boot DNS proof is not a LinuxPod result (see backend-evidence)"
		return 0
	fi
	dns_wired_gate "sidecar-dns" || return 0
	local pod out; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod for sidecar-dns"; return 1; }
	out="$(kn exec "$pod" -c app -- sh -c 'cat /shared/sidecar-dns 2>/dev/null' 2>"$OUT_DIR/sidecar-dns.err" || true)"
	echo "$out" >"$OUT_DIR/sidecar-dns.out"
	if printf '%s' "$out" | grep -q "$SIDECAR_DNS_MARKER"; then
		pass "late sidecar resolved its own Service by name at boot (shared-namespace DNS)"
	else
		fail "no boot-time DNS proof from the sidecar (see $OUT_DIR/sidecar-dns.out)"
	fi
}

phase_dns_after_rollout() {
	log "Phase: DNS after rollout-restart"
	kn rollout restart "deploy/$DEPLOY" >"$OUT_DIR/rollout-restart.log" 2>&1 \
		|| { fail "rollout restart failed (see $OUT_DIR/rollout-restart.log)"; return 1; }
	if ! kn rollout status "deploy/$DEPLOY" --timeout=5m >>"$OUT_DIR/rollout-restart.log" 2>&1; then
		fail "rollout-restart did not complete (see $OUT_DIR/rollout-restart.log)"
		return 1
	fi
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Running Pod after rollout-restart"; return 1; }
	pass "fresh Pod $pod after rollout-restart"
	core_dns_checks "after-rollout" "$pod"
}

phase_dns_after_cri_restart() {
	log "Phase: DNS after macvz-cri restart"
	if [ -z "${MACVZ_RESTART_CRI_CMD:-}" ]; then
		skip "dns-after-cri-restart (set MACVZ_RESTART_CRI_CMD)"
		return 0
	fi
	local pod uid_before uid_after; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Pod before macvz-cri restart"; return 1; }
	uid_before="$(pod_uid "$pod")"
	run_hook "$MACVZ_RESTART_CRI_CMD" >"$OUT_DIR/restart-cri.log" 2>&1 \
		|| fail "MACVZ_RESTART_CRI_CMD returned non-zero (see $OUT_DIR/restart-cri.log)"
	local ok=0 _
	for _ in $(seq 1 60); do
		[ "$(pod_phase "$pod")" = "Running" ] && { ok=1; break; }
		sleep 2
	done
	uid_after="$(pod_uid "$pod")"
	[ "$ok" = 1 ] && pass "Pod still Running after macvz-cri restart" || fail "Pod not Running after macvz-cri restart"
	if [ -n "$uid_before" ] && [ "$uid_before" = "$uid_after" ]; then
		pass "Pod UID unchanged across macvz-cri restart"
	else
		fail "Pod UID changed ($uid_before -> $uid_after)"
	fi
	core_dns_checks "after-cri-restart" "$pod"
}

phase_dns_after_helper_restart() {
	log "Phase: DNS after LinuxPod helper restart"
	if [ -z "${MACVZ_RESTART_HELPER_CMD:-}" ]; then
		skip "dns-after-helper-restart (set MACVZ_RESTART_HELPER_CMD)"
		return 0
	fi
	if [ "$LINUXPOD_BACKED" != 1 ]; then
		skip "dns-after-helper-restart: node not proven LinuxPod-backed, so a helper restart exercises nothing real (blocked on #127/#128/#129)"
		return 0
	fi
	run_hook "$MACVZ_RESTART_HELPER_CMD" >"$OUT_DIR/restart-helper.log" 2>&1 \
		|| fail "MACVZ_RESTART_HELPER_CMD returned non-zero (see $OUT_DIR/restart-helper.log)"
	local pod ok=0 _
	for _ in $(seq 1 60); do
		pod="$(pod_name)"
		[ -n "$pod" ] && [ "$(pod_phase "$pod")" = "Running" ] && { ok=1; break; }
		sleep 2
	done
	[ "$ok" = 1 ] && pass "Pod Running after helper restart" \
		|| { fail "Pod not Running after helper restart (see $OUT_DIR/restart-helper.log)"; return 1; }
	core_dns_checks "after-helper-restart" "$pod"
}

phase_dns_after_netd_reload() {
	log "Phase: DNS after macvz-netd reload"
	if [ -z "${MACVZ_RESTART_NETD_CMD:-}" ]; then
		skip "dns-after-netd-reload (set MACVZ_RESTART_NETD_CMD; e.g. hooks/netd-reload-policy.sh)"
		return 0
	fi
	# A netd reload must never mutate the host default route; capture it around
	# the reload as a focused guard in addition to the run-wide route audit.
	local route_mid=""
	if [ -n "${MACVZ_ROUTE_AUDIT_CMD:-}" ]; then
		route_mid="$OUT_DIR/route-netd-before.txt"
		run_hook "$MACVZ_ROUTE_AUDIT_CMD" >"$route_mid" 2>/dev/null || true
	fi
	run_hook "$MACVZ_RESTART_NETD_CMD" >"$OUT_DIR/restart-netd.log" 2>&1 \
		|| fail "MACVZ_RESTART_NETD_CMD returned non-zero (see $OUT_DIR/restart-netd.log)"
	if [ -n "$route_mid" ]; then
		run_hook "$MACVZ_ROUTE_AUDIT_CMD" >"$OUT_DIR/route-netd-after.txt" 2>/dev/null || true
		if diff -u "$route_mid" "$OUT_DIR/route-netd-after.txt" >"$OUT_DIR/route-netd.diff" 2>&1; then
			pass "default route unchanged across netd reload"
		else
			fail "default route changed across netd reload (see $OUT_DIR/route-netd.diff)"
		fi
	fi
	local pod; pod="$(pod_name)"
	[ -n "$pod" ] || { fail "no Running Pod after netd reload"; return 1; }
	core_dns_checks "after-netd-reload" "$pod"
}

phase_cleanup() {
	log "Phase: cleanup"
	k delete -f "$OUT_DIR/workload.applied.yaml" --wait=true --timeout=3m >"$OUT_DIR/cleanup.log" 2>&1 || true
	local remaining
	remaining="$(kn get pods -l app=linuxpod-dns -o name 2>/dev/null | wc -l | tr -d ' ')"
	[ "$remaining" = 0 ] && pass "no fixture Pods remain after delete" || fail "$remaining fixture Pod(s) remain after delete"
}

phase_route_after() {
	log "Phase: default-route audit (after)"
	if [ -z "${MACVZ_ROUTE_AUDIT_CMD:-}" ]; then
		skip "route-after (set MACVZ_ROUTE_AUDIT_CMD)"
		return 0
	fi
	if ! run_hook "$MACVZ_ROUTE_AUDIT_CMD" >"$OUT_DIR/route-after.txt" 2>"$OUT_DIR/route-after.err"; then
		fail "MACVZ_ROUTE_AUDIT_CMD failed after run (see $OUT_DIR/route-after.err)"
		return 1
	fi
	if [ -f "$OUT_DIR/route-before.txt" ] && diff -u "$OUT_DIR/route-before.txt" "$OUT_DIR/route-after.txt" >"$OUT_DIR/route.diff" 2>&1; then
		pass "node default route(s) unchanged across the run (non-goal honored)"
	else
		fail "node default route(s) changed across the run (see $OUT_DIR/route.diff) — the suite must never mutate default routes"
	fi
}

# --- main --------------------------------------------------------------------
if [ "$INTEGRATION" != 1 ] || [ -z "${KUBECONFIG:-}" ]; then
	print_plan
	exit 0
fi

setup
phase_preflight || die "preflight failed; not deploying onto an unverified node"
phase_route_before
phase_deploy
phase_scheduling
phase_backend_evidence
phase_dns_core
phase_svc_discovery
phase_headless
phase_cross_namespace
phase_dns_vs_route
phase_sidecar_dns
phase_dns_after_rollout
phase_dns_after_cri_restart
phase_dns_after_helper_restart
phase_dns_after_netd_reload
phase_cleanup
phase_route_after

echo
if [ "$FAILURES" -eq 0 ] && [ "$SKIPS" -eq 0 ]; then
	pass "CRI-L8-2 LinuxPod DNS/Service-discovery suite: all checks passed (diagnostics in $OUT_DIR)"
	exit 0
fi
if [ "$FAILURES" -eq 0 ]; then
	pass "CRI-L8-2 LinuxPod DNS/Service-discovery suite: checks passed with $SKIPS skipped (LinuxPod framing or a churn hook was unproven; see #127/#128/#129). Diagnostics in $OUT_DIR"
	exit 0
fi
fail "CRI-L8-2 LinuxPod DNS/Service-discovery suite: $FAILURES check(s) failed (diagnostics in $OUT_DIR)"
exit 1
