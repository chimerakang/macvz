# P6 compatibility fixture

This is the **P6 acceptance workload** (issue #53): a small but representative
multi-Deployment application that exercises the Kubernetes workload-compatibility
features delivered in milestone P6, and a `run.sh` harness that deploys it onto
one or more MacVz virtual nodes and validates the rollout end to end.

Use it as the repeatable proof that "real" controller-managed apps — Deployments
with ConfigMaps, Secrets, probes, ServiceAccounts, and Services — run on MacVz
without manifest rewrites.

## What it deploys

All objects live in the `macvz-p6` namespace (see `manifests/`):

| Object | Kind | Purpose |
| --- | --- | --- |
| `web-sa` | ServiceAccount | Projected token mount (#51), shared by both Deployments |
| `web-config`, `checker-config` | ConfigMap | env (`envFrom`, `configMapKeyRef`) and a volume file (#46) |
| `web-secret` | Secret | env (`secretKeyRef`) and a read-only volume (#47) |
| `web` | Deployment (2 replicas) | Renders a status page from every input, serves it over HTTP, gated by startup/readiness/liveness probes (#48, #50) |
| `web`, `web-headless` | Service | ClusterIP + headless access to `web` (#37) |
| `checker` | Deployment (1 replica) | Continuously fetches the `web` Service; its readiness is gated on success, proving cluster DNS + ClusterIP routing into the micro-VMs |

### Feature coverage

| Issue | Feature | Where it is exercised |
| --- | --- | --- |
| #45 | Deployment / restartPolicy | both Deployments (controller-managed Pods) |
| #46 | ConfigMap env + volume | `web` `envFrom`/`configMapKeyRef` + `/etc/web` mount; `checker` `configMapKeyRef` |
| #47 | Secret env + volume | `web` `secretKeyRef` + read-only `/etc/web-secret` mount |
| #48 | Downward API env | `web` `fieldRef` (pod name, node name) + `resourceFieldRef` (memory limit) |
| #50 | Probes | `web` startup/readiness/liveness HTTP probes; `checker` exec readiness probe |
| #51 | ServiceAccount projection | `serviceAccountName: web-sa` → projected kube-api-access token |
| #37 | ClusterIP Service routing | `checker` reaches `web` by cluster DNS name |

The `web` container reports the Secret only as `token_present=yes` and never
prints its value; the harness asserts presence, never contents, and its failure
diagnostics redact Secret material.

## Prerequisites

- A Kubernetes control plane with **at least one** registered `macvz-kubelet`
  node (labeled `type=virtual-kubelet`, carrying the
  `virtual-kubelet.io/provider` taint). Two or more nodes spread the replicas.
- The running kubelet must already implement P6 (#45–#51). On a kubelet missing
  one of these, the corresponding Pod stays Pending/Failed and the matching check
  fails with an actionable message.
- `kubectl` configured against the cluster (`KUBECONFIG`).
- The test image must be arm64 and reachable from the nodes. The default
  `busybox:1.36.1` is public and arm64-native.

## Running

```sh
# Apply, validate, and tear down.
KUBECONFIG=/path/to/kubeconfig ./run.sh

# Keep the namespace afterwards for manual inspection.
KUBECONFIG=/path/to/kubeconfig MACVZ_P6_KEEP=1 ./run.sh
```

The harness prints `PASS`/`FAIL`/`SKIP` per check and exits non-zero if any
check fails. On failure it writes a redacted diagnostics bundle (deployments,
replicasets, pods, describe output, endpoints, events, and recent logs) to a
temp directory (or `MACVZ_P6_DIAG_DIR`).

### Environment knobs

| Variable | Default | Meaning |
| --- | --- | --- |
| `KUBECONFIG` | standard | cluster credentials |
| `KUBECTL` | `kubectl` | kubectl binary |
| `MACVZ_P6_NAMESPACE` | `macvz-p6` | namespace to target (re-points every object) |
| `MACVZ_P6_IMAGE` | `busybox:1.36.1` | arm64 image with `sh`/`httpd`/`wget` |
| `MACVZ_P6_TIMEOUT` | `180` | per-wait timeout (seconds) |
| `MACVZ_P6_DIAG_DIR` | mktemp dir | where failure diagnostics are written |
| `MACVZ_P6_KEEP` | unset | set to `1` to skip teardown |

## Checks performed

1. **Rollout** — `web` and `checker` reach `rollout status` complete; `web` has
   all replicas available.
2. **Env wiring** — exec into a `web` Pod and assert the rendered page reflects
   `configMapKeyRef`, `envFrom` (prefixed), `secretKeyRef`, the ConfigMap and
   Secret volume files, and the Downward API `fieldRef`/`resourceFieldRef` values.
3. **ServiceAccount** — the projected token exists at the standard path.
4. **Logs** — `kubectl logs` returns the workload's startup banner.
5. **Exec** — a non-zero exit code propagates through `kubectl exec`.
6. **Service** — `checker` becomes Available (it could reach `web` over the
   Service) and its last fetch contains `web`'s rendered page.
7. **Probes** — every `web` Pod is Ready with zero restarts (probes healthy, not
   flapping).

## CI

The fixture runs on the same self-hosted topology as the multi-node suite
(`make compat`, or the `compat` job in `.github/workflows/e2e.yml`). GitHub-hosted
runners cannot provide Apple Silicon nodes, so it is `workflow_dispatch`-only.
