# MASTER_TASKS — MacVz Development Plan

Source of truth for the phased roadmap. Phases map to GitHub **Milestones**; tasks map to GitHub **Issues**. Regenerate with `/task-sync`.

> **Strategy:** MacVz is a *node-layer* project — a Virtual Kubelet provider that runs OCI workloads as native micro-VMs on Apple Silicon via `apple/container`. We do **not** build a control plane. See [README](../README.md).
>
> **CRI feasibility:** A separate `develop` track is evaluating whether MacVz
> can also become a CRI runtime path for kubelet/k3s. The current strategy does
> not change until the Phase 0-2 risks are proven. See
> [CRI_FEASIBILITY.md](CRI_FEASIBILITY.md).

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

## CRI Feasibility Track

This track is intentionally separate from P0-P9 because it may change the main
architecture from Virtual Kubelet provider to kubelet CRI runtime integration.

| Phase | Title | Status |
| --- | --- | --- |
| CRI-P0 | Map `apple/container` and MacVz runtime surfaces to CRI, name hard blockers | ✅ Complete |
| CRI-P1 | Build a minimal CRI server skeleton that answers kubelet `Status` | ✅ Complete |
| CRI-P2 | Spike Pod sandbox lifecycle over `apple/container` | ✅ Complete |
| CRI-P3 | Run a single-container Pod through the CRI adapter | ✅ Complete |
| CRI-P4 | Implement CRI ImageService pull/list/status/remove | ✅ Complete |
| CRI-P5 | Integrate CNI/Pod networking lifecycle | ✅ Complete |
| CRI-P6 | Implement logs, exec, attach, port-forward, and stats surfaces | ✅ Complete |
| CRI-P7 | Validate volumes, projected data, probes, and restart recovery | ✅ Complete |
| CRI-P8 | Harden k3s compatibility, install, cleanup, and soak behavior | ✅ Complete |
| CRI-P9 | Make the route-two go/no-go decision and migration plan | ✅ Complete (no-go for replacement; experimental side path) |
| CRI-P9 follow-up (#82) | Multi-container Pod feasibility: pause-VM shared-netns spike | ✅ Complete (blocked on missing `apple/container` primitive; flag-gated adapter path ready) |
| CRI-P9 follow-up (#83) | Real-hardware CRI-socket soak run and published report | ✅ Complete for crictl/socket soak; real kubelet/k3s in-loop soak still open |
| CRI-P9 follow-up (#84) | Host-namespace workload feasibility / honest scheduling exclusion | ✅ Complete (taint/label opt-in scheme + loud RunPodSandbox backstop) |
| CRI-P9 follow-up (#85) | Real kubelet/k3s fixture deployment and multi-day in-loop soak | 🟡 Harness/fixture/runbook built (`test/e2e/cri-k3s/k3s-inloop.sh`, `docs/CRI_K3S_INLOOP_REPORT.md`); gated live run operator-pending |
| CRI-P9 follow-up (#86) | Unblock honest multi-container Pods when `apple/container` exposes shared sandbox namespaces | 🟡 Capability re-verified absent (1.0.0); adapter-side honest path prepared behind `--experimental-multi-container` (join-in-sandbox-VM, one Pod IP, leak-free, owner-first drain guarded); blocked on runtime primitive with stable sandbox namespace lifetime |
| CRI runtime feasibility (#87) | Validate `apple/containerization` `LinuxPod` as the route-C Pod sandbox backend | 🟡 C0/C1 promising, but C2 proves post-create container hotplug unsupported; route C remains limited/experimental |
| CRI-C1 (#88) | Minimal LinuxPod two-container shared-namespace PoC | ✅ Complete (live LinuxPod two-container localhost/exec/stats/stop-order run passed; vmnet/PodIP probe remains a later network gate) |
| CRI-C2 (#89) | LinuxPod kubelet ordering and post-create container probe | ✅ Complete (live probe returns `unsupported: "hotplug not supported"`; all containers must be registered before `pod.create()` or use explicit stop/recreate fallback) |
| CRI-C3 (#90) | Decide LinuxPod backend limits and next sandbox strategy | ✅ Complete (route C stays experimental; no helper daemon/full CRI claim until hotplug boundary is proven or a limited model is explicitly accepted) |
| CRI-C4 (#91) | Probe LinuxPod HotplugProvider boundary on current apple/containerization | ✅ Complete (provider can be installed/called and USB mass-storage attach succeeds, but public APIs do not yield a deterministic guest block path for late rootfs mount) |
| CRI-R0 (#92) | Research Pod VM runtime architecture for full kubelet semantics | ✅ Complete (thin LinuxPod wrapper rejected as main path; target Pod VM runtime architecture and R1 device-discovery PoC defined) |
| CRI-R1 (#93) | Guest-side hotplug device discovery PoC for Pod VM rootfs attachments | ✅ Complete (host VZ USB attach succeeds, but guest observes no new USB/SCSI/block device; pivot to NBD or guest-side rootfs exposure) |
| CRI-R2 (#94) | Evaluate NBD and guest-side rootfs exposure fallback for Pod VM runtime | ✅ Complete (NBD pre-create rootfs identity selected for the next tiny PoC; guest-side rootfs staging remains the long-term kubelet-ordering answer) |
| CRI-R3 (#95) | NBD-backed pre-create rootfs identity PoC for LinuxPod containers | ✅ Complete (two NBD-served rootfs images boot as predeclared LinuxPod containers; guest virtio-block mount evidence and host-side EXT4 marker reads confirm identity) |
| CRI-R4 (#96) | Guest-side rootfs staging PoC for already-running Pod VM | ✅ Complete (post-create guest-side file staging works with explicit request identity; agent-created bind mount is not visible to a later exec in the predeclared utility container) |
| CRI-R5 (#97) | VM-agent process execution from staged guest rootfs | ✅ Complete (root-level VM process creation is unimplemented; `containerID=utility` can create/start a process but did not execute from the staged rootfs identity, outcome `processStartedButIdentityMismatch`) |
| CRI-R6 (#98) | Inspect `vminitd` container/rootfs process path after staged rootfs R5 | ✅ Complete (`id == containerID` reaches vminitd's new-container path and creates the container object; start still needs rootfs staged in the namespace vminitd can consume, outcome `vminitdContainerRootfsPathFound`) |
| CRI-R7 (#99) | Prove vminitd-visible rootfs staging for new-container start | ✅ Complete (utility-container staging addressed through `/run/container/utility/rootfs/...` still cannot start a new vminitd container; outcome `vminitdVisibleRootfsPrimitiveMissing`) |
| CRI-R8 (#100) | Design upstream-compatible vminitd rootfs/container primitive | ✅ Complete (selected two-stage `PrepareContainerRootfs` + `CreateContainer` primitive; next implementation is local experimental fork/patch plus upstream proposal, not production CRI wiring) |
| CRI-R9 (#101) | Prototype vminitd rootfs primitive launch with local patch | ⬜ Planned |

**CRI-P5 evidence (#77):** Pod networking is wired through the same primitives as
the shipped provider — `network.PodIPAM` for Pod IPs and `podnet.Router` for the
host pf binat path — reached via narrow interfaces in `pkg/criserver/network.go`.
`RunPodSandbox` reserves the Pod IP and rejects duplicate live sandboxes for the
same Pod key; `StartContainer` attaches the path once the micro-VM address
appears; `PodSandboxStatus.Network.Ip` and `Status.NetworkReady` are reported
only when actually ready; direct container stop/remove and self-exit reconcile
detach the path while sandbox remove releases the IP; teardown is idempotent and
`Server.RecoverNetwork` rebuilds reservations and re-attaches surviving sandboxes
after a restart. Hermetic coverage in `pkg/criserver/network_test.go`; gated live
smoke in `pkg/criserver/network_integration_test.go`
(`MACVZ_INTEGRATION=1 go test ./pkg/criserver -run 'Test.*Network|Test.*Sandbox'`).
`cmd/macvz-cri` exposes `--pod-cidr`/`--pod-network-interface` (networking off
until both are set). See [CRI_FEASIBILITY.md](CRI_FEASIBILITY.md) CRI-P5.

**CRI-P6 evidence (#78):** The kubelet-facing operational surfaces are honest over
the CRI path. Logs are file-based (`pkg/criserver/logs.go`): `StartContainer`
pumps the workload's follow stream into `<LogDirectory>/<LogPath>` in CRI format
(`<ts> stdout F <msg>`), with `ReopenContainerLog` for rotation. Exec/ExecSync and
PortForward (`pkg/criserver/streaming.go`) hand kubelet streaming URLs from
`k8s.io/kubelet/pkg/cri/streaming`; exec runs `container exec`, port-forward dials
the Pod micro-VM and proxies bytes. `Attach` returns a documented
`Unimplemented` (apple/container exposes no reattachable process stream). Stats
(`pkg/criserver/stats.go`) map the runtime `Stater` sample to
`ContainerStats`/`PodSandboxStats`, reporting attributes-only — never faked zeros —
when a sample is unavailable. Hermetic coverage in
`pkg/criserver/{streaming,logs,stats}_test.go`; gated live smoke in
`pkg/criserver/streaming_integration_test.go`
(`MACVZ_CRI_INTEGRATION=1 go test ./pkg/criserver -run 'Test.*Logs|Test.*Exec|Test.*PortForward|Test.*Stats'`).
`cmd/macvz-cri` exposes `--streaming-addr` (default `127.0.0.1:0`; exec/port-forward
return `FailedPrecondition` when empty). See
[CRI_FEASIBILITY.md](CRI_FEASIBILITY.md) CRI-P6.

**CRI-P7 evidence (#79):** Kubelet-driven Pod inputs and lifecycle behavior are
honest over the CRI path. In CRI mode the kubelet materializes projected
ConfigMap/Secret/Downward/SA-token and `emptyDir` content on the host and passes
them as bind mounts; `pkg/criserver/mounts.go` translates each into a
`types.Mount` (host bind or guest tmpfs for a Memory `emptyDir`) under a
conservative policy — mounts under the kubelet pods dir
(`--kubelet-pods-dir`, default `/var/lib/kubelet/pods`) are always allowed, any
other `hostPath` must be within a `--volume-host-path-allowed` prefix (empty
disables arbitrary hostPath), prefix matching is segment-aware, and bidirectional
propagation is rejected. Mounts are persisted and surface in
`ContainerStatus.Mounts`. Probes are kubelet-driven: HTTP/TCP from the kubelet
against the Pod IP (CRI-P5), exec via `ExecSync` (CRI-P6). `restartPolicy` is
honored by reporting exits faithfully and allowing recreate once the prior
container has Exited (only a live container blocks a new one).
`Server.RecoverContainers` (`pkg/criserver/recover.go`) reconciles persisted
containers against live workloads and resumes log pumps on restart without
duplicating or orphaning a workload, alongside `RecoverNetwork` for Pod IP/state.
Hermetic coverage in `pkg/criserver/{mounts,recover}_test.go`; gated live smoke in
`pkg/criserver/volumes_integration_test.go`
(`MACVZ_CRI_INTEGRATION=1 go test ./pkg/criserver -run 'Test.*Volume|Test.*Probe|Test.*Restart|Test.*Recovery'`).
Non-goals (multi-container shared volumes, subPath, dynamic PV) stay out of scope.
See [CRI_FEASIBILITY.md](CRI_FEASIBILITY.md) CRI-P7.

**CRI-P8 evidence (#80):** The experimental adapter is hardened into an
operator-facing k3s runtime path for single-container, non-host-namespace Pods.
`macvz-cri --preflight` (`cmd/macvz-cri/preflight.go`) reports runtime-dependency
status (apple/container CLI, socket, state dir, Pod networking, mount policy) as
`OK`/`WARN`/`FAIL` without mutating host state; its check logic is pure over
injectable probes and unit-tested. Unsupported Pod shapes
(`hostNetwork`/`hostPID`/`hostIPC`) are rejected by `RunPodSandbox` with a clear
`InvalidArgument` naming the spec field (`pkg/criserver/diagnose.go`).
`scripts/macvz-cri-install.sh` installs/uninstalls the adapter as a per-user
LaunchAgent — idempotent, preflighted, `KeepAlive`, with `uninstall`/`--purge`
leaving no stale socket/binary/state (`MACVZ_DRY_RUN=1` for rehearsal), and with
XML-escaped plist arguments plus `MACVZ_CRI_EXTRA_ARGS_FILE` for one-argument-per-line
extra flags when values contain spaces. The gated `test/e2e/cri-k3s/run.sh`
(`MACVZ_INTEGRATION=1`) drives the `crictl` compatibility suite — explicit
ImageService pull, lifecycle, logs, exec probe, projected config mount,
unsupported-shape rejection, adapter restart recovery, and cleanup verification —
and `test/e2e/cri-k3s/soak.sh` pulls once then runs a bounded create/delete soak
sampling adapter RSS with leak/orphan guards. k3s wiring is documented in
`test/e2e/cri-k3s/README.md`; `make cri-k3s`/`make cri-soak` are convenience
targets. Full `kubectl` fixture deployment and Service reachability remain CRI-P9
go/no-go evidence. No production-ready claim is made: the route-two go/no-go
(multi-container support, host-namespace workloads, real-hardware soak) is
deferred to CRI-P9. The Virtual Kubelet path is unchanged. See
[CRI_FEASIBILITY.md](CRI_FEASIBILITY.md) CRI-P8.

**CRI-P9 decision (#81):** The route-two go/no-go gate resolves to a **conditional
no-go for replacement, not a stop**: keep CRI as a documented experimental
`develop` side path; the shipped Virtual Kubelet provider remains the only
supported production runtime. The CRI adapter is honest and useful for the
single-container, non-host-namespace Pod class (P1–P8 have hermetic coverage plus
gated component/live harnesses; `go test ./...`, `go vet ./...`, `make build`,
`make cri` green), but it is still not a general node runtime. Follow-ups have
now narrowed the decision: #82/#86 prove the **current `apple/container` CLI path**
cannot model multi-container Pods by launching one independent micro-VM per
container, while #87 identifies `apple/containerization`'s experimental
`LinuxPod` API as the next route-C feasibility target; #84 clears
**host-namespace Pods** only by an honest scheduling-exclusion scheme, not by
pretending support; and #83/#85 separate CRI-socket soak evidence from real
kubelet/k3s in-loop evidence. Decision flips to **go** only after a LinuxPod-backed
Pod sandbox or equivalent runtime path proves Kubernetes Pod semantics and a
multi-day real k3s/kubelet in-loop soak passes; README positioning is unchanged
because the user-facing architecture does not change. Full decision package
(supported/unsupported workload shapes, gaps vs. Virtual Kubelet and vs.
ordinary CRI runtimes, k3s failure modes, operational + security model,
migration and fallback plans) is in [CRI_FEASIBILITY.md](CRI_FEASIBILITY.md)
CRI-P9.

**CRI-P9 follow-up (#82):** The multi-container blocker is confirmed
**architectural, not a missing flag**: `apple/container` runs one Linux kernel
per container, and a network namespace is per-kernel, so two micro-VMs cannot
share one. The exact missing primitive is *the ability to run a second OCI image
(own rootfs, lifecycle, limits) inside an existing Pod sandbox VM sharing that
VM's network namespace* — the pause-VM model used by Kata/Firecracker CRI
runtimes — recorded in code as `missingSharedNetnsPrimitive`. The adapter ships a
flag-gated path (`--experimental-multi-container`, `SharedPodNetworkRuntime` in
`pkg/criserver/multicontainer.go`): off by default it keeps the honest
one-container rejection; on, it admits a second container only if the runtime
implements `CreateInPodSandbox` (apple/container does not, so it rejects with a
diagnostic naming the gap). A hermetic test proves the adapter routes the second
container through that join operation with the first workload ID as the sandbox VM
target. An L3-shared-network approximation was rejected as dishonest (distinct
IPs break the single-Pod-IP/localhost contract). See
[CRI_FEASIBILITY.md](CRI_FEASIBILITY.md) "CRI-P9 Follow-up (#82)".

**CRI-P9 follow-up (#86):** Re-verified `apple/container` 1.0.0 CLI/runtime still
exposes no shared sandbox namespace (`--network "container:<id>"` → `network ...
not found`; no `pod`/`sandbox`/`pause` verb; no
`--net=container:`/`--pid`/`--ipc` flag), so the current CLI-backed CRI path
**stays blocked with evidence** and returns a clear `Unimplemented` diagnostic.
What #86 adds beyond #82 is the **adapter-side honest path** behind
`--experimental-multi-container`: container #2+ joins the **sandbox owner's** VM
via `CreateInPodSandbox` (never a second micro-VM), a joined container shares the
owner's **single Pod IP** with no second IP allocation or `binat` attach
(`store.Container.SharesPodNetwork`), per-container stop/remove keeps the Pod
network up **until the last container drains** (including owner-first stop order),
and a failed join leaks no record/IP/workload. Follow-up research found
`apple/containerization`'s experimental `LinuxPod` API, so the next step is no
longer "wait for `container` CLI support" or "self-build a runtime first"; it is
#87: validate `LinuxPod` as the route-C Pod sandbox backend and keep a MacVz-owned
sandbox runtime as fallback only. See [CRI_FEASIBILITY.md](CRI_FEASIBILITY.md)
"CRI-P9 Follow-up (#86)" and #87.

**CRI runtime feasibility (#87):** Route C is now the active research path:
validate `apple/containerization` `LinuxPod` before considering a MacVz-owned
sandbox runtime. C0 is complete in
[CRI_LINUXPOD_FEASIBILITY.md](CRI_LINUXPOD_FEASIBILITY.md): the decision is
**go to a minimal LinuxPod PoC**, not production adoption. `LinuxPod` is an
experimental Swift API that can place multiple Linux containers in one VM, with
separate rootfs/processes and shared VM CPU/memory/network; upstream integration
tests already cover multiple/concurrent containers, exec, stats, per-container
limits, filesystem isolation, optional shared PID namespace, and shared-network
sysctl evidence. The PoC must prove the Kubernetes-facing contract MacVz needs:
one Pod IP, localhost reachability across containers, sandbox lifetime
independent of any single container, kubelet-compatible
`RunPodSandbox`/`CreateContainer` ordering, logs/exec/stats, and the current
upstream gaps (`Attach`, `PortForward`, post-create `addContainer` hotplug). If
the PoC passes, create bridge implementation phases (likely a Swift helper daemon
controlled by the Go CRI adapter). #88 completed the first pure Swift
two-container shared-namespace PoC: one LinuxPod boots, two containers are
registered before `pod.create()`, localhost reaches across containers, exec and
stats work, and stopping the server first leaves the client observable. #89 then
tested kubelet-style ordering and confirmed `pod.addContainer` after
`pod.create()` returns `unsupported: "hotplug not supported"`. That means
LinuxPod can model predeclared multi-container Pods, but cannot currently provide
an honest general CRI backend for late sidecars/restarts without a deliberately
limited workload model or stop/recreate fallback. The PoCs do not yet prove vmnet
Pod IP attachment, `Attach`, `PortForward`, recovery, or k3s in-loop behavior.
#90 inspected the upstream hotplug path on `apple/containerization` 0.34.0:
post-create `addContainer` depends on `VirtualMachineInstance.hotplug`; the
default implementation returns `unsupported`, and the default
`VZVirtualMachineManager` path does not install a `HotplugProvider`. #91 then
installed a consumer provider and proved that post-create `addContainer` reaches
it; public VZ USB mass-storage attach succeeds, but no public API provides a
deterministic Linux guest block path for the attached ext4 rootfs, so MacVz
refuses to return a guessed `AttachedFilesystem`. Route C therefore should not
advance as a thin limited LinuxPod wrapper. #92 starts the deeper runtime
research track. The R0 decision is recorded in
[CRI_RUNTIME_R0_ARCHITECTURE.md](CRI_RUNTIME_R0_ARCHITECTURE.md): the target is
a true Pod VM runtime on Apple Silicon, including sandbox VM lifecycle,
guest-agent contract, deterministic rootfs hotplug/device discovery, CRI
mapping, networking, and recovery. The next smallest primitive is guest-side
hotplug device discovery, not a helper daemon or full runtime rewrite. See
[CRI_LINUXPOD_POC_REPORT.md](CRI_LINUXPOD_POC_REPORT.md),
[CRI_LINUXPOD_C2_REPORT.md](CRI_LINUXPOD_C2_REPORT.md),
[CRI_LINUXPOD_C3_DECISION.md](CRI_LINUXPOD_C3_DECISION.md),
[CRI_LINUXPOD_C4_REPORT.md](CRI_LINUXPOD_C4_REPORT.md),
[CRI_RUNTIME_R0_ARCHITECTURE.md](CRI_RUNTIME_R0_ARCHITECTURE.md), and issue #87.

**CRI-P9 follow-up (#84):** The host-namespace gate is **cleared via option (b),
an honest scheduling exclusion**. Honest host-namespace support is physically
impossible — a host namespace is per-kernel and each Pod is a separate micro-VM
kernel — so the answer is a kubelet-visible taint/label scheme (canonical keys in
`pkg/criserver/nodescheme.go`): labels `node.macvz.io/runtime=apple-container`
and `node.macvz.io/host-namespace=unsupported`, plus taint
`node.macvz.io/host-namespace-unsupported=true:NoSchedule`. Because system
DaemonSets often tolerate every taint, the scheme is layered: label-based
`nodeAffinity` exclusion for cluster-owned charts, and the existing loud
`RunPodSandbox` rejection (now naming the scheme) as the visible backstop for
charts you cannot edit — failing as a `FailedCreatePodSandBox` event, not
opaquely. The taint also makes MacVz an opt-in node for ordinary workloads:
compatible Pods must tolerate it and should select the runtime label.
`macvz-cri --preflight` prints the exact registration flags. See
[CRI_FEASIBILITY.md](CRI_FEASIBILITY.md) "CRI-P9 Follow-up (#84)".

**CRI-P9 follow-up (#85):** #83 intentionally stopped at a real-hardware
CRI-socket soak driven by `crictl`; it did not put a Linux kubelet/k3s control
plane in the loop. #85 **builds that missing layer**: a gated, operator-run
harness (`test/e2e/cri-k3s/k3s-inloop.sh`, `make cri-k3s-inloop`) plus a
single-container fixture (`test/e2e/cri-k3s/fixtures/workload.yaml`) that selects
the #84 runtime label and tolerates the host-namespace taint, and a runbook +
evidence template (`docs/CRI_K3S_INLOOP_REPORT.md`). The harness schedules the
fixture through a real k3s control plane and proves `kubectl rollout
status`/`logs`/`exec`/`port-forward`, ClusterIP/Service reachability from a
Linux-node probe, macvz-cri and k3s restart recovery, a soak (adapter RSS / Pod
restartCount / host workload counts), and a final orphan audit. It is gated
(`MACVZ_INTEGRATION=1` + reachable `KUBECONFIG`) and plan-only otherwise; restart
and audit phases use operator hooks and skip loudly when unset. The dev host
cannot stand up the Linux-control-plane + macOS-CRI-node topology unattended, so
the **live evidence is operator-pending**. Until that live run passes *and* #82
(multi-container) clears, CRI remains an experimental side path / no-go for
replacement even though #84 itself is resolved.

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
| #54 | Create a node bootstrap/join command or documented workflow | P7 | in progress — `macvz-kubelet bootstrap` renders a node config from the minimum join inputs (kubeconfig, node name, internal IP, pod CIDR, mesh address/peers) with optional WireGuard keypair + self-signed serving TLS; `macvz-kubelet doctor` preflights runtime, kubeconfig/API reachability, wireguard tooling, privileged helper, pf/forwarding, and serving-TLS validity, naming each missing prerequisite; workflow in [docs/NODE_JOIN.md](NODE_JOIN.md); tests in `pkg/bootstrap/` |
| #55 | Automate WireGuard key generation, public key exchange, and config rendering | P7 | in progress — `macvz-mesh` helper (`make mesh`): `keygen` (stable 0600 key), `export` (public-only node metadata, derives key/endpoint), `peer` (renders `mesh.peers:` YAML or wg `[Peer]` blocks that round-trip into config); rotation documented in [docs/NETWORKING.md](NETWORKING.md); tests in `pkg/config/metadata_test.go` + `pkg/network/wireguard/config_test.go` |
| #56 | Add node health and readiness diagnostics across runtime, provider, mesh, and pod network | P7 | in progress — `pkg/health` aggregates checks into a single report that distinguishes control-plane (registration + node-lease freshness), runtime (apple/container readiness), and data-plane (privileged helper, WireGuard mesh/routes, IP forwarding, pod attachments) failures, with text + JSON rendering and a ready/not-ready verdict that names the blocking class+check; served live from the kubelet at `/healthz/diagnostics` (200 ready / 503 not ready, `?format=json`) wired from the running components; complements join-time `macvz-kubelet doctor` (#54). Aggregation/checker tests in `pkg/health/`, wiring tests in `cmd/macvz-kubelet/diagnostics_test.go` |
| #57 | Add node drain and safe workload cleanup guidance/tooling | P7 | in progress — `kubectl drain` flow + expected controller rescheduling documented in [docs/NODE_DRAIN.md](NODE_DRAIN.md); per-Pod teardown already destroys VMs + detaches pod-network rules via `DeletePod`; new `macvz-kubelet cleanup` reaps orphan `macvz-*` micro-VMs (API-aware, `--dry-run`/`--all`) and flushes the pf anchor for post-drain/post-crash verification; `runtime.Lister`/`Driver.List` + exported `provider.WorkloadID`; tests in `pkg/drain/` |
| #58 | Add node removal workflow, including route, peer, pf, and VM cleanup | P7 | in progress — `macvz-kubelet remove` runs the departing node's permanent teardown in order, best-effort + idempotent: delete Node object → reap all MacVz micro-VMs (reuses #57 `pkg/drain`) → flush pf anchor → tear down WireGuard routes+interface (`wireguard.Mesh.Remove`); `--yes` safety gate, `--dry-run`/`--keep-node`, partial-failure reporting; remaining-node peer pruning stays a documented SIGHUP step. Orchestration in `pkg/noderemove` (+tests), wiring in `cmd/macvz-kubelet/remove.go` (+tests); runbook + recovery in [docs/NODE_REMOVAL.md](NODE_REMOVAL.md), ops section in [docs/MULTI_NODE_OPS.md](MULTI_NODE_OPS.md) |
| #59 | Produce a local diagnostic bundle command for support and bug reports | P7 | in progress — `macvz-kubelet bundle` collects config (loaded + raw), node object, recent events, live health report (#56), runtime status/containers/images, helper status, routes, IP forwarding, WireGuard interface, and pf anchor rules into a timestamped directory + tar.gz; every source is best-effort (a failing source records the error instead of aborting). All output passes through `pkg/diagbundle`'s redactor — PEM private keys, WireGuard private/preshared keys, JWT/bearer tokens, and the values of a curated sensitive-key set are stripped, while public material (certs, public keys, CA data) is kept for debugging. Redaction is the security chokepoint (Builder-level, so new sources cannot leak); tests in `pkg/diagbundle/` (redaction + packaging), docs in [docs/DIAGNOSTIC_BUNDLE.md](DIAGNOSTIC_BUNDLE.md) |
| #60 | Document multi-node operations, failure modes, and recovery playbooks | P7 | in progress — operator lifecycle runbook in [docs/MULTI_NODE_OPS.md](MULTI_NODE_OPS.md) covers join, verify, drain, remove, upgrade, network troubleshooting, and cleanup with commands + expected output; it is the order-of-operations index that ties together [NODE_JOIN.md](NODE_JOIN.md) (#54), the `macvz-mesh` peer workflow (#55), and the live `/healthz/diagnostics` report (#56), and routes each failure class to the per-symptom recovery procedures in [PRIVILEGED_NETWORKING.md](PRIVILEGED_NETWORKING.md). Drain (#57), automated removal (#58), and a diagnostic-bundle command (#59) are documented as the manual procedures operators run today, flagging where future tooling will replace a step. README + README.zh-TW link the runbook. Validation: two-Mac walkthrough (join→drain→remove with peer-list pruning). |
| #61 | Run a minimal public HTTP application and expose it through a browser-visible Service | P8 | in progress — `test/examples/hello-http/`: stock public `nginx:1.27-alpine` (arm64, no custom build/registry auth) Deployment fronted by a ClusterIP Service, serving a per-Pod page rendered from a ConfigMap via the Downward API so single-node vs. multi-node load-balancing is visible on refresh. Browser-visible path is `kubectl port-forward svc/hello` (no kube-proxy/LoadBalancer; proxies into the micro-VM via `pkg/provider/portforward.go`). `run.sh` harness applies, waits for rollout, asserts HTTP 200 + page body over a real port-forward, checks logs, then tears down (`MACVZ_HELLO_KEEP=1` leaves the namespace for manual browser smoke; `MACVZ_HELLO_REPLICAS=N` for multi-node spread). README documents browser verification + manual cheatsheet. Live bring-up on a kind control plane + single MacVz node proved node registration (`macvz-local` Ready) and an nginx micro-VM booting with a vmnet IP; surfaced and fixed two hard blockers only a live podNetwork run reveals: (1) the vmnet default-route guard crashed the kubelet at cold start (`route delete ... bad address: bridge100`) before any micro-VM created the bridge — now tolerated in [pkg/network/podnet/router.go](../pkg/network/podnet/router.go) (`TestStartToleratesMissingVMNetInterface`); (2) the always-present `default/kubernetes` Service (host-network apiserver, outside pod/vmnet CIDRs) poisoned the shared pf anchor and failed **every** Pod's network attach — the svcroute controller now filters Service backends to MacVz-routable CIDRs ([pkg/network/svcroute/controller.go](../pkg/network/svcroute/controller.go) + `Config.RoutableServiceCIDRs`, `TestReconcileFiltersUnroutableBackends`). Remaining: final HTTP-200-over-port-forward capture (automated in `run.sh`), deferred — the dual-NIC dev Mac matches the finding-#1 host-route-churn hazard, so the browser smoke should run on the two-Mac rig or a dedicated node. |
| #62 | Run a guestbook-style application with multiple Deployments, Services, ConfigMaps, and Secrets | P8 | in progress — `test/examples/guestbook/`: a multi-tier guestbook on two stock public arm64 images (no custom build/registry auth) — `redis-leader` (1) + `redis-follower` (2, `replicaof` the leader) data tier and a `frontend` (3) busybox `httpd`+CGI web tier. Three Deployments, three Services, two ConfigMaps (`APP_TITLE`/`app.conf` env + the landing page/CGI), one Secret (Redis password as env on Redis and as a read-only file on the frontend, #46/#47). The CGI speaks Redis RESP over `nc`: reads (`LRANGE`) from the follower replica, writes (`RPUSH`) to the leader, so the page proves replication across Deployments + ClusterIP routing (#37). Browser path is `kubectl port-forward svc/frontend` (no kube-proxy/NodePort). `run.sh` harness asserts rollout/availability, follower `master_link_status:up`, a real port-forward browser smoke (landing page + submit-an-entry write→leader/read←follower + HTML-escaping), logs banner, exec exit-code fidelity, `scale` 3→5→3, `rollout restart` with state surviving in Redis, and clean namespace teardown (no Pods/VMs left) — with redacted diagnostics. Wired into the #65 catalog + `run-all.sh`. Validation: full data path validated end-to-end with docker against the exact images (`redis:7-alpine`+`busybox:1.36.1`) — replication, urlencoded form posts, leader/follower split, ordering, HTML-escaping, AUTH enforcement; manifests offline-validated (3 Deploy/3 Svc/2 CM/1 Secret, namespace re-targeting); `run.sh`/`run-all.sh` pass `bash -n`. Live two-Mac browser smoke pending a running cluster. |
| #63 | Evaluate and run a Kubernetes management UI such as Headlamp or Dashboard on MacVz | P8 | in progress — evaluation in [docs/MANAGEMENT_UI.md](MANAGEMENT_UI.md) selects **Headlamp** over the Kubernetes Dashboard (single arm64 container vs. the Dashboard's multi-container gateway/scraper split, which MacVz's one-container-per-micro-VM model rejects). Fixture `test/e2e/headlamp-ui/`: hardened (restricted-PSS) single-replica Deployment using the projected SA token (#51) + ClusterIP routing to `kubernetes.default` (#37) for in-cluster API, HTTP readiness/liveness probes (#50), bound to the read-only `view` ClusterRole for RBAC-limited interaction. Browser path is `kubectl port-forward svc/headlamp` + a minted SA login token (no kube-proxy → no NodePort/LoadBalancer). `run.sh` asserts rollout/placement/probe-stability, projected-token mount, a real port-forward HTTP smoke of `/config`+SPA, and the RBAC boundary (`can-i list` yes / `delete`,`create`,`update` denied), with redacted diagnostics + teardown. Surfaced follow-ups: ingress/exposure without port-forward, multi-container UI support, and metrics-server panels. Validation: manifests offline-validated (YAML/selectors/labels/ports, render+image-override pipeline); live MacVz browser+RBAC smoke pending a running cluster. |
| #64 | Define and run a CBB arm64-compatible subset on MacVz | P8 | in progress — scoping/compatibility analysis in [docs/CBB_VALIDATION.md](CBB_VALIDATION.md). CBB = Cloud-Barista (`CB-`prefixed Go microservices). Defines the supported subset as the **stateless control services** (CB-Spider, CB-Tumblebug, CB-MapUI) — each single-container, wired by ClusterIP routing without kube-proxy (#37), config/secrets via #46–#49, probes via #50, browser access via `kubectl port-forward` (#28), with CB-Tumblebug pointed at an **external** key-value/RDB store so the on-node subset stays stateless. Unsupported components listed with reasons + required MacVz features: CB-Dragonfly monitoring stack and any in-cluster etcd/MariaDB/InfluxDB need dynamic PV provisioning + StatefulSet support (MacVz volumes are ephemeral VirtioFS, #26); amd64-only images need confirmed multi-arch builds; GPU/NVIDIA and hostPath/privileged/host-network agents stay on regular Linux nodes (#52); NodePort/LoadBalancer exposure blocked by no-kube-proxy (shared follow-up with #63). Scoped per the user's steer to a roadmap/blocker analysis rather than standing up a live CBB service; runnable manifests + expected outputs deferred to #65. Validation: component shapes classified against MacVz's feature set; no CBB service deployed (per scope), live subset smoke pending a running cluster. |
| #65 | Publish real-app validation manifests and expected outputs | P8 | in progress — published catalog [`test/examples/README.md`](../test/examples/README.md) + suite runner [`test/examples/run-all.sh`](../test/examples/run-all.sh) consolidating the complete, assertable P8 fixtures (`hello-http` #61, `p6-compat` #53, `headlamp-ui` #63, `guestbook` #62) into one repeatable entry point. `run-all.sh` runs each fixture's self-contained apply→assert→teardown harness in sequence (cheapest first), aggregates a pass/fail summary, supports id subsets (`./run-all.sh hello-http p6-compat`), `--list`, and suite-wide pass-through knobs (`KUBECONFIG`/`KUBECTL`/`MACVZ_E2E_TIMEOUT`/`MACVZ_E2E_CONTINUE`); exits non-zero if any fixture fails, while each fixture attempts its own namespace teardown before returning. README documents per-fixture expected output (pods, services, logs, browser behavior, pass line, cleanup) so failures are localizable, plus a "when a fixture fails" diagnostics guide; the guestbook fixture (#62) is now published and wired in, leaving the CBB subset (#64) as the remaining not-yet-published fixture. Validation: all four fixtures' manifest sets pass `kubectl apply --dry-run=client`; every `run.sh` plus `run-all.sh` pass `bash -n`; `run-all.sh` `--list`/`--help`/unknown-id/orchestration smoke-tested. Live clean-cluster `run-all.sh` pending a running MacVz cluster. |
| #66 | Implement kubelet restart recovery for existing apple/container workloads | P9 | in progress — micro-VMs outlive the kubelet process, so on restart the provider re-adopts running workloads instead of rebuilding them. `CreatePod` ([pkg/provider/pod.go](../pkg/provider/pod.go)) gained an adoption fast-path: it probes the runtime for the Pod's deterministic `WorkloadID` (`lookupWorkload`) and, when the micro-VM is already there, skips pull/create, records the existing workload, starts it only if recovery finds it stranded in Created, re-attaches the Pod network path, and resumes probing. Pod IPs stay stable because `RecoverAllocations` (#20) already reserves each Pod's recorded `Status.PodIP` before the controller runs. Adoption avoids a needless re-pull that could now fail (e.g. rotated `imagePullSecret`) and strand a healthy container; the driver's idempotent `Create` is the backstop so a missed probe can't duplicate a VM, and `unwindCreate` leaves an adopted VM running if a mid-recovery re-attach fails. Tests in `pkg/provider/pod_test.go` (adopt-without-pull/create/start for Running VMs; start a recovered Created VM; restart a recovered Stopped VM; adoption succeeds when the image can no longer be pulled); docs in [docs/WORKLOADS.md](WORKLOADS.md#kubelet-restart-recovery-66). Validation: `go test ./...`/`go vet ./...` green; live restart smoke (kubelet restart → Pod back to Running, same IP, no new VM) pending a running cluster. Orphan VMs whose Pod is gone are out of scope here (#67). |
| #67 | Add orphan micro-VM detection and cleanup policy | P9 | in progress — automatic in-kubelet orphan reaper in [pkg/orphan](../pkg/orphan/reaper.go): a periodic loop that destroys MacVz micro-VMs whose backing Pod no longer exists on this node (missed delete, or a Pod removed while the kubelet was down), reclaiming pinned CPU/RAM/disk. It is the continuous counterpart to restart-adoption (#66) and the operator-run `cleanup`/`remove` sweeps (#57/#58): it reuses `drain.Cleaner` Scan/Reap but adds a **grace period** (a VM must stay continuously orphaned across `gracePeriod` before reaping, so it never races Pod creation/adoption) and **fail-safe** behaviour (reaps nothing when the live-Pod set can't be determined). Expected-set source is the node-scoped Pod informer cache (no extra API calls; Pods being deleted or in unsupported init/ephemeral/multi-container shapes excluded); only `macvz-`prefixed VMs are ever touched. It deliberately does **not** flush the pf anchor (the node is live). Config `orphanCleanup{enabled,interval,gracePeriod,dryRun}` (default on, 2m/10m) in [pkg/config](../pkg/config/config.go) with validation; wired in [cmd/macvz-kubelet](../cmd/macvz-kubelet/serve.go) after the pod controller syncs; docs in [docs/ORPHAN_CLEANUP.md](ORPHAN_CLEANUP.md) + config.example.yaml. Tests in `pkg/orphan/reaper_test.go` (grace gating, grace reset when a Pod returns, dry-run, fail-safe on lister error, failed-destroy retry, non-MacVz VMs untouched) and config interval validation. Validation: `go build ./...`/`go vet`/targeted `go test` green; live orphan-after-crash smoke pending a running cluster. |
| #68 | Improve node and Pod resource accounting for CPU, memory, disk, and image cache usage | P9 | in progress — extends the #25 CPU/memory metrics with **disk and image-cache accounting**. New optional `runtime.DiskReporter` capability (`pkg/runtime/disk.go`: `FilesystemUsage`, `ImageCacheUsage`, `ErrDiskUsageUnavailable`) implemented by the apple/container driver (`pkg/runtime/container/disk.go`): node filesystem via `statfs(2)` on configurable `runtimeDataRoot` (falling back to the operator home directory; `statfs_unix.go`/`statfs_other.go`, injectable seam), image-cache size+count by summing `container image ls --format json` (tolerant of numeric/string/variant sizes). The metrics collector now populates the Summary API's node `fs` (capacity/used/available + inodes, for disk-pressure eviction) and `runtime.imageFs.usedBytes`, plus five Prometheus gauges (`macvz_node_filesystem_{capacity,used,available}_bytes`, `macvz_image_cache_bytes`, `macvz_image_cache_images`); each surface degrades independently (`DiskSample` per-field ok). `BuildNode` advertises node `ephemeral-storage` capacity/allocatable from the filesystem total/available (operator-set values preserved; caller map never mutated). Disk stays node-scoped — no per-Pod ephemeral attribution yet. Tests: `pkg/metrics/disk_test.go`, `pkg/runtime/container/disk_test.go`, `pkg/provider/node_test.go`; docs in [docs/METRICS.md](METRICS.md). Validation: `go test ./...`/`go vet ./...` green; live `kubectl top`/`statfs` numbers pending a running cluster. |
| #69 | Add log rotation and structured diagnostics for long-running nodes | P9 | in progress — log locations, rotation, and structured-logging conventions defined in [docs/LOGGING.md](LOGGING.md). The privileged helper runs under launchd `KeepAlive` and grew `/var/log/macvz-netd*.log` without bound; `macvz-netd install` now also drops a `newsyslog` rotation config (`/etc/newsyslog.d/macvz-netd.conf`, `DefaultNewsyslogPath`) so macOS's `com.apple.newsyslog` rotates both logs (size-driven ~5 MB, 7 bzip2 archives; tunable via `LogRotateCount`/`LogRotateSizeKB`, opt out by clearing `NewsyslogPath`), and `uninstall` removes it. Rendering is pure (`RenderNewsyslog`) mirroring the plist pattern; install/uninstall wired in [pkg/network/privhelper/launchd.go](../pkg/network/privhelper/launchd.go), surfaced in the install log line. Kubelet bounding documented two ways (klog `--log-file`/`--log-file-max-size`, or stderr redirect + newsyslog). Structured diagnostics already use `klog.*S` with workload/pod (`pod`,`workloadID`,`podIP`,`phase`), node, and helper-op (`name`,`args`,`exit`) context (probes at `V(2)`); conventions tabulated in the doc. Tests in `pkg/network/privhelper/newsyslog_test.go` + extended launchd install/uninstall. Validation: `go test ./pkg/network/privhelper/...` green; live long-running rotation smoke pending a running node. |
| #70 | Build install, upgrade, rollback, and uninstall packaging for macvz-kubelet and helper | P9 | in progress — unified installer [`scripts/macvz-install.sh`](../scripts/macvz-install.sh) packaging both long-running pieces as managed macOS services: `macvz-netd` (root LaunchDaemon, delegated to its own `install`/`uninstall`) and `macvz-kubelet` (per-user LaunchAgent, since `apple/container` refuses root). Versioned layout (`<prefix>/libexec/macvz/versions/<v>` + `current`/`previous` symlinks) makes upgrade reversible: `install` stages binaries + seeds `<etc>/config.yaml` only if absent + starts both services; `upgrade` flips `current` (recording `previous`) and reloads, preserving config/PKI/state; `rollback` returns to `previous` and is itself reversible; `uninstall [--purge]` removes services+binaries and optionally `<etc>`. Release wired up: [scripts/macos-release.sh](../scripts/macos-release.sh) now builds+signs **both** binaries and emits a self-contained `macvz_<version>_darwin_arm64` install bundle (binaries + installer + config template + packaging doc) alongside the single-binary tarball, with both binaries submitted for notarization. Operator guide [docs/PACKAGING.md](PACKAGING.md); `make install-rehearsal`. Validation: a no-root rehearsal [`scripts/macvz-install-rehearsal.sh`](../scripts/macvz-install-rehearsal.sh) drives install→upgrade→rollback→uninstall→purge in a temp prefix (launchctl/netd stubbed) and asserts symlink state, reversible rollback, and config preservation — all green; `make release` builds the signed bundle end-to-end (ad-hoc) and the bundle tarball contains both binaries + installer; scripts pass `bash -n`. Live install/upgrade/rollback/uninstall rehearsal on a real Mac (services started, node joins/drains) pending. |
| #71 | Run long-duration soak tests across kubelet/helper restarts and node churn | P9 | in progress — added the P9 soak harness [`test/e2e/soak/run.sh`](../test/e2e/soak/run.sh) plus operator guide [docs/SOAK_TESTS.md](SOAK_TESTS.md). The harness loops the existing multi-node e2e suite and optionally the P8 real-app catalog, captures before/after node resource and Summary API snapshots, cordon/uncordon churns a MacVz node each iteration, and uses operator-provided command hooks to restart kubelet/helper services, verify #66 restart recovery (same PodIP after kubelet restart), create an orphan while kubelet is stopped and verify #67 reaps it after the grace window, and run #57 cleanup dry-run as a final no-orphan check. `make soak` is the entry point. Validation: `bash -n test/e2e/soak/run.sh` green; live multi-hour run on the two-Mac rig pending. |
