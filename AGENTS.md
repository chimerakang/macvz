# AGENTS.md

Durable instructions for coding agents working in this repository.

## Project Direction

MacVz is an Apple Silicon Kubernetes node provider. It does not implement a new
control plane. A standard Kubernetes cluster remains responsible for the API
server, scheduler, etcd, Services, RBAC, and declarative APIs.

The `macvz-kubelet` process runs on each Mac and presents that host as a
Virtual Kubelet node. Workloads scheduled to the node are launched as isolated
Linux micro-VMs through Apple's `apple/container` runtime.

## Architecture Boundaries

- `cmd/macvz-kubelet`: resident node process entrypoint.
- `pkg/runtime`: single-host `apple/container` integration and runtime
  abstraction.
- `pkg/provider`: Virtual Kubelet provider implementation.
- `pkg/network`: WireGuard mesh, Pod IPAM, and Pod IP reporting.
- `pkg/metrics`: node and pod resource reporting.
- `pkg/config`: YAML configuration loading and validation.
- `internal/types`: small shared value types that avoid package cycles.

Do not add a custom scheduler, API server, `mvz-master`, `mvz-agent`, CRI
runtime, or `github.com/Code-Hex/vz` virtualization path unless the roadmap is
explicitly changed first.

## Phasing

Use `docs/MASTER_TASKS.md` as the roadmap source of truth.

- P0: scaffolding, CLI/config/logging, package boundaries, CI, build tooling.
- P1: single-Mac runtime integration over `apple/container`.
- P2: Virtual Kubelet provider MVP and node registration.
- P3: cross-host Pod networking and Service reachability.
- P4: hardening, metrics, volumes, image-arch handling, signing, e2e.

Prefer proving runtime behavior on one Mac before implementing Kubernetes
provider behavior, and prove provider behavior before cross-host networking.

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
