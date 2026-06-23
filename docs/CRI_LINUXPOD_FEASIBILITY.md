# CRI LinuxPod Feasibility (#87)

Date: 2026-06-21

## Decision

`apple/containerization` `LinuxPod` is the correct next route-C feasibility
target. It is not production-ready for MacVz yet, but it is the first upstream
surface found so far that can honestly model the Kubernetes multi-container Pod
shape: one VM, multiple container root filesystems/processes, shared VM
CPU/memory/network, and optional shared PID namespace.

The decision for #87 C0 is therefore **Go to a minimal LinuxPod PoC**, not
"replace the current runtime" and not "start a MacVz-owned VM runtime". The
current Virtual Kubelet architecture remains the shipped path. The current
`macvz-cri` path remains experimental.

The PoC must pass the shared Pod sandbox semantics before any bridge or CRI
backend work starts:

- two containers inside one `LinuxPod`;
- one VM network namespace, with Pod IP/vmnet attachment tracked as a separate
  network gate;
- `localhost` reachability from container B to a listener in container A;
- per-container rootfs/process/log/status/stats;
- sandbox VM/network lifetime survives one container stopping before another;
- kubelet-compatible ordering is understood, especially `RunPodSandbox` before
  `CreateContainer`.

## What LinuxPod Gives Us

The upstream `LinuxPod` API is explicitly marked experimental, but its shape is
much closer to Kubernetes Pod semantics than the `apple/container` CLI path.

| Need | LinuxPod surface | Feasibility |
| --- | --- | --- |
| One sandbox VM for a Pod | `LinuxPod(id, vmm, configuration)` | Promising. The Pod owns the VM. |
| Multiple containers in one Pod | `addContainer(id, rootfs, configuration)` | Promising if containers are registered before `create()`. |
| Start independent containers | `startContainer(containerID)` | Promising. |
| Stop independent containers | `stopContainer(containerID)` | Promising, but teardown ordering must be tested. |
| Stop whole sandbox | `stop()` | Promising. |
| Exec | `execInContainer(containerID, processID, configuration)` | Promising. |
| Logs | `ContainerConfiguration.process.stdout/stderr` writers | Promising; CRI-format file writer can mirror current adapter work. |
| Stats | `statistics(containerIDs:categories:)` | Promising for CPU/memory/process/memory-events surfaces. |
| Shared network namespace | One VM network, tests note containers share network sysctls | Must be proven with `localhost`, not just L3 IP. |
| Shared PID namespace | `Configuration.shareProcessNamespace` | Optional and promising for `shareProcessNamespace`; default separate PID namespace. |
| DNS/hosts | Pod-level and per-container DNS/hosts config | Promising for kubelet-provided DNS config mapping. |
| Shared volumes | `PodVolume` + `.sharedMount(...)` | Promising, but limited shape; Kubernetes volume coverage still needs design. |

## CRI Mapping

| CRI API | LinuxPod-backed mapping | Status |
| --- | --- | --- |
| `RunPodSandbox` | Allocate Pod ID/IP/interface, create a `LinuxPod` object, but likely do not call `pod.create()` yet unless a pause/empty VM model is required. | Open design point. Kubelet expects sandbox to exist before containers. |
| `CreateContainer` | Pull/unpack image to per-container rootfs, then `pod.addContainer(...)`. | Feasible before `pod.create()`. Risky after `pod.create()`. |
| `StartContainer` | If Pod VM is not created, call `pod.create()` once, then `pod.startContainer(id)`. | Feasible for all containers known before first start. |
| `StopContainer` | `pod.stopContainer(id)` and update CRI state. | Feasible; must test owner-first / sidecar-first ordering. |
| `RemoveContainer` | Stop if needed, remove CRI state, release per-container log/rootfs resources. | Partially feasible. LinuxPod public remove semantics need confirmation. |
| `StopPodSandbox` | `pod.stop()` and release Pod IP/interface. | Feasible. |
| `RemovePodSandbox` | Delete persistent state/rootfs/logs after stopped. | Feasible in MacVz layer. |
| `ContainerStatus` | MacVz state + LinuxPod process/wait status. | Feasible with persistent state. |
| `PodSandboxStatus` | MacVz state + Pod IP/interface + container statuses. | Feasible. |
| `Exec` / `ExecSync` | `pod.execInContainer(...)`. | Feasible. |
| `Attach` | Needs re-attach to an already-running primary process. | Blocked upstream; return honest `Unimplemented` initially. |
| `PortForward` | Needs host-to-pod network namespace dial. | Blocked upstream; `exec socat` workaround is possible but fragile. |
| `ContainerStats` | `pod.statistics(containerIDs:categories:)`. | Feasible for CPU/memory first. |
| `PodSandboxStats` | Aggregate per-container stats plus Pod-level accounting where available. | Partially feasible. |

