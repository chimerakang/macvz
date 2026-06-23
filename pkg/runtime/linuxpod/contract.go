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
	Message          string `json:"message,omitempty"`
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
// on. Bump it on any breaking change to the request/response shapes.
const ProtocolVersion = 1
