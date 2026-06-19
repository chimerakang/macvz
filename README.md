# MacVz — Apple Silicon Kubernetes Node Provider

*English | [繁體中文](README.zh-TW.md)*

**MacVz turns Apple Silicon (M-series) Macs into first-class Kubernetes nodes that run OCI workloads as native micro-VMs.**

Instead of building yet another orchestrator, MacVz plugs into the **standard Kubernetes control plane** through the [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) interface. Each Mac mini joins a cluster as a virtual node; each Pod scheduled onto it is launched as an isolated Linux micro-VM via Apple's native **Virtualization.framework** — using [`apple/container`](https://github.com/apple/container) as the runtime. The result: high-isolation, second-level startup, low power draw, and full use of unified-memory bandwidth — without Docker Desktop's monolithic Linux VM.

> **Positioning:** MacVz is **not** a new control plane. It is a *node-layer* project. You bring (or run) a normal Kubernetes cluster; MacVz makes Apple Silicon hosts usable as nodes. All the value lives in the runtime integration and cross-host networking — the parts the ecosystem does not yet provide.

---

## 1. Core Design Principles

- **Stand on Kubernetes, don't replace it.** Inherit `kubectl`, the scheduler, declarative APIs, Services, RBAC, and the entire ecosystem via Virtual Kubelet. No custom control plane, no etcd to operate on the Macs.
- **Micro-VM isolation.** One Pod = one isolated, minimal Linux micro-VM. No shared guest kernel between workloads.
- **Ride Apple's runtime.** Use `apple/container` for image pull, guest kernel, RootFS, and the in-VM init shim — rather than re-implementing the hardest low-level virtualization plumbing.
- **Go-native glue.** The provider, runtime driver, and networking are written in Go; the only non-Go dependency is Apple's runtime, driven via its CLI/service API.
- **Flat cross-host networking.** A WireGuard mesh gives Pods on different Macs direct, encrypted L3 connectivity.

---

## 2. System Architecture

```
        ┌────────────────────────────────────────────┐
        │      Standard Kubernetes Control Plane       │
        │   (api-server / scheduler / etcd — 1 host)   │   ← you run this, unchanged
        └───────────────┬──────────────────────────────┘
                        │ kubelet API (Virtual Kubelet)
      ┌─────────────────┼──────────────────────┐
      ▼                 ▼                       ▼
┌───────────┐    ┌───────────┐           ┌───────────┐
│ Mac mini  │    │ Mac mini  │   ...     │ Mac mini  │   ← each = one virtual node
│ macvz-    │    │ macvz-    │           │ macvz-    │
│ kubelet   │    │ kubelet   │           │ kubelet   │
│  ├ provider (Virtual Kubelet)          │           │
│  ├ runtime  (apple/container driver)    │           │
│  └ network  (WireGuard mesh)            │           │
│   micro-VM  micro-VM  micro-VM          │  micro-VM │
└───────────┘    └───────────┘           └───────────┘
        └──────── WireGuard encrypted mesh ──────────┘
```

### What you do NOT build

- **Kubernetes control plane** — use any standard distribution (a single-node `k3s`, `k0s`, or full `kubeadm` cluster works). etcd, the scheduler, and the API server are reused as-is and can live on one machine (Mac or Linux).

### What MacVz provides (`macvz-kubelet`, one resident process per Mac)

- **Provider (Virtual Kubelet)** — registers the Mac as a node, advertises its CPU/RAM capacity to the scheduler, and implements the Pod lifecycle: `CreatePod`, `UpdatePod`, `DeletePod`, `GetPod(s)`, `GetPodStatus`, plus `GetContainerLogs`, `RunInContainer` (exec), and metrics.
- **Runtime driver** — translates a Kubernetes Pod spec into `apple/container` operations (pull OCI image → boot micro-VM → wire up env/command/mounts → stream logs). This is the core glue layer.
- **Networking** — assigns each Pod a cluster IP, and uses a WireGuard mesh so Pods across different Macs reach each other directly. Reports Pod IPs back to Kubernetes so Services/Endpoints work.

---

## 3. Tech Stack

