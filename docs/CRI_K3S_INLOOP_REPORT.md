# CRI Adapter Real kubelet/k3s In-Loop Report — #85

Issue **#85** (CRI-P9 follow-up). This is the operator runbook and evidence
template for the **missing** CRI-P9 validation: a real Linux **kubelet/k3s
control plane** driving the experimental `macvz-cri` adapter through scheduling,
Pod lifecycle, `kubectl` user flows, Service routing, restart recovery, and a
sustained soak.

It is the in-loop counterpart to the #83 soak report
([CRI_SOAK_REPORT_2026-06-21.md](CRI_SOAK_REPORT_2026-06-21.md)), which
deliberately drove the CRI socket with `crictl` instead of a kubelet. #83 proved
adapter resource-boundedness over the socket; #85 proves the *supported workload
class works when scheduled by Kubernetes*.

It is scoped to the experimental `develop` CRI track and does **not** gate the
shipped Virtual Kubelet path.

## Status

**Harness complete; live operator run pending.** The repeatable harness
(`test/e2e/cri-k3s/k3s-inloop.sh`), the single-container fixture
(`test/e2e/cri-k3s/fixtures/workload.yaml`), and this runbook are committed and
pass `bash -n` / plan-only validation in CI. The **live** run requires a real
topology the dev host cannot stand up unattended — a Linux k3s control plane
plus a macOS host serving `macvz-cri` as that node's external CRI endpoint — so
it is explicitly gated (`MACVZ_INTEGRATION=1` + a reachable `KUBECONFIG`) and
operator-run. The "Run evidence" section below is the template an operator fills
in from a live run; until it is filled and all acceptance boxes are checked, the
CRI route-two decision **remains no-go for replacement** (and #82 multi-container
remains independently blocked regardless).

## Topology

```
              ┌─────────────────────────┐         ┌───────────────────────────────┐
              │ Linux host              │         │ macOS host (Apple Silicon)    │
              │  k3s server / control   │  CRI    │  macvz-cri  ── apple/container │
   kubectl ──▶│  plane + scheduler      │◀────────│  (external CRI endpoint, per-  │
              │  (+ optional k3s agent  │  socket │   user LaunchAgent)            │
              │   for the probe Pod)    │         │  node.macvz.io/runtime=apple-… │
              └─────────────────────────┘         └───────────────────────────────┘
```

- The MacVz node is registered by k3s/kubelet against the adapter socket with the
  #84 labels/taint (`test/e2e/cri-k3s/README.md`, "Pointing k3s at macvz-cri"):
  - `node.macvz.io/runtime=apple-container`
  - `node.macvz.io/host-namespace=unsupported`
  - taint `node.macvz.io/host-namespace-unsupported=true:NoSchedule`
- The fixture both **selects** the runtime label and **tolerates** the taint, so
  the scheduler places it on the MacVz node; the probe Pod does *not* tolerate
  the taint, so it lands on a Linux node — the documented, supported vantage for
  ClusterIP reachability.

## Fixture

`test/e2e/cri-k3s/fixtures/workload.yaml` — the supported workload class only:

