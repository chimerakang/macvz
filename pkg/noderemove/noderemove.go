// Package noderemove implements the permanent node-removal workflow (#58): the
// ordered, best-effort teardown that takes a Mac out of a MacVz node pool
// without leaving active workloads or stale network state behind.
//
// It is the union of three concerns, run in order:
//
//  1. delete-node — remove the Kubernetes Node object so the scheduler stops
//     placing Pods here and the remaining nodes drop this node's endpoints (they
//     then stop routing Service traffic to it).
//  2. reap-vms    — destroy every MacVz-managed micro-VM on this host.
//  3. flush-pf    — flush the Pod-network pf anchor, removing all NAT rules.
//  4. mesh-down   — delete this node's WireGuard routes and destroy the tunnel
//     interface.
//
// Reaping and pf flushing reuse the #57 drain primitives (pkg/drain) — this
// package adds the two things permanent removal needs beyond a drain: deleting
// the Node object and tearing down the cross-host WireGuard mesh, plus the
// ordering and partial-failure reporting that make removal safe to re-run.
//
// Removal is deliberately not transactional: each step is independent and
// idempotent, and a failure in one never aborts the others — a half-removed node
// is worse than one where every reachable resource was cleaned up. Run records a
// per-step outcome so an operator (or a wrapping command's exit code) can see
// exactly what remains and re-run to converge. Every dependency is optional: a
// nil one skips its step rather than failing it, and DryRun suppresses all side
// effects while still reporting what would run.
package noderemove

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/klog/v2"
)

// StepStatus is the outcome of a single removal step.
type StepStatus int

const (
	// StepOK means the step reached its desired end state (including when the
	// resource was already absent — removal is idempotent).
	StepOK StepStatus = iota
	// StepSkipped means the step did not run: its component is disabled, its
	// dependency was not provided, or this was a dry run.
	StepSkipped
	// StepFailed means the step ran but did not fully succeed. The node is only
	// partially removed; the step is safe to re-run.
	StepFailed
)

func (s StepStatus) String() string {
	switch s {
	case StepOK:
		return "OK"
	case StepSkipped:
		return "SKIP"
	case StepFailed:
		return "FAIL"
	default:
		return "UNKNOWN"
	}
}

// StepResult records one step's outcome.
type StepResult struct {
	// Name is the stable step identifier (e.g. "reap-vms").
	Name string
	// Status is the outcome.
	Status StepStatus
	// Detail is a short human-readable summary (e.g. "destroyed 3 micro-VMs").
	Detail string
	// Err is the underlying error when Status is StepFailed.
	Err error
}

// Result is the full outcome of a removal run.
type Result struct {
	// Node is the Kubernetes node name that was removed.
	Node string
	// DryRun reports whether side effects were suppressed.
	DryRun bool
	// Steps are the per-step outcomes, in execution order.
	Steps []StepResult
}

// OK reports whether the removal is complete — no step failed. Skipped steps do
// not block OK: an operator who omits a dependency (e.g. --keep-node) still gets
// a clean result for the parts they asked to remove.
func (r Result) OK() bool {
	for _, s := range r.Steps {
		if s.Status == StepFailed {
			return false
		}
	}
	return true
}

// NodeDeleter removes the Kubernetes Node object. Implementations must treat an
// already-absent node as success so removal stays idempotent.
type NodeDeleter interface {
	DeleteNode(ctx context.Context, name string) error
}

// VMReaper destroys every MacVz-managed micro-VM on the host and reports the IDs
// reaped. It is satisfied in the command by an adapter over *drain.Cleaner
// (which owns the MacVz-prefix safety filter and best-effort stop/destroy).
type VMReaper interface {
	// ReapAll destroys all MacVz micro-VMs (no Pod is expected to remain on a node
	// being removed) and returns the IDs reaped. With dryRun it mutates nothing.
	ReapAll(ctx context.Context, dryRun bool) (reaped []string, err error)
}

// PFFlusher flushes the Pod-network pf anchor, removing every MacVz NAT rule. It
// is the same interface drain uses (drain.AnchorFlusher), so the command reuses
// the existing helper/pfctl flusher rather than a second flush path.
type PFFlusher interface {
	FlushAnchor(ctx context.Context) error
}

// MeshRemover tears down the WireGuard mesh (routes + interface). *wireguard.Mesh
// satisfies it via Remove.
type MeshRemover interface {
	Remove(ctx context.Context) error
}

