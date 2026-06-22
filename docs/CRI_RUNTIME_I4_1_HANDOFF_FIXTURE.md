# CRI-I4-1 Handoff-Lifecycle crictl Fixture (#118)

Date: 2026-06-22
Milestone: CRI-I4 — Kubelet Validation
Fixture: `test/e2e/cri-handoff/run.sh` (`make cri-handoff`)

## Purpose

Before any k3s/kubelet in-loop testing (#119), validate the experimental
LinuxPod **runtime handoff** path (CRI-I, #109..#117) against the CRI socket with
`crictl`, the same RuntimeService contract a kubelet drives. The fixture walks
one container through the full lifecycle and asserts the CRI-R16 handoff
invariants (`docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md`):

- CreateContainer stages a runtime-private rootfs/handoff subtree (#115).
- StartContainer gates Running on handoff identity verification (#116).
- Verbose ContainerStatus surfaces identity diagnostics (#117).
- StopContainer retains the subtree; RemoveContainer deletes it idempotently (#117).

This is an isolated `develop`-track feasibility probe. It is **not** the shipped
Virtual Kubelet path and makes **no** k3s/kubelet compatibility or multi-day
stability claim — both are explicit non-goals of #118.

## What was added

- `--experimental-handoff` / `--handoff-root` flags on `macvz-cri`
  (`cmd/macvz-cri/main.go`): the binary now wires `runtime.HandoffManager` into
  the CRI server, so the handoff path — previously reachable only from Go tests —
  can be exercised live. Off by default; `--handoff-root` points the subtree at a
  writable per-user dir because the production `/run/macvz/containers` does not
  exist on macOS.
- `test/e2e/cri-handoff/run.sh` + `README.md`: the gated crictl fixture. Without
  `MACVZ_INTEGRATION=1` it prints its plan and exits 0 (CI-safe).
- `make cri-handoff` target.

## Exact commands

```sh
make cri
# Full lifecycle (host-sim identity producer; default):
MACVZ_INTEGRATION=1 ./test/e2e/cri-handoff/run.sh
# Honest blocker reproduction (no producer; start gate must time out):
MACVZ_INTEGRATION=1 MACVZ_HANDOFF_PRODUCER=none ./test/e2e/cri-handoff/run.sh
```

Diagnostics (adapter log, `crictl inspect` dumps, lifecycle log) land under
`MACVZ_CRI_OUT_DIR` (a per-run temp dir by default; set it to keep them).

## Identity producer modes

The handoff contract expects the **launched in-VM late-rootfs process** to write
its observed rootfs identity into the handoff evidence channel
(`/run/macvz/handoff/identity`, host-visible at `handoffPath/identity`). The
standard apple/container workload has no component that does this yet, so the
fixture supplies the producer:

- `host-sim` (default): the fixture writes the expected identity into the
  host-visible handoff channel before StartContainer, standing in for the
  cooperating in-VM process. This is faithful — it is the exact file the in-VM
  write would surface through the writable handoff bind mount.
- `none`: no producer; the bounded-wait gate is expected to time out, and the
  fixture asserts the precise identity-evidence diagnostic.

## Gated run evidence (2026-06-22, Apple Silicon, apple/container 1.0.0)

Host: `darwin/arm64`, `macvz-cri 1a6698b-dirty` (go1.25.8), apple/container
service `running`, image `busybox:1.36.1`.

### host-sim — full lifecycle, all phases passed

```
PASS crictl version handshake
PASS adapter logged handoff path enabled
PASS image pulled (busybox:1.36.1)
PASS runp / create
PASS handoffPrepared=true at create
PASS handoff subtree staged on disk (.../handoff/macvz-cri-<id>/handoff)
PASS expectedIdentity staged at create (macvz-rootfs-id=macvz-cri-<id>)
PASS wrote handoff identity evidence to .../handoff/identity
PASS start returned (identity verified within timeout)
PASS identityVerified=true
PASS observedIdentity matches expectedIdentity (macvz-rootfs-id=macvz-cri-<id>)
PASS handoff subtree retained after stop (post-mortem evidence intact)
PASS handoff subtree deleted after remove
PASS no containers / no sandboxes / no stale CRI socket remain
PASS CRI-I4-1 handoff-lifecycle fixture: all phases passed
```

Verbose `crictl inspect` of the Running container (handoff keys, flattened onto
the inspect document's top level):

```
workloadID=macvz-cri-3bac0138b7d409c872069442
handoffPrepared=true
identitySource=handoff
handoffMountPoint=/run/macvz/handoff
handoffPath=<temp>/handoff/macvz-cri-3bac0138b7d409c872069442/handoff
expectedIdentity=macvz-rootfs-id=macvz-cri-3bac0138b7d409c872069442
observedIdentity=macvz-rootfs-id=macvz-cri-3bac0138b7d409c872069442
identityVerified=true
procRoot=/ (host-sim producer)
```

Status transitions observed: `CONTAINER_CREATED` (handoffPrepared, evidence
`missing`) → `CONTAINER_RUNNING` (identityVerified=true) → stop (subtree retained)
→ remove (subtree gone).

### none — blocker reproduced honestly

With no producer, StartContainer's bounded wait (default 30s) expires and the
adapter never marks the container Running:

```
SKIP producer skipped (start gate is expected to time out)
PASS start gate timed out with the expected identity-evidence diagnostic (#119)
SKIP verified (no producer; container never reached Running)
PASS handoff subtree retained after stop
PASS handoff subtree deleted after remove
PASS CRI-I4-1 handoff-lifecycle fixture: all phases passed
```

Exact StartContainer error returned to the CRI client:

```
rpc error: code = FailedPrecondition desc = StartContainer: handoff identity
verification failed: runtime: handoff identity evidence missing: evidence did
not arrive before deadline (expected "macvz-rootfs-id=macvz-cri-<id>"):
context deadline exceeded
```

## Precise blocker for #119

The CRI **adapter** side of the handoff is complete and verified end-to-end:
prepare, the identity gate, verbose diagnostics, retain-on-stop, and
idempotent-delete-on-remove all work against a live micro-VM. The remaining gap
is the **in-VM evidence producer**: an ordinary apple/container workload writes
no identity evidence, so without a cooperating producer the StartContainer gate
times out by design. Wiring that producer (the late-rootfs process reporting its
identity from inside the VM) is the next step before a kubelet/k3s in-loop re-run
(#119).

## Cleanup

Cleanup is automatic and idempotent: an `EXIT` trap stops/removes any container
and sandbox, stops the adapter, and removes the temp tree (unless
`MACVZ_CRI_KEEP=1`). The `remove` phase asserts no container, sandbox, or stale
socket remains; a second RemoveContainer is a no-op.

## Validation

`go build ./...`, `go vet ./cmd/macvz-cri/... ./pkg/criserver/... ./pkg/runtime/...`,
and `go test ./pkg/criserver/... ./pkg/runtime/...` are green. Both fixture modes
were run live on Apple Silicon (evidence above).