## Main Blockers

| Blocker | Impact | Current answer |
| --- | --- | --- |
| Post-create `addContainer` hotplug | Kubelet may create/start one container before later containers are known; sidecars/restarts can need late adds. | Treat as a hard risk. Do not depend on live hotplug until a PoC proves it. |
| `Attach` missing | `kubectl attach` cannot be implemented honestly. | Return `Unimplemented` with a clear diagnostic for the experimental path. |
| `PortForward` missing | `kubectl port-forward` requires on-demand dial into Pod netns. | Return `Unimplemented` initially unless a safe helper is built. |
| Recovery/orphan cleanup | CRI runtime restart must rediscover live Pod VMs/containers. | Needs explicit state model; not solved by LinuxPod alone. |
| Service/DNS/CNI behavior | LinuxPod provides VM networking, not a full Kubernetes network data plane. | Reuse MacVz network lessons; keep k3s in-loop soak as a later gate. |
| API stability | LinuxPod is experimental and upstream issues are still active. | Acceptable only behind an experimental backend flag. |

## Lessons From `krust-cri`

`vanchonlee/krust-cri` is the closest public reference found. It exposes the
Kubernetes `runtime.v1` CRI API on macOS and drives Apple Containerization
`LinuxPod` underneath. Its smoke path runs k3s/kubelet in a LinuxPod with the
host CRI socket relayed into the guest, then verifies Pod creation and same-node
Pod-to-Pod TCP.

The useful lessons for MacVz are:

- A Swift LinuxPod helper/daemon is a credible bridge shape.
- k3s can be used as a real in-loop smoke client, not just `crictl`.
- Logs, restart status, and stats can be implemented over LinuxPod.
- DNS, Service networking, port mappings, multi-node routing, recovery, GC,
  volumes, security context, and RuntimeClass remain substantial work.
- The reference avoids relying on live post-create hotplug as the MVP path. If a
  container is added after the Pod is already created, it stops/recreates the Pod
  instead of pretending hotplug is solved.

That last point is important: a MacVz PoC must decide whether it can preserve
kubelet semantics without post-create hotplug. Recreating a Pod may be acceptable
for a local smoke test, but it is not an honest general CRI implementation unless
the lifecycle and event semantics are explicit.

## Bridge Options

| Option | Shape | Cost | Recommendation |
| --- | --- | --- | --- |
| Swift PoC CLI | Standalone Swift tool creates one LinuxPod and runs scripted checks. | Small | Do first. It proves semantics without Go/CRI complexity. |
| Swift helper daemon | Local Unix socket JSON-RPC or gRPC: Go `macvz-cri` calls Swift, Swift owns LinuxPod objects. | Medium | Best next bridge if PoC passes. |
| Direct Swift integration | Rewrite runtime-facing pieces in Swift or deeply embed Swift into Go build. | Medium/high | Avoid for now; packaging and build complexity arrive too early. |
| containerd Runtime v2 shim | kubelet -> containerd -> MacVz shim -> LinuxPod. | High | Good long-term shape, not first validation. |
| MacVz-owned VM runtime | Own VMM, guest agent, hotplug, image/rootfs, network, lifecycle. | Very high | Fallback only if LinuxPod cannot satisfy Pod semantics. |

## Phase Plan

### C1: Minimal LinuxPod Two-Container PoC

Build a small Swift PoC outside kubelet/CRI.

Implementation entrypoint:

```sh
make cri-linuxpod-poc                  # plan-only
MACVZ_LINUXPOD_POC=1 make cri-linuxpod-poc  # live run
```

Harness: `test/e2e/cri-linuxpod/`

Report: `docs/CRI_LINUXPOD_POC_REPORT.md`

Acceptance:

- creates a `LinuxPod` with two containers registered before `create()`;
- starts both containers;
- container A listens on `127.0.0.1:<port>`;
- container B connects to A via `localhost` and records success;
- both containers share the same LinuxPod network namespace through loopback;
- each container has a distinct rootfs/process identity;
- logs/stdout capture works per container;
- `execInContainer` works;
- `statistics(containerIDs:)` returns CPU/memory for both containers;
- stopping A first leaves B running and observable;
- `pod.stop()` tears down the VM cleanly;
- run duration and log paths are recorded.

### C1 Result

C1 passed on 2026-06-21 UTC using `apple/containerization` commit `6b7b42c`,
Swift 6.2.1, `bin/vmlinux-arm64`, `vminit:latest`, and
`docker.io/library/busybox:1.36.1`. The report is
`docs/CRI_LINUXPOD_POC_REPORT.md`.

