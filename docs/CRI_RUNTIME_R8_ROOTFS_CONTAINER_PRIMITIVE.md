# CRI-R8 vminitd Rootfs/Container Primitive Design (#100)

Date: 2026-06-22 UTC

## Decision

MacVz needs an upstream-compatible LinuxPod/vminitd primitive that prepares a
container rootfs in vminitd-visible state, registers the container from that
rootfs, starts the init process, and cleans up partial state on failure.

The next implementation should be both:

- a **local experimental fork/patch** against the apple/containerization checkout
  to prove the state machine with a gated probe; and
- an **upstream proposal** that keeps the API shape small enough to fit
  LinuxPod/vminitd instead of becoming a MacVz-only runtime.

MacVz should not wire this into production CRI code until the primitive is
proven by a live R9 probe.

## Why This Primitive Exists

R6 proved that `vminitd` already has a lower-level new-container branch:

- `containerID=nil` is rejected.
- `containerID=<existing>` is exec-like and ignores OCI `root.path`.
- `id == containerID` with a new ID creates a new `ManagedContainer` from the
  full OCI spec.

R7 closed the obvious shortcut. Staging rootfs data through a keeper utility
container and then addressing it through `/run/container/utility/rootfs/...`
still reached `createProcess`, but `vmexec run` failed with
`No such file or directory`.

So the missing piece is not another path rewrite. The missing piece is a
rootfs/container lifecycle primitive owned by the same state model that
`vminitd` uses to create and start container init processes.

## Current State Model

### LinuxPod

`LinuxPod.addContainer` has two modes:

- before `pod.create()`: record a `PodContainer` as registered, then let
  `LinuxPod.create` include the rootfs in `mountsByID`;
- after `pod.create()`: call VM hotplug, ask the agent to mount the rootfs at
  `/run/container/<id>/rootfs`, register mounts, configure DNS/hosts, then mark
  the container created.

`LinuxPod.startContainer` generates an OCI spec with
`root.path = /run/container/<id>/rootfs`, transforms non-rootfs mounts, creates a
`LinuxProcess`, and calls VM-agent `createProcess/startProcess`.

The important boundary is that LinuxPod owns host-side attachment/mount
bookkeeping, while vminitd owns in-guest process/container state.

### vminitd

`Server+GRPC.createProcess` has three relevant branches:

| Request shape | Current behavior |
| --- | --- |
| no `containerID` | rejected with `processes in the root of the vm not implemented` |
| existing `containerID` | exec path; only OCI `process` is preserved |
| new `containerID` and `id == containerID` | create `ManagedContainer` from the full OCI spec |

`ManagedContainer` creates an OCI bundle under `/run/container/<id>`, creates the
cgroup, and creates a `ManagedProcess`.

`ManagedProcess` starts container init with:

```text
vmexec run --bundle-path /run/container/<id>
```

`vmexec run` loads the bundle, prepares `root.path`, applies mounts, pivots
root, and then executes the configured process.

## Required State Transition

The primitive must support kubelet's normal ordering:

```text
RunPodSandbox
  -> VM and vminitd are already running
CreateContainer
  -> rootfs becomes available inside that running VM
  -> container state is created but not necessarily started
StartContainer
  -> container init process starts from that rootfs
```

The required internal state transition is:

1. **Allocate rootfs slot**
   - Reserve container ID.
   - Reserve rootfs handle.
   - Choose target path, normally `/run/container/<id>/rootfs`.
   - Reject duplicate live or prepared IDs.

2. **Expose rootfs**
   - Host attaches an existing rootfs artifact or transfers/unpacks rootfs data.
   - vminitd can see the target path in the namespace where `vmexec run` will
     resolve `root.path`.
   - The guest verifies the rootfs has the expected marker or image identity.

3. **Create container state**
   - Generate or accept the OCI spec.
   - Ensure `spec.root.path` is the prepared target path or opaque rootfs handle.
   - Create the OCI bundle.
   - Create cgroup state.
   - Insert the container into vminitd state only after rootfs preparation and
     bundle/cgroup creation are coherent.

4. **Start**
   - Start `vmexec run --bundle-path`.
   - Record pid and transition to started.
   - If start fails, either leave a recoverable created container or roll back
     according to the requested policy.

5. **Cleanup**
   - Stop process if running.
   - Delete bundle.
   - Remove cgroup.
   - Unmount/detach rootfs.
   - Remove prepared rootfs handle.
   - Release LinuxPod/host attachment state.

## Proposed Primitive

The smallest useful primitive is a two-stage API:

### `PrepareContainerRootfs`

Host to vminitd.

Request:

