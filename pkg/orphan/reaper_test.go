package orphan

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/drain"
	"github.com/chimerakang/macvz/pkg/provider"
	"github.com/chimerakang/macvz/pkg/runtime"
)

// fakeRuntime is a List/Stop/Destroy stub satisfying drain.VMLister + VMReaper.
type fakeRuntime struct {
	mu        sync.Mutex
	list      []runtime.Status
	listErr   error
	stopped   []string
	destroyed []string
	destErr   map[string]error
}

func (f *fakeRuntime) List(context.Context) ([]runtime.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.Status(nil), f.list...), f.listErr
}

func (f *fakeRuntime) Stop(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return nil
}

func (f *fakeRuntime) Destroy(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed = append(f.destroyed, id)
	if f.destErr != nil {
		return f.destErr[id]
	}
	// Model a successful destroy by removing the VM from the host listing, so the
	// next scan no longer sees it.
	out := f.list[:0]
	for _, s := range f.list {
		if s.ID != id {
			out = append(out, s)
		}
	}
	f.list = out
	return nil
}

func (f *fakeRuntime) destroyedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.destroyed...)
}

// wid builds a real MacVz workload ID so prefix matching is exercised exactly as
// production names it.
func wid(ns, pod, c string) string { return provider.WorkloadID(ns, pod, c) }

func expectSet(ids ...string) ExpectedFunc {
	return func(context.Context) (map[string]bool, error) {
		m := map[string]bool{}
		for _, id := range ids {
			m[id] = true
		}
		return m, nil
	}
}

// clock is a manually-advanced time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newReaper(rt *fakeRuntime, expected ExpectedFunc, grace time.Duration, dryRun bool, clk *clock) *Reaper {
	cleaner := &drain.Cleaner{Lister: rt, Reaper: rt}
	return New(cleaner, expected, Policy{Interval: time.Minute, GracePeriod: grace, DryRun: dryRun}, WithClock(clk.now))
}

func TestReaperReapsOnlyAfterGrace(t *testing.T) {
	orphanID := wid("default", "gone", "app")
	live := wid("default", "web", "app")
	rt := &fakeRuntime{list: []runtime.Status{
		{ID: orphanID, Phase: runtime.PhaseRunning},
		{ID: live, Phase: runtime.PhaseRunning},
		{ID: "not-macvz-vm", Phase: runtime.PhaseRunning},
	}}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	r := newReaper(rt, expectSet(live), 10*time.Minute, false, clk)

	// First pass: orphan observed, but within grace -> nothing reaped.
	res := r.reapOnce(context.Background())
	if res.Scanned != 1 || res.Pending != 1 || len(res.Reaped) != 0 {
		t.Fatalf("first pass: scanned=%d pending=%d reaped=%v, want 1/1/none", res.Scanned, res.Pending, res.Reaped)
	}
	if len(rt.destroyedIDs()) != 0 {
		t.Fatalf("destroyed within grace: %v", rt.destroyedIDs())
	}

	// Advance past grace: the orphan matures and is reaped; the live VM and the
	// non-MacVz VM are never touched.
	clk.advance(11 * time.Minute)
	res = r.reapOnce(context.Background())
	if got := rt.destroyedIDs(); len(got) != 1 || got[0] != orphanID {
		t.Fatalf("after grace destroyed=%v, want [%s]", got, orphanID)
	}
	if len(res.Reaped) != 1 || res.Reaped[0] != orphanID {
		t.Fatalf("result reaped=%v, want [%s]", res.Reaped, orphanID)
	}
}

