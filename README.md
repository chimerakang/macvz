# MacVz — Apple Silicon Kubernetes Node (k3s-compatible CRI)

*English | [繁體中文](README.zh-TW.md)*

**MacVz turns Apple Silicon (M-series) Macs into first-class Kubernetes nodes that run OCI workloads as native micro-VMs.**

Instead of building yet another orchestrator, MacVz plugs into a **standard Kubernetes / k3s control plane** and makes the Mac a real node. Each Pod scheduled onto it is launched as an isolated Linux micro-VM via Apple's native **Virtualization.framework**. The result: high-isolation, second-level startup, low power draw, and full use of unified-memory bandwidth — without Docker Desktop's monolithic Linux VM.

MacVz offers two integration paths:

- **Primary (strategic) direction — k3s-compatible CRI node.** The Mac runs a real kubelet (a **k3s** agent) that uses `macvz-cri` as its **CRI runtime**; each Pod becomes a **LinuxPod** micro-VM via **Apple Containerization**. This is the direction the project is moving toward. It is validated end-to-end on real micro-VMs with an in-loop k3s kubelet on the test host and is still hardening toward general availability.
- **Secondary (compatibility) direction — Virtual Kubelet provider.** `macvz-kubelet` presents the Mac as a [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) node (no real kubelet/CRI on macOS), launching Pods as micro-VMs through [`apple/container`](https://github.com/apple/container). It is the more mature, signed/notarized path today and remains fully supported.

> **Positioning:** MacVz is **not** a new control plane. It is a *node-layer* project. You bring (or run) a normal Kubernetes/k3s cluster; MacVz makes Apple Silicon hosts usable as nodes. All the value lives in the runtime integration and cross-host networking — the parts the ecosystem does not yet provide.

---

## 1. Core Design Principles

- **Stand on Kubernetes, don't replace it.** Inherit `kubectl`, the scheduler, declarative APIs, Services, RBAC, and the entire ecosystem — through a real kubelet/CRI on the primary path, or Virtual Kubelet on the compatibility path. No custom control plane, no etcd to operate on the Macs.
- **Micro-VM isolation.** One Pod = one isolated, minimal Linux micro-VM. No shared guest kernel between workloads.
- **Ride Apple's runtime.** Use Apple Containerization (LinuxPod backend) / `apple/container` for image pull, guest kernel, RootFS, and the in-VM init shim — rather than re-implementing the hardest low-level virtualization plumbing.
- **Go-native glue.** The CRI adapter, Virtual Kubelet provider, runtime driver, and networking are written in Go; the only non-Go dependency is Apple's runtime (driven via its CLI/service API, or the LinuxPod helper protocol).
- **Flat cross-host networking.** A WireGuard mesh gives Pods on different Macs direct, encrypted L3 connectivity.

---

## 2. System Architecture

### Primary path — k3s-compatible CRI node

```
        ┌────────────────────────────────────────────┐
        │       Standard k3s / Kubernetes server       │
        │   (api-server / scheduler / etcd — 1 host)   │   ← you run this, unchanged
        └───────────────┬──────────────────────────────┘
                        │ k3s agent ↔ server
      ┌─────────────────┼──────────────────────┐
      ▼                 ▼                       ▼
┌───────────┐    ┌───────────┐           ┌───────────┐
│ Mac mini  │    │ Mac mini  │   ...     │ Mac mini  │   ← each = one real k3s node
│ kubelet   │    │ kubelet   │           │ kubelet   │   (k3s agent)
│   │ CRI    │    │   │ CRI    │           │   │ CRI    │
│   ▼        │    │   ▼        │           │   ▼        │
│ macvz-cri  │    │ macvz-cri  │           │ macvz-cri  │   ← CRI runtime (LinuxPod backend)
│  └ LinuxPod helper (Apple Containerization)            │
│   micro-VM  micro-VM  micro-VM          │  micro-VM │
└───────────┘    └───────────┘           └───────────┘
        └──────── WireGuard encrypted mesh ──────────┘
```

### Secondary path — Virtual Kubelet provider (compatibility)

```
   Standard Kubernetes control plane
        │ kubelet API (Virtual Kubelet)
        ▼
   macvz-kubelet  ├ provider (Virtual Kubelet)
                  ├ runtime  (apple/container driver)
                  └ network  (WireGuard mesh)
```

### What you do NOT build

- **Kubernetes control plane** — use any standard distribution (a single-node `k3s`, `k0s`, or full `kubeadm` cluster works). etcd, the scheduler, and the API server are reused as-is and can live on one machine (Mac or Linux). On the primary path the Mac joins as a normal **k3s agent** node.

### What MacVz provides

**Primary — `macvz-cri` (CRI runtime, one per Mac), driven by the node's real kubelet:**

- **CRI RuntimeService / ImageService** — implements the kubelet CRI contract: Pod sandbox lifecycle, container create/start/stop/remove/status, image pull/list/status/remove, logs, exec, attach, port-forward, and stats.
- **LinuxPod backend** — launches each Pod as a LinuxPod micro-VM via Apple Containerization (image staging, guest kernel, RootFS, late-rootfs identity handoff, shared Pod namespace), with the apple/container CRI backend retained as an alternative.
- **Networking** — attaches each Pod sandbox to MacVz Pod networking (Pod IPAM + a WireGuard mesh) so Pods across Macs and ClusterIP Services work, while leaving the host default route untouched.

**Secondary — `macvz-kubelet` (Virtual Kubelet provider, one per Mac):**

- **Provider (Virtual Kubelet)** — registers the Mac as a node, advertises CPU/RAM capacity, and implements the Pod lifecycle (`CreatePod`/`UpdatePod`/`DeletePod`/`GetPod(s)`/`GetPodStatus`, `GetContainerLogs`, `RunInContainer`, metrics).
- **Runtime driver** — translates a Pod spec into `apple/container` operations (pull → boot micro-VM → env/command/mounts → logs).
- **Networking** — Pod IPAM + WireGuard mesh + Pod IP reporting so Services/Endpoints work.

---

## 3. Tech Stack

| Layer | Technology / Library | Why |
| --- | --- | --- |
| Language | Go (Golang) | CRI adapter, Virtual Kubelet provider, runtime driver, and networking; integrates cleanly with `client-go`. |
| Node integration (primary) | k3s agent kubelet + CRI (`macvz-cri`) | The Mac is a real k3s node; `macvz-cri` implements the kubelet CRI contract. |
| Pod runtime (primary) | LinuxPod backend via Apple Containerization | Launches each Pod as a LinuxPod micro-VM (image staging, guest kernel, RootFS, shared Pod namespace, late-rootfs identity handoff). |
| Node integration (secondary) | `virtual-kubelet/virtual-kubelet` | Presents a Mac as a Kubernetes node without a real kubelet/CRI on macOS. |
| Container runtime (secondary) | [`apple/container`](https://github.com/apple/container) | Apache-2.0 Apple runtime: OCI image pull, guest kernel, RootFS, in-VM init (`vminitd`), second-level micro-VM startup on Apple Silicon. |
| macOS virtualization | Virtualization.framework (via Apple Containerization / `apple/container`) | Native Apple Silicon hypervisor; no third-party VMM. |
| Kubernetes client / CRI | `k8s.io/client-go`, `k8s.io/cri-api` | Talk to the API server, watch Pods, report node/Pod status, and implement CRI RuntimeService/ImageService. |
| Cross-host network | WireGuard (Go-native) | Encrypted P2P mesh giving Pods flat L3 connectivity across Macs (the CNI-equivalent layer). |
| Config | go-yaml | Provider/node configuration. |

> **Reference projects:** [`agoda-com/macOS-vz-kubelet`](https://github.com/agoda-com/macOS-vz-kubelet) is the closest prior art for the Virtual Kubelet approach on macOS. [`abiosoft/colima`](https://github.com/abiosoft/colima) is a useful reference for CLI/UX and for how a Go program drives an Apple `vz` backend — but **not** for its Kubernetes model (it runs k3s inside a single large VM; MacVz instead makes the Mac a real k3s node and runs one micro-VM per Pod).

---

## 4. Project Layout (standard Go layout)

```
macvz/
├── cmd/
│   ├── macvz-cri/            # CRI runtime adapter binary — PRIMARY (one per Mac node)
│   │   └── main.go
│   └── macvz-kubelet/        # Virtual Kubelet provider binary — secondary/compatibility
│       └── main.go
├── pkg/
│   ├── criserver/            # CRI RuntimeService/ImageService adapter + store
│   ├── runtime/              # apple/container integration (CLI / service-API driver)
│   │   └── linuxpod/         # LinuxPod backend (Apple Containerization helper protocol)
│   ├── provider/             # Virtual Kubelet PodLifecycleHandler implementation
│   ├── network/              # WireGuard mesh + Pod IPAM + IP reporting
│   ├── config/               # YAML config parsing
│   └── metrics/              # node & pod resource reporting to Kubernetes
├── test/e2e/cri-k3s/         # k3s in-loop / soak / conformance-smoke harnesses
├── deployments/              # example k8s manifests, RBAC, node bootstrap
├── go.mod
└── README.md
```

---

### Primary path — k3s-compatible CRI node (`macvz-cri`)

`macvz-cri` is the **CRI runtime** the Mac's real kubelet (a k3s agent) talks to. Each
Pod is launched as a **LinuxPod** micro-VM via Apple Containerization. The LinuxPod
backend is the primary backend and is enabled with
`macvz-cri --experimental-linuxpod-backend` (the apple/container CRI backend is the
alternative). The `experimental-` flag prefix reflects that this path is still
hardening toward general availability — it is validated end-to-end on real micro-VMs
with an in-loop k3s kubelet on the project test host, but does **not yet** supersede
the signed/notarized Virtual Kubelet path for production use.

- LinuxPod-backed CRI: see [docs/CRI_LINUXPOD_FEASIBILITY.md](docs/CRI_LINUXPOD_FEASIBILITY.md) and the CRI-L reports under [docs/](docs/).
- k3s in-loop / soak / conformance-smoke harnesses: [test/e2e/cri-k3s/](test/e2e/cri-k3s/).
- Runtime-handoff operator guide (feature gate, supported/unsupported behavior, install/run/cleanup): [docs/CRI_EXPERIMENTAL_HANDOFF_OPERATOR.md](docs/CRI_EXPERIMENTAL_HANDOFF_OPERATOR.md).

---

## 5. Phased Development Plan

> **Guiding idea:** prove the runtime layer on a single Mac first, then make it a Kubernetes node, then connect nodes across machines. The same runtime layer feeds both integration paths.

### Primary track — k3s-compatible CRI node (`macvz-cri`, in progress)

**Goal:** the Mac is a real k3s node whose kubelet drives `macvz-cri`, launching each Pod as a LinuxPod micro-VM, with ordinary k3s workloads (Deployments, Services, DNS, ConfigMaps/Secrets, volumes, probes) working unchanged.

- **CRI-P0…P9 (complete):** map `apple/container`/MacVz surfaces to CRI; sandbox/container/image/networking/streaming/stats surfaces; volumes/probes/restart recovery; k3s compatibility, install, cleanup, soak; and the route decision (**revised to GO as the primary direction**).
- **CRI-L1…L8 (in progress):** the **LinuxPod backend** — a real Apple Containerization helper that boots a LinuxPod micro-VM per Pod, late-rootfs identity handoff, Pod networking, logs/exec/stats, kubelet/k3s in-loop validation, recovery/adoption after restart, and k3s compatibility hardening (DNS/Services, volume projection, image lifecycle, node reboot recovery, conformance smoke, long soak).
- **Acceptance:** a real kubelet/k3s drives the LinuxPod backend end-to-end (`simulated=false`) on real micro-VMs — Deployment Available, Pod IP + ClusterIP Service reachability, `kubectl logs`/`exec`/`port-forward`, clean teardown with zero residual state, and the host default route preserved.

### Secondary track — Virtual Kubelet provider (complete through P4)

> The original single-Mac → provider → cross-host progression that established the shared runtime, provider, and networking layers. Complete through P4 (signed/notarized, multi-node e2e); retained as the compatibility path.

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
- **Security.** The node ↔ API server channel (`k3s` kubelet on the CRI path, `macvz-kubelet` on the Virtual Kubelet path) must use the cluster's normal mTLS/RBAC. Do not expose runtime/helper sockets or node ports publicly. Image-registry credentials and any secrets come from Kubernetes Secrets / environment, never hardcoded.
- **Pod `securityContext` model.** Each Pod is a dedicated micro-VM (its own kernel, hardware isolation) — a stronger boundary than a shared-kernel container. MacVz *maps* the fields the runtime can enforce (`runAsUser`/`runAsGroup` → `--user`, `readOnlyRootFilesystem` → `--read-only`, `capabilities` → `--cap-add`/`--cap-drop`), *accepts* fields the VM boundary already satisfies (e.g. `allowPrivilegeEscalation`, `seccomp`/`appArmor` `RuntimeDefault`, `fsGroup`), and *rejects* — with a terminal `Failed` status, never a silent no-op — the fields it cannot honor (`privileged: true`, `seLinuxOptions`, `Localhost` seccomp/appArmor, `procMount`, `sysctls`). `runAsNonRoot` is enforced only when paired with `runAsUser`. Full table in [docs/WORKLOADS.md](docs/WORKLOADS.md#securitycontext-52).
- **Privileged networking needs root tools, but the runtime path runs as your user.** The cross-Mac data plane (WireGuard mesh + pf/route/sysctl) needs root, yet Apple's runtime refuses to run as root — so `macvz-cri` / `macvz-kubelet` run as your user and delegate privileged commands to the `macvz-netd` helper daemon over a unix socket. You install the helper once with `sudo`; day-to-day node starts need no elevation. See [docs/PRIVILEGED_NETWORKING.md](docs/PRIVILEGED_NETWORKING.md) for the full setup and recovery runbook.
- **Operating a multi-node pool.** Joining, verifying, draining, removing, upgrading, troubleshooting, and cleaning up a Mac node is a single lifecycle runbook: [docs/MULTI_NODE_OPS.md](docs/MULTI_NODE_OPS.md). It is the order-of-operations index that ties together node join, the WireGuard mesh, the privileged-networking recovery procedures, and the live `/healthz/diagnostics` health report.
- **Kubernetes management UI.** A management UI runs on MacVz: [Headlamp](https://headlamp.dev) deploys as a single arm64 container that uses the projected ServiceAccount token and in-cluster ClusterIP routing, reached in a browser via `kubectl port-forward` (a virtual node runs no kube-proxy, so NodePort/LoadBalancer do not apply). Evaluation, compatibility analysis, and the RBAC-limited fixture are in [docs/MANAGEMENT_UI.md](docs/MANAGEMENT_UI.md) (fixture: [test/e2e/headlamp-ui/](test/e2e/headlamp-ui/)).
- **Long-running reliability validation.** P9 soak testing loops the multi-node e2e suite and real-app fixtures while restarting kubelet/helper services, cordon/uncordon-churning nodes, checking orphan cleanup, and sampling resource usage. Run it with `make soak` / [test/e2e/soak/run.sh](test/e2e/soak/run.sh); setup and report format are in [docs/SOAK_TESTS.md](docs/SOAK_TESTS.md).
- **Signing & notarization.** MacVz release binaries must be signed appropriately (and notarized for distribution). The kubelet drives `apple/container` through its CLI rather than linking Virtualization.framework directly, so MacVz's own entitlement file is intentionally empty unless that architecture changes.
