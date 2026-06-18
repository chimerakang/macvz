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
- **P2** — `kubectl run alpine --image=alpine --restart=Never -- sleep 3600` lands a micro-VM on the Mac; `kubectl logs`/`exec` work; node shows in `kubectl get nodes`. Operator-facing run/verify/cleanup steps and expected output are documented in [docs/P2_SMOKE_TEST.md](P2_SMOKE_TEST.md); RBAC and manifests under [deployments/](../deployments/).
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
| #13 | Wire Virtual Kubelet controller into macvz-kubelet | P2 | in progress |
| #14 | Register virtual node with capacity, addresses, taints, and conditions | P2 | in progress |
| #15 | Implement node heartbeat and lease updates | P2 | in progress |
| #16 | Implement Provider PodLifecycleHandler state and CRUD methods | P2 | in progress |
| #17 | Translate Kubernetes Pod specs into runtime workload specs | P2 | in progress |
| #18 | Wire kubectl logs and exec through the runtime | P2 | in progress |
| #19 | Add RBAC, manifests, and P2 MVP smoke test docs | P2 | in progress |
| #20 | Implement Kubernetes-coordinated Pod IPAM | P3 | closed |
| #21 | Bring up WireGuard mesh between MacVz nodes | P3 | closed |
| #22 | Connect micro-VM networking to the controllable Pod network path | P3 | closed |
| #23 | Report Pod IPs and readiness so Services resolve across MacVz nodes | P3 | closed |
| #24 | Implement kubectl port-forward for MacVz-backed Pods | P3 | closed |
| #25 | Implement node and pod metrics reporting | P4 | open |
| #26 | Support VirtioFS-backed volumes for MacVz Pods | P4 | open |
| #27 | Handle image architecture and Rosetta-for-Linux behavior | P4 | open |
| #28 | Harden mTLS, RBAC, and runtime access boundaries | P4 | open |
| #29 | Add signed and notarized macvz-kubelet release flow | P4 | open |
| #30 | Build multi-node end-to-end test suite for beta readiness | P4 | open |
