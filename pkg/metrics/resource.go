package metrics

import (
	"context"
	"time"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

// ResourceMetrics builds the Prometheus resource-metrics families served at
// /metrics/resource and scraped by metrics-server's default mode. It reports
// the standard kubelet resource metric names so metrics-server parses it
// without bespoke configuration:
//
//	node_cpu_usage_seconds_total, node_memory_working_set_bytes,
//	pod_cpu_usage_seconds_total, pod_memory_working_set_bytes,
//	container_cpu_usage_seconds_total, container_memory_working_set_bytes
//
// plus MacVz-specific gauges (macvz_node_pods, macvz_runtime_ready) that other
// consumers ignore. CPU counters are cumulative core-seconds; memory is the
// live working set in bytes. Workloads the runtime cannot sample contribute no
// CPU/memory series.
func (c *Collector) ResourceMetrics(ctx context.Context, pods []PodInput, statsFn StatsFunc, diskFn DiskFunc, runtimeReady bool) []*dto.MetricFamily {
	now := time.Now()
	s := c.gather(ctx, now, pods, statsFn, diskFn)
	ts := now.UnixMilli()

	var (
		nodeCPU   = counterFamily("node_cpu_usage_seconds_total", "Cumulative CPU usage of the node's MacVz workloads in core-seconds.")
		nodeMem   = gaugeFamily("node_memory_working_set_bytes", "Current working-set memory of the node in bytes.")
		podCPU    = counterFamily("pod_cpu_usage_seconds_total", "Cumulative CPU usage of the Pod in core-seconds.")
		podMem    = gaugeFamily("pod_memory_working_set_bytes", "Current working-set memory of the Pod in bytes.")
		ctrCPU    = counterFamily("container_cpu_usage_seconds_total", "Cumulative CPU usage of the container in core-seconds.")
		ctrMem    = gaugeFamily("container_memory_working_set_bytes", "Current working-set memory of the container in bytes.")
		podsGauge = gaugeFamily("macvz_node_pods", "Number of Pods tracked on this MacVz node.")
		readyG    = gaugeFamily("macvz_runtime_ready", "Whether the apple/container runtime is ready (1) or not (0).")
		fsCap     = gaugeFamily("macvz_node_filesystem_capacity_bytes", "Total size of the filesystem backing MacVz micro-VM and image storage, in bytes.")
		fsUsed    = gaugeFamily("macvz_node_filesystem_used_bytes", "Used bytes of the filesystem backing MacVz micro-VM and image storage.")
		fsAvail   = gaugeFamily("macvz_node_filesystem_available_bytes", "Bytes available to MacVz on the filesystem backing micro-VM and image storage.")
		imgBytes  = gaugeFamily("macvz_image_cache_bytes", "Disk consumed by locally cached OCI images in bytes (sum of per-image sizes).")
		imgCount  = gaugeFamily("macvz_image_cache_images", "Number of OCI images in the local cache.")
	)

	nodeCPU.Metric = append(nodeCPU.Metric, counter(coreSeconds(s.nodeCPUNs), ts))
	if s.memOK {
		nodeMem.Metric = append(nodeMem.Metric, gauge(float64(s.memUsed), ts))
	}

	if s.disk.NodeFSOK {
		fsCap.Metric = append(fsCap.Metric, gauge(float64(s.disk.NodeFS.TotalBytes), ts))
		fsUsed.Metric = append(fsUsed.Metric, gauge(float64(s.disk.NodeFS.UsedBytes), ts))
		fsAvail.Metric = append(fsAvail.Metric, gauge(float64(s.disk.NodeFS.AvailableBytes), ts))
	}
	if s.disk.ImagesOK {
		imgBytes.Metric = append(imgBytes.Metric, gauge(float64(s.disk.Images.TotalBytes), ts))
		imgCount.Metric = append(imgCount.Metric, gauge(float64(s.disk.Images.Count), ts))
	}

	for _, ps := range s.pods {
		podLabels := []*dto.LabelPair{label("pod", ps.ref.Name), label("namespace", ps.ref.Namespace)}
		podCPU.Metric = append(podCPU.Metric, counter(coreSeconds(ps.cpuNs), ts, podLabels...))
		podMem.Metric = append(podMem.Metric, gauge(float64(ps.memWS), ts, podLabels...))

		for _, cs := range ps.containers {
			if !cs.ok {
				continue
			}
			cl := []*dto.LabelPair{label("container", cs.name), label("pod", ps.ref.Name), label("namespace", ps.ref.Namespace)}
			ctrCPU.Metric = append(ctrCPU.Metric, counter(coreSeconds(cs.stats.CPUUsageCoreNanoSeconds), ts, cl...))
			ctrMem.Metric = append(ctrMem.Metric, gauge(float64(cs.stats.MemoryUsageBytes), ts, cl...))
		}
	}

	podsGauge.Metric = append(podsGauge.Metric, gauge(float64(len(s.pods)), ts))
	readyG.Metric = append(readyG.Metric, gauge(boolToFloat(runtimeReady), ts))

	families := []*dto.MetricFamily{nodeCPU, nodeMem, podCPU, podMem, ctrCPU, ctrMem, podsGauge, readyG, fsCap, fsUsed, fsAvail, imgBytes, imgCount}
	// metrics-server tolerates empty families, but dropping them keeps the
	// exposition clean when no Pods are scheduled.
	out := families[:0]
	for _, f := range families {
		if len(f.Metric) > 0 {
			out = append(out, f)
		}
	}
	return out
}

// coreSeconds converts cumulative CPU core-nanoseconds to core-seconds.
func coreSeconds(coreNanoSeconds uint64) float64 {
	return float64(coreNanoSeconds) / 1e9
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func counterFamily(name, help string) *dto.MetricFamily {
	return &dto.MetricFamily{Name: proto.String(name), Help: proto.String(help), Type: dto.MetricType_COUNTER.Enum()}
}

func gaugeFamily(name, help string) *dto.MetricFamily {
	return &dto.MetricFamily{Name: proto.String(name), Help: proto.String(help), Type: dto.MetricType_GAUGE.Enum()}
}

func counter(value float64, tsMs int64, labels ...*dto.LabelPair) *dto.Metric {
	return &dto.Metric{Label: labels, Counter: &dto.Counter{Value: proto.Float64(value)}, TimestampMs: proto.Int64(tsMs)}
}

func gauge(value float64, tsMs int64, labels ...*dto.LabelPair) *dto.Metric {
	return &dto.Metric{Label: labels, Gauge: &dto.Gauge{Value: proto.Float64(value)}, TimestampMs: proto.Int64(tsMs)}
}

func label(name, value string) *dto.LabelPair {
	return &dto.LabelPair{Name: proto.String(name), Value: proto.String(value)}
}
