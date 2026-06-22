# CRI-I5-2 Upstream apple/containerization Primitive Proposal (#122)

Date: 2026-06-22

Outcome: `upstreamPrimitiveProposalDrafted`

Status: **draft for discussion — not submitted upstream.** Per #122 non-goals,
this does not open an upstream PR and does not gate continued local experimental
work. It is the translation of MacVz's local R8–R16 evidence into a concise
proposal for `apple/containerization`.

## 1. Summary

MacVz runs Kubernetes Pods as per-Pod Linux micro-VMs on Apple Silicon via
`apple/container` + `vminitd`. The Kubernetes/CRI ordering requires a container's
rootfs to become available **after** the Pod VM is already running:

```text
RunPodSandbox    -> VM + vminitd already running
CreateContainer  -> rootfs becomes available inside the running VM
StartContainer   -> container init starts from that rootfs
```

`vminitd` today has no first-class way to expose a container rootfs into an
already-running Pod VM and create/start an init process from it. MacVz proved the
state machine works with a small set of local `vminitd/vmexec` patches plus a
host-managed evidence channel, but that proof currently depends on harness-only
mechanics and MacVz-private workarounds.

This proposal asks for **two upstream changes**:

1. A **late-rootfs container primitive** (`PrepareContainerRootfs` +
   late-binding `CreateContainer`) so a rootfs can be staged into a running Pod
   VM and used as OCI `root.path` without harness tricks.
2. Two small **`vmexec` rootfs-setup robustness fixes** (R11, R12) so a minimally
   prepared rootfs boots.

It also asks for one **optional** change — a documented vminitd-visible
read-back guarantee for a host-prepared path — that would let MacVz drop its
bind-mounted evidence-handoff workaround. Everything else (identity format,
permission policy, path layout, CRI mapping) is **MacVz policy and stays local**.

## 2. Local evidence (R9–R16)

All probes ran on the same environment unless noted:

- Host: `Darwin chimeras-Mac-mini-2.local 25.5.0` (xnu-12377.121.6, `arm64`).
- Swift: `Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)`.
- `apple/containerization` checkout: `/tmp/apple-containerization`, base commit
  **`6b7b42c`**, with MacVz local patches R10/R11/R12 applied.
- Initfs reference: `vminit:macvz-r12`; kernel `bin/vmlinux-arm64`.
- Image: `docker.io/library/busybox:1.36.1`.
- Probe: `MACVZ_LINUXPOD_POC=1 make cri-linuxpod-r9`.

| Probe | Question | Outcome | Finding |
| --- | --- | --- | --- |
| R6/R7 | Does vminitd already create a new container from an OCI spec? | — | Yes: the `id == containerID` new-ID branch creates a `ManagedContainer` from the full spec, but it cannot address a *late, host-staged* rootfs; `/run/container/utility/rootfs/...` reached `createProcess` yet `vmexec run` failed `No such file or directory`. |
| R8 | What primitive is missing? | design | A rootfs/container lifecycle primitive owned by vminitd's own state model (see §3). |
| R9 | Can a late prepared rootfs launch at all? | `vminitdRootfsPrimitiveLaunchSucceeded` | Process create+start+exit 0; rootfs at `/run/container/r9-late-alpha/rootfs`. `namespaceVerified=false`: `proc_root` did **not** expose the host-visible prepared-rootfs path. |
| R10 | Why did early launches fail? | diagnostics | Added a `vmexec` failure-diagnostics patch dumping spec root/process/mounts/namespaces on `execInNamespace` failure. **Debug aid, not a required upstream change.** |
| R11 | First boot blocker | fix | `vmexec` `configureConsole()` failed on `remove(/dev/ptmx)` when the prepared rootfs has no `ptmx`. Patch tolerates `ENOENT`. |
| R12 | Second boot blocker | fix | Minimal prepared rootfs has no `/dev/null`; `vmexec` now `ensureDevNull()` (mknod char 1:3) before `setDevSymlinks`. |
| R13 | Does userspace run? | `lateRootfsUserspaceAdvanced` | With busybox applet symlinks the identity script runs (harness-only rootfs content). |
| R14 | Can vminitd see the result? | `lateRootfsResultVisibilityExplained` | Process exits 0 after writing `/macvz-r9-result` **inside its mount namespace**, but vminitd **cannot** `stat` `/run/container/<id>/rootfs/macvz-r9-result`. The process-local rootfs path is not a reliable post-exit read-back channel. |
| R15 | Reliable read-back? | `vminitdRootfsPrimitiveLaunchSucceeded` | A host-prepared, vminitd-visible path (`/run/macvz-r9-evidence/<id>`) bind-mounted into the late rootfs, written by the process, and read back by the host via Copy(COPY_OUT) **does** verify identity reliably. |
| R16 | Production shape | `runtimeHandoffDesignAccepted` | Model the read-back as a runtime-private per-container handoff directory + OCI bind mount; identity is a start invariant. |

