# CRI-L5 LinuxPod-Backed Kubelet/k3s In-Loop Validation Report (#130)

Date: 2026-06-23
Parent: #125 · Depends on: #127 (serving), #128 (Pod networking), #129 (logs/exec/stats)
Outcome: **`linuxpodKubeletInLoopBlocked`** — every layer proven on real hardware
up to a kubelet-*observed* Running Pod; CRI-L3 (#128) now has live CRI podnet
attach plus external host-to-Pod Pod IP reachability evidence, and the remaining
gate is applying that same podnet wiring to the kubelet/k3s node so it reaches
Ready. Honest, precise blocker — not a silent pass, and not a CRI-path or backend
defect.

## Summary

This phase adds the real kubelet/k3s **in-loop validation harness** for the
experimental LinuxPod-backed CRI path and then drove the path as far as real,
non-disruptive validation allows. Proven on real hardware: (1) the real Apple
Containerization LinuxPod helper runs an app + *late* sidecar to **Running** in a
shared namespace with verified identity; (2) `macvz-cri`'s CRI serving drives that
real backend to `CONTAINER_RUNNING` via `crictl` (the exact CRI API a kubelet
uses); (3) CRI-L3 live podnet attach now reports `NetworkReady=true`, assigns a
Pod IP, preserves the host default route on `test@192.168.1.122`, and serves HTTP
over the Pod IP from an external Mac route; (4) a
**real in-cluster kubelet** was temporarily repointed at the
LinuxPod backend and **drove it in-loop** (its log carries this backend's Status
RPC), then cleanly restored. The one remaining gate to a kubelet-observed
`Running` Pod is bringing up the kubelet node with the now-proven #128 podnet
settings (node-Ready requires `NetworkReady=true`) and running the in-loop
harness. The harness still **refuses to pass** LinuxPod acceptances on a
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

So the literal kubelet smoke is blocked at the **final** step — applying the
now-live-proven pod-network settings to the kubelet node — not by the CRI serving
path (proven via crictl), the backend (proven on a real VM), or the CRI podnet
attach path. The `linuxpod-inloop.sh` harness should be the next evidence
collector once that node is launched with #128 networking enabled.

## Acceptance criteria — honest status

1. **Live kubelet/k3s smoke reaches Running for a LinuxPod-backed app+sidecar
   Pod.** ◑ **Everything up to a kubelet-observed Running is proven; final gate is
   applying #128 live pod networking to the kubelet node.** Three layers proven on real hardware: (a) real
   helper test PASS (app + late sidecar Running on a real VM, shared namespace,
   identity verified); (b) `macvz-cri` CRI serving → real helper, crictl-driven,
   both containers `CONTAINER_RUNNING` (FAILED=0); (c) a **real in-cluster kubelet
   driving this LinuxPod backend** (its log carries this backend's Status RPC). The
   one remaining gap to a kubelet-observed `Running` Pod: the node stays NotReady
   until it is launched with the now-proven CRI-L3 (#128) live pod networking
   flags — operator/shared-infra, not a CRI-path or backend defect.
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
- **Remaining for literal AC1 — kubelet node with #128 podnet enabled.** The
  kubelet↔LinuxPod-backend connection is proven (a real in-cluster kubelet drove
  this `macvz-cri`), and CRI-L3 live podnet attach is now proven through the CRI
  serving test. Bring a LinuxPod node up with `--pod-cidr`/
  `--pod-network-interface`/`--pod-network-helper-socket` live, then
  `linuxpod-inloop.sh` collects the kubelet `Running` evidence.
- ~~**CRI-L3 Pod networking (#128) live CRI attach/reachability.**~~ **RESOLVED
  for CRI-level attach and external host-to-Pod Pod IP:** `TestLiveLinuxPodServingThroughHelper`
  passed on `test@192.168.1.122` with `NetworkReady=true`, Pod IP
  `10.244.102.2`, `vmIP=192.168.66.2`/`192.168.67.2`, clean detach, unchanged
  global default route, and local `curl http://10.244.102.2:8080/` over the
  `10.244.102.0/24 -> 192.168.1.122` route returning HTTP 200. Pod-to-host
  callback remains an optional diagnostic, not a #128 blocker.
- **CRI-L4 logs/exec/stats (#129).** Helper advertises `capLogs/capExec/capStats`;
  end-to-end kubelet logs/exec/stats against a real LinuxPod VM remain to be
  validated.
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
