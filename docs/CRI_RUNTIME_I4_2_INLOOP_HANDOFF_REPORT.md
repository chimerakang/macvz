# CRI-I4-2 kubelet/k3s In-Loop Against the Handoff-Aware Runtime (#119)

Date: 2026-06-22

Outcome: `kubeletHandoffSmokeBlocked` — live 2-host k3s topology operator-pending.

## Purpose

Issue **#119** (CRI-I4, Kubelet Validation) re-runs the #85 kubelet/k3s in-loop
fixture against the **handoff-aware runtime** built across CRI-I1..I3
(#109..#117). #85 built and validated the in-loop harness but its live run was
operator-pending, and at that time the handoff runtime did not exist. This issue
makes the handoff path actually reachable by the harness and publishes exact
evidence for everything runnable on the dev host, recording an honest blocker for
the part that requires a real two-host topology.

It is scoped to the experimental `develop` CRI track and does **not** gate the
shipped Virtual Kubelet path.

## What changed for #119

Before this issue the handoff-aware lifecycle (#115 CreateContainer prep, #116
StartContainer identity gate, #117 Stop/Remove/Status) existed in `pkg/criserver`
and `pkg/runtime`, but **the `macvz-cri` binary never constructed a
`HandoffManager`** — `criserver.Options.Handoff` was always nil, so no fixture,
crictl or kubelet, could exercise the handoff path. That was the central gap.

1. **Binary wiring** (`cmd/macvz-cri/main.go`): added `--experimental-handoff`
   and `--handoff-root`. When set, `run()` builds `runtime.NewHandoffManager(root)`
   and passes it as `Options.Handoff`. Off by default, so the shipped
   apple/container path is unchanged. The production handoff root
   `/run/macvz/containers` is not writable on macOS, so `--handoff-root` points the
   subtree at a writable per-user directory for local/operator runs.

2. **Harness** (`test/e2e/cri-k3s/k3s-inloop.sh`): added a `handoff` phase (gated
   by `MACVZ_HANDOFF=1`) between `scheduling` and `logs`. Because the handoff-aware
   runtime only persists a container Running after StartContainer's identity gate
   verifies the launched process's rootfs identity (#116), a Pod reaching Running
   is itself in-loop evidence that the gate passed. The phase asserts that and,
   when `MACVZ_HANDOFF_STATUS_CMD` is provided, surfaces the on-node
   `handoffStatusInfo` diagnostics (`handoffPrepared` / `identityVerified` /
   `expectedIdentity` / `observedIdentity`, #117) and asserts `identityVerified`.

3. **Runbook** (`test/e2e/cri-k3s/README.md`): documents enabling the handoff
   path on the node via `MACVZ_CRI_EXTRA="--experimental-handoff --handoff-root …"`
   and running the harness with `MACVZ_HANDOFF=1`.

## Environment (this run)

- Host: Darwin 25.5.0 arm64 (macOS 26.5.1, build 25F80).
- Go: go1.25.8 darwin/arm64.
- apple/container CLI: version 1.0.0 (build: release).
- macvz-cri version: `dev` (local build).

## Commands and evidence

### Build, vet, test (hermetic)

```sh
go build ./...                                              # exit 0
go vet ./cmd/macvz-cri/... ./pkg/criserver/... ./pkg/runtime/...   # clean
go test ./cmd/macvz-cri/... ./pkg/criserver/... ./pkg/runtime/...
#   ok  cmd/macvz-cri
#   ok  pkg/criserver
#   ok  pkg/criserver/store
#   ok  pkg/runtime
#   ok  pkg/runtime/container
```

### Binary exposes the handoff flags

```sh
go build -o /tmp/macvz-cri ./cmd/macvz-cri
/tmp/macvz-cri --help | grep -A1 'experimental-handoff\|handoff-root'
#   -experimental-handoff
#       opt into the experimental LinuxPod runtime handoff path (CRI-I, #109..#117): …
#   -handoff-root string
#       root directory for the experimental handoff subtree … point this at a writable per-user dir …
```

### Binary boots on the handoff path (wiring is live)

```sh
/tmp/macvz-cri --listen unix:///tmp/macvz-cri-h.sock --state-dir "" \
  --experimental-handoff --handoff-root /tmp/macvz-handoff --streaming-addr "" --logtostderr
# main.go:243] "experimental LinuxPod handoff path enabled" root="/tmp/macvz-handoff"
#   note="CreateContainer stages a runtime-private rootfs/handoff subtree and StartContainer
#         gates Running on identity verification (docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md)"
# main.go:284] "starting experimental macvz-cri server" version="dev" socket="…" …
```

This confirms `Options.Handoff` is now wired through the real server binary, not
just in unit tests.

### Harness is handoff-aware (plan + syntax)

```sh
bash -n test/e2e/cri-k3s/k3s-inloop.sh        # OK
bash test/e2e/cri-k3s/k3s-inloop.sh           # plan-only (no MACVZ_INTEGRATION)
#   handoff       (MACVZ_HANDOFF=1, node on macvz-cri --experimental-handoff)
#                 container Running implies StartContainer's identity gate passed (#116); …
```

### Runtime primitive already proven live (#114)

The handoff launch primitive the in-loop path depends on was validated on real
apple/container in **#114** (`docs/CRI_RUNTIME_I2_HANDOFF_LAUNCH_REPORT.md`):
`outcome=runtimeHandoffLaunchSucceeded`, observed identity matched the expected
`macvz-handoff-id=late-alpha`, `status=Verified`, on this same host class. The
gap for #119 is therefore not the runtime primitive but the kubelet-in-the-loop
control plane.

## Did kubelet create/start a Pod through the handoff-aware path?

**Not yet — blocked.** The kubelet/k3s in-loop smoke requires a real two-host
topology that the dev host cannot stand up unattended:

- a Linux k3s server / control plane + scheduler, and
- a macOS Apple Silicon host serving `macvz-cri --experimental-handoff` as that
  node's external CRI endpoint, joined with the #84 labels/taint.

This is the same operator-pending blocker recorded for #85
(`docs/CRI_K3S_INLOOP_REPORT.md`); #119 removes the *code* blocker (the binary
could not enable handoff at all) but the *topology* blocker remains. Per the issue
non-goals, no multi-day soak and no broad workload-compatibility claim are made.

## Precise blocker

`kubeletHandoffSmokeBlocked`: live run pending a Linux k3s control plane plus a
macOS handoff-enabled CRI node. Everything reachable without that topology is
green — handoff wiring builds/boots, the harness is handoff-aware and passes
`bash -n`/plan-mode, the affected packages pass `go test`/`go vet`, and the
underlying handoff launch primitive passed live in #114.

## Operator run (fill in from a live two-host run)

With the topology in place:

```sh
# On the macOS CRI node, enable the handoff path on the adapter service:
MACVZ_CRI_EXTRA="--experimental-handoff --handoff-root $HOME/.macvz/cri/handoff" \
  ./scripts/macvz-cri-install.sh install --from ./bin \
    --socket "$HOME/.macvz/cri/macvz-cri.sock" --state-dir "$HOME/.macvz/cri/state"

# From a machine with kubectl to the k3s control plane:
export KUBECONFIG=/path/to/k3s.yaml
export MACVZ_INTEGRATION=1
export MACVZ_HANDOFF=1
export MACVZ_CRI_OUT_DIR=/tmp/cri-inloop-handoff
# Optional: surface on-node identity diagnostics (crictl Verbose inspect).
export MACVZ_HANDOFF_STATUS_CMD="ssh mac 'crictl inspect --output json <container-id>'"
bash test/e2e/cri-k3s/k3s-inloop.sh
```

Expected on success: the `scheduling` phase places the Pod on the MacVz node, the
`handoff` phase reports `container Running through the handoff identity gate`
(and, with the status hook, `identityVerified=true`), and the outcome flips to
`kubeletHandoffSmokePassed`. Record here: command, host, runtime version, full
phase log, and cleanup confirmation.

## Cleanup

This run created only ephemeral local artifacts, all removed:

- temp socket `/tmp/macvz-cri-h-*.sock` (removed),
- temp handoff root `/tmp/macvz-handoff-*` (removed),
- temp binary `/tmp/macvz-cri` (build artifact).

No apple/container workloads were created (no live in-loop run), so no host
workload cleanup was required. A live operator run uses the harness `cleanup`
phase (delete the fixture namespace; assert no residual Pods and no stale
`macvz-cri-*` workloads).

## Non-goals honored

- No multi-day soak.
- No broad workload-compatibility claim from a single smoke test.
- The handoff path stays off by default; the shipped Virtual Kubelet runtime is
  unchanged; the CRI route stays no-go for replacement.
