# Permanently removing a MacVz node (#58)

This is the workflow for taking a Mac **out of the node pool for good** —
decommissioning the host, not just draining it for maintenance. Removal must
leave **no active workloads and no stale network state**, on the node itself or
on the nodes that stay. MacVz runs no control plane, so this is a sequence of
ordinary, idempotent teardown steps; the `macvz-kubelet remove` command runs the
ones local to the departing node, and a short manual step prunes the peer from
every remaining node.

Removal builds on the drain workflow — drain first
([NODE_DRAIN.md](NODE_DRAIN.md)) so the owning controllers reschedule the node's
Pods elsewhere before you tear it down.

## What removal must clean up

A live MacVz node holds state in five places. Removal addresses each:

| State | Where | Cleaned by |
| --- | --- | --- |
| Kubernetes Node object | API server | `remove` (delete-node) |
| MacVz micro-VMs | apple/container on the host | `remove` (reap-vms) |
| pod-network pf rules | `macvz/pods` pf anchor | `remove` (flush-pf) |
| WireGuard routes + tunnel | this node's `utun*` interface | `remove` (mesh-down) |
| **Peer entry on every other node** | each peer's `mesh.peers:` | **manual** (see below) |

The first four are local to the departing node and automated. The last is on the
*other* nodes and cannot be done from here — it is the easy-to-miss step that
leaves a blackhole route to a CIDR nobody serves, so treat it as part of removal.

## Step 1 — drain, then stop the kubelet

```sh
# Reschedule this node's Pods elsewhere (see NODE_DRAIN.md).
kubectl drain macvz-a --ignore-daemonsets --delete-emptydir-data

# Stop macvz-kubelet on the node being removed. This is REQUIRED before removal:
# a running kubelet re-registers the Node object and recreates micro-VMs, so
# removal would not converge. (Bundle users: test/e2e/two-node/run.sh stop a.)
```

A clean kubelet shutdown already flushes the pf anchor and best-effort tears the
mesh down; `remove` then makes that deterministic and adds Node-object deletion.

## Step 2 — remove local state with `macvz-kubelet remove`

```sh
# Plan first — prints the steps and changes nothing (no --yes ⇒ planning only).
macvz-kubelet remove --config /etc/macvz/config.yaml

# Execute. --yes is required to make changes (removal is irreversible).
macvz-kubelet remove --config /etc/macvz/config.yaml --yes
```

`remove` runs four steps in order, **best-effort** — a failure in one never
aborts the others, so one unreachable subsystem cannot strand the rest:

1. **delete-node** — deletes the Kubernetes Node object so the scheduler stops
   targeting this node and the remaining nodes drop its endpoints (they then stop
   routing Service traffic here). Deleting an already-absent node is success.
2. **reap-vms** — destroys every MacVz micro-VM (`macvz-*`) on the host. On a node
   being removed, no Pod should remain, so all MacVz VMs are reaped. It never
   touches non-MacVz workloads on the host.
3. **flush-pf** — flushes the `macvz/pods` pf anchor, removing every NAT rule. Via
   the privileged helper when `privilegedHelperSocket` is set, otherwise `pfctl`
   directly (run with `sudo`).
4. **mesh-down** — deletes this node's WireGuard routes and destroys the tunnel
   interface. It deletes the config-derived routes even if this process never
   installed them, so it also cleans up after a crashed kubelet.

Each step reports `OK`, `SKIP` (component disabled or dependency unavailable), or
`FAIL`. The final verdict is `REMOVED` (no failures), `PARTIAL` (re-run needed),
or `DRY-RUN`.

Flags:

- `--yes` — confirm permanent removal. Without it (and without `--dry-run`),
  `remove` prints the plan and exits, so an accidental invocation removes nothing.
- `--dry-run` — report what would happen and change nothing.
- `--keep-node` — skip delete-node (e.g. you will deregister from a different
  machine, or want to inspect the Node object first).
- `--stop-timeout` — graceful stop per micro-VM before force-destroy (default 10s).

If the API server is unreachable from the node, `remove` skips delete-node with a
warning and still tears down local state; deregister later from a machine with
cluster access: `kubectl delete node macvz-a`.

## Step 3 — drop the peer from every remaining node

This is the one step `remove` cannot do, because it runs on the *other* nodes. On
**each** remaining node, delete the removed node's stanza from `mesh.peers:` in
its config and reconcile — `Mesh.Sync` removes only that peer's route, with no
disruption to the others (see [NETWORKING.md → Reconcile without
restart](NETWORKING.md#reconcile-without-restart)):

```sh
# On each remaining node, after editing its config to drop the macvz-a peer:
kill -HUP "$(pgrep -f 'macvz-kubelet --config')"   # reconcile peers in place

# Confirm the removed node's mesh address and Pod CIDR are no longer routed:
netstat -rn -f inet | grep -e '10.99.0.1' -e '10.244.101'   # expect no output
```

`macvz-mesh` makes regenerating each node's peer list mechanical: keep the
exported metadata files and re-render the peer set without the removed node (see
[NETWORKING.md → macvz-mesh](NETWORKING.md#macvz-mesh--automated-keyconfig-exchange-58)).

## Recovering from a partial removal

Removal is **idempotent** — if `remove` reports `PARTIAL`, fix the named cause and
re-run `macvz-kubelet remove --config … --yes`; completed steps are no-ops the
second time. Common causes:

- **delete-node FAIL** — API unreachable from the node. Re-run when connectivity
  returns, or `kubectl delete node macvz-a` from another machine.
- **reap-vms FAIL** — the runtime could not destroy a VM, or apple/container is
  down (`container system start`). Re-run, or reap directly:
  `macvz-kubelet cleanup --config … --all`.
- **flush-pf / mesh-down FAIL** — usually a missing privilege (no helper socket
  and not run with `sudo`). Re-run with the helper running, or clear by hand:

```sh
sudo pfctl -a macvz/pods -F all                 # flush only the MacVz anchor
sudo pkill -f 'wireguard-go utun7' 2>/dev/null  # wireguard-go owns the utun
sudo ifconfig utun7 destroy 2>/dev/null         # best-effort mesh interface
```

Per-symptom recovery (orphan routes, wedged utun, stale pf) is in
[PRIVILEGED_NETWORKING.md → Recovery procedures](PRIVILEGED_NETWORKING.md#recovery-procedures).

## Verifying a removal

On the **removed node** — no workloads or data-plane state remain:

```sh
container list --all                       # expect no macvz-* entries
sudo pfctl -a macvz/pods -s all            # expect empty (no rdr/binat rules)
netstat -rn -f inet | grep utun7           # expect no mesh routes
ifconfig utun7 2>&1 | grep -q 'does not exist' && echo "interface gone"
```

On the **cluster and each remaining node** — the node is forgotten and no traffic
is routed to it:

```sh
kubectl get node macvz-a                                     # expect NotFound
netstat -rn -f inet | grep -e '10.99.0.1' -e '10.244.101'    # expect no output
```

To leave the host completely clean (remove the privileged helper, restore
`pf.conf`), follow
[PRIVILEGED_NETWORKING.md → Teardown](PRIVILEGED_NETWORKING.md#teardown).

## Rehearsing on the two-node bundle

The turnkey two-Mac bundle under [`test/e2e/two-node/`](../test/e2e/two-node/README.md)
is the removal rehearsal: drain `macvz-a`, run `macvz-kubelet remove --config … --yes`
on it, drop its peer on `macvz-b` and SIGHUP, then run the verification commands
above. Capture a diagnostic bundle (`macvz-kubelet bundle`) before and after to
diff the data-plane state and confirm removal left nothing behind.
