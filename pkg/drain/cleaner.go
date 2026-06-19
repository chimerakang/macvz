// Package drain implements safe node-removal cleanup for MacVz (#57): finding
// and reaping orphan micro-VMs and stale pf rules left behind after a node is
// drained or after an abrupt kubelet exit.
//
// The normal path needs none of this — when Pods are evicted by `kubectl drain`,
// Virtual Kubelet calls the provider's DeletePod, which stops/destroys each
// micro-VM, detaches its pod-network rules, and releases its IP. This package is
// the belt-and-suspenders pass an operator runs to *verify* a node came away
// clean, and to recover when the kubelet was killed before it could.
package drain

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chimerakang/macvz/pkg/provider"
	"github.com/chimerakang/macvz/pkg/runtime"
)

// VMLister enumerates every workload the host runtime knows about, including
// stopped ones. Satisfied by *container.Driver (runtime.Lister).
type VMLister interface {
	List(ctx context.Context) ([]runtime.Status, error)
}

// VMReaper stops and destroys a workload. Satisfied by *container.Driver.
type VMReaper interface {
	Stop(ctx context.Context, id string, timeout time.Duration) error
	Destroy(ctx context.Context, id string) error
}

// AnchorFlusher removes every MacVz rule from the pf anchor. It is the same
// `pfctl -a <anchor> -F all` the kubelet runs on shutdown, exposed so cleanup
// can repeat it when the kubelet exited without flushing.
type AnchorFlusher interface {
	FlushAnchor(ctx context.Context) error
}

// Orphan is a MacVz micro-VM on the host with no backing Pod on this node.
type Orphan struct {
	ID    string
	Phase runtime.Phase
}

// Cleaner reaps orphan MacVz micro-VMs and optionally flushes the pf anchor.
type Cleaner struct {
	Lister  VMLister
	Reaper  VMReaper
	Flusher AnchorFlusher // nil to skip pf flushing
	// Timeout bounds the graceful stop of each VM before it is force-destroyed.
	Timeout time.Duration
}

// vmPrefix is the workload-name prefix every MacVz micro-VM carries. Matching on
// it ensures cleanup only ever touches VMs this node created, never other
// apple/container workloads on the host.
var vmPrefix = provider.WorkloadPrefix + "-"

// IsMacVzVM reports whether a workload ID was created by MacVz.
func IsMacVzVM(id string) bool { return strings.HasPrefix(id, vmPrefix) }

// Scan returns the MacVz micro-VMs on the host that are not in expected.
//
// expected holds the workload IDs that legitimately back live Pods still
// assigned to this node (see provider.WorkloadID). After a full drain the node
// has no Pods, so an empty set makes every MacVz VM an orphan. Passing a
// populated set lets cleanup run safely while some Pods remain (it never reaps a
// VM that still backs a live Pod).
func (c *Cleaner) Scan(ctx context.Context, expected map[string]bool) ([]Orphan, error) {
	all, err := c.Lister.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workloads: %w", err)
	}
	var orphans []Orphan
	for _, w := range all {
		if !IsMacVzVM(w.ID) || expected[w.ID] {
			continue
		}
		orphans = append(orphans, Orphan{ID: w.ID, Phase: w.Phase})
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].ID < orphans[j].ID })
	return orphans, nil
}

// Reap stops and destroys the given orphans, then flushes the pf anchor. It is
// best-effort: a failure on one VM is collected and reaping continues, so a
// single stuck VM never blocks cleaning the rest. With dryRun it mutates nothing
// and reports what it would have reaped.
//
// It returns the IDs actually reaped (or that would be, under dryRun) and a
// joined error of every failure encountered.
func (c *Cleaner) Reap(ctx context.Context, orphans []Orphan, dryRun bool) (reaped []string, err error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	var errs []error
	for _, o := range orphans {
		if dryRun {
			reaped = append(reaped, o.ID)
			continue
		}
		// Stop is idempotent and tolerates an already-stopped VM; only Destroy
		// failure means the VM may linger, so a Stop error is folded in but does
		// not skip the Destroy.
		if serr := c.Reaper.Stop(ctx, o.ID, timeout); serr != nil && !errors.Is(serr, runtime.ErrNotFound) {
			errs = append(errs, fmt.Errorf("stop %s: %w", o.ID, serr))
		}
		if derr := c.Reaper.Destroy(ctx, o.ID); derr != nil && !errors.Is(derr, runtime.ErrNotFound) {
			errs = append(errs, fmt.Errorf("destroy %s: %w", o.ID, derr))
			continue
		}
		reaped = append(reaped, o.ID)
	}

	if c.Flusher != nil && !dryRun {
		if ferr := c.Flusher.FlushAnchor(ctx); ferr != nil {
			errs = append(errs, fmt.Errorf("flush pf anchor: %w", ferr))
		}
	}
	return reaped, errors.Join(errs...)
}
