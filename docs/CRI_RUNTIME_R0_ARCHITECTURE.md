# CRI-R0 Pod VM Runtime Architecture Research (#92)

Date: 2026-06-21

## Decision

MacVz should not make a deliberately limited LinuxPod backend the main research
path. LinuxPod remains valuable evidence that one VM can host multiple
container root filesystems and processes, but #89/#91 show that the current
public API surface cannot honestly support kubelet's normal late
`CreateContainer` ordering.

The next meaningful research target is a **Pod VM runtime core**:

```text
kubelet / k3s
  -> CRI RuntimeService + ImageService
    -> MacVz runtime adapter
      -> Pod VM manager
        -> one Linux sandbox VM per Kubernetes Pod
          -> guest agent
            -> multiple OCI container processes in the same VM namespaces
```

This is closer to the Kata/firecracker-containerd model than to a thin wrapper
around `LinuxPod`.

## Why Not A Limited LinuxPod Backend

A predeclared-container-only LinuxPod backend can prove useful things, but it
does not solve the project goal:

- kubelet creates the Pod sandbox before it necessarily creates every
  container;
- restarts and injected sidecars can require adding containers after the
  sandbox is already alive;
- stop/recreate changes Pod/container lifecycle semantics and must not be
  hidden;
- #91 proved that a custom `HotplugProvider` can be installed and called, and
  public VZ USB mass-storage attach can succeed, but public APIs still did not
  expose a deterministic Linux guest block path for the attached ext4 rootfs.

So the core problem is not "add one API". It is a host/guest runtime protocol:
attach or otherwise expose a rootfs, discover it reliably in the guest, mount it
at a container root, create the container process, and recover the state later.

## Reference Lessons

### containerd Runtime v2

containerd's runtime v2 model keeps containerd as the manager of image content,
snapshots, metadata, and lifecycle requests, while a runtime shim owns the
lower-level execution lifecycle. The important lesson for MacVz is the boundary:
the shim is where Pod/VM-specific state belongs, not inside kubelet and not
inside a one-shot process.

Reference:
https://github.com/containerd/containerd/blob/main/docs/runtime-v2.md

### Kata Containers

Kata's shimv2 architecture is directly relevant: one runtime shim can manage a
Pod VM and multiple containers inside that VM. Kata separates the host runtime
from a guest agent that creates containers, starts processes, handles I/O, and
reports status from inside the VM.

Reference:
https://github.com/kata-containers/kata-containers/blob/main/docs/design/architecture/README.md

### firecracker-containerd

firecracker-containerd makes the host/guest split explicit. The host runtime
manages microVMs, block devices, and request routing; the guest agent manages
container lifecycle inside the VM. Its design also treats "how many host shims"
and "how many guest shims" as first-class choices. For MacVz, the likely shape
is one host runtime object per Pod VM and one guest agent per Pod VM.

References:
https://github.com/firecracker-microvm/firecracker-containerd/blob/main/docs/shim-design.md
https://github.com/firecracker-microvm/firecracker-containerd/blob/main/docs/design-approaches.md

### apple/containerization

`apple/containerization` already provides useful building blocks:

- OCI image pull/unpack helpers;
- ext4 rootfs creation;
- optimized Linux kernel/initfs flow;
- VZ-backed VM lifecycle;
- vsock-connected `vminitd`;
- guest RPCs for mount/unmount, process create/start/wait/delete, signals,
  exec-like process creation, file operations, networking setup, and stats;
- `LinuxPod` as proof that multiple container processes can share one VM.

But it currently does not provide a complete public late-rootfs hotplug path for
LinuxPod. The missing part is deterministic rootfs device identity across the
host/guest boundary.

References:
https://github.com/apple/containerization
https://apple.github.io/containerization/documentation/containerization/

### OrbStack

OrbStack is useful as product validation, not as implementation guidance. It
proves there is value in a highly integrated macOS Linux/container/Kubernetes
environment, but it is closed-source and its internals should not be treated as
a design dependency.