The live run intentionally defaults to no vmnet interface. That keeps C1 focused
on the missing multi-container Pod sandbox primitive: one `LinuxPod`, two
containers, shared loopback reachability, per-container logs, `exec`, stats, and
container stop-order isolation. An optional `MACVZ_LINUXPOD_VMNET=1` mode exists
for later host-network probing, but vmnet/Pod IP attachment is not treated as
proven by C1.

Failure criteria:

- `localhost` between containers fails;
- each container needs its own VM/IP;
- stopping one container tears down the sandbox;
- the only working path depends on unsupported post-create hotplug;
- logs/exec/stats cannot be exposed with public APIs.

### C2: Kubelet Ordering Probe

Still outside production MacVz, test the awkward CRI ordering:

1. create `LinuxPod`;
2. register container A;
3. call `pod.create()` and start A;
4. attempt to add/register container B after `pod.create()`;
5. record whether public APIs can hotplug the second rootfs and start B.

If this fails, document the exact fallback model:

- pre-register all containers before first start, if kubelet ordering allows it
  for the target workload class;
- reject late sidecar/container creation honestly;
- or rebuild the Pod only for explicitly marked experimental smoke flows.

### C2 Result

C2 completed on 2026-06-21 UTC using the same upstream checkout and assets as
C1. The live probe registered `server` before `pod.create()`, created and
started the Pod, then attempted `pod.addContainer("late-client", ...)`.

Result: post-create add is **not supported**. The public LinuxPod API returned:

```text
ContainerizationError: unsupported: "hotplug not supported"
```

The report is `docs/CRI_LINUXPOD_C2_REPORT.md`.

Decision: LinuxPod is still useful for proving the missing "one VM, multiple
containers" sandbox primitive, but it cannot currently serve as an honest
general CRI backend for normal kubelet ordering. A LinuxPod-backed route-C
adapter must either:

- pre-register every container before `pod.create()` and explicitly restrict the
  supported workload class;
- reject late `CreateContainer` calls with a clear unsupported error; or
- use an explicit stop/recreate model only for marked experimental smoke flows,
  with honest Pod event/status consequences.

Do not build a production-shaped Swift helper daemon until this ordering
limitation has an accepted product/roadmap answer.

### C3: Backend Limit Decision

C3 chooses the next honest route after C1/C2, before any helper daemon or CRI
backend work starts. The decision is recorded in
[CRI_LINUXPOD_C3_DECISION.md](CRI_LINUXPOD_C3_DECISION.md).

C3 result:

- keep LinuxPod as route-C research evidence, not as a production backend;
- do not build a production-shaped Swift helper daemon yet;
- do not claim full kubelet/k3s CRI compatibility;
- allow a deliberately limited backend only behind an experimental flag, after
  one more hotplug boundary probe;
- treat stop/recreate only as an explicitly reported smoke-test fallback.

### C4: Hotplug Provider Boundary Probe (#91)

The next smallest experiment is not a helper daemon. It is a Swift probe that
installs a consumer-provided `VZInstanceExtension` / `HotplugProvider` on the
current `apple/containerization` checkout and records whether post-create
`LinuxPod.addContainer(...)` can be made real with public APIs.

Acceptance:

- a custom provider is installed on the VZ-backed VM instance;
- post-create `addContainer` reaches that provider;
- the probe either starts the late container using a real ext4/block rootfs
  attachment, or records the exact public API boundary that prevents it;
- no fake `AttachedFilesystem` or guessed guest `/dev/...` path is counted as a
  success.

C4 result: **blocked at guest block-path resolution**. The live run on
2026-06-21 installed the custom extension/provider, reached the provider through
post-create `LinuxPod.addContainer(...)`, and successfully attached the late
rootfs image as public VZ USB mass storage. It still could not turn that attach
into a real LinuxPod rootfs mount because public APIs did not expose a
deterministic Linux guest block path for the attached ext4 image. The probe
refused to return a guessed `AttachedFilesystem`, so `addContainer` failed before
the late container could start. See
[CRI_LINUXPOD_C4_REPORT.md](CRI_LINUXPOD_C4_REPORT.md).

### R0: Pod VM Runtime Architecture Research (#92)

After C4, the main research path no longer treats a deliberately limited
LinuxPod backend as the next meaningful destination. A predeclared-container
backend might still be useful as a comparison harness or smoke-test tool, but it
would not answer the core question: how MacVz becomes a true Pod VM runtime that
supports kubelet's normal sandbox/container ordering.

#92 is therefore a runtime architecture research phase:

- define the Pod VM lifecycle MacVz would need to own or extend;
- define the guest-agent contract for mount/unmount, process lifecycle, exec,
  logs, stats, cleanup, and recovery;
