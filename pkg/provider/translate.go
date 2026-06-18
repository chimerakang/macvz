package provider

import (
	"github.com/chimerakang/macvz/internal/types"
	corev1 "k8s.io/api/core/v1"
)

// translateContainer maps a Kubernetes container into the runtime's
// ContainerSpec. This is the minimal translation needed for the lifecycle CRUD
// (#16): image, command/args, literal env, and CPU/memory requests. Richer
// translation — env valueFrom, volumes, probes, downward API — is the subject
// of #17.
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
			// valueFrom (configmap/secret/fieldRef) is deferred to #17; only
			// literal values are carried here.
			if e.ValueFrom == nil {
				spec.Env[e.Name] = e.Value
			}
		}
	}

	// Prefer limits, fall back to requests, for the VM sizing hints.
	if cpu := resourceQuantity(c, corev1.ResourceCPU); cpu != nil {
		spec.CPUMillis = cpu.MilliValue()
	}
	if mem := resourceQuantity(c, corev1.ResourceMemory); mem != nil {
		spec.MemoryBytes = mem.Value()
	}
	return spec
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
