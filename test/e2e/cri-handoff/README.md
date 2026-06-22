# CRI-I4-1 handoff-lifecycle crictl fixture

Experimental MacVz CRI feasibility track (`develop`), issue #118, milestone
**CRI-I4 — Kubelet Validation**. This directory holds a repeatable `crictl`
fixture that drives the **experimental LinuxPod runtime handoff path** through a
full CRI lifecycle and asserts the CRI-R16 handoff invariants
(`docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md`).

It is **not** the shipped Virtual Kubelet path (`test/e2e/e2e.sh`) and **not**
the default apple/container CRI suite (`test/e2e/cri-k3s/run.sh`). It makes no
k3s/kubelet compatibility claim and no multi-day stability claim — both are
explicit non-goals of #118. The k3s in-loop re-run is the separate follow-up
#119.

## What it proves

Driving the handoff-aware adapter (`macvz-cri --experimental-handoff`) with
`crictl`, one container walks `RunPodSandbox → CreateContainer → StartContainer
→ ContainerStatus → StopContainer → RemoveContainer`, asserting:

| Phase | Invariant (CRI-R16 / CRI-I) |
|-------|------------------------------|
| `prepared`   | CreateContainer stages a runtime-private rootfs/handoff subtree — verbose status `handoffPrepared=true`, `handoffPath` exists on disk (#115). |
| `verified`   | StartContainer reaches Running only after the launched process reports the staged rootfs identity back through the handoff evidence file — `identityVerified=true`, `observedIdentity==expectedIdentity` (#116, #117). |
| `stop-keeps` | StopContainer records exit state but **retains** the handoff subtree for post-mortem (`handoffPath` still present after stop) (#117). |
| `remove`     | RemoveContainer deletes the subtree, and a second remove is a no-op — cleanup is idempotent (#117). |

Identity diagnostics are read from the verbose `ContainerStatus` info map
(`handoffStatusInfo`, #117), which `crictl inspect` flattens onto the top level
of its JSON output alongside `status`.

## Identity producer modes (`MACVZ_HANDOFF_PRODUCER`)

The handoff contract expects the **launched in-VM late-rootfs process** to write
the observed rootfs identity into the handoff evidence channel
(`/run/macvz/handoff/identity`, host-visible at `handoffPath/identity`). The
standard apple/container workload has no component that does this yet, so the
fixture provides the producer:

- **`host-sim`** (default) — before `StartContainer`, the fixture writes the
  expected identity into the host-visible handoff channel, standing in for the
  cooperating in-VM process. The identity gate verifies, the container reaches
  Running, and the **full** lifecycle runs. Writing host-side is faithful: it is
  the exact file the in-VM process's write would surface through the writable
  handoff bind mount.
- **`none`** — no producer. `StartContainer`'s bounded-wait gate is expected to
  **time out**, and the fixture asserts the precise identity-evidence diagnostic.
  This is the honest reproduction of the in-VM-producer gap, tracked as the
  CRI-I4 follow-up #119 — not a green pass papered over.

```sh
MACVZ_INTEGRATION=1 ./test/e2e/cri-handoff/run.sh                          # host-sim (full lifecycle)
MACVZ_INTEGRATION=1 MACVZ_HANDOFF_PRODUCER=none ./test/e2e/cri-handoff/run.sh  # blocker repro
```

## Layout

- `run.sh` — the gated fixture (phases above). Without `MACVZ_INTEGRATION=1` it
  prints its plan and exits 0, so it is safe in `go test`-style CI.

## Quick start

```sh
make cri
MACVZ_INTEGRATION=1 ./test/e2e/cri-handoff/run.sh
# or: MACVZ_INTEGRATION=1 make cri-handoff
```

The fixture self-manages a throwaway adapter on a temp socket with a per-run,
**writable** handoff root (the production `/run/macvz/containers` does not exist
on macOS). No cluster is required — `crictl` drives the CRI contract directly,
including the explicit `PullImage` before `CreateContainer`.

## Enabling the handoff path

The handoff path is off by default and is opted into with two adapter flags:

```sh
./bin/macvz-cri \
  --listen "unix://$HOME/.macvz/cri/macvz-cri.sock" \
  --state-dir "$HOME/.macvz/cri/state" \
  --experimental-handoff \
  --handoff-root "$HOME/.macvz/cri/handoff"
```

- `--experimental-handoff` wires `runtime.HandoffManager` into the CRI server so
  CreateContainer prepares the subtree and StartContainer gates Running on
  identity verification.
- `--handoff-root` overrides the subtree root. Leave it empty only on a host
  where `/run/macvz/containers` is writable; on macOS, point it at a per-user
  directory.

Against an already-serving managed adapter, run the fixture with
`MACVZ_CRI_MANAGE=0` and `MACVZ_CRI_SOCKET=…` (see the env table in `run.sh`).

## Environment

See the header of `run.sh` for the full list. The most useful:

| Variable | Default | Purpose |
|----------|---------|---------|
| `MACVZ_INTEGRATION` | `0` | `1` runs the live fixture; otherwise plan-only. |
| `MACVZ_HANDOFF_ROOT` | per-run temp dir | writable handoff subtree root. |
| `MACVZ_CRI_IMAGE` | `busybox:1.36.1` | arm64 image providing `sh`. |
| `CRICTL_TIMEOUT` | `2m` | per-RPC timeout (a real VM boot exceeds crictl's 2s default). |
| `MACVZ_CRI_KEEP` | `0` | `1` keeps the temp tree for inspection. |

## Cleanup

Cleanup is automatic and idempotent: the `EXIT` trap stops/removes any
container and sandbox, stops the adapter, and removes the temp tree (unless
`MACVZ_CRI_KEEP=1`). The `remove` phase additionally asserts no container,
sandbox, or stale socket remains.

## Evidence

A gated run's diagnostics (adapter log, `crictl inspect` dumps for the created
and running states, lifecycle log) land under `MACVZ_CRI_OUT_DIR`. The runbook
and an evidence template are in `docs/CRI_RUNTIME_I4_1_HANDOFF_FIXTURE.md`.
