# CRI-L7-1 Long-running LinuxPod CRI Soak After Supervisor Adoption (#140)

Date: 2026-06-27
Outcome: **PASS** for the 60-iteration acceptance path, including follow-up
`netd` policy-reload churn.

## Summary

This run extended the post-#139 LinuxPod CRI validation from a short smoke to a
60-iteration kubelet/k3s churn soak on the real test topology. The run exercised
the genuine non-simulated LinuxPod backend on `test@192.168.1.122` through:

- 20 Deployment rollout churns;
- 20 `macvz-cri` restarts;
- 20 public `linuxpod-helper` router restarts.

All checks passed. Every helper-router restart recovered live through the
supervisor-backed adoption path (`adoptedPods=1`, `lostPods=0`) and kept the same
Kubernetes Pod UID. Cleanup left zero residual LinuxPod VM/container/rootfs/
handoff/network state. The remote default route stayed `192.168.1.1` via `en0`
for the entire run.

The first run used the 60-iteration alternate acceptance path without `netd`
because no standardized non-sudo `MACVZ_RESTART_NETD_CMD` hook existed yet. A
follow-up run added `test/e2e/cri-k3s/hooks/netd-reload-policy.sh` and repeated
the 60-iteration soak with `rollout,cri,helper,netd`; it passed with 15 `netd`
policy reloads, 15/15 route guards, and 15/15 reachability checks.

## Environment

- Local repo: commit `3872514 Record LinuxPod adoption validation`
- Control plane: `kind-macvz61`
- MacVz CRI node: `macvz-b-cri`
- Remote Mac: `test@192.168.1.122`
- Runtime path: `macvz-cri --experimental-linuxpod-backend`
- Helper protocol: version 6, `simulated=false`
- Pod CIDR: `10.244.102.0/24`
- Remote default route before/after: `192.168.1.1` via `en0`

Diagnostics:

- Soak output: `/tmp/macvz-live-140-soak-20260627220753/run`
- Full log: `/tmp/macvz-live-140-soak-20260627220753/soak-live.log`
- Samples: `/tmp/macvz-live-140-soak-20260627220753/run/soak-samples.csv`
- Netd follow-up output: `/tmp/macvz-live-140-soak-netd-20260627225201/run`
- Netd follow-up log: `/tmp/macvz-live-140-soak-netd-20260627225201/soak-live.log`
- Netd follow-up samples:
  `/tmp/macvz-live-140-soak-netd-20260627225201/run/soak-samples.csv`

## Command

```sh
MACVZ_INTEGRATION=1 \
KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}" \
MACVZ_NODE=macvz-b-cri \
MACVZ_CRI_OUT_DIR=/tmp/macvz-live-140-soak-20260627220753/run \
MACVZ_SOAK_ITERATIONS=60 \
MACVZ_SOAK_CHURN_MODES=rollout,cri,helper \
MACVZ_SOAK_RECOVER_INTERVAL=2 \
MACVZ_SOAK_RECOVER_TRIES=90 \
MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD=/tmp/macvz-live-139-inloop-20260627204244/backend-evidence.sh \
MACVZ_RESTART_CRI_CMD=/tmp/macvz-live-139-inloop-20260627204244/restart-cri.sh \
MACVZ_RESTART_HELPER_CMD=/tmp/macvz-live-139-inloop-20260627204244/restart-helper-router.sh \
MACVZ_ADAPTER_RSS_CMD=/tmp/macvz-live-139-inloop-20260627204244/rss.sh \
MACVZ_HELPER_RSS_CMD=/tmp/macvz-live-139-inloop-20260627204244/helper-rss.sh \
MACVZ_LINUXPOD_AUDIT_CMD=/tmp/macvz-live-139-inloop-20260627204244/audit.sh \
MACVZ_ROUTE_AUDIT_CMD=/tmp/macvz-live-139-inloop-20260627204244/route.sh \
bash test/e2e/cri-k3s/linuxpod-soak.sh
```

Follow-up netd-inclusive command:

