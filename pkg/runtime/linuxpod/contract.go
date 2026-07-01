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
	// ErrInvalid means the request was malformed (missing required field, or a
	// state-machine violation such as creating a container before its Pod).
	ErrInvalid = errors.New("linuxpod: invalid request")
	// ErrAlreadyExists means the pod or container the request would create is
	// already live in the backend. The adapter branches on it to reconcile CRI
	// records against surviving backend state (v8; previously carried as
	// ErrInvalid with "already exists" message text, which the adapter had to
	// string-match).
	ErrAlreadyExists = errors.New("linuxpod: already exists")
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

	// Adopt attempts to recover a Pod VM and its containers after the helper's own
	// process restart (#138), without forcing the adapter to recreate the Pod when
	// recovery is possible. It is the live-VM counterpart to the fail-fast PodStatus
	// probe: where PodStatus answers ErrPodNotFound for a pod the restarted helper
	// lost, Adopt asks the helper to resolve it from its durable journal.
	//
	// On success it returns AdoptionResult{Adopted:true} with the containers' current
	// live status, and subsequent PodStatus/Status calls observe the reattached VM.
	// If the Pod VM did not survive the restart (or adoption is otherwise incomplete)
	// it returns AdoptionResult{Adopted:false} with a Reason and NO error, so the
	// adapter falls back to BackendLost/recreate — the supported behavior remains
	// intact. It returns ErrUnsupported when the helper does not implement a durable
	// adoption protocol (Capabilities.Adopt false), and ErrPodNotFound when the pod
	// id is unknown to the helper's journal entirely.
	Adopt(ctx context.Context, podID string) (AdoptionResult, error)

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

	// ExecStream negotiates an interactive/streaming exec session in a running
	// container (`kubectl exec -it`) — the streaming counterpart to ExecSync that
	// #129 scoped out (CRI-L4 follow-up #132). It is capability-gated: ErrUnsupported
	// when Capabilities.ExecStream is false. When supported it validates the target
	// (ErrContainerNotFound for an unknown ref, ErrInvalid when not Running or the
	// command is empty) and returns an ExecStreamResponse describing the negotiated
	// session (which streams attach, whether a TTY was granted). A simulated backend
	// sets ExecStreamResponse.Simulated true: the feasibility/negotiation is modeled,
	// the actual bidirectional VM-internal stream plumbing is the documented non-goal.
	ExecStream(ctx context.Context, req ExecStreamRequest) (ExecStreamResponse, error)

	// ContainerStats returns a point-in-time resource sample for one container for
	// kubelet summary stats. Returns ErrUnsupported when Capabilities.Stats is false.
	// A simulated backend marks the sample Simulated so the adapter never reports
	// modeled numbers as measured.
	ContainerStats(ctx context.Context, ref Ref) (ContainerStats, error)

	// Attach negotiates attaching to a running container's stdio streams
	// (`kubectl attach`), the interactive surface #129 deliberately scoped out
	// (CRI-L4 follow-up #131). It is capability-gated: ErrUnsupported when
	// Capabilities.Attach is false. When supported it validates the target
	// (ErrContainerNotFound for an unknown ref, ErrInvalid when not Running) and
	// returns an AttachResponse describing the negotiated streams. A simulated
	// backend sets AttachResponse.Simulated true: the feasibility/negotiation is
	// modeled, the actual VM-internal stream plumbing is the documented non-goal.
	Attach(ctx context.Context, req AttachRequest) (AttachResponse, error)

	// PortForward negotiates forwarding host ports to a Pod VM port
	// (`kubectl port-forward`), the other interactive surface #129 scoped out
	// (#131). It is capability-gated: ErrUnsupported when Capabilities.PortForward
	// is false. When supported it validates the pod (ErrPodNotFound for an unknown
	// pod) and the ports (ErrInvalid for an out-of-range port) and returns a
	// PortForwardResponse listing the forwardable ports. A simulated backend sets
	// PortForwardResponse.Simulated true; the actual byte-stream forwarding is the
	// documented non-goal.
	PortForward(ctx context.Context, req PortForwardRequest) (PortForwardResponse, error)
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
	// Adoption reports the helper's most recent startup adoption pass (#138): whether
	// the helper supports the durable adoption protocol for journaled Pod VMs after
	// its own restart, and how many it reacquired vs. could not. The adapter and
	// diagnostics read it to learn whether a restart preserved workloads or fell back
	// to recreate. Its zero value (Supported false) is honest for a helper that
	// predates the feature or carries no durable journal.
	Adoption AdoptionStatus `json:"adoption"`
}

