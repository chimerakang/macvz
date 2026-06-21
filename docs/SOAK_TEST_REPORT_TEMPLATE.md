# Soak Test Report - YYYY-MM-DD

Use this template when recording issue #71 long-duration runs. Keep the filled
report under `docs/` with the date in the filename.

## Build Under Test

| Item | Value |
| --- | --- |
| Repository | `macvz` |
| Commit |  |
| Date |  |
| MacVz version |  |
| Kubernetes version |  |
| `apple/container` version |  |
| Test image | `busybox:1.36.1` |

## Hosts

| Node | Host | Mac model | RAM | macOS | Mesh addr | Pod CIDR |
| --- | --- | --- | --- | --- | --- | --- |
| `macvz-a` |  |  |  |  |  |  |
| `macvz-b` |  |  |  |  |  |  |

## Harness

```sh
# command and environment used for the run
```

| Item | Value |
| --- | --- |
| Duration target |  |
| Actual duration |  |
| Iterations |  |
| Fixtures |  |
| Output directory/archive |  |

## Results

| Area | Result | Evidence |
| --- | --- | --- |
| e2e workload lifecycle + Service |  |  |
| P8 real-app catalog |  |  |
| kubelet restart recovery |  |  |
| helper/kubelet restart churn |  |  |
| node cordon/uncordon churn |  |  |
| orphan cleanup after downtime |  |  |
| cleanup dry-run |  |  |
| resource boundedness |  |  |
| install/upgrade/rollback/uninstall |  |  |

## Resource Snapshots

Summarize first/last iteration snapshots from `iteration-*/before` and
`iteration-*/after`.

| Metric | Start | End | Notes |
| --- | --- | --- | --- |
| MacVz workloads (`container list --all`) |  |  |  |
| Node filesystem used |  |  |  |
| Summary API node fs used |  |  |  |
| Summary API image fs used |  |  |  |
| Active Pods on MacVz nodes |  |  |  |

## Failures And Follow-ups

- None.

## Cleanup

- Namespaces removed:
- `container list --all` on each Mac:
- pf anchor state:
- WireGuard routes/interface state:
