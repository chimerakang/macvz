package noderemove

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// --- fakes ---

type fakeNodes struct {
	deleted []string
	err     error
}

func (f *fakeNodes) DeleteNode(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	return f.err
}

// fakeReaper records calls and returns canned reaped IDs / error.
type fakeReaper struct {
	reaped    []string
	err       error
	calledDry *bool
	notReaped []string // returned (partial) when err != nil
	called    bool
}

func (f *fakeReaper) ReapAll(_ context.Context, dryRun bool) ([]string, error) {
	f.called = true
	d := dryRun
	f.calledDry = &d
	if f.err != nil {
		return f.notReaped, f.err
	}
	return f.reaped, nil
}

type fakeFlusher struct {
	called bool
	err    error
}

func (f *fakeFlusher) FlushAnchor(context.Context) error { f.called = true; return f.err }

type fakeMesh struct {
	called bool
	err    error
}

func (f *fakeMesh) Remove(context.Context) error { f.called = true; return f.err }

func step(t *testing.T, r Result, name string) StepResult {
	t.Helper()
	for _, s := range r.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %q in result", name)
	return StepResult{}
}

// --- tests ---

func TestRunHappyPathRemovesEverything(t *testing.T) {
	nodes := &fakeNodes{}
	reaper := &fakeReaper{reaped: []string{"macvz-default-web", "macvz-kube-system-dns"}}
	pf := &fakeFlusher{}
	mesh := &fakeMesh{}

	res := Run(context.Background(), Options{
		Node:  "macvz-a",
		Nodes: nodes, VMs: reaper, PF: pf, Mesh: mesh,
	})

	if !res.OK() {
		t.Fatalf("expected OK result, got:\n%s", res.Render())
	}
	if len(nodes.deleted) != 1 || nodes.deleted[0] != "macvz-a" {
		t.Errorf("node delete = %v, want [macvz-a]", nodes.deleted)
	}
	if !reaper.called || !pf.called || !mesh.called {
		t.Errorf("reaper=%v pf=%v mesh=%v, want all true", reaper.called, pf.called, mesh.called)
	}
	if d := step(t, res, "reap-vms").Detail; !strings.Contains(d, "destroyed 2") {
		t.Errorf("reap-vms detail = %q", d)
	}
}

func TestRunSkipsNilDependencies(t *testing.T) {
	// Only the runtime is wired (e.g. --keep-node, mesh and pod-network disabled).
	reaper := &fakeReaper{}
	res := Run(context.Background(), Options{Node: "n", VMs: reaper})

	if !res.OK() {
		t.Fatalf("skipped deps should not fail: %s", res.Render())
	}
	for _, name := range []string{"delete-node", "flush-pf", "mesh-down"} {
		if s := step(t, res, name); s.Status != StepSkipped {
			t.Errorf("%s status = %s, want SKIP", name, s.Status)
		}
	}
	if s := step(t, res, "reap-vms"); s.Status != StepOK {
		t.Errorf("reap-vms with empty list = %s, want OK", s.Status)
	}
}

func TestRunPartialFailureContinuesAndReportsFailed(t *testing.T) {
	nodes := &fakeNodes{err: errors.New("apiserver unreachable")}
	reaper := &fakeReaper{err: errors.New("vm stuck"), notReaped: []string{"macvz-a-2"}}
	mesh := &fakeMesh{}

	res := Run(context.Background(), Options{
		Node: "macvz-a", Nodes: nodes, VMs: reaper, Mesh: mesh,
	})

	if res.OK() {
		t.Fatal("expected not-OK result on partial failure")
	}
	if s := step(t, res, "delete-node"); s.Status != StepFailed {
		t.Errorf("delete-node = %s, want FAIL", s.Status)
	}
	if s := step(t, res, "reap-vms"); s.Status != StepFailed || !strings.Contains(s.Detail, "reaped 1") {
		t.Errorf("reap-vms = %s detail=%q", s.Status, s.Detail)
	}
	// A later step still runs despite earlier failures (best-effort).
	if !mesh.called {
		t.Error("mesh-down must run even after earlier steps failed")
	}
	if !strings.Contains(res.Render(), "PARTIAL") {
		t.Errorf("render should flag PARTIAL:\n%s", res.Render())
	}
}

func TestRunDryRunMakesNoChanges(t *testing.T) {
	nodes := &fakeNodes{}
	reaper := &fakeReaper{reaped: []string{"macvz-a-1"}}
	pf := &fakeFlusher{}
	mesh := &fakeMesh{}

	res := Run(context.Background(), Options{
		Node: "macvz-a", DryRun: true,
		Nodes: nodes, VMs: reaper, PF: pf, Mesh: mesh,
	})

	// Node delete, pf, and mesh are pure side effects: dry-run must not call them.
	if len(nodes.deleted) != 0 || pf.called || mesh.called {
		t.Errorf("dry-run performed side effects: nodes=%v pf=%v mesh=%v", nodes.deleted, pf.called, mesh.called)
	}
	// The reaper IS invoked in dry-run (it computes what it would reap) but with
	// dryRun=true so it mutates nothing.
	if !reaper.called || reaper.calledDry == nil || !*reaper.calledDry {
		t.Errorf("reaper should be called with dryRun=true, called=%v dry=%v", reaper.called, reaper.calledDry)
	}
	for _, s := range res.Steps {
		if s.Status != StepSkipped {
			t.Errorf("dry-run step %s = %s, want SKIP", s.Name, s.Status)
		}
	}
	if d := step(t, res, "reap-vms").Detail; !strings.Contains(d, "would destroy 1") {
		t.Errorf("dry-run reap-vms detail = %q", d)
	}
	if !strings.Contains(res.Render(), "DRY-RUN") {
		t.Errorf("render should flag DRY-RUN:\n%s", res.Render())
	}
}

func TestRunReapErrorDoesNotBlockMesh(t *testing.T) {
	reaper := &fakeReaper{err: errors.New("runtime down")}
	mesh := &fakeMesh{}
	res := Run(context.Background(), Options{Node: "n", VMs: reaper, Mesh: mesh})

	if s := step(t, res, "reap-vms"); s.Status != StepFailed {
		t.Errorf("reap-vms = %s, want FAIL", s.Status)
	}
	if !mesh.called {
		t.Error("mesh-down must still run after a reap failure")
	}
}

func TestStepOrder(t *testing.T) {
	res := Run(context.Background(), Options{Node: "n"})
	want := []string{"delete-node", "reap-vms", "flush-pf", "mesh-down"}
	if len(res.Steps) != len(want) {
		t.Fatalf("got %d steps, want %d", len(res.Steps), len(want))
	}
	for i, w := range want {
		if res.Steps[i].Name != w {
			t.Errorf("step[%d] = %s, want %s", i, res.Steps[i].Name, w)
		}
	}
}
