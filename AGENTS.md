# AGENTS.md

Durable instructions for coding agents working in this repository.

## Project Direction

MacVz turns Apple Silicon Macs into Kubernetes nodes that run OCI workloads as
isolated Linux micro-VMs. It does not implement a new control plane: a standard
Kubernetes/k3s cluster remains responsible for the API server, scheduler, etcd,
Services, RBAC, and declarative APIs.

There are two integration paths:

1. **Primary (strategic) direction — k3s-compatible CRI node.** The Mac runs a
   real kubelet (a k3s agent) that drives `macvz-cri` as its CRI runtime, and
   each Pod becomes a **LinuxPod** micro-VM via Apple Containerization. The
   primary CRI backend is the LinuxPod backend
   (`macvz-cri --experimental-linuxpod-backend`); the apple/container CRI backend
   is retained as an alternative. This path is validated end-to-end on real
   micro-VMs with an in-loop k3s kubelet on the test host and is still hardening
   (CRI-L8).
2. **Secondary (compatibility) direction — Virtual Kubelet provider.** The
   `macvz-kubelet` process presents the Mac as a Virtual Kubelet node, launching
   workloads as micro-VMs through `apple/container`. It is the more mature,
   signed/notarized path today and remains fully supported.

## Architecture Boundaries

- `cmd/macvz-cri`: k3s/CRI runtime adapter entrypoint (primary direction).
- `cmd/macvz-kubelet`: Virtual Kubelet node process entrypoint (secondary path).
- `pkg/runtime`: single-host `apple/container` integration and runtime
  abstraction shared by both paths.
- `pkg/runtime/linuxpod`: LinuxPod backend (Apple Containerization helper
  protocol) — the primary CRI backend.
- `pkg/criserver`: CRI RuntimeService/ImageService adapter and store.
- `pkg/provider`: Virtual Kubelet provider implementation.
- `pkg/network`: WireGuard mesh, Pod IPAM, and Pod IP reporting.
- `pkg/metrics`: node and pod resource reporting.
- `pkg/config`: YAML configuration loading and validation.
- `internal/types`: small shared value types that avoid package cycles.

Do not add a custom scheduler, API server, `mvz-master`, `mvz-agent`, or
`github.com/Code-Hex/vz` virtualization path unless the roadmap is explicitly
changed first. (The CRI runtime path is now an explicit project direction, not a
prohibited one — see above.)

## Phasing

Use `docs/MASTER_TASKS.md` as the roadmap source of truth.

The Virtual Kubelet phases (the secondary/compatibility path) are complete
through P4:

- P0: scaffolding, CLI/config/logging, package boundaries, CI, build tooling.
- P1: single-Mac runtime integration over `apple/container`.
- P2: Virtual Kubelet provider MVP and node registration.
- P3: cross-host Pod networking and Service reachability.
- P4: hardening, metrics, volumes, image-arch handling, signing, e2e.

The strategic primary direction is the k3s-compatible CRI track:

- CRI-P0..P9: CRI feasibility, sandbox/container/image/networking/streaming
  surfaces, and the route decision — complete.
- CRI-L1..L8: LinuxPod-backed kubelet/k3s hardening — real-helper lifecycle,
  Pod networking, kubelet surfaces, in-loop validation, recovery/adoption, and
  k3s compatibility (DNS/Services, volume projection, image lifecycle, reboot
  recovery, conformance smoke, soak) — in progress.

Prefer proving runtime behavior on one Mac before wiring it into kubelet/CRI or
the Virtual Kubelet provider, and keep default tests hermetic with live runs
gated behind explicit environment variables.

## Runtime Integration Rules

- Keep all `apple/container` CLI or service API assumptions inside
  `pkg/runtime`.
- Record tested `apple/container` versions, command forms, `inspect` output, and
  notable stderr/error text when closing P1 tasks.
- Treat `apple/container` as pre-1.0: isolate unstable assumptions and cover
  parsing/error mapping with tests.
- Default unit tests must remain hermetic; real runtime tests should be gated by
  an explicit environment variable such as `MACVZ_INTEGRATION=1`.

## Validation

Before considering code changes complete, run the narrowest relevant checks.
For broad changes, prefer:

```sh
go test ./...
go vet ./...
make build
```

Use `golangci-lint run` when lint-sensitive code or CI configuration changes.

## Documentation

- Keep `README.md` and `README.zh-TW.md` aligned when changing user-facing
  architecture or requirements.
- Keep `docs/MASTER_TASKS.md` aligned with milestone status and acceptance
  evidence.
- For P1 benchmarks, write measured results to a stable document under `docs/`
  with machine model, RAM, macOS version, `apple/container` version, image,
  boot-time behavior, concurrency ceiling, and per-VM RAM overhead.
