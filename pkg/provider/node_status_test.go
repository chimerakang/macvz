package provider

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
)

// togglableRuntime is a fakeRuntime whose Pinger readiness can flip at runtime.
type togglableRuntime struct {
	fakeRuntime
	mu  sync.Mutex
	err error
}

func (r *togglableRuntime) Ready(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *togglableRuntime) set(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func TestNodeStatusProviderPingReturnsContextError(t *testing.T) {
	np := New("n", &togglableRuntime{}).NewNodeStatusProvider(testSpec(), time.Second)
	if err := np.Ping(context.Background()); err != nil {
		t.Errorf("Ping with live context should be nil, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := np.Ping(ctx); err == nil {
		t.Error("Ping with cancelled context should return its error")
	}
}

func TestNodeStatusProviderPushesOnReadinessChange(t *testing.T) {
	rt := &togglableRuntime{} // starts ready (nil error)
	np := New("mac-01", rt).NewNodeStatusProvider(testSpec(), 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	updates := make(chan *corev1.Node, 4)
	np.NotifyNodeStatus(ctx, func(n *corev1.Node) { updates <- n })

	// No change yet: the loop should not push while readiness is stable.
	select {
	case n := <-updates:
		t.Fatalf("unexpected status push while readiness unchanged: %+v", n.Status.Conditions)
	case <-time.After(40 * time.Millisecond):
	}

	// Flip to not-ready: expect a push with Ready=False.
	rt.set(runtime.ErrNotReady)
	select {
	case n := <-updates:
		if nodeReady(n) {
			t.Errorf("expected Ready=False after runtime failure, got Ready=True")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a not-ready status push")
	}

	// Recover: expect a push with Ready=True.
	rt.set(nil)
	select {
	case n := <-updates:
		if !nodeReady(n) {
			t.Errorf("expected Ready=True after runtime recovery, got Ready=False")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a recovery status push")
	}
}

func TestNodeStatusProviderStopsOnContextCancel(t *testing.T) {
	rt := &togglableRuntime{}
	np := New("n", rt).NewNodeStatusProvider(testSpec(), time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())

	pushed := make(chan *corev1.Node, 1)
	np.NotifyNodeStatus(ctx, func(n *corev1.Node) { pushed <- n })
	cancel()

	// After cancellation, a readiness change must not produce further pushes.
	time.Sleep(10 * time.Millisecond)
	rt.set(runtime.ErrNotReady)
	select {
	case <-pushed:
		t.Error("status push after context cancellation")
	case <-time.After(30 * time.Millisecond):
	}
}