| Field | Purpose |
| --- | --- |
| `podID` | Pod VM identity for diagnostics and cgroup path validation |
| `containerID` | New container ID to reserve |
| `rootfsSource` | Rootfs source descriptor: attached mount, NBD endpoint, virtiofs tag, or upload/unpack token |
| `targetPath` | Optional; default `/run/container/<containerID>/rootfs` |
| `readonly` | Desired OCI root readonly behavior |
| `requestID` | Idempotency and retry correlation |
| `expectedIdentity` | Optional marker/digest/UUID to verify before success |

Success:

| Field | Purpose |
| --- | --- |
| `rootfsHandle` | Opaque handle for the prepared rootfs |
| `rootfsPath` | vminitd-visible path to use as OCI `root.path` |
| `identity` | Observed marker/digest/UUID |
| `cleanupToken` | Token for safe cleanup/retry |

Failure boundaries:

- duplicate container ID;
- source unavailable;
- attach/mount failed;
- identity mismatch;
- unsupported source type;
- cleanup after partial mount failed.

### `CreateContainer`

Host to vminitd.

Request:

| Field | Purpose |
| --- | --- |
| `containerID` | Must match a prepared or inline rootfs target |
| `rootfsHandle` | Handle returned by `PrepareContainerRootfs`, or inline rootfs source if one-shot is supported |
| `ociSpec` | Full OCI spec; root path is either omitted or must match the handle path |
| `stdio` | Initial stdio ports |
| `ociRuntimePath` | Optional runtime path, matching existing process API |
| `start` | Optional bool; false maps to CRI `CreateContainer`, true is a one-shot PoC convenience |
| `rollbackOnStartFailure` | Whether failed start removes container/rootfs state |

Success:

| Field | Purpose |
| --- | --- |
| `containerID` | Registered container ID |
| `pid` | Present only if `start=true` |
| `state` | `created` or `started` |

Failure boundaries:

- missing rootfs handle;
- OCI spec invalid;
- root path mismatch;
- bundle creation failed;
- cgroup creation failed;
- start failed;
- rollback failed.

## State Machine

```text
Absent
  -> PreparingRootfs
  -> RootfsPrepared
  -> CreatingContainer
  -> Created
  -> Starting
  -> Started
  -> Exited
  -> Removing
  -> Absent
```

Failure transitions:

| From | On failure | Required action |
| --- | --- | --- |
| `PreparingRootfs` | mount/copy/verify failed | unmount/detach/delete partial rootfs, return precise error |
| `RootfsPrepared` | create canceled or timed out | keep handle for bounded retry or remove by policy |
| `CreatingContainer` | bundle/cgroup failed | delete partial bundle/cgroup, keep or remove rootfs by policy |
| `Starting` | `vmexec run` failed | if rollback requested, delete process/bundle/cgroup and remove rootfs; otherwise leave `Created` with start error |
| `Started` | process exits | record exit code/time, keep rootfs until remove |
| `Removing` | cleanup partially failed | report recoverable cleanup state through `ListState` |

Idempotency:

- `requestID` retries for `PrepareContainerRootfs` must return the same handle
  if the existing prepared state matches the request.
- A conflicting retry must fail with a conflict error.
- `CreateContainer` for an existing `Created` container should be idempotent
  only when the request matches the recorded rootfs/spec identity.

## Error Mapping

MacVz should preserve these boundaries when adapting to CRI:

| Primitive error | CRI-facing behavior |
| --- | --- |
| duplicate container ID | `AlreadyExists` |
| rootfs source unavailable | `NotFound` or `Unavailable`, depending on source |
| identity mismatch | `InvalidArgument` or image unpack failure |
| unsupported source type/capability | `Unimplemented` |
| bundle/cgroup creation failed | `Internal` with cleanup detail |
| start failed | `Internal` for `StartContainer`; container status should show created/failed according to rollback policy |
| cleanup incomplete | operation error plus state visible through diagnostics/recovery |

## Candidate Implementation Options

### Option A: New vminitd Rootfs + Container API

Add explicit vminitd RPCs similar to `PrepareContainerRootfs`,
`CreateContainer`, `StartContainer`, `RemoveContainer`, and `ListContainers`.

Pros:

- owns rootfs preparation in the same guest state model as `ManagedContainer`;
- gives kubelet `CreateContainer` and `StartContainer` honest lifecycle points;
- creates clear recovery and cleanup semantics;
- avoids relying on utility-container exec side effects.

Cons:

- requires upstream API design;
- needs new guest-side storage/rootfs code;
- requires capability negotiation because apple/containerization is pre-1.0.

Recommendation: **preferred design**.

