package criserver

import (
	"errors"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

// container_handoff.go wires the experimental LinuxPod handoff preparation into
// CreateContainer (CRI-I3, #115). It is gated by Options.Handoff: when it is nil,
// s.handoff is nil and this code is inert, so the default apple/container path is
// unchanged.
//
// Preparation follows the CreateContainer ordering in CRI-R16
// (docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md): the runtime creates the
// per-container rootfs and handoff directories, prepares the in-rootfs mount
// destination, and injects the runtime-private handoff bind mount into the
// workload spec — all before the workload is created. This file does NOT start
// the container and makes no k3s/kubelet compatibility claim. It uses only the
// runtime-owned helpers (runtime.HandoffManager, PrepareHandoffMountpoint,
// InjectHandoffMount); all apple/container and vminitd assumptions stay inside
// the runtime package.

// handoffEnabled reports whether the experimental LinuxPod handoff path is wired.
func (s *Server) handoffEnabled() bool { return s.handoff != nil }

// prepareHandoff prepares the runtime-private rootfs/handoff subtree for a
// workload and injects the handoff bind mount into spec. On success it returns a
// cleanup func the caller must invoke if a later step (workload create or
// persist) fails, so a failed CreateContainer leaves no staged handoff state. On
// failure it cleans up anything it created and returns a CRI error: a mount
// collision maps to FailedPrecondition, a filesystem/preparation fault to
// Internal.
//
// It must be called only when handoffEnabled() is true.
func (s *Server) prepareHandoff(spec *types.ContainerSpec, workloadID string) (cleanup func(), err error) {
	layout, err := s.handoff.Create(workloadID)
	if err != nil {
		// An invalid workload ID is a precondition fault; any other create error is
		// a filesystem fault. Either way nothing partial is left: Create undoes a
		// subtree it created on failure.
		if errors.Is(err, runtime.ErrInvalidHandoffID) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"CreateContainer: prepare handoff: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "CreateContainer: prepare handoff: %v", err)
	}
	// From here on, undo the subtree if preparation or a later step fails.
	cleanup = func() {
		if cerr := s.handoff.Cleanup(workloadID); cerr != nil {
			klog.ErrorS(cerr, "CreateContainer: failed to clean up handoff after error",
				"workloadID", workloadID)
		}
	}
	if _, err := runtime.PrepareHandoffMountpoint(layout); err != nil {
		cleanup()
		return nil, status.Errorf(codes.Internal, "CreateContainer: prepare handoff mountpoint: %v", err)
	}
	// Stage the expected rootfs identity (CRI-I2-2 contract, #113) so StartContainer
	// can verify the late process reported it back (CRI-I3-2, #116). The expected
	// identity is the workload ID: it is deterministic and fully recoverable from
	// the container record, so no extra CRI state is persisted — a restarted adapter
	// re-derives both the identity and its path from WorkloadID.
	if err := runtime.StageIdentityFile(layout.RootfsDir, handoffExpectedIdentity(workloadID)); err != nil {
		cleanup()
		return nil, status.Errorf(codes.Internal, "CreateContainer: stage handoff identity: %v", err)
	}
	if err := runtime.InjectHandoffMount(spec, layout); err != nil {
		cleanup()
		if errors.Is(err, runtime.ErrHandoffMountConflict) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"CreateContainer: inject handoff mount: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "CreateContainer: inject handoff mount: %v", err)
	}
	return cleanup, nil
}

// cleanupHandoff removes a container's runtime-private handoff subtree during
// RemoveContainer. It is a no-op when the handoff path is disabled or the
// container never prepared one, and tolerates a missing subtree (the underlying
// Cleanup is idempotent), so it never blocks container removal.
func (s *Server) cleanupHandoff(c store.Container) {
	if !s.handoffEnabled() || !c.HandoffPrepared {
		return
	}
	if err := s.handoff.Cleanup(c.WorkloadID); err != nil {
		klog.ErrorS(err, "RemoveContainer: failed to clean up handoff",
			"containerID", c.ID, "workloadID", c.WorkloadID)
	}
}
