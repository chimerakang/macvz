# CRI-L5 LinuxPod-Backed Kubelet/k3s In-Loop Validation Report (#130)

Date: 2026-06-23; updated 2026-06-24
Parent: #125 · Depends on: #127 (serving), #128 (Pod networking), #129 (logs/exec/stats)
Outcome: **`linuxpodKubeletInLoopPartiallyValidated`** — a real kubelet/kind
node now drives the genuine (`simulated=false`) LinuxPod backend to a Deployment
`Available` rollout with Pod IP, ClusterIP Service reachability, bounded short
soak, cleanup, and default-route-preservation evidence. Remaining failures are
the explicit unimplemented LinuxPod helper/kubelet surfaces: logs, exec,
port-forward, and the harness's sidecar shared-proof collection path.

## Summary

This phase adds the real kubelet/k3s **in-loop validation harness** for the
experimental LinuxPod-backed CRI path and then drove the path as far as the
current LinuxPod helper surfaces allow. Proven on real hardware: (1) the real Apple
Containerization LinuxPod helper runs an app + *late* sidecar to **Running** in a
shared namespace with verified identity; (2) `macvz-cri`'s CRI serving drives that
real backend to `CONTAINER_RUNNING` via `crictl` (the exact CRI API a kubelet
uses); (3) CRI-L3 live podnet attach now reports `NetworkReady=true`, assigns a
Pod IP, preserves the host default route on `test@192.168.1.122`, and serves HTTP
over the Pod IP from an external Mac route; (4) a
**real in-cluster kubelet** was repointed at the LinuxPod backend with #128
podnet enabled and drove the LinuxPod fixture to `rollout status` success,
Pod IP `10.244.102.2`, ClusterIP reachability from a Linux-node probe, short soak
pass, cleanup pass, and unchanged default route. The harness still **refuses to
pass** LinuxPod acceptances on a
`simulated=true` handshake (honesty gate below); discipline mirrors #119
`kubeletHandoffSmokeBlocked`.

## Environment (exact)

- Machine: Apple Silicon, `arm64`.
- macOS: 26.5.1 (build 25F80).
- Swift: Apple Swift 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1),
  target `arm64-apple-macosx26.0`.
- `apple/container` CLI: 1.0.0 (build: release).
- `apple/containerization` checkout: 0.34.0.
- Go: 1.25.8 darwin/arm64.
- Repo: commit `f73efda` plus **uncommitted** CRI-L2 serving work
  (`pkg/criserver/linuxpod_service.go` and siblings) authored in a concurrent
  session; evidence below was captured against that working-tree state.
- `crictl` present; `k3s`/`kubelet` **not installed** on this host.

## What was built (this phase)

- `test/e2e/cri-k3s/linuxpod-inloop.sh` — the LinuxPod sibling of
  `k3s-inloop.sh`. Drives a real k3s control plane against a macOS node running
  `macvz-cri --experimental-linuxpod-backend --linuxpod-helper-socket=…` and an
  app+late-sidecar Pod, with phases: preflight, route-before, deploy, scheduling,
  **backend-evidence (honesty gate)**, shared-ns, identity, podip, logs, exec,
  port-forward, service, restart-cri, restart-helper, soak, cleanup, route-after.
  `make cri-linuxpod-inloop`. Gated by `MACVZ_INTEGRATION=1` + `KUBECONFIG`
  (plan-only + exit 0 otherwise; CI/`bash -n` safe).
