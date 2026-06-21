# CRI Feasibility Track

This track evaluates whether MacVz can move from a Virtual Kubelet provider to a
Kubernetes CRI runtime path backed by `apple/container`.

The current shipped architecture remains Virtual Kubelet. This document is a
feasibility plan and evidence log for a possible route change, not a commitment
to replace the provider path before the CRI risks are understood.

## Target Shape

The desired route-two architecture is:

```text
k3s / kubelet
  -> CRI RuntimeService + ImageService
    -> MacVz CRI adapter
      -> apple/container
        -> Linux micro-VM workload on Apple Silicon
```

If feasible, k3s or a regular kubelet would talk to MacVz through the standard
CRI socket instead of scheduling to a Virtual Kubelet node.

## Phase Plan

| Phase | Goal | Exit Criteria |
| --- | --- | --- |
| CRI-P0 | Feasibility evidence | `apple/container` command surface and current MacVz runtime abstractions are mapped to CRI; hard blockers are named. |
| CRI-P1 | CRI skeleton | kubelet can connect to a MacVz CRI socket and receive sane `Status` responses. |
| CRI-P2 | Pod sandbox spike | `RunPodSandbox`, `StopPodSandbox`, `RemovePodSandbox`, and `PodSandboxStatus` work for a minimal sandbox model. |
| CRI-P3 | Single-container Pod | kubelet can create/start/stop/remove one container in one sandbox using a public arm64 image. |
| CRI-P4 | Image service | pull/list/status/remove image flows work, including registry auth and arm64/Rosetta policy. |
| CRI-P5 | CNI and Pod networking | kubelet-driven Pod networking has a repeatable lifecycle on macOS without manual route/pf steps. |
| CRI-P6 | Logs, exec, attach, port-forward, stats | common `kubectl` operations work through kubelet over CRI surfaces. |
| CRI-P7 | Volumes, projected data, probes | ConfigMaps, Secrets, ServiceAccounts, emptyDir, hostPath policy, and probes behave like regular kubelet workloads. |
| CRI-P8 | k3s compatibility hardening | A k3s node can run a compatibility suite and survive restart/cleanup/upgrade tests. |

## CRI-P0 Scope

CRI-P0 should answer four questions:

1. Can `apple/container` expose enough lifecycle primitives for kubelet CRI?
2. Can MacVz model Kubernetes Pod sandbox semantics on top of
   `apple/container` without lying to kubelet?
3. Can networking be integrated with kubelet/CNI lifecycle instead of the
   current Virtual Kubelet side path?
4. Are the unknowns small enough to justify CRI-P1/P2 implementation work?

## Current Evidence

Collected on 2026-06-21 from this development host:

```text
container CLI version 1.0.0 (build: release, commit: unspeci)
container system status: running
installRoot: /opt/homebrew/Cellar/container/1.0.0_1/
appRoot: /Users/chimera/Library/Application Support/com.apple.container/
```

The CLI exposes useful primitives:

- container lifecycle: `create`, `start`, `stop`, `delete`, `inspect`, `list`
- image lifecycle: `image pull`, `image inspect`, `image list`, `image delete`
- interactive surfaces: `logs`, `exec`
- resource accounting: `stats --format json --no-stream`
- filesystem ingress: `--volume`, `--mount`, `--tmpfs`
- process options: env, user/group, cwd, tty/stdin, ulimit
- network options: `--network`, `--dns`, `--dns-search`, `--publish`

These are enough for a CRI skeleton and a single-container Pod spike, but not
yet enough to declare the full route feasible.

## CRI Mapping

