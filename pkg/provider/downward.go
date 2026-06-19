package provider

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// resolveDownwardField resolves a Downward API env source (fieldRef or
// resourceFieldRef) against the Pod and container, returning the string value
// Kubernetes would inject (#48). Sources whose value is not known until after
// the micro-VM is scheduled — notably status.podIP — return a plain error so the
// caller surfaces a terminal, clear validation message rather than an empty
// string. cms-backed sources (configMap/secret) are handled by resolveEnv.
func resolveDownwardField(pod *corev1.Pod, c corev1.Container, src *corev1.EnvVarSource) (string, error) {
	switch {
	case src.FieldRef != nil:
		return resolveFieldRef(pod, src.FieldRef.FieldPath)
	case src.ResourceFieldRef != nil:
		return resolveResourceFieldRef(c, src.ResourceFieldRef)
	default:
		return "", fmt.Errorf("valueFrom has no fieldRef or resourceFieldRef source")
	}
}

// resolveFieldRef resolves a Downward API fieldRef path against the Pod. It
// supports the metadata.* and spec.* paths whose values are stable at
// translation time (the Pod is already bound to this node, so spec.nodeName is
// populated). status.* paths depend on runtime state assigned after translation
// and are rejected explicitly.
func resolveFieldRef(pod *corev1.Pod, path string) (string, error) {
	switch path {
	case "metadata.name":
		return pod.Name, nil
	case "metadata.namespace":
		return pod.Namespace, nil
	case "metadata.uid":
		return string(pod.UID), nil
	case "spec.nodeName":
		return pod.Spec.NodeName, nil
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName, nil
	}
	if key, ok := subscriptKey(path, "metadata.labels"); ok {
		return pod.Labels[key], nil
	}
	if key, ok := subscriptKey(path, "metadata.annotations"); ok {
		return pod.Annotations[key], nil
	}
	// status.hostIP/podIP/podIPs are assigned after the micro-VM is scheduled and
	// networked, so they cannot be resolved during translation; reject them (and
	// any unknown path) with a clear, terminal error.
	return "", fmt.Errorf("fieldRef %q is not supported", path)
}

// resolveResourceFieldRef resolves a Downward API resourceFieldRef against the
// container's resources, applying the divisor and rounding up, matching the
// Kubernetes Downward API (e.g. a 500m CPU limit with the default divisor of 1
// resolves to "1"). An unset resource resolves to "0".
func resolveResourceFieldRef(c corev1.Container, ref *corev1.ResourceFieldSelector) (string, error) {
	divisor := ref.Divisor
	if divisor.IsZero() {
		divisor = resource.MustParse("1")
	}
	switch ref.Resource {
	case "limits.cpu":
		return divideCPU(c.Resources.Limits.Cpu(), divisor), nil
	case "requests.cpu":
		return divideCPU(c.Resources.Requests.Cpu(), divisor), nil
	case "limits.memory":
		return divideBytes(c.Resources.Limits.Memory(), divisor), nil
	case "requests.memory":
		return divideBytes(c.Resources.Requests.Memory(), divisor), nil
	case "limits.ephemeral-storage":
		return divideBytes(c.Resources.Limits.StorageEphemeral(), divisor), nil
	case "requests.ephemeral-storage":
		return divideBytes(c.Resources.Requests.StorageEphemeral(), divisor), nil
	default:
		return "", fmt.Errorf("resourceFieldRef %q is not supported", ref.Resource)
	}
}

// divideCPU returns ceil(cpu / divisor) computed in milli-units, matching the
// Kubernetes Downward API's CPU rounding.
func divideCPU(cpu *resource.Quantity, divisor resource.Quantity) string {
	d := divisor.MilliValue()
	if d <= 0 {
		d = 1
	}
	return strconv.FormatInt(int64(math.Ceil(float64(cpu.MilliValue())/float64(d))), 10)
}

// divideBytes returns ceil(quantity / divisor) in whole bytes, matching the
// Kubernetes Downward API for memory and ephemeral-storage.
func divideBytes(q *resource.Quantity, divisor resource.Quantity) string {
	d := divisor.Value()
	if d <= 0 {
		d = 1
	}
	return strconv.FormatInt(int64(math.Ceil(float64(q.Value())/float64(d))), 10)
}

// subscriptKey extracts the quoted key from a field path of the form
// prefix['key'] (e.g. metadata.labels['app'] → "app"), supporting single or
// double quotes as Kubernetes does.
func subscriptKey(path, prefix string) (string, bool) {
	if !strings.HasPrefix(path, prefix+"[") || !strings.HasSuffix(path, "]") {
		return "", false
	}
	inner := path[len(prefix)+1 : len(path)-1]
	if len(inner) >= 2 {
		if first, last := inner[0], inner[len(inner)-1]; (first == '\'' && last == '\'') || (first == '"' && last == '"') {
			return inner[1 : len(inner)-1], true
		}
	}
	return "", false
}

// expandEnv substitutes $(VAR) references in value using the variables resolved
// so far, exactly as Kubernetes does: $$ is an escaped literal '$', $(VAR) with
// a known VAR is substituted, and an unknown or syntactically incomplete
// reference is left verbatim. Only literal env values are expanded; envFrom and
// valueFrom values are used as-is.
func expandEnv(value string, vars map[string]string) string {
	if !strings.Contains(value, "$") {
		return value
	}
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '$' || i+1 >= len(value) {
			b.WriteByte(value[i])
			continue
		}
		switch value[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '(':
			end := strings.IndexByte(value[i+2:], ')')
			if end < 0 { // unterminated: emit '$' literally, scan the rest normally
				b.WriteByte('$')
				continue
			}
			name := value[i+2 : i+2+end]
			if v, ok := vars[name]; ok {
				b.WriteString(v)
			} else { // unknown variable: leave $(name) verbatim
				b.WriteString(value[i : i+3+end])
			}
			i += end + 2
		default:
			b.WriteByte('$')
		}
	}
	return b.String()
}
