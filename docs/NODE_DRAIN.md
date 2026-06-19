# Draining and removing a MacVz node (#57)

This is the workflow for taking a MacVz node out of service safely — for
maintenance, decommissioning, or moving its workloads elsewhere. MacVz runs no
control plane, so draining is **standard `kubectl drain`**: the API server
evicts the node's Pods, Virtual Kubelet deletes each one, and the provider tears
its micro-VM and pod-network rules down. A `cleanup` helper verifies the node
came away clean and recovers the rare case where the kubelet exited before it
could.

## How cleanup happens automatically

When a Pod is evicted (by drain) or deleted, Virtual Kubelet calls the
provider's `DeletePod`, which for that Pod:

1. stops the probers, then stops and destroys every micro-VM backing its
   containers (graceful stop, then force);
2. detaches its pod-network path — flushing the per-Pod `rdr`/`binat` rules from
   the `macvz/pods` pf anchor;
3. returns its Pod IP to the IPAM pool;
4. removes its ephemeral volume directories.

So on a normally drained node, **no orphan VMs or pf rules remain** without any
extra step. The kubelet additionally flushes the whole `macvz/pods` anchor and
tears the mesh down on graceful shutdown (SIGTERM/SIGINT).

## Drain flow

MacVz nodes carry the `virtual-kubelet.io/provider=macvz:NoSchedule` taint, so
only Pods that tolerate it ever land here. To drain:

```sh
# 1. Cordon so the scheduler stops placing new Pods on the node.
kubectl cordon <node-name>

# 2. Evict the node's Pods. MacVz runs no DaemonСets itself, but the cluster's
#    may tolerate the taint; --ignore-daemonsets skips them. --delete-emptydir-data
#    is required if any Pod uses an emptyDir (MacVz emptyDirs are node-local and
#    do not survive the move).
kubectl drain <node-name> \
  --ignore-daemonsets \
  --delete-emptydir-data

# 3. (decommissioning) once drained, remove the node object.
kubectl delete node <node-name>
```

`kubectl drain` blocks until every evictable Pod is gone. Use `--grace-period` to
bound shutdown and `--force` only for unmanaged (bare) Pods, which are deleted
without rescheduling.

### Expected rescheduling behavior

What happens to the evicted Pods is the **owning controller's** job, not MacVz's:

- **Deployment / ReplicaSet / StatefulSet / Job** Pods are recreated by their
  controller and rescheduled onto another node that tolerates the MacVz taint
  (another MacVz node, or wherever the workload tolerates running). MacVz's
  `restartPolicy: Always`/`OnFailure` handling governs in-place container
  restarts on a *live* node (see [docs/WORKLOADS.md](WORKLOADS.md)); eviction is
  a Pod *deletion*, so rescheduling is driven by the controller, not the restart
  loop.
- **Bare Pods** (no controller) are not recreated — drain deletes them only with
  `--force`, and they do not come back.
- If no other node tolerates the taint, rescheduled Pods stay `Pending` until
  one does. This is expected: cordon/drain deliberately removes this node as a
  scheduling target.

## Verifying and recovering with `cleanup`

`macvz-kubelet cleanup` is the belt-and-suspenders pass: it finds MacVz
micro-VMs (`macvz-*`) with no backing Pod on this node and reaps them, then
flushes the pf anchor. Run it after a drain to confirm nothing leaked, or after
an abrupt kubelet exit (crash, `kill -9`, power loss) that skipped the graceful
teardown.

```sh
# Verify only — list orphans, change nothing.
macvz-kubelet cleanup --config /etc/macvz/config.yaml --dry-run

# Reap orphans whose Pods are gone (consults the API to spare live Pods).
macvz-kubelet cleanup --config /etc/macvz/config.yaml

# Node already drained AND removed from the cluster: skip the API and reap every
# MacVz VM this node created.
macvz-kubelet cleanup --config /etc/macvz/config.yaml --all
```

Behavior and safety:

- **Default** consults the API server for the Pods still assigned to this node
  and reaps only VMs with no live Pod. It **refuses to guess** if the API is
  unreachable (so it never reaps a VM that still backs a running Pod) — use
  `--all` for an already-removed node.
- `--dry-run` reports what it would reap and changes nothing.
- `--flush-anchor` (default on) flushes the `macvz/pods` pf anchor via the
  privileged helper when `privilegedHelperSocket` is set, otherwise via `pfctl`
  directly (run with `sudo` in that case). It is a no-op when the Pod network is
  not enabled.
- Reaping is best-effort and idempotent: a VM the runtime already lost counts as
  reaped, and one stuck VM does not block cleaning the rest.

## Cleanup verification checklist

After drain + cleanup, confirm no residue (matches the two-node bundle's Cleanup
section in [test/e2e/two-node/README.md](../test/e2e/two-node/README.md)):

```sh
container list --all                       # expect no macvz-* entries
sudo pfctl -a macvz/pods -s all            # expect empty (no rdr/binat rules)
netstat -rn -f inet | grep utun7           # expect no stale remote Pod CIDR routes
```

If `container list` still shows `macvz-*` VMs, re-run `cleanup` (add `--all` once
the node is removed from the cluster). If the pf anchor still holds rules, the
kubelet was killed without flushing — `cleanup --flush-anchor` (or
`sudo pfctl -a macvz/pods -F all`) clears them.
