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
