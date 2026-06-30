# CRI-L8-5 Node Reboot / Bootstrap Recovery for LinuxPod k3s (#144)

Date: 2026-06-30
Parent: #141 (CRI-L8 k3s compatibility hardening) · Siblings: #142 (DNS),
#143 (image lifecycle), #145 (volume projection), #146 (overnight soak),
#147 (conformance smoke)
Outcome: **`linuxpodRebootRecoveryHarnessLanded`** — a scripted, repeatable
recovery check that proves the test Mac can reboot or restart the node service
stack and return the LinuxPod-backed k3s node to a known-good `Ready` state
without manual cleanup of kubelet/CRI/helper/netd leftovers, integrated with the
existing `MACVZ_INTEGRATION=1` gated e2e conventions and the #130 honesty gate.

## What this is (and is not)

This is the **recovery sibling** of `linuxpod-soak.sh` (CRI-L6-1 #135). Where the
soak loops service-level churn while the rest of the stack stays up, this harness
proves a *full restart of the node stack* — a remote reboot or an ordered
service-stack restart — returns the node to `Ready` and the workload to a usable
Pod, with zero stale leftovers and the host default route preserved.

The shipped Virtual Kubelet / `apple/container` path is untouched. This harness
is an isolated `develop`-track feasibility probe and must never gate the VK
release path. It never mutates the host default route; the route guard exists to
*prove* that non-goal is honored.

## Harness

- Script: `test/e2e/cri-k3s/node-reboot-recovery.sh` (`make cri-linuxpod-reboot`).
- Reference bootstrap hook: `test/e2e/cri-k3s/hooks/node-bootstrap.sh`.
- Fixture: `test/e2e/cri-k3s/fixtures/linuxpod-workload.yaml` (the #130 app +
  late-sidecar Pod, reused as the known-good baseline to recover to).
- Gating: runs live only when `MACVZ_INTEGRATION=1` **and** a reachable
  `KUBECONFIG` are set. Otherwise it prints the runbook plan and exits 0, so it
  is safe under `go test`-style CI and `bash -n`.

## Documented startup order

The recovery contract — the order `MACVZ_BOOTSTRAP_CMD` must bring the stack up
in, and the order `MACVZ_STARTUP_PROBE_CMD` is expected to report:

1. **apple/container** — per-user container system (`container system start`).
2. **macvz-netd** — privileged network helper (pf/route/wg); up before the
   adapter so podNetwork rules can be applied. It is a launchd system service
   that returns on boot; the bootstrap hook only waits for its socket, it does
   not relaunch it.
3. **linuxpod-helper** — the #139 router. Per-Pod `supervise-pod` supervisors do
   not survive a host reboot, so journaled-but-dead pods come back as *lost* and
   are recreated, never falsely re-adopted.
4. **macvz-cri** — the adapter (`--experimental-linuxpod-backend`).
5. **kubelet / k3s** — the agent pointed at the adapter endpoint; node `Ready`.
6. **kind socket forward** — the test-topology forward the local kind control
   plane uses to reach the remote adapter (see `README.md`).

## Recovery scenarios

`MACVZ_RECOVERY_SCENARIOS` (default `services,reboot`), in order:

- **services** — restart the whole node service stack in startup order via
  `MACVZ_BOOTSTRAP_CMD` *without* rebooting the host. The node must return
  `Ready` and the workload must come back usable. A soft restart may preserve the
  live Pod VM via the #139 supervisor-backed adoption path; that is accepted but
  not required.
- **reboot** — reboot the remote Mac via `MACVZ_REBOOT_CMD` (which must block
  until the host is reachable again), then bring the stack up via
  `MACVZ_BOOTSTRAP_CMD`. A reboot does not preserve live Pod VMs, so a **fresh**
  healthy Pod — not a re-adopted one — is the expected and accepted outcome. What
  must *not* happen is stale helper sockets, supervisor journals, VM state, or
  kubelet sandbox records blocking that fresh run.

## Acceptance assertions

Per scenario the harness asserts:

- **node returns Ready** — the MacVz CRI node reports `Ready=True` after bootstrap.
- **workload usable** — a Running Pod serves the marker via `kubectl exec` and
  shows the boot markers in `kubectl logs` (Running-but-dead does not pass).
- **no stale leftovers** — `MACVZ_STALE_STATE_CMD` settles to zero residual lines
  (helper sockets, supervisor journals, VM state, kubelet sandbox records). The
  socket/journal/kubelet-sandbox portion is kubelet/CRI-visible regardless of
  backend; the LinuxPod-VM / supervisor portion is only an enforced claim once
  the honesty gate confirms a genuine non-simulated backend.
- **route guard** — `MACVZ_ROUTE_AUDIT_CMD` is captured before and after each
  scenario; the harness asserts the default route is unchanged from baseline AND
  still names the expected gateway/interface (`MACVZ_EXPECTED_DEFAULT_GW` via
  `MACVZ_EXPECTED_DEFAULT_IF`, default **`192.168.1.1` via `en0`**), before and
  after recovery and end-to-end.

A scenario whose required hook is unset (`MACVZ_BOOTSTRAP_CMD` for any,
`MACVZ_REBOOT_CMD` for `reboot`) is dropped with a **loud skip**, never a silent
pass. Failure output is tagged with the exact failed phase and points at
per-scenario diagnostics paths.

## Honesty gate (inherited from #130/#135)

A Pod reaching Running on a `--experimental-linuxpod-backend` node is NOT by
itself evidence of a LinuxPod-backed Pod: the shipped serving path runs on
apple/container, and the R17 prototype helper reports `simulated=true`. So the
LinuxPod-specific recovery claim (the stale LinuxPod-VM / supervisor audit
reaching zero) is only enforced once `MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD` proves
on the node that the Pod's sandbox is served by a genuine, non-simulated LinuxPod
backend. Absent that proof the harness still runs the kubelet-visible recovery
(node Ready, workload usable, socket/sandbox stale audit, route guard) but marks
the LinuxPod-VM portion as not enforced with the #127/#128/#129 blocker.

## Operator hooks

The harness cannot reach the remote macOS node itself; recovery is driven through
hooks (commands run via `sh -c`):

| Hook | Required for | Purpose |
| --- | --- | --- |
| `MACVZ_BOOTSTRAP_CMD` | any scenario | Bring the stack up in startup order, cleaning stale state first, preserving the default route. `hooks/node-bootstrap.sh` is a reference impl. |
| `MACVZ_REBOOT_CMD` | `reboot` | Reboot the remote Mac and block until reachable again. |
| `MACVZ_STARTUP_PROBE_CMD` | optional | Print per-component readiness in startup order; asserted all-ready. |
| `MACVZ_STALE_STATE_CMD` | the no-leftover claim | Print residual stale state lines; asserted to settle to zero. |
| `MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD` | the LinuxPod-VM claim | Prove the Pod is a genuine non-simulated LinuxPod backend. |
| `MACVZ_ROUTE_AUDIT_CMD` | the route guard | Print the node default route(s). |

The reference `hooks/node-bootstrap.sh` delegates helper/adapter bring-up to the
existing `hooks/linuxpod-helper-restart.sh` and `hooks/linuxpod-cri-restart.sh`
so the startup topology stays single-sourced. Its stale-state cleanup is safe and
scoped: it removes only *unbound* macvz-cri / linuxpod-helper sockets and clears
supervisor journals whose `supervise-pod` process is gone, and it never uses
`sudo`, `route`, or `pfctl` or touches default routes.

## Example invocation

```sh
export KUBECONFIG=/path/to/k3s.yaml
MACVZ_INTEGRATION=1 \
  MACVZ_RECOVERY_SCENARIOS=services,reboot \
  MACVZ_CRI_SSH_TARGET=test@192.168.1.122 \
  MACVZ_HELPER_SSH_TARGET=test@192.168.1.122 \
  MACVZ_BOOTSTRAP_SSH_TARGET=test@192.168.1.122 \
  MACVZ_BOOTSTRAP_CMD="./test/e2e/cri-k3s/hooks/node-bootstrap.sh" \
  MACVZ_REBOOT_CMD="ssh test@192.168.1.122 'sudo shutdown -r now' >/dev/null 2>&1 || true; \
    until ssh -o ConnectTimeout=5 test@192.168.1.122 true 2>/dev/null; do sleep 5; done" \
  MACVZ_STARTUP_PROBE_CMD="…" \
  MACVZ_STALE_STATE_CMD="…" \
  MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD="…" \
  MACVZ_ROUTE_AUDIT_CMD="ssh test@192.168.1.122 'route -n get default'" \
  ./test/e2e/cri-k3s/node-reboot-recovery.sh
```

## Validation

- `bash -n` + `shellcheck -S warning` clean on `node-reboot-recovery.sh` and
  `hooks/node-bootstrap.sh`.
- Plan-only mode (no `MACVZ_INTEGRATION`/`KUBECONFIG`) prints the runbook and
  exits 0.

## Coverage boundary (what this does NOT cover)

- Control-plane (kind / k3s server) reboot recovery — out of scope; this harness
  recovers the **MacVz CRI node**, not the Linux control plane.
- Deep DNS / Service-discovery matrix — `linuxpod-dns.sh` (CRI-L8-2 #142).
- Volume projection breadth — CRI-L8-3 (#145).
- Image lifecycle / GC / arch handling — CRI-L8-4 (#143).
- Long wall-clock soak — `linuxpod-soak.sh` / CRI-L8-1 (#146).

## Live evidence

Pending a live run on the two-Mac topology (`kind-macvz61` /
`test@192.168.1.122`). The harness is gated and plan-only safe; once a live
recovery run is performed, append the per-scenario record, diagnostics paths, and
the before/after default route (`192.168.1.1` via `en0`) here.