### Option B: Extend LinuxPod post-create `addContainer`

Make current post-create `addContainer` fully reliable by upstreaming the
missing host/guest attachment path and ensuring LinuxPod state, VM mount state,
and vminitd state are updated together.

Pros:

- fits existing LinuxPod API shape;
- minimal impact on users who already call `addContainer/startContainer`;
- keeps host-side attachment bookkeeping in LinuxPod.

Cons:

- still needs a vminitd-visible rootfs preparation contract under the hood;
- may hide too much state inside LinuxPod for MacVz recovery;
- harder to express CRI's separate create/start/remove transitions.

Recommendation: useful upstream compatibility layer, but not enough alone unless
it exposes recoverable state and precise failure boundaries.

### Option C: Host-Provided Rootfs Attachment Visible to vminitd

Expose rootfs through a host-controlled attachment that appears directly at a
path vminitd can mount, without utility-container exec staging.

Examples:

- predeclared NBD rootfs for VM creation;
- dynamic NBD or virtiofs if upstream adds deterministic runtime attach;
- host-pushed archive unpacked by vminitd into guest storage.

Pros:

- can reuse host image/rootfs pipeline;
- keeps registry credentials and image content outside the guest if desired;
- may be easiest for a local fork PoC.

Cons:

- current public VZ USB path was rejected by R1;
- pre-create NBD does not solve late `CreateContainer`;
- dynamic attachment still needs identity, cleanup, and recovery semantics.

Recommendation: valid transport family, but it must be wrapped by Option A's
state machine.

## Proposed R9 Probe

R9 should be a gated local fork/patch probe, not production MacVz code.

Goal:

- add the smallest experimental vminitd-side rootfs preparation path;
- launch one new container after `pod.create()` from that prepared rootfs;
- prove identity and cleanup.

Suggested shape:

1. Add experimental RPC or temporary debug command to vminitd:
   `PrepareContainerRootfs(containerID, requestID, sourcePathOrArchive,
   targetPath)`.
2. Prepare rootfs directly in vminitd-visible state at
   `/run/container/<containerID>/rootfs`.
3. Call the existing new-container `createProcess` branch with
   `id == containerID` and OCI `root.path` set to the prepared path.
4. Start, wait, verify result identity, delete process/container, and remove
   rootfs.

Outcome names:

| Outcome | Meaning |
| --- | --- |
| `vminitdRootfsPrimitiveLaunchSucceeded` | Rootfs prepared in vminitd-visible state and new container started/wrote verified identity |
| `vminitdRootfsPrepareFailed` | Experimental prepare path could not create/verify rootfs |
| `vminitdContainerCreateFailed` | Rootfs prepared, but `createProcess(id == containerID)` failed |
| `vminitdContainerStartFailed` | Container object created, but `vmexec run` failed |
| `vminitdRootfsIdentityMismatch` | Process ran but did not observe expected identity/rootfs |
| `vminitdRootfsCleanupFailed` | Launch result known, but rootfs/container cleanup failed |
| `upstreamPrimitiveRequired` | Local patch cannot model the required state without broader upstream changes |

R9 success does not mean full CRI support. It only proves the missing primitive's
state machine. Production CRI wiring remains blocked until the primitive has a
stable upstream or vendored experimental boundary with capability gating.

## R9 Result

R9 completed on 2026-06-22 UTC. The live probe is published at
[CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md](CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md).

Outcome: `vminitdContainerStartFailed`.

The local experimental shape did **not** modify the apple/containerization
checkout. Instead, the MacVz harness used existing vminitd
`Copy(COPY_OUT/COPY_IN archive)` as a temporary `PrepareContainerRootfs`
transport:

1. copy a minimal busybox rootfs payload out of the utility container rootfs;
2. copy it back to `/run/container/r9-late-alpha/rootfs`;
3. call existing `createProcess(id == containerID)` with OCI `root.path` set to
   that prepared path;
4. call `startProcess`;
5. call `deleteProcess` and verify cleanup.

The prepare step succeeded, `createProcess` created the new vminitd container
object, and cleanup removed the prepared rootfs. The start step failed inside
`vmexec run` with:

```text
NSPOSIXErrorDomain Code=2 "No such file or directory"
```

This is useful negative evidence. The missing primitive is narrower than "copy
files to a vminitd-visible path" but still deeper than MacVz production code:
the next patch should instrument or extend vminitd/vmexec's bundle/rootfs start
path so it can explain or accept a rootfs prepared after Pod VM creation.

## R10 Result

R10 completed on 2026-06-22 UTC. The live diagnostic report is published at
[CRI_RUNTIME_R10_VMEXEC_START_REPORT.md](CRI_RUNTIME_R10_VMEXEC_START_REPORT.md).