// Options configures a removal run. Node is required; every dependency is
// optional and a nil one skips its step.
type Options struct {
	// Node is the Kubernetes node name being removed.
	Node string
	// DryRun reports what would run without performing any side effect.
	DryRun bool

	// Nodes deletes the Kubernetes Node object. Nil skips delete-node (e.g. the
	// operator passed --keep-node, or no kubeconfig is available).
	Nodes NodeDeleter
	// VMs reaps local micro-VMs. Nil skips reap-vms.
	VMs VMReaper
	// PF flushes the pod-network anchor. Nil skips flush-pf (Pod network disabled).
	PF PFFlusher
	// Mesh tears down WireGuard. Nil skips mesh-down (mesh disabled).
	Mesh MeshRemover
}

// Run executes the removal steps in order, best-effort, and returns the
// per-step result. It never returns an error itself — inspect Result.OK and the
// individual StepResults.
func Run(ctx context.Context, opts Options) Result {
	res := Result{Node: opts.Node, DryRun: opts.DryRun}
	res.Steps = append(res.Steps,
		runStep(ctx, "delete-node", opts.DryRun, opts.Nodes == nil, "no kubeconfig or --keep-node set", func(ctx context.Context) (string, error) {
			if err := opts.Nodes.DeleteNode(ctx, opts.Node); err != nil {
				return "", err
			}
			return fmt.Sprintf("Node %q deleted", opts.Node), nil
		}),
		runReapVMs(ctx, opts),
		runStep(ctx, "flush-pf", opts.DryRun, opts.PF == nil, "Pod network not enabled", func(ctx context.Context) (string, error) {
			return "pf anchor flushed", opts.PF.FlushAnchor(ctx)
		}),
		runStep(ctx, "mesh-down", opts.DryRun, opts.Mesh == nil, "mesh not enabled", func(ctx context.Context) (string, error) {
			return "routes deleted and interface destroyed", opts.Mesh.Remove(ctx)
		}),
	)
	klog.InfoS("node removal finished", "node", opts.Node, "dryRun", opts.DryRun, "ok", res.OK())
	return res
}

// runReapVMs destroys all MacVz micro-VMs via the injected reaper. The reaper is
// best-effort internally (one stuck VM never strands the rest); a non-nil error
// means at least one VM could not be removed, which fails the step.
func runReapVMs(ctx context.Context, opts Options) StepResult {
	if opts.VMs == nil {
		return StepResult{Name: "reap-vms", Status: StepSkipped, Detail: "runtime unavailable"}
	}
	reaped, err := opts.VMs.ReapAll(ctx, opts.DryRun)
	if err != nil {
		return StepResult{
			Name:   "reap-vms",
			Status: StepFailed,
			Detail: fmt.Sprintf("reaped %d micro-VM(s) before error: %s", len(reaped), summarizeIDs(reaped)),
			Err:    err,
		}
	}
	if opts.DryRun {
		if len(reaped) == 0 {
			return StepResult{Name: "reap-vms", Status: StepSkipped, Detail: "dry-run: no MacVz micro-VMs to remove"}
		}
		return StepResult{Name: "reap-vms", Status: StepSkipped, Detail: fmt.Sprintf("dry-run: would destroy %d micro-VM(s): %s", len(reaped), summarizeIDs(reaped))}
	}
	if len(reaped) == 0 {
		return StepResult{Name: "reap-vms", Status: StepOK, Detail: "no MacVz micro-VMs to remove"}
	}
	return StepResult{Name: "reap-vms", Status: StepOK, Detail: fmt.Sprintf("destroyed %d micro-VM(s): %s", len(reaped), summarizeIDs(reaped))}
}

func summarizeIDs(ids []string) string {
	if len(ids) == 0 {
		return "(none)"
	}
	return strings.Join(ids, ", ")
}

// runStep is the common scaffold for the simple steps: it honors skip and
// dry-run, then runs the action and maps its error to a StepResult.
func runStep(ctx context.Context, name string, dryRun, skip bool, skipReason string, action func(ctx context.Context) (string, error)) StepResult {
	if skip {
		return StepResult{Name: name, Status: StepSkipped, Detail: skipReason}
	}
	if dryRun {
		return StepResult{Name: name, Status: StepSkipped, Detail: "dry-run: would run"}
	}
	detail, err := action(ctx)
	if err != nil {
		return StepResult{Name: name, Status: StepFailed, Detail: "step failed", Err: err}
	}
	return StepResult{Name: name, Status: StepOK, Detail: detail}
}
