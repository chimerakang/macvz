# MASTER_TASKS — MacVz Development Plan

Source of truth for the phased roadmap. Phases map to GitHub **Milestones**; tasks map to GitHub **Issues**. Regenerate with `/task-sync`.

> **Strategy:** MacVz is a *node-layer* project — a Virtual Kubelet provider that runs OCI workloads as native micro-VMs on Apple Silicon via `apple/container`. We do **not** build a control plane. See [README](../README.md).

## Phase Overview

| Phase | Title | Goal | Status |
| --- | --- | --- | --- |
| P0 | Scaffolding & Foundations | Buildable Go project: module, layout, CLI skeleton, CI, build tooling | ✅ Complete |
| P1 | Runtime Integration | Drive `apple/container` from Go on a single Mac: micro-VM lifecycle, logs/exec, density benchmark | ✅ Complete |
| P2 | Virtual Kubelet Provider MVP | Mac registers as a k8s virtual node and runs real Pods as micro-VMs | ✅ Complete |
| P3 | Cross-host Mesh Networking | WireGuard mesh + Pod IPAM so Pods across Macs communicate and Services resolve | ✅ Implemented |
| P4 | Hardening & Beta | Metrics, volumes, image arch/Rosetta, mTLS/RBAC, signing/notarization, multi-node e2e | ✅ Complete |
| P5 | Privileged Networking & Full Data Plane | Make cross-Mac Service traffic work end-to-end and remove manual sudo from day-to-day operation | ⬜ Planned |
| P6 | Kubernetes Workload Compatibility | Support common Deployment-era Kubernetes primitives needed by real applications | ⬜ Planned |
| P7 | Multi-node Operations | Make Mac node bootstrap, joining, diagnostics, and removal repeatable | ⬜ Planned |
| P8 | Real App Validation | Run useful public and project-specific Kubernetes applications on MacVz | ⬜ Planned |
| P9 | Production Hardening | Improve recovery, resource accounting, packaging, upgrades, and long-running reliability | ⬜ Planned |

## Milestone Acceptance Criteria

- **P0** — `go build ./...`, lint, and tests are green in CI.
- **P1** — Go boots an Alpine micro-VM in seconds; `logs`/`exec` work; per-host concurrent-VM ceiling and per-VM RAM overhead are measured and recorded.
- **P2** — `kubectl run alpine --image=alpine --restart=Never -- sleep 3600` lands a micro-VM on the Mac; `kubectl logs`/`exec` work; node shows in `kubectl get nodes`. Operator-facing run/verify/cleanup steps and expected output are documented in [docs/P2_SMOKE_TEST.md](P2_SMOKE_TEST.md); RBAC and manifests under [deployments/](../deployments/).
- **P3** — A Service backed by Pods on two different Macs is reachable through normal Kubernetes networking.
- **P4** — Multi-node e2e suite green; signed/notarized `macvz-kubelet` build; volumes + image-arch handling supported.
- **P5** — Two-Mac e2e passes with `mesh.enabled: true` and `podNetwork.enabled: true`; Pod-to-Pod and Service traffic cross the WireGuard data plane; operators no longer need to start the main kubelet process with manual sudo.
- **P6** — A multi-Deployment application using ConfigMaps, Secrets, ServiceAccounts, probes, and image pull credentials can roll out and recover through normal Kubernetes controllers.
- **P7** — A new Mac can join the MacVz node pool through a documented bootstrap flow; existing nodes can be drained, diagnosed, and removed without manual cleanup.
- **P8** — At least one public Kubernetes application and one CBB-compatible subset run on MacVz and expose a browser-visible service.
- **P9** — Long-running soak tests survive kubelet/helper restarts, orphan cleanup works, resource usage remains bounded, and release artifacts can be installed, upgraded, rolled back, and removed.

## Current Validation Snapshot

As of 2026-06-19, `main` has passed the two-node baseline described in
[MULTI_NODE_TEST_REPORT_2026-06-19.md](MULTI_NODE_TEST_REPORT_2026-06-19.md):

