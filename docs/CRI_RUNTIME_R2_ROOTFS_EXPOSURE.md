# CRI-R2 Rootfs Exposure Fallback Research (#94)

Date: 2026-06-21 UTC

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 arm64
- Swift: Apple Swift 6.2.1
- Apple Containerization checkout: `/tmp/apple-containerization`
- Apple Containerization version: `0.34.0`
- Apple Containerization commit: `6b7b42c`

## Context

#91 proved that a consumer `HotplugProvider` can be installed and called from
post-create `LinuxPod.addContainer(...)`, and that public VZ USB mass-storage
attach can return success.

#93 tested the missing guest-side half directly. Host attach succeeded, but the
guest observed no new USB, SCSI, or block device. The R1 outcome was
`guestCouldNotObserveNewDevice`. That rejects public VZ USB mass-storage
hotplug as the next rootfs attachment primitive for the current
Apple Containerization/LinuxPod environment.

R2 evaluates two fallback families:

- NBD-backed rootfs or volume exposure.
- Guest-side rootfs staging, pull, or unpack.

## Upstream Findings

Apple Containerization already has first-class NBD configuration paths for
pre-create VM storage:

- `Mount.block(...)` accepts `nbd://`, `nbds://`, `nbd+unix://`, and
  `nbds+unix://` sources.
- NBD mounts are converted into
  `VZNetworkBlockDeviceStorageDeviceAttachment`.
- `AttachedFilesystem` assigns virtio-block guest paths deterministically from
  the VM storage attachment order: `/dev/vda`, `/dev/vdb`, and so on.
- `LinuxPod.PodVolume.Source.nbd(...)` maps a named Pod volume to a predeclared
  NBD-backed block mount.
- Upstream integration tests cover container NBD mounts, read-only mounts, raw
  blocks, volume identity, shared Pod NBD volumes, multiple Pod NBD volumes,
  persistence, and concurrent writers.

Important boundary: this is all pre-create configuration. It is not evidence
that VZ can add a new NBD-backed virtio-block device to an already-running Pod
VM. Public VZ dynamic attach remains the USB path that #93 rejected.

## Option Comparison

| Option | Determinism | Complexity | Image lifecycle | Cleanup/retry | Security | Kubelet ordering fit | Recommendation |
| --- | --- | --- | --- | --- | --- | --- | --- |
| VZ USB mass-storage hotplug | Weak in current environment. Host attach succeeds but guest sees no device. | Medium | Reuses ext4 rootfs images. | Detach API exists, but guest cannot observe device. | Host controls image; guest identity absent. | Poor. #93 blocks before mount. | Reject for now. |
| Pre-create NBD rootfs | Strong for VM creation. `AttachedFilesystem` assigns a known `/dev/vd*`; upstream tests prove NBD volumes mount and retain identity. | Medium. Needs NBD server lifecycle and rootfs image export. | Reuses host-side ext4 rootfs images. | Host can stop server; VM teardown releases devices. Retry means recreate VM. | Prefer Unix socket NBD and per-rootfs ownership; TCP needs tight binding/auth. | Partial. Works only before `pod.create()`, so it does not solve late `CreateContainer`. | Best next small PoC. |
| Pre-create NBD Pod volumes | Strong for named shared volumes. Already upstream-tested. | Medium. Similar to rootfs NBD. | Good for Kubernetes volumes, not rootfs by itself. | VM teardown cleanup is straightforward. | Same NBD server concerns. | Useful for volumes; does not solve rootfs ordering. | Keep as supporting evidence. |
| Guest-side NBD client | Potentially strong because the guest initiates by explicit endpoint/token. | High. Needs guest nbd tooling/kernel module and a guest-agent API. | Reuses host ext4 images. | Guest can disconnect/unmount explicitly. | Needs endpoint auth and lifecycle control. | Good if available. | Not selected until kernel/tooling is proven. |
| Guest-side rootfs copy/unpack | Strongest semantic fit. Rootfs identity is created inside the running Pod VM by a request ID, not by host device discovery. | High. Needs guest-agent `PrepareRootfs` contract, storage accounting, unpack, overlay/writable layer, GC, and recovery. | Duplicates or moves image unpack into each Pod VM. | Can be explicit and recoverable if state model is built. | Registry auth/content trust move into guest path or must be proxied. | Best long-term fit for normal kubelet ordering. | Long-term target, but too large for the next tiny PoC. |
| VirtioFS rootfs directory | Avoids block-device identity. | Medium/high. Permission, isolation, overlay, mount propagation, and macOS semantics need care. | Uses unpacked host directory. | Cleanup is host filesystem cleanup. | Larger host filesystem exposure surface. | Partial. Dynamic virtiofs updates may help data, but rootfs semantics remain tricky. | Keep as fallback, not next. |

