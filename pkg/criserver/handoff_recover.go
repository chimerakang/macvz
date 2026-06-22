package criserver

import (
	"context"

	"k8s.io/klog/v2"
)

// handoff_recover.go closes the restart side of the handoff lifecycle (CRI-I4-3,
// #120). The handoff subtree is runtime-private per-container state under the
// handoff root; its paths derive purely from the workload ID, so a restarted
// adapter recomputes the subtree for every container it still knows about
// (container_handoff.go) and never loses track of live state. What recovery must
// still do is reclaim subtrees that have no backing container record — orphans
// left by a crash between staging a subtree and persisting its record, or by an
// adapter that died mid-RemoveContainer after deleting the record but before
// (or during) cleanupHandoff. Such a subtree would otherwise leak runtime-private
// rootfs/handoff state across restarts indefinitely.
//
// The sweep is deliberately conservative: it removes a subtree only when no
// current container record claims its workload ID. A subtree belonging to any
// known container — Created, Running, or Exited, handoff-prepared or not — is
// always kept, because its record may still drive a later Start or carry exit
// evidence that StopContainer preserved until RemoveContainer. Identity evidence
// is never reread here: orphan-ness is decided from the record set alone, matching
// the rest of the lifecycle where identity is a start invariant, not an ongoing
// property (#110, #117).

// sweepOrphanHandoffs reclaims handoff subtrees with no backing container record.
// It returns the number cleaned and the number kept (claimed by a known
// container). It is a no-op when the handoff path is disabled. It is best-effort:
// a per-subtree cleanup failure is logged and counted as not-cleaned rather than
// failing recovery, so one wedged subtree cannot block adapter startup.
//
// Caller holds s.lifecycleMu (RecoverContainers does), so the record set it reads
// cannot race a concurrent create/remove.
func (s *Server) sweepOrphanHandoffs(ctx context.Context) (cleaned, kept int) {
	if !s.handoffEnabled() {
		return 0, 0
	}
	onDisk, err := s.handoff.ListContainerIDs()
	if err != nil {
		klog.ErrorS(err, "CRI restart recovery: failed to list handoff subtrees for orphan sweep")
		return 0, 0
	}
	if len(onDisk) == 0 {
		return 0, 0
	}

	// The set of workload IDs any current record still claims. A subtree whose
	// name is in this set is live state and must be left untouched.
	claimed := make(map[string]struct{})
	for _, c := range s.containers.List() {
		claimed[c.WorkloadID] = struct{}{}
	}

	for _, id := range onDisk {
		if _, ok := claimed[id]; ok {
			kept++
			continue
		}
		if err := s.handoff.Cleanup(id); err != nil {
			klog.ErrorS(err, "CRI restart recovery: failed to reclaim orphan handoff subtree", "workloadID", id)
			continue
		}
		cleaned++
		klog.InfoS("CRI restart recovery: reclaimed orphan handoff subtree", "workloadID", id)
	}
	if cleaned > 0 || kept > 0 {
		klog.InfoS("CRI restart recovery: swept handoff subtrees",
			"orphansReclaimed", cleaned, "claimedKept", kept)
	}
	return cleaned, kept
}