// AdoptionStatus summarizes a helper's startup adoption pass (#138). It is surfaced
// through Ping so an operator or the adapter can see, at the handshake, whether a
// helper restart reattached to live Pod VMs or fell back to recreate.
type AdoptionStatus struct {
	// Supported is true when the helper persists a durable journal and implements
	// Adopt for per-pod recovery outcomes after its own restart. False means every
	// restart falls back to BackendLost/recreate (the pre-#138 behavior).
	Supported bool `json:"supported"`
	// AdoptedPods is how many journaled Pod VMs the helper reacquired at startup.
	AdoptedPods int `json:"adoptedPods"`
	// LostPods is how many journaled Pod VMs the helper could not reacquire (their
	// micro-VM did not survive the restart); those fall back to recreate.
	LostPods int `json:"lostPods"`
}

// AdoptionResult reports whether a restarted helper reattached to a Pod VM (#138).
type AdoptionResult struct {
	PodID string `json:"podID"`
	// Adopted is true when the helper reacquired the live Pod VM and its containers,
	// so the adapter can keep the sandbox Ready without a kubelet recreate. False
	// means adoption was impossible or incomplete and the adapter must fall back to
	// BackendLost/recreate; Reason explains why.
	Adopted bool `json:"adopted"`
	// Containers is the adopted containers' current live status, letting the adapter
	// reconcile its records against the live VM in one round-trip. Empty when Adopted
	// is false.
	Containers []ContainerStatus `json:"containers,omitempty"`
	// Reason explains a false Adopted (e.g. the micro-VM did not survive the restart),
	// for diagnostics and the adapter's fallback log.
	Reason string `json:"reason,omitempty"`
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
	// ExecStream is true when the backend negotiates an interactive/streaming exec
	// session (ExecStream): stdin plus an optional TTY and terminal resize, for
	// `kubectl exec -it`. Separate from Exec on purpose — a backend may back the
	// synchronous ExecSync but not interactive streaming. The contract surface
	// negotiates feasibility, TTY, and stdin; the actual bidirectional byte plumbing
	// into the Pod VM is the documented non-goal (CRI-L4 follow-up #132). False →
	// ExecStream returns ErrUnsupported.
	ExecStream bool `json:"execStream"`
	// Stats is true when the backend reports per-container resource samples
	// (ContainerStats) for kubelet summaries.
	Stats bool `json:"stats"`
	// Attach is true when the backend can attach to a running container's stdio
	// streams (`kubectl attach`). The contract surface (Attach) negotiates
	// feasibility and TTY; the actual bidirectional stream plumbing into the Pod VM
	// is a separate concern (CRI-L4 follow-up #131 non-goal). False → Attach returns
	// ErrUnsupported.
	Attach bool `json:"attach"`
	// PortForward is true when the backend can forward host ports to a Pod VM port
	// (`kubectl port-forward`). The contract surface (PortForward) negotiates which
	// ports are forwardable; the actual byte-stream forwarding is out of scope (#131
	// non-goal). False → PortForward returns ErrUnsupported.
	PortForward bool `json:"portForward"`
	// Adopt is true when the helper persists a durable journal and implements per-pod
	// recovery outcomes through Adopt after its own process restart (#138). A helper
	// may still return Adopted=false for a journaled pod it cannot reacquire, letting
	// the adapter fall back immediately. False means a helper restart always loses
	// Pod VM state and the adapter uses BackendLost/recreate; Adopt then returns
	// ErrUnsupported.
	Adopt bool `json:"adopt"`
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
	// DNS is the kubelet-provided per-sandbox DNS configuration to materialize in
	// the prepared rootfs (typically /etc/resolv.conf) before the late container is
	// created. Empty means leave the image/default resolver unchanged.
	DNS DNSConfig `json:"dns,omitempty"`
	// ExpectedIdentity is the rootfs identity the late process must report back to
	// pass StartContainer verification (CRI-R16). It is staged into the prepared
	// rootfs at this step and is the single source of truth for verification.
	ExpectedIdentity string `json:"expectedIdentity"`
}

