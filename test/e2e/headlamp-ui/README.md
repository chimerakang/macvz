# Headlamp management-UI fixture (issue #63)

This fixture deploys **Headlamp**, a Kubernetes-native management UI, onto a
MacVz virtual node and validates that it runs, is reachable in a browser, and
respects RBAC. It is the repeatable proof for milestone P8's "run a management
UI" goal (issue #63).

For the evaluation that chose Headlamp over the Kubernetes Dashboard, and the
full MacVz compatibility analysis, see [`docs/MANAGEMENT_UI.md`](../../../docs/MANAGEMENT_UI.md).

## What it deploys

All objects live in the `macvz-headlamp` namespace (see `manifests/`), except the
cluster-scoped RBAC binding:

| Object | Kind | Purpose |
| --- | --- | --- |
| `macvz-headlamp` | Namespace | restricted Pod Security Standard enforced |
| `headlamp` | ServiceAccount | Pod identity; projected token (#51) for in-cluster API |
| `macvz-headlamp-view` | ClusterRoleBinding | binds `headlamp` to the built-in read-only `view` ClusterRole |
| `headlamp` | Deployment (1 replica) | the UI; single arm64 container, hardened securityContext, HTTP probes |
| `headlamp` | Service (ClusterIP) | port-forward target for browser access |

When `MACVZ_HEADLAMP_NAMESPACE` overrides the namespace, `run.sh` also renames
the ClusterRoleBinding to `<namespace>-view` so parallel/manual fixture runs do
not share a cluster-scoped binding.

### Feature coverage

| Issue | Feature | Where it is exercised |
| --- | --- | --- |
| #45 | Deployment / restartPolicy | the `headlamp` Deployment (controller-managed Pod) |
| #50 | readiness + liveness probes | HTTP probes against the UI's own `/` on port 4466 |
| #51 | ServiceAccount projection | `serviceAccountName: headlamp` → in-cluster API token |
| #52 | securityContext (restricted PSS) | `runAsUser`+`runAsNonRoot`, cap-drop ALL, seccomp RuntimeDefault |
| #37 | ClusterIP routing | the UI reaches `kubernetes.default` in-cluster |
| #28 | port-forward | the browser access path the harness smoke-tests |

## Prerequisites

- A Kubernetes control plane with **at least one** registered `macvz-kubelet`
  node (labeled `type=virtual-kubelet`, carrying the
  `virtual-kubelet.io/provider` taint).
- The running kubelet must implement P5–P7 (#37, #45, #50, #51, #52). On a
  kubelet missing one of these the Pod stays Pending/Failed and the matching
  check fails with an actionable message.
- `kubectl` configured against the cluster (`KUBECONFIG`), and `curl` on the host
  for the browser-path smoke.
- The Headlamp image must be reachable from the node. The default
  `ghcr.io/headlamp-k8s/headlamp:v0.30.0` is public and arm64-native.

## Running

```sh
# Apply, validate (rollout, probes, SA token, port-forward HTTP smoke, RBAC),
# then tear down.
KUBECONFIG=/path/to/kubeconfig ./run.sh

# Keep the namespace afterwards and print the browser/login commands.
KUBECONFIG=/path/to/kubeconfig MACVZ_HEADLAMP_KEEP=1 ./run.sh

# Pin a different Headlamp build.
MACVZ_HEADLAMP_IMAGE=ghcr.io/headlamp-k8s/headlamp:v0.31.0 ./run.sh
```

See the header of `run.sh` for every tunable environment variable.

## Browser access path

A MacVz virtual node runs **no kube-proxy**, so a `NodePort`/`LoadBalancer`
Service has nowhere to program on the node. The supported, documented path is
`kubectl port-forward` (#28):

```sh
# 1. Forward the ClusterIP Service to your workstation.
kubectl -n macvz-headlamp port-forward svc/headlamp 4466:80

# 2. Mint a login token for the read-only ServiceAccount.
kubectl -n macvz-headlamp create token headlamp

# 3. Open the UI and paste the token at the prompt.
open http://127.0.0.1:4466
```

The token's RBAC (`view` here) governs what the UI can do: you can browse and
inspect every resource but mutations are denied. Swap the binding in
`manifests/20-rbac.yaml` to `edit` or `cluster-admin` to grant write access.

Exposing the UI without a manual port-forward (Ingress / gateway) is out of
scope for this fixture and tracked as a P8/P9 follow-up; see
[`docs/MANAGEMENT_UI.md`](../../../docs/MANAGEMENT_UI.md).