## Minimal Guest-Agent Contract

A real Pod VM runtime still needs a guest-side API with explicit request IDs and
state transitions. The contract should look roughly like this:

| Operation | Request | Success state | Failure boundaries |
| --- | --- | --- | --- |
| `PrepareRootfs` | container ID, image/rootfs reference, readonly flag, writable-layer policy, request token | rootfs staged and identified by an opaque guest rootfs handle | image/rootfs unavailable, auth failed, unpack failed, storage quota exceeded |
| `MountRootfs` | rootfs handle, target path, mount options | target path mounted and verified | handle missing, mount failed, marker/content mismatch |
| `UnmountRootfs` | rootfs handle or target path | mount removed and not busy | process still using mount, missing mount, transient EBUSY |
| `RemoveRootfs` | rootfs handle | staged data deleted or marked for GC | busy rootfs, partial delete, retry required |
| `ListRootfs` | sandbox ID | current rootfs handles and mount state | agent restart/recovery mismatch |

NBD can implement a subset of this contract if rootfs devices are known before
VM start. Guest-side staging can implement the full contract for post-create
containers.

## Decision

Select **pre-create NBD rootfs identity** as the next tiny PoC because it is the
smallest fallback with concrete upstream support and deterministic guest paths.

This decision is deliberately narrow:

- It does not solve normal kubelet post-create `CreateContainer` ordering.
- It does not resurrect USB hotplug.
- It does not claim full CRI compatibility.
- It tells us whether NBD can replace host disk-image rootfs exposure in the
  pre-create LinuxPod path and provide a stable baseline for later guest-agent
  work.

If the NBD rootfs PoC passes, the next architectural question remains guest-side
rootfs staging for late containers. If it fails, the NBD branch should be
closed and R4 should go directly to guest-side copy/unpack staging.

## Next Issue

