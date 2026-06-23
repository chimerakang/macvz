// Package linuxpod defines the smallest experimental LinuxPod runtime backend
// contract that the Go macvz-cri adapter can call, plus a helper client that
// speaks it over a socket and an in-process fake helper for hermetic tests
// (CRI-R17, #124).
//
// Motivation. The LinuxPod route (Apple Containerization) already proved the
// multi-container shared-namespace sandbox primitive (CRI LinuxPod C1/C2/C4,
// docs/CRI_LINUXPOD_FEASIBILITY.md) and the late-rootfs handoff identity channel
// (CRI-R15/R16, docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md). Those are PoC/harness
// results, not a runtime backend a Go CRI adapter can drive. This package turns
// them into one explicit, fakeable contract — without replacing the shipped
// apple/container CLI backend (it is selected only behind an experimental gate)
// and without claiming production readiness.
//
// The contract is pod-centric on purpose: a LinuxPod is one micro-VM hosting one
// or more Linux containers that share a network namespace (localhost). It models
// the kubelet ordering the issue requires:
//
//	CreatePod -> PrepareContainerRootfs(app) -> CreateContainer(app) -> StartContainer(app)
//	          -> PrepareContainerRootfs(sidecar) -> CreateContainer(sidecar) [after app started]
//	          -> StartContainer(sidecar) -> identityVerified -> Stop/Remove -> Cleanup
//
// PrepareContainerRootfs is the late-rootfs primitive (R8/R16, see
// docs/CRI_RUNTIME_I5_2_UPSTREAM_PROPOSAL.md §3): it stages a prepared rootfs and
// its expected identity into a running Pod VM so a container can be created and
// started after the Pod (and other containers) are already up. Identity
// verification reuses the production exact-match logic in pkg/runtime.
package linuxpod

