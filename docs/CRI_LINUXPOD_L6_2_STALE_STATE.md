# CRI-L6-2: Stale LinuxPod CRI state cleanup and diagnostics (#136)

Parent: #134 (CRI-L6 LinuxPod production recovery and node stability).

This document explains how the LinuxPod-backed CRI adapter handles **stale state**
after crashes, partial cleanup, helper restarts, and kubelet retry loops, and how
to read the residual-state diagnostic it ships.

It is scoped to the experimental LinuxPod backend (`--experimental-linuxpod-backend`).
The shipped Virtual Kubelet / apple-container path is untouched.

## Fail-fast / recreate policy

The adapter supports an opt-in adoption pass (#138), and the real
`linuxpod-helper` now persists a durable journal so it can report per-pod adoption
outcomes instead of `Unsupported`. True VM-handle reattachment still depends on a
Containerization lookup/reattach hook, so the production-facing fallback remains:
MacVz does **not** keep a Pod "Running" across a backend that has lost it. It fails
fast and lets kubelet recreate:

1. **Backend reconciliation.** A background reconciler (and every status read)
   probes each `Ready` sandbox against the helper with `PodStatus`. If the helper
   answers "no such pod" (`ErrPodNotFound`/`ErrContainerNotFound`), the sandbox's
   live backend is gone.
2. **BackendLost marking.** The adapter marks every still-`Running` container in
   that sandbox `Exited` with reason **`BackendLost`** and the sandbox `NotReady`.
   This turns an otherwise stale "Running-but-unusable" view (where `exec`/`logs`
   would only fail later) into an honest CRI state immediately.
3. **Bounded recreate.** kubelet observes the `NotReady` sandbox / `Exited`
   containers and recreates the Pod. On the recreate `RunPodSandbox` call, the
   adapter **discards** the stale `NotReady` record for that Pod
   (`namespace/name`) — detaching its host network path, deleting its container
   records, and running an idempotent backend `Cleanup` — before creating the new
   sandbox. A stale `NotReady` record therefore never blocks replacement.

This is the policy the live CRI-L5 evidence exercised (commit `888ce30`); CRI-L6-2
hardens its diagnosability and proves the stale-state matrix in unit tests.

### How to interpret `BackendLost`

`BackendLost` on a container means: *the helper no longer has live backend state
for this sandbox, so the container cannot be running; kubelet should recreate the
Pod.* It is expected after a `linuxpod-helper` restart and is **not** a data-plane
error. It is distinct from `IdentityVerificationFailed` (a rootfs identity
mismatch at start) and from the network failure classes
(`helper`/`ip-reservation`/`address-discovery`/`route-pf`).

## Pod IP stability across recreation

Pod IPs are keyed by Kubernetes Pod identity (`namespace/name`), not by per-attempt
sandbox ID. The reservation is released **only** at `RemovePodSandbox`, never on
`BackendLost`/`StopPodSandbox` or on the `NotReady`-discard recreate path. So a Pod
that is recreated after `BackendLost` keeps the same Pod IP: the discard retains the
IPAM reservation and the recreate's `Allocate(namespace/name)` returns the same
address (`PodIPAM.Allocate` is idempotent per key).

## Cleanup never touches unrelated host routes

Stale-state cleanup is always scoped to the affected Pod key:

- `detachSandboxNetwork` and `releaseSandboxIP` act on a single `namespace/name`
  key; reconciling or discarding one sandbox never detaches another sandbox's rule
  or releases its IP.
- The host **default route** is never modified: `podnet.Router` only ever removes
  apple/container's scoped (`-ifscope`) vmnet default route, asserted by
  `pkg/network/podnet` default-route tests and re-asserted at the CRI level by
  `TestLinuxPodDiagnoseReconcileDetachesOnlyLostSandbox`.

## Residual-state diagnostic

`macvz-cri --diagnose-linuxpod` scans the persisted sandbox/container stores and
prints a machine-readable JSON report to stdout, then exits without serving. It is
**read-only**: it never mutates a record, an IP reservation, or a host route.

```
macvz-cri --diagnose-linuxpod \
  --state-dir /var/lib/macvz/cri \
  --linuxpod-helper-socket /run/macvz/linuxpod-helper.sock
```

- With `--linuxpod-helper-socket`, the CLI first handshakes with the helper and
  verifies the NDJSON protocol version, then probes each `Ready` sandbox against
  the live helper so backend liveness is authoritative. A stale/mismatched helper
  fails the diagnostic with actionable protocol guidance rather than producing an
  ambiguous report.
- Without it, sandbox liveness is reported honestly as **unprobed** rather than
  guessed (`backendProbed: false`).

The same classification is available in-process while serving via
`LinuxPodService.Diagnose`.

### Report shape

```json
{
  "generatedAt": 1750000000000000000,
  "backendProbed": true,
  "networkEnabled": false,
  "sandboxes": [
    {
      "sandboxID": "…",
      "namespace": "default",
      "name": "web",
      "uid": "…",
      "state": "Ready",
      "category": "sandbox-ready-backend-lost",
      "backendLive": false,
      "network": { "podIP": "10.244.102.2", "attached": true, "ipReservedForKey": true },
      "containerIDs": ["…"]
    }
  ],
  "containers": [
    {
      "containerID": "…",
      "sandboxID": "…",
      "name": "web",
      "state": "Running",
      "category": "container-running-backend-lost",
      "mounts": [
        { "hostPath": "/var/lib/kubelet/.../token", "containerPath": "/var/run/secrets/...", "hostPathExists": true }
      ]
    }
  ],
  "summary": { "sandbox-ready-backend-lost": 1, "container-running-backend-lost": 1 }
}
```

### Machine-readable categories

Sandbox categories:

| Category | Meaning | Operator action |
| --- | --- | --- |
| `sandbox-ready-backend-live` | Ready, helper still backs it | none (healthy) |
| `sandbox-ready-backend-lost` | Ready record but helper lost it | recreate; the reconciler will mark it `NotReady` (`BackendLost`) |
| `sandbox-ready-backend-unprobed` | Ready, no helper socket given | re-run with `--linuxpod-helper-socket` to confirm liveness |
| `sandbox-ready-backend-error` | Ready, helper probe failed after a successful CLI handshake | check the helper; liveness indeterminate |
| `sandbox-notready-retained` | stopped/lost record retained | none; discarded on `RemovePodSandbox` or next same-Pod recreate |

Container categories:

| Category | Meaning |
| --- | --- |
| `container-running-backend-live` | Running in a backend-live sandbox (healthy) |
| `container-running-backend-lost` | recorded Running but sandbox backend is gone; reconciler marks it `Exited`/`BackendLost` |
| `container-running-backend-unprobed` | recorded Running but no helper socket was given, so backend liveness is unknown |
| `container-running-backend-error` | recorded Running but the helper probe failed for a non-missing-backend reason; liveness is indeterminate |
| `container-created-retained` | created but never started |
| `container-exited-retained` | exited record retained until removal |
| `container-orphaned` | container record whose owning sandbox record is gone (partial removal) — a removal retry should reap it |

`mounts[].hostPathExists` flags whether a materialized mount source is still present
on the host, so leftover materialized-mount state after a partial teardown is
visible.

## Evidence

Hermetic unit tests in `pkg/criserver`:

- `TestLinuxPodDiagnoseReadyBackendLive` — healthy Ready + Running classification.
- `TestLinuxPodDiagnoseBackendLost` — missing-backend Ready/Running classification,
  and the read-only guarantee (records are **not** reconciled by the scan).
- `TestLinuxPodDiagnoseNotReadyRetained` — stopped sandbox classification.
- `TestLinuxPodDiagnoseOrphanedContainer` — partially-removed (orphaned) container.
- `TestLinuxPodDiagnoseUnprobedWithoutBackend` — honest unprobed liveness.
- `TestLinuxPodDiagnoseMaterializedMounts` — present/absent mount-source reporting.
- `TestLinuxPodDiagnoseDoesNotTouchNetwork` — the scan changes no route and releases
  no IP, even for a backend-lost networked sandbox.
- `TestLinuxPodDiagnoseReconcileDetachesOnlyLostSandbox` — reconciling one lost
  sandbox leaves an unrelated sandbox's rule and IP intact.
- `TestLinuxPodPodIPStableAcrossBackendLostRecreate` — Pod IP stays stable across a
  `BackendLost` → recreate cycle.

CLI driver test in `cmd/macvz-cri`:

- `TestRunLinuxPodDiagnose` — the `--diagnose-linuxpod` path loads the stores and
  emits valid, classified JSON.
- `TestRunLinuxPodDiagnoseWithHelperSocketHandshakesAndProbes` — a helper socket
  enables a protocol-checked live probe.
- `TestRunLinuxPodDiagnoseRejectsHelperProtocolMismatch` — a stale helper version
  fails before any ambiguous JSON report is emitted.