Exact local patches referenced by this proposal:

- `docs/CRI_RUNTIME_R10_APPLE_CONTAINERIZATION_DEBUG.patch` (diagnostics).
- `docs/CRI_RUNTIME_R11_APPLE_CONTAINERIZATION_PTMX.patch` (ptmx ENOENT).
- `docs/CRI_RUNTIME_R12_APPLE_CONTAINERIZATION_DEVNULL.patch` (ensure /dev/null).

Full reports: `docs/CRI_RUNTIME_R8_ROOTFS_CONTAINER_PRIMITIVE.md`,
`R9_…`, `R13_…`, `R14_…`, `R15_EVIDENCE_CHANNEL_REPORT.md`,
`R16_HANDOFF_DESIGN.md`.

## 3. Required upstream primitive: late-rootfs container creation

This is the one change that removes MacVz's harness assumptions. Today MacVz must
stage rootfs bytes through the existing `Copy(COPY_IN archive)` transport and
fight to make `vmexec run` resolve `root.path` — R7 showed the obvious
utility-container path does not resolve, and R14 showed the namespace boundary is
real, not a path-rewrite bug.

The proposal keeps the API small enough to live inside `LinuxPod`/`vminitd`
rather than becoming a MacVz-only runtime. It is a two-stage, idempotent,
host→vminitd API. (Field tables reproduced from the R8 design for a single
upstream-facing reference.)

### 3.1 `PrepareContainerRootfs` (host → vminitd)

Reserve a container ID and make a rootfs visible at a vminitd-resolvable path
inside an already-running Pod VM, before any process is created.

Request: `podID`, `containerID`, `rootfsSource` (attached mount / NBD endpoint /
virtiofs tag / upload+unpack token), optional `targetPath` (default
`/run/container/<containerID>/rootfs`), `readonly`, `requestID` (idempotency),
optional `expectedIdentity`.

Success: `rootfsHandle` (opaque), `rootfsPath` (the path to use as OCI
`root.path`), observed `identity`, `cleanupToken`.

Failure boundaries: duplicate container ID; source unavailable; attach/mount
failed; identity mismatch; unsupported source type; cleanup-after-partial-mount
failed.

### 3.2 late-binding `CreateContainer` (host → vminitd)

Register/create container state from a prepared rootfs handle.

Request: `containerID` (matches a prepared target), `rootfsHandle`, full
`ociSpec` (root path omitted or must equal the handle path), `stdio`, optional
`ociRuntimePath`, optional `start` (false = CRI `CreateContainer`; true = one-shot
PoC convenience), `rollbackOnStartFailure`.

Success: `containerID`, `pid` (only if `start=true`), `state` (`created` |
`started`).

Failure boundaries: missing rootfs handle; invalid OCI spec; root-path mismatch;
bundle-creation failed; cgroup-creation failed; start failed; rollback failed.

### 3.3 Required state transition (what vminitd must own)

1. **Allocate rootfs slot** — reserve container ID + rootfs handle; reject
   duplicate live/prepared IDs.
2. **Expose rootfs** — attach/transfer/unpack so the target path is resolvable in
   the namespace where `vmexec run` reads `root.path`. **This is the gap R7/R14
   exposed**: the rootfs must be visible to the create/start path, not only inside
   a foreign mount namespace.