// DNSConfig is the subset of CRI DNSConfig the helper needs to render a
// resolv.conf inside the prepared LinuxPod rootfs (CRI-L8-2, #142).
type DNSConfig struct {
	Servers  []string `json:"servers,omitempty"`
	Searches []string `json:"searches,omitempty"`
	Options  []string `json:"options,omitempty"`
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
	Mounts      []Mount           `json:"mounts,omitempty"`
	// LogPath is the kubelet-assigned CRI container log path. When set (and the
	// backend advertises Capabilities.Logs) the backend creates the file and writes
	// container lifecycle output to it in CRI log format. Empty means the container
	// has no kubelet log file, and ContainerLogPath then returns ErrInvalid.
	LogPath string `json:"logPath,omitempty"`
}

// Mount is a kubelet-provided mount the backend should realize inside the late
// container's rootfs namespace.
type Mount struct {
	Source   string `json:"source,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	Tmpfs    bool   `json:"tmpfs,omitempty"`
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

// AttachRequest negotiates an attach to a running container's stdio (#131). The
// stream-direction flags mirror the CRI AttachRequest; the backend reports which
// it can honor in AttachResponse.
type AttachRequest struct {
	PodID       string `json:"podID"`
	ContainerID string `json:"containerID"`
	Stdin       bool   `json:"stdin,omitempty"`
	Stdout      bool   `json:"stdout,omitempty"`
	Stderr      bool   `json:"stderr,omitempty"`
	TTY         bool   `json:"tty,omitempty"`
}

// AttachResponse reports the negotiated attach. Simulated marks a stub/fake that
// modeled the negotiation without wiring real VM-internal streams (#131 non-goal).
type AttachResponse struct {
	// Stdin/Stdout/Stderr/TTY echo the streams the backend would actually attach,
	// which may be a subset of those requested.
	Stdin     bool   `json:"stdin"`
	Stdout    bool   `json:"stdout"`
	Stderr    bool   `json:"stderr"`
	TTY       bool   `json:"tty"`
	Simulated bool   `json:"simulated"`
	Message   string `json:"message,omitempty"`
}

// PortForwardRequest negotiates forwarding the given Pod VM ports (#131).
type PortForwardRequest struct {
	PodID string  `json:"podID"`
	Ports []int32 `json:"ports,omitempty"`
}

// PortForwardResponse lists the ports the backend can forward. Simulated marks a
// stub/fake that modeled the negotiation without wiring real byte streams (#131
// non-goal).
type PortForwardResponse struct {
	Ports     []int32 `json:"ports,omitempty"`
	Simulated bool    `json:"simulated"`
	Message   string  `json:"message,omitempty"`
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
// v4 added CreateRequest.Mounts so kubelet-managed ConfigMap/Secret/emptyDir
// volumes reach the LinuxPod helper in the #130 in-loop path. v5 added the
// interactive surface ops + capabilities: Attach and PortForward (CRI-L4
// follow-up #131) and ExecStream (CRI-L4 follow-up #132). v6 added the adoption op
// + capability (Adopt) and HelperInfo.Adoption so a restarted helper can either
// reattach to existing Pod VM state or report an immediate recreate fallback (#138).
// v7 added RootfsRequest.DNS so the helper can materialize kubelet's sandbox DNS
// config into LinuxPod prepared rootfs files (#142). v8 added the AlreadyExists
// error code so the adapter reconciles duplicate-create conflicts against
// surviving backend state without matching error message text (#159).
const ProtocolVersion = 8
