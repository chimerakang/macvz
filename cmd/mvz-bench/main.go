// Command mvz-bench measures the headline value props of macvz on real Apple
// Silicon hardware: micro-VM density, per-VM RAM overhead, and boot latency.
//
// It drives the same runtime.Driver the kubelet uses, booting N Alpine
// micro-VMs (optionally probing the density ceiling), sampling host memory and
// per-VM helper RSS at steady state, and recording the boot-latency
// distribution. Results print as a table and, with -out, as JSON.
//
// Safety: the harness refuses to allocate more than -safety-fraction of total
// RAM (guest allocation × VM count), so a benchmark never OOMs the host. It
// always tears down every VM it created, even on error or interrupt.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/chimerakang/macvz/pkg/runtime/container"
)

func main() {
	var (
		image       string
		count       int
		memMiB      int
		cpus        int
		concurrency int
		binary      string
		keep        bool
		findCeiling bool
		safetyFrac  float64
		bootTimeout time.Duration
		outPath     string
	)
	flag.StringVar(&image, "image", "docker.io/library/alpine:3.20", "OCI image to boot")
	flag.IntVar(&count, "count", 10, "number of micro-VMs to launch")
	flag.IntVar(&memMiB, "mem", 256, "per-VM memory request, MiB")
	flag.IntVar(&cpus, "cpus", 0, "per-VM CPU request in milli-cores (0 = unset)")
	flag.IntVar(&concurrency, "concurrency", 4, "max VMs booting in parallel")
	flag.StringVar(&binary, "binary", "", "apple/container CLI (default: container)")
	flag.BoolVar(&keep, "keep", false, "do not destroy VMs after measuring")
	flag.BoolVar(&findCeiling, "find-ceiling", false, "keep launching past -count until failure to find the density ceiling")
	flag.Float64Var(&safetyFrac, "safety-fraction", 0.5, "never allocate more than this fraction of total RAM")
	flag.DurationVar(&bootTimeout, "boot-timeout", 60*time.Second, "max wait for a VM to reach Running")
	flag.StringVar(&outPath, "out", "", "write JSON results to this path")
	flag.Parse()

	if err := run(benchParams{
		image:       image,
		count:       count,
		memBytes:    int64(memMiB) * 1024 * 1024,
		cpuMillis:   int64(cpus),
		concurrency: concurrency,
		binary:      binary,
		keep:        keep,
		findCeiling: findCeiling,
		safetyFrac:  safetyFrac,
		bootTimeout: bootTimeout,
		outPath:     outPath,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "mvz-bench: %v\n", err)
		os.Exit(1)
	}
}

type benchParams struct {
	image       string
	count       int
	memBytes    int64
	cpuMillis   int64
	concurrency int
	binary      string
	keep        bool
	findCeiling bool
	safetyFrac  float64
	bootTimeout time.Duration
	outPath     string
}

// Results is the recorded benchmark output (also serialized to JSON).
type Results struct {
	Image            string       `json:"image"`
	TotalRAMBytes    int64        `json:"totalRAMBytes"`
	GuestMemBytes    int64        `json:"guestMemBytesPerVM"`
	Launched         int          `json:"launched"`
	CeilingReached   bool         `json:"ceilingReached"`
	CeilingReason    string       `json:"ceilingReason,omitempty"`
	BootLatencyMs    Distribution `json:"bootLatencyMs"`
	PerVMRSSBytes    Distribution `json:"perVMRSSBytes"`
	SysUsedDelta     int64        `json:"sysUsedDeltaBytes"`
	SysPerVMBytes    int64        `json:"sysPerVMBytes"`
	OverheadPerVMRSS int64        `json:"overheadPerVMRSSBytes"`
}