3. **Create container state** — generate/accept the OCI spec with
   `root.path == targetPath`, build the OCI bundle + cgroup, insert into vminitd
   state only after the rootfs is coherent.
4. **Start** — `vmexec run --bundle-path`; record pid.
5. **Cleanup** — stop, delete bundle, remove cgroup, unmount/detach rootfs,
   release LinuxPod/host attachment.

A clean upstream implementation makes step 2 a first-class operation. With it,
MacVz drops: the Copy(COPY_IN) rootfs-staging hack, the utility-container path
attempts (R7), and the assumption that a process-local path doubles as a host
read-back channel (R14).

## 4. Required upstream fixes: `vmexec` rootfs setup

These are tiny and self-contained; they make `childRootSetup` tolerate a
*minimal* prepared rootfs (the realistic input for a late-rootfs primitive). Both
are already expressed as local patches against base `6b7b42c`.

### 4.1 ptmx tolerance (R11)

`vminitd/Sources/vmexec/Mount.swift`, `configureConsole()`:

```swift
let ptmx = rootfs + "/dev/ptmx"
- guard remove(ptmx) == 0 else {
+ guard remove(ptmx) == 0 || errno == ENOENT else {
      throw App.Errno(stage: "remove(ptmx)")
  }
```

Rationale: a freshly staged rootfs may have no `/dev/ptmx`; removing a
non-existent path should not be fatal before re-symlinking `pts/ptmx`.

### 4.2 ensure `/dev/null` (R12)

`vminitd/Sources/vmexec/RunCommand.swift`, `childRootSetup()` adds
`ensureDevNull(rootfs:)` before `setDevSymlinks`: if `/dev/null` is absent or not
a char device, (re)create it via `mknod(S_IFCHR | 0666, dev 1:3)` and `chmod`.

Rationale: minimal images frequently omit `/dev/null`; multiple setup steps and
the workload itself assume it exists.

### 4.3 diagnostics (R10) — optional

The R10 patch dumps the failing spec (root path/readonly, process args/cwd,
namespaces, mounts) when `execInNamespace` throws. It is a debugging convenience
MacVz found essential while bringing the path up; upstream may prefer its own
logging. **Not required for correctness.**

## 5. Optional upstream change: vminitd-visible read-back

R14 is the sharp finding: a late process can write a file and exit 0, yet vminitd
cannot read it back from the prepared rootfs path because that path lives in the
container's private mount namespace. R15 worked around this with a host-prepared,
bind-mounted evidence directory.

If `apple/containerization` documented (or provided) a **host-prepared path that
is guaranteed readable by the host/vminitd after process start and exit** — e.g.
a blessed per-container scratch mount whose host side the agent can `Copy(COPY_OUT)`
— MacVz could drop the bind-mount injection entirely and read identity/evidence
through that guaranteed channel.

This is **optional**: MacVz's handoff bind mount (§6) already achieves it. We
raise it only because it is a primitive others building late-rootfs flows will
also need, and a documented guarantee is cleaner than every runtime inventing a
bind mount.

## 6. Out of scope for upstream — MacVz policy (stays local)

These are deliberately **not** asked of upstream; they are MacVz's runtime
contract and belong in `pkg/runtime` / `pkg/criserver`:

- **Handoff path layout** — `/run/macvz/containers/<id>/{rootfs,handoff}` and the
  in-guest mount point `/run/macvz/handoff` (R16). MacVz-owned namespace.
- **Evidence/identity format** — line-oriented `key=value`, `identity == expected`
  exact match (not substring), `proc_root` as debug-only. MacVz semantics.
