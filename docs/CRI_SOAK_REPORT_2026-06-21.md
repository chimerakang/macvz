# CRI Adapter Real-Hardware Soak Report — 2026-06-21

Issue **#83** (CRI-P9 follow-up). This records the first **live, real-hardware**
run of the experimental `macvz-cri` adapter's k3s-facing harness
(`test/e2e/cri-k3s/`) on Apple Silicon: the `crictl` compatibility suite plus a
sustained create/delete soak with leak/orphan guards, driving the adapter over
the CRI socket against real `apple/container` micro-VMs.

It is scoped to the experimental `develop` CRI track. It does **not** gate the
shipped Virtual Kubelet path.

## Scope and honesty note

- **What ran:** the canonical CRI gRPC contract a kubelet drives, exercised by
  `crictl` (the reference CRI client) against the live adapter, which booted real
  `apple/container` micro-VMs for every Pod. Both the compatibility suite and a
  sustained soak completed on real hardware.
- **What did *not* run:** a real Linux **kubelet/k3s control plane** in the loop,
  and a literal **multi-day** duration. There is no native macOS kubelet on the
  dev host, so `crictl` stands in as the CRI client. This clears the *soak harness
  / adapter resource-boundedness* blocker on real hardware; a multi-day run with a
  real kubelet remains open (see `docs/CRI_FEASIBILITY.md`, CRI-P9).

## Build under test

