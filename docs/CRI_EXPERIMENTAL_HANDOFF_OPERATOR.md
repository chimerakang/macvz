# Experimental LinuxPod CRI Handoff — Operator Guide & Feature-Gate Decision (CRI-I5-3, #123)

Date: 2026-06-22

Outcome: `experimentalHandoffGateDefined`

## Status: experimental, off by default

The handoff-aware LinuxPod CRI path is an **experimental feasibility track**, not a
production runtime. It is **not** the shipped MacVz product surface: MacVz remains a
Virtual Kubelet node provider (`cmd/macvz-kubelet`) that presents an Apple Silicon
Mac as a Kubernetes node and runs OCI workloads as `apple/container` micro-VMs (see
[README.md](../README.md)). This document does not change that positioning.

The handoff path lives only in the separate `macvz-cri` adapter
(`cmd/macvz-cri`, the CRI feasibility spike, [CRI_FEASIBILITY.md](CRI_FEASIBILITY.md))
and is **disabled unless explicitly opted in**. With it off, `macvz-cri` runs the
default `apple/container` single-container-per-Pod path with no handoff preparation
and no identity gate — exactly as before this track existed.

Do not treat this path as production-ready. kubelet/k3s validation is operator-pending
(CRI-I4 `kubeletHandoffSmokeBlocked`, [CRI_RUNTIME_I4_2_INLOOP_HANDOFF_REPORT.md](CRI_RUNTIME_I4_2_INLOOP_HANDOFF_REPORT.md)).

## The feature gate

The single, canonical gate is the `macvz-cri` CLI flag:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--experimental-handoff` | `false` (off) | Opt into the experimental LinuxPod runtime handoff path. |
| `--handoff-root <dir>` | `""` → `/run/macvz/containers` | Root for the runtime-private per-container rootfs/handoff subtree. The production default is **not writable on macOS**; point this at a writable per-user directory to exercise the path locally. |

There is intentionally **no environment-variable gate** for the binary: it follows the
same explicit-CLI-flag convention as `--experimental-multi-container`, so enabling an
experimental path is always visible in the process arguments and the service unit,
never implicit in the environment. (The e2e harness's `MACVZ_HANDOFF=1` gates a *test
phase*, not the adapter — see [test/e2e/cri-k3s/README.md](../test/e2e/cri-k3s/README.md).)

When enabled, the adapter logs at startup:

```
experimental LinuxPod handoff path enabled (off by default; not the shipped Virtual Kubelet runtime) root=<dir> ...
```

## Supported behavior (when enabled)

- Single-container Pods only. `CreateContainer` stages a runtime-private
  `<root>/<workloadID>/{rootfs,handoff}` subtree, prepares the in-rootfs mount point,
  stages the expected rootfs identity, and injects the handoff bind mount before the
  workload is created (CRI-I3-1, #115).
- `StartContainer` gates the transition to Running on handoff identity verification:
  the launched process must report the staged identity back through the evidence file
  within a bounded timeout, else the start fails `FailedPrecondition` and the workload
  is unwound (never left Running) (CRI-I3-2, #116).
- `StopContainer` preserves the handoff evidence for post-mortem; `RemoveContainer`
  cleans the subtree idempotently; verbose `ContainerStatus` surfaces identity
  diagnostics (CRI-I3-3, #117).
- Restart recovery reclaims orphan handoff subtrees no container record claims and
  keeps every claimed subtree; `RemoveContainer` stays idempotent across restarts
  (CRI-I4-3, #120, [CRI_RUNTIME_I4_3_RESTART_CLEANUP_REPORT.md](CRI_RUNTIME_I4_3_RESTART_CLEANUP_REPORT.md)).

## Unsupported behavior

- **Not production-ready**; no compatibility or stability guarantee. The gate may change
  or be removed.
- **No multi-container Pods** on this path (the multi-container probe is a separate
  experimental gate, #82).
- **No host-namespace Pods** (hostNetwork/hostPID/hostIPC) — rejected at
  `RunPodSandbox` by the per-Pod-VM model (#84); register the node with the advertised
  taint/label so the scheduler routes them elsewhere (see `macvz-cri --preflight`).
- **The handoff directory is not a Kubernetes volume** and not an API surface; it is
  runtime-private and never kubelet-visible.
- **No kubelet/k3s production smoke claim** yet (operator-pending topology).

## Honest failure when disabled or unsupported

- **Disabled (default):** the handoff helpers are inert; the default `apple/container`
  path runs. `macvz-cri --preflight` reports `experimental handoff … [OK] disabled …`
  so an operator can confirm the path is off rather than silently assuming it is on.
- **Enabled but environment unsupported:** the adapter validates the handoff root at
  startup and **fails loudly** before serving, e.g. on macOS with the default root:

  ```
  macvz-cri exited with error: --experimental-handoff: handoff root "/run/macvz/containers" could not be created: mkdir /run/macvz: read-only file system; pass --handoff-root to a writable directory (the production /run/macvz/containers is not writable on macOS)
  ```

  The same condition is a `FAIL` in `--preflight`, with the `--handoff-root` remedy, so
  the failure is caught before the kubelet ever connects. There is no silent degrade to
  the non-handoff path when the operator explicitly asked for handoff.

## Install / run / cleanup for operators

**Build:**

```sh
make cri      # builds the macvz-cri adapter
```

**Preflight (safe, never serves or boots a VM):**

```sh
macvz-cri --preflight --experimental-handoff --handoff-root "$HOME/.macvz/cri/handoff"
```

Resolve any `FAIL` (apple/container CLI on PATH, writable socket/state/handoff dirs)
before starting; `WARN` items are degradations the adapter still runs with.

**Run (experimental handoff enabled, writable per-user root):**

```sh
macvz-cri \
  --listen unix:///tmp/macvz-cri.sock \
  --state-dir "$HOME/.macvz/cri/sandboxes" \
  --experimental-handoff \
  --handoff-root "$HOME/.macvz/cri/handoff"
```

**Cleanup:**

- The adapter cleans each container's handoff subtree on `RemoveContainer` and reclaims
  orphan subtrees on restart, so steady-state needs no manual cleanup.
- To fully reset the experimental state, stop the adapter and remove the handoff root
  and state dir:

  ```sh
  rm -rf "$HOME/.macvz/cri/handoff" "$HOME/.macvz/cri/sandboxes"
  ```

  This is safe only for the experimental adapter's own directories; never point
  `--handoff-root` at a shared or system path.

## Non-goals (honored)

- Does not label the CRI path production-ready.
- Does not remove or weaken the Virtual Kubelet node-provider positioning.
- Does not add an environment-variable gate (CLI flag is the single explicit gate).
