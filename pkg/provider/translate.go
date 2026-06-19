package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/chimerakang/macvz/internal/types"
	corev1 "k8s.io/api/core/v1"
)

// maxWorkloadIDLen bounds the derived workload name to a DNS label (RFC 1123),
// which is the strictest constraint apple/container and Kubernetes share.
const maxWorkloadIDLen = 63

// workloadIDPrefix namespaces every workload this node creates, so MacVz
// workloads are easy to distinguish from anything else on the host.
const workloadIDPrefix = "macvz"

// translatePod validates that a Pod is a shape MacVz can run in the MVP and
// translates its single container into a runtime ContainerSpec. Unsupported
// shapes return a clear error so the caller can surface a Kubernetes-facing
// Failed status rather than silently running a partial workload.
func translatePod(pod *corev1.Pod, policy VolumePolicy, dns DNSConfig) (types.ContainerSpec, resolvedVolumes, error) {
	if reasons := unsupportedReasons(pod); len(reasons) > 0 {
		return types.ContainerSpec{}, resolvedVolumes{}, fmt.Errorf("unsupported Pod spec: %s", strings.Join(reasons, "; "))
	}
	vols, err := resolveVolumes(pod, policy)
	if err != nil {
		return types.ContainerSpec{}, resolvedVolumes{}, fmt.Errorf("unsupported Pod spec: %v", err)
	}
	c := pod.Spec.Containers[0]
	spec := translateContainer(pod, c)
	spec.Name = workloadID(pod.Namespace, pod.Name, c.Name)
	spec.Mounts = vols.mounts
	spec.DNS, spec.DNSSearch, spec.DNSOptions = resolveDNS(pod, dns)
	return spec, vols, nil
}

// unsupportedReasons returns a human-readable reason for every feature in the
// Pod that the MVP cannot honor. An empty result means the Pod is supported.
func unsupportedReasons(pod *corev1.Pod) []string {
	var reasons []string

	switch len(pod.Spec.Containers) {
	case 0:
		reasons = append(reasons, "Pod has no containers")
	case 1:
	default:
		reasons = append(reasons, fmt.Sprintf("multi-container Pods are not supported yet (found %d containers; the MVP runs a single container)", len(pod.Spec.Containers)))
	}
	if n := len(pod.Spec.InitContainers); n > 0 {
		reasons = append(reasons, fmt.Sprintf("init containers are not supported yet (found %d)", n))
	}
	if n := len(pod.Spec.EphemeralContainers); n > 0 {
		reasons = append(reasons, fmt.Sprintf("ephemeral containers are not supported yet (found %d)", n))
	}
	if pod.Spec.RestartPolicy != "" && pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		reasons = append(reasons, fmt.Sprintf("restartPolicy %q is not supported yet (only Never is supported in the MVP)", pod.Spec.RestartPolicy))
	}

	// Volume sources are validated by resolveVolumes against the node's volume
	// policy (#26); the shape checks here stay focused on Pod-level features.

	if pod.Spec.SecurityContext != nil && !isEmptyPodSecurityContext(pod.Spec.SecurityContext) {
		reasons = append(reasons, "pod-level securityContext is not supported yet")
	}
	if pod.Spec.HostNetwork {
		reasons = append(reasons, "hostNetwork is not supported")
	}
	if pod.Spec.HostPID {
		reasons = append(reasons, "hostPID is not supported")
	}
	if pod.Spec.HostIPC {
		reasons = append(reasons, "hostIPC is not supported")
	}

	for _, c := range pod.Spec.Containers {
		if c.SecurityContext != nil {
			reasons = append(reasons, fmt.Sprintf("container %q securityContext is not supported yet", c.Name))
		}
		if len(c.EnvFrom) > 0 {
			reasons = append(reasons, fmt.Sprintf("container %q envFrom is not supported yet", c.Name))
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil {
				reasons = append(reasons, fmt.Sprintf("container %q env %q uses valueFrom, which is not supported yet", c.Name, e.Name))
			}
		}
	}

	return reasons
}