func run(p benchParams) error {
	// Interruptible context so Ctrl-C still triggers cleanup.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d := container.New(container.Config{Binary: p.binary})
	if err := d.Ready(ctx); err != nil {
		return fmt.Errorf("runtime not ready: %w", err)
	}

	totalRAM, err := hostTotalRAM()
	if err != nil {
		return fmt.Errorf("read host RAM: %w", err)
	}

	// Enforce the memory safety budget.
	maxByBudget := int(float64(totalRAM) * p.safetyFrac / float64(p.memBytes))
	if maxByBudget < 1 {
		return fmt.Errorf("guest memory %d MiB exceeds safety budget", p.memBytes/1024/1024)
	}
	target := p.count
	hardCap := p.count
	if p.findCeiling {
		hardCap = maxByBudget
	}
	if hardCap > maxByBudget {
		fmt.Printf("capping %d VMs to %d to stay within %.0f%% of %d GiB RAM\n",
			hardCap, maxByBudget, p.safetyFrac*100, totalRAM/1024/1024/1024)
		hardCap = maxByBudget
		if target > hardCap {
			target = hardCap
		}
	}

	fmt.Printf("warming image %s ...\n", p.image)
	if err := d.Pull(ctx, p.image, nil); err != nil {
		return fmt.Errorf("pull image: %w", err)
	}

	usedBefore, err := hostUsedRAM()
	if err != nil {
		return fmt.Errorf("sample memory: %w", err)
	}

	created := &createdSet{}
	defer cleanup(d, created, p.keep)

	res := Results{Image: p.image, TotalRAMBytes: totalRAM, GuestMemBytes: p.memBytes}
	var latencies []float64

	if p.findCeiling {
		fmt.Printf("probing density ceiling up to %d VMs (sequential)...\n", hardCap)
		latencies = launchToCeiling(ctx, d, p, hardCap, created, &res)
	} else {
		fmt.Printf("launching %d VMs (concurrency %d)...\n", target, p.concurrency)
		latencies = launchFixed(ctx, d, p, target, created, &res)
	}

	res.Launched = created.len()
	if res.Launched == 0 {
		return fmt.Errorf("no VMs reached Running; cannot measure")
	}
	res.BootLatencyMs = summarize(latencies)

	// Steady-state memory sampling.
	usedAfter, err := hostUsedRAM()
	if err != nil {
		return fmt.Errorf("sample memory: %w", err)
	}
	rss, err := vmmResidentSizes()
	if err != nil {
		return fmt.Errorf("sample VMM RSS: %w", err)
	}
	res.PerVMRSSBytes = summarize(toFloats(rss))
	res.SysUsedDelta = usedAfter - usedBefore
	if res.Launched > 0 {
		res.SysPerVMBytes = res.SysUsedDelta / int64(res.Launched)
		res.OverheadPerVMRSS = int64(res.PerVMRSSBytes.Mean)
	}

	printReport(res)
	if p.outPath != "" {
		if err := writeJSON(p.outPath, res); err != nil {
			return fmt.Errorf("write results: %w", err)
		}
		fmt.Printf("\nwrote JSON results to %s\n", p.outPath)
	}
	return nil
}

// launchFixed boots `target` VMs with bounded parallelism, recording boot
// latency for each that reaches Running.
func launchFixed(ctx context.Context, d *container.Driver, p benchParams, target int, created *createdSet, res *Results) []float64 {
	var (
		mu        sync.Mutex
		latencies []float64
		sem       = make(chan struct{}, p.concurrency)
		wg        sync.WaitGroup
	)
	for i := 0; i < target; i++ {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			lat, err := bootOne(ctx, d, p, i, created)
			if err != nil {
				mu.Lock()
				if res.CeilingReason == "" {
					res.CeilingReason = err.Error()
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			latencies = append(latencies, lat)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	return latencies
}

// launchToCeiling boots VMs sequentially until one fails (resource exhaustion),
// marking the ceiling.
func launchToCeiling(ctx context.Context, d *container.Driver, p benchParams, hardCap int, created *createdSet, res *Results) []float64 {
	var latencies []float64
	for i := 0; i < hardCap; i++ {
		if ctx.Err() != nil {
			res.CeilingReason = "interrupted"
			break
		}
		lat, err := bootOne(ctx, d, p, i, created)
		if err != nil {
			res.CeilingReached = true
			res.CeilingReason = err.Error()
			break
		}
		latencies = append(latencies, lat)
		if (i+1)%5 == 0 {
			fmt.Printf("  %d VMs running...\n", i+1)
		}
	}
	return latencies
}

// bootOne creates and starts a single VM, returning its boot latency in ms
// (Create through Running).
func bootOne(ctx context.Context, d *container.Driver, p benchParams, idx int, created *createdSet) (float64, error) {
	spec := types.ContainerSpec{
		Name:        fmt.Sprintf("mvz-bench-%d", idx),
		Image:       p.image,
		Command:     []string{"sleep", "3600"},
		MemoryBytes: p.memBytes,
		CPUMillis:   p.cpuMillis,
	}
	t0 := time.Now()
	id, err := d.Create(ctx, spec)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", spec.Name, err)
	}
	created.add(id)
	if err := d.Start(ctx, id); err != nil {
		return 0, fmt.Errorf("start %s: %w", id, err)
	}

	deadline := time.Now().Add(p.bootTimeout)
	for {
		st, err := d.Status(ctx, id)
		if err == nil && st.Phase == runtime.PhaseRunning && st.IP != "" {
			return float64(time.Since(t0).Microseconds()) / 1000, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("%s did not reach Running within %s", id, p.bootTimeout)
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// createdSet tracks created VM IDs for guaranteed teardown.
type createdSet struct {
	mu  sync.Mutex
	ids []string
}

func (c *createdSet) add(id string) {
	c.mu.Lock()
	c.ids = append(c.ids, id)
	c.mu.Unlock()
}

func (c *createdSet) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ids)
}

func (c *createdSet) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.ids))
	copy(out, c.ids)
	return out
}