func TestReaperResetsGraceWhenPodReturns(t *testing.T) {
	orphanID := wid("default", "flap", "app")
	rt := &fakeRuntime{list: []runtime.Status{{ID: orphanID, Phase: runtime.PhaseRunning}}}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}

	// Expected set toggles: first the VM is an orphan, then its Pod "returns".
	var hasPod bool
	expected := func(context.Context) (map[string]bool, error) {
		if hasPod {
			return map[string]bool{orphanID: true}, nil
		}
		return map[string]bool{}, nil
	}
	r := newReaper(rt, expected, 10*time.Minute, false, clk)

	r.reapOnce(context.Background()) // t0: orphan seen, pending
	clk.advance(5 * time.Minute)
	hasPod = true
	r.reapOnce(context.Background()) // t+5: no longer orphan -> grace cleared
	clk.advance(1 * time.Minute)
	hasPod = false
	res := r.reapOnce(context.Background()) // t+6: orphan again, grace restarts
	if res.Pending != 1 || len(res.Reaped) != 0 {
		t.Fatalf("grace should have reset: pending=%d reaped=%v", res.Pending, res.Reaped)
	}

	// Only after a fresh full grace from the recurrence is it reaped.
	clk.advance(11 * time.Minute)
	r.reapOnce(context.Background())
	if got := rt.destroyedIDs(); len(got) != 1 || got[0] != orphanID {
		t.Fatalf("destroyed=%v, want [%s] after fresh grace", got, orphanID)
	}
}

func TestReaperDryRunDestroysNothing(t *testing.T) {
	orphanID := wid("default", "gone", "app")
	rt := &fakeRuntime{list: []runtime.Status{{ID: orphanID, Phase: runtime.PhaseRunning}}}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	r := newReaper(rt, expectSet(), 1*time.Minute, true, clk)

	clk.advance(2 * time.Minute) // already matured (firstSeen set on this pass == now, so not matured yet)
	r.reapOnce(context.Background())
	clk.advance(2 * time.Minute)
	res := r.reapOnce(context.Background())

	if len(rt.destroyedIDs()) != 0 {
		t.Fatalf("dry run destroyed VMs: %v", rt.destroyedIDs())
	}
	if len(res.Reaped) != 1 || res.Reaped[0] != orphanID || !res.DryRun {
		t.Fatalf("dry run should report would-reap: reaped=%v dryRun=%v", res.Reaped, res.DryRun)
	}
}

func TestReaperSkipsWhenExpectedUnavailable(t *testing.T) {
	orphanID := wid("default", "gone", "app")
	rt := &fakeRuntime{list: []runtime.Status{{ID: orphanID, Phase: runtime.PhaseRunning}}}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	wantErr := errors.New("informer not synced")
	expected := func(context.Context) (map[string]bool, error) { return nil, wantErr }
	r := newReaper(rt, expected, 0, false, clk)

	res := r.reapOnce(context.Background())
	if !errors.Is(res.Err, wantErr) {
		t.Fatalf("res.Err = %v, want %v", res.Err, wantErr)
	}
	if len(rt.destroyedIDs()) != 0 {
		t.Fatalf("reaped despite unknown expected set: %v", rt.destroyedIDs())
	}
}

func TestReaperRetriesFailedDestroy(t *testing.T) {
	orphanID := wid("default", "stuck", "app")
	rt := &fakeRuntime{
		list:    []runtime.Status{{ID: orphanID, Phase: runtime.PhaseRunning}},
		destErr: map[string]error{orphanID: errors.New("destroy boom")},
	}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	r := newReaper(rt, expectSet(), 1*time.Minute, false, clk)

	r.reapOnce(context.Background()) // observe (pending)
	clk.advance(2 * time.Minute)
	res := r.reapOnce(context.Background()) // matured, destroy fails
	if res.Err == nil {
		t.Fatal("expected a reap error when destroy fails")
	}
	// firstSeen retained -> next pass still matured and retried.
	res = r.reapOnce(context.Background())
	if res.Err == nil {
		t.Fatal("failed orphan should be retried on the next pass")
	}
	if got := rt.destroyedIDs(); len(got) < 2 {
		t.Fatalf("expected repeated destroy attempts, got %v", got)
	}
}

func TestReaperNoOrphansIsClean(t *testing.T) {
	live := wid("default", "web", "app")
	rt := &fakeRuntime{list: []runtime.Status{{ID: live, Phase: runtime.PhaseRunning}}}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	r := newReaper(rt, expectSet(live), time.Minute, false, clk)

	res := r.reapOnce(context.Background())
	if res.Scanned != 0 || res.Pending != 0 || len(res.Reaped) != 0 {
		t.Fatalf("clean node should reap nothing: %+v", res)
	}
}
