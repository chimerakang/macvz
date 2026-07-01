package criserver

import (
	"fmt"
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
	var reasons []string
	if ns := cfg.GetLinux().GetSecurityContext().GetNamespaceOptions(); ns != nil {
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
	}
	// kubelet sends a PortMapping for every declared containerPort; only a
	// non-zero HostPort is an actual host-port-forward request. The adapter
	// programs no host port forwards, so accepting one would let the Pod run
	// while the mapping silently never exists — reject it instead (#162).
	hostPorts := 0
	for _, pm := range cfg.GetPortMappings() {
		if pm.GetHostPort() != 0 {
			hostPorts++
		}
	}
	if hostPorts > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"hostPort (spec.containers[].ports[].hostPort): %d host port mapping(s) requested, but the adapter does not program host port forwards; use a ClusterIP Service or kubectl port-forward instead", hostPorts))
	}
	// Explicit-only requests below: kubelet never sets these for a vanilla Pod,
	// so rejecting them cannot break ordinary workloads — only surface requests
	// that would otherwise be silently dropped (#162).
	if sysctls := cfg.GetLinux().GetSysctls(); len(sysctls) > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"sysctls (spec.securityContext.sysctls): %d sysctl(s) requested, but the adapter does not apply kernel tunables in the Pod VM", len(sysctls)))
	}
	if sc := cfg.GetLinux().GetSecurityContext(); sc != nil {
		if sc.GetPrivileged() {
			reasons = append(reasons,
				"privileged (spec.containers[].securityContext.privileged=true): the micro-VM model does not grant host-privileged access")
		}
		if userns := sc.GetNamespaceOptions().GetUsernsOptions(); userns != nil && userns.GetMode() == runtimeapi.NamespaceMode_POD {
			reasons = append(reasons,
				"user namespaces (spec.hostUsers=false): the adapter does not apply UID/GID mappings inside the Pod VM")
		}
	}
	if len(reasons) == 0 {
		return "", false
	}
	return strings.Join(reasons, "; "), true
}

// unsupportedContainerShape is the container-level counterpart: it inspects a
// ContainerConfig for explicit, non-default requests the LinuxPod micro-VM
// model cannot honor. The zero-value discipline matters — kubelet sends
// security_context with privileged=false and pid=CONTAINER for every vanilla
// container, so only explicit non-default values may reject (#162).
func unsupportedContainerShape(cfg *runtimeapi.ContainerConfig) (string, bool) {
	var reasons []string
	if len(cfg.GetDevices()) > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"devices: %d host device(s) requested, but the adapter cannot pass host devices into the Pod VM", len(cfg.GetDevices())))
	}
	if len(cfg.GetCDIDevices()) > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"CDI devices: %d CDI device(s) requested, but the adapter cannot pass host devices into the Pod VM", len(cfg.GetCDIDevices())))
	}
	if sc := cfg.GetLinux().GetSecurityContext(); sc != nil {
		if sc.GetPrivileged() {
			reasons = append(reasons,
				"privileged (securityContext.privileged=true): the micro-VM model does not grant host-privileged access")
		}
		if ns := sc.GetNamespaceOptions(); ns.GetPid() == runtimeapi.NamespaceMode_TARGET {
			reasons = append(reasons,
				"targeted PID namespace (ephemeral debug container targeting another container): the LinuxPod helper cannot join a specific container's PID namespace")
		}
	}
	if len(reasons) == 0 {
		return "", false
	}
	return strings.Join(reasons, "; "), true
}

// ignoredContainerFields lists explicit container requests the LinuxPod path
// accepts but does not enforce. Rejecting these would break ordinary hardened
// workloads (runAsUser / readOnlyRootFilesystem / seccomp RuntimeDefault /
// allowPrivilegeEscalation:false / resource limits are standard chart
// boilerplate), so the honest posture is a logged warning per create, plus the
// support matrix in docs/CRI_FIELD_SUPPORT.md. Zero-value discipline: fields
// kubelet always populates (cpu_shares, oom_score_adj, masked_paths,
// pid=CONTAINER) are never reported.
func ignoredContainerFields(cfg *runtimeapi.ContainerConfig) []string {
	var warns []string
	if wd := cfg.GetWorkingDir(); wd != "" {
		warns = append(warns, fmt.Sprintf("workingDir %q is not applied; the process runs in the image default", wd))
	}
	if cfg.GetTty() || cfg.GetStdin() {
		warns = append(warns, "tty/stdin are not attached for the main process (interactive stdio is exec-only)")
	}
	if res := cfg.GetLinux().GetResources(); res != nil {
		if res.GetMemoryLimitInBytes() > 0 || res.GetCpuQuota() > 0 ||
			res.GetCpusetCpus() != "" || res.GetCpusetMems() != "" ||
			len(res.GetHugepageLimits()) > 0 || len(res.GetUnified()) > 0 {
			warns = append(warns, "container resource limits are not enforced inside the Pod VM (the VM is sized from the Pod-level resource sum)")
		}
	}
	if sc := cfg.GetLinux().GetSecurityContext(); sc != nil {
		if sc.GetRunAsUser() != nil || sc.GetRunAsGroup() != nil || sc.GetRunAsUsername() != "" {
			warns = append(warns, "runAsUser/runAsGroup is not applied; the process runs as the image default user")
		}
		if sc.GetReadonlyRootfs() {
			warns = append(warns, "readOnlyRootFilesystem=true is not enforced; the container rootfs stays writable")
		}
		if caps := sc.GetCapabilities(); len(caps.GetAddCapabilities()) > 0 || len(caps.GetDropCapabilities()) > 0 {
			warns = append(warns, "capability add/drop lists are not applied in the Pod VM")
		}
		if sc.GetNoNewPrivs() {
			warns = append(warns, "allowPrivilegeEscalation=false (no_new_privs) is not enforced")
		}
		if p := sc.GetSeccomp(); p != nil && p.GetProfileType() != runtimeapi.SecurityProfile_Unconfined {
			warns = append(warns, "seccomp profile is not applied inside the Pod VM")
		}
		if p := sc.GetApparmor(); p != nil && p.GetProfileType() != runtimeapi.SecurityProfile_Unconfined {
			warns = append(warns, "AppArmor profile is not applied (no AppArmor in the guest kernel)")
		}
		if ns := sc.GetNamespaceOptions(); ns.GetPid() == runtimeapi.NamespaceMode_POD {
			warns = append(warns, "shareProcessNamespace: containers in one LinuxPod VM share the VM but not a merged PID namespace")
		}
		if len(sc.GetSupplementalGroups()) > 0 {
			warns = append(warns, "supplementalGroups are not applied")
		}
	}
	return warns
}