- treat deterministic rootfs hotplug/device discovery as a first-class design
  problem;
- compare options such as VZ USB, NBD, virtiofs, guest-side unpack, and upstream
  `apple/containerization` contribution;
- study Kata Containers, firecracker-containerd, containerd Runtime v2 shims,
  `vminitd`, and nearby macOS/Apple Silicon projects;
- produce the next tiny PoC issue for one runtime primitive, not a full runtime
  rewrite.

R0 result: the architecture note is
[CRI_RUNTIME_R0_ARCHITECTURE.md](CRI_RUNTIME_R0_ARCHITECTURE.md). The selected
next primitive is guest-side hotplug device discovery for VZ USB mass-storage
rootfs attachments. If the guest can reliably correlate a host attach request to
a real block device and mount it, MacVz can design a real rootfs attachment
manager. If not, the research should pivot to NBD or guest-side image
pull/unpack.

### R1: Guest-Side Hotplug Device Discovery (#93)

R1 completed on 2026-06-21 UTC. The probe booted one LinuxPod with a utility
container, recorded the guest `/sys/block` baseline, attached a second ext4
rootfs image through public VZ USB mass storage, and asked the guest to discover
a new block device without guessing `/dev/sdX`.

Result: **blocked before correlation**. Host attach succeeded, but the guest did
not observe a new device. The diagnostic output showed:

- baseline block devices: `vda` and `vdb`;
- post-attach `/sys/block`: still only loop/ram plus `vda` and `vdb`;
- `/sys/bus/usb/devices`: empty;
- `/sys/class/scsi_disk`: empty;
- outcome: `guestCouldNotObserveNewDevice`.

The report is
[CRI_RUNTIME_R1_DEVICE_DISCOVERY_REPORT.md](CRI_RUNTIME_R1_DEVICE_DISCOVERY_REPORT.md).
This closes the C4/R0 USB-mass-storage question for the current environment:
public host-side attach success is not enough to build an honest guest rootfs
attachment primitive. The next route-C/runtime research should pivot to NBD or
guest-side rootfs exposure/pull/unpack.

### R2: Rootfs Exposure Fallback Research (#94)

R2 completed the fallback comparison in
[CRI_RUNTIME_R2_ROOTFS_EXPOSURE.md](CRI_RUNTIME_R2_ROOTFS_EXPOSURE.md).

The key finding is that Apple Containerization already has a concrete NBD path
for pre-create storage: `Mount.block(...)` accepts NBD URLs,
`VZNetworkBlockDeviceStorageDeviceAttachment` attaches them as virtio block
devices, `AttachedFilesystem` assigns deterministic `/dev/vd*` guest paths, and
upstream integration tests cover NBD volume identity, shared Pod volumes,
persistence, and concurrent writers.

The boundary is equally important: this is pre-create VM configuration, not a
post-create rootfs hotplug answer. It can support the next small PoC and improve
the limited/predeclared LinuxPod research path, but it does not solve kubelet's
normal `RunPodSandbox` before later `CreateContainer` ordering. Guest-side
rootfs staging remains the longer-term design needed for a full Pod VM runtime.

### R3: NBD Pre-Create Rootfs Identity (#95)

R3 completed on 2026-06-21 UTC. The report is
[CRI_RUNTIME_R3_NBD_ROOTFS_REPORT.md](CRI_RUNTIME_R3_NBD_ROOTFS_REPORT.md).

Result: **pre-create NBD rootfs works**. The probe served two busybox rootfs
ext4 images through NBD Unix sockets, registered both containers before
`pod.create()`, and started both containers. Guest output showed the rootfs
mounts came from virtio block devices (`/dev/vdb` and `/dev/vdc`), and host-side
EXT4 reads confirmed each container wrote to its own backing image.

Decision: keep NBD as a valid rootfs exposure building block for predeclared
LinuxPod containers and for future Pod volume work. Do not treat it as the full
CRI answer. The next phase should test guest-side rootfs staging inside an
already-running Pod VM.

### R4: Guest-Side Rootfs Staging (#96)

R4 completed on 2026-06-21 UTC. The report is
[CRI_RUNTIME_R4_GUEST_STAGING_REPORT.md](CRI_RUNTIME_R4_GUEST_STAGING_REPORT.md).

Result: **post-create guest-side file staging works, but agent-created bind
mount visibility is not yet enough for a late-container rootfs**. The probe
booted one LinuxPod with a predeclared utility container, dialed the running VM
agent after `pod.create()`, staged a rootfs-like tree with explicit request ID
`late-alpha`, and verified the direct marker from inside the guest. The direct
identity was visible as `macvz-r4-id=late-alpha`.

