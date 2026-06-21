// Package orphan implements the automatic, in-kubelet orphan micro-VM cleanup
// policy (#67): a periodic reaper that detects MacVz micro-VMs whose backing Pod
// no longer exists on this node and destroys them, reclaiming the CPU, RAM, and
// disk a leaked VM would otherwise pin.
//
// It complements two sibling mechanisms:
//
//   - kubelet restart recovery (#66) re-adopts micro-VMs whose Pod still exists,
//     so a restart does not rebuild healthy workloads. This package handles the
//     opposite case: a VM whose Pod is gone.
//   - the operator-run `macvz-kubelet cleanup` command and node-removal flow
//     (#57/#58) are a one-shot, operator-initiated sweep — typically after a
//     drain — that also flushes the pf anchor. This package is the continuous,
//     unattended counterpart that runs for the life of the kubelet.
//
// Safety is the design priority: reaping a VM that still backs a live Pod would
// take down a running workload. Two guards prevent that:
//
//  1. The set of "expected" workload IDs is sourced authoritatively from the
//     Pods Kubernetes still assigns to this node. A VM is a candidate orphan only
//     when no live Pod maps to it.
//  2. A candidate must remain orphaned across a grace period before it is reaped,
//     absorbing the brief windows where a VM exists but its Pod is not yet
//     visible (informer lag right after creation, mid-adoption after a restart).
//
// When it cannot determine the expected set (e.g. the lister errors), it reaps
// nothing that tick — it never guesses.
package orphan

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/chimerakang/macvz/pkg/drain"
	"k8s.io/klog/v2"
)

// ExpectedFunc returns the set of runtime workload IDs that legitimately back
// live Pods on this node. Anything MacVz-created and absent from this set is a
// candidate orphan. It returns an error when the truth cannot be established, in
// which case the reaper skips the tick rather than risk reaping a live VM.
type ExpectedFunc func(ctx context.Context) (map[string]bool, error)

// Policy tunes the reaper's cadence and safety margin.
type Policy struct {
	// Interval is how often the reaper scans for orphans.
	Interval time.Duration
	// GracePeriod is how long a VM must remain continuously orphaned before it is
	// reaped, measured from when the reaper first observed it as an orphan.
	GracePeriod time.Duration
	// DryRun reports what would be reaped without destroying anything.
	DryRun bool
}

// Reaper periodically reaps orphan MacVz micro-VMs under a grace policy.
type Reaper struct {
	cleaner  *drain.Cleaner
	expected ExpectedFunc
	policy   Policy
	now      func() time.Time

	mu sync.Mutex
	// firstSeen records when each currently-orphaned VM was first observed as an
	// orphan, so the grace period is measured against wall-clock time rather than
	// a tick count (robust to interval jitter and missed ticks).
	firstSeen map[string]time.Time
	last      Result
}

// Result is the outcome of one reap pass, retained for diagnostics.
type Result struct {
	At      time.Time
	Scanned int      // MacVz orphans observed this pass (pre-grace)
	Pending int      // orphans seen but still within the grace period
	Reaped  []string // workload IDs reaped (or, under DryRun, that would be)
	DryRun  bool
	Err     error
}

// Option customizes a Reaper, chiefly for tests.
type Option func(*Reaper)

// WithClock injects the time source used for grace bookkeeping (tests use it to
// drive the grace period deterministically).
func WithClock(now func() time.Time) Option {
	return func(r *Reaper) { r.now = now }
}

