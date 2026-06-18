// Package runtime defines the contract between the Virtual Kubelet provider and
// the host container runtime (apple/container), which runs each workload as an
// isolated Linux micro-VM via Apple's Virtualization.framework.
//
// The interface is deliberately concrete-driver-agnostic so the provider can be
// developed and tested against a fake. The apple/container driver lands in P1.
package runtime

import (
	"context"
	"io"
	"time"

	"github.com/chimerakang/macvz/internal/types"
)

// Phase is the lifecycle state of a workload's micro-VM.
type Phase string

const (
	PhaseUnknown Phase = "Unknown"
	PhaseCreated Phase = "Created"
	PhaseRunning Phase = "Running"
	PhaseStopped Phase = "Stopped"
	PhaseFailed  Phase = "Failed"
)

// Status reports the observed state of a single workload.
type Status struct {
	ID       string
	Phase    Phase
	ExitCode int
	// Message carries human-readable detail for failures.
	Message   string
	StartedAt time.Time
	// IP is the workload's address once networking is wired up (P3).
	IP string
}

// LogOptions controls log retrieval.
type LogOptions struct {
	// Follow streams new output until the context is cancelled.
	Follow bool
	// Tail limits output to the last N lines when > 0.
	Tail int
}

// ExecIO wires standard streams for an Exec session.
type ExecIO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	TTY    bool
}

// Runtime drives the full lifecycle of a workload micro-VM on a single host.
//
// Implementations must be safe for concurrent use; the provider may operate on
// multiple workloads at once.
type Runtime interface {
	// Pull fetches an OCI image into the local store.
	Pull(ctx context.Context, image string) error
	// Create provisions (but does not start) a workload, returning its ID.
	Create(ctx context.Context, spec types.ContainerSpec) (id string, err error)
	// Start boots the workload's micro-VM.
	Start(ctx context.Context, id string) error
	// Stop requests graceful shutdown, forcing after timeout.
	Stop(ctx context.Context, id string, timeout time.Duration) error
	// Destroy removes the workload and reclaims its resources.
	Destroy(ctx context.Context, id string) error
	// Status returns the current observed state of the workload.
	Status(ctx context.Context, id string) (Status, error)
	// Logs returns a reader over the workload's output. Caller closes it.
	Logs(ctx context.Context, id string, opts LogOptions) (io.ReadCloser, error)
	// Exec runs a command inside the workload, wiring the given streams.
	Exec(ctx context.Context, id string, cmd []string, sio ExecIO) error
}
