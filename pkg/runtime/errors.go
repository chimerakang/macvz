package runtime

import (
	"context"
	"errors"
	"fmt"
)

// Sentinel errors returned by Runtime implementations. Callers should compare
// with errors.Is so drivers can wrap them with contextual detail.
var (
	// ErrNotReady means the host runtime (e.g. the apple/container service) is
	// not reachable or not healthy. Distinct from a per-workload failure.
	ErrNotReady = errors.New("runtime: not ready")

	// ErrNotFound means the referenced workload does not exist.
	ErrNotFound = errors.New("runtime: workload not found")

	// ErrAlreadyExists means a workload with the requested ID already exists.
	ErrAlreadyExists = errors.New("runtime: workload already exists")

	// ErrNotRunning means the workload exists but is not running, so the
	// requested operation (e.g. Exec) cannot proceed.
	ErrNotRunning = errors.New("runtime: workload not running")

	// ErrIncompatibleArch means the image has no variant for the host CPU
	// architecture (arm64 on Apple Silicon). amd64 emulation is deferred to P4.
	ErrIncompatibleArch = errors.New("runtime: image architecture incompatible")
)

// ExitError reports that a command run via Exec completed but exited with a
// non-zero status. It is distinct from a failure to start the command (which
// returns ErrNotFound/ErrNotRunning); here the command ran and chose its code.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("runtime: command exited with code %d", e.Code)
}

// Pinger is an optional capability for a Runtime that can report whether the
// underlying host runtime is reachable and healthy. The provider uses it on
// startup to surface readiness before accepting workloads.
type Pinger interface {
	// Ready returns nil when the host runtime is reachable and healthy,
	// otherwise an error wrapping ErrNotReady.
	Ready(ctx context.Context) error
}
