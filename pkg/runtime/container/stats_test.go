package container

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// realStatsJSON is a sample `container stats --format json` payload, matching
// the shape apple/container emits for a running micro-VM.
const realStatsJSON = `[{"blockReadBytes":1744896,"blockWriteBytes":0,"cpuUsageUsec":2945,"id":"pod-x","memoryLimitBytes":1073741824,"memoryUsageBytes":2002944,"networkRxBytes":14886,"networkTxBytes":516,"numProcesses":1}]`

func TestStatsParsesAndConvertsUnits(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"stats": []byte(realStatsJSON)}}
	rs, err := driverWith(f).Stats(context.Background(), "pod-x")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got, want := rs.CPUUsageCoreNanoSeconds, uint64(2945*1000); got != want {
		t.Errorf("CPU core-nanoseconds = %d, want %d (usec*1000)", got, want)
	}
	if got, want := rs.MemoryUsageBytes, uint64(2002944); got != want {
		t.Errorf("MemoryUsageBytes = %d, want %d", got, want)
	}
	if got, want := rs.MemoryLimitBytes, uint64(1073741824); got != want {
		t.Errorf("MemoryLimitBytes = %d, want %d", got, want)
	}
	if rs.NetworkRxBytes != 14886 || rs.NetworkTxBytes != 516 {
		t.Errorf("network = rx %d tx %d, want 14886/516", rs.NetworkRxBytes, rs.NetworkTxBytes)
	}
	if rs.Timestamp.IsZero() {
		t.Error("Timestamp not set")
	}
}

func TestStatsBuildsNoStreamJSONArgs(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"stats": []byte(realStatsJSON)}}
	if _, err := driverWith(f).Stats(context.Background(), "pod-x"); err != nil {
		t.Fatalf("Stats: %v", err)
	}
	args := lastCall(f)
	if !argsContain(args, "stats", "--no-stream", "--format", "json", "pod-x") {
		t.Errorf("stats args = %v, want --no-stream --format json for pod-x", args)
	}
}

func TestStatsPicksMatchingID(t *testing.T) {
	multi := `[{"id":"other","cpuUsageUsec":1},{"id":"pod-x","cpuUsageUsec":7}]`
	f := &fakeRunner{outputs: map[string][]byte{"stats": []byte(multi)}}
	rs, err := driverWith(f).Stats(context.Background(), "pod-x")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if rs.CPUUsageCoreNanoSeconds != 7000 {
		t.Errorf("picked wrong entry: CPU = %d, want 7000", rs.CPUUsageCoreNanoSeconds)
	}
}

func TestStatsUnavailableForMissingOrStopped(t *testing.T) {
	cases := map[string]error{
		"not found":   &CommandError{Stderr: "Error: container not found", ExitCode: 1},
		"not running": &CommandError{Stderr: "Error: container not running", ExitCode: 1},
	}
	for name, runErr := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeRunner{errs: map[string]error{"stats": runErr}}
			_, err := driverWith(f).Stats(context.Background(), "pod-x")
			if !errors.Is(err, runtime.ErrStatsUnavailable) {
				t.Errorf("err = %v, want ErrStatsUnavailable", err)
			}
		})
	}
}

func TestStatsEmptyArrayIsUnavailable(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"stats": []byte("[]")}}
	_, err := driverWith(f).Stats(context.Background(), "pod-x")
	if !errors.Is(err, runtime.ErrStatsUnavailable) {
		t.Errorf("err = %v, want ErrStatsUnavailable for empty stats", err)
	}
}

func TestParseStatsTimestamp(t *testing.T) {
	at := time.Unix(1700000000, 0)
	rs, err := parseStats("pod-x", []byte(realStatsJSON), at)
	if err != nil {
		t.Fatalf("parseStats: %v", err)
	}
	if !rs.Timestamp.Equal(at) {
		t.Errorf("Timestamp = %v, want %v", rs.Timestamp, at)
	}
}