Reference:
https://docs.orbstack.dev/architecture

## Target Architecture

### Host Components

| Component | Responsibility |
| --- | --- |
| CRI adapter | Implements kubelet-facing CRI. Keeps Kubernetes API semantics honest. |
| Pod VM manager | Creates/stops one VM per Pod sandbox, owns Pod identity, VM identity, and network identity. |
| Image/rootfs manager | Pulls images, prepares immutable or writable rootfs artifacts, records image/rootfs metadata. |
| Rootfs attachment manager | Exposes a container rootfs to a running Pod VM and correlates host request to guest device/path. |
| Guest-agent client | Sends mount/process/stats/cleanup commands to the guest over vsock or equivalent. |
| Network manager | Assigns Pod IP, configures VM interface, integrates with CNI/MacVz data plane. |
| State store | Persists Pod VM, container, rootfs attachment, process, and network state for restart recovery. |
| GC/recovery controller | Reconciles persisted state, live VMs, live guest-agent state, and host artifacts. |

### Guest Components

| Component | Responsibility |
| --- | --- |
| Guest init | Boots minimal VM environment and starts the agent. `vminitd` is the current best starting point. |
| Guest agent | Receives host commands, mounts rootfs, creates container namespaces/processes, manages I/O and signals, reports stats. |
| Device discovery worker | Observes block/storage changes, maps an attachment request to the correct guest device, and reports ready/failure. |
| Container process supervisor | Tracks per-container init/process, exit code, timestamps, and cleanup. |
| Network setup | Brings up loopback and Pod interface, applies routes/DNS/hosts as requested. |

## Host/Guest Protocol Sketch

The protocol should be intentionally small at first:

| RPC | Direction | Purpose |
| --- | --- | --- |
| `AgentStatus` | host -> guest | Agent version, capabilities, kernel/device support. |
| `PrepareRootfs` | host -> guest | Given an attachment identity, discover and mount the rootfs at a staged path. |
| `CreateContainer` | host -> guest | Create OCI bundle/spec using a staged rootfs and namespace policy. |
| `StartContainer` | host -> guest | Start the container init process. |
| `WaitContainer` | host -> guest | Wait for container exit and return status. |
| `SignalContainer` | host -> guest | Send signal to init or exec process. |
| `Exec` / `ExecSync` | host -> guest | Create a secondary process in a container. |
| `Stats` | host -> guest | Return CPU/memory/pids/block/network stats. |
| `RemoveContainer` | host -> guest | Stop processes, unmount rootfs, release guest-side state. |
| `ListState` | host -> guest | Recovery inventory after host restart. |

`vminitd` already covers many process, mount, network, and stats calls. R0's
gap is whether MacVz can extend or layer a deterministic device-discovery
contract around it.

## Rootfs Exposure Options

| Option | Shape | Pros | Risks |
| --- | --- | --- | --- |
| VZ USB mass storage + guest discovery | Host attaches ext4 image as USB storage; guest scans `/sys`/`/dev` and maps it to request metadata. | Public VZ runtime attach exists; #91 proves attach can succeed. | Need reliable identity/path discovery; USB device naming can race. |
| NBD | Host serves each rootfs over NBD; guest connects/mounts by URL or host hotplugs an NBD-backed block device. | Strong identity if guest initiates connection; common VM-container pattern. | Requires guest NBD tooling/kernel support and host server lifecycle. |
| VirtioFS rootfs | Host exposes unpacked rootfs directory as a share. | Avoids block path mapping; useful for dev. | Rootfs isolation, overlay/writable layer, permissions, and mount propagation need care. |
| Guest-side pull/unpack | Guest agent pulls/unpacks image inside the VM. | Avoids rootfs hotplug entirely; identity stays inside guest. | Duplicates image stack in every VM; slower; registry auth/security complexity. |
| Pre-create registration | Register every rootfs before VM start. | Already proven by LinuxPod C1. | Does not support normal late `CreateContainer`; not the main research path. |
| Upstream HotplugProvider contribution | Implement missing public provider in `apple/containerization`. | Benefits upstream and MacVz. | Must solve the same guest identity problem and fit upstream design. |

