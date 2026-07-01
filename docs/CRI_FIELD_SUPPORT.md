# CRI field support matrix — LinuxPod backend (#162)

How the LinuxPod-backed CRI adapter (`macvz-cri --experimental-linuxpod-backend`)
treats every CRI v1 `PodSandboxConfig` / `ContainerConfig` field. Classes:

- **Supported** — read and honored.
- **Rejected** — explicit request fails loudly at RunPodSandbox/CreateContainer
  with an actionable message. Only non-default values reject; the zero values
  kubelet sends for every vanilla Pod always pass.
- **Warned** — accepted but not enforced; one `klog` line per create points
  here. Rejecting these would break ordinary hardened charts.
- **Ignored** — not read; documented-harmless (no user-visible promise broken).

The guards live in `pkg/criserver/diagnose.go` (`unsupportedSandboxShape`,
`unsupportedContainerShape`, `ignoredContainerFields`) and
`pkg/criserver/mounts.go`; the sandbox-level guard is shared with the
apple/container CRI backend.

## PodSandboxConfig

| Field | Class | Notes |
| --- | --- | --- |
| metadata (name/uid/namespace/attempt) | Supported | identity, idempotent-retry matching |
| hostname | Supported | sets the Pod VM hostname |
| log_directory | Supported | composes container log paths |
| dns_config (servers/searches/options) | Supported | injected into the Pod rootfs as `/etc/resolv.conf` (protocol v7, #142) |
| port_mappings | **Rejected** when any `host_port != 0` | the adapter programs no host port forwards; use a ClusterIP Service or `kubectl port-forward`. Plain `containerPort` declarations (host_port 0) always pass |
| labels / annotations | Supported | stored, echoed, filterable |
| linux.resources (Pod-level sum) | Supported (sizing) | sizes the Pod VM: CPUs = ceil(cpu_quota/cpu_period), memory = memory_limit; unset leaves helper defaults (2 CPU / 1 GiB) |
| linux.sysctls | **Rejected** when non-empty | explicit kernel tuning the adapter does not apply |
| linux.security_context.namespace_options network/pid/ipc = NODE | **Rejected** | hostNetwork/hostPID/hostIPC cannot be shared with the macOS host; NoSchedule taint should keep such Pods off the node (see nodescheme) |
| linux.security_context.namespace_options.userns_options mode=POD | **Rejected** | `hostUsers: false` UID/GID mapping is not applied in the VM |
| linux.security_context.privileged | **Rejected** when true | fail-fast copy of the container-level rule |
| linux.security_context run_as_user / readonly_rootfs / supplemental_groups | Ignored | sandbox-level copies govern the pause container, which LinuxPod does not have; container-level copies are what matter |
| linux.cgroup_parent, linux.overhead | Ignored | no host cgroup for a micro-VM to join (same posture as Kata) |
| linux.security_context.selinux_options / seccomp / apparmor (sandbox level) | Ignored | see container level |
| windows.* | Ignored | N/A on this platform |

## ContainerConfig

| Field | Class | Notes |
| --- | --- | --- |
| metadata (name/attempt) | Supported | naming, idempotency |
| image.image | Supported | staged by the helper |
| command / args / envs / labels / annotations / log_path | Supported | forwarded to the helper; log in CRI format |
| mounts | Partial | see Mounts below |
| working_dir | **Warned** | not applied; process runs in the image default cwd |
| devices / CDI_devices | **Rejected** when non-empty | host devices cannot be passed into the Pod VM |
| stdin / stdin_once / tty | **Warned** | main-process interactive stdio is not attached (exec-only) |
| stop_signal | Ignored | helper picks the stop signal; `StopContainer` timeout is honored |
| image.annotations / user_specified_image / runtime_handler | Ignored | informational |
| linux.resources limits (memory_limit, cpu_quota, cpuset, hugepages, unified) | **Warned** | not enforced per-container; the VM is sized from the Pod-level sum. cpu_shares / oom_score_adj are kubelet always-set noise and never warned |
| linux.security_context.privileged = true | **Rejected** | micro-VM model grants no host-privileged access |
| linux.security_context.namespace_options pid=TARGET | **Rejected** | ephemeral debug container targeting another container's PID namespace is not supported |
| linux.security_context.namespace_options pid=POD | **Warned** | shareProcessNamespace: one VM, but not a merged PID namespace |
| run_as_user / run_as_group / run_as_username | **Warned** | not applied; process runs as image default. Plumbing a User field through the wire contract is future work |
| readonly_rootfs = true | **Warned** | rootfs stays writable |
| capabilities add/drop | **Warned** | not applied in the VM |
| no_new_privs = true | **Warned** | `allowPrivilegeEscalation: false` is not enforced |
| seccomp / apparmor profiles (non-Unconfined) | **Warned** | syscall filtering is not applied inside the VM; the VM boundary provides host protection but not intra-Pod hardening |
| supplemental_groups | **Warned** | not applied |
| masked_paths / readonly_paths | Ignored | kubelet defaults on every container; they mask host-kernel procfs leaks that a dedicated guest kernel moots |
| selinux_options | Ignored | no SELinux in the guest stack |

## Mounts

| Field | Class | Notes |
| --- | --- | --- |
| container_path / host_path / readonly | Supported | absolute paths; sources must be under the kubelet pods dir or `--volume-host-path-allowed`; empty host_path = in-guest tmpfs (Memory emptyDir) |
| propagation BIDIRECTIONAL | **Rejected** | VirtioFS cannot honor shared-mount semantics |
| propagation HOST_TO_CONTAINER | Partial | treated as private; VirtioFS shares are live views so host→guest updates propagate in practice |
| image / image_sub_path (OCI image volumes, KEP-4639) | **Rejected** | previously fell into the tmpfs branch and fabricated an **empty** directory where image content was promised |
| uidMappings / gidMappings | **Rejected** | ownership mapping is not applied in the VM |
| selinux_relabel | Ignored | no SELinux |
| recursive_read_only | Ignored | kubelet gates RRO on advertised runtime support, which is not advertised |

## Design rules

1. **Never reject on message presence or zero values.** kubelet populates
   `security_context`, `resources`, `namespace_options`, `cpu_shares=2`,
   `oom_score_adj`, `masked_paths`, `privileged=false`, `pid=CONTAINER`, and a
   `PortMapping` per declared containerPort on 100% of vanilla Pods.
2. **Reject only explicit requests that would otherwise be silently broken**
   (hostPort, privileged, devices, sysctls, userns, image volumes, targeted PID).
3. **Warn where rejecting would break standard hardening boilerplate**
   (runAsUser, RO rootfs, drop:[ALL], RuntimeDefault seccomp,
   allowPrivilegeEscalation:false, resource limits).
4. When a Warned field gains real enforcement (e.g. a `User` field in the wire
   contract), move it to Supported here and delete its warn line — this doc and
   `diagnose.go` must change together.
