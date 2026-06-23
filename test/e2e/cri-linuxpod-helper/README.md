# CRI-R17 LinuxPod backend helper stub (#124)

Experimental MacVz CRI feasibility track (`develop`), issue #124. This is the
**Swift side** of the smallest LinuxPod late-rootfs runtime backend contract that
the Go `macvz-cri` adapter can call. It is **not** the shipped Virtual Kubelet
path and is **not** production-ready.

## What it is

`LinuxPodHelperStub` is a Foundation/Darwin-only Swift program that speaks the
[`pkg/runtime/linuxpod`](../../../pkg/runtime/linuxpod) NDJSON protocol over a unix
socket. It implements the minimal lifecycle the issue requires — `Ping`,
`CreatePod`, `PrepareContainerRootfs`, `CreateContainer`, `StartContainer`,
`StopContainer`, `RemoveContainer`, `Status`, `Cleanup` — with an **in-memory
model that mirrors the Go `FakeBackend`**:

- one Pod VM with a single shared network namespace (`sandboxNamespace`),
- late-binding container creation after the pod is already running (the late
  sidecar case),
- rootfs identity verification at start (CRI-R16 exact match), and
- a `Cleanup` that leaves no pod/container/rootfs state.

It boots **no real VM** (`Ping` reports `simulated=true`). A production helper
replaces the in-memory model with Apple Containerization LinuxPod calls and keeps
this exact wire protocol, so the Go adapter does not change.

## Why a stub

The contract is proven hermetically in Go (the `FakeBackend` and over-pipe tests
in `pkg/runtime/linuxpod`). This stub additionally proves the contract is
implementable in Swift with no gRPC, no code generation, and no dependency on
Apple Containerization — and that the Go `HelperClient` drives it across a real
unix socket. It is the seam a real LinuxPod helper grows from.

## Run

```sh
# Build the stub and run the Go<->Swift contract test (the R17 probe):
./run.sh

# Or serve manually for ad-hoc probing, then point the adapter at it:
./run.sh --serve /tmp/macvz-linuxpod-helper.sock
macvz-cri --experimental-linuxpod-backend --linuxpod-helper-socket /tmp/macvz-linuxpod-helper.sock
```

The contract test is also runnable directly and is gated so the default
`go test ./...` stays hermetic:

```sh
MACVZ_LINUXPOD_HELPER=1 \
MACVZ_LINUXPOD_HELPER_BIN="$PWD/.build/debug/LinuxPodHelperStub" \
  go test ../../../pkg/runtime/linuxpod/ -run TestSwiftHelperStubContract -v
```

## Relationship to the other LinuxPod harness

`../cri-linuxpod` is the **PoC probe harness** (C1/C2/C4/R1–R9) that proved the
LinuxPod primitives against real Apple Containerization. This directory turns
those results into a **callable backend contract** with a Swift helper the Go
adapter drives. See `docs/CRI_RUNTIME_R17_LINUXPOD_BACKEND_REPORT.md`.