- single container (multi-container is blocked by #82),
- no host namespaces (rejected by #84; default Pod netns),
- projected ConfigMap + Secret (read-only mounts),
- an HTTP readiness/liveness probe the kubelet drives,
- a ClusterIP Service.

## Runbook

Prerequisites: a Linux k3s server, a macOS host with `apple/container` and the
adapter installed as a per-user LaunchAgent (`scripts/macvz-cri-install.sh`), and
the node joined with the #84 labels/taint. See `test/e2e/cri-k3s/README.md`.

```sh
# From a machine with kubectl access to the k3s control plane:
export KUBECONFIG=/path/to/k3s.yaml
export MACVZ_INTEGRATION=1
export MACVZ_CRI_KEEP=1                 # preserve diagnostics for this report
export MACVZ_CRI_OUT_DIR=/tmp/cri-inloop

# Operator hooks so the harness can restart/audit the remote macOS node.
# (Adjust the launchd label and ssh target for your environment.)
export MACVZ_RESTART_CRI_CMD="ssh mac 'launchctl kickstart -k gui/\$(id -u)/io.macvz.cri'"
export MACVZ_RESTART_K3S_CMD="ssh mac 'launchctl kickstart -k gui/\$(id -u)/io.macvz.k3s-agent'"
export MACVZ_ADAPTER_RSS_CMD="ssh mac 'ps -o rss= -p \$(pgrep -x macvz-cri)'"
export MACVZ_HOST_AUDIT_CMD="ssh mac 'container list --all'"

# Multi-day soak: ~8640 samples at 10s ≈ 24h; scale MACVZ_INLOOP_SOAK_ITERATIONS
# to the operator-run duration. Shorter runs are acceptable if documented.
export MACVZ_INLOOP_SOAK_ITERATIONS=8640
export MACVZ_INLOOP_SOAK_INTERVAL=10

make cri-k3s-inloop      # or: ./test/e2e/cri-k3s/k3s-inloop.sh
```

The harness phases map 1:1 to the acceptance criteria: preflight → deploy →
scheduling → logs → exec → port-forward → service → restart-cri → restart-k3s →
soak → cleanup. A phase whose operator hook is unset is **skipped loudly**, never
silently passed.

## Run evidence (operator fills in)

> Replace the bracketed placeholders from a live run. Leave a phase marked
> `PENDING` if hardware availability blocked it, with the reason.

### Build under test

| Item | Value |
| --- | --- |
| Commit | `[git describe]` |
| `macvz-cri` version | `[version]` |
| k3s / kubelet version | `[k3s version]` |
| `apple/container` version | `[version]` |
| Test image | `[busybox:1.36.1 arm64]` |

### Hosts

| Role | Detail |
| --- | --- |
| Linux control plane | `[distro, arch, k3s server]` |
| macOS CRI node | `[Mac model, chip, RAM, macOS version]` |

### Acceptance checklist

| # | Acceptance criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | k3s/kubelet schedules the fixture onto the MacVz node | `[PASS/PENDING]` | `pod-events.log`, `kubectl get pod -o wide` |
| 2 | Fixture uses #84 node selection + toleration intentionally | `[PASS]` | `fixtures/workload.yaml` |
| 3 | `kubectl rollout status` succeeds | `[PASS/PENDING]` | `rollout.log` |
| 4 | `kubectl logs` returns the boot marker | `[PASS/PENDING]` | logs phase |
| 5 | `kubectl exec` reads projected Secret + ConfigMap | `[PASS/PENDING]` | `exec.out` |
| 6 | `kubectl port-forward` + curl returns the served marker | `[PASS/PENDING]` | `pf.log` |
| 7 | ClusterIP Service reachable from a Linux-node probe | `[PASS/PENDING]` | `probe.log` |
| 8 | Restarting `macvz-cri` keeps the Pod (no dup/loss) | `[PASS/PENDING]` | `restart-cri.log`, host audit |
| 9 | Restarting k3s/kubelet keeps the Pod (no orphan) | `[PASS/PENDING]` | `restart-k3s.log` |
| 10 | Soak: bounded adapter RSS, no crash loop | `[PASS/PENDING]` | `soak-samples.csv` |
| 11 | Final host audit: no stale `macvz-cri-*` workloads | `[PASS/PENDING]` | `cleanup.log`, host audit |

### Soak summary

| Metric | Value |
| --- | --- |
| Duration / samples | `[e.g. 24h / 8640 samples]` |
| First / last adapter RSS | `[KB] / [KB]` |
| RSS growth (bound 64 MiB) | `[KB]` |
| Pod restartCount over soak | `[n]` |
| Residual host workloads at end | `[0]` |

## Decision impact

This report answers issue #85's central question: *does the supported CRI
workload class — single-container, non-host-namespace Pods — work when scheduled
by Kubernetes, not only when driven by `crictl`?*

- A **clean operator run** (all boxes PASS) clears CRI-P9 gate 3's in-loop
  portion for the supported class. It does **not** flip route two to **go**:
  gate 1 (multi-container Pods, #82) remains blocked on a missing `apple/container`
  shared-netns primitive. Per the issue's own acceptance text, *until #82 is
  unblocked and this issue passes, the answer remains no-go for replacement.*
- Until this report's evidence is filled from a live run, the in-loop validation
  is **pending**, and the route-two decision is unchanged.

See [CRI_FEASIBILITY.md](CRI_FEASIBILITY.md) "CRI-P9 Follow-up (#85)" for how
this fits the full decision package, and
[CRI_SOAK_REPORT_2026-06-21.md](CRI_SOAK_REPORT_2026-06-21.md) for the #83 socket
soak it builds on.
