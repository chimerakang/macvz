package criserver

import (
	"context"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

// container_handoff_start.go gates a late-rootfs container's transition to
// Running on handoff identity verification (CRI-I3-2, #116), following CRI-R16:
// StartContainer marks the container Running only after the launched process has
// reported the expected rootfs identity back through the runtime-private handoff
// evidence file. It is inert unless the experimental LinuxPod handoff path is
// enabled (s.handoff != nil) and the container actually prepared a handoff.

// Handoff identity verification timing. The wait is bounded so a process that
// never writes evidence fails the start with a precise diagnostic instead of
// hanging. The fields are overridable so tests stay fast.
const (
	defaultHandoffVerifyTimeout  = 30 * time.Second
	defaultHandoffVerifyInterval = 250 * time.Millisecond
)

// handoffExpectedIdentity returns the rootfs identity a late-rootfs workload must
// report. It is the workload ID: deterministic, unique per container, and fully
// recoverable from the persisted record, so verification needs no extra CRI
// state. Staged into the rootfs at create time and echoed back by the process.
func handoffExpectedIdentity(workloadID string) string {
	return "macvz-rootfs-id=" + workloadID
}

// verifyHandoffIdentity blocks until the late-rootfs container reports its
// expected identity through the handoff evidence file, or the bounded timeout
// expires. It returns nil (start may proceed to Running) when verification is not
// applicable — the handoff path is off, or this container prepared no handoff —
// or when the observed identity matches. On a missing/mismatched/late identity it
// unwinds the just-started workload (stop + mark Exited) and returns a
// FailedPrecondition error, so the container is never left Running after a failed
// verification.
//
// proc_root and mount diagnostics in the evidence never affect the decision
// (CRI-R16); app readiness is out of scope (Kubernetes probes stay separate).
func (s *Server) verifyHandoffIdentity(ctx context.Context, c *store.Container) error {
	if !s.handoffEnabled() || !c.HandoffPrepared {
		return nil
	}
	layout, err := s.handoff.Layout(c.WorkloadID)
	if err != nil {
		s.unwindContainerStartReason(context.WithoutCancel(ctx), c,
			"IdentityVerificationFailed",
			"handoff layout could not be derived for identity verification")
		return status.Errorf(codes.Internal,
			"StartContainer: derive handoff layout for %q: %v", c.ID, err)
	}
	// The expected identity is whatever was staged into the prepared rootfs at
	// create time (CRI-I2-2, #113), the same source the verbose status path reads
	// (#117). Its absence means the container was never properly prepared.
	expected, err := runtime.ReadStagedIdentity(layout.RootfsDir)
	if err != nil {
		s.unwindContainerStartReason(context.WithoutCancel(ctx), c,
			"IdentityVerificationFailed",
			"staged rootfs identity could not be read for verification")
		return status.Errorf(codes.Internal,
			"StartContainer: read staged identity for %q: %v", c.ID, err)
	}
	meta := runtime.NewHandoffMeta(c.WorkloadID, layout, expected)

	vctx, cancel := context.WithTimeout(ctx, s.handoffVerifyTimeout)
	_, verr := runtime.WaitForHandoffIdentity(vctx, &meta, s.now, s.handoffVerifyInterval)
	cancel()
	if verr != nil {
		// Attach any stderr the failed late process left behind, best-effort.
		msg := "handoff identity verification failed: " + verr.Error()
		if diag := runtime.ReadStderrDiagnostics(layout.HandoffDir); diag != "" {
			msg += " (stderr: " + diag + ")"
		}
		s.unwindContainerStartReason(context.WithoutCancel(ctx), c, "IdentityVerificationFailed", msg)
		return status.Errorf(codes.FailedPrecondition,
			"StartContainer: %s", msg)
	}
	klog.V(4).InfoS("CRI StartContainer: handoff identity verified",
		"containerID", c.ID, "workloadID", c.WorkloadID, "identity", meta.ObservedIdentity)
	return nil
}
