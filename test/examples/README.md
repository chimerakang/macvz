# MacVz real-app validation catalog (P8, #65)

Published, repeatable real-app validations for MacVz. Each fixture is a
self-contained harness — it applies manifests, asserts the result over the same
access path a human uses, then tears down — so a developer can validate a clean
cluster with one command and **compare any failure against the expected output
documented here**.

All fixtures are lightweight and arm64-compatible (stock multi-arch public
images, no custom builds, no registry auth) so they run unchanged on Apple
Silicon micro-VMs.

## Prerequisites

- A running MacVz cluster: at least one virtual-kubelet node joined and `Ready`
  (see [`test/e2e/two-node/`](../e2e/two-node/) for a turnkey two-Mac bring-up).
- `KUBECONFIG` pointed at that cluster, and `kubectl` on `PATH`.
- Outbound network so the nodes can pull the public images below.

Verify before you start:

```bash
export KUBECONFIG=/path/to/kubeconfig
kubectl get nodes -o wide        # a node with ROLES containing agent / labeled type=virtual-kubelet
```

## Run everything

```bash
cd test/examples
./run-all.sh                 # run every published fixture, print a pass/fail summary
./run-all.sh --list          # list the fixtures and exit
./run-all.sh hello-http p6-compat   # run a selected subset by id
```

`run-all.sh` exits `0` only when every fixture passed, and each fixture tears
its namespace down whether it passes or fails — so the cluster is left clean
either way. A passing run ends with `PASS all published P8 fixtures passed`.

Pass-through knobs: `KUBECONFIG`, `KUBECTL`, `MACVZ_E2E_TIMEOUT` (per-wait
seconds applied to every fixture), `MACVZ_E2E_CONTINUE=0` (stop at first
failure instead of running the rest).

## Published fixtures

| id | Issue | Location | Image | What it proves |
| --- | --- | --- | --- | --- |
| `hello-http` | #61 | [`test/examples/hello-http/`](hello-http/) | `nginx:1.27-alpine` | A public HTTP app served from a micro-VM, browser-visible via a ClusterIP Service + port-forward |
| `p6-compat` | #53 | [`test/e2e/p6-compat/`](../e2e/p6-compat/) | `busybox:1.36.1` | Multi-Deployment compatibility: ConfigMap, Secret, Downward API, ServiceAccount, probes, logs, exec exit-code fidelity, in-cluster Service consumption |
| `headlamp-ui` | #63 | [`test/e2e/headlamp-ui/`](../e2e/headlamp-ui/) | `ghcr.io/headlamp-k8s/headlamp:v0.30.0` | A real management UI: projected SA token, in-cluster API access, RBAC-limited (`view`) interaction, browser-visible via port-forward |
| `guestbook` | #62 | [`test/examples/guestbook/`](guestbook/) | `redis:7-alpine`, `busybox:1.36.1` | A multi-tier app: redis leader/follower + frontend, replicated state across Deployments, scaling, rollout restart, browser-visible guestbook via port-forward |

Each fixture has its own `README.md` with the full manual cheatsheet and env
knobs; this catalog documents the **expected output** so failures are easy to
localize.

---

### `hello-http` — minimal public HTTP app (#61)

```bash
KUBECONFIG=... test/examples/hello-http/run.sh
```

**Expected pods** (namespace `macvz-hello`):

```
NAME                     READY   STATUS    NODE
hello-xxxxxxxxxx-yyyyy   1/1     Running   <a virtual-kubelet node>
```

**Expected services**: `hello` (ClusterIP, port `80` → targetPort `http`),
backed by one endpoint per running replica.

**Expected logs** (`kubectl -n macvz-hello logs -l app=hello`):

```
macvz-hello nginx starting pod=hello-... node=macvz-a
```

**Expected browser behavior**: `kubectl -n macvz-hello port-forward svc/hello
8080:80`, then `http://127.0.0.1:8080/` renders the card
`It works on MacVz ✓` naming the **pod** and **virtual node** that answered.
With `MACVZ_HELLO_REPLICAS>1`, reconnecting the forward cycles the pod/node
lines as different micro-VMs answer.

**Pass line**: `PASS hello-http demo: all checks passed`.

**Cleanup**: automatic (`run.sh` deletes namespace `macvz-hello`). Manual:
`kubectl delete namespace macvz-hello`.

---

### `p6-compat` — workload compatibility (#53)

```bash
KUBECONFIG=... test/e2e/p6-compat/run.sh
```

**Expected pods** (namespace `macvz-p6`):

```
NAME                       READY   STATUS    NODE
web-xxxxxxxxxx-yyyyy        1/1     Running   <a virtual-kubelet node>
checker-xxxxxxxxxx-yyyyy    1/1     Running   <a virtual-kubelet node>
```

**Expected services**: `web` (ClusterIP, targetPort `http`) and `web-headless`
(headless, for DNS). `checker` consumes `web` in-cluster via cluster DNS +
ClusterIP routing.

