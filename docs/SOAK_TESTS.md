# Long-duration soak tests (#71)

Issue #71 is the P9 live reliability gate. It runs MacVz for an extended period
while workloads are created, Services are exercised, kubelet/helper services are
restarted, nodes churn, orphan cleanup is checked, and resource snapshots are
kept for comparison.

The harness lives at [`test/e2e/soak/run.sh`](../test/e2e/soak/run.sh). It is a
real-cluster test: it needs at least one registered MacVz node, and full coverage
needs operator hooks that can restart services on each Mac.

## Quick run

Cluster-only soak, useful when you have `kubectl` access but no SSH/service
control on the Macs:

```sh
MACVZ_SOAK_NODES=macvz-a,macvz-b \
  test/e2e/soak/run.sh --duration 2h --fixtures e2e
```

Full P9 soak with restart, cleanup, and host-state hooks:

```sh
export MACVZ_SOAK_NODES=macvz-a,macvz-b
export MACVZ_SOAK_FIXTURES=e2e,examples
export MACVZ_SOAK_DURATION=8h
export MACVZ_SOAK_REQUIRE_RESTARTS=1

# The command templates replace {node} with the Kubernetes node name. This
# dispatcher uses the two-node bundle's per-user LaunchAgent commands; adjust
# paths/SSH targets for your rig.
export MACVZ_SOAK_RESTART_KUBELET_CMD='case "{node}" in macvz-a) cd /path/to/macvz && test/e2e/two-node/run.sh agent-restart a ;; macvz-b) ssh mac-b "cd ~/macvz-two-node && ./run.sh agent-restart b" ;; esac'
export MACVZ_SOAK_STOP_KUBELET_CMD='case "{node}" in macvz-a) cd /path/to/macvz && test/e2e/two-node/run.sh agent-stop a ;; macvz-b) ssh mac-b "cd ~/macvz-two-node && ./run.sh agent-stop b" ;; esac'
export MACVZ_SOAK_START_KUBELET_CMD='case "{node}" in macvz-a) cd /path/to/macvz && KUBECONFIG=/path/to/kubeconfig test/e2e/two-node/run.sh agent-start a ;; macvz-b) ssh mac-b "cd ~/macvz-two-node && KUBECONFIG=~/macvz-two-node/generated/kubeconfig-tunnel.yaml ./run.sh agent-start b" ;; esac'
export MACVZ_SOAK_CLEANUP_CMD='case "{node}" in macvz-a) cd /path/to/macvz/test/e2e/two-node && macvz-kubelet cleanup --config macvz-a.yaml --dry-run ;; macvz-b) ssh mac-b "cd ~/macvz-two-node && ./bin/macvz-kubelet cleanup --config macvz-b.yaml --dry-run" ;; esac'
export MACVZ_SOAK_NODE_CMD='case "{node}" in macvz-a) bash -lc ;; macvz-b) ssh mac-b -- ;; esac'

test/e2e/soak/run.sh
```

Run the soak harness itself with a direct kubeconfig for the Kubernetes API.
The MacVz kubelet processes may use a `kubectl proxy` kubeconfig or SSH tunnel
to reach the API server, but the harness exercises `kubectl exec` and
`kubectl port-forward`; those streaming upgrades are rejected by `kubectl proxy`.

If you installed MacVz as the packaged service instead of using the two-node
bundle, point the hooks at that LaunchAgent/LaunchDaemon label. If your node
names are not DNS/SSH hostnames, use SSH config aliases or wrap the command in a
small dispatcher script as shown above.

`MACVZ_SOAK_NAMESPACE_PREFIX` defaults to `macvz-soak`. Keep it lowercase
DNS-label safe and 40 characters or shorter; the harness derives per-iteration
namespaces and the expected orphan workload ID from it.

## What It Covers

Every iteration writes logs and samples under `MACVZ_SOAK_OUT_DIR` (or a temp
directory printed at startup). The root of that directory also contains
`results.tsv` (one PASS/FAIL/SKIP row per phase per iteration) and `summary.txt`
for quick triage after a long run. `summary.txt` is also written from the exit
trap when the harness is interrupted after preflight, so partial runs still leave
a readable index.

| Check | Mechanism |
| --- | --- |
| Workload lifecycle and Service reachability | Reuses `test/e2e/e2e.sh` with a per-iteration namespace |
| Real app validation under repetition | Optional `MACVZ_SOAK_FIXTURES=e2e,examples` runs `test/examples/run-all.sh` with per-iteration namespace overrides for every fixture |
| Kubelet restart recovery (#66) | Keeps a Pod running, restarts kubelet, waits for Ready, and requires the Pod IP to stay stable |
| Helper/kubelet restart churn | Runs the configured restart hooks on every node, then waits for all nodes Ready |
| Node churn | Cordon/uncordon one MacVz node per iteration and verifies it remains Ready |
| Orphan cleanup (#67) | With stop/start/node hooks, deletes a Pod while kubelet is stopped, restarts kubelet, waits for the grace window, and checks that the workload ID is gone from `container list --all` |
| Operator cleanup (#57) | Optional cleanup dry-run hook fails if it would reap orphan VMs |
| Resource boundedness (#68) | Captures node capacity/allocatable, Summary API stats, active Pods, events, and optional host `container list`/`df` before and after each iteration |
| Packaging lifecycle (#70) | Keep `make install-rehearsal` as the no-root rehearsal; run a live install/upgrade/rollback before or after the soak window |

The script is intentionally hook-based for host operations. The repo can verify
the harness syntax in CI, but only a real MacVz rig can safely restart launchd
services and inspect apple/container state.

## Acceptance

A #71 run is acceptable when:

- `test/e2e/soak/run.sh` exits 0 for the chosen duration or iteration count;
- all configured MacVz nodes are Ready after every restart/churn phase;
- the restart-recovery phase keeps the same Pod IP across kubelet restart;
- the orphan phase either reaps the downtime orphan automatically or proves it
  absent after the grace window;
- cleanup dry-run reports no remaining orphan MacVz VMs;
- resource snapshots do not show unbounded growth in active MacVz workloads,
  image cache, or node filesystem usage outside the expected image-pull cache;
- `make install-rehearsal` is green, and one live install/upgrade/rollback/
  uninstall pass has been recorded for the same artifact family.

Record the run in a dated report under `docs/`; start from
[SOAK_TEST_REPORT_TEMPLATE.md](SOAK_TEST_REPORT_TEMPLATE.md). Include:

- date, Mac models, RAM, macOS version, `apple/container` version, MacVz commit;
- node names, Pod CIDRs, mesh endpoints, and Kubernetes version;
- harness command/environment, duration, iterations, and fixture set;
- output directory or archived logs, including `summary.txt` and `results.tsv`;
- any failed iteration, captured diagnostics, and follow-up issue IDs.
