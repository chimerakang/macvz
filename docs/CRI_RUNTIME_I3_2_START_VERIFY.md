# CRI-I3-2 StartContainer Identity Verification Before Running (#116)

Date: 2026-06-22

Outcome: `startContainerIdentityGated`

## Purpose

Gate a late-rootfs container's transition to Running on handoff identity
verification, per CRI-R16 (`docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md`):
StartContainer marks the container Running only after the launched process has
reported the expected rootfs identity back through the runtime-private handoff
evidence file. It builds on the create-time preparation (#115) and the identity
contract (#113).

Implemented in `pkg/criserver/container_handoff_start.go` and a bounded-wait
helper in `pkg/runtime/identity.go`.

## Flow

`StartContainer` ordering, with the new gate inserted after the workload starts
and before Running is persisted:

1. Start the runtime workload (unchanged).
2. **Verify handoff identity** (`s.verifyHandoffIdentity`): for a handoff-prepared
   container only, derive the layout from the workload ID, read the expected
   identity staged into the prepared rootfs (`runtime.ReadStagedIdentity`, the
   same source #117's verbose status reads), then bounded-wait on the evidence
   file (`runtime.WaitForHandoffIdentity`) until the observed identity is verified
   or the timeout expires.
3. Mark Running + persist (unchanged), now reached only after verification.
4. Network attach + log pump (unchanged ordering; they remain after Running).

The gate is inert when the handoff path is disabled (`s.handoff == nil`) or the
container prepared no handoff (`!c.HandoffPrepared`), so the default
apple/container path is unchanged.

## Bounded wait — `runtime.WaitForHandoffIdentity`

Polls the evidence file every interval until verified, terminal failure, or
ctx expiry:

- Missing/empty evidence is retried (the process may not have written it yet).
- A mismatch or malformed file is terminal — waiting cannot turn a wrong identity
  into the right one.
- ctx expiry returns an error wrapping `ErrEvidenceMissing` that names the
  expected identity and the ctx cause, so a timeout reads as "evidence never
  arrived", not a generic deadline.

The CRI server bounds it with `handoffVerifyTimeout` (default 30s,
`handoffVerifyInterval` default 250ms; tests shorten both).

## Failure behavior — never left Running

On any verification failure the gate unwinds the just-started workload via
`unwindContainerStartReason` (generalized from the network-attach unwind): the
workload is **stopped** and the record is marked Exited with reason
`IdentityVerificationFailed` and a message that includes the underlying error and
any best-effort `stderr` capture (`runtime.ReadStderrDiagnostics`). StartContainer
returns `FailedPrecondition`. The container is never persisted Running after a
missing/mismatched/late identity.

Persisting the Running state is also hardened: if the Put fails after a verified
start, the workload is stopped (reason `PersistFailed`) rather than leaked behind
a Created record the kubelet cannot see, and `Internal` is returned.

## Expected identity source

The expected identity is whatever `prepareHandoff` (#115) staged into the prepared
rootfs at create time — `handoffExpectedIdentity(workloadID)` =
`macvz-rootfs-id=<workloadID>`, deterministic and fully recoverable from the
record, so no extra CRI state is persisted. StartContainer and verbose
ContainerStatus (#117) both read it via `runtime.ReadStagedIdentity`, so they
never disagree.

## Tests

`pkg/criserver/container_handoff_start_test.go`:

- verified → Running; mismatch → Exited + `IdentityVerificationFailed`, workload
  stopped; timeout (no evidence) → bounded failure, Exited, workload stopped;
  runtime Start error → never verified, stays Created, not stopped; persist
  failure after verified start → Internal, workload stopped, not Running;
  no-handoff path → starts normally with no evidence.

`pkg/runtime/identity_test.go`: `WaitForHandoffIdentity` late arrival, timeout
(maps to `ErrEvidenceMissing`/`IdentityMissing`), mismatch-is-terminal, nil
metadata error, and already-canceled context returns promptly.

`go test ./pkg/criserver/...`, `go test ./pkg/runtime/...`, `go vet ./...`, and
full `go build ./...` / `go test ./...` are green.

## Non-goals (honored)

- No app readiness — the evidence is a launch-of-process signal; Kubernetes
  readiness probes remain separate.
- No status semantics beyond launch identity verification.
