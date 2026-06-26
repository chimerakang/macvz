# CRI-L6 — LinuxPod live-VM adoption after helper restart (#138)

## Problem

CRI-L5 (#130) and CRI-L6-2 (#136) made a LinuxPod helper restart **correct but
disruptive**: when a restarted `linuxpod-helper` cannot find a Pod's live VM, the
adapter's backend reconciler marks the sandbox `BackendLost`/`NotReady`, kubelet
recreates the Pod, and the workload restarts. That is bounded and leak-free, but it
throws away a micro-VM that — in the common case — kept running across the helper's
own process restart.

This change adds an opt-in **live-VM adoption** path: a restarted helper that still
has its Pod VMs reattaches to them, so the adapter keeps the sandbox `Ready` and
kubelet never recreates a healthy Pod. The fail-fast/recreate path remains the
supported fallback whenever adoption is impossible or incomplete.

## Design

Adoption is modeled at the existing `pkg/runtime/linuxpod.Backend` seam — the same
contract `FakeBackend`, `HelperClient`, and the Swift helpers already implement —
so the apple/container CRI path is untouched and the mechanism is fully testable
without Apple hardware.

### Contract (ProtocolVersion 5 → 6)

- `Backend.Adopt(ctx, podID) (AdoptionResult, error)` — asks the helper to reacquire
  a Pod VM and its containers from its durable journal after the helper's own
  restart. It is the live-VM counterpart to the fail-fast `PodStatus` probe.
  - `AdoptionResult{Adopted:true, Containers:[…]}` — the VM was reattached; the
    containers' current live status is returned so the adapter reconciles in one
    round-trip. Subsequent `PodStatus`/`Status` observe the reattached VM.
  - `AdoptionResult{Adopted:false, Reason:…}` with **no error** — the VM did not
    survive (or adoption is incomplete); the adapter falls back to recreate.
  - `ErrPodNotFound` — the pod is unknown to the helper's journal entirely.
  - `ErrUnsupported` — the helper has no durable journal (`Capabilities.Adopt`
    false); the adapter treats this as "feature off" and uses the legacy fallback.
- `Capabilities.Adopt` advertises journal-backed adoption.
- `HelperInfo.Adoption{Supported, AdoptedPods, LostPods}` reports the helper's
  startup adoption pass through `Ping`, so an operator/diagnostics see at the
  handshake whether a restart preserved workloads or fell back.

Identity is **not** re-verified at adoption: it is a start invariant (#110/#117), so
a container that verified at start stays verified across the helper restart.

### Modeled backend (`FakeBackend`)

The fake gains a durable journal and `SimulateHelperRestart()`, which:
1. snapshots the live pods into the journal (per pod: whether its micro-VM survived,
   driven by `VMSurvivesRestart` so tests exercise both outcomes),
2. drops the live in-memory handles — so an un-adopted pod answers `ErrPodNotFound`,
   exactly as the pre-#138 fail-fast path, and
3. runs the startup adoption pass: pods whose VM survived are reattached into the
   live set and counted `AdoptedPods`; the rest are counted `LostPods`.

`Adopt` then reports the per-pod outcome. This models a real helper that persists a
journal to disk, reloads it on startup, and reacquires the VM handles whose
processes outlived it.

### Adapter (`LinuxPodService`)

`AdoptSandboxes(ctx)` runs once at startup (in `serveLinuxPod`, after
`RecoverNetwork`, before `RecoverContainers`):

- For each `Ready` sandbox it calls `backend.Adopt`.
- `ErrUnsupported` → immediate no-op return; the reconciler's `BackendLost`/recreate
  path handles everything, identical to a pre-#138 helper.
- Adopted cleanly (every recorded-`Running` container confirmed live) → the sandbox
  stays `Ready`; kubelet does not recreate.
- Not adopted, or **incomplete** (a recorded-`Running` container is not in the live
  set) → the sandbox is funneled through the shared `markSandboxBackendLostLocked`
  fallback (network detached, containers `BackendLost`, sandbox `NotReady`). This is
  the single fallback both the periodic reconciler and the adoption pass use, so the
  "never leave a stale Running-but-unusable Pod" guarantee holds regardless of which
  path detected the loss.

## Acceptance mapping

| Criterion | Status |
| --- | --- |
| Helper restart preserves a LinuxPod-backed Pod without recreate when adoption succeeds | ✅ proven against the modeled backend (`TestLinuxPodServiceAdoptsLiveVMAfterHelperRestart`) and Swift stub contract; real-helper implementation pending |
| `exec`/logs/stats/port-forward/Service work after adoption | ✅ exec exercised post-adoption in the adapter test; live surfaces resume because `PodStatus`/`Status` observe the reattached VM; full live run pending |
| Incomplete adoption never leaves stale Running-but-unusable Pod; fallback intact | ✅ `TestLinuxPodServiceAdoptionIncompleteFallsBack`, `TestLinuxPodServiceFallsBackToRecreateWhenVMLost` |
| Live test evidence compares adoption vs fallback | ✅ at the modeled-backend level (adopt vs VM-gone vs incomplete); live real-helper probe `/tmp/macvz-live-adoption-probe-20260627002536` confirms today's helper advertises `adopt:false` and returns `Unsupported`, so real journal-backed reattachment remains pending |

## Non-goals (honored)

- **Does not block CRI-L6 stability.** Fail-fast/recreate (#136) remains the
  supported default. The real `linuxpod-helper` advertises `adopt:false` and answers
  `Unsupported` until real journal-based VM reattachment lands, so live behavior is
  unchanged today.
- No change to the shipped Virtual Kubelet path or the apple/container CRI backend.
- Real reacquisition of Apple Containerization VM handles after a helper process
  restart is the remaining live step (needs Apple Silicon hardware + a persisted
  helper journal); the Go contract, adapter, and `FakeBackend` model and test the
  mechanism so that work has a fixed target.
