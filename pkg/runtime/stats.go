package runtime

import (
	"context"
	"errors"
	"time"
)

// ErrStatsUnavailable means the runtime cannot report resource usage for the
// workload right now (e.g. it is not running, or the host runtime exposes no
// stats for it). It is distinct from ErrNotFound: the workload may exist but
// have no sampleable metrics. The metrics layer treats it as "skip this
// workload" so a single unobservable VM never fails a whole stats scrape.
var ErrStatsUnavailable = errors.New("runtime: workload stats unavailable")

// ResourceStats is a point-in-time resource-usage sample for one workload's
// micro-VM. CPU is a cumulative counter since the VM started; callers derive
// rate metrics (CPU nanocores) by differencing successive samples. Memory and
// IO/network fields are the live values at Timestamp.
type ResourceStats struct {
	// Timestamp is when the sample was taken.
	Timestamp time.Time
	// CPUUsageCoreNanoSeconds is cumulative CPU time across all cores, in
	// nanoseconds, since the VM started.
	CPUUsageCoreNanoSeconds uint64
	// MemoryUsageBytes is the VM's current memory use.
	MemoryUsageBytes uint64
	// MemoryLimitBytes is the VM's memory ceiling; 0 when unlimited or unknown.
	MemoryLimitBytes uint64
	// NetworkRxBytes and NetworkTxBytes are cumulative bytes over the VM's
	// interfaces since it started.
	NetworkRxBytes uint64
	NetworkTxBytes uint64
}

// Stater is an optional Runtime capability that reports per-workload resource
// usage. A Runtime that cannot sample stats simply does not implement it; the
// metrics layer then degrades gracefully and reports only what it can observe.
type Stater interface {
	// Stats returns a resource-usage sample for the workload, or
	// ErrStatsUnavailable when no sample can be taken (wrapped, comparable with
	// errors.Is).
	Stats(ctx context.Context, id string) (ResourceStats, error)
}