The VM agent accepted the bind mount for the staged rootfs, but a later exec in
the utility container did not observe the mount target or a `/proc/mounts` line.
This points to a namespace boundary between agent-level mounts and already
running container execs. The next step should test VM-agent-created process
execution from the staged rootfs rather than relying on an existing utility
container to observe the mount.

### R5: VM-Agent Process From Staged Rootfs (#97)

R5 completed on 2026-06-21 UTC. The report is
[CRI_RUNTIME_R5_STAGED_PROCESS_REPORT.md](CRI_RUNTIME_R5_STAGED_PROCESS_REPORT.md).

Result: **VM-agent process creation is not yet a late-container rootfs
primitive**. The probe staged a rootfs-like tree inside the running Pod VM after
`pod.create()`, then attempted to create/start a VM-agent process with an OCI
spec whose `root.path` pointed at the staged rootfs.

The root-of-VM process path failed with the explicit upstream boundary
`processes in the root of the vm not implemented`. A fallback using
`containerID=utility` did create and start a process, but that process exited
with code 1 and did not write the expected identity/result file into the staged
rootfs. The observed outcome is `processStartedButIdentityMismatch`.

Decision: R5 closes the immediate "can we just call VM-agent createProcess with
a staged rootfs?" question as **not proven / currently blocked**. Container-scoped
process creation behaves like an exec-like path, not a demonstrated arbitrary
late rootfs launch path. The next research step should inspect or extend the
guest/vminitd container creation path itself rather than adding more wrapper
logic around existing `execInContainer` behavior.

### R6: vminitd Container Rootfs Process Path (#98)

R6 completed on 2026-06-22 UTC. The report is
[CRI_RUNTIME_R6_VMINITD_CONTAINER_REPORT.md](CRI_RUNTIME_R6_VMINITD_CONTAINER_REPORT.md).

Result: **the lower-level vminitd new-container path exists, but rootfs staging
must happen in the namespace vminitd can consume**. Source inspection showed
that `createProcess` without `containerID` is explicitly rejected, while
`containerID=utility` is treated as exec because `utility` already exists in
`vminitd` state. That exec branch writes only OCI `process` JSON and ignores the
submitted `root.path`.

The non-exec branch is available when `containerID` is absent from vminitd state
and `id == containerID`. R6 called that path with `r6-late-alpha`; live evidence
showed `createProcess` succeeded and vminitd created a new cgroup/vmexec init
process. `startProcess` then failed with `No such file or directory`, and no
result file was written. The likely boundary is mount namespace ownership: the
staged rootfs was created by an exec inside the predeclared utility container,
not by vminitd in the init namespace where `vmexec run` resolves the OCI
`root.path`.

Outcome: `vminitdContainerRootfsPathFound`.

Decision: continue, but do not wire this into production yet. The next work
should prove an init-namespace rootfs preparation path, or define the smallest
upstream/LinuxPod primitive that stages rootfs data and registers/starts a new
vminitd container atomically enough for kubelet `CreateContainer` ordering.

### R7: vminitd-Visible Rootfs Launch (#99)

R7 completed on 2026-06-22 UTC. The report is
[CRI_RUNTIME_R7_VMINITD_VISIBLE_ROOTFS_REPORT.md](CRI_RUNTIME_R7_VMINITD_VISIBLE_ROOTFS_REPORT.md).

Result: **current public APIs still do not provide a proven vminitd-visible
rootfs staging primitive**. R7 tested the direct follow-up from R6: stage rootfs
data through the utility container, but address the same tree from vminitd using
`/run/container/utility/rootfs/...` as the OCI `root.path`.

Two variants were tried. Staging under the utility container's `/run` failed,
which is consistent with `/run` being container-local mount state. The final
probe staged under `/macvz-r7/...` to avoid `/run` tmpfs and used
`/run/container/utility/rootfs/macvz-r7/...` as the vminitd-visible path.
`createProcess` still succeeded and reached the new-container branch, but
`startProcess` failed with `No such file or directory`, and no result file was
written.

Outcome: `vminitdVisibleRootfsPrimitiveMissing`.

Decision: stop adding wrappers around utility-container exec staging. The next
step should define the smallest upstream-compatible LinuxPod/vminitd primitive
that prepares rootfs data in vminitd-visible state, registers the new container,
starts it, and cleans up failure state.

### R8: vminitd Rootfs/Container Primitive Design (#100)

R8 completed on 2026-06-22 UTC. The design is
[CRI_RUNTIME_R8_ROOTFS_CONTAINER_PRIMITIVE.md](CRI_RUNTIME_R8_ROOTFS_CONTAINER_PRIMITIVE.md).

