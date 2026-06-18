package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
)

var _ runtime.Stater = (*Driver)(nil)

// statsResult is the subset of `container stats --format json` the driver
// consumes. apple/container reports cumulative CPU in microseconds and live
// memory/network counters in bytes.
type statsResult struct {
	ID               string `json:"id"`
	CPUUsageUsec     uint64 `json:"cpuUsageUsec"`
	MemoryUsageBytes uint64 `json:"memoryUsageBytes"`
	MemoryLimitBytes uint64 `json:"memoryLimitBytes"`
	NetworkRxBytes   uint64 `json:"networkRxBytes"`
	NetworkTxBytes   uint64 `json:"networkTxBytes"`
}

// Stats returns a resource-usage sample for the workload by querying
// `container stats`. A workload that is missing or not running has no
// sampleable metrics, so those map to runtime.ErrStatsUnavailable rather than
// surfacing as a hard error to the metrics layer.
func (d *Driver) Stats(ctx context.Context, id string) (runtime.ResourceStats, error) {
	out, err := d.run.output(ctx, "stats", "--no-stream", "--format", "json", id)
	if err != nil {
		mapped := mapErr(err)
		if errors.Is(mapped, runtime.ErrNotFound) || errors.Is(mapped, runtime.ErrNotRunning) {
			return runtime.ResourceStats{}, fmt.Errorf("stats %q: %w", id, runtime.ErrStatsUnavailable)
		}
		return runtime.ResourceStats{}, fmt.Errorf("stats %q: %w", id, mapped)
	}
	return parseStats(id, out, time.Now())
}

// parseStats maps a `container stats` payload to runtime.ResourceStats. The CLI
// returns an array; the entry matching id is used, falling back to the sole
// entry when the id field is absent. An empty array means the workload reported
// no stats and is treated as unavailable.
func parseStats(id string, out []byte, now time.Time) (runtime.ResourceStats, error) {
	var results []statsResult
	if err := json.Unmarshal(out, &results); err != nil {
		return runtime.ResourceStats{}, fmt.Errorf("stats %q: parse stats output: %w", id, err)
	}
	if len(results) == 0 {
		return runtime.ResourceStats{}, fmt.Errorf("stats %q: %w", id, runtime.ErrStatsUnavailable)
	}

	r := results[0]
	for i := range results {
		if results[i].ID == id {
			r = results[i]
			break
		}
	}

	return runtime.ResourceStats{
		Timestamp: now,
		// apple/container reports cumulative CPU in microseconds.
		CPUUsageCoreNanoSeconds: r.CPUUsageUsec * 1000,
		MemoryUsageBytes:        r.MemoryUsageBytes,
		MemoryLimitBytes:        r.MemoryLimitBytes,
		NetworkRxBytes:          r.NetworkRxBytes,
		NetworkTxBytes:          r.NetworkTxBytes,
	}, nil
}
