# CRI-L1 Real Apple Containerization LinuxPod Helper (#126)

Date: 2026-06-23
Milestone: CRI-L (LinuxPod runtime)
Outcome: `realLinuxPodHelperLifecycleProven`

## Purpose

Replace the CRI-R17 Swift helper **stub's** in-memory lifecycle model with a real
Apple Containerization LinuxPod implementation, while keeping the Go
`pkg/runtime/linuxpod` NDJSON protocol contract stable. The Go adapter, the
protocol (v3), and the hermetic Go fake/stub are unchanged; only the model behind
the socket is now real — it boots actual micro-VMs and drives the R9/R16
late-rootfs identity primitive (`Ping` reports `simulated:false`).

## What was added

- **Real helper target** `linuxpod-helper` in the existing `test/e2e/cri-linuxpod`
  Swift package (which already wires the Apple Containerization framework). Source
  under `test/e2e/cri-linuxpod/Sources/LinuxPodHelper/`:
  - `Entry.swift` — `@main` ArgumentParser command: bootstraps the VM manager +
    image store and runs the server.
  - `Server.swift` — async unix-socket NDJSON server (one JSON request/response per
    line) that bridges blocking accept/read/write to async and routes each line to
    the actor.
  - `Primitives.swift` — ported R9 transport: VM/image-store bootstrap, the vminitd
    `Copy(COPY_IN/COPY_OUT)` host↔guest primitive, vsock capture, and a CRI-format
    log writer.
  - `Backend.swift` — the `LinuxPodBackend` actor implementing all 13 protocol ops
    over a real `LinuxPod` + the late-rootfs primitive, with identity-gated Running.
- **Stub retained** (`test/e2e/cri-linuxpod-helper`, `LinuxPodHelperStub`) for
  hermetic Go tests — it stays the dependency-free in-memory model.
- **Gated live test** `TestRealLinuxPodHelperLifecycle`
  (`pkg/runtime/linuxpod/real_helper_integration_test.go`, `make cri-linuxpod-helper-real`).
- No Go contract change: `ProtocolVersion` stays `3`; the real helper speaks the
  same wire protocol as the stub and the fake.

## Lifecycle mapping (LinuxPod + R9/R16)

The model: one `LinuxPod` VM per pod, kept up by a long-lived **holder** container.
Each late container is a **vminitd process launched from a rootfs staged into the
already-running VM** via the R9 Copy primitive — the late-rootfs primitive proven
live in CRI-R9/R15/R16. Identity is verified **host-side** through a bind-mounted
handoff evidence channel (CRI-R16), never by the guest trusting itself.

| Op | Real implementation |
|----|---------------------|
| `Ping` | `{name: linuxpod-helper, protocolVersion: 3, simulated: false, capabilities:{logs/exec/stats:false}}` |
| `CreatePod` | unpack a holder rootfs; `LinuxPod(...).create()`; `startContainer(holder)` so the VM + its shared namespace stay up |
| `PodStatus` | report the Pod VM phase + shared namespace (+ `sandboxAddress` reserved for CRI-L3) |
| `PrepareContainerRootfs` | stage a minimal busybox rootfs into the running VM (copy busybox/lib out of the holder, stage `/etc/macvz-container-identity` = ExpectedIdentity, Copy the prepared rootfs + a writable evidence dir into the guest) — the R9 late-rootfs primitive |
| `CreateContainer` | `vminitd.createProcess` bound to the staged rootfs; `createdAfterPodRunning` is true when another app/sidecar is already Running (the late-sidecar case) — does **not** start |
| `StartContainer` | `vminitd.startProcess`; bounded-wait reads the identity evidence the process wrote to the handoff channel back to the host and compares to ExpectedIdentity. Match → Running + `identityVerified`; mismatch/timeout → delete the process, return `ErrIdentityUnverified`, never Running |
| `StopContainer` | delete the workload process, keep the staged rootfs/evidence for post-mortem until Remove |
| `RemoveContainer` | delete the process + staged rootfs/evidence; idempotent |
| `Status` | report the container's stored state + identity/namespace evidence |
| `Cleanup` | delete every container process + rootfs, stop the holder + `pod.stop()` (tear down the VM), remove host artifacts; idempotent, reports no stale state |
| `ContainerLogPath` / `ExecSync` / `ContainerStats` | the kubelet surfaces are owned by CRI-L4 (#129); the real helper advertises them **false** and returns `ErrUnsupported` rather than faking results |

### Identity handoff (CRI-R16, host-side verification)

`PrepareContainerRootfs` stages the ExpectedIdentity into the prepared rootfs at
`/etc/macvz-container-identity`. The container's process, before exec'ing the real
workload, writes `identity=<staged>` and `netns=<inode>` into the bind-mounted
handoff dir and `sync`s. `StartContainer` reads that evidence back to the host and
verifies `observed == expected`. This is the same exact-match identity contract the
Go adapter (`pkg/runtime`) and the fake use, so the real helper never diverges from
the stub's semantics.

### Shared-namespace evidence (real, not modeled)

Late containers share the Pod VM's network namespace (their OCI specs declare no
net namespace), so they reach each other over `127.0.0.1` — the same property the
C1/C2 PoC proved live. The evidence script emits the process's real
`/proc/self/ns/net` inode; the helper records it as the pod's `SandboxNamespace`, so
the app and sidecar report the **same real netns inode** — genuine shared-namespace
evidence, not a derived string.

## Failure modes → wire codes (AC6)

