# Multi-node End-to-End Suite

The beta-readiness e2e suite for MacVz (issue #30). It proves the runtime,
provider, networking, and operational paths work together across more than one
Mac, and is the recorded P4 acceptance evidence.

- Harness: [test/e2e/e2e.sh](../test/e2e/e2e.sh)
- Go wrapper / release gate: [test/e2e/e2e_test.go](../test/e2e/e2e_test.go)
- CI (manual, self-hosted): [.github/workflows/e2e.yml](../.github/workflows/e2e.yml)

## Topology

```
                 ┌──────────────────────┐
                 │  Kubernetes control   │
                 │  plane (any platform) │
                 └───────────┬───────────┘
            register +        │       register +
            heartbeat         │       heartbeat
        ┌──────────────┐      │      ┌──────────────┐
        │  Mac A        │◀─────┴─────▶│  Mac B        │
        │ macvz-kubelet │  WireGuard  │ macvz-kubelet │
        │ apple/container│   mesh     │ apple/container│
        └──────────────┘            └──────────────┘
```

- A reachable Kubernetes API server (kind/k3s/managed — any platform).
- **2+ Apple Silicon Macs**, each running `macvz-kubelet` and registered as a
  Ready virtual node. For the cross-node Service check, both nodes need a
  Kubernetes-assigned Pod CIDR, the [WireGuard mesh](NETWORKING.md) up, and
  `podNetwork.enabled: true`.
- A serving TLS cert/key on each node so `logs`/`exec`/`port-forward` work (see
  [P2_SMOKE_TEST.md](P2_SMOKE_TEST.md) and [SECURITY.md](SECURITY.md)).

## What it covers

| Phase | Checks | Issues |
| --- | --- | --- |
| Node registration | each node Ready, `arch=arm64`, provider taint, capacity advertised | #13–#15 |
| Pod lifecycle | schedule → Ready → `logs` → `exec` (`uname -m`, exit-code fidelity) → delete | #16–#18 |
| Cross-node Service | one backend per node behind a Service; 2 ready endpoints; client Pod reaches **both** nodes | #20–#23 |
| Port-forward | `port-forward` to a backend reaches the in-Pod HTTP server | #24 |
| Cleanup | namespace and all workloads removed | — |

On any failure the suite captures diagnostics (node describes, Pod describes,
endpoints, events) to `MACVZ_E2E_DIAG_DIR` and exits non-zero — suitable for
release gating.

## Run it

```sh
# Auto-detect MacVz nodes (label type=virtual-kubelet):
MACVZ_E2E=1 make e2e

# …or pin the node names explicitly:
MACVZ_E2E=1 MACVZ_E2E_NODES=mac-a,mac-b go test -tags e2e -v -timeout 30m ./test/e2e/

# …or run the harness directly:
MACVZ_E2E_NODES=mac-a,mac-b ./test/e2e/e2e.sh
```

Configuration (environment):

| Var | Default | Purpose |
| --- | --- | --- |
| `KUBECONFIG` | standard | cluster credentials |
| `MACVZ_E2E_NODES` | auto-detect | comma-separated node names |
| `MACVZ_E2E_IMAGE` | `alpine:3.20` | arm64 image (busybox `httpd` + `wget`) |
| `MACVZ_E2E_NAMESPACE` | `macvz-e2e` | namespace for test objects |
| `MACVZ_E2E_DIAG_DIR` | mktemp | where failure diagnostics are written |
| `MACVZ_E2E_TIMEOUT` | `120` | per-wait timeout (seconds) |

Setup and teardown are automated: the suite creates its own namespace and tears
it down at the end (and on failure, after capturing diagnostics). It does **not**
provision the cluster, nodes, RBAC, or mesh — bring those up first per
[P2_SMOKE_TEST.md](P2_SMOKE_TEST.md) and [NETWORKING.md](NETWORKING.md), and
apply [deployments/rbac.yaml](../deployments/rbac.yaml).

## Manual fallback (hardware-limited)

With only **one** Mac available, the suite still runs every single-node phase and
reports the cross-node Service check as `SKIP` (single-node Service reachability
is verified instead). This is the supported fallback when a second Mac is not
available:

```sh
MACVZ_E2E_NODES=mac-a ./test/e2e/e2e.sh    # cross-node phase -> SKIP
```

For pure-runtime validation without a cluster at all, use the gated driver
integration tests on the Mac itself:

```sh
MACVZ_INTEGRATION=1 go test ./pkg/runtime/container/ -v
```

These cover micro-VM lifecycle, logs, exec, volumes, stats, and Rosetta against
a real `apple/container` service. The [P2 smoke test](P2_SMOKE_TEST.md) covers
the single-node provider path end-to-end by hand.

## CI

[.github/workflows/e2e.yml](../.github/workflows/e2e.yml) runs the suite on
demand (`workflow_dispatch`) on a self-hosted runner labeled `macvz-e2e` whose
`kubectl` is configured against the beta topology; failure diagnostics are
uploaded as an artifact. GitHub-hosted runners cannot provide multiple Macs plus
a cluster, so this path is intentionally manual.

## P4 acceptance evidence

| Acceptance criterion | Evidence |
| --- | --- |
| Multi-node e2e passes on the beta topology | `make e2e` against 2 Macs; all phases `PASS` |
| Output suitable for release gating | non-zero exit on failure; diagnostics captured; Go test wrapper for CI |
| P4 acceptance recorded in docs | this document + [MASTER_TASKS.md](MASTER_TASKS.md) |

Record a passing run (date, node names, MacVz version, and the final `PASS`
summary line) in the release notes when cutting a beta tag.
