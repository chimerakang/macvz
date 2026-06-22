# CRI-I2-3 Runtime Handoff Launch Report (#114)

Date: 2026-06-22T10:40:12Z

## Purpose

CRI-R15 proved the late-rootfs identity evidence channel inside the gated Swift
vminitd harness (see
[CRI_RUNTIME_R15_EVIDENCE_CHANNEL_REPORT.md](CRI_RUNTIME_R15_EVIDENCE_CHANNEL_REPORT.md)).
CRI-I2-3 moves that proof onto the **production handoff helper and spec shape**:
the Go `runtime.HandoffManager` / `runtime.HandoffMeta` introduced in CRI-I1
(#109, #110) and the reserved `/run/macvz` layout accepted in CRI-R16 (#108).

The deliverable is a gated Go integration test, not a new Swift probe. It
exercises the shipped apple/container backend rather than the vminitd late-rootfs
primitive: it proves the production handoff path lifecycle, the VirtioFS bind
mount at `runtime.HandoffMountPoint`, the R16 identity-file format, exact-key
identity parsing, and `HandoffMeta.Verify` end to end.

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 (xnu-12377.121.6, arm64)
- Go: go1.25.8 darwin/arm64
- apple/container CLI: version 1.0.0 (release)
- Image: `docker.io/library/alpine:3.20`

## Gating

The test is skipped unless `MACVZ_INTEGRATION=1`, so the default
`go test ./...` stays hermetic and never requires apple/container.

Run command (and this report's evidence):

```sh
MACVZ_INTEGRATION=1 go test ./pkg/runtime/container/ \
    -run TestHandoffLaunchIntegration -v
```

## Flow

The test reuses the production CRI-I1/I2 helpers rather than hand-rolled logic:

1. `runtime.HandoffManager` (#109, rooted at a test-owned temp dir so the run
   needs no root) creates the production per-container layout:
   `<root>/macvz-handoff-it/{rootfs,handoff}` with the `0777` handoff directory
   mode from CRI-I1.
2. `runtime.InjectHandoffMount` (#112) appends the handoff bind mount to the
   workload spec: the handoff directory bind-mounted writable at the production
   `runtime.HandoffMountPoint` (`/run/macvz/handoff`). The apple/container driver
   maps that to a writable VirtioFS `--volume source:target`.
3. The launched alpine process writes the canonical `runtime.FormatIdentity`
   line (#113) plus an `expected=` self-report and a `proc_root=/` diagnostic to
   `/run/macvz/handoff/identity`, then `sync`s it.
4. The host reads the evidence back through the VirtioFS share and verifies
   identity with `runtime.VerifyHandoffIdentity` (#113): exact-match against
   `HandoffMeta.ExpectedIdentity` (no substring matching, per CRI-R16), with
   `expected`/`proc_root` carried only as diagnostics.

## Result

Outcome: `runtimeHandoffLaunchSucceeded`.

```text
handoff identity verified: observed="macvz-handoff-id=late-alpha" expected="macvz-handoff-id=late-alpha" diagnostics=[expected proc_root]
[outcome=runtimeHandoffLaunchSucceeded] handoff=<tmp>/macvz-handoff-it/handoff mount=/run/macvz/handoff status=Verified
--- PASS: TestHandoffLaunchIntegration (2.81s)
```

The workload booted, wrote its rootfs identity into the bind-mounted handoff
path, and the runtime read the same payload back from the host-visible handoff
directory and confirmed it matched the expected start invariant.

Verified payload (R16 format):

```text
identity=macvz-handoff-id=late-alpha
expected=macvz-handoff-id=late-alpha
proc_root=/
```

## Precise Failure Outcomes

Every failure stage logs a distinct outcome so an operator report records exactly
where the handoff broke rather than a generic timeout:

- `runtimeHandoffServiceUnavailable` — apple/container service not Ready.
- `runtimeHandoffImagePullFailed` — image pull failed.
- `runtimeHandoffPrepareFailed` — `HandoffManager.Create` could not stage the layout.
- `runtimeHandoffCreateFailed` / `runtimeHandoffStartFailed` — workload create/start failed.
- `runtimeHandoffProcessFailed` — workload failed or exited non-zero before writing evidence.
- `runtimeHandoffEvidenceMissing` — no identity evidence reached the host within the timeout.
- `runtimeHandoffEvidenceMalformed` — payload had no `identity=` line.
- `runtimeHandoffIdentityMismatch` — observed identity did not equal expected.

## Interpretation

The production Go handoff helper, bind-mount shape, identity-file format, and
verification work end to end against the shipped runtime backend. This closes the
gap between the R15 harness-only proof and the experimental CRI runtime path
without enabling the production CRI route by default and without running
kubelet/k3s in this loop.

## Non-goals (unchanged)

- Does not run kubelet/k3s in-loop.
- Does not enable the production CRI route by default.
- Does not use the vminitd late-rootfs primitive; that remains the gated Swift
  harness path.
