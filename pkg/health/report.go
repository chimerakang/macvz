// Package health aggregates a MacVz node's operational readiness into a single
// report that explains, in human- and machine-readable form, why the node is or
// is not ready to run workloads.
//
// The model separates three failure domains so an operator can tell at a glance
// where a problem lives:
//
//   - control-plane: kubelet registration and node-lease heartbeat health.
//   - runtime:       the apple/container service that boots the micro-VMs.
//   - data-plane:    the privileged helper, WireGuard mesh, host routes, pf
//     anchors, IP forwarding, and per-Pod network attachments.
//
// A node is "ready for workloads" only when no check in any class has failed;
// warnings (e.g. a single-node mesh with no peers) do not block readiness but
// are surfaced so they are not silently ignored. The package is deliberately
// free of transport concerns: it produces a Report from a set of Checkers, and
// the caller decides whether to print it (CLI) or serve it (HTTP endpoint).
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Class is the failure domain a check belongs to. Grouping by class lets the
// report distinguish control-plane, runtime, and data-plane failures, which is
// the first question an operator asks when a node is not ready.
type Class string

const (
	ClassControlPlane Class = "control-plane"
	ClassRuntime      Class = "runtime"
	ClassDataPlane    Class = "data-plane"
)

// classOrder fixes the display order of classes (control-plane first, since a
// node that is not even registered makes downstream checks moot).
var classOrder = map[Class]int{
	ClassControlPlane: 0,
	ClassRuntime:      1,
	ClassDataPlane:    2,
}

// Status is the outcome of a single check.
type Status string

const (
	// StatusPass means the check observed a healthy state.
	StatusPass Status = "pass"
	// StatusWarn means a non-fatal condition worth surfacing (e.g. a mesh with
	// no peers). Warnings do not make the node not-ready.
	StatusWarn Status = "warn"
	// StatusFail means a condition that prevents the node from running workloads.
	StatusFail Status = "fail"
	// StatusSkipped means the check did not apply (e.g. a disabled subsystem).
	StatusSkipped Status = "skipped"
)

// Check is the result of probing one facet of node health.
type Check struct {
	// Name identifies the check (stable, kebab-case, e.g. "container-runtime").
	Name string `json:"name"`
	// Class is the failure domain this check belongs to.
	Class Class `json:"class"`
	// Status is the outcome.
	Status Status `json:"status"`
	// Summary is a one-line, human-readable result.
	Summary string `json:"summary"`
	// Detail carries optional extra context (observed values, error text).
	Detail string `json:"detail,omitempty"`
	// Hint is an optional actionable remediation pointer for failures.
	Hint string `json:"hint,omitempty"`
}

// Checker probes one facet of node health. Implementations must be safe to call
// concurrently with other checkers and must not panic: a probe failure is
// reported as a StatusFail Check, never an error return, so one failing
// subsystem never aborts the whole report.
type Checker interface {
	// Check runs the probe and returns its result. It must honour ctx for
	// cancellation/deadline so a hung subsystem cannot stall the report.
	Check(ctx context.Context) Check
}

// Report is the aggregated readiness view of a single node.
type Report struct {
	// Node is the Kubernetes node name this report describes.
	Node string `json:"node"`
	// Ready reports whether the node can run workloads (no failed checks).
	Ready bool `json:"ready"`
	// Reason explains the readiness verdict: the blocking failures when not
	// ready, or a healthy summary (with any warnings) when ready.
	Reason string `json:"reason"`
	// Checks are the individual results, ordered by class then name.
	Checks []Check `json:"checks"`
}

// Collect runs every checker (concurrently) and aggregates the results into a
// Report. Checkers run in parallel because probes are I/O-bound (sockets, the
// API server, the runtime); the results are sorted deterministically afterwards
// so the report is stable regardless of completion order.
func Collect(ctx context.Context, node string, checkers ...Checker) Report {
	results := make([]Check, len(checkers))
	var wg sync.WaitGroup
	for i, c := range checkers {
		wg.Add(1)
		go func(i int, c Checker) {
			defer wg.Done()
			results[i] = c.Check(ctx)
		}(i, c)
	}
	wg.Wait()
	return newReport(node, results)
}