| CRI Area | `apple/container` Surface | Feasibility | Notes |
| --- | --- | --- | --- |
| RuntimeService `Status` | `container system status` | Likely | Already used by the current runtime `Ready` check. |
| ImageService pull/status/list/remove | `container image pull/inspect/list/delete` | Likely | Registry auth is global runtime state today; concurrent authenticated pulls need serialization. |
| Create/start/stop/remove container | `container create/start/stop/delete` | Likely | Existing `pkg/runtime/container` already wraps these operations. |
| Container status | `container inspect`, `container list --all --format json` | Likely | Existing parser maps lifecycle states and guest IPs. |
| Logs | `container logs [-f] [-n]` | Likely | Already wired to `kubectl logs` through Virtual Kubelet. |
| Exec | `container exec [-i] [-t]` | Likely | Already wired; attach semantics still need separate validation. |
| Stats | `container stats --format json --no-stream` | Likely | Existing stats parser feeds metrics. |
| Volumes | `--volume`, `--mount`, `--tmpfs` | Partial | Projected data can be materialized on host and bind mounted; kubelet-managed mounts must be reconciled with macOS paths. |
| Pod sandbox | No native CRI sandbox object | High risk | Need a MacVz-owned sandbox model. One `apple/container` VM per Kubernetes container does not equal one Pod sandbox. |
| Multi-container Pod | Not represented by current MacVz model | High risk | The current provider rejects multi-container Pods. CRI kubelet expects multiple containers can share one Pod sandbox. |
| CNI lifecycle | No direct kubelet/CNI integration yet | High risk | Current data plane is MacVz-managed WireGuard/pf/route. CRI needs deterministic ADD/DEL timing around sandbox lifecycle. |
| Port-forward / attach | CLI surfaces exist only partially | Unknown | Need a kubelet-facing streaming server implementation and live behavior tests. |
| Checkpoint/restart recovery | Current adoption is provider-side | Unknown | CRI state store needs to survive adapter restarts and match kubelet expectations. |

## Phase 0 Decision

CRI-P0 is **conditionally positive**:

- Proceed to CRI-P1/P2 only as an isolated `develop` track.
- Do not replace the Virtual Kubelet architecture yet.
- Treat Pod sandbox, multi-container Pod semantics, and kubelet/CNI networking as
  the three make-or-break risks.

The next concrete milestone is a tiny CRI server that satisfies kubelet
connection and `Status`, followed by a sandbox spike that proves whether a
single-container Pod can be honestly represented without breaking kubelet
expectations.

## CRI-P1: Minimal CRI Server Skeleton

CRI-P1 ships an **experimental** CRI server skeleton. It is not the default
MacVz runtime mode and is intentionally separate from the shipped Virtual
Kubelet provider (`cmd/macvz-kubelet`). It exists to prove the CRI server
process, gRPC wiring, and basic RuntimeService/ImageService responses are
compatible enough for `kubelet`/`crictl` to connect.

- Command: `cmd/macvz-cri` (build with `make cri` → `bin/macvz-cri`).
- Package: `pkg/criserver` implements `RuntimeServiceServer` and
  `ImageServiceServer` from `k8s.io/cri-api/pkg/apis/runtime/v1`.
- Listen: `--listen unix:///tmp/macvz-cri.sock` (a bare absolute path also works).

What the skeleton answers:

- `Version` — CRI handshake (`RuntimeApiVersion: v1`, name `macvz`).
- `Status` — `RuntimeReady=true` (the server is up) and `NetworkReady=false`
  with an explicit reason, since CNI/Pod networking is out of scope for this
  phase. `--verbose` adds an `experimental`/`track` info map.
- `ListPodSandbox`, `ListContainers`, `ListImages` — empty lists.
- `ImageFsInfo` — empty (no image store tracked).
- Every other CRI method returns `codes.Unimplemented` via the embedded
  `Unimplemented*Server` defaults.

Deliberately **not** in scope here: Pod sandboxes, image pulls, starting
`apple/container` workloads, and any host networking. The skeleton carries no
`apple/container` assumptions.

Quick check:

```sh
make cri
./bin/macvz-cri --listen unix:///tmp/macvz-cri.sock &
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock version
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock info
```

## CRI-P2: State-Only Pod Sandbox Spike

