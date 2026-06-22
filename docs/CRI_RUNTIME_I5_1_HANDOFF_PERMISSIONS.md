# CRI-I5-1 Handoff Permission Hardening (#121)

Date: 2026-06-22
Milestone: CRI-I5 — Hardening & Upstream Alignment
Outcome: `handoffPermissionsHardened`

## Purpose

R15 required a writable handoff directory; R16 accepted `0777` initially but
called out future hardening around `runAsUser`/`runAsGroup`
(`docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md`). This task narrows the runtime-private
handoff directory's permissions to the container's configured user when that is
known and reliable, while preserving the safe fallback that lets a non-root
container still write its identity evidence.

## Permission policy

The handoff directory (`<root>/<id>/handoff`, host source of the writable bind
mount at `/run/macvz/handoff`) is the only handoff path a container process
writes — it carries the observed identity evidence. The container/rootfs dirs are
runtime-owned (`0755`) and never chowned.

`runtime.HandoffManager.CreateForUser(id, owner)` resolves the directory mode
from the resolved owner (`runtime.HandoffOwner`, derived from the Pod
securityContext `runAsUser`/`runAsGroup`):

| Owner | Action | Mode |
|-------|--------|------|
| Known uid + gid, chown succeeds | chown to uid:gid | `0770` (owner+group) |
| Known uid only, chown succeeds | chown to uid | `0700` (owner-only) |
| Known uid, chown **not permitted** (EPERM) | none | `0777` (fallback) |
| Unknown (no `runAsUser`; image's user) | none | `0777` (fallback) |

Key properties:

- **Non-root containers can always write.** Narrowing happens *only after a
  successful chown to the container's uid*, so the owning process always retains
  write access. When the adapter cannot chown — the common macOS case, where
  apple/container refuses to run as root so the adapter is unprivileged — the
  directory keeps the world-writable fallback rather than locking the container
  out. Correctness wins over hardening.
- **Unrelated host-local processes are excluded when hardened.** A narrowed
  `0700`/`0770` directory owned by the container's uid removes the world-write
  bit, so a process that is not that user (or in that group) can no longer write
  the evidence channel. Cross-*container* writes were already impossible: each
  container only has its own handoff directory bind-mounted into its VM.
- **`runAsUsername` is ignored.** Resolving a name to a uid needs the image's
  `/etc/passwd` and is not reliable at prepare time, so a name-only security
  context degrades to the safe fallback rather than guessing.
- **No host paths are exposed to workloads, and no user-namespace model is
  introduced** (non-goals honored). The handoff directory is runtime-private and
  removed with the container.

### Read-only root filesystem

`readOnlyRootFilesystem` does not block evidence: the handoff bind mount is a
separate, independently writable mount (`runtime.HandoffMount` sets
`ReadOnly=false`), so a read-only-rootfs container still writes its identity into
`/run/macvz/handoff`. Verified by `TestCreateContainerHandoffReadOnlyRootFSStillWritable`.

## Wiring

- `pkg/runtime/handoff.go`: `HandoffOwner` type; `CreateForUser` applies the
  policy via `applyHandoffOwnership` (best-effort chown, never fatal). `Create`
  is retained as `CreateForUser(id, HandoffOwner{})` for callers with no security
  context.
- `pkg/criserver/container_handoff.go`: `handoffOwnerFromConfig` parses
  `runAsUser`/`runAsGroup` from the CRI container security context;
  `prepareHandoff` threads the owner to `CreateForUser`. `CreateContainer` passes
  `handoffOwnerFromConfig(cfg)`.
- `pkg/criserver/handoff_lifecycle.go`: verbose `ContainerStatus` surfaces
  `handoffDirMode` (octal) and `handoffWritePolicy`
  (`owner-only`/`owner-group`/`world-writable-fallback`) so an operator can
  confirm the posture per container.

## Observability

`crictl inspect` (verbose `ContainerStatus`) reports, for a handoff-prepared
container:

```
handoffDirMode=0770
handoffWritePolicy=owner-group
```

(or `0777` / `world-writable-fallback` when the owner is unknown or the chown was
not permitted).

## Tests

- `pkg/runtime/handoff_test.go`:
  `TestHandoffCreateForUserNarrowsToOwner` (uid+gid → `0770`, uid only → `0700`,
  chowned to the owner, still writable), `TestHandoffCreateForUserFallsBackWhenChownNotPermitted`
  (foreign uid 0 as non-root → EPERM → `0777`, ownership unchanged, writable;
  skipped as root), `TestHandoffCreateUnknownOwnerIsWorldWritable`. A unix-only
  `statFileOwner` test helper asserts the chowned owner.
- `pkg/criserver/container_handoff_test.go`:
  `TestHandoffOwnerFromConfig` (uid+gid, uid only, nil context,
  username/group-only/invalid-id ignored),
  `TestCreateContainerNarrowsHandoffToRunAsUser` (end-to-end narrowing to the
  test uid/gid + verbose-status policy), `TestCreateContainerHandoffReadOnlyRootFSStillWritable`.
- `cmd/macvz-cri/preflight.go`: `checkHandoff` documents the policy in the
  operator preflight report.

`go build ./...`, `go vet ./...`, and
`go test ./pkg/runtime/... ./pkg/criserver/... ./cmd/macvz-cri/...` are green. The
narrowing and fallback paths are exercised on any machine: chowning to the test's
own uid always succeeds (narrow), and a non-root process chowning to uid 0 always
fails (fallback).

## Non-goals (honored)

- No full Linux user-namespace / id-mapping model.
- No host path exposed to workloads (the handoff directory is runtime-private).
- The `0777` fallback is retained where chown is unavailable; this is a deliberate
  correctness-over-hardening choice on the unprivileged macOS adapter, not an
  oversight.