// newReport sorts the checks and computes the readiness verdict and reason. It
// is separated from Collect so aggregation can be unit-tested without spawning
// goroutines or real checkers.
func newReport(node string, checks []Check) Report {
	sorted := append([]Check(nil), checks...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if classOrder[sorted[i].Class] != classOrder[sorted[j].Class] {
			return classOrder[sorted[i].Class] < classOrder[sorted[j].Class]
		}
		return sorted[i].Name < sorted[j].Name
	})

	ready, reason := verdict(sorted)
	return Report{Node: node, Ready: ready, Reason: reason, Checks: sorted}
}

// verdict derives the overall readiness and a one-line reason. The node is
// ready iff no check failed. The reason names the blocking failures grouped by
// class when not ready, and surfaces any warnings when ready so they are not
// lost.
func verdict(checks []Check) (bool, string) {
	var failures, warnings []Check
	for _, c := range checks {
		switch c.Status {
		case StatusFail:
			failures = append(failures, c)
		case StatusWarn:
			warnings = append(warnings, c)
		}
	}

	if len(failures) > 0 {
		return false, "not ready for workloads: " + describe(failures)
	}
	if len(warnings) > 0 {
		return true, "ready for workloads (with warnings: " + describe(warnings) + ")"
	}
	return true, "ready for workloads"
}

// describe renders a compact "class/name" list (deduplicated class prefixes) for
// the readiness reason, e.g. "runtime[container-runtime], data-plane[wireguard-mesh]".
func describe(checks []Check) string {
	byClass := map[Class][]string{}
	var order []Class
	for _, c := range checks {
		if _, seen := byClass[c.Class]; !seen {
			order = append(order, c.Class)
		}
		byClass[c.Class] = append(byClass[c.Class], c.Name)
	}
	sort.SliceStable(order, func(i, j int) bool { return classOrder[order[i]] < classOrder[order[j]] })

	parts := make([]string, 0, len(order))
	for _, class := range order {
		parts = append(parts, fmt.Sprintf("%s[%s]", class, strings.Join(byClass[class], ", ")))
	}
	return strings.Join(parts, ", ")
}

// ByClass returns the report's checks grouped by class, in display order. Empty
// classes are omitted.
func (r Report) ByClass() []ClassGroup {
	groups := map[Class][]Check{}
	var order []Class
	for _, c := range r.Checks {
		if _, seen := groups[c.Class]; !seen {
			order = append(order, c.Class)
		}
		groups[c.Class] = append(groups[c.Class], c)
	}
	sort.SliceStable(order, func(i, j int) bool { return classOrder[order[i]] < classOrder[order[j]] })

	out := make([]ClassGroup, 0, len(order))
	for _, class := range order {
		out = append(out, ClassGroup{Class: class, Checks: groups[class]})
	}
	return out
}

// ClassGroup is the checks of one failure domain, for grouped rendering.
type ClassGroup struct {
	Class  Class
	Checks []Check
}

// JSON renders the report as indented JSON for machine-readable consumption.
func (r Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// Text renders the report as a human-readable, class-grouped summary suitable
// for a terminal. The first line is the overall verdict; each class follows
// with its checks, marked by a status glyph.
func (r Report) Text() string {
	var b strings.Builder
	verdict := "READY"
	if !r.Ready {
		verdict = "NOT READY"
	}
	fmt.Fprintf(&b, "Node %s: %s\n", r.Node, verdict)
	fmt.Fprintf(&b, "  %s\n", r.Reason)

	for _, g := range r.ByClass() {
		fmt.Fprintf(&b, "\n[%s]\n", g.Class)
		for _, c := range g.Checks {
			fmt.Fprintf(&b, "  %s %s: %s\n", glyph(c.Status), c.Name, c.Summary)
			if c.Detail != "" {
				fmt.Fprintf(&b, "      %s\n", c.Detail)
			}
			if c.Status == StatusFail && c.Hint != "" {
				fmt.Fprintf(&b, "      hint: %s\n", c.Hint)
			}
		}
	}
	return b.String()
}

// glyph maps a status to a fixed-width marker for the text report.
func glyph(s Status) string {
	switch s {
	case StatusPass:
		return "[ok]  "
	case StatusWarn:
		return "[warn]"
	case StatusFail:
		return "[fail]"
	case StatusSkipped:
		return "[skip]"
	default:
		return "[?]   "
	}
}
