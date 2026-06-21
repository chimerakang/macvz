# Guestbook real-app validation (#62)

This is the **P8 guestbook fixture**: a realistic, multi-component Kubernetes
application — a Redis leader/follower data tier plus a stateless web frontend —
and a `run.sh` harness that deploys it onto one or more MacVz virtual nodes and
validates it end to end.

It is the repeatable proof that a *real* app made of several Deployments,
Services, ConfigMaps, and a Secret — with replicated state and a browser-visible
UI — runs on MacVz through normal Kubernetes controllers, with no manifest
rewrites and no custom image builds.

Everything runs on two stock, public, arm64-native images: **`redis:7-alpine`**
and **`busybox:1.36.1`**. No registry auth, no Dockerfile.

## What it deploys

All objects live in the `macvz-guestbook` namespace (see `manifests/`):

| Object | Kind | Purpose |
| --- | --- | --- |
| `guestbook-redis` | Secret | Redis password — as env on Redis (`requirepass`/`masterauth`) and as a read-only file the frontend reads to AUTH |
| `guestbook-config` | ConfigMap | `APP_TITLE` env (`configMapKeyRef`) + a sourceable `app.conf` (Redis hostnames + list key) |
| `guestbook-content` | ConfigMap | the landing page and the CGI program, staged into the served tree at startup |
| `redis-leader` | Deployment (1) + Service | the write side; the frontend `RPUSH`es entries here |
| `redis-follower` | Deployment (2) + Service | read replicas of the leader; the frontend `LRANGE`s entries from here |
| `frontend` | Deployment (3) + Service | busybox `httpd` + a CGI that renders the guestbook and talks to Redis over `nc`; the browser-visible tier |

The frontend page reads from the **follower** and writes to the **leader**, so a
working page proves Redis replication is flowing across two separate Deployments.

### Architecture

```
            ┌──────────────┐   RPUSH (write)   ┌──────────────┐
 browser ──▶│  frontend    │ ────────────────▶ │ redis-leader │
 (port-fwd) │  (httpd+CGI) │                   │   (1 pod)    │
            │   3 pods     │ ◀──────────────┐  └──────┬───────┘
            └──────────────┘  LRANGE (read) │         │ replicate
                                            │  ┌──────▼─────────┐
                                            └──│ redis-follower │
                                               │    (2 pods)    │
                                               └────────────────┘
```

## How the frontend works (no custom image)

`busybox` ships both `httpd` and `nc`, so the frontend needs no build:

- The `guestbook-content` ConfigMap carries a small POSIX-`sh` CGI. The Pod's
  entrypoint copies it into a writable `emptyDir`, marks it executable, and runs
  `httpd -h /www`. `httpd` runs anything under `/cgi-bin/` as CGI.
- The CGI speaks the Redis RESP protocol directly over `nc`: `LRANGE` against
  `redis-follower` to render the list, `RPUSH` against `redis-leader` to add an
  entry, authenticating with the Secret-provided password. Entries are
  HTML-escaped and length-capped.

This data path was validated against the exact images (`redis:7-alpine` +
`busybox:1.36.1`) with docker before shipping — replication, urlencoded form
posts, leader/follower split, newest-first ordering, HTML escaping, and AUTH
enforcement all pass.

## Prerequisites

- A Kubernetes control plane with **at least one** registered `macvz-kubelet`
  node (labeled `type=virtual-kubelet`, carrying the
  `virtual-kubelet.io/provider` taint). Two or more nodes spread the replicas
  across Macs.
