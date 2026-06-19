# Two-Mac WireGuard + podNetwork e2e (issue #37)

Turnkey bundle to prove the full cross-host data plane on two physical Apple
Silicon Macs with `mesh.enabled: true` and `podNetwork.enabled: true`, then run
`test/e2e/e2e.sh` to exit 0 across both nodes.

This bundle covers issue #37 **Scope** items 1–2 (two-node config with WireGuard
peers, pod CIDRs, bridge100 podNetwork; pf anchor hooks, IPv4 forwarding, routes,
handshakes). Items 3–4 (run the suite, capture diagnostics) are executed by the
operator because they require **privileged macOS networking and a second host**
that cannot be driven non-interactively from a coding agent.

## Topology (matches docs/MULTI_NODE_TEST_REPORT_2026-06-19.md)

| Node | Host | Mesh addr | Pod CIDR | WG endpoint |
| --- | --- | --- | --- | --- |
| `macvz-a` | 192.168.1.110 | 10.99.0.1/32 | 10.244.101.0/24 | 192.168.1.110:51820 |
| `macvz-b` | 192.168.1.122 | 10.99.0.2/32 | 10.244.102.0/24 | 192.168.1.122:51820 |

Mesh net `10.99.0.0/24`, Pod supernet `10.244.0.0/16`. Adjust IPs in
`macvz-a.yaml` / `macvz-b.yaml` if your hosts differ, then regenerate keys.

## Files

- `macvz-a.yaml`, `macvz-b.yaml` — per-node kubelet configs (mesh + podNetwork
  on). Peer `publicKey` fields are filled from `keys/*.pub`.
- `pf-anchor-hooks.conf` — the four anchor lines referenced from `/etc/pf.conf`.
- `keys/` — `*.pub` are committed; `*.key` (private) are gitignored. Regenerate
  with `make keys` below.
- `prep-node.sh` — privileged per-host setup (key, serving TLS, pf hook,
  forwarding). Run once per Mac with `sudo`.
- `verify-dataplane.sh` — issue #37 Validation checks (wg handshake, routes, pf).
- `run.sh` — starts the local kubelet and drives `test/e2e/e2e.sh`.

## Prerequisites (per host)

`apple/container`, `wireguard-tools` (`wg`, `wireguard-go`), `kubectl`, and a
shared Kubernetes control plane reachable from both Macs. WireGuard UDP 51820
must be open between the two hosts. **sudo** is required on both.

## Run

```sh
# 0. (re)generate keypairs — only if you changed hosts/keys.
#    Public keys are baked into the *.yaml peer stanzas.
( cd keys && wg genkey | tee macvz-a.key | wg pubkey > macvz-a.pub \
           && wg genkey | tee macvz-b.key | wg pubkey > macvz-b.pub \
           && chmod 600 *.key )
#    then copy each *.pub into the OTHER node's peers[].publicKey.

# 1. copy this bundle + the built bin/macvz-kubelet to BOTH Macs.

# 2. privileged setup on EACH Mac
sudo ./prep-node.sh a      # on 192.168.1.110
sudo ./prep-node.sh b      # on 192.168.1.122

# 3. start macvz-kubelet on EACH Mac (KUBECONFIG must point at the cluster)
KUBECONFIG=... ./run.sh start a   # on .110
KUBECONFIG=... ./run.sh start b   # on .122

# 4. confirm the data plane is live (either Mac)
sudo ./verify-dataplane.sh a

# 5. run the suite (from any host with cluster access)
MACVZ_E2E_NODES=macvz-a,macvz-b MACVZ_E2E_TIMEOUT=120 \
  MACVZ_E2E_DIAG_DIR=./diag ../e2e.sh
```

## Acceptance (issue #37)

- Pod-to-Pod by Pod IP works across Macs.
- ClusterIP Service reaches backends on both Macs (e2e.sh "Service across nodes"
  phase PASSes instead of SKIPPED).
- `e2e.sh` exits 0 with both nodes.
- Cleanup leaves no orphan VMs, routes, or stale pf rules — see below.

## Cleanup

```sh
./run.sh stop a                 # stop kubelet (flushes the macvz/pods anchor)
# Reap any orphan micro-VMs + flush the anchor (e.g. after a kubelet kill -9).
# See docs/NODE_DRAIN.md for the full drain/remove workflow.
macvz-kubelet cleanup --config macvz-a.yaml --all
sudo pfctl -a macvz/pods -F all # belt-and-suspenders anchor flush
sudo sysctl -w net.inet.ip.forwarding=0   # if you want to revert forwarding
# restore /etc/pf.conf from the .macvz.bak.* prep-node.sh left, if desired
container list --all            # expect empty
netstat -rn -f inet | grep utun7  # expect no stale remote Pod CIDR routes
```

Record results in `docs/MULTI_NODE_TEST_REPORT_2026-06-19.md` (or a new dated
report) per the issue's Validation section.
