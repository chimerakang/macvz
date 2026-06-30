# CRI-L8-3 LinuxPod CRI Volume-Projection Matrix (#145)

Date: 2026-06-30
Outcome: **Hermetic PASS**; live k3s matrix **pending a run** on
`test@192.168.1.122` (gated harness in place).

Parent: #141 (CRI-L8 k3s compatibility hardening). Siblings: CRI-L8-2 DNS (#142),
CRI-L8-5 reboot recovery (#144).

## Goal

Prove the LinuxPod-backed CRI path handles the common kubelet-managed volume
types an ordinary k3s workload depends on — `emptyDir`, `configMap`, `secret`,
`projected`, downward API, the service-account token projection, and allowed
`hostPath` — with correct read-only/read-write permission, multi-container
sharing, projected-update behavior, and a clean teardown, while keeping the
existing hostPath allowlist/security posture intact.

## What the CRI adapter actually owns

In CRI mode the **kubelet** (not MacVz) materializes a Pod's ConfigMaps, Secrets,
projected service-account tokens, downward API data, and `emptyDir` storage on
the host filesystem under its pods directory, then passes them to the runtime as
host bind mounts in `CreateContainerRequest.Config.Mounts`. The adapter's
contract is therefore narrow and honest: validate each kubelet-provided mount
against a conservative policy and translate it into the LinuxPod backend's mount
set, never re-projecting content the kubelet already wrote. File **content, modes,
and ownership** are kubelet-owned; the dimensions the adapter controls and is
tested on are: which mounts are admitted (policy), the source/target/read-only
mapping, the Memory-`emptyDir` → guest-tmpfs translation, and that each realized
mount reaches the backend per container (including a shared volume).

The translation is shared with the apple/container path
(`translateMountsWithPolicy` in [`pkg/criserver/mounts.go`](../pkg/criserver/mounts.go)),
then mapped to `linuxpod.Mount` by `linuxpodMounts` and carried in
`linuxpod.CreateRequest.Mounts` (protocol v4+) to the helper
([`pkg/criserver/linuxpod_service.go`](../pkg/criserver/linuxpod_service.go),
[`pkg/runtime/linuxpod/contract.go`](../pkg/runtime/linuxpod/contract.go)).

## Volume matrix

| Volume type | Kubelet source layout | Realized backend mount | Permission |
| --- | --- | --- | --- |
| `configMap` | `…/volumes/kubernetes.io~configmap/<name>` | bind | read-only |
| `secret` | `…/volumes/kubernetes.io~secret/<name>` | bind | read-only |
| `projected` (SA token) | `…/volumes/kubernetes.io~projected/kube-api-access-*` | bind | read-only |
| downward API | `…/volumes/kubernetes.io~downward-api/<name>` | bind | read-only |
| `emptyDir` (disk) | `…/volumes/kubernetes.io~empty-dir/<name>` | bind | read-write |
| `emptyDir` (Memory) | empty `HostPath` | guest tmpfs | read-write |
| `hostPath` (allowed) | operator-allowlisted prefix | bind | as requested |
| `hostPath` (arbitrary) | outside pods dir / allowlist | **rejected** (`FailedPrecondition`) | — |
| reserved `/run/macvz/*` target | any | **rejected** (`FailedPrecondition`) | — |
| bidirectional propagation | any | **rejected** (`FailedPrecondition`) | — |

`hostPath` keeps the existing posture: mounts under the kubelet pods dir are
always allowed; any other host source must fall under a
`--volume-host-path-allowed` prefix (empty by default = arbitrary hostPath
disabled on macOS), prefix matching is path-segment aware, and a mount targeting
the runtime-private `/run/macvz` namespace is rejected regardless of source.

## Hermetic evidence (PASS)

[`pkg/criserver/linuxpod_volumes_test.go`](../pkg/criserver/linuxpod_volumes_test.go)
drives the full matrix through the **LinuxPod-backed** CRI service against the
in-process fake backend, asserting both the persisted `ContainerStatus.Mounts`
and — newly — the **exact mount set received by the backend's `CreateRequest`**
(`FakeBackend.ContainerMounts`, added in
[`pkg/runtime/linuxpod/fake.go`](../pkg/runtime/linuxpod/fake.go)):

- `TestLinuxPodVolumeProjectionMatrix` — lays out configMap, secret, projected
  SA token, downward API, disk `emptyDir`, Memory `emptyDir`, and an allowlisted
  `hostPath` exactly as the kubelet lays them out, then asserts each reaches the
  backend with the right source/target, the read-only volumes are read-only, the
  Memory `emptyDir` is a guest tmpfs (empty source), and all surface in
  `ContainerStatus`.
- `TestLinuxPodVolumeSharedEmptyDirAcrossContainers` — an app and a **late**
  sidecar both mount the same `emptyDir` source; the shared mount reaches the
  backend for **each** container at its own target (multi-container sharing).
- `TestLinuxPodVolumePolicyErrors` — unallowed hostPath, a reserved `/run/macvz`
  target, bidirectional propagation, and relative host/container paths are each
  rejected with the right gRPC code and **no** backend container left behind.

Existing coverage retained: `pkg/criserver/mounts_test.go` (apple/container path
matrix + policy), `pkg/criserver/linuxpod_service_test.go`
`TestLinuxPodServiceTranslatesKubeletMounts`.

```
go test ./pkg/criserver/ ./pkg/runtime/linuxpod/   # PASS
go build ./...   # ok
go vet ./...     # ok
```

## Live k3s evidence (pending)

The gated live harness is in place and validated for syntax (`bash -n`) and
lint (`shellcheck -S warning`):

- Harness: [`test/e2e/cri-k3s/linuxpod-volumes.sh`](../test/e2e/cri-k3s/linuxpod-volumes.sh)
  (`make cri-linuxpod-volumes`)
- Fixture: [`test/e2e/cri-k3s/fixtures/linuxpod-volumes-workload.yaml`](../test/e2e/cri-k3s/fixtures/linuxpod-volumes-workload.yaml)

It schedules an app + late-sidecar Pod (the LinuxPod shared-namespace shape) onto
the MacVz CRI node and, by `kubectl exec` inside the containers, proves:

1. **volume-matrix** — configMap/secret/downward-API content correct, each
   read-only mount proven read-only by a *failed* write probe, Memory `emptyDir`
   writable;
2. **sa-token** — the projected service-account token (token + ca.crt +
   namespace) present at the standard path;
3. **shared-volume** — the disk `emptyDir` visible both ways across the app and
   the sidecar;
4. **projected-update** — a ConfigMap patch propagates into the running Pod
   (kubelet projected-volume update behavior);
5. re-runs the core matrix after **rollout-restart**, **macvz-cri restart** (Pod
   UID preserved), and **LinuxPod helper restart**;
6. **cleanup/residual** — no fixture Pods remain and (via
   `MACVZ_LINUXPOD_RESIDUAL_CMD`) no residual materialized mount/rootfs state
   remains on the node;
7. the host default route is unchanged across the run.

It inherits the #130 **honesty gate**: the volume checks run on either backend,
but the LinuxPod-specific framing is only asserted when
`MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD` proves a genuine, non-simulated LinuxPod
Pod. As with the other CRI-L8 live suites, the live matrix is **blocked on the
CRI-L serving path reporting `simulated=false`** (#127/#128/#129); until then the
harness runs and reports against the apple/container path and skips the LinuxPod
framing loudly. The runbook to execute on `test@192.168.1.122` mirrors the DNS
sibling's hook set (`MACVZ_INTEGRATION=1`, `KUBECONFIG`, `MACVZ_RESTART_CRI_CMD`,
`MACVZ_RESTART_HELPER_CMD`, `MACVZ_LINUXPOD_RESIDUAL_CMD`, `MACVZ_ROUTE_AUDIT_CMD`).

## Non-goals honored

- No dynamic PV provisioning, no StatefulSet/PVC, no `subPath` (out of scope;
  MacVz volumes are ephemeral VirtioFS, see #26/#64).
- No change to the macOS hostPath allowlist/security posture.
- No mutation of the host default route; the shipped Virtual Kubelet and
  apple/container paths are untouched.

## Acceptance status

- [x] Hermetic tests cover CRI-side mount translation and policy errors.
- [ ] Gated/live k3s fixture proves the volume matrix on `test@192.168.1.122`
      (harness ready; blocked on a non-simulated LinuxPod serving path).
- [x] Cleanup audit wired (no stale materialized mount/rootfs state) — hermetic +
      live residual hook.
- [x] Evidence documented here and linked from `docs/MASTER_TASKS.md`.