Outcome: `vmexecStartFailureExplained`.

The local apple/containerization diagnostic patch rebuilt the Pod VM initfs as
`vminit:macvz-r10` and reran the R9 harness. The instrumented `vmexec` error
ruled out missing bundle, missing rootfs, missing `/bin/sh`, and missing dynamic
linker. The actual failure was:

```text
macvz-r10-errno=1 stage=remove(ptmx) errno=2 strerror=No such file or directory
```

That places the blocker in `vmexec` console `/dev/ptmx` setup after OCI `/dev`
tmpfs and `/dev/pts` devpts mounts are configured. The next useful patch is to
make that step idempotent for ENOENT, then rerun the same harness to see whether
the late prepared rootfs reaches process identity evidence.

## R11 Result

R11 completed on 2026-06-22 UTC. The live probe report is published at
[CRI_RUNTIME_R11_PTMX_PROBE_REPORT.md](CRI_RUNTIME_R11_PTMX_PROBE_REPORT.md).

Outcome: `vmexecPtmxFixAdvancedToExec`.

The local apple/containerization patch treated `remove(ptmx)` ENOENT as
idempotent and rebuilt the Pod VM initfs as `vminit:macvz-r11`. The same
late-rootfs harness no longer failed at `stage=remove(ptmx)`. It advanced to:

```text
macvz-r10-errno=1 stage=open(/dev/null) errno=2 strerror=No such file or directory
```

This confirms the ptmx invariant was real and removable, but process identity is
still not reached. The next blocker is `/dev/null` device setup in the late
prepared rootfs path.

## R12 Result

R12 completed on 2026-06-22 UTC. The live probe report is published at
[CRI_RUNTIME_R12_DEVNULL_PROBE_REPORT.md](CRI_RUNTIME_R12_DEVNULL_PROBE_REPORT.md).

Outcome: `vmexecDevNullFixAdvancedToExec`.

The local apple/containerization patch created `/dev/null` as character device
`1:3` after OCI `/dev` tmpfs setup and before `pivotRoot`. The same
late-rootfs harness no longer failed at `stage=open(/dev/null)`. The process
started and then exited from userspace with code 127:

```text
processStartSucceeded=true
processExitCode=127
```

That advances the research from vmexec device setup into rootfs completeness:
the minimal R9 rootfs has `/bin/sh` and `/bin/busybox`, but no applet symlinks
for commands used by the identity script.

## MacVz Integration Boundary

Until the primitive exists, MacVz should keep production runtime code unchanged.
The integration boundary should be:

- `pkg/runtime` may grow an experimental backend only behind an explicit flag
  after R9 succeeds;
- default tests remain hermetic;
- live tests require an explicit environment variable;
- `README.md` should not advertise kubelet/k3s CRI compatibility from this path;
- Virtual Kubelet provider behavior remains the shipped user-facing path.

Once the primitive exists, MacVz can map CRI as:

| CRI call | Primitive mapping |
| --- | --- |
| `RunPodSandbox` | create Pod VM and wait for vminitd capabilities |
| `CreateContainer` | prepare rootfs, create container state, return container ID only after recoverable created state exists |
| `StartContainer` | start prepared container init process |
| `StopContainer` | signal/wait/kill through vminitd |
| `RemoveContainer` | remove process, bundle, cgroup, rootfs, and host attachment state |
| `ListContainer`/`ContainerStatus` | merge host store with vminitd `ListState` |

## Open Questions

- Should rootfs preparation copy/unpack inside the guest, or should the host
  attach an already-prepared filesystem?
- Should `CreateContainer` and `StartContainer` remain separate upstream calls,
  or should a one-shot helper exist only for tests?
- What persistent state should vminitd keep across guest-agent restart?
- How should rootfs storage quotas be enforced inside a Pod VM?
- Can rootfs source identity reuse existing image digests, filesystem UUIDs, or
  does it need a MacVz-specific marker?
- What is the minimum capability bit MacVz should require before enabling this
  path?

## Completion Criteria for Moving Beyond Design

Before MacVz attempts production CRI integration on this route, the following
must be proven:

- R9 or equivalent live probe returns `vminitdRootfsPrimitiveLaunchSucceeded`.
- Cleanup leaves no stale vminitd container, cgroup, bundle, rootfs path, or
  host attachment.
- Failure at each state-machine stage is observable and retryable.
- The primitive has a stable API boundary: upstream accepted, or locally vendored
  behind an explicit experimental capability gate.
- Documentation clearly states that the path is experimental until upstream/API
  stability is known.