- **Permission policy** — handoff dir `0777` for the first cut because it is
  private to one container and deleted with it; future narrowing to
  `runAsUser/runAsGroup` (tracked locally as #121). Not an upstream concern.
- **Bind-mount injection + kubelet-mount collision guard** — MacVz injects the
  writable handoff bind mount and rejects any CRI/kubelet mount targeting the
  reserved `/run/macvz` namespace.
- **CRI lifecycle mapping** — Create→prepare, Start→identity-gated Running,
  Stop→preserve evidence, Remove→idempotent cleanup, Status→verbose-only
  diagnostics. This is the K8s/CRI provider boundary, not a vminitd API.
- **Harness rootfs content** — busybox applet symlinks (R13) are test scaffolding.

This separation preserves the architecture boundary R16 insisted on: Kubernetes
is the control plane, MacVz is a node/runtime provider, and apple/container
assumptions stay inside MacVz's runtime integration layer.

## 7. Failure modes

| Failure | Where | Expected behavior |
| --- | --- | --- |
| Duplicate container ID | `PrepareContainerRootfs` | Reject; no slot reserved. |
| Rootfs source unavailable / unsupported | `PrepareContainerRootfs` | Reject with source-typed error; no partial mount left. |
| Attach/mount failed | `PrepareContainerRootfs` | Cleanup partial mount; return error; `cleanupToken` still valid. |
| `expectedIdentity` mismatch | `PrepareContainerRootfs` | Reject before any process; identity is a precondition. |
| Missing rootfs handle / root-path mismatch | `CreateContainer` | Reject; no bundle/cgroup created. |
| Bundle or cgroup creation failed | `CreateContainer` | Roll back per `rollbackOnStartFailure`; leave recoverable or clean state, never half-registered. |
| `/dev/ptmx` or `/dev/null` missing | `vmexec` setup | Tolerated by R11/R12; no longer fatal. |
| Process exits before writing evidence | start/read-back | Runtime captures stderr, marks Exited; MacVz maps to StartContainer failure (local). |
| Evidence path not host-visible | read-back | The §5 gap; today handled by MacVz bind-mount handoff. |
| Cleanup path missing | `Destroy` | Idempotent; tolerate + log at debug. |

## 8. Security considerations

- **No host filesystem escape.** The prepared rootfs and any scratch/handoff path
  must be per-container, inside the Pod VM's runtime namespace, and removed on
  cleanup. They are not shared Pod volumes and not host paths.
- **Identity is a precondition, not a capability.** `expectedIdentity` /
  identity verification proves *which* rootfs booted; it must be exact-match and
  must not be satisfiable by a substring or a process-controlled path value
  (`proc_root` is explicitly debug-only — R15/R16).
- **Permission surface.** The `0777` handoff directory is MacVz policy and is
  bounded: one container, private, deleted with the container. Upstream is not
  asked to adopt it. If upstream provides the §5 read-back channel, it should be
  per-container and host-owned, writable by the container user without widening
  host access.
- **Reserved namespace must be unspoofable.** A late-rootfs/evidence mount point
  must not be targetable by image content or by externally supplied mounts; MacVz
  enforces this for `/run/macvz` locally and any upstream blessed path should be
  similarly reserved.
- **Idempotency/retry safety.** `requestID`/`cleanupToken` must make
  prepare/cleanup retry-safe so a crashed host cannot leak rootfs attachments or
  half-registered containers.

## 9. What upstream acceptance would remove locally

| Local assumption / workaround | Removed by |
| --- | --- |
| `Copy(COPY_IN archive)` rootfs staging + path-resolution fighting (R7) | §3 late-rootfs primitive |
| R11 ptmx / R12 dev-null local `vmexec` patches carried against `6b7b42c` | §4 upstreamed fixes |
| R15 bind-mounted evidence directory as the only reliable read-back (R14) | §5 documented vminitd-visible read-back (optional) |
| MacVz-private kernel/initfs build pinned at `vminit:macvz-r12` | §3+§4 landing in a released `apple/containerization` |

The handoff *policy* (path layout, identity format, CRI mapping, permissions)
stays local regardless — it is MacVz's runtime contract, not an upstream API.

## 10. Non-goals (per #122)

- No upstream PR is opened by this draft.
- Local experimental work does not depend on upstream acceptance.
- This proposal does not claim multi-container Pod support (separately blocked,
  #82) or change MacVz's shipped Virtual Kubelet path.
