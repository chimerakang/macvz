package criserver

import (
	"errors"
	"os"
	"strconv"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
)

// handoffWritePolicy describes a handoff directory's permission posture in terms
// of the CRI-I5-1 (#121) policy, derived from its mode bits: owner-only and
// owner+group are the hardened forms applied after a successful chown to the
// container's runAsUser/runAsGroup; world-writable is the safe fallback used when
// the owner is unknown or the adapter cannot chown to it. Any other mode is
// reported verbatim so an unexpected posture is visible rather than mislabeled.
func handoffWritePolicy(perm os.FileMode) string {
	switch perm & 0o777 {
	case 0o700:
		return "owner-only"
	case 0o770:
		return "owner-group"
	case 0o777:
		return "world-writable-fallback"
	default:
		return "0" + strconv.FormatUint(uint64(perm&0o777), 8)
	}
}

// handoff_lifecycle.go completes the CRI lifecycle mapping for handoff-aware
// containers (CRI-I3-3, #117), building on the create-time preparation in
// container_handoff.go (#115). It covers the back half of the R16 lifecycle
// (docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md):
//
//   - StopContainer records exit state but MUST NOT delete the handoff subtree:
//     identity evidence and stderr stay available for post-mortem debugging until
//     RemoveContainer. Stop achieves this simply by never calling cleanupHandoff
//     (only removeContainerRecord does), an invariant the lifecycle tests assert.
//   - RemoveContainer deletes the runtime-private rootfs/handoff subtree
//     idempotently. removeContainerRecord calls s.cleanupHandoff (defined in
//     container_handoff.go), whose underlying runtime.HandoffManager.Cleanup is a
//     no-op on a missing subtree, so repeated removal never errors.
//   - ContainerStatus verbose info MAY surface runtime-private identity
//     verification diagnostics via handoffStatusInfo, read best-effort from the
//     on-disk evidence channel. This is a debug-only surface; it is never part of
//     the stable CRI status and never exposed as a Kubernetes volume.
//
// Reconciliation (container.go reconcile) intentionally does not appear here: it
// advances a workload to Exited from the runtime *phase* alone and never rereads
// identity evidence, so a container that verified identity at start stays
// verified without re-verification after a runtime-not-found or restart. Identity
// is a start invariant, not an ongoing property (#110).

// handoffStatusInfo returns runtime-private identity-verification diagnostics for
// a verbose ContainerStatus, or nil when the handoff path is disabled or the
// container prepared no handoff. The values are read best-effort from the on-disk
// evidence channel the runtime helpers own (#109/#113): the expected identity
// staged into the prepared rootfs and the observed identity the launched process
// wrote into the handoff directory. Read failures are reported as diagnostics
// rather than errors so verbose status never fails on a half-prepared container.
//
// This is the only place identity evidence is read on the status path, and only
// under Verbose. The non-verbose status and the reconcile path never read it.
func (s *Server) handoffStatusInfo(c store.Container) map[string]string {
	if !s.handoffEnabled() || !c.HandoffPrepared {
		return nil
	}
	// Paths derive purely from the workload ID; no filesystem access yet.
	layout, err := s.handoff.Layout(c.WorkloadID)
	if err != nil {
		return map[string]string{"handoffError": err.Error()}
	}
	info := map[string]string{
		"handoffPrepared":   "true",
		"handoffPath":       layout.HandoffDir,
		"handoffMountPoint": layout.MountPoint,
		"identitySource":    "handoff",
	}

	// Surface the runtime-private handoff directory's permission posture
	// (CRI-I5-1, #121) so an operator can confirm whether it was narrowed to the
	// container's runAsUser/runAsGroup or fell back to world-writable. Best-effort:
	// a stat failure is reported as a diagnostic, never an error.
	if fi, statErr := os.Stat(layout.HandoffDir); statErr == nil {
		perm := fi.Mode().Perm()
		info["handoffDirMode"] = "0" + strconv.FormatUint(uint64(perm), 8)
		info["handoffWritePolicy"] = handoffWritePolicy(perm)
	} else {
		info["handoffDirMode"] = "stat-error: " + statErr.Error()
	}

	expected, expErr := runtime.ReadStagedIdentity(layout.RootfsDir)
	if expErr == nil && expected != "" {
		info["expectedIdentity"] = expected
	}

	ev, evErr := runtime.ReadHandoffEvidence(layout.IdentityFile)
	switch {
	case evErr == nil:
		info["observedIdentity"] = ev.Identity
		// identityVerified reflects the start invariant: the process reported the
		// rootfs identity the runtime staged. With no expected identity staged yet,
		// it cannot be verified, so report false rather than a misleading match.
		info["identityVerified"] = strconv.FormatBool(expected != "" && ev.Identity == expected)
		if pr, ok := ev.Get("proc_root"); ok {
			info["procRoot"] = pr
		}
	case errors.Is(evErr, runtime.ErrEvidenceMissing):
		// No evidence yet: the container has not run, or did not write it. Not an
		// error for a debug surface — say so plainly.
		info["identityVerified"] = "false"
		info["identityEvidence"] = "missing"
	case errors.Is(evErr, runtime.ErrEvidenceMalformed):
		info["identityVerified"] = "false"
		info["identityEvidence"] = "malformed"
	default:
		info["identityVerified"] = "false"
		info["identityEvidence"] = evErr.Error()
	}
	return info
}
