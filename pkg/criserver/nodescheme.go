package criserver

import "fmt"

// This file is the CRI-P9 follow-up (#84) answer to host-namespace workloads.
//
// apple/container runs every Pod as an isolated Linux micro-VM with its own
// network, PID, and IPC namespaces. A Pod that asks to share the *host*
// namespaces (`spec.hostNetwork`/`hostPID`/`hostIPC`) therefore cannot be
// honored honestly on this node — booting it anyway would silently ignore the
// request (see diagnose.go). Option (a), "represent host-namespace workloads on
// the per-Pod-VM model", is not physically possible: a host namespace is a
// per-kernel construct and each Pod is a different kernel. So #84 takes option
// (b): a kubelet-visible taint/label scheme that lets the scheduler route
// host-namespace system workloads to a Linux node, backed by the existing loud
// RunPodSandbox rejection for any workload that tolerates its way onto the node
// regardless.
//
// These keys are the single source of truth for the scheme. The adapter cannot
// set them itself — in CRI mode the kubelet owns the Node object — so the
// operator applies them at node registration (`--node-labels` /
// `--register-with-taints` for a raw kubelet, `--node-label` / `--node-taint`
// for a k3s agent). Because the NoSchedule taint repels ordinary Pods too,
// workloads that are intentionally MacVz-compatible must opt in with a matching
// toleration and should select NodeRuntimeLabel. The preflight advisory prints
// the exact node registration flags, and the RunPodSandbox rejection points back
// at this scheme, so the documented keys, the diagnostics, and
// docs/CRI_FEASIBILITY.md never drift apart.
const (
	// NodeRuntimeLabel identifies the node's container runtime. Cluster-owned
	// workloads can target or avoid the node by this value via nodeAffinity.
	NodeRuntimeLabel = "node.macvz.io/runtime"
	// NodeRuntimeLabelValue is the value advertised for the apple/container
	// micro-VM runtime backing this CRI adapter.
	NodeRuntimeLabelValue = "apple-container"

	// NodeHostNamespaceLabel declares whether host-namespace Pods are honored.
	// It is always "unsupported" on this runtime; a workload that must run on a
	// node with host namespaces can require this label be absent (or use a
	// `NotIn`/`DoesNotExist` nodeAffinity) to schedule elsewhere.
	NodeHostNamespaceLabel = "node.macvz.io/host-namespace"
	// NodeHostNamespaceUnsupported is the only value the runtime ever sets for
	// NodeHostNamespaceLabel.
	NodeHostNamespaceUnsupported = "unsupported"

	// NodeHostNamespaceTaintKey repels Pods that do not explicitly tolerate a
	// host-namespace-incapable node. It uses NoSchedule (not NoExecute) so a Pod
	// the operator deliberately tolerates onto the node is not evicted; the
	// adapter's RunPodSandbox rejection remains the honest backstop for any Pod
	// that both tolerates the taint and still requests a host namespace.
	NodeHostNamespaceTaintKey    = "node.macvz.io/host-namespace-unsupported"
	NodeHostNamespaceTaintValue  = "true"
	NodeHostNamespaceTaintEffect = "NoSchedule"
)

// NodeLabels returns the canonical node labels the operator should register the
// kubelet/k3s node with, as ordered key=value pairs.
func NodeLabels() []string {
	return []string{
		NodeRuntimeLabel + "=" + NodeRuntimeLabelValue,
		NodeHostNamespaceLabel + "=" + NodeHostNamespaceUnsupported,
	}
}

// NodeTaint returns the canonical scheduling taint in `key=value:effect` form.
func NodeTaint() string {
	return fmt.Sprintf("%s=%s:%s",
		NodeHostNamespaceTaintKey, NodeHostNamespaceTaintValue, NodeHostNamespaceTaintEffect)
}

// hostNamespaceSchedulingHint is appended to the RunPodSandbox rejection so an
// operator who sees a host-namespace Pod fail is pointed at the honest fix
// rather than left to guess. It names the scheme keys so the message, the
// preflight advisory, and the docs stay in lockstep.
func hostNamespaceSchedulingHint() string {
	return fmt.Sprintf(
		"schedule host-namespace workloads on a Linux node instead; register this node with taint %q and label %q=%q so the scheduler can route them elsewhere (scope a cluster-owned DaemonSet away with a nodeAffinity that excludes %s)",
		NodeTaint(), NodeHostNamespaceLabel, NodeHostNamespaceUnsupported, NodeHostNamespaceLabel)
}