// translateContainer maps a single Kubernetes container into a ContainerSpec.
// The caller is responsible for setting spec.Name to the stable workload ID.
func translateContainer(pod *corev1.Pod, c corev1.Container) types.ContainerSpec {
	spec := types.ContainerSpec{
		Name:    c.Name,
		Image:   c.Image,
		Command: c.Command,
		Args:    c.Args,
	}

	if len(c.Env) > 0 {
		spec.Env = make(map[string]string, len(c.Env))
		for _, e := range c.Env {
			if e.ValueFrom == nil { // valueFrom is rejected in unsupportedReasons
				spec.Env[e.Name] = e.Value
			}
		}
	}

	// CPU/memory: prefer the limit, fall back to the request, as the VM sizing.
	if cpu := resourceQuantity(c, corev1.ResourceCPU); cpu != nil {
		spec.CPUMillis = cpu.MilliValue()
	}
	if mem := resourceQuantity(c, corev1.ResourceMemory); mem != nil {
		spec.MemoryBytes = mem.Value()
	}
	return spec
}

// workloadID derives a stable, DNS-safe (RFC 1123 label) workload name from the
// Pod's identity. The same namespace/name/container always yields the same ID;
// when the joined name would exceed the label limit it is truncated and made
// unique with a short hash of the full identity.
func workloadID(namespace, podName, containerName string) string {
	id := sanitizeDNSLabel(strings.Join([]string{workloadIDPrefix, namespace, podName, containerName}, "-"))
	if len(id) <= maxWorkloadIDLen {
		return id
	}
	sum := sha256.Sum256([]byte(namespace + "/" + podName + "/" + containerName))
	suffix := hex.EncodeToString(sum[:])[:8]
	keep := maxWorkloadIDLen - len(suffix) - 1
	return strings.TrimRight(id[:keep], "-") + "-" + suffix
}

// sanitizeDNSLabel lowercases s and replaces any run of characters outside
// [a-z0-9-] with a single hyphen, trimming leading/trailing hyphens so the
// result is a valid DNS label.
func sanitizeDNSLabel(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	lastDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "wl"
	}
	return out
}

// isDefaultProjectedToken reports whether v is the service-account access token
// volume that Kubernetes auto-injects (so it can be tolerated, not rejected).
func isDefaultProjectedToken(v corev1.Volume) bool {
	if strings.HasPrefix(v.Name, "kube-api-access-") {
		return true
	}
	if v.Projected != nil {
		for _, s := range v.Projected.Sources {
			if s.ServiceAccountToken != nil {
				return true
			}
		}
	}
	return false
}

// isEmptyPodSecurityContext reports whether a pod securityContext carries no
// meaningful settings. kubectl run leaves it nil, but other tooling may attach
// an empty struct that should not trip the unsupported check.
func isEmptyPodSecurityContext(sc *corev1.PodSecurityContext) bool {
	if sc == nil {
		return true
	}
	return sc.RunAsUser == nil &&
		sc.RunAsGroup == nil &&
		sc.RunAsNonRoot == nil &&
		sc.FSGroup == nil &&
		sc.SELinuxOptions == nil &&
		sc.WindowsOptions == nil &&
		len(sc.SupplementalGroups) == 0 &&
		len(sc.Sysctls) == 0 &&
		sc.SeccompProfile == nil
}

// resourceQuantity returns the limit for a resource, falling back to the
// request, or nil when neither is set.
func resourceQuantity(c corev1.Container, name corev1.ResourceName) *resourceValue {
	if q, ok := c.Resources.Limits[name]; ok {
		return &resourceValue{q.MilliValue(), q.Value()}
	}
	if q, ok := c.Resources.Requests[name]; ok {
		return &resourceValue{q.MilliValue(), q.Value()}
	}
	return nil
}

// resourceValue carries both the milli and whole-unit views of a quantity so
// callers can pick the appropriate one without re-parsing.
type resourceValue struct {
	milli int64
	value int64
}

func (r resourceValue) MilliValue() int64 { return r.milli }
func (r resourceValue) Value() int64      { return r.value }
