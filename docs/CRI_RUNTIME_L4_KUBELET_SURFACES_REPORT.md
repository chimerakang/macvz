# CRI-L4 LinuxPod Kubelet Surfaces: Logs, Exec, Stats (#129)

Date: 2026-06-23

Outcome: `linuxpodKubeletSurfacesDefined`

Parent: #125. Builds on the R17 backend contract
([CRI_RUNTIME_R17_LINUXPOD_BACKEND_REPORT.md](CRI_RUNTIME_R17_LINUXPOD_BACKEND_REPORT.md)).

## Purpose

Give the LinuxPod-backed CRI path the kubelet-facing runtime surfaces needed for
normal operation and debugging — **logs, exec, stats** — with **honest
unsupported behavior** for anything not yet implemented, and **capability
negotiation** so the adapter learns what a helper backs before it calls. This
does not touch the shipped `apple/container` backend and makes no
production-readiness claim.

## What shipped

All in the experimental contract package `pkg/runtime/linuxpod`, kept in lock-step
between the Go `FakeBackend`, the `HelperClient`/`Serve` NDJSON transport, and the
Swift `LinuxPodHelperStub`. Protocol version is now `3` (v2 added the kubelet
surfaces; v3 added PodStatus/SandboxAddress for CRI-L3).

### 1. Capability negotiation (AC4)

`HelperInfo.Capabilities{Logs, Exec, Stats}` is returned from `Ping`, so the
startup handshake reports which surfaces the helper backs. `cmd/macvz-cri` logs
them (`capLogs`/`capExec`/`capStats`). Calling an unadvertised surface returns the
new `ErrUnsupported` sentinel (wire code `Unsupported`) rather than a generic
failure — and never mutates lifecycle state, so a surface failure cannot wedge a
Pod.

`LinuxPodService` maps `ErrUnsupported` to CRI `Unimplemented`, so kubelet sees a
clear unsupported surface rather than an Internal runtime failure.

### 2. Logs (AC1)

`CreateRequest.LogPath` carries the kubelet-assigned CRI log path. When set (and
`Capabilities.Logs`), the backend creates the file and appends **CRI-format**
lines (`<rfc3339nano> <stream> <P|F> <message>`) across create/start.
`ContainerLogPath(ref)` reports the path + `format:"cri"`. A container created
without a log path is an honest `ErrInvalid`. Log-write failures are best-effort
and ignored so they cannot wedge create/start (test:
`TestLogFailureDoesNotWedgeLifecycle`).

`LinuxPodService.CreateContainer` passes the absolute CRI log path
(`sandbox.log_directory` + `container.log_path`) to the backend, and
`ContainerStatus` reports that same absolute path for kubelet/crictl.

### 3. Exec (AC2)

`ExecSync(ExecRequest) (ExecResult, error)` runs a command to completion — the
primitive kubelet uses for exec liveness/readiness probes and non-interactive
`kubectl exec`. The fake/stub echo the command (simulated, no real VM). Exec on a
non-running container or with an empty command is `ErrInvalid`.

Interactive/streaming exec (`kubectl exec -it`) landed as the `ExecStream`
**negotiation surface** (#132), a peer of the Attach/PortForward interactive
surfaces below: capability-gated by `Capabilities.ExecStream` (separate from
`Capabilities.Exec`, which is ExecSync), it validates the target and returns an
`ExecStreamResponse` describing the session it would open — which streams attach,
whether a TTY was granted (a TTY folds stderr into stdout) — flagged
`Simulated:true`. The actual bidirectional VM-internal byte plumbing has nothing to
attach to in a stub/fake and remains a future production streaming transport.

### 4. Stats (AC3)

`ContainerStats(ref) (ContainerStats, error)` returns a timestamped per-container
sample for kubelet summaries, flagged `Simulated:true` so modeled numbers are
never read as measured. Real cgroup-backed stats are tracked in **#133**.

`LinuxPodService` implements the CRI stats RPCs (`ContainerStats`,
`ListContainerStats`, `PodSandboxStats`, `ListPodSandboxStats`) by asking the
backend for running LinuxPod containers. Unsupported stats degrade to
attributes-only stats, matching the existing apple/container behavior for
unobservable samples.

## Deferred (follow-ups)

- **#131** — Attach / PortForward negotiation surfaces (landed, simulated;
  real byte plumbing is the documented non-goal).
- **#132** — interactive/streaming Exec (`ExecStream`) negotiation surface
  (landed, simulated; real bidirectional byte plumbing remains a future transport
  concern).
- **#133** — back logs/exec/stats with real Apple Containerization data and drop
  the `simulated` flags once measured.

## Validation

- `go test ./pkg/criserver ./pkg/runtime/linuxpod ./cmd/macvz-cri`, `go test ./...`,
  `go vet ./...`, `make build` — all green.
- Hermetic surface tests (`pkg/runtime/linuxpod/surfaces_test.go`) cover every
  surface on both the supported and `ErrUnsupported` paths, in-process and over the
  NDJSON pipe.
- CRI service regression tests (`pkg/criserver/linuxpod_service_test.go`) prove
  absolute log-path propagation, `ReopenContainerLog`, `ExecSync`, CRI stats
  RPCs, and honest `Unimplemented` for unsupported/streaming surfaces.
- Gated Go↔Swift parity (`MACVZ_LINUXPOD_HELPER=1`) exercises capabilities, exec,
  and stats over a real unix socket against the Swift stub.
