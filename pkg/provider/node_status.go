package provider

import (
	"context"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// NodeStatusProvider implements the Virtual Kubelet node.NodeProvider contract.
//
// Ping reports process liveness (it drives the node heartbeat / lease, keeping
// the node present in Kubernetes). NotifyNodeStatus runs a loop that re-probes
// the runtime on an interval and pushes a refreshed node whenever readiness
// changes, so runtime failures surface in the node's Ready condition without
// removing the node.
type NodeStatusProvider struct {
	provider *Provider
	spec     NodeSpec
	interval time.Duration
}

// Compile-time assertion that NodeStatusProvider satisfies the contract.
var _ node.NodeProvider = (*NodeStatusProvider)(nil)

// NewNodeStatusProvider builds a NodeProvider that re-probes runtime readiness
// every interval and reports changes through the node controller.
func (p *Provider) NewNodeStatusProvider(spec NodeSpec, interval time.Duration) *NodeStatusProvider {
	return &NodeStatusProvider{provider: p, spec: spec, interval: interval}
}

// Ping reports whether this kubelet process is alive. Runtime health is carried
// in node conditions (via NotifyNodeStatus), not here: a down runtime should
// make the node NotReady, not make it disappear. Returning the context error
// keeps the heartbeat lightweight and lets shutdown propagate.
func (np *NodeStatusProvider) Ping(ctx context.Context) error {
	return ctx.Err()
}

// NotifyNodeStatus registers the controller callback and starts the readiness
// watch loop. It must not block, so the loop runs in its own goroutine and
// exits when ctx is cancelled.
func (np *NodeStatusProvider) NotifyNodeStatus(ctx context.Context, cb func(*corev1.Node)) {
	go np.watch(ctx, cb)
}

func (np *NodeStatusProvider) watch(ctx context.Context, cb func(*corev1.Node)) {
	ticker := time.NewTicker(np.interval)
	defer ticker.Stop()

	// The node registered by the controller already reflects the readiness
	// observed at startup; only push when it subsequently changes.
	last := nodeReady(np.provider.BuildNode(ctx, np.spec))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			node := np.provider.BuildNode(ctx, np.spec)
			ready := nodeReady(node)
			if ready == last {
				continue
			}
			last = ready
			klog.InfoS("node runtime readiness changed", "node", np.provider.nodeName, "ready", ready)
			cb(node)
		}
	}
}

// nodeReady reports whether the node's Ready condition is True.
func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
