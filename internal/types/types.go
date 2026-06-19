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
	// Mounts are the filesystem mounts exposed inside the micro-VM, in the order
	// they should be applied. The runtime realizes each as a VirtioFS share (or a
	// guest tmpfs); the provider is responsible for validating sources.
	Mounts []Mount

	// DNS are nameserver IPs written into the micro-VM's resolv.conf (passed as
	// `--dns`). Empty leaves the image's baked DNS in place. Cluster DNS lets the
	// guest resolve Service names (#37).
	DNS []string
	// DNSSearch are resolv.conf search domains (passed as `--dns-search`).
	DNSSearch []string
	// DNSOptions are resolv.conf options such as "ndots:5" (passed as
	// `--dns-option`).
	DNSOptions []string

	// User sets the guest process user as "uid" or "uid:gid", translated from the
	// Pod's securityContext runAsUser/runAsGroup (#52). Empty runs the image's
	// configured user.
	User string
	// ReadOnlyRootFS mounts the guest root filesystem read-only, translated from
	// securityContext.readOnlyRootFilesystem (#52).
	ReadOnlyRootFS bool
	// CapAdd and CapDrop are Linux capabilities to grant or remove for the guest
	// process, translated from securityContext.capabilities (#52). Names are in
	// the runtime's form (e.g. "CAP_NET_ADMIN", or "ALL").
	CapAdd  []string
	CapDrop []string
}

// Mount describes one filesystem mount inside the micro-VM. A bind mount shares
// a host directory into the guest over VirtioFS; a tmpfs mount allocates
// in-guest memory-backed storage with no host source.
type Mount struct {
	// Source is the host path shared into the guest. It is ignored when Tmpfs is
	// set, and required otherwise.
	Source string
	// Target is the absolute mount path inside the micro-VM.
	Target string
	// ReadOnly mounts the source read-only in the guest.
	ReadOnly bool
	// Tmpfs requests a guest-local tmpfs at Target instead of a host bind mount.
	Tmpfs bool
}