- A kubelet implementing the P6 workload features (#45–#51): Deployments,
  ConfigMap/Secret env + volumes, probes, and ClusterIP Service routing (#37).
- `kubectl` and `curl` on the operator host. `KUBECONFIG` set.
- Both images are public arm64 multi-arch and reachable from the nodes.

## Running

```sh
# Apply, validate end to end, and tear down.
KUBECONFIG=/path/to/kubeconfig ./run.sh

# Keep the namespace afterwards for manual inspection / browser use.
KUBECONFIG=/path/to/kubeconfig MACVZ_GB_KEEP=1 ./run.sh
```

The harness prints `PASS`/`FAIL` per check and exits non-zero if any check
fails. On failure it writes a **redacted** diagnostics bundle (deployments,
replicasets, pods, describe output, endpoints, events, and recent logs — Secret
material masked) to a temp dir (or `MACVZ_GB_DIAG_DIR`).

### Open it in a browser

Either run with `MACVZ_GB_KEEP=1`, or apply the manifests directly, then:

```sh
kubectl -n macvz-guestbook port-forward svc/frontend 8080:80
# open http://localhost:8080/  → sign the guestbook, refresh to see entries
```

### Environment knobs

| Variable | Default | Meaning |
| --- | --- | --- |
| `KUBECONFIG` | standard | cluster credentials |
| `KUBECTL` | `kubectl` | kubectl binary |
| `MACVZ_GB_NAMESPACE` | `macvz-guestbook` | namespace to target (re-points every object) |
| `MACVZ_GB_BUSYBOX_IMAGE` | `busybox:1.36.1` | arm64 image with `sh`/`httpd`/`nc` |
| `MACVZ_GB_REDIS_IMAGE` | `redis:7-alpine` | arm64 redis image |
| `MACVZ_GB_TIMEOUT` | `240` | per-wait timeout (seconds) |
| `MACVZ_GB_PF_PORT` | `18080` | local port for the browser-visibility check |
| `MACVZ_GB_DIAG_DIR` | mktemp dir | where failure diagnostics are written |
| `MACVZ_GB_KEEP` | unset | set to `1` to skip teardown |

## Checks performed

1. **Rollout** — `redis-leader`, `redis-follower`, and `frontend` all reach
   `rollout status` complete; the multi-replica Deployments report all replicas
   available.
2. **Replication** — exec into a follower and assert `master_link_status:up`,
   proving it synced from the leader Service across Deployments.
3. **Browser-visible + functional** — `kubectl port-forward svc/frontend`, then:
   the landing page is served; the guestbook page renders; a submitted entry is
   written to the leader and read back from the follower; entries are
   HTML-escaped.
4. **Logs** — `kubectl logs` returns the frontend startup banner.
5. **Exec** — the CGI is staged and executable in the Pod, and a non-zero exit
   code propagates through `kubectl exec`.
6. **Scaling** — `kubectl scale deploy/frontend --replicas=5` reaches 5/5
   available through the controller, then scales back to 3.
7. **Rollout restart** — `kubectl rollout restart deploy/frontend` completes a
   rolling update, and the guestbook entries survive it (state lives in Redis,
   not the frontend).
8. **Cleanup** — deleting the namespace blocks until every Pod (and its
   micro-VM) is gone; the harness confirms the namespace is fully removed, so no
   VMs or network state are left behind.

## Acceptance mapping (#62)

| Acceptance criterion | Where it is proven |
| --- | --- |
| Full app deploys and is browser-visible | phases 1 + 3 (rollout, port-forward landing/guestbook page) |
| Scaling and rollout status work through Kubernetes controllers | phases 6 + 7 (`scale`, `rollout restart`/`status`) |
| Cleanup leaves no VMs or network state behind | phase 8 (namespace delete waits for Pod/VM teardown, then verifies removal) |
| Multiple Deployments/Services + ConfigMap + Secret | 3 Deployments, 3 Services, 2 ConfigMaps, 1 Secret in `manifests/` |
| Exercise rollout, scaling, logs, exec, cleanup | phases 1, 4, 5, 6, 7, 8 |

## CI

The fixture targets the same self-hosted Apple-Silicon topology as the multi-node
suite. GitHub-hosted runners cannot provide arm64 MacVz nodes, so it runs on the
self-hosted lane (`workflow_dispatch`) rather than on hosted CI.
