package container

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// concurrencyRunner records, per workload ID, the peak number of operations
// in flight at once, so a test can assert the Driver's per-ID serialization.
type concurrencyRunner struct {
	mu           sync.Mutex
	active       map[string]int
	peak         map[string]int
	globalActive int
	globalPeak   int
	hold         time.Duration
}

func newConcurrencyRunner(hold time.Duration) *concurrencyRunner {
	return &concurrencyRunner{active: map[string]int{}, peak: map[string]int{}, hold: hold}
}

// id extracts the workload ID, which the Driver always passes as the last arg
// for start/stop/delete.
func (c *concurrencyRunner) enter(args []string) func() {
	id := args[len(args)-1]
	c.mu.Lock()
	c.active[id]++
	if c.active[id] > c.peak[id] {
		c.peak[id] = c.active[id]
	}
	c.globalActive++
	if c.globalActive > c.globalPeak {
		c.globalPeak = c.globalActive
	}
	c.mu.Unlock()

	time.Sleep(c.hold)

	return func() {
		c.mu.Lock()
		c.active[id]--
		c.globalActive--
		c.mu.Unlock()
	}
}

func (c *concurrencyRunner) output(_ context.Context, args ...string) ([]byte, error) {
	defer c.enter(args)()
	return nil, nil
}
func (c *concurrencyRunner) run(_ context.Context, _ streams, args ...string) error {
	defer c.enter(args)()
	return nil
}
func (c *concurrencyRunner) pipe(_ context.Context, args ...string) (io.ReadCloser, error) {
	defer c.enter(args)()
	return io.NopCloser(nil), nil
}

func TestSameWorkloadSerializes(t *testing.T) {
	c := newConcurrencyRunner(20 * time.Millisecond)
	d := &Driver{run: c}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.Start(context.Background(), "pod-x")
		}()
	}
	wg.Wait()

	if c.peak["pod-x"] != 1 {
		t.Errorf("peak concurrency for one workload = %d, want 1 (ops must serialize)", c.peak["pod-x"])
	}
}

func TestDistinctWorkloadsRunConcurrently(t *testing.T) {
	c := newConcurrencyRunner(40 * time.Millisecond)
	d := &Driver{run: c}

	ids := []string{"a", "b", "c", "d"}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_ = d.Start(context.Background(), id)
		}(id)
	}
	wg.Wait()

	// Each distinct ID still peaks at 1 (its own lock), but distinct IDs must
	// overlap: the global peak should exceed 1, proving no cross-ID blocking.
	for _, id := range ids {
		if c.peak[id] != 1 {
			t.Errorf("peak[%s] = %d, want 1", id, c.peak[id])
		}
	}
	if c.globalPeak < 2 {
		t.Errorf("global peak = %d, want >1 (distinct workloads must run concurrently)", c.globalPeak)
	}
}