## Recommended Next Primitive

The next PoC should be **guest-side hotplug device discovery**, because it is the
smallest experiment that attacks the actual #91 blocker.

The PoC should answer:

- after a VZ USB mass-storage rootfs attach, can a process inside the Pod VM
  reliably identify the new device without guessing;
- can the device be correlated to a host request using size, serial-like
  metadata, filesystem UUID, partition label, or an injected marker;
- can the guest mount it at a requested path and prove the expected rootfs
  content exists;
- can the guest unmount and survive detach/retry without stale state.

R1 completed on 2026-06-21 UTC. The host configured an XHCI controller, captured
the running VZ instance, and `VZUSBController.attach(device:)` returned success
for a second ext4 rootfs image. The guest did not observe a new block device:
`/sys/block` stayed at the baseline `vda/vdb`, `/sys/bus/usb/devices` was empty,
and `/sys/class/scsi_disk` was empty. The R1 outcome is therefore
`guestCouldNotObserveNewDevice`, not a mount/correlation success. See
[CRI_RUNTIME_R1_DEVICE_DISCOVERY_REPORT.md](CRI_RUNTIME_R1_DEVICE_DISCOVERY_REPORT.md).

This means public VZ USB mass-storage hotplug is not a reliable rootfs
attachment primitive for the current Apple Containerization LinuxPod/vminitd
environment. The next research should pivot to NBD or guest-side rootfs
exposure/pull/unpack instead of building a rootfs attachment manager on USB
block hotplug.

## CRI Mapping Implications

| CRI call | Target runtime behavior |
| --- | --- |
| `RunPodSandbox` | Create Pod VM, assign Pod IP, start guest agent, report sandbox Ready only when agent/network are ready. |
| `CreateContainer` | Prepare/pull image, create rootfs artifact, attach/expose rootfs to the running Pod VM, ask guest agent to mount and create OCI process state. |
| `StartContainer` | Ask guest agent to start the already-created container process. |
| `StopContainer` | Signal/wait/kill via guest agent; keep sandbox VM alive while other containers run. |
| `RemoveContainer` | Guest cleanup, unmount rootfs, host detach/delete rootfs artifact if safe. |
| `StopPodSandbox` | Stop all containers, release network, stop VM. |
| `PodSandboxStatus` | Report Pod IP, agent readiness, and container inventory from persisted + guest state. |
| `Exec` / `Logs` / `Stats` | Guest-agent operations; host streams and CRI log files remain MacVz-owned. |
| Recovery | Host reconciles persisted state with live VM and guest `ListState`; orphaned VMs/rootfs attachments are cleaned explicitly. |

## Non-Goals

- Do not replace the shipped Virtual Kubelet provider during this research.
- Do not claim full k3s/kubelet CRI compatibility.
- Do not build a production helper daemon before proving the rootfs/device
  primitive.
- Do not depend on OrbStack internals.
- Do not hide stop/recreate behavior as normal CRI lifecycle.

## Follow-Up Issues

- **CRI-R1 (#93):** guest-side hotplug device discovery PoC for VZ USB
  mass-storage rootfs attachments. Complete: host attach succeeded, but the
  guest did not observe a new USB/SCSI/block device.
- **CRI-R2 (#94):** NBD or guest-side rootfs exposure fallback study. This is
  now the active next primitive because R1 did not produce a reliable mapping.
  Complete: [CRI_RUNTIME_R2_ROOTFS_EXPOSURE.md](CRI_RUNTIME_R2_ROOTFS_EXPOSURE.md)
  selects a narrow pre-create NBD rootfs identity PoC as R3, while keeping
  guest-side rootfs staging as the long-term answer for normal kubelet ordering.