CRI-P2 extends the same experimental adapter (`cmd/macvz-cri`, `pkg/criserver`)
with a **state-only** Pod sandbox lifecycle. A sandbox here is a metadata record
with a lifecycle — it pulls no images, boots no micro-VM, and touches no host
networking. The goal is to validate the kubelet/`crictl` sandbox lifecycle and
status contract before committing to any data-plane work.

Implemented `RuntimeService` methods:

- `RunPodSandbox` — generates a 64-hex-char sandbox ID, records CRI metadata
  (namespace/name/UID/attempt), labels, annotations, hostname, log directory,
  DNS config, and runtime handler, and marks the sandbox `READY`. It validates
  that namespace, name, and UID are present (`InvalidArgument` otherwise).
- `StopPodSandbox` — transitions to `NOTREADY`; idempotent, and a no-op success
  for an already-stopped or absent sandbox (kubelet calls Stop repeatedly).
- `RemovePodSandbox` — deletes the record; idempotent for an absent sandbox.
- `PodSandboxStatus` — returns the record, or `NotFound` if absent. The
  `Network` field is deliberately `nil`: the state-only model owns no Pod IP and
  must not fake one.
- `ListPodSandbox` — supports the CRI filter (id, state, label selector).

State store (`pkg/criserver/store`):

- One JSON file per sandbox, written atomically (temp + rename), under
  `--state-dir` (default `~/.macvz/cri/sandboxes`). An empty `--state-dir` runs
  in memory only.
- Records survive an adapter restart, satisfying the CRI restart-tolerance
  expectation. A corrupt record on load is skipped (counted, logged), not fatal.

Sandbox-to-Pod mapping: each record stores the Kubernetes namespace, name, and
UID, so a sandbox ID maps unambiguously back to its Pod identity.

Honesty boundary: container creation (`CreateContainer`/`StartContainer`), CNI
ADD/DEL, image pulls, and host networking all remain `Unimplemented` via the
embedded `Unimplemented*Server`. The spike never returns a fake success for a
capability it does not have.

Validated end-to-end over a real gRPC Unix socket (a `crictl` stand-in, since
`crictl` is not installed on the dev host): `runp → pods → inspectp → stopp →
rmp` round-trips correctly, `inspectp` reports `network=<nil>`, and
`CreateContainer` returns `Unimplemented`. With `crictl` installed the same flow
is:

```sh
make cri
./bin/macvz-cri --listen unix:///tmp/macvz-cri.sock &
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock runp sandbox.json
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock pods
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock inspectp <id>
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock stopp <id>
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock rmp <id>
```

### CRI-P2 Decision

The state-only sandbox model is **honest and credible enough to continue**, with
one explicit caveat to carry into CRI-P3.

The kubelet/`crictl` sandbox lifecycle (run/stop/remove/status/list), its
idempotency requirements, restart tolerance, and the sandbox-ID-to-Pod-identity
mapping are all satisfiable without lying to the client. The risk named in
CRI-P0 — that MacVz has no native CRI sandbox object — is resolved: a
MacVz-owned metadata record is a faithful sandbox identity/lifecycle owner.

What the spike does **not** yet prove, and what CRI-P3 must decide, is the
**container-to-sandbox topology**. `apple/container` runs one Linux micro-VM per
container, but a Kubernetes Pod sandbox is expected to own shared network
identity and shared volumes for all its containers. The realistic next model to
try is:

- **single-container Pod restriction for the first kubelet spike** — one sandbox
  owns exactly one `apple/container` micro-VM. This is the smallest honest step:
  it sidesteps the shared-network/shared-volume problem entirely while proving
  the create/start/stop/remove container path end-to-end through kubelet. A
  multi-container Pod is rejected with a clear, explicit error rather than
  silently mismodeled.

