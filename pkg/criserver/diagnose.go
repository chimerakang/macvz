package criserver

import (
	"strings"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// This file adds CRI-P8 operator diagnostics for Pod shapes the experimental
// single-container micro-VM model cannot honor (#80). apple/container runs each
// Pod sandbox as an isolated Linux micro-VM with its own network, PID, and IPC
// namespaces, so a Pod that asks to share the host's namespaces cannot be
// satisfied. Booting it anyway would silently ignore the request and mislead the
// operator into believing host sharing worked — exactly the kind of fake success
// the CRI track forbids. Instead the adapter rejects the shape with a clear,
// operator-facing reason naming the offending field.
//
// The CRI-P9 follow-up (#84) pairs this rejection with a kubelet-visible
// taint/label scheme (nodescheme.go) so host-namespace system workloads are
// scheduled onto a Linux node instead of failing here; the rejection message
// points back at that scheme so an operator who still sees one fail knows the
// honest fix. See docs/CRI_FEASIBILITY.md, "CRI-P9 Follow-up (#84)".

// unsupportedSandboxShape inspects a Pod sandbox config for shapes the
// single-container micro-VM model cannot honor honestly. It returns a clear
// reason and true when the shape is unsupported, or "" and false when the Pod is
// representable. The reasons name the Kubernetes Pod spec field an operator would
// recognise (hostNetwork/hostPID/hostIPC) so the diagnostic is actionable.
func unsupportedSandboxShape(cfg *runtimeapi.PodSandboxConfig) (string, bool) {
	ns := cfg.GetLinux().GetSecurityContext().GetNamespaceOptions()
	if ns == nil {
		return "", false
	}
	var reasons []string
	if ns.GetNetwork() == runtimeapi.NamespaceMode_NODE {
		reasons = append(reasons,
			"hostNetwork (spec.hostNetwork=true): the Pod runs in an isolated micro-VM network namespace and cannot share the host network")
	}
	if ns.GetPid() == runtimeapi.NamespaceMode_NODE {
		reasons = append(reasons,
			"hostPID (spec.hostPID=true): the micro-VM cannot share the host PID namespace")
	}
	if ns.GetIpc() == runtimeapi.NamespaceMode_NODE {
		reasons = append(reasons,
			"hostIPC (spec.hostIPC=true): the micro-VM cannot share the host IPC namespace")
	}
	if len(reasons) == 0 {
		return "", false
	}
	return strings.Join(reasons, "; "), true
}
