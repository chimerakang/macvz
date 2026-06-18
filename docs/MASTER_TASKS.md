# MASTER_TASKS — MacVz Development Plan

Source of truth for the phased roadmap. Phases map to GitHub **Milestones**; tasks map to GitHub **Issues**. Regenerate with `/task-sync`.

> **Strategy:** MacVz is a *node-layer* project — a Virtual Kubelet provider that runs OCI workloads as native micro-VMs on Apple Silicon via `apple/container`. We do **not** build a control plane. See [README](../README.md).

## Phase Overview

| Phase | Title | Goal | Status |
| --- | --- | --- | --- |
| P0 | Scaffolding & Foundations | Buildable Go project: module, layout, CLI skeleton, CI, build tooling | ✅ Complete |
| P1 | Runtime Integration | Drive `apple/container` from Go on a single Mac: micro-VM lifecycle, logs/exec, density benchmark | 🚧 In Progress |
| P2 | Virtual Kubelet Provider MVP | Mac registers as a k8s virtual node and runs real Pods as micro-VMs | ⬜ Planned |
| P3 | Cross-host Mesh Networking | WireGuard mesh + Pod IPAM so Pods across Macs communicate and Services resolve | ⬜ Planned |
| P4 | Hardening & Beta | Metrics, volumes, image arch/Rosetta, mTLS/RBAC, signing/notarization, multi-node e2e | ⬜ Planned |

## Milestone Acceptance Criteria

- **P0** — `go build ./...`, lint, and tests are green in CI.
- **P1** — Go boots an Alpine micro-VM in seconds; `logs`/`exec` work; per-host concurrent-VM ceiling and per-VM RAM overhead are measured and recorded.
- **P2** — `kubectl run alpine --image=alpine -- sleep 3600` lands a micro-VM on the Mac; `kubectl logs`/`exec` work; node shows in `kubectl get nodes`.
- **P3** — A Service backed by Pods on two different Macs is reachable through normal Kubernetes networking.
- **P4** — Multi-node e2e suite green; signed/notarized `macvz-kubelet` build; volumes + image-arch handling supported.

## Issue Tracker

| Issue | Title | Phase | Status |
| --- | --- | --- | --- |
| #1 | Initialize Go module and base project layout | P0 | closed |
| #2 | macvz-kubelet CLI entrypoint: flags, config loading, structured logging | P0 | closed |
| #3 | Define core package interfaces (runtime / provider boundaries) | P0 | closed |
| #4 | CI pipeline on macOS runner (build, vet, golangci-lint, test) | P0 | closed |
| #5 | Build & release tooling (Makefile, version stamping) | P0 | closed |
| #7 | Define and implement Runtime interface over apple/container | P1 | open |
| #8 | micro-VM lifecycle: start / stop / destroy | P1 | open |
| #9 | Log streaming from micro-VMs | P1 | open |
| #10 | Exec into running micro-VMs | P1 | open |
| #11 | Density & per-VM RAM-overhead benchmark | P1 | open |
| #12 | arm64 image pull verification | P1 | open |

> P2–P4 issues are created when each phase begins (use `/task-add`), to avoid a backlog of speculative tickets. Planned task seeds:
>
> - **P2**: virtual-kubelet wiring + node registration · capacity advertisement · `PodLifecycleHandler` (Create/Update/Delete/Get/GetPods/GetPodStatus) · Pod spec → runtime translation · `kubectl logs`/`exec` · node heartbeat/lease · RBAC + example manifests.
> - **P3**: Pod IPAM coordinated via k8s · WireGuard mesh bring-up · userspace network path (file-handle attachment + gvisor-tap-vsock style) · Pod IP reporting → Endpoints/Services · port-forward.
> - **P4**: `pkg/metrics` resource reporting · VirtioFS volumes · Rosetta-for-Linux / amd64 handling · mTLS/RBAC hardening · signing & notarization · multi-node e2e.
