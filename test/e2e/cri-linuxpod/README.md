# CRI LinuxPod PoC (#88)

This directory contains the gated Swift proof of concept for #88. It validates
whether `apple/containerization` `LinuxPod` can honestly host two containers in
one Pod-like sandbox before MacVz builds any Go CRI backend or Swift helper
daemon around it.

Default mode is plan-only:

```sh
make cri-linuxpod-poc
```

Live mode requires macOS 26+, Apple Containerization assets, and boots a real
LinuxPod:

```sh
git clone https://github.com/apple/containerization test/e2e/cri-linuxpod/containerization
make -C test/e2e/cri-linuxpod/containerization fetch-default-kernel
make -C test/e2e/cri-linuxpod/containerization cross-prep
make -C test/e2e/cri-linuxpod/containerization init

MACVZ_LINUXPOD_POC=1 make cri-linuxpod-poc
MACVZ_LINUXPOD_POC=1 make cri-linuxpod-c2
MACVZ_LINUXPOD_POC=1 make cri-linuxpod-c4
MACVZ_LINUXPOD_POC=1 make cri-linuxpod-r1
MACVZ_LINUXPOD_POC=1 make cri-linuxpod-r3
MACVZ_LINUXPOD_POC=1 make cri-linuxpod-r4
MACVZ_LINUXPOD_POC=1 make cri-linuxpod-r5
MACVZ_LINUXPOD_POC=1 make cri-linuxpod-r6
MACVZ_LINUXPOD_POC=1 make cri-linuxpod-r7
```

Set `MACVZ_CONTAINERIZATION_DIR` if the checkout lives elsewhere. The default
live run does not attach a vmnet interface; C1 focuses on the LinuxPod
shared-namespace proof by using loopback inside the Pod. C2 tests whether a
container can be added after `pod.create()`, which maps to kubelet's
`RunPodSandbox` before later `CreateContainer` ordering. C4 installs a custom
`VZInstanceExtension` / `HotplugProvider` and records whether public APIs can
turn a post-create rootfs hotplug request into a real late container start
without guessing the guest block path. R1 boots a utility container, records the
guest `/sys/block` baseline, attaches a second ext4 rootfs image through public
VZ USB mass storage, and verifies whether the guest can discover, correlate,
mount, validate, unmount, detach, and observe cleanup without treating a guessed
`/dev/sdX` or `/dev/vdX` path as success. R3 serves two rootfs ext4 images over
local NBD Unix sockets, uses those NBD URLs as pre-create LinuxPod rootfs
sources, and verifies identity by reading each backing ext4 image after the
containers write distinct markers. R4 boots one predeclared utility container,
then uses the running VM agent to stage, bind mount, verify, clean up, and retry
rootfs-like guest directories with explicit request IDs after `pod.create()`.
R5 stages a minimal busybox rootfs after `pod.create()` and asks the VM agent to
create/start a process whose OCI root points at that staged tree.
R6 stages a minimal busybox rootfs after `pod.create()` and asks vminitd to
create/start a new container-scoped process with `id == containerID`, which
targets the non-exec `ManagedContainer` path instead of the existing utility
container's exec path.
R7 stages a rootfs through the utility container outside container-local `/run`,
then addresses that same tree through vminitd's init-namespace path under
`/run/container/utility/rootfs` to test whether the new-container path can
actually start from it.
Set
`MACVZ_LINUXPOD_VMNET=1` to include vmnet attachment as an additional host
network probe.

The C1 live run writes `docs/CRI_LINUXPOD_POC_REPORT.md`; C2 writes
`docs/CRI_LINUXPOD_C2_REPORT.md`; C4 writes
`docs/CRI_LINUXPOD_C4_REPORT.md`; R1 writes
`docs/CRI_RUNTIME_R1_DEVICE_DISCOVERY_REPORT.md` unless
`MACVZ_LINUXPOD_REPORT` points to another path.
R3 writes `docs/CRI_RUNTIME_R3_NBD_ROOTFS_REPORT.md`.
R6 writes `docs/CRI_RUNTIME_R6_VMINITD_CONTAINER_REPORT.md`.
R7 writes `docs/CRI_RUNTIME_R7_VMINITD_VISIBLE_ROOTFS_REPORT.md`.
R4 writes `docs/CRI_RUNTIME_R4_GUEST_STAGING_REPORT.md`.
R5 writes `docs/CRI_RUNTIME_R5_STAGED_PROCESS_REPORT.md`.

## What It Proves

- one `LinuxPod`;
- two busybox containers registered before `pod.create()`;
- container A listens on `127.0.0.1`;
- container B connects to A through `localhost`;
- per-container logs are captured;
- `execInContainer` works;
- `statistics(containerIDs:)` returns CPU/memory;
- stopping one container first does not immediately destroy the Pod VM.

## What It Does Not Prove

- kubelet/k3s compatibility;
- post-create `LinuxPod.addContainer` hotplug;
- CRI `Attach`;
- CRI `PortForward`;
- Service/CNI/multi-node routing;
- daemon restart recovery.
- production rootfs attachment management.

Those remain later gates in `docs/CRI_LINUXPOD_FEASIBILITY.md`.
