# Running a Kubernetes management UI on MacVz (#63)

This document is the P8 evaluation of a Kubernetes-native management UI on MacVz:
which UI was chosen and why, how it maps onto MacVz's virtual-node model, the
browser access path, and the gaps that became follow-up issues.

The repeatable fixture lives in
[`test/e2e/headlamp-ui/`](../test/e2e/headlamp-ui/) — manifests plus a `run.sh`
that deploys the UI, asserts rollout/probes/token, smoke-tests the browser path,
and checks the RBAC boundary.

## Decision: Headlamp

**Headlamp** ([headlamp.dev](https://headlamp.dev), a CNCF Sandbox project) is
the chosen UI. It is the best fit for a virtual-kubelet node:

| Criterion | Headlamp | Kubernetes Dashboard |
| --- | --- | --- |
| Pod topology | **single** stateless container | multi-container split (dashboard + metrics-scraper + kong gateway in recent charts) |
| arm64 image | published multi-arch (`ghcr.io/headlamp-k8s/headlamp`) | published, but the auxiliary gateway/scraper images add arm64 surface to verify |
| In-cluster API access | projected SA token + `kubernetes.default` only | same, plus inter-component traffic |
| Hardening | runs cleanly under restricted PSS | workable, more moving parts |
| Browser access | single HTTP port, port-forward-friendly | newer charts assume an in-cluster gateway/ingress |

The deciding factor is MacVz's **one-container-per-micro-VM** model: MacVz fails
a multi-container Pod fast with a sticky `Failed` status (see
[WORKLOADS.md](./WORKLOADS.md)). The current Kubernetes Dashboard ships as
several Deployments wired through a gateway, so it is not a single-Pod drop-in;
Headlamp is one container that needs nothing MacVz does not already provide.

> The Dashboard *can* run on MacVz when split into its component single-container
> Deployments, but that is a larger validation surface than #63 calls for.
> Headlamp proves the management-UI category with the least incidental risk.

## How Headlamp maps onto MacVz

Headlamp in `-in-cluster` mode is a back-end HTTP server that proxies the
Kubernetes API to a single-page app. Everything it needs already exists on a
MacVz node:

| Headlamp requirement | MacVz feature | Status |
| --- | --- | --- |
| Run as a controller-managed Pod | Deployment / restartPolicy (#45) | ✅ |
| Single container | one-container-per-micro-VM | ✅ (Headlamp is single-container) |
| arm64 image | arm64 image verification (P1) | ✅ |
| Talk to the API server in-cluster | projected SA token (#51) + ClusterIP routing to `kubernetes.default` (#37) | ✅ |
| Health gating | readiness/liveness HTTP probes (#50) | ✅ |
| Hardened (restricted PSS) | field-by-field securityContext (#52) | ✅ |
| Browser reachability | `kubectl port-forward` (#28) | ✅ (via port-forward) |
| Per-user RBAC | standard ClusterRole/ClusterRoleBinding | ✅ (control-plane) |

### securityContext note

MacVz enforces `runAsNonRoot` **only when paired with an explicit non-zero
`runAsUser`** (see [WORKLOADS.md](./WORKLOADS.md) §securityContext). The fixture
therefore sets `runAsUser: 100` (Headlamp's image user) alongside
`runAsNonRoot: true`, so the guarantee is actually enforced rather than treated
as a bare declaration. `capabilities.drop: [ALL]` maps to `--cap-drop ALL`;
`allowPrivilegeEscalation: false` and `seccompProfile: RuntimeDefault` are
accepted no-ops satisfied by the micro-VM boundary.

## Browser access path

A MacVz virtual node runs **no kube-proxy**. A `NodePort` or `LoadBalancer`
Service has no node-side program point, so the supported browser path is a
ClusterIP Service plus `kubectl port-forward` (#28):

```sh
kubectl -n macvz-headlamp port-forward svc/headlamp 4466:80
kubectl -n macvz-headlamp create token headlamp   # login token
open http://127.0.0.1:4466                         # paste the token
```

The pasted token's RBAC governs the session. The fixture binds the `headlamp`
ServiceAccount to the built-in read-only `view` ClusterRole, so the UI can list
and inspect every resource but cannot mutate — the "RBAC-limited interaction"
required by #63. Granting write access is a one-line change to `edit` or
`cluster-admin` in `manifests/20-rbac.yaml`.

## Validation

`test/e2e/headlamp-ui/run.sh` asserts, against a live cluster with a MacVz node:

1. **Rollout & placement** — the Deployment rolls out, the Pod lands on a
   `type=virtual-kubelet` node, becomes Ready (readiness probe), and does not
   restart-loop (liveness stable). (#45, #50)
2. **In-cluster identity** — the projected SA token is mounted at the standard
   path. (#51)
3. **Browser path** — `kubectl port-forward` to the ClusterIP Service serves
   Headlamp's `/config` JSON and SPA shell on `127.0.0.1`. (#28, #37)
4. **RBAC boundary** — the bound `view` identity can `list pods` cluster-wide but
   is denied `delete`/`create`/`update`.

Each check names the feature it exercises, so on a kubelet missing one the
failure points straight at the gap.

## Outcome and follow-ups

**Headlamp runs on MacVz** with no manifest rewrites beyond the standard
virtual-node `nodeSelector`/`toleration`, and is reachable in a browser via
port-forward with RBAC-limited control. The management-UI category is validated
for P8.

Gaps surfaced during this evaluation, to be filed as follow-up issues:

- **Ingress / browser exposure without port-forward.** With no kube-proxy,
  NodePort/LoadBalancer do not work on a virtual node. A documented Ingress or
  gateway path would let users reach UIs (and other web apps) without a manual
  port-forward. Relates to #61/#62 (browser-visible Services).
- **Multi-container UI support.** The Kubernetes Dashboard and other UIs ship as
  multi-container Pods (sidecar gateway/metrics-scraper). MacVz's one-container
  model rejects these. A sidecar/multi-container story (or documented
  split-Deployment recipe) would widen UI compatibility.
- **`metrics-server` / resource metrics.** Management UIs show CPU/memory graphs
  from `metrics.k8s.io`. MacVz's improved resource accounting (#68) still needs
  a metrics API surface before those panels can populate instead of showing
  "N/A".
