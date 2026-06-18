package provider

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// fakeRuntime satisfies runtime.Runtime but not runtime.Pinger, so the provider
// treats it as readiness-unknown (assumed ready).
type fakeRuntime struct{}

func (fakeRuntime) Pull(context.Context, string) error                          { return nil }
func (fakeRuntime) Create(context.Context, types.ContainerSpec) (string, error) { return "", nil }
func (fakeRuntime) Start(context.Context, string) error                         { return nil }
func (fakeRuntime) Stop(context.Context, string, time.Duration) error           { return nil }
func (fakeRuntime) Destroy(context.Context, string) error                       { return nil }
func (fakeRuntime) Status(context.Context, string) (runtime.Status, error) {
	return runtime.Status{}, nil
}
func (fakeRuntime) Logs(context.Context, string, runtime.LogOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (fakeRuntime) Exec(context.Context, string, []string, runtime.ExecIO) error { return nil }

// pingableRuntime adds the optional runtime.Pinger capability.
type pingableRuntime struct {
	fakeRuntime
	readyErr error
}

func (p pingableRuntime) Ready(context.Context) error { return p.readyErr }

func testSpec() NodeSpec {
	return NodeSpec{
		KubeletVersion: "test",
		OS:             "linux",
		Arch:           "arm64",
		InternalIP:     "10.0.0.5",
		Capacity: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
			corev1.ResourcePods:   resource.MustParse("32"),
		},
		Labels: map[string]string{"custom": "yes", "kubernetes.io/os": "override"},
		Taints: []corev1.Taint{{Key: "virtual-kubelet.io/provider", Value: "macvz", Effect: corev1.TaintEffectNoSchedule}},
	}
}

func findCondition(node *corev1.Node, t corev1.NodeConditionType) (corev1.NodeCondition, bool) {
	for _, c := range node.Status.Conditions {
		if c.Type == t {
			return c, true
		}
	}
	return corev1.NodeCondition{}, false
}

func TestBuildNodeShape(t *testing.T) {
	p := New("mac-01", pingableRuntime{})
	node := p.BuildNode(context.Background(), testSpec())

	if node.Name != "mac-01" {
		t.Errorf("Name = %q, want mac-01", node.Name)
	}
	if got := node.Status.Capacity.Cpu().String(); got != "4" {
		t.Errorf("capacity cpu = %q, want 4", got)
	}
	// Allocatable mirrors capacity in the MVP (no reservation).
	if node.Status.Allocatable.Memory().String() != node.Status.Capacity.Memory().String() {
		t.Error("allocatable should mirror capacity")
	}
	if node.Status.NodeInfo.OperatingSystem != "linux" || node.Status.NodeInfo.Architecture != "arm64" {
		t.Error("node info OS/arch not set to workload platform")
	}
	// User labels override built-ins; built-ins still present for unset keys.
	if node.Labels["kubernetes.io/os"] != "override" {
		t.Errorf("user label override not applied: %v", node.Labels["kubernetes.io/os"])
	}
	if node.Labels["custom"] != "yes" {
		t.Error("custom label missing")
	}
	if node.Labels["type"] != "virtual-kubelet" {
		t.Error("built-in type label missing")
	}
	if len(node.Spec.Taints) != 1 || node.Spec.Taints[0].Key != "virtual-kubelet.io/provider" {
		t.Errorf("taints = %v, want the provider taint", node.Spec.Taints)
	}

	var foundIP bool
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP && a.Address == "10.0.0.5" {
			foundIP = true
		}
	}
	if !foundIP {
		t.Errorf("InternalIP address missing: %v", node.Status.Addresses)
	}
}

func TestBuildNodeReadyReflectsRuntime(t *testing.T) {
	ready := New("n", pingableRuntime{}).BuildNode(context.Background(), testSpec())
	if c, ok := findCondition(ready, corev1.NodeReady); !ok || c.Status != corev1.ConditionTrue {
		t.Fatalf("expected Ready=True, got %+v (found=%v)", c, ok)
	}

	notReady := New("n", pingableRuntime{readyErr: runtime.ErrNotReady}).
		BuildNode(context.Background(), testSpec())
	c, ok := findCondition(notReady, corev1.NodeReady)
	if !ok {
		t.Fatal("Ready condition missing")
	}
	if c.Status != corev1.ConditionFalse {
		t.Fatalf("expected Ready=False when runtime not ready, got %+v", c)
	}
	if c.Reason != "RuntimeNotReady" {
		t.Errorf("Ready reason = %q, want RuntimeNotReady", c.Reason)
	}
}

func TestBuildNodeWithoutPingerAssumesReady(t *testing.T) {
	node := New("n", fakeRuntime{}).BuildNode(context.Background(), testSpec())
	if c, ok := findCondition(node, corev1.NodeReady); !ok || c.Status != corev1.ConditionTrue {
		t.Fatalf("runtime without Pinger should be assumed Ready, got %+v", c)
	}
}

func TestBuildNodeOmitsEmptyInternalIP(t *testing.T) {
	spec := testSpec()
	spec.InternalIP = ""
	node := New("n", fakeRuntime{}).BuildNode(context.Background(), spec)
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			t.Errorf("did not expect an InternalIP address, got %q", a.Address)
		}
	}
}

func TestBuildNodeAdvertisesKubeletPortOnlyWhenConfigured(t *testing.T) {
	spec := testSpec()
	node := New("n", fakeRuntime{}).BuildNode(context.Background(), spec)
	if node.Status.DaemonEndpoints.KubeletEndpoint.Port != 0 {
		t.Errorf("kubelet endpoint port = %d, want unset", node.Status.DaemonEndpoints.KubeletEndpoint.Port)
	}

	spec.KubeletPort = 10250
	node = New("n", fakeRuntime{}).BuildNode(context.Background(), spec)
	if node.Status.DaemonEndpoints.KubeletEndpoint.Port != 10250 {
		t.Errorf("kubelet endpoint port = %d, want 10250", node.Status.DaemonEndpoints.KubeletEndpoint.Port)
	}
}
