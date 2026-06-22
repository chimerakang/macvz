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

**Short live in-loop runs passed for the supported workload class.** On
2026-06-23, the fixture passed scheduling, rollout, logs, exec, port-forward,
ClusterIP Service reachability from a Linux-node probe, `macvz-cri` restart
recovery, kubelet restart recovery, bounded short soaks, cleanup, and host
orphan audit on the local two-Mac lab. The default adapter path passed first
(`MACVZ_HANDOFF=0`), then the experimental handoff-aware path passed with
`MACVZ_HANDOFF=1` and `identityVerified=true`. A multi-day soak is still pending.
The CRI route-two decision **remains no-go for replacement** until #82
multi-container support is resolved and the longer operator run is complete.

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
- experimental handoff evidence when enabled: if the runtime-private identity
  file and handoff mount are present, the fixture copies the staged rootfs
  identity into `/run/macvz/handoff/identity` before starting `httpd`.

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

## Run evidence

Short live run:

```sh
KUBECONFIG=$HOME/.kube/config \
MACVZ_INTEGRATION=1 \
MACVZ_NODE=macvz-b-cri \
MACVZ_CRI_OUT_DIR=/tmp/cri-inloop-restartk3s-20260623040202 \
MACVZ_INLOOP_SOAK_ITERATIONS=10 \
MACVZ_INLOOP_SOAK_INTERVAL=5 \
MACVZ_HOST_AUDIT_CMD="ssh test@192.168.1.122 '/opt/homebrew/bin/container list --all'" \
MACVZ_ADAPTER_RSS_CMD="ssh test@192.168.1.122 \"ps -axo rss,command | awk '/[m]acvz-cri --listen unix:\\/\\/\\/Users\\/test\\/macvz-cri-i5-test\\/service-default\\/macvz-cri.sock/ {print \\$1; exit}'\"" \
MACVZ_RESTART_CRI_CMD="ssh test@192.168.1.122 'launchctl kickstart -k gui/501/io.macvz.cri.default'" \
MACVZ_RESTART_K3S_CMD="docker exec macvz61-control-plane sh -lc '<restart macvz-b-cri kubelet from /tmp/macvz-cri-inloop/kubelet.pid>'" \
bash test/e2e/cri-k3s/k3s-inloop.sh
```

Result:

```text
PASS CRI-P9 in-loop suite: checks passed with 1 skipped hook-dependent phase(s)
diagnostics: /tmp/cri-inloop-restartk3s-20260623040202
```

### Build under test

| Item | Value |
| --- | --- |
| Commit | working tree based on `7f28326` |
| `macvz-cri` version | `7f28326-dirty` |
| k3s / kubelet version | Kubernetes `v1.35.0` |
| `apple/container` version | `1.0.0_1` |
| Test image | `busybox:1.36.1` arm64 |

### Hosts

| Role | Detail |
| --- | --- |
| Linux control plane | local kind node `macvz61-control-plane` |
| macOS CRI node | `test@192.168.1.122`, node `macvz-b-cri` |

### Acceptance checklist

| # | Acceptance criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | k3s/kubelet schedules the fixture onto the MacVz node | `PASS` | `pod-events.log`, `kubectl get pod -o wide` |
| 2 | Fixture uses #84 node selection + toleration intentionally | `PASS` | `fixtures/workload.yaml` |
| 3 | `kubectl rollout status` succeeds | `PASS` | `rollout.log` |
| 4 | `kubectl logs` returns the boot marker | `PASS` | logs phase |
| 5 | `kubectl exec` reads projected Secret + ConfigMap | `PASS` | `exec.out` |
| 6 | `kubectl port-forward` + curl returns the served marker | `PASS` | `pf.log` |
| 7 | ClusterIP Service reachable from a Linux-node probe | `PASS` | `probe.log` |
| 8 | Restarting `macvz-cri` keeps the Pod (no dup/loss) | `PASS` | `restart-cri.log`, host audit |
| 9 | Restarting k3s/kubelet keeps the Pod (no orphan) | `PASS` | `restart-k3s.log` |
| 10 | Soak: bounded adapter RSS, no crash loop | `PASS` | `soak-samples.csv` |
| 11 | Final host audit: no stale `macvz-cri-*` workloads | `PASS` | `cleanup.log`, host audit |

### Soak summary

| Metric | Value |
| --- | --- |
| Duration / samples | 10 samples at 5s interval |
| First / last adapter RSS | `27840 KB` / `27968 KB` |
| RSS growth (bound 64 MiB) | `128 KB` |
| Pod restartCount over soak | `0` |
| Residual host workloads at end | `0` |

### Handoff-Aware Run

The default CRI adapter was temporarily restarted with:

```text
--experimental-handoff --handoff-root /Users/test/macvz-cri-i5-test/service-default-handoff
```

After the run, those temporary flags were removed and the default adapter was
restarted back on the non-handoff path. The macOS default route remained
`192.168.1.1` via `en0` before and after the test.

```sh
KUBECONFIG=$HOME/.kube/config \
MACVZ_INTEGRATION=1 \
MACVZ_NODE=macvz-b-cri \
MACVZ_HANDOFF=1 \
MACVZ_CRI_OUT_DIR=/tmp/cri-inloop-handoff-20260623041435 \
MACVZ_INLOOP_SOAK_ITERATIONS=10 \
MACVZ_INLOOP_SOAK_INTERVAL=5 \
MACVZ_HANDOFF_STATUS_CMD="<crictl inspect through /run/macvz-cri-default.sock>" \
bash test/e2e/cri-k3s/k3s-inloop.sh
```

Result:

```text
PASS CRI-P9 in-loop suite: all checks passed
diagnostics: /tmp/cri-inloop-handoff-20260623041435
```

Handoff diagnostics:

```text
handoffPrepared=true
identityVerified=true
expectedIdentity=macvz-rootfs-id=macvz-cri-66f891c7889567332eccb97d
observedIdentity=macvz-rootfs-id=macvz-cri-66f891c7889567332eccb97d
identitySource=handoff
handoffWritePolicy=world-writable-fallback
```

Handoff soak summary:

| Metric | Value |
| --- | --- |
| Duration / samples | 10 samples at 5s interval |
| First / last adapter RSS | `28160 KB` / `28480 KB` |
| RSS growth (bound 64 MiB) | `320 KB` |
| Pod restartCount over soak | `0` |
| Residual host workloads at end | `0` |

## Decision impact

This report answers issue #85's central question: *does the supported CRI
workload class — single-container, non-host-namespace Pods — work when scheduled
by Kubernetes, not only when driven by `crictl`?*

- A **clean operator run** (all boxes PASS) clears CRI-P9 gate 3's in-loop
  portion for the supported class. It does **not** flip route two to **go**:
  gate 1 (multi-container Pods, #82) remains blocked on a missing `apple/container`
  shared-netns primitive. Per the issue's own acceptance text, *until #82 is
  unblocked and this issue passes, the answer remains no-go for replacement.*
- The short live run clears the basic in-loop smoke for the supported
  single-container class, but the route-two decision is unchanged until #82 and
  the longer operator run are complete.

See [CRI_FEASIBILITY.md](CRI_FEASIBILITY.md) "CRI-P9 Follow-up (#85)" for how
this fits the full decision package, and
[CRI_SOAK_REPORT_2026-06-21.md](CRI_SOAK_REPORT_2026-06-21.md) for the #83 socket
soak it builds on.
