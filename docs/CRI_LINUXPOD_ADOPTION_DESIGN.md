# CRI-L6 — LinuxPod live-VM adoption after helper restart (#138)

## Problem

CRI-L5 (#130) and CRI-L6-2 (#136) made a LinuxPod helper restart **correct but
disruptive**: when a restarted `linuxpod-helper` cannot find a Pod's live VM, the
adapter's backend reconciler marks the sandbox `BackendLost`/`NotReady`, kubelet
recreates the Pod, and the workload restarts. That is bounded and leak-free, but it
throws away a micro-VM that — in the common case — kept running across the helper's
own process restart.

This change adds an opt-in **adoption protocol**: a restarted helper consults a
durable journal for each previously-created Pod and either reattaches the live VM
or reports that the Pod must fall back to recreate. The fail-fast/recreate path
remains the supported fallback whenever adoption is impossible or incomplete.

## Design

Adoption is modeled at the existing `pkg/runtime/linuxpod.Backend` seam — the same
contract `FakeBackend`, `HelperClient`, and the Swift helpers already implement —
so the apple/container CRI path is untouched and the mechanism is fully testable
without Apple hardware.

### Contract (ProtocolVersion 5 → 6)

- `Backend.Adopt(ctx, podID) (AdoptionResult, error)` — asks the helper to resolve a
  Pod VM and its containers from its durable journal after the helper's own restart.
  It is the live-VM counterpart to the fail-fast `PodStatus` probe.
  - `AdoptionResult{Adopted:true, Containers:[…]}` — the VM was reattached; the
    containers' current live status is returned so the adapter reconciles in one
    round-trip. Subsequent `PodStatus`/`Status` observe the reattached VM.
  - `AdoptionResult{Adopted:false, Reason:…}` with **no error** — the VM did not
    survive (or adoption is incomplete); the adapter falls back to recreate.
  - `ErrPodNotFound` — the pod is unknown to the helper's journal entirely.
  - `ErrUnsupported` — the helper has no durable journal (`Capabilities.Adopt`
    false); the adapter treats this as "feature off" and uses the legacy fallback.
- `Capabilities.Adopt` advertises the durable adoption protocol. A helper may still
  answer `Adopted:false` for a journaled pod it cannot reacquire.
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
`RecoverNetwork`, before `RecoverContainers`). The periodic backend reconciler also
tries `Adopt` once when `PodStatus` reports a missing live handle, covering a helper
restart while `macvz-cri` itself stayed alive:

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
| Helper restart preserves a LinuxPod-backed Pod without recreate when adoption succeeds | ✅ proven against the modeled backend (`TestLinuxPodServiceAdoptsLiveVMAfterHelperRestart`) and Swift stub contract; the Go reconciler now also attempts adoption after a helper-only backend miss (`TestLinuxPodServiceReconcilerAdoptsAfterHelperRestart`) |
| `exec`/logs/stats/port-forward/Service work after adoption | ✅ exec exercised post-adoption in the adapter tests; live surfaces resume because `PodStatus`/`Status` observe the reattached VM; full true-reattach live run pending |
| Incomplete adoption never leaves stale Running-but-unusable Pod; fallback intact | ✅ `TestLinuxPodServiceAdoptionIncompleteFallsBack`, `TestLinuxPodServiceFallsBackToRecreateWhenVMLost` |
| Live test evidence compares adoption vs fallback | ✅ at the modeled-backend level (adopt vs VM-gone vs incomplete); live real-helper probe on `test@192.168.1.122` after this change confirms `Ping` advertises `simulated=false`, `protocolVersion=6`, `capabilities.adopt=true`, `adoption.supported=true`; a journaled lost-pod fixture reports `lostPods=1` after helper restart and `Adopted:false` with the Containerization lookup/reattach diagnostic, then `Cleanup` removes the journal entry and a restart returns `lostPods=0` |

## Non-goals (honored)