- `test/e2e/cri-k3s/fixtures/linuxpod-workload.yaml` — a two-container Pod (app +
  a native-sidecar that curls the app on `127.0.0.1` to prove shared-namespace
  localhost), ConfigMap/Secret, ClusterIP Service, with the #84 nodeSelector +
  taint toleration. This is the multi-container shape the LinuxPod backend models
  and the default apple/container path excludes (#82/#86).
- Fixed a Swift 6 strict-concurrency build regression in the gated contract
  helper (`test/e2e/cri-linuxpod-helper/.../main.swift`): a top-level
  `let capabilities` is `@MainActor`-isolated under Swift 6 and could not be read
  from the nonisolated logs/exec/stats methods added for CRI-L4 (#129). Moved it
  into a nonisolated `enum Capabilities { static let all: [String: Bool] }`. The
  gated `TestSwiftHelperStubContract` went from **build-failed** to **PASS**.

### The honesty gate (central #130 invariant)

The shipped CRI serving path runs on apple/container. The LinuxPod gate today
serves through a helper that, for the prototype, reports `simulated=true` and
boots no real Pod VM. So a Pod reaching Running on such a node is **not** evidence
of a LinuxPod-backed Pod. `linuxpod-inloop.sh` therefore requires an operator
backend-evidence hook (`MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD`) to prove on the node
that the Pod is served by a genuine, non-simulated LinuxPod backend before it will
pass any LinuxPod-specific acceptance (shared namespace, localhost, identity, Pod
IP ownership, residual LinuxPod-VM audit). It explicitly rejects `simulated=true`
output, and otherwise skips those phases **loudly** with the #127/#128/#129
blocker — never a silent pass.

## Validations run (this host)

| Check | Result |
|-------|--------|
| `go build ./...` | **green** (RC=0) |
| `go vet ./...` | **green** (RC=0) |
| `go test ./...` | **green** (no failures) |
| `MACVZ_LINUXPOD_HELPER=1 go test ./pkg/runtime/linuxpod -run TestSwiftHelperStubContract` | **PASS** (after the Swift-6 fix; was build-failed) |
| Real `macvz-cri --experimental-linuxpod-backend` handshake vs. stub | **handshake succeeded**, `protocolVersion=3 simulated=true capLogs=true capExec=true capStats=true`; adapter then logged `serving experimental LinuxPod-backed CRI` |
| `bash -n test/e2e/cri-k3s/linuxpod-inloop.sh` | clean; plan-mode exit 0 |
| `kubectl apply --dry-run=client -f fixtures/linuxpod-workload.yaml` | valid (5 objects) |
| Default-route audit (before/after) | **unchanged** — `default 192.168.1.1` on `en0`/`en1` identical pre/post (non-goal honored: no route mutation) |

Observed adapter handshake line (verbatim):

```text
"experimental LinuxPod backend handshake succeeded; CRI serving will use LinuxPod backend"
  helper="linuxpod-helper-stub" protocolVersion=3 simulated=true capLogs=true capExec=true capStats=true
"serving experimental LinuxPod-backed CRI (prototype; not the shipped Virtual Kubelet runtime)"
  note="lifecycle served through pkg/runtime/linuxpod backend; logs/exec/stats per helper capabilities (CRI-L2/#127)"
```

## Live real-helper attempt (AC1 substance)

After the dependent CRI-L1 (#126) real-helper work reached a buildable state in a
concurrent session, the gated real-VM lifecycle test was run to capture AC1
evidence at the backend level (a real Apple Containerization LinuxPod, not the
simulated stub):

```text
MACVZ_LINUXPOD_REAL_HELPER=1 go test -run TestRealLinuxPodHelperLifecycle ./pkg/runtime/linuxpod/
```

**First attempt (2026-06-23 ~14:57) — FAIL, reproduced blocker.** The real helper
built, booted, and listened; the Pod VM came up; but `StartContainer(app)` failed
`startProcess … vmexec … ENOENT` — the late container's rootfs was not yet staged
with the workload binary (`/bin/sh` unresolved in the guest), an in-progress
CRI-L1 (#126) gap then under active development in a concurrent session. Recorded
honestly and **not** patched here (it was another session's live code).

**Second attempt (2026-06-23 15:09), after #126's staged-rootfs fix landed — PASS.**
The same gated test now drives a real Apple Containerization LinuxPod end to end:

```text
--- PASS: TestRealLinuxPodHelperLifecycle (20.72s)
LIVE EVIDENCE: pod=pod-l1 sandboxNamespace="net:[4026531840]" (shared by app+sidecar)
LIVE EVIDENCE: app     id=pod-l1/app-2     phase=Running identityVerified=true observed="macvz-rootfs-id=app"     createdAfterPodRunning=false localhostReachable=true
LIVE EVIDENCE: sidecar id=pod-l1/sidecar-4 phase=Running identityVerified=true observed="macvz-rootfs-id=sidecar" createdAfterPodRunning=true  localhostReachable=true
```

This is the AC1/AC2/AC3 **substance on a real LinuxPod micro-VM**: an app and a
*late* sidecar (`createdAfterPodRunning=true`) both reach **Running** in **one
shared sandbox namespace** (`net:[4026531840]`), each reachable on localhost, each
with its rootfs **identity verified** (observed==expected), and `Cleanup` leaves
no residual state. It is driven through the `pkg/runtime/linuxpod` NDJSON contract
(the same backend `macvz-cri` serves), not yet through a live kubelet.

### CRI serving against the real helper (the kubelet-equivalent path) — PASSES

To close toward the literal AC1 wording, a self-contained crictl end-to-end was
run: the real helper + `macvz-cri --experimental-linuxpod-backend` serving the CRI
lifecycle (#127), driven by `crictl` (`runp` → create/start app → create/start
late sidecar), no shared cluster touched.

**First runs (15:13) — FAIL, integration gap found.** `macvz-cri` handshaked the
real helper (`protocolVersion=3 simulated=false`) and `RunPodSandbox` booted a
real Pod VM, but `StartContainer` failed: the staged rootfs was absent at the
guest path (`path.rootfs=…:missing`). The identical contract calls passed in the
direct test, so this was a #127↔#126 integration gap — the serving path had only
ever been exercised against the in-process fake. Also `ImageFsInfo` was
unimplemented (`crictl pull` validate failed). Both were recorded as blockers and
**not** patched here (they were a concurrent session's actively-edited code).

**After those fixes landed (15:45) — PASS (FAILED=0).** The same E2E now drives
the full CRI serving path → real LinuxPod backend to Running:

```text
PASS crictl handshake
PASS image pull
PASS runp (c95b3ab310691…)
PASS create app          PASS start app
PASS create sidecar      PASS start sidecar      (late: after app Running)
app     -> "state": "CONTAINER_RUNNING"
sidecar -> "state": "CONTAINER_RUNNING"
crictl ps: app + sidecar both Running in pod c95b3ab310691 (shared sandbox linuxpod-ns-c95b3ab310691…)
cleanup: containers left referencing pod: 0
```

This is the **substance of AC1 via the exact CRI gRPC API a kubelet drives**: a
real LinuxPod-backed Pod with an app and a *late* sidecar both reach
`CONTAINER_RUNNING` through `macvz-cri`'s CRI serving (#127) against the real
helper (#126), in one shared sandbox, with clean teardown.

### Real in-cluster kubelet driving the LinuxPod backend — connection PROVEN; node-Ready blocked on #128

To close the literal "kubelet/k3s in-loop" wording, the live `kind-macvz61` node
`macvz-b-cri` (a real kubelet whose CRI endpoint is normally the remote
apple/container `macvz-cri` via an in-container ssh forward) was temporarily
repointed at a host-local LinuxPod-serving `macvz-cri` + real helper (via a
loopback `host.docker.internal` bridge; the I5 forwarder was restored immediately
after, and the node returned Ready — no lasting change).

Result: the **real in-cluster kubelet connected to and drove the LinuxPod
backend** — its log carries this backend's own Status response:

```text
kubelet.go:3130 "Container runtime network not ready"
  networkReady="NetworkReady=false reason:LinuxPodNetworkNotConfigured
  message:Pod networking is not wired; LinuxPod sandboxes run without a Pod IP"
```

That message is emitted by *this* LinuxPod `macvz-cri` Status RPC — proof the
kubelet was in-loop against the LinuxPod backend. That first node run stayed
**NotReady** (`reason=KubeletNotReady`) because it deliberately used
`--pod-network` off. A later CRI-level live run on `test@192.168.1.122` enabled
#128 podnet with `10.244.102.0/24`, `bridge100`, `/var/run/macvz-netd.sock`,
ingress `en0`, and forwarding; it reported `NetworkReady=true`, assigned Pod IP
`10.244.102.2`, attached `vmIP=192.168.66.2`, detached cleanly, and preserved the
global default route. A follow-up reachability run held the same live sandbox with
busybox `httpd`; CRI verbose status exposed `podIP=10.244.102.2`,
`vmIP=192.168.67.2`, and `interface=bridge101`, local routing to
`10.244.102.0/24` via `192.168.1.122` was verified, and
`curl --max-time 10 http://10.244.102.2:8080/` returned HTTP 200 with
`macvz-linuxpod-podnet-ok`. The remaining step is to run the kubelet node with
that same podnet wiring and collect the kubelet-observed `Running` evidence.

### Real in-loop kubelet run with #128 podnet enabled — partial PASS, honest surface blockers

On 2026-06-24, `kind-macvz61`'s `macvz-b-cri` kubelet was pointed at the remote
LinuxPod-backed CRI socket on `test@192.168.1.122`, with the node-side adapter
started as:

```text
macvz-cri --experimental-linuxpod-backend
  --linuxpod-helper-socket .../linuxpod-helper.sock
  --pod-cidr 10.244.102.0/24
  --pod-network-interface bridge100
  --pod-network-helper-socket /var/run/macvz-netd.sock
  --pod-network-ingress-interface en0
  --pod-network-enable-forwarding
  --kubelet-pods-dir /Users/test/macvz-cri-i5-test/kubelet-root/pods
  --volume-host-path-allowed /Users/test/macvz-cri-i5-test
```

Several kubelet-specific blockers were found and fixed in the CRI/LinuxPod path:

- `ImageFsInfo` now reports mountpoint `/`, because kubelet stats that path on
  the Linux control-plane side of the tunneled CRI socket.
- `ImageStatus`/`ListImages` return a non-zero placeholder size until the helper
  exposes authoritative image sizes; kubelet rejects size `0`.
- same-name recreate after an exited container now removes stale backend state
  before `CreateContainer`, matching kubelet restart ordering.
- LinuxPod `CreateContainer` now preserves CRI annotations, including kubelet's
  container hash, avoiding false "container definition changed" restarts.
- the LinuxPod protocol was bumped to v4 and carries CRI mounts; the Swift helper
  stages kubelet-managed ConfigMap/Secret/projected/emptyDir sources into the Pod
  VM, materializes Kubernetes atomic-writer symlinks, and uses hash-suffixed
  staging names to avoid collisions between long kubelet volume paths.
- `macvz-netd` policy was reloaded with `podNetwork.vmNetCIDRs:
  ["192.168.64.0/20"]` after vmnet assigned `192.168.68.2`/`192.168.69.2`; the
  global default route remained `192.168.1.1` via `en0` throughout.

Latest short in-loop result (2026-06-24, one soak sample):

```text
PASS preflight: node macvz-b-cri Ready
PASS default-route audit before
PASS fixture applied
PASS kubectl rollout status (Deployment available)
PASS scheduled onto macvz-b-cri
PASS Pod events clean (no FailedScheduling/FailedCreatePodSandBox)
PASS backend evidence: genuine LinuxPod backend, simulated=false, protocolVersion=4
PASS Pod IP assigned: 10.244.102.2
PASS Pod IP belongs to the LinuxPod-backed sandbox
PASS ClusterIP Service reachable from a Linux-node probe
PASS Pod Running for the full short soak
PASS restartCount bounded
PASS no fixture Pods remain after delete
PASS default route unchanged after run
FAIL shared sandbox sidecar proof collection
FAIL kubectl logs app/sidecar
FAIL kubectl exec Secret/ConfigMap/shared proof
FAIL kubectl port-forward
```

The remaining failures align with the helper's honest capability report
(`capLogs=false capExec=false capStats=false`) and the still-open streaming
surface work: real-helper logs/exec/stats (#133), interactive/streaming exec
(#132), and Attach/PortForward (#131). They are no longer kubelet ordering,
image metadata, Pod networking, or volume materialization blockers.

### Post-review streaming fix (2026-06-24)

After #131/#132/#133 landed, the remaining #130 `kubectl exec` and
`kubectl port-forward` failures still had one CRI-serving gap: `LinuxPodService`
did not wire kubelet's streaming URL server, so the RuntimeService `Exec` and
`PortForward` RPCs returned before any backend helper call could happen. The
follow-up code fix adds the same streaming handoff used by the default
apple/container service to the LinuxPod service:

- `macvz-cri --experimental-linuxpod-backend` now starts the CRI streaming server
  when `--streaming-addr` is set and installs it on `LinuxPodService`.
- LinuxPod `Exec` mints a kubelet streaming URL; the callback currently runs the
  command through real backend `ExecSync` and writes captured stdout/stderr to
  the client, covering non-interactive `kubectl exec`.
- LinuxPod `PortForward` mints a streaming URL; the callback proxies bytes to the
  sandbox's host-reachable VM IP, falling back to Pod IP if needed.
- Attach remains honestly `Unimplemented`; true interactive stdin/TTY byte
  plumbing is still a future production streaming transport.

Validation for the code-level fix: `go test ./...`, `go vet ./...`,
`go build ./...`. A fresh `test@192.168.1.122` in-loop run is still required
before replacing the historical FAIL lines above with live PASS evidence.

## Acceptance criteria — honest status

1. **Live kubelet/k3s smoke reaches Running for a LinuxPod-backed app+sidecar
   Pod.** ✅ **Short in-loop smoke reaches Deployment Available/Pod Running with
   Pod IP and Service reachability.** Remaining failures are logs/exec/
   port-forward/shared-proof surfaces, not lifecycle admission or podnet.
2. **Evidence of shared namespace, sidecar localhost, identity, Pod IP.** ◑
   **Proven live at backend + CRI podnet layers; kubelet-observed Pod IP pending.**
   The real-helper run shows the
   app and late sidecar share one sandbox namespace (`net:[4026531840]`), both
   `localhostReachable=true`, both `identityVerified=true` (observed==expected).
   CRI-L3 now validates LinuxPod Pod IP assignment and external host-to-Pod
   reachability (`10.244.102.2:8080`) live.
3. **Stop/delete removes containers/sandbox/rootfs/handoff/network state.** ◐
   **Modeled + audited by harness.** The contract's `Cleanup` leaves no stale
   state; the harness `cleanup` phase asserts zero residual LinuxPod
   VM/container/rootfs/handoff/network state via `MACVZ_LINUXPOD_AUDIT_CMD`.
   Not yet exercised against a real VM.
4. **Adapter restart recovery.** ◐ Harness `restart-cri` phase asserts same Pod
   UID + no duplicate LinuxPod state after `MACVZ_RESTART_CRI_CMD`; serving path
   reconciles persisted records against the live backend. Not yet run live.
5. **Helper crash/restart behavior tested or documented as next blocker.** ◐
   Harness `restart-helper` phase added (`MACVZ_RESTART_HELPER_CMD`); behavior is
   the documented next blocker pending a real helper.
6. **Report under `docs/` and `docs/MASTER_TASKS.md` updated.** ✅ This report;
   MASTER_TASKS #130 row updated.

## Blockers / next issues

- ~~**B1 — CRI serving ↔ real-helper rootfs staging (#127↔#126).**~~ **RESOLVED**
  (concurrent session). `macvz-cri` serving now drives the real helper to
  `CONTAINER_RUNNING` for app + late sidecar (crictl E2E PASS, 15:45).
- ~~**B2 — LinuxPod `ImageService.ImageFsInfo` unimplemented.**~~ **RESOLVED**;
  `crictl pull` validate now passes.
- ~~**Remaining for literal AC1 — kubelet node with #128 podnet enabled.**~~
  **RESOLVED for short smoke:** the in-loop kubelet run now reaches rollout
  available, Pod IP, ClusterIP Service reachability, short soak, cleanup, and
  route-after pass on the genuine LinuxPod backend.
- ~~**CRI-L3 Pod networking (#128) live CRI attach/reachability.**~~ **RESOLVED
  for CRI-level attach and external host-to-Pod Pod IP:** `TestLiveLinuxPodServingThroughHelper`
  passed on `test@192.168.1.122` with `NetworkReady=true`, Pod IP
  `10.244.102.2`, `vmIP=192.168.66.2`/`192.168.67.2`, clean detach, unchanged
  global default route, and local `curl http://10.244.102.2:8080/` over the
  `10.244.102.0/24 -> 192.168.1.122` route returning HTTP 200. Pod-to-host
  callback remains an optional diagnostic, not a #128 blocker.
- **CRI-L4 logs/exec/stats/streaming retest.** Helper now advertises
  `capLogs/capExec/capStats`; LinuxPod CRI streaming handoff for non-interactive
  Exec and PortForward is wired locally. End-to-end kubelet logs/exec/stats/exec/
  port-forward against a real LinuxPod VM remain to be revalidated on
  `test@192.168.1.122`.
- **k3s topology.** Same operator-pending topology as #85/#119: a Linux k3s
  control plane plus a macOS node running `macvz-cri --experimental-linuxpod-backend`.

## Non-goals (honored)

- No production-readiness claim from a simulated smoke.
- No default-route mutation (audited before/after; unchanged).
- No hidden unsupported surfaces: the harness skips loudly and this report names
  every blocker.
- The shipped Virtual Kubelet path and the apple/container CRI backend are
  unchanged; the LinuxPod serving path is opt-in behind
  `--experimental-linuxpod-backend`.
