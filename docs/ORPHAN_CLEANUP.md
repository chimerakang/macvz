# Automatic orphan micro-VM cleanup (#67)

A MacVz micro-VM is an **orphan** when MacVz created it but no live Pod on this
node maps to it any longer. Orphans arise from the gaps the normal delete path
cannot cover:

- the kubelet is killed (crash, power loss, `kill -9`) after a Pod is removed but
  before `DeletePod` could destroy its micro-VM;
- a Pod is deleted from the API **while the kubelet is down**, so the kubelet
  never sees the delete to act on it;
- a `DeletePod` whose `Destroy` failed transiently and was never retried.

Each orphan keeps pinning the CPU, RAM, and disk of a running micro-VM for a
workload that no longer exists. The orphan reaper reclaims them automatically,
for the life of the kubelet, with no operator action.

It is the continuous counterpart to the one-shot, operator-run sweeps:

| Mechanism | Trigger | Scope |
| --- | --- | --- |
| **Restart recovery (#66)** | kubelet start | *re-adopts* VMs whose Pod still exists |
| **Orphan reaper (#67, this)** | periodic, always on | *destroys* VMs whose Pod is gone |
| **`macvz-kubelet cleanup` (#57)** | operator, after a drain/crash | one-shot reap **+ pf anchor flush** |
| **`macvz-kubelet remove` (#58)** | node decommission | full local teardown |

## How it decides what to reap

Every pass:

1. **Lists** every workload `apple/container` knows about on the host.
2. Keeps only **MacVz-created** VMs (those under the `macvz-` workload-ID prefix),
   so it can never touch another tool's workloads on the same Mac.
3. Builds the **expected set** — the workload IDs of every live, supported Pod
   Kubernetes still assigns to this node — from the node's own Pod informer
   cache (no extra API calls). A Pod with a deletion timestamp is excluded: its
   VM is on its way out and is fair game if it lingers. Unsupported beta shapes
   (init, ephemeral, or multi-container Pods) are also excluded because MacVz
   cannot legitimately back them with a workload.
4. A MacVz VM **not** in the expected set is a candidate orphan.

Two guards make a false reap (destroying a VM that still backs a live Pod)
practically impossible:

- **Grace period.** A candidate must stay continuously orphaned for the whole
  `gracePeriod` (measured from when the reaper first saw it as an orphan) before
  it is destroyed. This absorbs the brief windows where a VM exists but its Pod
  is not yet visible — informer lag just after creation, or mid-adoption right
  after a restart. If the Pod reappears in the expected set during the grace
  period, the clock resets.
- **Fail-safe on uncertainty.** If the expected set cannot be determined (the
  lister errors), the reaper reaps **nothing** that pass. It never guesses.

A VM whose `Destroy` fails stays orphaned and is retried on the next pass.

## What it does *not* do

The reaper destroys VMs; it does **not** flush the pf pod-network anchor. The
node is live, so a global anchor flush would drop rules for the Pods still
running here. Per-orphan pf cleanup is not possible from the reaper (it knows VM
IDs, not Pod keys). Anchor-level cleanup belongs to the operator
[`cleanup`](NODE_DRAIN.md) command (`--flush-anchor`) and to node
[removal](NODE_REMOVAL.md), which run when the node is being emptied.

## Configuration

```yaml
orphanCleanup:
  enabled: true      # turn the periodic reaper on (default)
  interval: 2m       # how often to scan
  gracePeriod: 10m   # how long a VM must stay orphaned before reaping (must be >= interval)
  dryRun: false      # log what would be reaped without destroying anything
```

- `gracePeriod` must comfortably exceed the time between a Pod being scheduled
  and its micro-VM appearing in this node's Pod cache. The 10-minute default is
  conservative; lower it only if you understand your scheduling/pull latency.
- Set `dryRun: true` first on an unfamiliar cluster: the reaper logs the orphan
  IDs it *would* reap (`orphan reaper (dry run): would reap orphan micro-VMs`)
  so you can confirm the policy before it destroys anything.
- `enabled: false` turns the loop off entirely; leaked VMs are then reclaimed
  only by the operator `cleanup` command or node removal.

## Observing it

The reaper logs at startup (`orphan micro-VM reaper started`) with its effective
policy, and on every reap (`orphan reaper: reaped orphan micro-VMs` with the
count and IDs). Candidates still inside the grace period are logged at `-v=2`.
Confirm a clean host with `container list --all` (expect no `macvz-*` entries for
Pods that no longer exist).
