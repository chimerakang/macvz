package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
func translatePod(ctx context.Context, pod *corev1.Pod, policy VolumePolicy, dns DNSConfig, cms ConfigMapGetter, secrets SecretGetter, tokens TokenRequester) (types.ContainerSpec, resolvedVolumes, error) {
	if reasons := unsupportedReasons(pod); len(reasons) > 0 {
		return types.ContainerSpec{}, resolvedVolumes{}, fmt.Errorf("unsupported Pod spec: %s", strings.Join(reasons, "; "))
	}
	vols, err := resolveVolumes(ctx, pod, policy, cms, secrets, tokens)
	if err != nil {
		// An unresolved ConfigMap/Secret dependency (errConfigPending /
		// errSecretUnavailable) must propagate untouched so CreatePod can
		// distinguish "retry later" from a terminal "unsupported" shape; only wrap
		// the unsupported-volume case.
		if errors.Is(err, errConfigPending) || errors.Is(err, errSecretUnavailable) {
			return types.ContainerSpec{}, resolvedVolumes{}, err
		}
		return types.ContainerSpec{}, resolvedVolumes{}, fmt.Errorf("unsupported Pod spec: %v", err)
	}
	c := pod.Spec.Containers[0]
	spec := translateContainer(pod, c)
	env, err := resolveEnv(pod, c, cms, secrets)
	if err != nil {
		return types.ContainerSpec{}, resolvedVolumes{}, err
	}
	if env != nil {
		spec.Env = env
	}
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
	// All three Kubernetes restart policies are honored: Always and OnFailure
	// drive the provider's micro-VM restart loop (see restart.go), Never leaves a
	// terminated workload terminal. Only an unrecognized value is rejected; the
	// API server defaults an unset policy to Always before the Pod reaches us.
	switch pod.Spec.RestartPolicy {
	case "", corev1.RestartPolicyAlways, corev1.RestartPolicyOnFailure, corev1.RestartPolicyNever:
	default:
		reasons = append(reasons, fmt.Sprintf("restartPolicy %q is not a recognized value (use Always, OnFailure, or Never)", pod.Spec.RestartPolicy))
	}

	// Volume sources are validated by resolveVolumes against the node's volume
	// policy (#26); the shape checks here stay focused on Pod-level features.

	// securityContext is honored field-by-field (#52): supported fields are mapped
	// onto the runtime in translateContainer, and unsupported ones are rejected
	// here with a precise reason instead of silently no-opping.
	reasons = append(reasons, securityContextReasons(pod)...)

	if pod.Spec.HostNetwork {
		reasons = append(reasons, "hostNetwork is not supported")
	}
	if pod.Spec.HostPID {
		reasons = append(reasons, "hostPID is not supported")
	}
	if pod.Spec.HostIPC {
		reasons = append(reasons, "hostIPC is not supported")
	}

	// env/envFrom sources — ConfigMap (#46), Secret (#47), and the downward-API
	// sources fieldRef/resourceFieldRef (#48) — are all resolved in resolveEnv,
	// which surfaces a precise error for an unsupported fieldRef path (e.g.
	// status.podIP) or a missing required ConfigMap/Secret. No shape rejection is
	// needed here.

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

	// securityContext: map the supported fields (user/group, read-only root,
	// capabilities) onto the spec. Unsupported fields are already rejected by
	// unsupportedReasons, so the Pod never reaches here with one (#52).
	applySecurityContext(pod, c, &spec)
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