Decision: **MacVz needs a first-class vminitd/LinuxPod primitive, not another
utility-container staging wrapper**. The proposed primitive splits the missing
behavior into `PrepareContainerRootfs` and `CreateContainer`, with explicit
state transitions for rootfs preparation, container registration, start,
failure rollback, and cleanup.

The next implementation should be both a local experimental fork/patch and an
upstream proposal. The local patch is only for proving the state machine with a
gated live probe; production CRI wiring remains blocked until the primitive is
stable or capability-gated.

The follow-up R9 probe should use the outcome names defined in the R8 design,
starting with `vminitdRootfsPrimitiveLaunchSucceeded` for success and precise
failure outcomes for prepare, create, start, identity, cleanup, or required
upstream changes.

### R9: vminitd Rootfs Primitive Launch (#101)

R9 completed on 2026-06-22 UTC. The report is
[CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md](CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md).

Result: **vminitd Copy can prepare a rootfs-like tree in a vminitd-visible
path, but current `vmexec run` still cannot start the late-created container
from it**. The harness did not patch apple/containerization source. It used the
existing vminitd `Copy(COPY_OUT/COPY_IN archive)` RPC as a local experimental
`PrepareContainerRootfs` shape, prepared
`/run/container/r9-late-alpha/rootfs`, verified it with `vminitd.stat`, and
called `createProcess(id == containerID)`.

`createProcess` succeeded and cleanup through `deleteProcess` removed the
prepared rootfs. `startProcess` failed with
`NSPOSIXErrorDomain Code=2 "No such file or directory"`, so the observed outcome
is `vminitdContainerStartFailed`.

Decision: do not wire this into production CRI. The research path is now
narrower: instrument or extend the upstream vminitd/vmexec bundle/rootfs start
path so a rootfs prepared after Pod VM creation can be accepted, or so the exact
missing invariant is exposed.

### R10: vminitd/vmexec Start Failure Instrumentation (#102)

R10 completed on 2026-06-22 UTC. The report is
[CRI_RUNTIME_R10_VMEXEC_START_REPORT.md](CRI_RUNTIME_R10_VMEXEC_START_REPORT.md).

Result: **the late prepared rootfs reaches `vmexec`, and the start failure is
now explained**. A local apple/containerization diagnostic patch rebuilt the Pod
VM initfs as `vminit:macvz-r10` and reran the R9 harness. The diagnostic payload
showed the bundle, OCI config, prepared rootfs, `/bin/sh`, dynamic linker, and
`libc` were all present where expected.

The actual failure was `stage=remove(ptmx) errno=2` in `vmexec` console/device
setup after the OCI `/dev` tmpfs and `/dev/pts` devpts mounts were configured.
The observed outcome is `vmexecStartFailureExplained`.

Decision: the next probe should patch the `vmexec` `/dev/ptmx` handling to be
idempotent for ENOENT and rerun the same late-rootfs harness. Production CRI
wiring remains blocked until a process can actually start and report rootfs
identity from the prepared container rootfs.

### R11: vmexec `/dev/ptmx` Probe (#103)

R11 completed on 2026-06-22 UTC. The report is
[CRI_RUNTIME_R11_PTMX_PROBE_REPORT.md](CRI_RUNTIME_R11_PTMX_PROBE_REPORT.md).

Result: **the `/dev/ptmx` blocker was removed, and the harness advanced to the
next device invariant**. A local apple/containerization patch changed
`configureConsole` so `remove(ptmx)` treats ENOENT as idempotent. The rebuilt
`vminit:macvz-r11` initfs no longer failed at `stage=remove(ptmx)`.

The next observed failure is `stage=open(/dev/null) errno=2`. The observed
outcome is `vmexecPtmxFixAdvancedToExec`.

Decision: the next probe should ensure a usable `/dev/null` exists after OCI
`/dev` tmpfs setup for late prepared rootfs starts. Production CRI wiring
remains blocked until the late container reports rootfs identity.

### R12: vmexec `/dev/null` Probe (#104)

R12 completed on 2026-06-22 UTC. The report is
[CRI_RUNTIME_R12_DEVNULL_PROBE_REPORT.md](CRI_RUNTIME_R12_DEVNULL_PROBE_REPORT.md).

Result: **the `/dev/null` blocker was removed, and the harness reached
userspace process execution**. A local apple/containerization patch creates
`/dev/null` as character device `1:3` after OCI `/dev` tmpfs setup and before
`pivotRoot`. The rebuilt `vminit:macvz-r12` initfs no longer failed at
`stage=open(/dev/null)`.

The new result is `processStartSucceeded=true` followed by `processExitCode=127`.
The observed outcome is `vmexecDevNullFixAdvancedToExec`.

