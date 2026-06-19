package health

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// staticChecker returns a fixed Check, optionally recording that it ran.
type staticChecker struct {
	check Check
	ran   *bool
}

func (s staticChecker) Check(ctx context.Context) Check {
	if s.ran != nil {
		*s.ran = true
	}
	return s.check
}

func TestVerdictReadyWhenNoFailures(t *testing.T) {
	r := newReport("node-a", []Check{
		{Name: "container-runtime", Class: ClassRuntime, Status: StatusPass},
		{Name: "kubelet-registration", Class: ClassControlPlane, Status: StatusPass},
		{Name: "wireguard-mesh", Class: ClassDataPlane, Status: StatusSkipped},
	})
	if !r.Ready {
		t.Fatalf("expected ready, got not ready: %s", r.Reason)
	}
	if r.Reason != "ready for workloads" {
		t.Fatalf("unexpected reason: %q", r.Reason)
	}
}

func TestVerdictReadyWithWarningsSurfacesThem(t *testing.T) {
	r := newReport("node-a", []Check{
		{Name: "container-runtime", Class: ClassRuntime, Status: StatusPass},
		{Name: "wireguard-mesh", Class: ClassDataPlane, Status: StatusWarn},
	})
	if !r.Ready {
		t.Fatalf("warnings must not block readiness; reason=%s", r.Reason)
	}
	if !strings.Contains(r.Reason, "warnings") || !strings.Contains(r.Reason, "wireguard-mesh") {
		t.Fatalf("reason should surface the warning: %q", r.Reason)
	}
}

func TestVerdictNotReadyGroupsFailuresByClass(t *testing.T) {
	r := newReport("node-a", []Check{
		{Name: "container-runtime", Class: ClassRuntime, Status: StatusFail},
		{Name: "wireguard-mesh", Class: ClassDataPlane, Status: StatusFail},
		{Name: "ip-forwarding", Class: ClassDataPlane, Status: StatusFail},
		{Name: "kubelet-registration", Class: ClassControlPlane, Status: StatusPass},
	})
	if r.Ready {
		t.Fatal("expected not ready")
	}
	// Control plane has no failure; runtime then data-plane do. Order must follow
	// classOrder, and the data-plane failures must be grouped together.
	// Within a class, checks are sorted by name, so ip-forwarding precedes wireguard-mesh.
	want := "not ready for workloads: runtime[container-runtime], data-plane[ip-forwarding, wireguard-mesh]"
	if r.Reason != want {
		t.Fatalf("reason\n got: %q\nwant: %q", r.Reason, want)
	}
}

func TestNewReportSortsByClassThenName(t *testing.T) {
	r := newReport("node-a", []Check{
		{Name: "pod-network", Class: ClassDataPlane, Status: StatusPass},
		{Name: "node-lease", Class: ClassControlPlane, Status: StatusPass},
		{Name: "container-runtime", Class: ClassRuntime, Status: StatusPass},
		{Name: "kubelet-registration", Class: ClassControlPlane, Status: StatusPass},
	})
	gotOrder := make([]string, len(r.Checks))
	for i, c := range r.Checks {
		gotOrder[i] = c.Name
	}
	want := []string{"kubelet-registration", "node-lease", "container-runtime", "pod-network"}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Fatalf("order[%d]=%q want %q (full %v)", i, gotOrder[i], want[i], gotOrder)
		}
	}
}

func TestCollectRunsEveryCheckerAndAggregates(t *testing.T) {
	var ranA, ranB bool
	r := Collect(context.Background(), "node-x",
		staticChecker{check: Check{Name: "a", Class: ClassRuntime, Status: StatusPass}, ran: &ranA},
		staticChecker{check: Check{Name: "b", Class: ClassDataPlane, Status: StatusFail}, ran: &ranB},
	)
	if !ranA || !ranB {
		t.Fatalf("not all checkers ran: a=%v b=%v", ranA, ranB)
	}
	if r.Node != "node-x" {
		t.Fatalf("node=%q", r.Node)
	}
	if r.Ready {
		t.Fatal("expected not ready due to checker b failure")
	}
	if len(r.Checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(r.Checks))
	}
}

func TestByClassGroupsInDisplayOrder(t *testing.T) {
	r := newReport("n", []Check{
		{Name: "pod-network", Class: ClassDataPlane, Status: StatusPass},
		{Name: "container-runtime", Class: ClassRuntime, Status: StatusPass},
		{Name: "kubelet-registration", Class: ClassControlPlane, Status: StatusPass},
	})
	groups := r.ByClass()
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}
	wantClasses := []Class{ClassControlPlane, ClassRuntime, ClassDataPlane}
	for i, g := range groups {
		if g.Class != wantClasses[i] {
			t.Fatalf("group[%d].Class=%q want %q", i, g.Class, wantClasses[i])
		}
	}
}

func TestJSONRoundTrips(t *testing.T) {
	r := newReport("node-a", []Check{
		{Name: "container-runtime", Class: ClassRuntime, Status: StatusFail, Summary: "down", Hint: "start it"},
	})
	b, err := r.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var got Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Ready != r.Ready || got.Node != r.Node || len(got.Checks) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Checks[0].Hint != "start it" {
		t.Fatalf("hint lost: %+v", got.Checks[0])
	}
}

func TestTextRenderingIncludesVerdictAndFailHint(t *testing.T) {
	r := newReport("node-a", []Check{
		{Name: "container-runtime", Class: ClassRuntime, Status: StatusFail, Summary: "down", Detail: "boom", Hint: "start it"},
	})
	out := r.Text()
	for _, want := range []string{"NOT READY", "container-runtime", "down", "boom", "hint: start it"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text missing %q:\n%s", want, out)
		}
	}
}
