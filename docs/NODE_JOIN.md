# Joining a Mac to a MacVz node pool (#54)

This is the repeatable workflow for adding a fresh Apple Silicon Mac as a
Kubernetes virtual node. MacVz does **not** run its own control plane — a node
joins an existing cluster the same way a kubelet does, using a kubeconfig with
node credentials. The `macvz-kubelet` binary ships two helpers for this:

- `macvz-kubelet bootstrap` — generate a node config from the minimum join
  inputs (and, optionally, a WireGuard keypair and self-signed serving TLS).
- `macvz-kubelet doctor` — verify every prerequisite before the first start, so
  errors identify the missing piece instead of failing opaquely at runtime.

For the two-Mac WireGuard data-plane rehearsal, the turnkey bundle under
[`test/e2e/two-node/`](../test/e2e/two-node/README.md) wires these inputs into a
ready-to-run example.

## Minimum join inputs

| Input | Flag | Required when | Notes |
| --- | --- | --- | --- |
| Cluster kubeconfig | `--kubeconfig` | always | Credentials to reach the existing API server. No control plane is created. |
| Node name | `--node-name` | always | Kubernetes node name to register as. |
| Internal IP | `--internal-ip` | always | The node's reachable IPv4 (API server connects here for logs/exec). |
| Pod CIDR | `--pod-cidr` | clusters without node-CIDR allocation | Omit when Kubernetes assigns `Node.Spec.PodCIDR`. |
| Cluster DNS | `--cluster-dns` | ClusterIP DNS in-VM | CoreDNS/kube-dns ClusterIP, e.g. `10.96.0.10`. |
| Helper socket | `--helper-socket` | mesh or Pod network on | macvz-netd unix socket; the kubelet runs as your user and routes pf/wg/route through it. |
| Mesh address | `--mesh-address` | cross-host networking | This node's mesh address in CIDR form, e.g. `10.99.0.1/32`. Enables the mesh stanza. |
| Peer data | (edit config) | cross-host networking | Each other node's `publicKey`, `endpoint`, `podCIDR`, `address`. |
| Serving TLS | `--gen-tls` / `--serving-cert` | `kubectl logs`/`exec` | Cert SAN must include `--internal-ip`. |

## Step 1 — generate the node config

Single-host (no cross-host networking):

```sh
macvz-kubelet bootstrap \
  --node-name mac-mini-01 \
  --internal-ip 192.168.1.50 \
  --kubeconfig /etc/macvz/kubeconfig \
  --gen-tls \
  --out /etc/macvz/config.yaml
```

Cross-host (WireGuard mesh + Pod network). Generate this node's keypair first;
the public key is printed for you to paste into the **other** nodes' peer lists:

```sh
macvz-kubelet bootstrap \
  --node-name macvz-a \
  --internal-ip 192.168.1.110 \
  --kubeconfig /etc/macvz/kubeconfig \
  --pod-cidr 10.244.101.0/24 \
  --cluster-dns 10.96.0.10 \
  --helper-socket /var/run/macvz-netd.sock \
  --mesh-address 10.99.0.1/32 \
  --gen-key /etc/macvz/wireguard.key \
  --podnet-interface bridge100 \
  --gen-tls \
  --out /etc/macvz/config.yaml
```

Then edit `/etc/macvz/config.yaml` and fill in the `mesh.peers` stanza with each
other node's public key, endpoint, Pod CIDR, and mesh address. (The generated
config validates structurally but ships with an empty peer list, since peer keys
come from the other hosts.) The `macvz-mesh` helper (`make mesh`, #55) automates
keygen, public-key export, and rendering peer stanzas — see
[docs/NETWORKING.md](NETWORKING.md).

The generated YAML is self-validated through the same loader the kubelet uses,
so a successful `bootstrap` means the config parses.

## Step 2 — install the privileged helper (cross-host only)

When the mesh or Pod network is enabled, pf/wg/route must run as root via
`macvz-netd`. Install and start it per
[docs/PRIVILEGED_NETWORKING.md](PRIVILEGED_NETWORKING.md), and hook the pf anchor
into `/etc/pf.conf`. The helper must run with `--config` so it enforces the
per-request policy (allowed CIDRs/peers/anchors).

## Step 3 — preflight with `doctor`

```sh
macvz-kubelet doctor --config /etc/macvz/config.yaml
```

`doctor` checks, scoped to what the config enables:

- **apple/container runtime** — installed and `container system start` has run.
- **cluster kubeconfig / API server** — kubeconfig resolves and the API server
  answers (a warning, not a failure, if it is only reachable once the mesh is up).
- **wireguard tooling** — `wg` and `wireguard-go` on PATH (when mesh enabled).
- **privileged network helper** — macvz-netd reachable, can run commands, and is
  enforcing policy (when mesh/Pod network enabled).
- **pod network tooling** — `pfctl` present and IPv4 forwarding state.
- **kubelet serving TLS** — cert/key present, unexpired, and SAN covers the node
  InternalIP (so `kubectl logs`/`exec` work).

Exit code `0` means the node is clear to join (warnings allowed); `1` means a
required prerequisite is missing, named with a remediation. Fix the `FAIL`
items and re-run until green.

## Step 4 — start the node

```sh
KUBECONFIG=/etc/macvz/kubeconfig macvz-kubelet --config /etc/macvz/config.yaml
```

On cross-host nodes the kubelet brings the data plane up before connecting to
the API server, then registers the virtual node. Confirm with:

```sh
kubectl get node <node-name> -o wide
```

The node should report `Ready`. Schedule Pods that tolerate the
`virtual-kubelet.io/provider=macvz:NoSchedule` taint.

## Live health diagnostics

`doctor` is a one-shot preflight. Once the node is running, the kubelet serves an
ongoing health report at `/healthz/diagnostics` on its serving endpoint (the same
hardened HTTPS listener as logs/exec, so it requires `node.servingTLSCertFile`/
`servingTLSKeyFile`, and mutual TLS when a client CA is configured):

```sh
# human-readable, from a host that can reach the node's kubelet port
curl -sk https://<internal-ip>:<kubeletPort>/healthz/diagnostics
# machine-readable
curl -sk https://<internal-ip>:<kubeletPort>/healthz/diagnostics?format=json
```

The report answers **why** a node is not ready for workloads, grouping checks into
three failure domains so the cause is unambiguous:

- **control-plane** — node registration and node-lease heartbeat freshness.
- **runtime** — apple/container service readiness.
- **data-plane** — privileged helper reachability/policy, WireGuard mesh peers and
  routes, host IP forwarding, and live Pod network attachments.

A node is `READY` only when no check fails (warnings, e.g. a single-node mesh with
no peers, do not block readiness). The endpoint returns HTTP `200` when ready and
`503` when not, so a probe or `curl -f` can gate on it directly.

## Troubleshooting

`doctor` is the first stop — every failure names the missing prerequisite. Common
cases:

- **kubeconfig FAIL** — wrong path or missing credentials; the node joins an
  existing cluster, so the kubeconfig must already grant node access.
- **API server WARN** — address is right but not yet routable; on mesh nodes it
  becomes reachable once `macvz-kubelet` brings WireGuard up.
- **privileged helper FAIL** — `macvz-netd` not running, not root, or started
  without `--config` (policy not enforced).
- **serving TLS WARN** — SAN does not include the InternalIP; reissue with
  `--gen-tls` or `subjectAltName=IP:<internal-ip>`.
