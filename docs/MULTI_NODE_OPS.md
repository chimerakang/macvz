# Operating a MacVz node pool (#60)

This is the operator runbook for running more than one Mac as MacVz Kubernetes
nodes: how to **join, verify, drain, remove, upgrade, troubleshoot the network,
and clean up** a node, with the exact commands and the output you should expect.

It is the lifecycle index. Each step links to the deep reference for that area;
this page is the order to do things in and the glue between them.

- Join a fresh Mac → [NODE_JOIN.md](NODE_JOIN.md)
- Mesh keys, peers, and reconcile → [NETWORKING.md](NETWORKING.md#wireguard-mesh-mvp)
- Privileged setup, recovery, and teardown → [PRIVILEGED_NETWORKING.md](PRIVILEGED_NETWORKING.md)
- Two-Mac turnkey rehearsal → [`test/e2e/two-node/`](../test/e2e/two-node/README.md)

## Mental model (read once)

Four facts shape every procedure below:

1. **No MacVz control plane.** Each Mac joins an existing cluster exactly like a
   kubelet — with a kubeconfig that grants node access. Removing a node is a
   `kubectl delete node`, not a custom deregistration.
2. **The kubelet runs as your user; root work is delegated.** `apple/container`
   refuses to run as root, so privileged data-plane commands (pf, route, wg,
   sysctl) go through the `macvz-netd` helper over a unix socket. Day-to-day node
   starts need no `sudo`.
3. **Data-plane state lives in the kernel and outlives the process.** Routes, the
   `macvz/pods` pf anchor, and the `utun` mesh interface survive a kubelet crash.
   Removal and cleanup are therefore explicit steps, not automatic on exit.
4. **The helper only honors what this node's config pins.** `macvz-netd --config`
   refuses any CIDR/peer/interface/anchor not in the loaded config. Most
   "permission denied" data-plane failures are a config that drifted from reality.

## Lifecycle at a glance

| Operation | Primary command | Tooling | Deep reference |
| --- | --- | --- | --- |
| **Join** | `macvz-kubelet bootstrap` + `doctor` | shipped (#54) | [NODE_JOIN.md](NODE_JOIN.md) |
| **Mesh keys/peers** | `macvz-mesh keygen`/`export`/`peer` | shipped (#55) | [NETWORKING.md](NETWORKING.md#peer-identity--keys) |
| **Verify** | `doctor`, `/healthz/diagnostics`, `kubectl get node` | shipped (#54/#56) | [below](#verify-a-node) |
| **Drain** | `kubectl drain` | kubectl today; helper #57 planned | [below](#drain-a-node) |
| **Remove** | `kubectl delete node` + peer/route/pf cleanup | manual today; helper #58 planned | [below](#remove-a-node) |
| **Upgrade** | drain → swap binary → restart | manual today | [below](#upgrade-a-node) |
| **Troubleshoot** | `/healthz/diagnostics` → recovery runbook | shipped (#56) | [below](#network-troubleshooting) |
| **Cleanup** | helper teardown + anchor/route/VM flush | manual today; bundle #59 planned | [PRIVILEGED_NETWORKING.md](PRIVILEGED_NETWORKING.md#teardown) |

> Status note: drain (#57), automated removal (#58), and a diagnostic-bundle
> command (#59) are planned. The procedures here use the tooling that ships today
> (kubectl plus the documented manual cleanup); the table flags where a future
> command will replace a manual step. Nothing below waits on unshipped tooling.

## Join a node

Full workflow with every flag and expected output is in
[NODE_JOIN.md](NODE_JOIN.md). The short version, for a cross-host node:

```sh
# 1. Generate the node config (+ keypair + serving TLS).
macvz-kubelet bootstrap \
  --node-name macvz-a --internal-ip 192.168.1.110 \
  --kubeconfig /etc/macvz/kubeconfig \
  --pod-cidr 10.244.101.0/24 --cluster-dns 10.96.0.10 \
  --helper-socket /var/run/macvz-netd.sock \
  --mesh-address 10.99.0.1/32 --gen-key /etc/macvz/wireguard.key \
  --podnet-interface bridge100 --gen-tls \
  --out /etc/macvz/config.yaml

# 2. Exchange mesh identities (no private keys leave a host), then paste the
#    rendered peers under mesh: in this node's config. See NETWORKING.md.
macvz-mesh export --config /etc/macvz/config.yaml --out macvz-a.yaml
macvz-mesh peer macvz-b.yaml macvz-c.yaml      # paste output under mesh.peers:

# 3. Preflight. Exit 0 = clear to join (warnings allowed); 1 = a named FAIL.
macvz-kubelet doctor --config /etc/macvz/config.yaml

# 4. Start. The data plane comes up before the node registers.
KUBECONFIG=/etc/macvz/kubeconfig macvz-kubelet --config /etc/macvz/config.yaml
```

## Verify a node

Three checks, escalating from local to cluster-wide.

**1. Preflight (`doctor`) — before the first start.** One-shot; names any missing
prerequisite with a remediation. See [NODE_JOIN.md](NODE_JOIN.md#step-3--preflight-with-doctor).

```sh
macvz-kubelet doctor --config /etc/macvz/config.yaml; echo "exit=$?"
# exit=0 → clear to join (warnings allowed); exit=1 → a FAIL to fix
```

**2. Runtime health (`/healthz/diagnostics`) — while running.** The kubelet
serves a live report on its hardened HTTPS listener, classifying every check as
**control-plane**, **runtime**, or **data-plane** so the cause is unambiguous.

```sh
# Human-readable; HTTP 200 = READY, 503 = NOT READY (so curl -f gates on it).
curl -fsk https://192.168.1.110:10250/healthz/diagnostics
# Machine-readable for automation:
curl -sk  https://192.168.1.110:10250/healthz/diagnostics?format=json | jq .verdict
```

A node is `READY` only when no check fails; warnings (e.g. a single-node mesh
with no peers) do not block. See
[NODE_JOIN.md](NODE_JOIN.md#live-health-diagnostics) for the field reference.

**3. Cluster view — what the scheduler sees.**

```sh
kubectl get node macvz-a -o wide
# NAME      STATUS   ROLES   AGE   VERSION   INTERNAL-IP     ...
# macvz-a   Ready    agent   2m    ...       192.168.1.110   ...
```

`Ready` here plus `200` from diagnostics means the node is fully operational.
Schedule Pods that tolerate `virtual-kubelet.io/provider=macvz:NoSchedule`.

## Drain a node

Drain before any disruptive action (upgrade, reboot, removal) so workloads
reschedule onto other nodes first. This uses stock kubectl today; a MacVz drain
helper that also tears down per-Pod data-plane state is planned (#57).

```sh
# 1. Stop new placements and evict existing Pods.
kubectl drain macvz-a \
  --ignore-daemonsets --delete-emptydir-data \
  --pod-selector 'app!=keep-me'        # optional carve-outs
# node/macvz-a cordoned
# evicting pod default/web-7d9...   ... pod/web-7d9... evicted
# node/macvz-a drained

# 2. Confirm the node is empty of reschedulable Pods and is unschedulable.
kubectl get node macvz-a -o jsonpath='{.spec.unschedulable}{"\n"}'   # true
kubectl get pods --all-namespaces --field-selector spec.nodeName=macvz-a
# only mirror/daemonset pods (or none) should remain
```

What happens under the hood: each evicted Pod's micro-VM is stopped and
destroyed by the provider, its Pod IP is released back to IPAM, and its
`binat`/`rdr` entries are removed from the `macvz/pods` anchor on the next
reconcile. Verify no micro-VMs linger:

```sh
container list --all          # expect no MacVz-managed micro-VMs for drained Pods
sudo pfctl -a macvz/pods -s nat   # expect no binat rules for the drained Pods
```

If you only need a temporary pause (reboot, then return the node), `kubectl
uncordon macvz-a` after it restarts puts it back in rotation — skip the removal
steps below.

## Remove a node

Removal is drain + deregister + **explicit data-plane cleanup on this node and
on every peer**, because kernel state does not revert on its own (mental-model
fact 3). `macvz-kubelet remove` automates the four steps local to the departing
node (delete Node object, reap micro-VMs, flush pf, tear down mesh); pruning the
peer on the *other* nodes stays manual. Full runbook:
[NODE_REMOVAL.md](NODE_REMOVAL.md).

```sh
# 1. Drain first (see above). Then stop the kubelet on the node being removed.
#    A running kubelet re-registers the node and recreates VMs, so removal would
#    not converge. (bundle: run.sh stop a — otherwise signal the macvz-kubelet process)

# 2. On the removed node: delete-node + reap-vms + flush-pf + mesh-down, in order,
#    best-effort and idempotent. Plan first, then --yes to act.
macvz-kubelet remove --config /etc/macvz/config.yaml            # plan only
macvz-kubelet remove --config /etc/macvz/config.yaml --yes      # execute

# 3. On EVERY OTHER node: drop the removed node from the peer list.
#    Remove its stanza from mesh.peers: in each config, then reconcile.
#    Mesh.Sync removes only the affected host route — no full teardown:
kill -HUP "$(pgrep -f 'macvz-kubelet --config')"   # reconcile peers in place
#    Confirm the removed node's mesh route and Pod CIDR are gone on each peer:
netstat -rn -f inet | grep -e '10.99.0.1' -e '10.244.101'   # expect no output
```

The critical, easy-to-miss step is **#3** — `remove` runs on the departing node
and cannot touch the others, so a removed node left in a peer's config keeps a
blackhole route to a CIDR nobody serves. Treat peer-list pruning as part of
removal, not optional hygiene. If `remove` reports `PARTIAL`, fix the named cause
and re-run (completed steps are no-ops); per-symptom recovery is in
[NODE_REMOVAL.md → Recovering from a partial removal](NODE_REMOVAL.md#recovering-from-a-partial-removal)
and [PRIVILEGED_NETWORKING.md → Recovery procedures](PRIVILEGED_NETWORKING.md#recovery-procedures).

To leave the host completely clean (remove the helper, restore `pf.conf`), follow
[PRIVILEGED_NETWORKING.md → Teardown](PRIVILEGED_NETWORKING.md#teardown).

## Upgrade a node

MacVz is a single resident binary per Mac, so an upgrade is a drain-swap-restart.
Roll one node at a time so capacity stays available.

```sh
# 1. Drain so workloads move off (see "Drain a node").
kubectl drain macvz-a --ignore-daemonsets --delete-emptydir-data

# 2. Stop the kubelet, replace the binary, re-run doctor against the SAME config.
#    (A new binary can add config validation — doctor catches drift before start.)
make build && sudo install -m 0755 bin/macvz-kubelet /usr/local/bin/macvz-kubelet
macvz-kubelet doctor --config /etc/macvz/config.yaml; echo "exit=$?"   # want exit=0

# 3. If the privileged helper changed, upgrade it too (it is a separate binary).
#    See PRIVILEGED_NETWORKING.md "Upgrading".
sudo macvz-netd uninstall && sudo macvz-netd install --config /etc/macvz/config.yaml ...

# 4. Restart and confirm health, then return the node to scheduling.
KUBECONFIG=/etc/macvz/kubeconfig macvz-kubelet --config /etc/macvz/config.yaml &
curl -fsk https://192.168.1.110:10250/healthz/diagnostics >/dev/null && echo READY
kubectl uncordon macvz-a
```

Version skew: `macvz-kubelet` and `macvz-netd` share the config schema. Upgrade
the helper whenever a release notes a config change, and always re-run `doctor`
after swapping either binary — it validates the config against the new build.

## Network troubleshooting

Start at `/healthz/diagnostics`. Its three-class taxonomy points you straight at
the right runbook; you rarely need to guess.

| Diagnostics says | Likely cause | Go to |
| --- | --- | --- |
| `control-plane` FAIL | node not registered, or stale node-lease heartbeat | kubeconfig/API reachability — [NODE_JOIN.md](NODE_JOIN.md#troubleshooting) |
| `runtime` FAIL | `apple/container` not started | `container system start`; [NODE_JOIN.md](NODE_JOIN.md#troubleshooting) |
| `data-plane` helper FAIL | `macvz-netd` down, not root, or no `--config` | [PRIVILEGED_NETWORKING.md → Helper failures](PRIVILEGED_NETWORKING.md#helper-failures) |
| `data-plane` mesh/route FAIL | peer down, key mismatch, or orphan/missing route | [PRIVILEGED_NETWORKING.md → Stale routes](PRIVILEGED_NETWORKING.md#stale-routes) |
| `data-plane` pod-network FAIL | wrong bridge, pf anchor not loaded, forwarding off | [PRIVILEGED_NETWORKING.md → Stale pf rules](PRIVILEGED_NETWORKING.md#stale-pf-rules) |

Cross-Mac Pod reachability, in the order to check it:

```sh
wg show                                  # is the tunnel up? handshakes recent?
netstat -rn -f inet | grep utun          # does the remote Pod CIDR route via utun?
sudo pfctl -a macvz/pods -s nat          # one binat rule per local Pod?
sysctl net.inet.ip.forwarding            # must be 1 for transit
kubectl exec <pod-on-a> -- ping -c1 <pod-ip-on-b>   # end-to-end
```

The safe first move for most stale-state symptoms is **restart the kubelet** —
bring-up tolerates "already exists" and `Mesh.Sync` reconciles incrementally, so
a restart fixes everything except orphans from a since-changed config (those you
delete by hand, per the recovery runbook).

## Cleanup and diagnostic bundles

- **Leave a host clean** — stop the kubelet, flush the `macvz/pods` anchor,
  destroy the mesh interface, remove the helper, optionally restore `pf.conf`:
  [PRIVILEGED_NETWORKING.md → Teardown](PRIVILEGED_NETWORKING.md#teardown).
- **Collect support state** — a one-shot diagnostic-bundle command is planned
  (#59). Until it ships, capture the live report plus the data-plane state for a
  bug report:

  ```sh
  curl -sk https://<internal-ip>:10250/healthz/diagnostics?format=json > diag.json
  macvz-kubelet doctor --config /etc/macvz/config.yaml > doctor.txt 2>&1
  wg show > wg.txt; netstat -rn -f inet > routes.txt
  sudo pfctl -a macvz/pods -s all > pf.txt
  tail -n 200 /var/log/macvz-netd.log > helper.txt
  ```

## Validating this runbook

Per the acceptance criteria for #60, walk the full lifecycle on **two Macs**:
join both, verify each (`doctor` exit 0, diagnostics `200`, both `Ready`), deploy
a Service backed by Pods on both, drain one, confirm the Service still serves
from the survivor, remove the drained node (including peer-list pruning on the
other), and confirm the survivor reports no orphan routes. The turnkey bundle in
[`test/e2e/two-node/`](../test/e2e/two-node/README.md) sets up the join half; the
cross-Mac Service check is the
[end-to-end walkthrough in NETWORKING.md](NETWORKING.md#end-to-end-a-service-across-two-macs).
</content>
</invoke>
