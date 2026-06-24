# CRI-L4 Follow-up: Real Apple Containerization logs/exec/stats (#133)

Date: 2026-06-24
Parent: #125 · Follow-up from #129 (CRI-L4)
Outcome: **`linuxpodKubeletSurfacesMeasured`** — the real `linuxpod-helper` now
backs logs, exec, and stats with measured Pod-VM data; the `simulated` markers are
false on the real path and asserted by a gated live test on Apple Silicon.

## Summary

#129 defined the logs/exec/stats contract and implemented it in the Go
`FakeBackend` and the Swift `LinuxPodHelperStub` with simulated values; the real
`linuxpod-helper` (#126) advertised the surfaces `false` and returned
`Unsupported`. This change backs all three with real Apple Containerization
LinuxPod data and flips the helper's advertised capabilities to true, so the
`macvz-cri` LinuxPod serving path (which already routes these RPCs to the backend
by capability) now serves measured kubelet surfaces.

The #133-specific work is in the real helper
(`test/e2e/cri-linuxpod/Sources/LinuxPodHelper/`) plus the gated live test; the
simulated stub still returns simulated logs/exec/stats for hermetic CI.

## What changed (real helper, `Backend.swift`)

- **Stats** — `containerStats` samples real cgroup data through the public
  `Vminitd.containerStatistics(containerIDs:categories:.all)`. Memory working-set
  is the point-in-time `memory.usageBytes`; `CPUUsageNanoCores` (the kubelet's
  `UsageNanoCores`, a rate) is derived honestly from two `cpu.usageUsec` samples a
  100 ms window apart. The sample is flagged `simulated=false`.
- **Exec** — `execSync` runs the command inside the running container's namespaces
  via the low-level vminitd exec path (`createProcess` with a fresh process id but
  the container's id, full OCI runtime spec like the container's), capturing real
  stdout/stderr over host vsock ports (the helper's existing
  `Transport.captureVsockStream`) and the real exit code from `waitProcess`.
- **Logs** — when CreateContainer supplies a log path, the container's stdout and
  stderr are wired to host vsock ports at `createProcess` time and streamed by
  detached tasks into the CRI log file as
  `"<rfc3339nano> <stdout|stderr> F <message>"` lines for the container's lifetime
  (tasks cancelled on remove; the process exit drains them). `ContainerLogPath`
  returns the path with `format=cri`.
- Capabilities in `Ping` flipped to `["logs": true, "exec": true, "stats": true]`;
  `simulated` stays `false`. Helper NDJSON `protocolVersion` aligned to the Go
  contract's `ProtocolVersion = 5`.

## Live evidence (gated `TestRealLinuxPodHelperLifecycle`, Apple Silicon)

Environment: arm64, macOS 26.5.1, Swift 6.3 (swiftly; `.build` cache toolchain),
Apple Containerization checkout 0.34.0, Go 1.25.8. Run with
`MACVZ_LINUXPOD_REAL_HELPER=1` against a real LinuxPod micro-VM. The helper binary
is codesigned with the `com.apple.security.virtualization` entitlement.

```text
--- PASS: TestRealLinuxPodHelperLifecycle (5.11s)
LIVE EVIDENCE: app id=pod-l1/app-2 phase=Running identityVerified=true ... (shared net:[4026531840])
LIVE EVIDENCE: exec app `echo macvz-exec-ok` -> exit=0 stdout="macvz-exec-ok"
LIVE EVIDENCE: stats app cpuNanoCores=0 memWorkingSetBytes=212992 simulated=false
LIVE EVIDENCE: logs container stdout -> .../logger.cri.log : "2026-06-24T04:38:03.173Z stdout F macvz-log-marker"
```

- **exec**: a real command runs in the container and returns measured stdout +
  exit code; a `exit 7` command is reported faithfully (exit-code passthrough).
- **stats**: `simulated=false`, non-zero measured cgroup memory working set
  (~212 KiB for an idle busybox; CPU rate ~0 over the idle window).
- **logs**: a container created with a log path streams its real stdout into a
  CRI-format log file the test reads back (`stdout F macvz-log-marker`).

## Acceptance criteria

1. **Logs/exec/stats reflect real workload behavior on Apple Silicon.** ✅ Proven
   live (above): real exec stdout/exit, real cgroup stats, real streamed stdout.
2. **`simulated` flags false on the real path, asserted by a gated live test.** ✅
   `containerStats.simulated=false` and `Ping` `simulated=false` with all three
   capabilities true; `TestRealLinuxPodHelperLifecycle` asserts measured exec
   output/exit, `Simulated=false` + non-zero memory, and real CRI log content.

## Validation

- `MACVZ_LINUXPOD_REAL_HELPER=1 … TestRealLinuxPodHelperLifecycle` — **PASS**.
- `swift build --product linuxpod-helper` green; `go build ./...`, `go vet ./...`,
  hermetic `go test ./...` green.

## Non-goals (honored)

- Interactive/streaming Exec (#132), Attach/PortForward (#131) are separate; this
  covers non-interactive logs/exec/stats only.
- The simulated stub keeps logs/exec/stats `simulated=true` for hermetic CI.
- The shipped apple/container CRI path and Virtual Kubelet runtime are untouched.

## Notes / limitations

- The log-capture tasks use blocking `availableData` reads on detached tasks (one
  pair per container with a log path); fine at the prototype's container counts.
  A production helper may prefer a readability-handler/event-loop reader.
- CPU `UsageNanoCores` is a short-window (100 ms) rate; kubelet summary smoothing
  applies on top.