Decision: the next probe should make the minimal R9 rootfs runnable by adding
BusyBox applet symlinks or invoking `/bin/busybox <applet>` explicitly.
Production CRI wiring remains blocked until the late container reports rootfs
identity.

### R13: Late Rootfs Userspace Probe (#105)

R13 completed on 2026-06-22 UTC. The report is
[CRI_RUNTIME_R13_USERSPACE_PROBE_REPORT.md](CRI_RUNTIME_R13_USERSPACE_PROBE_REPORT.md).

Result: **the late-rootfs userspace command-not-found blocker was removed**.
The R9 harness now creates relative BusyBox applet symlinks in the minimal
rootfs. The rebuilt run no longer exits 127; it starts the process and receives
`processExitCode=0`.

The observed outcome is `lateRootfsUserspaceAdvanced`.

Decision: the next probe should explain why `/macvz-r9-result` is not visible
to vminitd at `/run/container/r9-late-alpha/rootfs/macvz-r9-result` after the
process exits. Production CRI wiring remains blocked until rootfs identity
evidence is verified.

### R14: Late Rootfs Result Visibility (#106)

R14 completed on 2026-06-22 UTC. The report is
[CRI_RUNTIME_R14_RESULT_VISIBILITY_REPORT.md](CRI_RUNTIME_R14_RESULT_VISIBILITY_REPORT.md).

Result: **result visibility is the current blocker**. The process starts and
exits 0, proving the userspace path runs. The harness records that
the script completed after writing `/macvz-r9-result` inside the vmexec
namespace, but vminitd cannot stat it at
`/run/container/r9-late-alpha/rootfs/macvz-r9-result`.

The observed outcome is `lateRootfsResultVisibilityExplained`.

Decision: the next probe should add a vminitd-verifiable evidence channel,
such as stdout capture or an explicit shared result mount. Production CRI wiring
remains blocked until rootfs identity evidence is verified.

### R15: Late Rootfs Evidence Channel (#107)

R15 completed on 2026-06-22 UTC. The report is
[CRI_RUNTIME_R15_EVIDENCE_CHANNEL_REPORT.md](CRI_RUNTIME_R15_EVIDENCE_CHANNEL_REPORT.md).

Result: **late rootfs identity evidence is verified**. The harness prepares a
vminitd-visible handoff directory, bind-mounts it into the late rootfs, and
verifies the copied result at
`/run/macvz-r9-evidence/r9-late-alpha/macvz-r9-result`.

The observed outcome is `vminitdRootfsPrimitiveLaunchSucceeded`:

```text
processStartSucceeded=true
processExitCode=0
resultVerified=true
```

Decision: the research direction can move from harness proof to CRI runtime
design for a per-container evidence/result handoff path and cleanup lifecycle.

### R16: Production Evidence Handoff Design (#108)

R16 completed on 2026-06-22 UTC. The design is
[CRI_RUNTIME_R16_HANDOFF_DESIGN.md](CRI_RUNTIME_R16_HANDOFF_DESIGN.md).

Result: **runtime handoff design is accepted**. The handoff remains
runtime-private, lives under `/run/macvz/containers/<containerID>/handoff`, is
bind-mounted into the container at `/run/macvz/handoff`, and is created,
verified, and cleaned up by the runtime rather than the CRI server or kubelet.

The observed outcome is `runtimeHandoffDesignAccepted`.

Decision: no new probe is needed before implementation. The next work can split
into runtime handoff lifecycle implementation, OCI bind mount injection, and
gated R15-derived integration coverage.

### R17: LinuxPod Late-Rootfs Runtime Backend Prototype (#124)

R17 completed on 2026-06-23. The report is
[CRI_RUNTIME_R17_LINUXPOD_BACKEND_REPORT.md](CRI_RUNTIME_R17_LINUXPOD_BACKEND_REPORT.md).

Result: the LinuxPod sandbox primitive (C1/C2/C4) and the R15/R16 late-rootfs
handoff identity primitive are now expressed as the **smallest callable backend
contract** for Go `macvz-cri`, realizing the prototype scope of **C5** (Swift
helper daemon) and **C6** (experimental backend gate) below:

- **Contract** (`pkg/runtime/linuxpod.Backend`): `Ping`, `CreatePod`,
  `PrepareContainerRootfs` (the late-rootfs primitive), `CreateContainer`,
  `StartContainer` (identity-gated), `StopContainer`, `RemoveContainer`,
  `Status`, `Cleanup` — a MacVz-owned, narrow NDJSON protocol; the kubelet-facing
  boundary stays Go, no CRI server in Swift (C5 constraints honored).
