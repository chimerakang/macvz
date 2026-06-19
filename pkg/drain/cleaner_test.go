package drain

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/provider"
	"github.com/chimerakang/macvz/pkg/runtime"
)

type fakeRuntime struct {
	list      []runtime.Status
	listErr   error
	stopped   []string
	destroyed []string
	stopErr   map[string]error
	destErr   map[string]error
}

func (f *fakeRuntime) List(context.Context) ([]runtime.Status, error) { return f.list, f.listErr }
func (f *fakeRuntime) Stop(_ context.Context, id string, _ time.Duration) error {
	f.stopped = append(f.stopped, id)
	if f.stopErr != nil {
		return f.stopErr[id]
	}
	return nil
}
func (f *fakeRuntime) Destroy(_ context.Context, id string) error {
	f.destroyed = append(f.destroyed, id)
	if f.destErr != nil {
		return f.destErr[id]
	}
	return nil
}

type fakeFlusher struct {
	called int
	err    error
}

func (f *fakeFlusher) FlushAnchor(context.Context) error { f.called++; return f.err }

// id builds a real MacVz workload ID so the prefix matching is exercised exactly
// as production names it.
func id(ns, pod, c string) string { return provider.WorkloadID(ns, pod, c) }

func TestScanFindsOrphansByPrefix(t *testing.T) {
	mine := id("default", "web", "app")
	other := id("kube-system", "dns", "coredns")
	rt := &fakeRuntime{list: []runtime.Status{
		{ID: mine, Phase: runtime.PhaseRunning},
		{ID: other, Phase: runtime.PhaseRunning},
		{ID: "some-other-tool-vm", Phase: runtime.PhaseRunning}, // not MacVz
	}}
	c := &Cleaner{Lister: rt, Reaper: rt}

	// Expected set keeps `mine`; everything else MacVz is an orphan.
	orphans, err := c.Scan(context.Background(), map[string]bool{mine: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orphans) != 1 || orphans[0].ID != other {
		t.Fatalf("expected only %s orphaned, got %+v", other, orphans)
	}
}

func TestScanEmptyExpectedTreatsAllMacVzAsOrphan(t *testing.T) {
	a, b := id("default", "a", "c"), id("default", "b", "c")
	rt := &fakeRuntime{list: []runtime.Status{
		{ID: a}, {ID: b}, {ID: "foreign"},
	}}
	c := &Cleaner{Lister: rt, Reaper: rt}
	orphans, err := c.Scan(context.Background(), nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orphans) != 2 {
		t.Fatalf("expected 2 orphans, got %+v", orphans)
	}
	// Sorted, and never includes the non-MacVz workload.
	if orphans[0].ID != a || orphans[1].ID != b {
		t.Errorf("unexpected/unsorted orphans: %+v", orphans)
	}
}

func TestReapStopsDestroysAndFlushes(t *testing.T) {
	a := id("default", "a", "c")
	rt := &fakeRuntime{}
	fl := &fakeFlusher{}
	c := &Cleaner{Lister: rt, Reaper: rt, Flusher: fl}

	reaped, err := c.Reap(context.Background(), []Orphan{{ID: a}}, false)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != a {
		t.Errorf("expected %s reaped, got %v", a, reaped)
	}
	if len(rt.stopped) != 1 || len(rt.destroyed) != 1 {
		t.Errorf("expected stop+destroy, stopped=%v destroyed=%v", rt.stopped, rt.destroyed)
	}
	if fl.called != 1 {
		t.Errorf("expected anchor flush once, got %d", fl.called)
	}
}

func TestReapDryRunMutatesNothing(t *testing.T) {
	a := id("default", "a", "c")
	rt := &fakeRuntime{}
	fl := &fakeFlusher{}
	c := &Cleaner{Lister: rt, Reaper: rt, Flusher: fl}

	reaped, err := c.Reap(context.Background(), []Orphan{{ID: a}}, true)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(reaped) != 1 {
		t.Errorf("dry-run should still report what it would reap, got %v", reaped)
	}
	if len(rt.stopped) != 0 || len(rt.destroyed) != 0 || fl.called != 0 {
		t.Errorf("dry-run must not mutate: stopped=%v destroyed=%v flush=%d", rt.stopped, rt.destroyed, fl.called)
	}
}

func TestReapContinuesPastFailureAndJoinsErrors(t *testing.T) {
	a, b := id("default", "a", "c"), id("default", "b", "c")
	rt := &fakeRuntime{destErr: map[string]error{a: errors.New("boom")}}
	c := &Cleaner{Lister: rt, Reaper: rt}

	reaped, err := c.Reap(context.Background(), []Orphan{{ID: a}, {ID: b}}, false)
	if err == nil {
		t.Fatalf("expected error from failed destroy")
	}
	// b still reaped despite a failing.
	if len(reaped) != 1 || reaped[0] != b {
		t.Errorf("expected %s reaped after %s failed, got %v", b, a, reaped)
	}
	// Both were attempted (destroy called for both).
	if len(rt.destroyed) != 2 {
		t.Errorf("expected destroy attempted for both, got %v", rt.destroyed)
	}
}

func TestReapToleratesNotFound(t *testing.T) {
	a := id("default", "a", "c")
	rt := &fakeRuntime{
		stopErr: map[string]error{a: runtime.ErrNotFound},
		destErr: map[string]error{a: runtime.ErrNotFound},
	}
	c := &Cleaner{Lister: rt, Reaper: rt}
	reaped, err := c.Reap(context.Background(), []Orphan{{ID: a}}, false)
	if err != nil {
		t.Fatalf("not-found should be tolerated, got %v", err)
	}
	if len(reaped) != 1 {
		t.Errorf("a VM the runtime already lost counts as reaped, got %v", reaped)
	}
}