- **Does not block CRI-L6 stability.** Fail-fast/recreate (#136) remains the
  supported default. The real `linuxpod-helper` now implements the journal-backed
  protocol and reports `Adopted:false` for journaled pods it cannot reacquire, so the
  adapter falls back immediately without treating adoption as unsupported.
- No change to the shipped Virtual Kubelet path or the apple/container CRI backend.
- Real reacquisition of Apple Containerization VM handles after a helper process
  restart remains blocked by the current public Containerization API exposing
  creation but no VM lookup/reattach hook. The journal/protocol path is implemented,
  so adding true reattach later is localized to the helper's startup/adopt logic.

---

# CRI-L6-4 — Supervisor-backed true adoption (#139)

## Why a single process cannot truly reattach

#138 implemented the durable journal and the `Adopt` protocol, but the real
`linuxpod-helper` still had to answer `Adopted:false` for a journaled Pod after its
own restart: the vendored Apple Containerization API exposes
`VirtualMachineManager.create(config:)` and **no public VM lookup/reattach** hook, so
a fresh helper process has no way to reconstruct a `VZVirtualMachineInstance` handle
for a VM a previous process booted. The blocker is API shape, not journal logic.

## Ownership inversion

#139 moves live VM ownership out of the main helper and into **per-Pod supervisor
processes**, so the handle never has to be reconstructed — it is simply still held by
a process that outlived the main helper's restart.

- `linuxpod-helper serve` (default subcommand) is the **router**: it owns the public
  CRI NDJSON socket, the durable supervisor journal, and routing. It owns no VM.
- `linuxpod-helper supervise-pod` (hidden subcommand) is a **per-Pod supervisor**: it
  owns exactly one `LinuxPod` / `VZVirtualMachineInstance` and serves the *same* NDJSON
  protocol on a private socket. It is the unchanged VM-owning backend (`LinuxPodBackend`)
  from #126–#138, now hosted in its own process. It calls `setsid()` at startup so a
  `SIGTERM`/`SIGKILL` to the router alone leaves the Pod VM running.

Bare invocation (`linuxpod-helper --socket … --kernel …`) still works: options with no
subcommand token route to the default `serve`, so existing operators and the
real-helper lifecycle test are unaffected.

### Routing

| Op | Router behavior |
| --- | --- |
| `CreatePod` | spawn a detached supervisor, wait for its socket, forward `CreatePod`, record the journal entry (`podID`, socket, pid, startUnix, sandbox addr/ns) |
| pod-scoped ops (`PrepareContainerRootfs`, `CreateContainer`, `Start/Stop/Remove`, `Status`, `ContainerLogPath`, `ExecSync`, `ContainerStats`, …) | forward verbatim to that Pod's supervisor; a transport failure drops the dead client and surfaces the error, which the reconciler reads as `BackendLost` |
| `Adopt` | reconnect to the journaled supervisor: reachable → forward (`Adopted:true` + live containers); unreachable → `Adopted:false` (no error) so the adapter falls back |
| `Cleanup` | drive the supervisor's own `Cleanup` (VM/rootfs/interface teardown), terminate the supervisor, drop the journal entry; idempotent if the supervisor is already gone |
| `Ping` | report the startup adoption pass (`adoption.supported`, `adoptedPods`, `lostPods`) |

### Startup adoption pass

`RouterBackend.init` loads `supervisor-journal.json` and probes each recorded
supervisor with a `Ping`: a reachable supervisor still owns its live Pod VM →
`adoptedPods++` and its live client is retained; an unreachable one (the supervisor
died while the router was down) → `lostPods++` and its stale journal entry is dropped
so the adapter recreates rather than wedging. The Go adapter and protocol are
unchanged — the router speaks the exact `HelperClient` wire contract, and the real
helper's `Adopt` now returns `Adopted:true` because a supervisor kept the VM alive.

### Failure handling

- Writing to a dead supervisor socket would raise `SIGPIPE` and take the router down,
  so the helper ignores `SIGPIPE` process-wide and sets `SO_NOSIGPIPE` on each
  supervisor connection; a dead supervisor surfaces as `EPIPE`/`EOF` → adoption
  fallback, never a router crash.
- A supervisor that dies while the router is alive is detected on the next routed op
  (it fails) and by `Adopt` (returns `Adopted:false`), funneling into the same
  `markSandboxBackendLostLocked` fallback as every other loss.

## Tests

`TestSwiftRouterSupervisorAdoption` (gated `MACVZ_LINUXPOD_HELPER=1`) exercises the
whole inversion **without booting a VM** by pointing `--supervisor-command` at the
in-memory stub: create-via-router, kill the router only, restart and confirm
`Ping` reports `adoptedPods=1`/`lostPods=0` and `Adopt` reattaches the running
container; then kill the supervisor and confirm `Adopt`→`adopted:false`, a routed
`Status` fails, and `Cleanup` removes the journal entry idempotently. On hardware the
same router spawns the real `supervise-pod` VM owner (`TestRealLinuxPodHelperLifecycle`,
gated `MACVZ_LINUXPOD_REAL_HELPER=1`).

Live smoke on `test@192.168.1.122` with the real router/supervisor helper:

- deployed the new signed helper without changing `macvz-netd`, pf policy, or routes;
- `CreatePod(pod-139-live)` through the public helper socket spawned supervisor pid
  `15915`, returned `sandboxAddress=192.168.82.2`, and wrote
  `supervisor-journal.json` with the private supervisor socket;
- restarted only the public router helper. The supervisor survived with PPID `1`, and
  the new router reported `adoption.supported=true`, `adoptedPods=1`, `lostPods=0`;
- `Adopt(pod-139-live)` returned `Adopted:true`, and routed `PodStatus` still reported
  the same running Pod VM;
- `Cleanup` returned `podRemoved=true`, a second Cleanup was an idempotent no-op, the
  supervisor journal became empty, only the public router process remained, and the
  default route stayed `192.168.1.1` via `en0`.
