// Package provider implements the Virtual Kubelet PodLifecycleHandler, turning
// an Apple Silicon Mac into a Kubernetes node. Each Pod is realized as a
// micro-VM through the runtime.Runtime abstraction.
//
// This is the P0 skeleton: the type satisfies the interface and depends only on
// runtime.Runtime (never a concrete driver). Real translation and lifecycle
// logic land in P2.
package provider

import (
	"context"
	"errors"

	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	corev1 "k8s.io/api/core/v1"
)

// errNotImplemented is returned by every method until P2 fills them in.
var errNotImplemented = errors.New("macvz provider: not implemented yet")

// Provider realizes Kubernetes Pods as micro-VMs via a runtime.Runtime.
type Provider struct {
	nodeName string
	rt       runtime.Runtime
}

// New constructs a Provider bound to a node name and runtime driver.
func New(nodeName string, rt runtime.Runtime) *Provider {
	return &Provider{nodeName: nodeName, rt: rt}
}

// Compile-time assertion that Provider satisfies the Virtual Kubelet contract.
var _ node.PodLifecycleHandler = (*Provider)(nil)

// CreatePod takes a Kubernetes Pod and launches it as a micro-VM.
func (p *Provider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	return errNotImplemented
}

// UpdatePod reconciles an existing Pod's desired state.
func (p *Provider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	return errNotImplemented
}

// DeletePod tears down the micro-VM backing a Pod.
func (p *Provider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	return errNotImplemented
}

// GetPod returns the Pod for namespace/name, or nil if not found.
func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return nil, errNotImplemented
}

// GetPodStatus returns the status of the Pod identified by namespace/name.
func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	return nil, errNotImplemented
}

// GetPods lists all Pods known to this provider.
func (p *Provider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	return nil, errNotImplemented
}