**Expected behavior asserted**: rollout/availability; ConfigMap + Secret +
Downward API env wiring visible inside `web`; projected ServiceAccount token
mounted; `kubectl logs` and `kubectl exec` work, and a non-zero exec exit code
(`exit 9`) propagates faithfully; the `checker` Pod reaches `web` through the
Service; HTTP/exec probes keep Pods ready without restart-looping.

**Pass line**: `PASS P6 compatibility fixture: all checks passed`.

**Cleanup**: automatic (deletes namespace `macvz-p6`).

---

### `headlamp-ui` — management UI (#63)

```bash
KUBECONFIG=... test/e2e/headlamp-ui/run.sh
```

**Expected pods** (namespace `macvz-headlamp`):

```
NAME                        READY   STATUS    NODE
headlamp-xxxxxxxxxx-yyyyy   1/1     Running   <a virtual-kubelet node>
```

**Expected services**: `headlamp` (ClusterIP, port `80` → targetPort `http`).

**Expected behavior asserted**: rollout with healthy readiness/liveness probes
and no restart loop; the Pod lands on a virtual-kubelet node; the projected
ServiceAccount token is mounted and used to reach `kubernetes.default`
in-cluster; the bound `view` ClusterRole can `list` resources but is denied
`create`/`update`/`delete` (the RBAC boundary).

**Expected browser behavior**: `kubectl -n macvz-headlamp port-forward
svc/headlamp 4466:80`, then `http://127.0.0.1:4466/` serves the Headlamp SPA and
`/config`. Log in with a minted SA token
(`kubectl -n macvz-headlamp create token headlamp`). There is no NodePort or
LoadBalancer — port-forward is the supported browser path on MacVz (no
kube-proxy). See [`docs/MANAGEMENT_UI.md`](../../docs/MANAGEMENT_UI.md).

**Pass line**: `PASS Headlamp management-UI fixture: all checks passed`.

**Cleanup**: automatic (deletes namespace `macvz-headlamp`).

---

### `guestbook` — multi-tier real app (#62)

```bash
KUBECONFIG=... test/examples/guestbook/run.sh
```

**Expected pods** (namespace `macvz-guestbook`):

```
NAME                              READY   STATUS    NODE
redis-leader-xxxxxxxxxx-aaaaa     1/1     Running   <a virtual-kubelet node>
redis-follower-xxxxxxxxxx-bbbbb   1/1     Running   <a virtual-kubelet node>
redis-follower-xxxxxxxxxx-ccccc   1/1     Running   <a virtual-kubelet node>
frontend-xxxxxxxxxx-ddddd         1/1     Running   <a virtual-kubelet node>
frontend-xxxxxxxxxx-eeeee         1/1     Running   <a virtual-kubelet node>
frontend-xxxxxxxxxx-fffff         1/1     Running   <a virtual-kubelet node>
```

**Expected services**: `redis-leader` and `redis-follower` (ClusterIP, port
`6379`) and `frontend` (ClusterIP, port `80` → targetPort `http`).

**Expected behavior asserted**: all three Deployments roll out; a follower
reports `master_link_status:up` (replication across Deployments); the frontend
Service is browser-visible via port-forward and a submitted entry is written to
the leader and read back from the follower; logs show the frontend banner; exec
exit codes are faithful; `scale` to 5 and back works through the controller;
`rollout restart` completes and entries survive it (state is in Redis); the
namespace deletes cleanly with no Pods/VMs left behind.

**Expected browser behavior**: `kubectl -n macvz-guestbook port-forward
svc/frontend 8080:80`, then `http://localhost:8080/` shows the guestbook —
sign it, refresh, and entries persist (served from the follower replica). No
NodePort/LoadBalancer — port-forward is the supported browser path on MacVz.

**Pass line**: `PASS guestbook real-app validation: all checks passed`.

**Cleanup**: automatic (deletes namespace `macvz-guestbook`).

---

## When a fixture fails

Every fixture prints `PASS`/`FAIL`/`SKIP` per check and, on failure, writes a
redacted diagnostics bundle (nodes, deployment, pods, describe output,
endpoints, events, recent logs, and any port-forward log) to a temp directory —
override the location with that fixture's `*_DIAG_DIR` env var. Compare the
failing check and the diagnostics against the **Expected** sections above to
localize the regression (a stuck `Pending` Pod points at scheduling/node
labels; a failed in-cluster Service check points at cluster DNS / ClusterIP
routing; a failed RBAC check points at the bound role).

To leave a fixture's objects up for manual inspection, run that fixture
directly with its `*_KEEP=1` knob (e.g. `MACVZ_HELLO_KEEP=1`,
`MACVZ_P6_KEEP=1`, `MACVZ_HEADLAMP_KEEP=1`, `MACVZ_GB_KEEP=1`) instead of
through `run-all.sh`.

## Not yet published

These P8 apps are tracked separately and will be added to the catalog and to
`run-all.sh` once their harnesses land:

- **CBB arm64 subset** (#64) — the project-specific compatible subset.