- **Swift helper stub** (`test/e2e/cri-linuxpod-helper`): serves the protocol over
  a unix socket with an in-memory model mirroring the Go `FakeBackend`; boots no
  VM (`simulated=true`). The seam a real Apple Containerization LinuxPod helper
  grows from.
- **Gate** (`macvz-cri --experimental-linuxpod-backend --linuxpod-helper-socket`):
  off by default, loud startup handshake, apple/container serving path untouched
  (C6 constraints honored).

The required kubelet ordering is proven hermetically and across the Go↔Swift
boundary: `CreatePod → app create/start → late sidecar create/start (after the app
is running) → shared sandbox namespace → localhost reachable → sidecar
identityVerified → stop/remove both orderings → cleanup leaves no stale state`. The
C2 ordering limitation (containers known before `pod.create()` for shared-namespace
on the upstream API) is represented honestly in the contract via the explicit
`PrepareContainerRootfs` late-rootfs step rather than assumed away.

The observed outcome is `linuxpodBackendContractPrototyped`. Still **not**
production-ready: no k3s in-loop (**C7** remains future work), no Service/DNS/Pod-IP
or `Attach`/`PortForward`, and the real LinuxPod-backed helper still has to replace
the stub's in-memory model.

### C5: Swift Helper Daemon Prototype

Only if R0 later selects a LinuxPod-based bridge as a valid runtime building
block:

- expose a small local socket API for `CreatePod`, `AddContainer`,
  `StartContainer`, `StopContainer`, `StopPod`, `Exec`, `Stats`, and `Status`;
- keep the protocol MacVz-owned and narrow;
- do not implement a full CRI server in Swift;
- keep Go `macvz-cri` as the kubelet-facing boundary.

### C6: Experimental `macvz-cri --runtime-backend=linuxpod`

Only if C5 is stable enough and the C2/C4 ordering limitation is either resolved
or represented honestly in CRI behavior:

- add a new backend behind an explicit experimental flag;
- keep the current CLI-backed backend untouched;
- run hermetic CRI lifecycle tests against a fake helper;
- run gated live tests against the Swift helper.

### C7: k3s In-Loop Evidence

Only after the backend can run a real two-container Pod shape within the
accepted ordering limits:

- run a single-node k3s/kubelet smoke;
- verify scheduling, Pod events, logs, exec, stats, restart behavior, and cleanup;
- keep `Attach`/`PortForward` honest if still unsupported;
- run longer soak only after basic semantics pass.

## Recommendation After C4

Pause route-C implementation work while #92 researches the true runtime core.
The currently accepted position is:

- keep LinuxPod as a research-only proof of the shared sandbox primitive;
- do not build a helper daemon on the assumption that hotplug is viable;
- do not make the limited/predeclared-container backend the main path unless it
  is explicitly scoped as a comparison harness;
- research the Pod VM runtime architecture needed for full kubelet semantics;
- keep the shipped Virtual Kubelet provider unchanged while this research runs.

## References

- `apple/containerization` `LinuxPod.swift`: experimental LinuxPod API, multiple
  containers in one VM, shared VM resources/network, pre-create registration and
  post-create hotplug contract.
- `apple/containerization` `PodTests.swift`: upstream integration tests for
  multiple/concurrent containers, exec, stats, DNS/hosts, shared PID namespace,
  shared volumes, sysctl, and networking.
- `apple/containerization#735`: missing re-attach primitive for CRI `Attach`.
- `apple/containerization#736`: missing host-to-Pod network namespace dial for
  CRI `PortForward`.
- `apple/containerization#767`: post-create `LinuxPod.addContainer` hotplug is
  not currently proven for public VZ-backed ext4/block rootfs consumers.
- [CRI_LINUXPOD_C3_DECISION.md](CRI_LINUXPOD_C3_DECISION.md): MacVz route-C
  backend limit decision after C1/C2.
- [CRI_LINUXPOD_C4_REPORT.md](CRI_LINUXPOD_C4_REPORT.md): consumer-installed
  HotplugProvider reaches public USB attach but cannot resolve a deterministic
  guest block path for LinuxPod rootfs hotplug.
- Issue #92: runtime architecture research for a true Pod VM runtime instead of
  a thin limited LinuxPod wrapper.
- [CRI_RUNTIME_R0_ARCHITECTURE.md](CRI_RUNTIME_R0_ARCHITECTURE.md): target Pod
  VM runtime architecture and R1/R2 follow-up direction.
- `vanchonlee/krust-cri`: public experimental macOS CRI runtime over
  Apple Containerization `LinuxPod`.
- Kata Containers and firecracker-containerd: mature references for per-Pod VM,
  guest-agent, and multi-container Pod runtime design.