The **pause-like sandbox VM plus workload VMs** model (mirroring a pause
container) is the eventual target for honest multi-container Pods, but it
requires shared-netns semantics across micro-VMs that `apple/container` does not
expose today; it is deferred until the single-container path is proven and the
CRI-P5 networking story exists. The CRI route is **not** stopped.

## CRI-P3: Single-Container Pod Spike

CRI-P3 takes the single-container step CRI-P2 recommended: one CRI Pod sandbox
owns exactly one `apple/container` micro-VM, driven through the existing
`pkg/runtime/container` driver. It stays narrow on purpose — no shared Pod
network, no shared volumes, no multi-container support — so the container
lifecycle is proven end-to-end before any data-plane work.

Implemented `RuntimeService` methods (`pkg/criserver/container.go`):

- `CreateContainer` — validates the sandbox exists and is `READY`, enforces the
  one-container-per-sandbox rule, pulls the image (the `ImageService` is out of
  scope, so create is self-sufficient and the driver's pull verifies the arm64
  variant), provisions the workload, and persists the record. The workload is
  reclaimed if persistence fails, so a create leaves neither an orphan record nor
  an orphan VM.
- `StartContainer` — boots the workload; requires the `Created` state.
- `StopContainer` — stops the workload, captures its exit code/reason, marks the
  record `Exited`; idempotent for an already-exited container.
- `RemoveContainer` — destroys the workload and deletes the record; idempotent
  for an absent container.
- `ContainerStatus` — returns the record (`NotFound` if absent) and reconciles it
  against the live workload, so a container that exited on its own — or after an
  adapter restart — is reported `EXITED` with its real exit code, not a stale
  `RUNNING`.
- `ListContainers` — supports the CRI filter (id, sandbox id, state, label
  selector), replacing the CRI-P1 always-empty stub.

Container state store (`pkg/criserver/store/container.go`):

