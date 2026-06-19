package main

import (
	"context"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/drain"
	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func TestNodeDeleterIsIdempotent(t *testing.T) {
	cs := kubernetesfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "macvz-a"}})
	d := &nodeDeleter{cs: cs}

	if err := d.DeleteNode(context.Background(), "macvz-a"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// Deleting an already-absent node must be a no-op success so removal can be
	// safely re-run after a partial failure.
	if err := d.DeleteNode(context.Background(), "macvz-a"); err != nil {
		t.Fatalf("second delete (absent node) should be nil, got %v", err)
	}
}

// fakeDriver implements drain.VMLister + drain.VMReaper.
type fakeDriver struct {
	list      []runtime.Status
	destroyed []string
}

func (f *fakeDriver) List(context.Context) ([]runtime.Status, error)    { return f.list, nil }
func (f *fakeDriver) Stop(context.Context, string, time.Duration) error { return nil }
func (f *fakeDriver) Destroy(_ context.Context, id string) error {
	f.destroyed = append(f.destroyed, id)
	return nil
}

func TestCleanerReaperReapsOnlyMacVzVMs(t *testing.T) {
	fd := &fakeDriver{list: []runtime.Status{
		{ID: "macvz-default-web"},
		{ID: "macvz-kube-system-dns"},
		{ID: "unrelated-tool"}, // not MacVz: must be left alone
	}}
	r := &cleanerReaper{cleaner: &drain.Cleaner{Lister: fd, Reaper: fd}}

	reaped, err := r.ReapAll(context.Background(), false)
	if err != nil {
		t.Fatalf("ReapAll: %v", err)
	}
	if len(reaped) != 2 {
		t.Fatalf("reaped %v, want 2 MacVz VMs", reaped)
	}
	for _, id := range fd.destroyed {
		if id == "unrelated-tool" {
			t.Errorf("destroyed a non-MacVz workload %q", id)
		}
	}

	// Dry run must not destroy anything but still reports the would-reap set.
	fd2 := &fakeDriver{list: fd.list}
	r2 := &cleanerReaper{cleaner: &drain.Cleaner{Lister: fd2, Reaper: fd2}}
	would, err := r2.ReapAll(context.Background(), true)
	if err != nil {
		t.Fatalf("dry-run ReapAll: %v", err)
	}
	if len(would) != 2 || len(fd2.destroyed) != 0 {
		t.Errorf("dry-run reaped=%v destroyed=%v, want 2 reported / 0 destroyed", would, fd2.destroyed)
	}
}
