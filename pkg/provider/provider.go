// Package provider implements the Virtual Kubelet PodLifecycleHandler, turning
// an Apple Silicon Mac into a Kubernetes node. Each Pod is realized as one or
// more micro-VMs through the runtime.Runtime abstraction.
//
// The provider keeps an in-memory store mapping each Pod (by namespace/name) to
// the runtime workload IDs backing its containers, and reconciles observed
// runtime state into Kubernetes Pod status on demand. Pod-spec translation is
// intentionally minimal here and is extended in #17.
package provider

import (
	"sync"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	corev1 "k8s.io/api/core/v1"
)

// defaultStopTimeout is used when a Pod has no terminationGracePeriodSeconds.
const defaultStopTimeout = 30 * time.Second

// Provider realizes Kubernetes Pods as micro-VMs via a runtime.Runtime.
type Provider struct {
	nodeName string
	rt       runtime.Runtime

	mu   sync.RWMutex
	pods map[string]*podState
}

// podState tracks one Pod and the runtime workloads backing its containers.
type podState struct {
	// pod is the tracked Pod, including the status the provider maintains.
	pod *corev1.Pod
	// workloads maps each container to its runtime workload ID, in spec order.
	workloads []workload
	// terminalStatus, when set, is a sticky status that overrides live
	// reconciliation. It is used for Pods that can never run on this node (an
	// unsupported spec, or an image with no arm64 variant), so they surface a
	// clear, stable Failed status instead of being re-derived as Pending.
	terminalStatus *corev1.PodStatus
}

// workload binds a Pod container to a runtime workload ID.
type workload struct {
	container string
	id        string
}

// New constructs a Provider bound to a node name and runtime driver.
func New(nodeName string, rt runtime.Runtime) *Provider {
	return &Provider{
		nodeName: nodeName,
		rt:       rt,
		pods:     make(map[string]*podState),
	}
}

// Compile-time assertion that Provider satisfies the Virtual Kubelet contract.
var _ node.PodLifecycleHandler = (*Provider)(nil)

// podKey is the store key for a Pod.
func podKey(namespace, name string) string {
	return namespace + "/" + name
}

// stopTimeout returns the graceful-stop timeout for a Pod.
func stopTimeout(pod *corev1.Pod) time.Duration {
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		return time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second
	}
	return defaultStopTimeout
}