```sh
MACVZ_INTEGRATION=1 \
KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}" \
MACVZ_NODE=macvz-b-cri \
MACVZ_CRI_OUT_DIR=/tmp/macvz-live-140-soak-netd-20260627225201/run \
MACVZ_SOAK_ITERATIONS=60 \
MACVZ_SOAK_CHURN_MODES=rollout,cri,helper,netd \
MACVZ_SOAK_RECOVER_INTERVAL=2 \
MACVZ_SOAK_RECOVER_TRIES=90 \
MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD=/tmp/macvz-live-139-inloop-20260627204244/backend-evidence.sh \
MACVZ_RESTART_CRI_CMD=/tmp/macvz-live-139-inloop-20260627204244/restart-cri.sh \
MACVZ_RESTART_HELPER_CMD=/tmp/macvz-live-139-inloop-20260627204244/restart-helper-router.sh \
MACVZ_NETD_SSH_TARGET=test@192.168.1.122 \
MACVZ_RESTART_NETD_CMD="$PWD/test/e2e/cri-k3s/hooks/netd-reload-policy.sh" \
MACVZ_ADAPTER_RSS_CMD=/tmp/macvz-live-139-inloop-20260627204244/rss.sh \
MACVZ_HELPER_RSS_CMD=/tmp/macvz-live-139-inloop-20260627204244/helper-rss.sh \
MACVZ_LINUXPOD_AUDIT_CMD=/tmp/macvz-live-139-inloop-20260627204244/audit.sh \
MACVZ_ROUTE_AUDIT_CMD=/tmp/macvz-live-139-inloop-20260627204244/route.sh \
bash test/e2e/cri-k3s/linuxpod-soak.sh
```

## Results

```text
PASS CRI-L6-1 LinuxPod soak: all checks passed over 60 iterations
```

Iteration summary:

| Metric | Value |
| --- | --- |
| Total iterations | 60 |
| Rollout churn | 20 |
| CRI restart churn | 20 |
| Helper-router restart churn | 20 |
| Max container restartCount | 0 |
| Route guard failures | 0 |
| Final cleanup residual | 0 |
| Adapter RSS min/max | 24144KB / 28752KB |
| Adapter RSS first/last growth | 26576KB -> 27216KB (`+640KB`) |
| Helper RSS min/max | 14800KB / 16176KB |
| Max per-iteration residual before GC convergence | 8 lines |
| Steady residual baseline after convergence | 7 lines |

Helper adoption summary:

| Metric | Value |
| --- | --- |
| Helper-router restarts | 20 |
| `adoptedPods` total | 20 |
| `lostPods` total | 0 |
| Helper recoveries with same Pod UID | 20/20 |

Follow-up netd-inclusive run:

```text
PASS CRI-L6-1 LinuxPod soak: all checks passed over 60 iterations
```

| Metric | Value |
| --- | --- |
| Total iterations | 60 |
| Rollout churn | 15 |
| CRI restart churn | 15 |
| Helper-router restart churn | 15 |
| Netd policy reload churn | 15 |
| Netd default-route guard passes | 15/15 |
| Netd reachability passes | 15/15 |
| Helper recoveries with same Pod UID | 15/15 |
| Max container restartCount | 0 |
| Route guard failures | 0 |
| Final cleanup residual | 0 |
| Adapter RSS min/max | 25872KB / 29600KB |
| Adapter RSS first/last growth | 26400KB -> 27776KB (`+1376KB`) |
| Helper RSS min/max | 14816KB / 16176KB |
| Max per-iteration residual before GC convergence | 8 lines |

## Notable Observations

- Rollout churn briefly produced 8 residual audit lines while kubelet still held
  the stopped sandbox JSON for the prior Pod. The soak harness now waits for CRI
  state to converge before treating residual count as duplicate backend state;
  CRI restart duplicate checks consistently returned to the 7-line steady-state
  baseline.
- `macvz-cri` restart preserved Pod UID in every CRI churn iteration.
- Helper-router restart used true #139 live adoption in every helper churn
  iteration. No helper restart fell back to kubelet recreate.
- Adapter RSS remained bounded and ended only 640KB above the first recorded
  sample.
- The remote route guard passed on every iteration and at final route-after,
  including all 15 `netd` policy reloads in the follow-up run.

## Final State

After cleanup:

- Kubernetes fixture namespace was gone.
- Remote LinuxPod state directory contained no JSON records.
- `supervisor-journal.json` was empty: `{"pods":{},"protocolVersion":6}`.
- No per-Pod supervisor process remained.
- Only the public `macvz-cri` and public helper router processes remained.
- Default route remained:

```text
gateway: 192.168.1.1
interface: en0
```

## Follow-ups

- Run an overnight or 2+ hour variant to add wall-clock evidence in addition to
  the iteration-count evidence.
