// Package types holds small value types shared between the runtime driver and
// the Virtual Kubelet provider, so neither package needs to import the other.
package types

// PodRef identifies a Kubernetes Pod by namespace and name.
type PodRef struct {
	Namespace string
	Name      string
}

// String returns the "namespace/name" form.
func (r PodRef) String() string {
	return r.Namespace + "/" + r.Name
}

// ContainerSpec is the minimal description the runtime needs to launch one
// workload as a micro-VM. It is intentionally decoupled from the Kubernetes API
// types; the provider translates a Pod into one or more ContainerSpecs.
type ContainerSpec struct {
	// Name is unique within a Pod.
	Name string
	// Image is an OCI image reference (e.g. "docker.io/library/alpine:3.20").
	Image string
	// Command overrides the image entrypoint when non-empty.
	Command []string
	// Args overrides the image CMD when non-empty.
	Args []string
	// Env is the environment passed to the workload.
	Env map[string]string
	// CPUMillis is the CPU request in milli-cores (1000 = 1 vCPU); 0 means unset.
	CPUMillis int64
	// MemoryBytes is the memory request/limit in bytes; 0 means unset.
	MemoryBytes int64
}