| Layer | Technology / Library | Why |
| --- | --- | --- |
| Language | Go (Golang) | Provider, runtime driver, and networking; integrates cleanly with `client-go`. |
| Node integration | `virtual-kubelet/virtual-kubelet` | Presents a Mac as a Kubernetes node without a real kubelet/CRI on macOS. |
| Container runtime | [`apple/container`](https://github.com/apple/container) | Apache-2.0 Apple runtime: OCI image pull, guest kernel, RootFS, in-VM init (`vminitd`), second-level micro-VM startup on Apple Silicon. |
| macOS virtualization | Virtualization.framework (via `apple/container`) | Native Apple Silicon hypervisor; no third-party VMM. |
| Kubernetes client | `k8s.io/client-go` | Talk to the API server, watch Pods, report node/Pod status. |
| Cross-host network | WireGuard (Go-native) | Encrypted P2P mesh giving Pods flat L3 connectivity across Macs (the CNI-equivalent layer). |
| Config | go-yaml | Provider/node configuration. |

> **Reference projects:** [`agoda-com/macOS-vz-kubelet`](https://github.com/agoda-com/macOS-vz-kubelet) is the closest prior art for the Virtual Kubelet approach on macOS. [`abiosoft/colima`](https://github.com/abiosoft/colima) is a useful reference for CLI/UX and for how a Go program drives an Apple `vz` backend — but **not** for its Kubernetes model (it runs k3s inside a single large VM, the opposite of this design).

---

## 4. Project Layout (standard Go layout)

```
macvz/
├── cmd/
│   └── macvz-kubelet/        # Virtual Kubelet provider binary (one per Mac node)
│       └── main.go
├── pkg/
│   ├── provider/             # Virtual Kubelet PodLifecycleHandler implementation
│   ├── runtime/              # apple/container integration (CLI / service-API driver)
│   ├── network/              # WireGuard mesh + Pod IPAM + IP reporting
│   ├── config/               # YAML config parsing
│   └── metrics/              # node & pod resource reporting to Kubernetes
├── deployments/              # example k8s manifests, RBAC, node bootstrap
├── go.mod
└── README.md
```

---

## 5. Phased Development Plan

> **Guiding idea:** prove the runtime layer on a single Mac first, then make it a Kubernetes node, then connect nodes across machines.

### Phase 1 — Runtime integration (single Mac, no Kubernetes)

**Goal:** drive `apple/container` from Go to manage the full lifecycle of one micro-VM.

- Initialize the project and `go.mod`.
- Build `pkg/runtime`: pull an OCI image, boot an Alpine micro-VM in seconds, stop/destroy it, stream logs, and exec into it — all from Go via the `apple/container` CLI/service API.
- Define the abstraction the provider will sit on top of (start/stop/status/logs/exec).

### Phase 2 — Virtual Kubelet provider MVP

**Goal:** a single Mac appears in `kubectl get nodes` and runs real Pods.

- Build `pkg/provider` implementing the Virtual Kubelet `PodLifecycleHandler`.
- Register the Mac as a virtual node; advertise CPU/RAM capacity so the standard scheduler can place Pods.
- Translate Pod spec → `pkg/runtime` calls (image, command/args, env, resource limits).
- Wire up `kubectl logs` and `kubectl exec` through the provider.
- **Acceptance:** `kubectl run alpine --image=alpine --restart=Never -- sleep 3600` lands a micro-VM on the Mac; `kubectl logs`/`exec` work.

### Phase 3 — Cross-host mesh networking

**Goal:** a Pod on Mac A can reach a Pod on Mac B; Services resolve cluster-wide.

- Implement Pod IPAM coordinated through Kubernetes (no decentralized self-assignment — avoid IP collisions).
- Stand up a WireGuard mesh between Macs; route micro-VM traffic into the WireGuard interface using a userspace network path (e.g. file-handle attachment + gvisor-tap-vsock style stack) so cluster IPs are fully controllable.
- Report Pod IPs back to the API server so Endpoints/Services work; add port-forward support.
- **Acceptance:** a Service backed by Pods on two different Macs is reachable through normal Kubernetes networking.

---

## 6. Requirements & Known Constraints

- **macOS 26 (Tahoe) or later, on Apple Silicon.** `apple/container` requires it; inter-container and host networking rely on recent OS support.
- **`apple/container` is a hard dependency** (Apache-2.0, pre-1.0). Its API may change until 1.0; pin versions and isolate all calls inside `pkg/runtime`.
- **Density is bounded by RAM, not by container-style kernel sharing.** Each micro-VM carries its own kernel and a fixed memory floor. Validate the real per-host concurrent-VM ceiling and per-VM overhead early in Phase 1 — this defines the project's practical capacity.
- **Image architecture.** Guests are arm64. Pulling amd64 images requires the arm64 variant or Rosetta-for-Linux support; surface this clearly to users.
- **Security.** The `macvz-kubelet` ↔ API server channel must use the cluster's normal mTLS/RBAC. Do not expose the runtime service or node ports publicly. Image-registry credentials and any secrets come from Kubernetes Secrets / environment, never hardcoded.
- **Pod `securityContext` model.** Each Pod is a dedicated micro-VM (its own kernel, hardware isolation) — a stronger boundary than a shared-kernel container. MacVz *maps* the fields the runtime can enforce (`runAsUser`/`runAsGroup` → `--user`, `readOnlyRootFilesystem` → `--read-only`, `capabilities` → `--cap-add`/`--cap-drop`), *accepts* fields the VM boundary already satisfies (e.g. `allowPrivilegeEscalation`, `seccomp`/`appArmor` `RuntimeDefault`, `fsGroup`), and *rejects* — with a terminal `Failed` status, never a silent no-op — the fields it cannot honor (`privileged: true`, `seLinuxOptions`, `Localhost` seccomp/appArmor, `procMount`, `sysctls`). `runAsNonRoot` is enforced only when paired with `runAsUser`. Full table in [docs/WORKLOADS.md](docs/WORKLOADS.md#securitycontext-52).
- **Privileged networking needs root tools, but the kubelet runs as your user.** The cross-Mac data plane (WireGuard mesh + pf/route/sysctl) needs root, yet `apple/container` refuses to run as root — so `macvz-kubelet` runs as your user and delegates the privileged commands to the `macvz-netd` helper daemon over a unix socket. You install the helper once with `sudo`; day-to-day kubelet starts need no elevation. See [docs/PRIVILEGED_NETWORKING.md](docs/PRIVILEGED_NETWORKING.md) for the full setup and recovery runbook.
- **Entitlements & signing.** `macvz-kubelet` runs as a resident process needing the virtualization entitlement; it must be signed appropriately (and notarized for distribution).
