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
```

Set `MACVZ_CONTAINERIZATION_DIR` if the checkout lives elsewhere. The default
live run does not attach a vmnet interface; it focuses on the LinuxPod C1
shared-namespace proof by using loopback inside the Pod. Set
`MACVZ_LINUXPOD_VMNET=1` to include vmnet attachment as an additional host
network probe.

The live run writes `docs/CRI_LINUXPOD_POC_REPORT.md` unless
`MACVZ_LINUXPOD_REPORT` points to another path.

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

Those remain later gates in `docs/CRI_LINUXPOD_FEASIBILITY.md`.
