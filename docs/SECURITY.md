# Security Model & Operator Responsibilities

MacVz turns an Apple Silicon Mac into a Kubernetes virtual node that runs OCI
workloads as isolated Linux micro-VMs via `apple/container`. This document
states the security boundaries, conservative defaults, and the operator's
responsibilities for a beta deployment (issue #28).

## Trust boundaries

```
 Kubernetes API server  ──TLS──▶  macvz-kubelet (host process on the Mac)
        ▲                              │
        │ mutual TLS (logs/exec/        │ local unix socket / CLI
        │  portforward/stats)           ▼
   kubectl user                   apple/container service ──▶ micro-VMs
```

- **macvz-kubelet → API server:** outbound, authenticated by the kubeconfig /
  in-cluster ServiceAccount token over the cluster's normal TLS. No bespoke auth
  path; standard client-go credential flows only.
- **API server → kubelet endpoint:** inbound to the Mac for `kubectl
  logs`/`exec`/`port-forward` and metrics scraping. This is the most sensitive
  surface and is hardened below.
- **macvz-kubelet → apple/container:** local only (CLI over the host's unix
  socket). Not a network listener.

## 1. Kubernetes API access (mTLS)

Access uses ordinary kubeconfig or in-cluster configuration
(`config.RestConfig`), so connections to the API server use client-go's standard
TLS (server CA verification + client credential). MacVz adds nothing here and
hardcodes no endpoints or tokens.

**Operator responsibility:** provide a kubeconfig that points at the
`macvz-kubelet` ServiceAccount (or an equivalently scoped identity), not a
cluster-admin credential, in production.

## 2. Kubelet serving endpoint (logs/exec/portforward/stats)

This HTTPS endpoint exposes powerful operations — `exec` is arbitrary in-guest
command execution. It is hardened in three ways:

- **Mutual TLS.** Set `node.servingClientCAFile` to the cluster's API-server
  client CA. The server then **requires and verifies** a client certificate
  signed by that CA (`RequireAndVerifyClientCert`), so only the API server can
  call it.
- **Bind address.** When `node.internalIP` is set (or auto-detected), the server
  binds to that address rather than all interfaces, shrinking exposure.
- **Loud default.** With no client CA configured, macvz-kubelet logs a prominent
  warning at startup that the endpoint is unauthenticated.

**Operator responsibility:** in production, always set `servingClientCAFile`. If
you leave it unset (e.g. local dev), restrict the kubelet port by firewall to
the API server only. The endpoint is disabled entirely when no serving
certificate is configured (Pods still run; logs/exec are unavailable).

```yaml
node:
  internalIP: 10.0.0.42
  kubeletPort: 10250
  servingTLSCertFile: /etc/macvz/serving.crt
  servingTLSKeyFile: /etc/macvz/serving.key
  servingClientCAFile: /etc/macvz/apiserver-client-ca.crt   # enables mutual TLS
```

## 3. RBAC (least privilege)

[deployments/rbac.yaml](../deployments/rbac.yaml) is scoped to exactly what the
Virtual Kubelet node and pod controllers invoke, verified against the upstream
controller source:

| Resource | Verbs | Why |
| --- | --- | --- |
| `nodes` | get, list, watch, create | register and read the node; never deleted by macvz |
| `nodes/status` | get, patch, update | maintain conditions/addresses/capacity |
| `pods` | get, list, watch, delete | reconcile Pods; delete when the workload is gone |
| `pods/status` | get, update, patch | report Pod status |
| `configmaps`, `secrets`, `services` | get, list, watch | required by the upstream pod-controller informers |
| `events` | create, patch, update | Pod/node lifecycle events |
| `leases` | get, create, update | node heartbeat in kube-node-lease |

`delete` on nodes and leases is intentionally **not** granted (the controllers
never call it), nor is write access to the Pod spec.

**Secrets note:** the broad `secrets` read is the one rule wider than MacVz's own
needs — MacVz does not materialize Secret contents (env `valueFrom` and
configMap/secret volumes are rejected; see [VOLUMES.md](VOLUMES.md)), but the
upstream pod controller constructs a Secret informer regardless. If your cluster
supports it, scope this via per-namespace Roles instead of a cluster-wide read,
or run on a Kubernetes build where the informer can be disabled. This is a known
beta limitation, documented rather than silently granted.

## 4. Runtime access boundary

The `apple/container` runtime is driven through its CLI over a host-local unix
socket; macvz-kubelet opens no network listener for runtime control. The runtime
socket must not be proxied or bound to a network address. Each workload runs in
its own micro-VM (hardware-isolated), not a shared kernel namespace.

## 5. Secrets, registry credentials, and Pod env

- **No secret/credential logging.** Startup and lifecycle logs record image
  refs, IDs, and config flags — never env values, tokens, or kubeconfig
  contents.
- **Pod env injection is conservative.** Env `valueFrom` (Secret/ConfigMap/field
  refs) is rejected by translation, so MacVz never reads Secret material to
  inject into a guest. Only literal env values are passed.
- **Registry credentials.** `imagePullSecrets` are not consumed. Private
  registries must be authenticated at the host level via the `container`
  tooling's own credential store, keeping registry secrets off the Pod path.

## Operator checklist

- [ ] Run macvz-kubelet under the `macvz-kubelet` ServiceAccount, not cluster-admin.
- [ ] Set `servingClientCAFile` (mutual TLS) — or firewall the kubelet port to the API server.
- [ ] Set `node.internalIP` so the kubelet endpoint binds to one address.
- [ ] Keep `node.volumes.hostPathAllowedPrefixes` empty unless hostPath is needed; keep it tightly scoped when it is (see [VOLUMES.md](VOLUMES.md)).
- [ ] Store serving keys and kubeconfig with `0600` perms, outside any image or git.
- [ ] Authenticate private registries on the host, not via `imagePullSecrets`.