| Item | Value |
| --- | --- |
| Repository | `macvz` |
| Commit | `2b304b9` (`develop`; working tree carried the #83 fixes below) |
| Date | 2026-06-21 |
| `macvz-cri` version | `b0ae087-dirty` (build under test) |
| Kubernetes / k3s | not in loop — `crictl` is the CRI client |
| `crictl` version | 1.36.0 |
| `apple/container` version | 1.0.0 |
| Go | go1.25.8 |
| Test image | `busybox:1.36.1` (arm64) |

## Host

| Item | Value |
| --- | --- |
| Mac model | Mac13,1 (Apple M1 Max, 10 cores) |
| RAM | 32 GiB |
| macOS | 26.5.1 (arm64) |
| `apple/container` system | running |

## Harness

```sh
make cri
# Compatibility suite (single-container Pod lifecycle, restart recovery, cleanup)
MACVZ_INTEGRATION=1 MACVZ_CRI_KEEP=1 MACVZ_CRI_OUT_DIR=/tmp/cri-k3s-compat make cri-k3s
# Sustained soak (create/delete cycles; per-iteration RSS + orphan sampling)
MACVZ_INTEGRATION=1 MACVZ_CRI_KEEP=1 MACVZ_SOAK_ITERATIONS=360 \
  MACVZ_CRI_OUT_DIR=/tmp/cri-soak-long make cri-soak
```

| Item | Value |
| --- | --- |
| Duration target | multi-day (operator-run; harness accepts unbounded `MACVZ_SOAK_ITERATIONS`) |
| Actual soak duration | ~19m43s (2026-06-21T13:15:41Z → 13:35:24Z), ≈3.3 s/cycle |
| Soak iterations | 360 create/delete cycles |
| Fixture (compat) | single-container Pod: busybox `httpd`, read-only projected config mount, exec probe |
| Fixture (soak) | single-container Pod: busybox `sh -c 'sleep 1'` |
| RSS leak bound | 65536 KiB (64 MiB) end-to-end growth |

## Results — compatibility suite (`run.sh`)

All phases passed live:

| Phase | Result | Evidence |
| --- | --- | --- |
| preflight | PASS | no hard-dependency failures (apple/container, socket, state dir) |
| adapter handshake | PASS | `crictl version` + `crictl info` |
| image pull | PASS | `busybox:1.36.1` pulled via CRI ImageService |
| single-container lifecycle | PASS | runp → create → start → ps |
| logs + projected config mount | PASS | read-only config bound; `crictl logs` returns the marker |
| exec probe | PASS | `crictl exec sh -c 'exit 0'` returns real exit code |
| unsupported shape | PASS | hostNetwork Pod rejected with a clear diagnostic (not booted) |
| adapter restart recovery | PASS | container survived adapter stop+restart (state recovered) |
| cleanup | PASS | no residual container, sandbox, or socket after teardown |

## Results — sustained soak (`soak.sh`)

| Guard | Result | Evidence |
| --- | --- | --- |
| orphan guard | **PASS** | no sandbox/container left in the CRI view after the final cycle (live counts were 0 at every one of the 360 samples) |
| leak guard (RSS) | **PASS** | adapter RSS grew 4,784 KB (~4.7 MiB) end-to-end, well under the 64 MiB bound |
| resource boundedness | **PASS** | per-iteration RSS plateaued; 360 samples in `samples.csv` |

### Adapter RSS trend

Over 360 create/delete cycles (~19m43s), adapter resident memory rose modestly
from its cold-start value and then **plateaued** — it did not grow monotonically,
which is the signature a leak would show.

| Metric | Value |
| --- | --- |
| First-iteration RSS | 20,816 KB (~20.3 MiB) |
| Last-iteration RSS | 25,600 KB (~25.0 MiB) |
| Min RSS | 18,656 KB (~18.2 MiB) |
| Max RSS | 25,744 KB (~25.1 MiB) |
| Mean RSS | 24,135 KB (~23.6 MiB) |
| End-to-end growth (first→last) | 4,784 KB (~4.7 MiB) — bound is 65,536 KB |
| Peak over first | 4,928 KB (~4.8 MiB) |
| Live sandboxes / containers per sample | 0 / 0 at all 360 samples |

The curve climbs into the low-20s MiB over the first few dozen cycles and then
oscillates within a narrow ~18–25 MiB band for the remaining ~300 cycles, with no
upward drift — consistent with bounded, GC-stable steady-state behavior and full
per-cycle workload cleanup. Raw samples: `samples.csv` (`iteration,rss_kb,sandboxes,containers`).

## Restart-recovery observations

- **Adapter restart:** exercised by the compatibility suite — the adapter was
  stopped and restarted mid-lifecycle and `RecoverContainers` reconciled the
  persisted state against the live `apple/container` workload without orphaning or
  duplicating it (`recovered CRI container state after restart … resumedLogPumps=1`).
  The recovered container remained visible to `crictl ps` and its log pump resumed
  (the projected-config marker appears twice in the container log: once at first
  boot, once after the pump resumed).
- **kubelet restart:** not exercised — no real kubelet runs on the macOS dev host.
  The adapter-side recovery contract a kubelet would rely on (idempotent
  reconcile, no orphan/dup) is the part demonstrated above.

## Defects found by the live run

The first live run was **not** clean; the failures it surfaced are the value of
running on real hardware. One was a real adapter bug; the rest were harness
defects masked by plan-only validation.

### Adapter bug (fixed)

- **`ContainerStatus` reported a relative `log_path`.** The adapter returned the
  container's relative `log_path` (e.g. `app.log`) in `ContainerStatus`, but the
  CRI contract requires the **absolute** path (sandbox `log_directory` + container
  `log_path`); `crictl logs`/kubelet resolve it directly and otherwise fail with
  `lstat app.log: no such file`. Fixed to report the absolute path, covered by a
  regression test (`pkg/criserver/container_test.go:TestContainerStatusReportsAbsoluteLogPath`).

### Harness defects (fixed)

- **Preflight ran after the adapter started**, so its "socket not already serving"
  check always falsely FAILed against our own adapter. Reordered to run preflight
  before starting the adapter.
- **The projected-config fixture used an arbitrary `hostPath`** that the adapter
  correctly rejects by default (safe macOS posture). The harness now passes
  `--volume-host-path-allowed "$OUT_DIR"`, exactly as the adapter's diagnostic
  instructs (a real kubelet's mounts come from the always-allowed pods dir).
- **`ps`/restart checks grepped the full 64-char id against `crictl ps`'s table**,
  whose `CONTAINER` column truncates ids to 13 chars, so they never matched even
  though the container was Running. Switched to matching `crictl ps -q` (full ids).
- **`crictl`'s 2s default RPC timeout was too short for a real micro-VM boot**,
  surfacing as a spurious `StartContainer` `DeadlineExceeded`. The harness now uses
  a kubelet-like `--timeout 2m` (`CRICTL_TIMEOUT`).
- **The soak orphan/RSS counter used `grep -c . || echo 0`**, which double-counts
  on an empty list (grep exits non-zero, so the `|| echo 0` fires in addition to
  grep's own `0`), corrupting `samples.csv` and failing the orphan guard spuriously.
  Switched to `wc -l | tr -d ' '`.

After these fixes the compatibility suite and the soak both pass live.

## Failures and follow-ups

- A multi-day run and a real Linux kubelet/k3s control plane in the loop remain
  outstanding (tracked in `docs/CRI_FEASIBILITY.md` CRI-P9). The harness supports
  the longer run unchanged via `MACVZ_SOAK_ITERATIONS`.
- The remaining architectural route-two blockers are tracked outside this run:
  multi-container Pods (#82) remain blocked on shared-netns support, while
  host-namespace workloads are handled separately by #84's honest
  scheduling-exclusion design.

## Cleanup

- The soak's orphan guard asserts no sandbox/container remains in the CRI view
  after the final cycle (PASS — 0 live sandboxes/containers at every sample).
- The compatibility suite asserts no stale socket, container, or sandbox after
  teardown (PASS).
- `MACVZ_CRI_KEEP=1` preserved per-run diagnostics under the output dirs for this
  report. A post-review host audit found two stale `macvz-cri-*` debug workloads
  from earlier kept runs; both were stopped/removed before accepting #83, and the
  final `container list --all` audit showed no `macvz-cri-*` workloads.