- One JSON file per container, written atomically, under a `containers/`
  subdirectory of `--state-dir` (separate from sandbox records so the two stores
  never read each other's files). An empty `--state-dir` runs in memory only.
- Records survive an adapter restart; a corrupt record on load is skipped
  (counted, logged), not fatal.

CRI container ID vs. workload ID: CRI container IDs are generated separately
(64-hex), and the `apple/container` workload name is derived **deterministically**
from the container ID (`macvz-cri-<id-prefix>`, see `store.DeriveWorkloadID`). A
restarted adapter recomputes the same workload name without extra state, and the
derived name stays within the runtime's name-length limits.

Honesty boundaries:

- A **second** container in a sandbox is rejected with `FailedPrecondition`
  naming the existing container — multi-container Pods are not silently
  mismodeled.
- With **no runtime configured** (the default skeleton), the container methods
  return `FailedPrecondition` ("no container runtime is configured"), never a
  fake success or a misleading `Unimplemented`.
- Env var **ordering** is not preserved (CRI's ordered list is flattened into the
  driver's `Env` map); acceptable for the single-container spike, noted for
  CRI-P4.
- The container has **no Pod network** — it runs on the default `apple/container`
  network only. This is acceptable for CRI-P3 and explicitly deferred to CRI-P5.

Testing: the default `go test ./...` run is hermetic — the lifecycle is exercised
against a fake `ContainerRuntime` covering create/start/stop/remove/status/list,
missing sandbox, duplicate/second-container create, start-from-wrong-state,
idempotent stop/remove, status reconcile on self-exit, and restart/reload. A
gated live test (`MACVZ_CRI_INTEGRATION=1`) drives the same path through a real
`apple/container` service with a public arm64 image and asserts no orphan
workload remains. With `crictl` installed the manual flow is:

```sh
make cri
./bin/macvz-cri --listen unix:///tmp/macvz-cri.sock --state-dir /tmp/macvz-cri-state
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock runp sandbox.json
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock create <sandbox-id> container.json sandbox.json
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock start <container-id>
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock ps -a
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock inspect <container-id>
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock stop <container-id>
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock rm <container-id>
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock stopp <sandbox-id>
crictl --runtime-endpoint unix:///tmp/macvz-cri.sock rmp <sandbox-id>
```

### CRI-P3 Decision

The single-container path is **proven and honest**. One CRI sandbox mapping to
one `apple/container` micro-VM satisfies the create/start/stop/remove/status/list
contract, restart tolerance, and the container-ID-to-workload-ID mapping without
faking any capability. The container-to-sandbox topology question CRI-P2 raised
is resolved for the single-container case; multi-container Pods remain explicitly
rejected.

Carry into **CRI-P4/P5**:

- **CRI-P4** should wire a minimal `ImageService` (`PullImage`/`ImageStatus`/
  `ListImages`/`RemoveImage`) so image lifecycle is driven by the CRI client
  rather than implicitly by `CreateContainer`, and preserve env ordering. It is
  also the right phase to attempt a real kubelet/k3s node join against this
  adapter.
- **CRI-P5** owns Pod networking: a Pod IP, CNI ADD/DEL (or the MacVz podNetwork
  equivalent), and `PodSandboxStatus.Network`. Only once that exists does the
  **pause-like sandbox VM plus workload VMs** model for honest multi-container
  Pods become tractable. The CRI route remains **not** stopped.

## CRI-P4: ImageService

CRI-P4 moves image lifecycle off `CreateContainer` (where CRI-P3 pulled
implicitly) and onto the CRI `ImageService`, where kubelet and `crictl` expect
it. The adapter now implements all five image methods over the apple/container
image store (`pkg/criserver/image.go`, driven by the new
`runtime.ImageManager` capability in `pkg/runtime/container/image.go`):

| CRI method | apple/container command | Notes |
| --- | --- | --- |
| `PullImage` | `image pull` (+ optional `registry login/logout`) | Reuses the driver's arch-verifying `Pull`; returns a runtime-usable image ref. |
| `ImageStatus` | `image inspect` | Absent image → empty, non-error response (CRI contract). |
| `ListImages` | `image ls --format json` | Honours the reference filter (ID / RepoTag / RepoDigest). |
| `RemoveImage` | `image delete` | Idempotent: removing an absent image succeeds. |
| `ImageFsInfo` | `image ls` + `statfs(2)` | Reports image-cache used bytes + data-root mountpoint/inodes, or degrades to an empty response — never fabricated values. |

Design decisions:

- **`CreateContainer` no longer pulls** when the ImageService is wired. It
  verifies the image is already present via `ImageStatus` and returns
  `FailedPrecondition` if not, directing the caller to `PullImage` first — the
  normal kubelet/`crictl` order. A container-runtime-only configuration (no
  ImageService) keeps the CRI-P3 fallback of pulling in `CreateContainer`, so
  the single-container spike still works standalone.
- **Image ID is honest and runtime-usable.** apple/container's image metadata
  does not map cleanly onto CRI's fields, so the ID prefers a repo digest
  (`name@sha256:...`) that kubelet can feed back into `CreateContainer` and
  `RemoveImage`; it degrades to the canonical reference when no digest is
  available, rather than fabricating one. `RepoDigests` includes the repo digest
  and, when useful for matching, the raw digest.
- **Registry auth is implemented for username/password.** CRI `AuthConfig`
  username/password (or a base64 `user:password` in `Auth`) maps to the existing
  `runtime.RegistryAuth` login/pull/logout flow (#49), which serialises per
  registry server. When `ServerAddress` is empty the registry host is derived
  from the image reference (Docker Hub short names default to `docker.io`).
  **Token credentials** (`IdentityToken`/`RegistryToken`) are **not** supported —
  apple/container's `registry login` takes only username/password — so they are
  rejected with an explicit `Unimplemented` rather than silently dropped.
- **`arm64`/Rosetta policy is preserved.** Pull still goes through the driver's
  `selectPlatform` arch verification, so an image with no bootable variant fails
  at `PullImage` with the same actionable `ErrIncompatibleArch` as before.

Testing: hermetic tests cover the driver image methods against the fake CLI
runner (`pkg/runtime/container/image_test.go`) and the CRI ImageService against a
fake image runtime, including auth mapping, the filter, the FsInfo degrade paths,
and the `CreateContainer` no-implicit-pull behaviour
(`pkg/criserver/image_test.go`). A gated live test
(`MACVZ_CRI_INTEGRATION=1`, `pkg/criserver/image_integration_test.go`) drives
`PullImage → ImageStatus → ListImages → ImageFsInfo → RemoveImage` against a real
apple/container service, and the single-container lifecycle live test now pulls
through the ImageService first.

### CRI-P4 Decision

The image lifecycle is **proven and honest** over the CRI ImageService. The
adapter no longer pulls implicitly, registry username/password auth is wired
through the existing driver flow, and every surface that cannot report real data
(token auth, absent digests, unsampleable image filesystem) degrades explicitly
rather than faking a value.

Carry into **CRI-P5**: Pod networking is the remaining blocker before a real
kubelet/k3s node join is worthwhile — a Pod IP, CNI ADD/DEL (or the MacVz
podNetwork equivalent), and `PodSandboxStatus.Network`. Only once that exists
does the pause-like sandbox-VM model for honest multi-container Pods become
tractable. A kubelet join attempt against the current image+container surface is
a reasonable exploratory smoke but is expected to stall at Pod networking. The
CRI route remains **not** stopped.

## CRI-P5: Pod Networking Lifecycle

CRI-P5 gives the experimental adapter an honest Pod networking lifecycle (#77).
It deliberately **reuses the shipped Virtual Kubelet networking primitives**
rather than inventing a CRI-only path: Pod IPs come from `network.PodIPAM` (the
node's Kubernetes-assigned Pod CIDR, #20) and the host packet-filter path comes
from `podnet.Router` (one pf `binat` rule per micro-VM, #22). The CRI adapter
reaches both through narrow interfaces (`PodNetwork`, `PodIPAllocator` in
`pkg/criserver/network.go`) so the orchestration is testable against fakes and so
the provider and adapter share identical allocation/attach semantics.

**Where each step happens.** CRI splits the lifecycle differently from the
provider, which allocates the IP, boots the VM, observes its address, and
attaches in one `CreatePod`. In CRI:

| CRI method | Networking action |
| --- | --- |
| `RunPodSandbox` | Reserve a Pod IP from IPAM, keyed by Pod identity (`namespace/name`). No attach yet — there is no micro-VM. |
| `StartContainer` | After the workload starts, poll for the micro-VM's host-only address, then program the `binat` rule via the Router and record the attachment. This is the first point the VM IP exists. |
| `PodSandboxStatus` | Report `Network.Ip` **only** once the path is actually attached; a reserved-but-unattached IP is withheld. |
| `Status` | `NetworkReady=true` **only** when both IPAM and the Router are wired (a half-configured path can't produce a reachable Pod). |
| `StopContainer` / `RemoveContainer` / self-exit reconcile | Detach the Pod network path when the single backing micro-VM reaches a terminal or removed state; retain the Pod IP reservation until sandbox removal. |
| `StopPodSandbox` | Detach the path idempotently; retain the IP reservation so a stop/start keeps the same address. |
| `RemovePodSandbox` | Detach, release the Pod IP, delete the record — each step idempotent. |

Design decisions:

- **Pod IP is keyed by Pod identity, not sandbox ID.** Keying IPAM and the Router
  by `namespace/name` (matching the provider's `podKey`) keeps a Pod's address
  stable when its sandbox is recreated. CRI-P5 keeps the one-sandbox-per-Pod,
  one-container-per-sandbox restriction and rejects a second live sandbox for the
  same Pod key; multi-container shared networking is still out of scope.
- **Nothing is faked.** `PodSandboxStatus.Network` stays nil until the binat rule
  is live, and `NetworkReady` is false (with an explicit reason) whenever the
  dependency is missing or half-configured. A missing VM address (DHCP not yet
  acquired) surfaces as `Unavailable` so the caller retries.
- **Failed attach unwinds the start.** If the VM address never appears or the
  Router rejects the rule, `StartContainer` stops the workload and marks the
  container `Exited` with reason `NetworkSetupFailed`, rather than leaving it
  `Running` behind an unreachable Pod IP.
- **Restart is leak-free.** `Server.RecoverNetwork` (called once at adapter
  startup, after the Router is `Start`ed) re-reserves each persisted sandbox's Pod
  IP and re-attaches every sandbox that was attached before the restart. The
  Router rebuilds its anchor wholesale on every change, so without re-attaching
  the surviving endpoints the next `Attach`/`Detach` would drop other Pods' rules.
- **No manual repair on the normal path.** The Router's existing cold-start guards
  (stripping apple/container's scoped vmnet default route, tolerating an absent
  pf anchor) are inherited unchanged, so the documented test path needs no manual
  `route`/`pfctl` steps.

Wiring: `cmd/macvz-cri` gains `--pod-cidr` and `--pod-network-interface` (plus
optional `--pod-network-mesh-interface`, `--pod-network-helper-socket`, and
`--pod-network-enable-forwarding`). Pod networking is **off unless both
`--pod-cidr` and `--pod-network-interface` are set**; until then sandboxes run
without a Pod IP and `Status` reports `NetworkReady=false`, exactly as before
CRI-P5. The privileged pf/route operations route through `macvz-netd` when a
helper socket is given, mirroring `macvz-kubelet`.

Testing: hermetic tests (`pkg/criserver/network_test.go`) cover the sandbox
network lifecycle success, the withheld-then-reported Pod IP, idempotent
stop/remove, direct `StopContainer`/`RemoveContainer` detach, self-exit detach
during status reconciliation, failed-attach cleanup, missing-VM-IP unwind,
restart recovery (reserve + re-attach from disk-persisted state), `NetworkReady`
reporting (including the half-configured case), duplicate Pod-key rejection, and
the networking-off path. A gated live smoke test (`MACVZ_INTEGRATION=1`,
`pkg/criserver/network_integration_test.go`) runs a real apple/container micro-VM
behind a real `podnet.Router` and asserts the sandbox reports its real Pod IP
through `PodSandboxStatus`, then that removal releases it:

```sh
MACVZ_INTEGRATION=1 go test ./pkg/criserver -run 'Test.*Network|Test.*Sandbox' -count=1
# requires root (pf/route) or MACVZ_CRI_POD_HELPER_SOCKET=<macvz-netd socket>
# tunables: MACVZ_CRI_POD_CIDR (default 10.244.0.0/24), MACVZ_CRI_POD_IFACE (default bridge100)
```

### CRI-P5 Decision

Pod networking is **proven and honest** over the CRI path: a sandbox/container
receives a real Pod IP from the same IPAM the shipped provider uses, the host
binat path is programmed through the same Router, and the Pod IP is reported only
once it is actually reachable. Restart recovery, idempotent teardown, and
failed-attach cleanup all hold without manual repair.

Carry into **CRI-P6**: with an honest Pod IP and `PodSandboxStatus.Network`, a
real kubelet/k3s node join is now worthwhile. The next blockers are the streaming
surfaces — logs, exec, attach, port-forward — and container stats, which kubelet
exercises immediately after a Pod goes Ready. The CRI route remains **not**
stopped.

## CRI-P6: Logs, Exec, Attach, Port-Forward, Stats

CRI-P6 gives the experimental adapter the kubelet-facing operational surfaces a
node needs once a Pod is Ready (#78): `kubectl logs`, `kubectl exec`,
`kubectl port-forward`, and container/Pod stats. All `apple/container`
assumptions stay inside `pkg/runtime`; the adapter reaches them through the
narrow `ContainerRuntime`/`statsRuntime` interfaces it already owns.

### What is honest

- **Logs** (`pkg/criserver/logs.go`). CRI logging is file-based, not an RPC: the
  runtime must write each container's output to the CRI log file, and kubelet
  reads that file directly. On `StartContainer` the adapter opens a follow stream
  over the workload (`container logs --follow`) and pumps every line into
  `<LogDirectory>/<LogPath>` in the kubelet format
  `<RFC3339Nano> stdout F <message>`. `ReopenContainerLog` swaps the destination
  file for kubelet's log rotation. The pump runs on a background context, is
  reaped on stop/remove, and never fails or blocks the container start.
- **Exec / ExecSync** (`pkg/criserver/streaming.go`). The `Exec` RPC validates the
  container is Running and hands kubelet a streaming URL minted by the
  `k8s.io/kubelet/pkg/cri/streaming` server, whose backend runs `container exec`
  with the client's stdin/stdout/stderr and TTY. `ExecSync` runs a command to
  completion and returns captured stdout/stderr and the exit code — the path
  kubelet uses for exec liveness/readiness probes. A clean non-zero exit is
  reported as an exit code, not an RPC error.
- **Port-forward** (`pkg/criserver/streaming.go`). The `PortForward` RPC hands
  kubelet a streaming URL; the backend dials the Pod micro-VM's address directly
  (the kubelet shares the host with the guest, so the host-only address is always
  reachable) and proxies bytes both ways until either side closes. Both copy
  goroutines and the connection are always reaped.
- **Stats** (`pkg/criserver/stats.go`). `ContainerStats`, `ListContainerStats`,
  `PodSandboxStats`, and `ListPodSandboxStats` sample the runtime's optional
  `Stater` capability (`container stats`). CPU (cumulative core-nanoseconds) and
  memory (working set, usage, and available against a known limit) are mapped to
  the CRI shapes. A container that is not running or whose sample is unavailable
  is reported with only its attributes — never faked zeros — so a consumer cannot
  mistake "unobservable" for "idle". With one container per Pod (CRI-P3), Pod
  stats lift the single container's sample to the Pod level.

### Documented limitations

- **Attach is unsupported.** `apple/container` exposes no honest way to reattach
  to a started container's primary process streams, so the `Attach` RPC returns
  `codes.Unimplemented` with that reason rather than minting a URL that would
  carry nothing. `kubectl exec` is the supported alternative.
- **Logs merge stdout and stderr.** `container logs` returns one combined stream,
  so every CRI log line is tagged `stdout`. The `F` (full) tag is always used
  because the pump reassembles complete lines before writing.
- **No writable-layer / swap / PSI stats.** The micro-VM runtime exposes no
  honest source for those fields in this phase, so they are left unset.
- **Streaming requires a configured server.** Started without `--streaming-addr`,
  `Exec`/`PortForward` return `FailedPrecondition` rather than a dead URL. The
  default binds `127.0.0.1:0` (kubelet runs on the same Mac).

### CRI-P6 Decision

The operational surfaces are **proven and honest** over the CRI path: logs, exec,
exec-sync, port-forward, and stats all work through kubelet-compatible endpoints,
and the one surface the runtime cannot back — attach — fails loudly instead of
faking success. The CRI route remains **not** stopped. The remaining gap before a
default-capable node is multi-container Pod support, which stays explicitly out of
scope. A live kubelet/k3s smoke (`kubectl logs`/`exec`/`port-forward` against a
single-container Pod on the experimental CRI node) is the natural next validation.

## Reproducible Probe

Run:

```sh
make cri-feasibility
```

This performs a non-invasive CLI surface probe. It does not create, start, or
delete workloads unless explicitly requested:

```sh
MACVZ_CRI_LIVE=1 make cri-feasibility
```

The live mode is intentionally gated because it may pull images and boot a
micro-VM.