import (
	"context"
	"errors"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// Sentinel errors the backend and helper protocol round-trip. They let the Go
// adapter branch on backend failures (e.g. treat a missing pod/container like the
// apple/container ErrNotFound) without string matching.
var (
	// ErrPodNotFound means no Pod VM is known for the given pod id.
	ErrPodNotFound = errors.New("linuxpod: pod not found")
	// ErrContainerNotFound means no container is known for the given ref.
	ErrContainerNotFound = errors.New("linuxpod: container not found")
	// ErrRootfsNotFound means no prepared rootfs is known for the given token.
	ErrRootfsNotFound = errors.New("linuxpod: prepared rootfs token not found")
	// ErrInvalid means the request was malformed (missing required field, duplicate
	// id, or a state-machine violation such as creating a container before its Pod).
	ErrInvalid = errors.New("linuxpod: invalid request")
	// ErrIdentityUnverified means StartContainer ran the late process but the rootfs
	// identity it reported did not match the expected identity staged at prepare
	// time. The container is left non-Running, mirroring CRI-R16 StartContainer.
	ErrIdentityUnverified = errors.New("linuxpod: rootfs identity not verified")
	// ErrUnsupported means the backend does not implement the requested kubelet
	// surface (logs, exec, or stats). The adapter branches on it to return an honest
	// CRI unsupported response rather than a generic failure, and it must never
	// wedge a lifecycle operation (CRI-L4, #129). A surface that is unsupported is
	// advertised up-front in HelperInfo.Capabilities so the adapter can skip the call
	// entirely; ErrUnsupported is the defense for a call made anyway.
	ErrUnsupported = errors.New("linuxpod: capability not supported")
)

// Backend is the minimal experimental LinuxPod lifecycle a Go CRI adapter calls.
// It is implemented by the in-process FakeBackend (tests) and by HelperClient
// (which forwards to a Swift helper over a socket). A real implementation drives
// Apple Containerization's LinuxPod; this package does not depend on it.
//
// Every method takes the owning pod id explicitly so the backend holds no
// ambient "current pod" state and a restarted adapter can address pods by id.
type Backend interface {
	// Ping returns helper identity/capability info for a startup handshake. It must
	// succeed before the adapter trusts the backend.
	Ping(ctx context.Context) (HelperInfo, error)

	// CreatePod boots the LinuxPod sandbox VM (the RunPodSandbox analog) and returns
	// its status, including the SandboxNamespace every container in the pod shares.
	CreatePod(ctx context.Context, spec PodSpec) (PodStatus, error)

	// PodStatus returns the Pod VM's current observed status, including its
	// SandboxAddress once the VM has acquired its host-reachable address (CRI-L3,
	// #128). It is the pod-level analog of Status: the adapter polls it after
	// CreatePod to discover the address the Pod network path attaches to, and
	// re-queries it on restart recovery. SandboxAddress is "" while the address is
	// not yet known, which the caller treats as a transient not-ready condition, not
	// a failure. Returns ErrPodNotFound for an unknown pod id.
	PodStatus(ctx context.Context, podID string) (PodStatus, error)

	// PrepareContainerRootfs stages a prepared rootfs and its expected identity into
	// an already-running Pod VM (the late-rootfs primitive). It returns a handle
	// (token) CreateContainer binds against. It may be called before or after other
	// containers in the pod have started.
	PrepareContainerRootfs(ctx context.Context, req RootfsRequest) (RootfsHandle, error)

	// CreateContainer late-binds a container onto a prepared rootfs but does not
	// start it. It succeeds even when the pod already has running containers — that
	// is the late-sidecar case the issue requires.
	CreateContainer(ctx context.Context, req CreateRequest) (ContainerStatus, error)

	// StartContainer starts a created container and gates Running on rootfs identity
	// verification: the late process must report the expected identity through the
	// handoff channel, else it returns ErrIdentityUnverified and the container is
	// left non-Running.
	StartContainer(ctx context.Context, ref Ref) (ContainerStatus, error)

	// StopContainer stops a running container, preserving its record (and identity
	// evidence) until RemoveContainer. Stopping an already-stopped container is a
	// no-op success.
	StopContainer(ctx context.Context, req StopRequest) (ContainerStatus, error)

	// RemoveContainer deletes a container's record and prepared rootfs. It is
	// idempotent: removing an unknown container is a success.
	RemoveContainer(ctx context.Context, ref Ref) error

	// Status returns the current container status, including identity-verification
	// and shared-namespace evidence.
	Status(ctx context.Context, ref Ref) (ContainerStatus, error)

	// Cleanup tears down the Pod VM and every container/rootfs/handoff artifact it
	// owns, returning a report. It is idempotent and must leave no stale state.
	Cleanup(ctx context.Context, podID string) (CleanupReport, error)

	// ContainerLogPath returns the CRI-format container log file the backend created
	// for a container, so the adapter can tail it for kubelet log requests. Returns
	// ErrUnsupported when Capabilities.Logs is false, ErrInvalid when the container
	// was created without a log path, and ErrContainerNotFound for an unknown ref. It
	// never mutates lifecycle state, so a log failure cannot wedge a Pod.
	ContainerLogPath(ctx context.Context, ref Ref) (LogInfo, error)

	// ExecSync runs a command to completion inside a running container and returns
	// its combined result (stdout, stderr, exit code) — the primitive kubelet uses
	// for exec liveness/readiness probes and non-interactive `kubectl exec`. Returns
	// ErrUnsupported when Capabilities.Exec is false. Interactive/streaming Exec
	// (#132), Attach, and PortForward (#131) are deliberately out of scope here
	// (#129 non-goals) and tracked as follow-ups.
	ExecSync(ctx context.Context, req ExecRequest) (ExecResult, error)

	// ContainerStats returns a point-in-time resource sample for one container for
	// kubelet summary stats. Returns ErrUnsupported when Capabilities.Stats is false.
	// A simulated backend marks the sample Simulated so the adapter never reports
	// modeled numbers as measured.
	ContainerStats(ctx context.Context, ref Ref) (ContainerStats, error)
}

// HelperInfo is the handshake result: which helper answered and what it supports.
type HelperInfo struct {
	// Name identifies the helper implementation (e.g. "linuxpod-helper-stub").
	Name string `json:"name"`
	// ProtocolVersion is the wire-protocol version the helper speaks.
	ProtocolVersion int `json:"protocolVersion"`
	// Simulated is true for a stub/fake that does not boot a real Pod VM, so the
	// adapter can log honestly that it is not driving real workloads.
	Simulated bool `json:"simulated"`
	// Capabilities advertises which kubelet-facing runtime surfaces this helper
	// backs. The adapter reads it once at the startup handshake and only calls a
	// surface the helper advertises (CRI-L4, #129).
	Capabilities Capabilities `json:"capabilities"`
}

// Capabilities reports which kubelet-facing runtime surfaces a helper backs.
// Calling a surface that is false returns ErrUnsupported rather than wedging the
// Pod lifecycle. A surface being false is honest, not a bug — the LinuxPod backend
// is a prototype and these surfaces are added incrementally.
type Capabilities struct {
	// Logs is true when the backend creates CRI-format container log files and can
	// report their path through ContainerLogPath.
	Logs bool `json:"logs"`
	// Exec is true when the backend runs a synchronous command in a container
	// (ExecSync) — enough for kubelet exec liveness/readiness probes. It does not
	// imply interactive/streaming Exec, Attach, or PortForward.
	Exec bool `json:"exec"`
	// Stats is true when the backend reports per-container resource samples
	// (ContainerStats) for kubelet summaries.
	Stats bool `json:"stats"`
}

// PodSpec describes a LinuxPod sandbox VM to create.
type PodSpec struct {
	ID          string `json:"id"`
	Hostname    string `json:"hostname,omitempty"`
	CPUs        int    `json:"cpus,omitempty"`
	MemoryBytes int64  `json:"memoryBytes,omitempty"`
}

// PodStatus reports a Pod VM's observed state.
type PodStatus struct {
	ID    string        `json:"id"`
	Phase runtime.Phase `json:"phase"`
	// SandboxNamespace identifies the shared network namespace of the Pod VM. Every
	// container created in this pod reports the same value, which is how callers
	// prove app and sidecar share localhost.
	SandboxNamespace string `json:"sandboxNamespace"`
	// SandboxAddress is the Pod VM's host-reachable address — its host-only/vmnet
	// guest address — once the VM has acquired it (CRI-L3, #128). The CRI Pod
	// networking integration attaches the host pf/route path to this address (as the
	// binat target's source VMIP). It is "" while the address is not yet known (the
	// VM is still booting or acquiring DHCP), which the caller treats as a transient
	// "address not discovered yet" condition rather than a failure.
	SandboxAddress string `json:"sandboxAddress,omitempty"`
	Message        string `json:"message,omitempty"`
}

// RootfsRequest asks the backend to stage a prepared rootfs into a running pod.
type RootfsRequest struct {
	PodID         string `json:"podID"`
	ContainerName string `json:"containerName"`
	Image         string `json:"image"`
	// ExpectedIdentity is the rootfs identity the late process must report back to
	// pass StartContainer verification (CRI-R16). It is staged into the prepared
	// rootfs at this step and is the single source of truth for verification.
	ExpectedIdentity string `json:"expectedIdentity"`
}

// RootfsHandle is the result of PrepareContainerRootfs.
type RootfsHandle struct {
	// Token uniquely identifies the prepared rootfs; CreateContainer binds to it.
	Token string `json:"token"`
	// RootfsPath is the runtime-private staged rootfs path (diagnostic).
	RootfsPath string `json:"rootfsPath,omitempty"`
}

// CreateRequest late-binds a container onto a prepared rootfs.
type CreateRequest struct {
	PodID       string            `json:"podID"`
	Name        string            `json:"name"`
	RootfsToken string            `json:"rootfsToken"`
	Command     []string          `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	// LogPath is the kubelet-assigned CRI container log path. When set (and the
	// backend advertises Capabilities.Logs) the backend creates the file and writes
	// container lifecycle output to it in CRI log format. Empty means the container
	// has no kubelet log file, and ContainerLogPath then returns ErrInvalid.
	LogPath string `json:"logPath,omitempty"`
}

// StopRequest stops a container with a grace timeout.
type StopRequest struct {
	PodID          string `json:"podID"`
	ContainerID    string `json:"containerID"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

// Ref addresses one container within a pod.
type Ref struct {
	PodID       string `json:"podID"`
	ContainerID string `json:"containerID"`
}

// ContainerStatus reports a container's observed state plus the LinuxPod-specific
// evidence the issue requires: shared-namespace membership, late-binding, and
// rootfs identity verification.
type ContainerStatus struct {
	PodID    string        `json:"podID"`
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Phase    runtime.Phase `json:"phase"`
	ExitCode int           `json:"exitCode,omitempty"`
	Message  string        `json:"message,omitempty"`
	// SandboxNamespace is the Pod VM's shared namespace; equal across all containers
	// in the pod (shared-localhost evidence).
	SandboxNamespace string `json:"sandboxNamespace"`
	// CreatedAfterPodRunning is true when this container was created after the pod
	// already had a running container — the late-sidecar ordering proof.
	CreatedAfterPodRunning bool `json:"createdAfterPodRunning"`
	// LocalhostReachable is true once the container is running in the shared
	// namespace, modeling that it can reach other containers over 127.0.0.1.
	LocalhostReachable bool `json:"localhostReachable"`
	// ExpectedIdentity/ObservedIdentity/IdentityVerified report the CRI-R16 handoff
	// outcome: IdentityVerified is true only when ObservedIdentity exactly matched
	// ExpectedIdentity at start.
	ExpectedIdentity string `json:"expectedIdentity,omitempty"`
	ObservedIdentity string `json:"observedIdentity,omitempty"`
	IdentityVerified bool   `json:"identityVerified"`
}

// LogInfo locates a container's CRI-format log file.
type LogInfo struct {
	PodID       string `json:"podID"`
	ContainerID string `json:"containerID"`
	// Path is the absolute CRI log file path the kubelet can tail.
	Path string `json:"path"`
	// Format names the on-disk format; always "cri": one
	// "<rfc3339nano> <stdout|stderr> <P|F> <message>" line per entry.
	Format string `json:"format"`
}

// ExecRequest runs one command to completion in a running container.
type ExecRequest struct {
	PodID       string   `json:"podID"`
	ContainerID string   `json:"containerID"`
	Command     []string `json:"command"`
	// TimeoutSeconds bounds the exec; 0 means the backend default.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// ExecResult is the outcome of an ExecSync.
type ExecResult struct {
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	ExitCode int    `json:"exitCode"`
}

// ContainerStats is a point-in-time resource sample for one container, the
// minimum a kubelet summary needs.
type ContainerStats struct {
	PodID       string `json:"podID"`
	ContainerID string `json:"containerID"`
	// TimestampNanos is when the sample was taken (unix nanoseconds).
	TimestampNanos        int64  `json:"timestampNanos"`
	CPUUsageNanoCores     uint64 `json:"cpuUsageNanoCores"`
	MemoryWorkingSetBytes uint64 `json:"memoryWorkingSetBytes"`
	// Simulated is true when the numbers are modeled by a stub/fake rather than
	// measured from a real Pod VM, so the adapter never presents them as real
	// metrics (the #129 non-goal against claiming metrics parity). Real cgroup-backed
	// stats that set this false are tracked in #133.
	Simulated bool `json:"simulated"`
}

// CleanupReport summarizes a Cleanup so the caller can assert no leaks.
type CleanupReport struct {
	PodID             string `json:"podID"`
	RemovedContainers int    `json:"removedContainers"`
	RemovedRootfs     int    `json:"removedRootfs"`
	PodRemoved        bool   `json:"podRemoved"`
	// StaleState is true if the backend detected leftover container/rootfs/handoff
	// state it could not remove. A correct backend reports false.
	StaleState bool `json:"staleState"`
}

// ProtocolVersion is the wire-protocol version HelperClient and the helper agree
// on. Bump it on any breaking change to the request/response shapes. v2 added the
// kubelet surfaces (capabilities in Ping, ContainerLogPath/ExecSync/ContainerStats
// ops, CreateRequest.LogPath) for CRI-L4 (#129). v3 added the PodStatus op and
// PodStatus.SandboxAddress for Pod networking address discovery, CRI-L3 (#128).
const ProtocolVersion = 3