CRI-R3 (#95) should implement a gated live probe:

- Serve one or more busybox rootfs ext4 images through an NBD server.
- Use an NBD URL as the `rootfs` for a predeclared LinuxPod container.
- Verify the container starts, reads a marker from its rootfs, and reports the
  backing mount from `/proc/mounts`.
- Verify multiple NBD-backed rootfs images retain identity when two containers
  are predeclared.
- Keep default mode hermetic/plan-only.
- Publish a report under `docs/`.

Expected outcome: either `nbdRootfsPrecreateSucceeded` or a precise failure
boundary such as `nbdServerFailed`, `vzNbdAttachmentFailed`,
`guestRootfsMountFailed`, `containerStartFailed`, or
`rootfsIdentityMismatch`.

## R3 Result

R3 completed on 2026-06-21 UTC. The live probe is published at
[CRI_RUNTIME_R3_NBD_ROOTFS_REPORT.md](CRI_RUNTIME_R3_NBD_ROOTFS_REPORT.md).

Outcome: `nbdRootfsPrecreateSucceeded`.

The probe served two busybox rootfs ext4 images through local NBD Unix sockets,
pre-registered two LinuxPod containers before `pod.create()`, and started both
containers successfully. Guest evidence showed rootfs mounts through virtio
block devices:

- alpha: `/dev/vdb / ext4 ...`
- beta: `/dev/vdc / ext4 ...`

Each container wrote a distinct marker to its rootfs, and the host read those
markers back from the corresponding ext4 backing images:

- alpha backing image: `alpha-rootfs`
- beta backing image: `beta-rootfs`

This proves NBD can serve as a deterministic pre-create rootfs exposure building
block for the LinuxPod research path. It still does not solve normal kubelet
ordering, because the containers and rootfs attachments must be known before
`pod.create()`. The next useful primitive is therefore guest-side rootfs staging
inside an already-running Pod VM.

## R4 Result

R4 completed on 2026-06-21 UTC. The live probe is published at
[CRI_RUNTIME_R4_GUEST_STAGING_REPORT.md](CRI_RUNTIME_R4_GUEST_STAGING_REPORT.md).

Outcome: `stagedRootfsIdentityMismatch`.

The probe booted one LinuxPod with a predeclared utility container, then dialed
the running VM agent after `pod.create()`. Guest-side staging through a temporary
guest command worked: the direct staged marker was visible as
`macvz-r4-id=late-alpha`, and no guessed `/dev/*` path was used.

The remaining boundary is mount namespace visibility. The VM agent accepted the
bind mount for `/run/macvz-r4/mounts/late-alpha`, but a later exec inside the
predeclared utility container did not see the mount target or a corresponding
`/proc/mounts` entry. This means R4 proves post-create file staging, but not yet
a full late-container rootfs primitive.

The next probe should bypass the already-running utility container as the
observer and use the VM agent to create/start a process in the intended sandbox
namespace from the staged rootfs.

## R5 Result

R5 completed on 2026-06-21 UTC. The live probe is published at
[CRI_RUNTIME_R5_STAGED_PROCESS_REPORT.md](CRI_RUNTIME_R5_STAGED_PROCESS_REPORT.md).

Outcome: `processStartedButIdentityMismatch`.

The probe booted one LinuxPod with a predeclared keeper utility container, staged
a minimal busybox rootfs under `/run/macvz-r5/staged/late-alpha/rootfs` after
`pod.create()`, and then called the VM agent process APIs with an OCI spec whose
root pointed at that staged path.

The root-level VM process path is explicitly unavailable in the current
`vminitd` path:

```text
processes in the root of the vm not implemented
```

The fallback `containerID=utility` path did create and start a process, but the
process exited with code 1 and did not write the expected result file into the
staged rootfs. That means container-scoped agent process creation works as an
exec-like operation, but this probe did not prove that it can launch a new
container process from an arbitrary post-create staged rootfs.

R5 therefore narrows the remaining gap: post-create file staging works, but the
current public agent/LinuxPod path still lacks a proven "prepare rootfs, then
start a new container process from that rootfs" primitive for normal kubelet
ordering.

## R6 Result

R6 completed on 2026-06-22 UTC. The live probe is published at
[CRI_RUNTIME_R6_VMINITD_CONTAINER_REPORT.md](CRI_RUNTIME_R6_VMINITD_CONTAINER_REPORT.md).

Outcome: `vminitdContainerRootfsPathFound`.

Source inspection and live evidence clarified the state model:

- `containerID=nil` is rejected by vminitd with `processes in the root of the vm
  not implemented`.
- `containerID=utility` is exec-like because the utility container already
  exists in vminitd state. That path writes only OCI `process` JSON and does not
  use the submitted `root.path`.
- `id == containerID` with a previously unknown container ID enters the
  new-container branch. R6 proved `createProcess` can create that vminitd
  container object from a submitted OCI spec.

The process did not start successfully because `vmexec run` returned
`No such file or directory`. The likely cause is that the rootfs was staged by a
utility-container exec, so the populated tree was not the same rootfs tree
visible to vminitd's init namespace at start time.

This moves the remaining rootfs exposure problem one layer down: MacVz needs a
rootfs preparation primitive that vminitd can consume directly, or an upstream
LinuxPod/vminitd API that performs rootfs staging plus new-container registration
and start in one coherent state transition.

## R7 Result

R7 completed on 2026-06-22 UTC. The live probe is published at
[CRI_RUNTIME_R7_VMINITD_VISIBLE_ROOTFS_REPORT.md](CRI_RUNTIME_R7_VMINITD_VISIBLE_ROOTFS_REPORT.md).

Outcome: `vminitdVisibleRootfsPrimitiveMissing`.

R7 tested whether rootfs data staged by the utility container could be addressed
through vminitd's init-namespace view under `/run/container/utility/rootfs`.
Staging under utility `/run` was not sufficient, likely because it is
container-local mount state. Staging under utility `/macvz-r7/...` and using
`/run/container/utility/rootfs/macvz-r7/...` as OCI `root.path` still let
`createProcess` create the new vminitd container object, but `startProcess`
failed with `No such file or directory`.

This closes the "can MacVz fake a late rootfs by staging through a keeper
container?" question as no. The remaining rootfs exposure work should move to a
real vminitd/LinuxPod primitive: prepare rootfs data where vminitd can consume
it, register a new container from that rootfs, start the init process, and clean
up consistently on failure.
