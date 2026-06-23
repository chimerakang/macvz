# CRI-L3 Pod Networking for LinuxPod-backed CRI Sandboxes (#128)

Date: 2026-06-23

Outcome: `linuxpodPodNetworkingIntegratedHermetic` — code path and hermetic
coverage are complete; live host-to-Pod/Pod-to-host reachability remains
operator-pending.

Parent: [#125](https://github.com/chimerakang/macvz/issues/125) ·
Depends on: [#127](https://github.com/chimerakang/macvz/issues/127) (LinuxPod
lifecycle serving)

## Purpose

Attach MacVz Pod networking to LinuxPod-backed CRI sandboxes
([linuxpod_service.go](../pkg/criserver/linuxpod_service.go), #127) so kubelet
sees a Pod IP and `NetworkReady`, while preserving the existing host default route
and operator-managed routes. It reuses the shipped Pod networking primitives
rather than inventing a CRI-only path: Pod IPs come from `network.PodIPAM` (the
node's Kubernetes-assigned Pod CIDR) and the host packet-filter/route path comes
from `podnet.Router` (pf binat per micro-VM).

This stays within the experimental LinuxPod track: it does **not** change the Mac
host default route, does **not** rewrite the shipped Virtual Kubelet / apple
container networking, and makes **no** production-readiness claim.

## The timing difference vs. the apple/container path

The apple/container path (one micro-VM per single-container Pod) can only learn
its VM address after the container starts, so it attaches the host path in
`StartContainer` ([network.go](../pkg/criserver/network.go), CRI-P5/#77). A
LinuxPod sandbox is a **Pod VM that boots at `RunPodSandbox` time**, so its
address is available before any container starts. CRI-L3 therefore attaches the
host path at `RunPodSandbox`, keyed by Pod identity.

Two things are new; everything else is reused:

1. **Address discovery.** The Pod VM's host-reachable address is discovered from
   the LinuxPod backend, not from the container runtime. The contract gains a
   `PodStatus(podID)` query and a `PodStatus.SandboxAddress` field
   ([contract.go](../pkg/runtime/linuxpod/contract.go)); the integration polls it
   until the address appears (the VM acquires it shortly after boot).
2. **Failure diagnostics.** Attach distinguishes the four failure classes the
   issue requires via `LinuxPodNetworkError`
   ([linuxpod_network.go](../pkg/criserver/linuxpod_network.go)).

## What shipped

### 1. Contract: LinuxPod sandbox address discovery (`pkg/runtime/linuxpod`)

- `PodStatus.SandboxAddress` — the Pod VM's host-reachable (vmnet) address; `""`
  until acquired (a transient "not discovered yet" condition, not a failure).
- `Backend.PodStatus(ctx, podID) (PodStatus, error)` — the pod-level analog of
  `Status`, polled for address discovery and re-queried on recovery.
- Wired end-to-end: NDJSON `opPodStatus` ([protocol.go](../pkg/runtime/linuxpod/protocol.go)),
  helper-side dispatch ([server.go](../pkg/runtime/linuxpod/server.go)),
  `HelperClient.PodStatus` ([client.go](../pkg/runtime/linuxpod/client.go)),
  and `FakeBackend` ([fake.go](../pkg/runtime/linuxpod/fake.go)) with a
  controllable address + `SandboxAddressReadyAfter` latency model. `ProtocolVersion`
  bumped 2 → 3; the Swift helper stub
  ([main.swift](../test/e2e/cri-linuxpod-helper/Sources/LinuxPodHelperStub/main.swift))
  mirrors the op and field.
- The real Swift `linuxpod-helper`
  ([test/e2e/cri-linuxpod/Sources/LinuxPodHelper](../test/e2e/cri-linuxpod/Sources/LinuxPodHelper))
  now supports an explicit `--vmnet` flag. When enabled, `CreatePod` allocates one
  `VmnetNetwork` interface for the Pod, attaches it to the `LinuxPod`
  configuration, reports the interface IPv4 as `sandboxAddress`, and releases the
  interface during `Cleanup` or create failure. The default remains off so ordinary
  helper runs do not allocate vmnet interfaces.

### 2. Integration: LinuxPod Pod networking ([linuxpod_network.go](../pkg/criserver/linuxpod_network.go))

`LinuxPodService` gained `PodNetwork` + `IPAM` (both required to enable the path;
either nil leaves it off and sandboxes run without a Pod IP). New behavior:

| Lifecycle point | Behavior |
| --- | --- |
| `Status` | `NetworkReady` true only when IPAM + host path are both wired. |
| `RunPodSandbox` | Reserve a Pod IP (keyed by `namespace/name`, stable across recreation) → discover the Pod VM address → program the host binat rule → persist the attachment. Idempotent on retry. |
| `PodSandboxStatus` | Reports the Pod IP (via the shared `toCRIStatus`) only after the attach is recorded — never a reserved-but-unattached address. |
| `StopPodSandbox` | Detach the host path; **retain** the IP reservation. |
| `RemovePodSandbox` | Detach (idempotent) + release the IP reservation. |
| `RecoverNetwork` | Re-reserve each persisted Pod IP and re-attach surviving sandboxes after an adapter restart, without leaking reservations or wiping other Pods' rules. |

`cmd/macvz-cri` builds the Pod network in `serveLinuxPod`
([linuxpod.go](../cmd/macvz-cri/linuxpod.go)) from the existing
`--pod-cidr`/`--pod-network-interface`/… flags and calls `RecoverNetwork` at
startup.

### 3. Failure diagnostics (4 classes)

`LinuxPodNetworkError{Class, Err}` implements `GRPCStatus()` so the right CRI code
surfaces automatically, and exposes `Class` for `errors.As`:

| Class | Cause | CRI code |
| --- | --- | --- |
| `helper` | LinuxPod backend/helper call failed (unreachable, unknown pod) | `Unavailable` |
| `ip-reservation` | Pod IP could not be reserved (IPAM exhausted / unwritable store) | `ResourceExhausted` |
| `address-discovery` | backend answered but the Pod VM address never appeared in the budget | `Unavailable` |
| `route-pf` | host route/pf programming failed | `Internal` |

## Default-route safety

CRI-L3 never touches the Mac host default route. The host path is `podnet.Router`,
which only ever removes apple/container's **scoped** (`-ifscope <bridge>`) vmnet
default route, never the global default or the broad `-interface` form. This is
verified explicitly by
`TestAttachDetachPreservesGlobalDefaultRoute`
([router_test.go](../pkg/network/podnet/router_test.go)) across a full
attach/detach cycle (acceptance criterion 6).

## Validation

Hermetic (no pf, no route, no Pod VM):

- `pkg/criserver` — `TestLinuxPodNetworkAttachOnRunPodSandbox` (Pod IP reserved,
  host path attached to the discovered sandbox address, `PodSandboxStatus`
  reports the IP, `NetworkReady` true), `TestLinuxPodNetworkDisabledRunsWithoutPodIP`,
  `TestLinuxPodNetworkAddressDiscoveryLatencyTolerated`,
  `TestLinuxPodNetworkFailureClasses` (all four classes + codes),
  `TestLinuxPodNetworkDetachAndReleaseOnStopRemove`,
  `TestLinuxPodNetworkRestartRecovery` (re-reserve same IP, re-attach, no leak).
- `pkg/runtime/linuxpod` — `TestPodStatusAddressDiscoveryOverWire`.
- `pkg/network/podnet` — `TestAttachDetachPreservesGlobalDefaultRoute`.

Commands: `go test ./pkg/criserver ./pkg/network/... ./pkg/runtime/...`,
`go test ./...`, `go vet ./...`, `make build` — all green.

Gated/live (operator-pending, same topology as the rest of the LinuxPod track;
keeps #128 open until captured):

- Host-to-Pod / Pod-to-host smoke and the cleanup audit (routes/pf/IPAM/helper
  state) require a real LinuxPod helper + the local `192.168.1.122` or two-host
  topology. The hermetic suite proves the control flow, IPAM lifecycle, status
  reporting, and default-route safety; live reachability is the remaining gate.
- For live `SandboxAddress` validation without changing the default helper mode:
  `MACVZ_LINUXPOD_REAL_HELPER=1 MACVZ_LINUXPOD_REAL_HELPER_VMNET=1 go test ./pkg/runtime/linuxpod -run TestRealLinuxPodHelperLifecycle -count=1`.
  The test starts `linuxpod-helper --vmnet` and fails if `CreatePod` reports an
  empty `sandboxAddress`.
- Captured on `test@192.168.1.122`: the same gated test passed against the real
  helper with `MACVZ_CONTAINERIZATION_ROOT` pointed at the checked-in
  `containerization/bin/initfs.ext4` fallback; `sandboxAddress` was `192.168.66.2`
  and route audit before/after stayed on global default `192.168.1.1` via `en0`.
- Captured on `test@192.168.1.122`: `TestLiveLinuxPodServingThroughHelper` passed
  with `MACVZ_LINUXPOD_PODNET=1`,
  `MACVZ_LINUXPOD_POD_CIDR=10.244.102.0/24`,
  `MACVZ_LINUXPOD_PODNET_IFACE=bridge100`,
  `MACVZ_LINUXPOD_PODNET_HELPER_SOCKET=/var/run/macvz-netd.sock`,
  ingress `en0`, and forwarding enabled. The live CRI path reported
  `NetworkReady=true`, assigned Pod IP `10.244.102.2`, attached
  `pod=default/pod podIP=10.244.102.2 vmIP=192.168.66.2 interface=bridge101`,
  detached on cleanup, stopped the `macvz/pods` anchor, and preserved the global
  default route (`192.168.1.1` via `en0`).

## Non-goals honored

- Mac host default route unchanged; existing provider networking unchanged.
- No full Services/DNS work (follow-ups remain under the parent #125 scope).
- No production-readiness claim; the LinuxPod backend stays behind
  `--experimental-linuxpod-backend`.