// New builds a Reaper from a drain.Cleaner (which it reuses for the scan/reap
// primitives), an expected-set source, and a policy. The cleaner's Flusher must
// be nil for the in-kubelet reaper: flushing the whole pf anchor would tear down
// rules for the live Pods still on this node. Per-orphan pf cleanup is out of
// scope here (the reaper only knows VM IDs, not Pod keys); the operator cleanup
// command and node-removal flow own anchor flushing.
func New(cleaner *drain.Cleaner, expected ExpectedFunc, policy Policy, opts ...Option) *Reaper {
	r := &Reaper{
		cleaner:   cleaner,
		expected:  expected,
		policy:    policy,
		now:       time.Now,
		firstSeen: make(map[string]time.Time),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run scans immediately, then on every Interval tick, until ctx is cancelled.
func (r *Reaper) Run(ctx context.Context) {
	klog.InfoS("orphan micro-VM reaper started",
		"interval", r.policy.Interval, "gracePeriod", r.policy.GracePeriod, "dryRun", r.policy.DryRun)
	r.reapOnce(ctx)

	t := time.NewTicker(r.policy.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			klog.InfoS("orphan micro-VM reaper stopped")
			return
		case <-t.C:
			r.reapOnce(ctx)
		}
	}
}

// reapOnce performs a single detect-and-reap pass. It is safe to call directly
// in tests. The returned Result is also stored for LastResult.
func (r *Reaper) reapOnce(ctx context.Context) Result {
	now := r.now()
	res := Result{At: now, DryRun: r.policy.DryRun}

	// Establish the expected set first. If we cannot, reap nothing: we never guess
	// when the source of truth is unavailable. firstSeen is left intact so a
	// transient error does not reset the grace clock of a genuine orphan.
	expected, err := r.expected(ctx)
	if err != nil {
		res.Err = err
		klog.ErrorS(err, "orphan reaper: cannot determine live Pods; skipping this pass")
		r.store(res)
		return res
	}

	orphans, err := r.cleaner.Scan(ctx, expected)
	if err != nil {
		res.Err = err
		klog.ErrorS(err, "orphan reaper: scan failed; skipping this pass")
		r.store(res)
		return res
	}
	res.Scanned = len(orphans)

	r.mu.Lock()
	// Reset the grace clock for any VM no longer orphaned (reaped earlier, or its
	// Pod came back), so a recurrence starts its grace afresh.
	current := make(map[string]bool, len(orphans))
	for _, o := range orphans {
		current[o.ID] = true
	}
	for id := range r.firstSeen {
		if !current[id] {
			delete(r.firstSeen, id)
		}
	}
	// Partition current orphans into matured (past grace) and still-pending.
	var matured []drain.Orphan
	for _, o := range orphans {
		first, seen := r.firstSeen[o.ID]
		if !seen {
			r.firstSeen[o.ID] = now
			first = now
		}
		if now.Sub(first) >= r.policy.GracePeriod {
			matured = append(matured, o)
		} else {
			res.Pending++
		}
	}
	r.mu.Unlock()

	if len(matured) == 0 {
		if res.Pending > 0 {
			klog.V(2).InfoS("orphan reaper: candidates within grace period; not reaping yet",
				"pending", res.Pending, "gracePeriod", r.policy.GracePeriod)
		}
		r.store(res)
		return res
	}

	sort.Slice(matured, func(i, j int) bool { return matured[i].ID < matured[j].ID })
	reaped, rerr := r.cleaner.Reap(ctx, matured, r.policy.DryRun)
	res.Reaped = reaped
	res.Err = rerr

	if r.policy.DryRun {
		klog.InfoS("orphan reaper (dry run): would reap orphan micro-VMs",
			"count", len(reaped), "ids", reaped)
	} else {
		// Drop the grace record only for VMs actually reaped; a VM whose destroy
		// failed stays orphaned and is retried (still matured) next pass.
		r.mu.Lock()
		for _, id := range reaped {
			delete(r.firstSeen, id)
		}
		r.mu.Unlock()
		if len(reaped) > 0 {
			klog.InfoS("orphan reaper: reaped orphan micro-VMs", "count", len(reaped), "ids", reaped)
		}
		if rerr != nil {
			klog.ErrorS(rerr, "orphan reaper: some orphans could not be reaped; will retry next pass")
		}
	}

	r.store(res)
	return res
}

func (r *Reaper) store(res Result) {
	r.mu.Lock()
	r.last = res
	r.mu.Unlock()
}

// LastResult returns the outcome of the most recent reap pass, for diagnostics.
func (r *Reaper) LastResult() Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}
