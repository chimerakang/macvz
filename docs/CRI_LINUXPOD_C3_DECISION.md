# CRI LinuxPod C3 Decision (#90)

Date: 2026-06-21T17:08:36Z

## Decision

Route C stays alive, but only as a limited and explicit research path.

`apple/containerization` `LinuxPod` has proven the important sandbox primitive:
one VM can host multiple Linux containers with shared loopback, separate
rootfs/processes, logs, exec, stats, and container stop-order isolation. It does
not currently prove a general kubelet CRI backend, because normal kubelet
ordering can require adding a container after the Pod sandbox already exists.
The C2 live probe showed that `LinuxPod.addContainer(...)` after `pod.create()`
fails with:

```text
ContainerizationError: unsupported: "hotplug not supported"
```

MacVz should therefore not build a production-shaped Swift helper daemon or
claim full k3s/kubelet CRI compatibility yet.

## What The Code Says

The current upstream checkout inspected for this decision is
`apple/containerization` commit `6b7b42c` (`0.34.0`).

`LinuxPod.addContainer` has two different paths:

- before `pod.create()`, the container rootfs and mounts are registered into the
  VM creation configuration;
- after `pod.create()`, the API tries to hotplug the rootfs/mounts into the
  running VM.

The running-VM path depends on `VirtualMachineInstance.hotplug`. The default
implementation returns `unsupported: "hotplug not supported"`. The VZ-backed
instance can delegate to a `HotplugProvider`, but the default
`VZVirtualMachineManager` path does not install one. Source search found the
protocol and delegation hooks, but no default concrete provider wired into the
public manager path.

This means C2 failed at a real backend boundary, not because the MacVz harness
registered the second container incorrectly.

## Upstream Signal

`apple/containerization#767` is an open upstream issue describing the same
problem for public consumers: post-create `LinuxPod.addContainer` fails with
`hotplug not supported`, and a custom provider can receive the request but still
does not prove a real ext4/block rootfs hotplug path that starts the second
container. That issue is not a contract, but it is useful signal that this is a
known public-surface gap rather than a MacVz-only usage mistake.

## Compared Paths

| Path | What it gives | Risk | Decision |
| --- | --- | --- | --- |
| Research-only LinuxPod evidence | Keeps the C1 result as proof that the missing Pod sandbox primitive exists somewhere upstream. | Does not move MacVz closer to kubelet integration by itself. | Keep as baseline evidence. |
| Limited LinuxPod backend | Requires all containers to be known before `pod.create()`; rejects late `CreateContainer`. | Honest but narrow; many kubelet flows become unsupported. | Plausible only behind an experimental flag after one more hotplug boundary probe. |
| Stop/recreate fallback | Stops the Pod VM, registers the late container, recreates the Pod. | Changes Kubernetes semantics: restarts, events, IP/network continuity, and container state all need loud reporting. | Allowed only for marked smoke experiments, never hidden as normal CRI behavior. |
| Wait for upstream hotplug | Preserves correct semantics if upstream ships the provider. | Timeline and API are outside MacVz control. | Track, but do not block all research on waiting. |
| MacVz-owned sandbox runtime | Full control over VMM, guest agent, hotplug, rootfs, networking, lifecycle, and recovery. | Very large project; effectively starts building a local Pod VM runtime. | Not next. Use only if LinuxPod route C is ruled out. |

## Supported Claims After C3

MacVz may claim:

- LinuxPod can model a predeclared multi-container Pod sandbox in a single VM.
- LinuxPod is the best current research target for route C.
- A future LinuxPod backend would have to be explicitly experimental and honest
  about late container limitations.

MacVz must not claim:

- full kubelet/k3s CRI compatibility;
- normal late sidecar/container creation support;
- working `Attach` or `PortForward` over LinuxPod;
- stable Pod IP/vmnet/CNI behavior over LinuxPod;
- production readiness for route C.

## Next Experiment

Create #91 as a small C4 issue for a hotplug-provider boundary probe on the current
`apple/containerization` checkout. The probe should answer only this:

- can a consumer-installed `VZInstanceExtension` attach a `HotplugProvider` to a
  `VZVirtualMachineInstance`;
- does post-create `LinuxPod.addContainer` call that provider;
- can the provider, using only public APIs, attach an ext4/block rootfs and
  return a real guest path that lets the late container start;
- if not, where exactly does the public API boundary stop.

If C4 fails, the next route-C work should be a limited-backend design or a
stop/recreate semantics document, not helper-daemon implementation. If C4
passes, then a Swift helper daemon prototype becomes reasonable again.
