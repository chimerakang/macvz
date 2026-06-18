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

	"github.com/chimerakang/macvz/pkg/metrics"
	"github.com/chimerakang/macvz/pkg/network"
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

	// createMu serializes CreatePod. Virtual Kubelet can retry or race the same
	// Pod key; serializing keeps the idempotency check and runtime side effects
	// together so duplicate workloads are not leaked.
	createMu sync.Mutex

	// ipam, when set, allocates each Pod a stable IP from this node's
	// Kubernetes-assigned Pod CIDR. It is nil on clusters without coordinated
	// IPAM, in which case the Pod IP falls back to the runtime-reported address.
	ipam *network.PodIPAM

	// podNet, when set, wires each Pod's micro-VM into the Pod network path so it
	// is reachable at its assigned Pod IP across the mesh (#22). Nil disables it.
	podNet PodNetwork

	// hostIP is this node's reachable address, reported as each Pod's HostIP so
	// `kubectl get pod -o wide` and topology-aware routing resolve the host. Set
	// once at startup before the Pod controller runs; treated as immutable after.
	hostIP string

	// collector builds the node/Pod resource metrics served through the kubelet
	// stats and resource-metrics endpoints (#25).
	collector *metrics.Collector
}

// Option configures a Provider at construction time.
type Option func(*Provider)

// WithIPAM attaches a Pod IP allocator so Pods receive stable, collision-free
// addresses from this node's Kubernetes-assigned Pod CIDR.
func WithIPAM(ipam *network.PodIPAM) Option {
	return func(p *Provider) { p.ipam = ipam }
}

// WithPodNetwork attaches the Pod network path that makes each micro-VM
// reachable at its Pod IP across the mesh (#22).
func WithPodNetwork(pn PodNetwork) Option {
	return func(p *Provider) { p.podNet = pn }
}

// WithHostIP sets the node address reported as each Pod's HostIP.
func WithHostIP(ip string) Option {
	return func(p *Provider) { p.hostIP = ip }
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
	// vmIP is the micro-VM's apple/container host-only address, observed once the
	// VM has booted. It is mapped to the Pod IP by the Pod network path (#22).
	vmIP string
	// attached records whether the Pod's micro-VM has been wired into the Pod
	// network path, so DeletePod knows to tear the mapping down.
	attached bool
}

// workload binds a Pod container to a runtime workload ID.
type workload struct {
	container string
	id        string
}

// New constructs a Provider bound to a node name and runtime driver.
func New(nodeName string, rt runtime.Runtime, opts ...Option) *Provider {
	p := &Provider{
		nodeName: nodeName,
		rt:       rt,
		pods:     make(map[string]*podState),
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.collector == nil {
		p.collector = metrics.NewCollector(nodeName, metrics.DefaultMemorySampler())
	}
	return p
}

// WithCollector overrides the metrics collector, chiefly so tests can inject a
// fake host memory sampler. Production wiring uses the default in New.
func WithCollector(c *metrics.Collector) Option {
	return func(p *Provider) { p.collector = c }
}

// SetIPAM attaches a Pod IP allocator after construction. The node's Pod CIDR is
// only known once Kubernetes assigns it (after node registration), so the
// allocator is wired in then, before the Pod controller starts. It must not be
// called once Pods are being reconciled.
func (p *Provider) SetIPAM(ipam *network.PodIPAM) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ipam = ipam
}

// SetPodNetwork attaches the Pod network path after construction, before the Pod
// controller starts. It must not be called once Pods are being reconciled.
func (p *Provider) SetPodNetwork(pn PodNetwork) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.podNet = pn
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