All failures classify through the protocol error codes so `errors.Is` works on the
Go side: `Invalid` (missing field, duplicate, wrong phase), `PodNotFound`,
`ContainerNotFound`, `RootfsNotFound`, `IdentityUnverified` (mismatch/late
evidence), `Unsupported` (kubelet surfaces, #129), `Internal` (VM/transport fault).

## Tests

- Hermetic (unchanged, still green): `pkg/runtime/linuxpod` fake + over-pipe +
  error-round-trip tests, and `cmd/macvz-cri` gate tests. `go test ./pkg/runtime/linuxpod ./cmd/macvz-cri` passes.
- Gated stub contract: `TestSwiftHelperStubContract` (`MACVZ_LINUXPOD_HELPER=1`).
- **Gated real lifecycle**: `TestRealLinuxPodHelperLifecycle`
  (`MACVZ_LINUXPOD_REAL_HELPER=1`) runs the exact kubelet ordering against real VMs:
  CreatePod → app prepare/create/start → **late** sidecar prepare/create (after the
  app is running) /start → assert shared namespace + localhost + identity verified →
  Cleanup leaves no residual (and is idempotent). It also asserts the kubelet
  surfaces return `ErrUnsupported`.

## Build / run

Operator-provisioned dependencies (heavy, externally fetched — the Containerization
track's gated boundary):

```sh
cd test/e2e/cri-linuxpod
git clone https://github.com/apple/containerization containerization   # pinned 6b7b42c
# Apply the R10/R11/R12 vmexec late-rootfs fixes (ptmx + /dev/null + diagnostics);
# late-rootfs launch requires them (upstream proposal #122, §"required changes").
git -C containerization apply ../../../docs/CRI_RUNTIME_R10_APPLE_CONTAINERIZATION_DEBUG.patch
git -C containerization apply ../../../docs/CRI_RUNTIME_R11_APPLE_CONTAINERIZATION_PTMX.patch
# R12 (ensureDevNull): apply by hand — call ensureDevNull() in childRootSetup before
# setDevSymlinks() and add the function (see the patch; its hunk header is malformed).
make -C containerization fetch-default-kernel                           # bin/vmlinux-arm64
make -C containerization init                                           # builds patched vminit:latest into the local image store
rm -f "$HOME/Library/Application Support/com.apple.containerization/initfs.ext4"  # bust any stale unpatched initfs
cd ../../.. && make cri-linuxpod-helper-real
```

## Verification status (this change)

- `swift build --product linuxpod-helper` — **green** (compiles against the real
  Apple Containerization framework, pinned `6b7b42c`, Swift 6.2.1, macOS 26 SDK).
- `swift build` of the stub — **green** (stub retained, dependency-free).
- `go build ./...`, `go vet ./...`, `go test ./pkg/runtime/linuxpod ./cmd/macvz-cri` — **green** (hermetic contract unchanged).
- Binary smoke: the helper runs, parses args, and fails honestly on a missing
  kernel before serving.
- Kernel fetched (`bin/vmlinux-arm64`).
- **Live VM run (`TestRealLinuxPodHelperLifecycle`): PASS** on real micro-VMs — see "Live evidence" below.

## Live evidence

Run on real micro-VMs (Apple Silicon, macOS 26, Swift 6.2.1, Apple
Containerization `6b7b42c` + R10/R11/R12 vmexec patches, kernel `vmlinux-arm64`,
busybox-staged rootfs). `TestRealLinuxPodHelperLifecycle` **PASS** (~15–29s):

```
LIVE EVIDENCE: pod=pod-l1 sandboxNamespace="net:[4026531840]" (shared by app+sidecar)
LIVE EVIDENCE: app     id=pod-l1/app-2     phase=Running identityVerified=true observed="macvz-rootfs-id=app"     createdAfterPodRunning=false localhostReachable=true
LIVE EVIDENCE: sidecar id=pod-l1/sidecar-4 phase=Running identityVerified=true observed="macvz-rootfs-id=sidecar" createdAfterPodRunning=true  localhostReachable=true
--- PASS: TestRealLinuxPodHelperLifecycle
```

What this proves, live:

- **AC3 — late-rootfs ordering**: CreatePod booted a real LinuxPod VM; the app was
  prepared/created/started; the sidecar was prepared and created **after the app
  was already Running** (`createdAfterPodRunning=true`) and started — the kubelet
  ordering the apple/container CLI path cannot model.
- **AC4 — shared namespace + identity**: app and sidecar report the **same real net
  namespace inode** (`net:[4026531840]`, read from `/proc/self/ns/net` inside each
  container), and each container's handoff identity verified
  (`observed == expected`) before it was reported Running (CRI-R16 gate).
- **AC5 — cleanup**: the deferred Cleanup returned `podRemoved=true staleState=false`
  and a second Cleanup of the now-unknown pod was an idempotent no-op — no residual
  VM/container/rootfs/handoff state.
- **AC6 — failure classification**: the kubelet surfaces returned `ErrUnsupported`
  over the wire (verified in the same run).

The first attempts surfaced the expected provisioning gates and were fixed, not
papered over: missing VM entitlement → codesign with
`com.apple.security.virtualization`; vmexec `ENOENT` launching the staged rootfs →
the R11/R12 vmexec fixes (ptmx + `/dev/null`); a stale unpatched `initfs.ext4`
cached in the default store → busted before the passing run.

## Non-goals (honored)

- No kubelet CRI serving wiring (CRI-L2).
- No Pod IP routing/Services (CRI-L3; `SandboxAddress` is reserved but unset here).
- No change to the shipped Virtual Kubelet or apple/container CLI path.
- Kubelet log/exec/stats surfaces left to CRI-L4 (#129); advertised false, honest
  `Unsupported`.
