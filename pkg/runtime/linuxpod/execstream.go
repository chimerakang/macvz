package linuxpod

import (
	"context"
	"fmt"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// execstream.go adds the interactive/streaming exec negotiation surface (CRI-L4
// follow-up #132): the streaming counterpart to ExecSync for `kubectl exec -it`.
//
// Like the Attach/PortForward interactive surfaces (#131), ExecStream is a
// capability-gated *negotiation* surface: it validates the target and reports the
// session the backend would open (which streams attach, whether a TTY was
// granted), with a simulated backend setting Simulated=true. The actual
// bidirectional byte plumbing into the Pod VM has nothing to attach to in a
// stub/fake and is the documented non-goal here. Keeping the negotiation shape
// identical to Attach/PortForward lets a future real streaming transport be
// layered uniformly across all three interactive surfaces.

// ExecStreamRequest negotiates an interactive exec session in a running container
// (#132). The flags mirror the CRI ExecRequest; the backend reports what it can
// honor in ExecStreamResponse.
type ExecStreamRequest struct {
	PodID       string   `json:"podID"`
	ContainerID string   `json:"containerID"`
	Command     []string `json:"command"`
	Stdin       bool     `json:"stdin,omitempty"`
	Stdout      bool     `json:"stdout,omitempty"`
	Stderr      bool     `json:"stderr,omitempty"`
	TTY         bool     `json:"tty,omitempty"`
}

// ExecStreamResponse reports the negotiated interactive exec session. Simulated
// marks a stub/fake that modeled the negotiation without wiring real VM-internal
// streams (#132 non-goal).
type ExecStreamResponse struct {
	// Stdin/Stdout/Stderr/TTY echo the streams the backend would attach, which may
	// be a subset of those requested. Per CRI/TTY convention a TTY session merges
	// stderr into stdout, so Stderr is false whenever TTY is true.
	Stdin     bool   `json:"stdin"`
	Stdout    bool   `json:"stdout"`
	Stderr    bool   `json:"stderr"`
	TTY       bool   `json:"tty"`
	Simulated bool   `json:"simulated"`
	Message   string `json:"message,omitempty"`
}

// ExecStream negotiates an interactive exec session for the HelperClient by
// forwarding the request to the helper over the NDJSON protocol.
func (c *HelperClient) ExecStream(ctx context.Context, req ExecStreamRequest) (ExecStreamResponse, error) {
	var res ExecStreamResponse
	err := c.call(ctx, opExecStream, req, &res)
	return res, err
}

// ExecStream models the interactive exec negotiation for the FakeBackend.
func (f *FakeBackend) ExecStream(_ context.Context, req ExecStreamRequest) (ExecStreamResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Capabilities.ExecStream {
		return ExecStreamResponse{}, fmt.Errorf("%w: execstream", ErrUnsupported)
	}
	_, c, err := f.lookupLocked(Ref{PodID: req.PodID, ContainerID: req.ContainerID})
	if err != nil {
		return ExecStreamResponse{}, err
	}
	if len(req.Command) == 0 {
		return ExecStreamResponse{}, fmt.Errorf("%w: exec command is required", ErrInvalid)
	}
	if c.phase != runtime.PhaseRunning {
		return ExecStreamResponse{}, fmt.Errorf("%w: container %q is %s, exec requires Running", ErrInvalid, req.ContainerID, c.phase)
	}
	// Simulated negotiation: report the requested streams as attachable. A TTY
	// session merges stderr into stdout (CRI convention), so stderr is not attached
	// separately. A real backend wires bidirectional vminitd streams here; that
	// plumbing is the #132 non-goal.
	return ExecStreamResponse{
		Stdin:     req.Stdin,
		Stdout:    req.Stdout,
		Stderr:    req.Stderr && !req.TTY,
		TTY:       req.TTY,
		Simulated: true,
		Message:   "simulated interactive exec negotiation (no real VM-internal streams)",
	}, nil
}