// cleanup destroys every VM created during the run (unless -keep), using a
// fresh context so teardown proceeds even after the run context is cancelled.
func cleanup(d *container.Driver, created *createdSet, keep bool) {
	ids := created.snapshot()
	if keep {
		fmt.Printf("keeping %d VMs (-keep); destroy with: container delete --all --force\n", len(ids))
		return
	}
	if len(ids) == 0 {
		return
	}
	fmt.Printf("destroying %d VMs...\n", len(ids))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := d.Destroy(ctx, id); err != nil && !errors.Is(err, runtime.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "  destroy %s: %v\n", id, err)
			}
		}(id)
	}
	wg.Wait()
}

func writeJSON(path string, res Results) error {
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func printReport(r Results) {
	mib := func(b float64) float64 { return b / 1024 / 1024 }
	fmt.Printf("\n=== macvz density & RAM benchmark ===\n")
	fmt.Printf("image                 %s\n", r.Image)
	fmt.Printf("host RAM              %.1f GiB\n", float64(r.TotalRAMBytes)/1024/1024/1024)
	fmt.Printf("guest mem / VM        %.0f MiB\n", mib(float64(r.GuestMemBytes)))
	fmt.Printf("VMs launched          %d\n", r.Launched)
	if r.CeilingReached {
		fmt.Printf("density ceiling       %d VMs (next failed: %s)\n", r.Launched, r.CeilingReason)
	} else if r.CeilingReason != "" {
		fmt.Printf("note                  some VMs failed: %s\n", r.CeilingReason)
	}
	fmt.Printf("\nboot latency (Create→Running), ms:\n")
	fmt.Printf("  min %.0f  p50 %.0f  p90 %.0f  p99 %.0f  max %.0f  mean %.0f  (n=%d)\n",
		r.BootLatencyMs.Min, r.BootLatencyMs.P50, r.BootLatencyMs.P90,
		r.BootLatencyMs.P99, r.BootLatencyMs.Max, r.BootLatencyMs.Mean, r.BootLatencyMs.N)
	fmt.Printf("\nper-VM host RAM overhead (container-runtime-linux RSS), MiB:\n")
	fmt.Printf("  min %.1f  p50 %.1f  p90 %.1f  max %.1f  mean %.1f  (n=%d)\n",
		mib(r.PerVMRSSBytes.Min), mib(r.PerVMRSSBytes.P50), mib(r.PerVMRSSBytes.P90),
		mib(r.PerVMRSSBytes.Max), mib(r.PerVMRSSBytes.Mean), r.PerVMRSSBytes.N)
	fmt.Printf("\nsystem used-memory delta   %.0f MiB total, %.1f MiB / VM\n",
		mib(float64(r.SysUsedDelta)), mib(float64(r.SysPerVMBytes)))
}