- two MacVz nodes register as Ready;
- Pods schedule to each Mac and clean up their micro-VMs;
- `logs`, `exec`, `port-forward`, metrics, and stats work through the kubelet API;
- Services publish EndpointSlices with one Ready endpoint per Mac;
- cross-node Service data-plane reachability remains blocked until the privileged
  WireGuard + `podNetwork` path is enabled and verified.

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
| #25 | Implement node and pod metrics reporting | P4 | closed |
| #26 | Support VirtioFS-backed volumes for MacVz Pods | P4 | closed |
| #27 | Handle image architecture and Rosetta-for-Linux behavior | P4 | closed |
| #28 | Harden mTLS, RBAC, and runtime access boundaries | P4 | closed |
| #29 | Add signed and notarized macvz-kubelet release flow | P4 | closed |
| #30 | Build multi-node end-to-end test suite for beta readiness | P4 | closed |
| #37 | Run full WireGuard + podNetwork two-Mac e2e with privileged networking | P5 | planned |
| #38 | Add a privileged network helper daemon for WireGuard, route, sysctl, and pf operations | P5 | planned |
| #39 | Define and implement the local Unix-socket API between macvz-kubelet and the network helper | P5 | done — versioned control API (`pkg/network/privhelper`: protocol negotiation, `status`/`exec` ops, structured `APIError` codes, 1 MiB request cap); kubelet surfaces helper status at startup ([cmd/macvz-kubelet/main.go](../cmd/macvz-kubelet/main.go)); tests in `control_test.go`; spec in [PRIVILEGED_NETWORKING.md](PRIVILEGED_NETWORKING.md#control-api-kubelet--helper-39) |
| #40 | Add launchd install/uninstall support for the privileged network helper | P5 | in progress |
| #41 | Restrict helper inputs to configured CIDRs, interfaces, peers, and pf anchors | P5 | in progress |
| #42 | Add mesh peer reconciliation for adding/removing MacVz nodes without full restart | P5 | in progress |
| #43 | Extend e2e diagnostics for WireGuard handshakes, routes, pf anchors, and forwarding state | P5 | in progress |
| #44 | Document full privileged networking setup and recovery procedures | P5 | done — [docs/PRIVILEGED_NETWORKING.md](PRIVILEGED_NETWORKING.md) |
| #45 | Support restartPolicy Always and controller-managed workload expectations | P6 | in progress — restart loop + docs in [docs/WORKLOADS.md](WORKLOADS.md) |
| #46 | Support ConfigMap-backed environment variables and volume mounts | P6 | in progress — env + volume projection, docs in [docs/WORKLOADS.md](WORKLOADS.md) |
| #47 | Support Secret-backed environment variables and volume mounts | P6 | in progress — `secretKeyRef`/`envFrom secretRef` env + read-only `secret` volume projection (items, modes, optional), values never logged; tests in `pkg/provider/secrets_test.go`, docs in [docs/WORKLOADS.md](WORKLOADS.md) |
| #48 | Support envFrom, valueFrom, fieldRef, and resourceFieldRef translation | P6 | in progress — unified env resolver covers `envFrom` precedence, literal `$(VAR)` expansion, `fieldRef` metadata/spec paths, and `resourceFieldRef` CPU/memory/ephemeral-storage divisors; tests in `pkg/provider/downward_test.go`, fixture coverage in [test/e2e/p6-compat](../test/e2e/p6-compat/) |
| #49 | Support imagePullSecrets and private registry authentication | P6 | in progress — dockerconfigjson pull secrets resolved per Pod, registry login/pull/logout in the driver, docs in [docs/WORKLOADS.md](WORKLOADS.md) |
| #50 | Implement readiness, liveness, and startup probe handling | P6 | in progress — exec/HTTP/TCP probes gate readiness, restart on liveness failure, startup gating; docs in [docs/WORKLOADS.md](WORKLOADS.md) |
| #51 | Improve ServiceAccount token projection and in-cluster API compatibility | P6 | in progress — projected kube-api-access volume (bound token via TokenRequest, cluster CA, namespace) materialized at the standard path, docs in [docs/WORKLOADS.md](WORKLOADS.md) |
| #52 | Define supported and unsupported securityContext behavior for MacVz Pods | P6 | in progress — field-by-field policy: maps `runAsUser`/`runAsGroup`/`readOnlyRootFilesystem`/`capabilities` onto runtime flags, accepts VM-isolation no-ops, rejects `privileged`/`seLinux`/`Localhost` seccomp+appArmor/`procMount`/`sysctls` terminally; tests in `pkg/provider/securitycontext_test.go` + `pkg/runtime/container/driver_test.go`, model in [README](../README.md) and [docs/WORKLOADS.md](WORKLOADS.md) |
| #53 | Build a multi-Deployment compatibility fixture for rollout validation | P6 | in progress — P6 acceptance workload under [test/e2e/p6-compat/](../test/e2e/p6-compat/) (web + checker Deployments, ConfigMap/Secret/probe/ServiceAccount/Downward coverage), `run.sh` validates rollout/status/logs/exec/Service with redacted diagnostics; `make compat` + CI `compat` job |
| #54 | Create a node bootstrap/join command or documented workflow | P7 | planned |
| #55 | Automate WireGuard key generation, public key exchange, and config rendering | P7 | planned |
| #56 | Add node health and readiness diagnostics across runtime, provider, mesh, and pod network | P7 | planned |
| #57 | Add node drain and safe workload cleanup guidance/tooling | P7 | planned |
| #58 | Add node removal workflow, including route, peer, pf, and VM cleanup | P7 | planned |
| #59 | Produce a local diagnostic bundle command for support and bug reports | P7 | planned |
| #60 | Document multi-node operations, failure modes, and recovery playbooks | P7 | planned |
| #61 | Run a minimal public HTTP application and expose it through a browser-visible Service | P8 | planned |
| #62 | Run a guestbook-style application with multiple Deployments, Services, ConfigMaps, and Secrets | P8 | planned |
| #63 | Evaluate and run a Kubernetes management UI such as Headlamp or Dashboard on MacVz | P8 | planned |
| #64 | Define and run a CBB arm64-compatible subset on MacVz | P8 | planned |
| #65 | Publish real-app validation manifests and expected outputs | P8 | planned |
| #66 | Implement kubelet restart recovery for existing apple/container workloads | P9 | planned |
| #67 | Add orphan micro-VM detection and cleanup policy | P9 | planned |
| #68 | Improve node and Pod resource accounting for CPU, memory, disk, and image cache usage | P9 | planned |
| #69 | Add log rotation and structured diagnostics for long-running nodes | P9 | planned |
| #70 | Build install, upgrade, rollback, and uninstall packaging for macvz-kubelet and helper | P9 | planned |
| #71 | Run long-duration soak tests across kubelet/helper restarts and node churn | P9 | planned |
