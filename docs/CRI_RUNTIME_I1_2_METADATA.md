# CRI-I1-2 Runtime Metadata for Rootfs and Handoff State (#110)

Date: 2026-06-22

Outcome: `runtimeHandoffMetadataDefined`

## Purpose

Define the minimal runtime-private metadata a late-rootfs container needs so the
runtime can recover or clean it up after a restart, per the CRI-R16 handoff
design (`docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md`).

This is the metadata *shape*, not the wiring. It does not change the CRI
container store, is never exposed as a Kubernetes API surface, and is not a
kubelet-visible volume.

## Where it lives

`pkg/runtime/handoffmeta.go`, alongside the path lifecycle helper
(`handoff.go`, #109) it depends on. `handoffmeta.go` is the single source of
truth for the reserved path layout constants; `handoff.go` consumes them.

Identity is a runtime *start-time invariant*, not user-facing CRI state, so it
belongs with the runtime driver, not the persisted CRI record. The CRI store is
intentionally unchanged.

## Schema

`HandoffMeta` records:

| Field | Type | Recovery class | Meaning |
| --- | --- | --- | --- |
| `ContainerID` | string | reconstructable | Sanitized runtime container/workload ID. |
| `RootfsPath` | string | reconstructable | Prepared (late) rootfs guest path (`HandoffLayout.RootfsDir`). |
| `HandoffPath` | string | reconstructable | Handoff evidence dir, bind-mount source (`HandoffLayout.HandoffDir`). |
| `IdentityFile` | string | reconstructable | Host path of the required evidence file (`HandoffLayout.IdentityFile`). |
| `ExpectedIdentity` | string | recoverable from spec | Identity the launched process must report. |
| `ObservedIdentity` | string | best-effort | Identity the process actually reported. |
| `Status` | `IdentityStatus` | best-effort | `Pending` / `Verified` / `Mismatch` / `Missing`. |
| `VerifiedAt` | time | best-effort | When verification last ran. |
| `Cleanup` | `CleanupState` | best-effort | `Active` / `Stopped` / `Removed`. |

`IdentityStatus` and `CleanupState` are string enums whose zero value is the
empty string; `EffectiveStatus()` maps it to `Pending` and `EffectiveCleanup()`
to `Active`.

Verification (`Verify(observed, now)`) uses **exact** equality, not substring
matching (CRI-R16): empty observed → `Missing`, unequal → `Mismatch`, exact
match → `Verified`. A container must not be reported Running unless `Verified()`.

## Restart / recovery behavior (explicit)

- **Reconstructable from the container ID alone** (deterministic, never needs to
  be persisted): `ContainerID`, `RootfsPath`, `HandoffPath`, `IdentityFile`. A
  restarted runtime recomputes them with `HandoffPaths(id)` /
  `HandoffManager.Layout(id)`, exactly as `store.DeriveWorkloadID` recomputes the
  workload name. `HandoffPaths` and `Layout` are tested to agree.

- **Recoverable from the container spec** (not from the runtime alone):
  `ExpectedIdentity`. It comes from the prepared rootfs the container was created
  with, so the runtime re-derives it when it reloads that spec.

- **Best-effort, may be lost on restart**: `ObservedIdentity`, `Status`,
  `VerifiedAt`, `Cleanup`. These capture the *result* of a past verification.
  They are deliberately not required to be durable: identity is a start invariant,
  not an ongoing property. A container that was already `Verified` and is still
  running does not re-read evidence; a restarted runtime that cannot confirm a
  prior verification treats `EffectiveStatus()` as `Pending` (the safe default)
  rather than trusting an unrecoverable result. Whether these fields are persisted
  at all is a driver decision deferred to the CRI wiring tasks (CRI-I3). JSON tags
  exist so a driver that chooses to persist gets a stable on-disk shape.

This means orphan recovery is **not** implemented here (a non-goal): the type
only makes the recoverable/best-effort split explicit so a later recovery pass
knows what it can trust.

## Tests

`pkg/runtime/handoffmeta_test.go`:

- `HandoffPaths` agrees with `HandoffManager.Layout`.
- `NewHandoffMeta` derives paths from the layout and defaults to
  `Pending`/`Active`/zero `VerifiedAt`.
- `Verify` exact-match table: match / mismatch / missing / substring-not-enough.
- `EffectiveStatus` and `EffectiveCleanup` zero-value mapping.
- Reconstruct-from-ID: deterministic fields rebuild identically while a
  best-effort result is dropped back to `Pending`.
- JSON round-trip stability.

`go test ./pkg/runtime/...`, `go vet ./...`, and full `go build ./...` /
`go test ./...` are green.

## Non-goals (honored)

- No full orphan recovery.
- No CRI container store change.
- No Kubernetes API surface for the metadata.
