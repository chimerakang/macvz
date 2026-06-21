package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// okProbes returns a probe set where every host interaction succeeds, so a test
// can override only the field it wants to exercise.
func okProbes() preflightProbes {
	return preflightProbes{
		lookPath:      func(string) (string, error) { return "/usr/local/bin/container", nil },
		stat:          func(string) (os.FileInfo, error) { return nil, nil },
		dirWritable:   func(string) error { return nil },
		socketServing: func(string) bool { return false },
		validateCIDR:  func(string) error { return nil },
	}
}

func statusFor(results []checkResult, name string) checkResult {
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	return checkResult{Name: name, Status: "MISSING"}
}

func TestPreflightChecksAllOK(t *testing.T) {
	cfg := preflightConfig{
		listen:   "unix:///tmp/macvz-cri.sock",
		stateDir: "/tmp/macvz-cri-state",
	}
	results := preflightChecks(cfg, okProbes())
	if got := worstStatus(results); got != checkOK {
		t.Fatalf("worstStatus = %s, want OK; results=%+v", got, results)
	}
	if r := statusFor(results, "apple/container CLI"); r.Status != checkOK {
		t.Errorf("runtime binary check = %s (%s)", r.Status, r.Detail)
	}
}

func TestPreflightRuntimeBinaryMissingFails(t *testing.T) {
	p := okProbes()
	p.lookPath = func(string) (string, error) { return "", fmt.Errorf("not found") }
	results := preflightChecks(preflightConfig{stateDir: "/tmp/s"}, p)
	r := statusFor(results, "apple/container CLI")
	if r.Status != checkFail {
		t.Fatalf("missing CLI should FAIL, got %s", r.Status)
	}
	if worstStatus(results) != checkFail {
		t.Errorf("report should be FAIL when CLI missing")
	}
}

func TestPreflightSocketAlreadyServingFails(t *testing.T) {
	p := okProbes()
	p.socketServing = func(string) bool { return true }
	results := preflightChecks(preflightConfig{listen: "unix:///tmp/x.sock"}, p)
	if r := statusFor(results, "CRI socket"); r.Status != checkFail {
		t.Fatalf("live socket should FAIL, got %s (%s)", r.Status, r.Detail)
	}
}

func TestPreflightSocketDirNotWritableFails(t *testing.T) {
	p := okProbes()
	p.dirWritable = func(string) error { return fmt.Errorf("permission denied") }
	results := preflightChecks(preflightConfig{listen: "unix:///nope/x.sock", stateDir: "/tmp/s"}, p)
	if r := statusFor(results, "CRI socket"); r.Status != checkFail {
		t.Fatalf("unwritable socket dir should FAIL, got %s", r.Status)
	}
}

func TestPreflightEmptyStateDirWarns(t *testing.T) {
	results := preflightChecks(preflightConfig{listen: "unix:///tmp/x.sock"}, okProbes())
	r := statusFor(results, "state dir")
	if r.Status != checkWarn {
		t.Fatalf("empty state dir should WARN, got %s", r.Status)
	}
	// A WARN must not fail the overall report.
	if worstStatus(results) == checkFail {
		t.Errorf("WARN-only report should not be FAIL")
	}
}

func TestPreflightPodNetwork(t *testing.T) {
	tests := []struct {
		name string
		pn   podNetConfig
		mut  func(*preflightProbes)
		want checkStatus
	}{
		{"off", podNetConfig{}, nil, checkOK},
		{"partial", podNetConfig{podCIDR: "10.0.0.0/24"}, nil, checkWarn},
		{
			name: "enabled no helper warns",
			pn:   podNetConfig{podCIDR: "10.0.0.0/24", iface: "bridge100"},
			want: checkWarn,
		},
		{
			name: "bad cidr fails",
			pn:   podNetConfig{podCIDR: "bogus", iface: "bridge100"},
			mut:  func(p *preflightProbes) { p.validateCIDR = func(string) error { return fmt.Errorf("bad cidr") } },
			want: checkFail,
		},
		{
			name: "enabled with helper ok",
			pn:   podNetConfig{podCIDR: "10.0.0.0/24", iface: "bridge100", helperSocket: "/var/run/macvz-netd.sock"},
			want: checkOK,
		},
		{
			name: "helper missing fails",
			pn:   podNetConfig{podCIDR: "10.0.0.0/24", iface: "bridge100", helperSocket: "/var/run/macvz-netd.sock"},
			mut:  func(p *preflightProbes) { p.stat = func(string) (os.FileInfo, error) { return nil, fmt.Errorf("no such file") } },
			want: checkFail,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := okProbes()
			if tt.mut != nil {
				tt.mut(&p)
			}
			r := checkPodNetwork(tt.pn, p)
			if r.Status != tt.want {
				t.Fatalf("checkPodNetwork(%+v) = %s (%s), want %s", tt.pn, r.Status, r.Detail, tt.want)
			}
		})
	}
}

func TestPreflightHostPathAllowlistNonAbsoluteFails(t *testing.T) {
	mc := mountConfig{hostPathAllowed: stringList{"relative/path"}}
	results := checkMounts(mc, okProbes())
	if r := statusFor(results, "hostPath allowlist"); r.Status != checkFail {
		t.Fatalf("non-absolute hostPath prefix should FAIL, got %s (%s)", r.Status, r.Detail)
	}
}

func TestPreflightKubeletPodsDirMissingWarns(t *testing.T) {
	p := okProbes()
	p.stat = func(string) (os.FileInfo, error) { return nil, fmt.Errorf("no such file") }
	results := checkMounts(mountConfig{}, p)
	if r := statusFor(results, "kubelet pods dir"); r.Status != checkWarn {
		t.Fatalf("missing kubelet pods dir should WARN, got %s", r.Status)
	}
}

func TestRenderPreflightReturnsFalseOnFail(t *testing.T) {
	var buf bytes.Buffer
	results := []checkResult{
		{Name: "apple/container CLI", Status: checkFail, Detail: "not found"},
		{Name: "state dir", Status: checkOK, Detail: "/tmp/s"},
	}
	ok := renderPreflight(&buf, results)
	if ok {
		t.Fatal("renderPreflight should return false when a check FAILs")
	}
	if !strings.Contains(buf.String(), "preflight: FAIL") {
		t.Errorf("report missing FAIL summary:\n%s", buf.String())
	}
}

func TestRenderPreflightReturnsTrueOnWarnOnly(t *testing.T) {
	var buf bytes.Buffer
	results := []checkResult{
		{Name: "apple/container CLI", Status: checkOK, Detail: "/bin/container"},
		{Name: "state dir", Status: checkWarn, Detail: "in-memory"},
	}
	if !renderPreflight(&buf, results) {
		t.Fatal("WARN-only report should return true")
	}
	if !strings.Contains(buf.String(), "preflight: OK") {
		t.Errorf("report missing OK summary:\n%s", buf.String())
	}
}

func TestSortedStatusesHelper(t *testing.T) {
	got := sortedStatuses([]checkResult{{Status: checkWarn}, {Status: checkOK}, {Status: checkFail}})
	want := []string{"FAIL", "OK", "WARN"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedStatuses = %v, want %v", got, want)
		}
	}
}
