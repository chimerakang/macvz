package provider

import (
	"context"

	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodeSpec is the resolved shape advertised to Kubernetes when the node is
// registered. The config package translates user YAML into this struct so the
// provider stays free of parsing and YAML concerns.
type NodeSpec struct {
	// KubeletVersion is reported in node info (the macvz-kubelet build version).
	KubeletVersion string
	// OS and Arch describe the workload platform (linux/arm64), not the host.
	OS, Arch string
	// InternalIP is the node's reachable address; omitted when empty.
	InternalIP string
	// Capacity is used for both capacity and allocatable (no reservation in MVP).
	Capacity corev1.ResourceList
	// Labels and Annotations are merged over the built-in node metadata.
	Labels, Annotations map[string]string
	// Taints gate scheduling so only Pods that tolerate MacVz land here.
	Taints []corev1.Taint
}

// BuildNode assembles the corev1.Node this provider registers. The Ready
// condition reflects live runtime readiness when the runtime supports
// runtime.Pinger; ongoing heartbeat/lease updates are handled by the node
// controller (#15).
func (p *Provider) BuildNode(ctx context.Context, spec NodeSpec) *corev1.Node {
	ready, readyMsg := p.runtimeReady(ctx)

	labels := map[string]string{
		"type":                   "virtual-kubelet",
		"kubernetes.io/role":     "agent",
		"kubernetes.io/hostname": p.nodeName,
		"kubernetes.io/os":       spec.OS,
		"kubernetes.io/arch":     spec.Arch,
	}
	for k, v := range spec.Labels {
		labels[k] = v
	}

	annotations := map[string]string{}
	for k, v := range spec.Annotations {
		annotations[k] = v
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        p.nodeName,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.NodeSpec{
			Taints: spec.Taints,
		},
		Status: corev1.NodeStatus{
			Capacity:    spec.Capacity,
			Allocatable: spec.Capacity,
			NodeInfo: corev1.NodeSystemInfo{
				OperatingSystem: spec.OS,
				Architecture:    spec.Arch,
				KubeletVersion:  spec.KubeletVersion,
			},
			Conditions: nodeConditions(ready, readyMsg),
		},
	}

	if spec.InternalIP != "" {
		node.Status.Addresses = []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: spec.InternalIP},
			{Type: corev1.NodeHostName, Address: p.nodeName},
		}
	} else {
		node.Status.Addresses = []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: p.nodeName},
		}
	}

	return node
}

// runtimeReady probes the runtime for readiness when it implements
// runtime.Pinger. Runtimes without the capability are assumed ready.
func (p *Provider) runtimeReady(ctx context.Context) (ready bool, message string) {
	pinger, ok := p.rt.(runtime.Pinger)
	if !ok {
		return true, "runtime readiness probe unavailable; assuming ready"
	}
	if err := pinger.Ready(ctx); err != nil {
		return false, err.Error()
	}
	return true, "macvz-kubelet and apple/container runtime are ready"
}

// nodeConditions builds a coherent condition set for `kubectl describe node`.
// Ready tracks runtime readiness; the pressure conditions are reported False
// because micro-VM workloads do not consume the host kubelet's resources.
func nodeConditions(ready bool, readyMsg string) []corev1.NodeCondition {
	now := metav1.Now()
	readyStatus := corev1.ConditionFalse
	readyReason := "RuntimeNotReady"
	if ready {
		readyStatus = corev1.ConditionTrue
		readyReason = "KubeletReady"
	}

	cond := func(t corev1.NodeConditionType, s corev1.ConditionStatus, reason, msg string) corev1.NodeCondition {
		return corev1.NodeCondition{
			Type:               t,
			Status:             s,
			Reason:             reason,
			Message:            msg,
			LastHeartbeatTime:  now,
			LastTransitionTime: now,
		}
	}

	return []corev1.NodeCondition{
		cond(corev1.NodeReady, readyStatus, readyReason, readyMsg),
		cond(corev1.NodeMemoryPressure, corev1.ConditionFalse, "KubeletHasSufficientMemory", "kubelet has sufficient memory available"),
		cond(corev1.NodeDiskPressure, corev1.ConditionFalse, "KubeletHasNoDiskPressure", "kubelet has no disk pressure"),
		cond(corev1.NodePIDPressure, corev1.ConditionFalse, "KubeletHasSufficientPID", "kubelet has sufficient PID available"),
		cond(corev1.NodeNetworkUnavailable, corev1.ConditionFalse, "RouteCreated", "node network is configured"),
	}
}
