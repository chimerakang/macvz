# Node & Pod Metrics

MacVz reports node and workload resource usage to Kubernetes so operators can
observe capacity, usage, and runtime health. This is the operator-facing
acceptance path for issue #25 (**P4 — Hardening & Beta**).

Metrics are served by the same HTTPS kubelet server that backs `kubectl
logs`/`exec`, so they are available whenever serving TLS is configured (see
[P2_SMOKE_TEST.md](P2_SMOKE_TEST.md) for `servingTLSCertFile`/`KeyFile` and the
kubelet port). With no serving certificate the endpoints are unavailable, the
same as logs/exec.

## Endpoints

| Endpoint | Format | Consumed by |
| --- | --- | --- |
| `GET /stats/summary` | kubelet Summary JSON (`stats.k8s.io/v1alpha1`) | `kubectl top` (legacy), older metrics-server scrape modes |
| `GET /metrics/resource` | Prometheus text | metrics-server default scrape mode |

Both draw from the same source each scrape: per-workload CPU and memory come
from `apple/container` via `container stats`; node memory comes from the host;
node disk and image-cache usage come from `statfs` on the runtime's data root
and `container image ls` (#68).

## Metric names & units

### Summary API (`/stats/summary`)

Standard kubelet [`Summary`](https://pkg.go.dev/k8s.io/kubelet/pkg/apis/stats/v1alpha1#Summary)
schema. MacVz populates:

- **Node** — `nodeName`; `memory.workingSetBytes` / `usageBytes` (host
  in-use RAM) and `memory.availableBytes` (total − used); `cpu.usageCoreNanoSeconds`
  (cumulative, aggregated across the node's workloads) and, once a second sample
  exists, `cpu.usageNanoCores` (the derived rate).
- **Pods** — `podRef` (name/namespace/uid); aggregated `cpu` and `memory` over
  the Pod's containers.
- **Containers** — per-container `cpu` (`usageCoreNanoSeconds`, plus
  `usageNanoCores` after the first sample) and `memory` (`workingSetBytes` /
  `usageBytes`, and `availableBytes` when the VM has a memory limit).
- **Node disk** (#68) — `fs` reports the filesystem backing micro-VM and image
  storage (`capacityBytes`, `usedBytes`, `availableBytes`, and inode counts),
  which is what disk-pressure eviction reads; `runtime.imageFs.usedBytes`
  reports the bytes consumed by the local image cache on that same filesystem.

Units follow the schema: CPU is core-nanoseconds (cumulative) and nanocores
(rate); memory and disk are bytes.

### Resource metrics (`/metrics/resource`)

Prometheus families using the canonical kubelet names so metrics-server parses
them without extra configuration:

| Metric | Type | Unit | Labels |
| --- | --- | --- | --- |
| `node_cpu_usage_seconds_total` | counter | CPU core-seconds | — |
| `node_memory_working_set_bytes` | gauge | bytes | — |
| `pod_cpu_usage_seconds_total` | counter | CPU core-seconds | `pod`, `namespace` |
| `pod_memory_working_set_bytes` | gauge | bytes | `pod`, `namespace` |
| `container_cpu_usage_seconds_total` | counter | CPU core-seconds | `container`, `pod`, `namespace` |
| `container_memory_working_set_bytes` | gauge | bytes | `container`, `pod`, `namespace` |
| `macvz_node_pods` | gauge | count | — |
| `macvz_runtime_ready` | gauge | 0/1 | — |
| `macvz_node_filesystem_capacity_bytes` | gauge | bytes | — |
| `macvz_node_filesystem_used_bytes` | gauge | bytes | — |
| `macvz_node_filesystem_available_bytes` | gauge | bytes | — |
| `macvz_image_cache_bytes` | gauge | bytes | — |
| `macvz_image_cache_images` | gauge | count | — |

The `macvz_*` families are MacVz-specific; metrics-server ignores unrecognized
families. The disk and image-cache gauges complement the Summary API's `fs`
stats for operators scraping Prometheus directly. The standard kubelet resource
endpoint defines no per-Pod disk series, so disk is reported only at node scope.

## Inspecting metrics

```sh
# Summary API (raw kubelet stats)
kubectl get --raw "/api/v1/nodes/<node>/proxy/stats/summary" | jq .

# Prometheus resource metrics
kubectl get --raw "/api/v1/nodes/<node>/proxy/metrics/resource"

# With metrics-server installed:
kubectl top node <node>
kubectl top pod -n <namespace>
```

## Known limitations

- **Per-workload stats require a running micro-VM.** A Pod that is pending,
  stopped, or whose VM the runtime cannot sample is reported without CPU/memory
  rather than failing the whole scrape (graceful degradation). Containers with
  no backing workload appear with a name only.
- **Node CPU is the aggregate of the node's workloads**, not whole-host CPU.
  MacVz is a virtual node whose purpose is hosting micro-VM Pods, so node CPU
  reflects what those Pods consume. The host's non-workload CPU is not counted.
  Because Pods come and go, the node CPU counter is not strictly monotonic;
  metrics-server treats a decrease as a counter reset.
- **`usageNanoCores` needs two samples.** The first scrape after a workload (or
  the kubelet) starts reports only the cumulative counter; the rate appears on
  the next scrape.
- **Node memory is macOS-only.** It is derived from `sysctl hw.memsize` and
  `vm_stat` (active + wired + compressed pages, matching the density benchmark).
  On non-macOS builds the node entry omits memory.
- **Disk is node-scoped, not per-Pod.** MacVz reports the data-root filesystem
  and image-cache size at node scope (#68). It does not yet attribute ephemeral
  storage to individual Pods/containers, so the Summary API omits per-Pod
  `ephemeral-storage` usage. Image-cache bytes are the sum of `container image
  ls` per-image sizes — an upper bound when images share layers.
- **No network-per-pod or PSI stats** are reported yet.

## Node ephemeral-storage capacity

When the runtime can report disk, MacVz advertises the node's
`ephemeral-storage` resource (#68): capacity from the data-root filesystem's
total size and allocatable from its available bytes, so the scheduler accounts
for disk. An explicit `ephemeral-storage` in the node config is left untouched
(operator intent wins). The sampled filesystem is `runtimeDataRoot`; when it is
empty, MacVz falls back to the operator's home directory — the standard
per-user volume where `apple/container` stores micro-VM and image data.
