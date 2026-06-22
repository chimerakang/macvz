# CRI-I4-3 Handoff Restart & Cleanup Validation (#120)

Date: 2026-06-22

Outcome: `handoffRestartCleanupValidated`

## Purpose

The handoff directory is runtime-private per-container state under the handoff
root (`/run/macvz/containers/<workloadID>/{rootfs,handoff}`, per
[CRI_RUNTIME_R16_HANDOFF_DESIGN.md](CRI_RUNTIME_R16_HANDOFF_DESIGN.md)). It must
not leak across adapter/runtime restarts or failed starts. CRI-I4-3 validates the
restart-recovery and cleanup behavior of that state and closes the one real gap
the earlier handoff tasks left open: reclaiming subtrees that no container record
claims.

This is a validation task. It adds the orphan-reclamation primitive recovery was
missing, then proves the full restart/cleanup contract with hermetic tests and one
gated live scenario.

## What recovery already guaranteed (and what was missing)

Handoff paths derive purely from the workload ID, which itself derives
deterministically from the container ID (`store.DeriveWorkloadID`). So a restarted
adapter already recomputes the subtree for every container it still knows about
(`container_handoff.go`) and never loses track of live state. `HandoffPrepared` on
the persisted record is the only handoff bit that survives a restart; the paths are
recomputed, nothing host-specific is stored.

The gap: a subtree with **no** surviving record — left by a crash between staging a
subtree and persisting its record, or by a death mid-`RemoveContainer` after the
record was deleted but before/while `cleanupHandoff` ran — was never reclaimed. It
would leak runtime-private rootfs/handoff state across restarts indefinitely.

## Change

- `runtime.HandoffManager.ListContainerIDs()` (`pkg/runtime/handoff.go`) enumerates
  the well-formed per-container subtree names under the handoff root. A missing
  root yields an empty list (a node that never prepared a handoff has no orphans);
  stray files and ill-formed names are skipped so nothing masquerades as an orphan
  workload.
- `Server.sweepOrphanHandoffs` (`pkg/criserver/handoff_recover.go`) compares the
  on-disk subtree set against the workload IDs every current container record
  claims, and reclaims (idempotent `HandoffManager.Cleanup`) any subtree no record
  claims, reporting reclaimed-vs-kept counts. It is conservative: a subtree claimed
  by any known container — Created, Running, or Exited, handoff-prepared or not — is
  always kept, because its record may still drive a later Start or carry exit
  evidence that Stop preserved until Remove. Identity evidence is never reread;
  orphan-ness is decided from the record set alone, matching the rest of the
  lifecycle where identity is a start invariant, not an ongoing property (#110,
  #117).
- `Server.RecoverContainers` (`pkg/criserver/recover.go`) calls the sweep after the
  reconcile loop, so the record set reflects recovered states. The call is inert
  when the experimental handoff path is disabled, so the default apple/container
  recovery path is unchanged.

## Restart-state matrix (hermetic)

`pkg/criserver/handoff_recover_test.go` (disk-backed stores + a handoff root under
`t.TempDir`, reopened over the same dirs to simulate the adapter process
restarting):

| Pre-restart state | Subtree after restart | RemoveContainer after restart |
|---|---|---|
| Created | kept (claimed) | cleans subtree, idempotent |
| Running | kept (claimed) | cleans subtree, idempotent |
| Exited | kept (claimed) | cleans subtree, idempotent |
| Failed Start (no identity evidence → unwound to Exited) | kept (claimed) | cleans subtree, idempotent |
| Removed before restart | absent (already cleaned) | redundant remove is idempotent success |
| Orphan (no record) | **reclaimed** | n/a |

Additional hermetic coverage:

- `TestRecoverSweepsOrphanHandoffSubtree` — recovery reclaims an orphan subtree
  while keeping a live container's subtree.
- `TestSweepOrphanHandoffsCountsAndReports` — reclaimed (2) vs kept (1) accounting.
- `TestSweepOrphanHandoffsInertWhenDisabled` — no-op on the default path.
- `pkg/runtime/handoff_test.go`: `TestListContainerIDsReportsSubtrees`,
  `TestListContainerIDsMissingRootIsEmpty`.

`go build ./...`, `go vet ./...`, and `go test ./...` are green.

## Gated live scenario

`pkg/criserver/handoff_recover_integration_test.go` /
`TestLiveHandoffRestartCleanup`, gated behind `MACVZ_CRI_INTEGRATION=1` (boots a
micro-VM; the default run stays hermetic). Against a real apple/container backend
with the handoff path enabled and disk-backed stores it:

1. creates a handoff container (real workload create with the injected handoff bind
   mount);
2. stages an orphan handoff subtree with no backing record;
3. reopens the adapter over the same on-disk state and runs `RecoverContainers`;
4. asserts the orphan subtree is reclaimed and the live container's subtree is kept;
5. asserts `RemoveContainer` then cleans the live subtree and leaves no orphan
   apple/container workload.

Run command:

```sh
MACVZ_CRI_INTEGRATION=1 go test ./pkg/criserver/ \
    -run TestLiveHandoffRestartCleanup -v
```

## Non-goals (honored)

- No full long-duration soak (left to the soak suite).
- No change to non-CRI Virtual Kubelet behavior.
- No rereading of identity evidence during recovery; identity stays a start
  invariant.
