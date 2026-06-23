# CRI-R17 LinuxPod Late-Rootfs Runtime Backend Prototype (#124)

Date: 2026-06-23

Outcome: `linuxpodBackendContractPrototyped`

## Purpose

Turn the LinuxPod multi-container sandbox primitive (CRI LinuxPod C1/C2/C4,
[CRI_LINUXPOD_FEASIBILITY.md](CRI_LINUXPOD_FEASIBILITY.md)) and the R15/R16
late-rootfs handoff identity primitive
([CRI_RUNTIME_R16_HANDOFF_DESIGN.md](CRI_RUNTIME_R16_HANDOFF_DESIGN.md)) from
PoC/harness evidence into the **smallest LinuxPod runtime backend contract** that
the Go `macvz-cri` adapter can call, and prove the kubelet-ordering sequence:

```text
RunPodSandbox -> Create/Start app -> late Create/Start sidecar -> shared namespace -> identityVerified -> cleanup
```

This is a prototype. It does **not** replace the shipped `apple/container` CLI
backend, does **not** run k3s in-loop, and makes **no** production-readiness
claim.

## What shipped

### 1. The backend contract (`pkg/runtime/linuxpod`)

`Backend` is the minimal, pod-centric lifecycle a Go CRI adapter calls
([contract.go](../pkg/runtime/linuxpod/contract.go)):

| Method | Role |
| --- | --- |
| `Ping` | Startup handshake / capability check. |
| `CreatePod` | Boot the LinuxPod sandbox VM (RunPodSandbox analog); returns the shared `SandboxNamespace`. |
| `PrepareContainerRootfs` | The late-rootfs primitive (R8/R16): stage a prepared rootfs + expected identity into a running Pod VM; returns a token. |
| `CreateContainer` | Late-bind a container onto a prepared rootfs — succeeds even after the pod has running containers. |
| `StartContainer` | Start; gate Running on rootfs identity verification (exact match, CRI-R16). |
| `StopContainer` | Stop, preserving record until removal. |
| `RemoveContainer` | Idempotent removal of container + rootfs. |
| `Status` | Container status incl. shared-namespace / late-binding / identity evidence. |
| `Cleanup` | Tear down the Pod VM and all artifacts; idempotent; leaves no stale state. |

Classified sentinel errors (`ErrPodNotFound`, `ErrContainerNotFound`,
`ErrRootfsNotFound`, `ErrInvalid`, `ErrIdentityUnverified`) round-trip over the
wire so callers branch with `errors.Is`.

### 2. Wire protocol + client + server

A newline-delimited JSON (NDJSON) protocol ([protocol.go](../pkg/runtime/linuxpod/protocol.go))
chosen over gRPC so the helper stays a few hundred lines of Foundation Swift with
no code generation while remaining trivially fakeable in Go:

- `HelperClient` ([client.go](../pkg/runtime/linuxpod/client.go)) implements
  `Backend` by framing each call over a `Dialer` (a unix socket in production, an
  in-memory pipe in tests). Connection-per-call keeps framing trivial and the
  client concurrency-safe.
- `Serve` ([server.go](../pkg/runtime/linuxpod/server.go)) is the helper-side
  dispatcher; the Swift helper mirrors it.
- `FakeBackend` ([fake.go](../pkg/runtime/linuxpod/fake.go)) is the in-process
  lifecycle model. Identity verification reuses the production exact-match logic
  (`runtime.HandoffMeta.Verify`) so the fake never diverges from how the real
  adapter decides verification.

### 3. Swift helper stub (`test/e2e/cri-linuxpod-helper`)

`LinuxPodHelperStub` is a Foundation/Darwin-only Swift program that serves the
same NDJSON protocol over a unix socket with an in-memory model mirroring
`FakeBackend`. It boots no VM (`Ping` → `simulated=true`); a production helper
swaps the model for Apple Containerization LinuxPod calls and keeps the wire
protocol unchanged. Build/run via `test/e2e/cri-linuxpod-helper/run.sh`.

### 4. Adapter gate (`macvz-cri`)

`--experimental-linuxpod-backend` + `--linuxpod-helper-socket`
([linuxpod.go](../cmd/macvz-cri/linuxpod.go)): off by default. When enabled the
adapter connects to the helper and performs a startup `Ping` handshake, failing
**loudly** if the helper is unreachable or the socket flag is missing, so an
operator learns immediately rather than mid-Pod. The shipped apple/container CRI
serving path is unchanged — full CRI serving onto the LinuxPod backend is the
explicit next step, out of scope for this prototype.

## Evidence

### Hermetic (default `go test`)

`pkg/runtime/linuxpod` ([linuxpod_test.go](../pkg/runtime/linuxpod/linuxpod_test.go)):

- `TestFakeBackendOrderingAndEvidence` and `TestHelperClientOrderingOverPipe` run
  the exact required ordering (CreatePod → app create/start → **late** sidecar
  create/start after the app is running) directly against the fake and over the
  NDJSON wire, asserting AC5 evidence: app and sidecar share one
  `SandboxNamespace`, both report `LocalhostReachable`, and the sidecar's rootfs
  identity handoff verifies (`IdentityVerified=true`, observed == expected).
  `CreatedAfterPodRunning` proves the late-binding ordering.
- `TestStopRemoveOrderingNoStaleState` proves both stop orderings (sidecar-first,
  app-first), idempotent remove, and a `Cleanup` that removes the pod + rootfs and
  reports `StaleState=false`, with `Status` returning `ErrPodNotFound` afterward
  (AC6).
- `TestStartContainerIdentityMismatch` proves a wrong reported identity fails
  `StartContainer` with `ErrIdentityUnverified` and leaves the container
  non-Running (CRI-R16 invariant).
- `TestBackendErrorsRoundTripOverWire` proves classified errors survive the wire.

`cmd/macvz-cri` ([linuxpod_test.go](../cmd/macvz-cri/linuxpod_test.go)): gate
validation (disabled no-op; enabled-without-socket fails naming the flag;
unreachable socket fails loudly) and a real-unix-socket handshake against
`Serve(FakeBackend)`.

`go build ./...`, `go vet ./...`, `go test ./...` all green.

### Gated live Go↔Swift contract

`TestSwiftHelperStubContract` (gated `MACVZ_LINUXPOD_HELPER=1`) launches the Swift
stub and runs the full ordering probe through `HelperClient` over a real unix
socket — the same assertions, now across the language boundary. Verified locally:

```text
linuxpod-helper-stub listening on .../h.sock
--- PASS: TestSwiftHelperStubContract (0.37s)
```

And the real adapter binary handshakes the stub over a socket:

```text
"experimental LinuxPod backend handshake succeeded (prototype; CRI serving stays on apple/container)"
  helper="linuxpod-helper-stub" protocolVersion=1 simulated=true socket=".../h.sock"
```

## Non-goals (honored)

- No production-readiness claim; the backend is gated and `Ping` reports simulated.
- No k3s in-loop; no Service/DNS/Pod-IP cross-node networking; no Attach/PortForward.
- The shipped Virtual Kubelet path and the `apple/container` CLI backend are
  unchanged; the LinuxPod backend is opt-in only.

## Next steps

- Replace the Swift stub's in-memory model with real Apple Containerization
  LinuxPod calls (pod.create / addContainer / late-rootfs staging from R9),
  keeping the NDJSON contract.
- Wire `macvz-cri` CRI serving (RunPodSandbox/CreateContainer/…) onto the LinuxPod
  backend behind the gate, mapping CRI sandboxes/containers to pods/containers.
