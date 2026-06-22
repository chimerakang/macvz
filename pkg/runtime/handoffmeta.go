package runtime

// handoffmeta.go defines the runtime-private metadata for late-rootfs / handoff
// containers (CRI-I1-2, #110). It is the minimal state the runtime needs to
// recover or clean up a late-rootfs container after a restart, following the
// production handoff design accepted in CRI-R16 (docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md).
//
// Scope boundary: this file defines the metadata *shape* and the identity/cleanup
// state model. The reserved path layout and the create/cleanup lifecycle belong
// to the handoff path helper (CRI-I1-1, #109, handoff.go), and kubelet mount
// collision validation belongs to the mount guard (CRI-I1-3, #111). This file
// consumes HandoffLayout from #109 rather than re-deriving paths.
//
// This metadata is runtime-private. It is never exposed as a Kubernetes API
// surface or a kubelet-visible volume. The CRI container store (pkg/criserver/
// store) is intentionally left unchanged: handoff identity is a *start-time
// invariant* of the runtime, not user-facing CRI state, so it lives with the
// runtime driver, not the CRI record.

import (
	"path"
	"time"
)

// Reserved runtime-private path layout (CRI-R16). This is the single source of
// truth for the handoff namespace within the runtime package. The path lifecycle
// helper (handoff.go, #109) creates/cleans up directories under these paths, and
// the CRI mount guard (#111) rejects kubelet mounts that target them.
const (
	// HandoffRuntimeRoot is the top of MacVz's runtime-private namespace inside
	// the guest. Nothing kubelet-provided may target this subtree.
	HandoffRuntimeRoot = "/run/macvz"

	// HandoffContainersRoot is the per-container work root. Each container gets
	// HandoffContainersRoot/<containerID>/{rootfs,handoff}. It lives under /run so
	// it is tmpfs-backed and cleared on reboot.
	HandoffContainersRoot = HandoffRuntimeRoot + "/containers"

	// HandoffMountPoint is the in-container destination of the handoff bind mount.
	// The launched process writes evidence here; it is deliberately
	// runtime-private so it does not collide with normal image contents.
	HandoffMountPoint = HandoffRuntimeRoot + "/handoff"

	// IdentityFile is the minimum required evidence file the late process writes
	// into the handoff directory to report its rootfs identity to the runtime.
	IdentityFile = "identity"

	// rootfsSubdir and handoffSubdir are the per-container subdirectories for the
	// prepared rootfs and the runtime-owned evidence directory.
	rootfsSubdir  = "rootfs"
	handoffSubdir = "handoff"
)

// HandoffPaths returns the reserved rootfs and handoff guest paths for a
// container ID. It is a pure path join under HandoffContainersRoot and performs
// no sanitization or filesystem access: callers that touch the filesystem (the
// #109 lifecycle helper) must sanitize and reject path traversal in the ID
// first. It exists so callers that only need the layout — and the metadata in
// this file — agree with HandoffManager.Layout on one derivation.
func HandoffPaths(containerID string) (rootfs, handoff string) {
	base := path.Join(HandoffContainersRoot, containerID)
	return path.Join(base, rootfsSubdir), path.Join(base, handoffSubdir)
}

// IdentityStatus is the verification state of a late-rootfs container's identity
// evidence. Identity verification is a launch invariant: a container reaches
// Running only after the handoff evidence proves the expected rootfs was used.
type IdentityStatus string

const (
	// IdentityPending means evidence has not yet been read and verified. This is
	// the state of a freshly created container and the safe default a restarted
	// runtime assumes when no verification result was persisted.
	IdentityPending IdentityStatus = "Pending"
	// IdentityVerified means the observed identity exactly matched the expected
	// identity. Required before the container is reported Running.
	IdentityVerified IdentityStatus = "Verified"
	// IdentityMismatch means evidence was present but the observed identity did
	// not equal the expected identity. StartContainer must fail.
	IdentityMismatch IdentityStatus = "Mismatch"
	// IdentityMissing means the evidence file was absent or empty within the
	// start timeout. StartContainer must fail.
	IdentityMissing IdentityStatus = "Missing"
)

// CleanupState tracks the lifecycle of a container's runtime-private handoff
// resources (the prepared rootfs + handoff directory created by the #109
// helper). It is distinct from the container's process/CRI state: a Stopped
// container keeps its handoff files for status/debug until RemoveContainer, per
// CRI-R16.
type CleanupState string

const (
	// CleanupActive means the rootfs and handoff directories exist and back a
	// created or running container.
	CleanupActive CleanupState = "Active"
	// CleanupStopped means the process/VM is stopped but the handoff directory is
	// intentionally retained so ContainerStatus and debugging can still read it.
	CleanupStopped CleanupState = "Stopped"
	// CleanupRemoved means the per-container subtree has been removed by the #109
	// helper's Cleanup. Cleanup is idempotent, so this is also the correct end
	// state when the paths were already gone.
	CleanupRemoved CleanupState = "Removed"
)

// HandoffMeta is the minimal runtime-private metadata for one late-rootfs
// container. It records the prepared rootfs path, the handoff evidence path, the
// expected and observed rootfs identity, the verification timestamp/status, and
// the cleanup state.
//
// Restart / recovery contract (see also docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md):
//
//   - Reconstructable from the container ID alone (deterministic, never needs to
//     be persisted): ContainerID, RootfsPath, HandoffPath, IdentityFile. A
//     restarted runtime recomputes these from HandoffManager.Layout(containerID),
//     exactly as DeriveWorkloadID recomputes the workload name. NewHandoffMeta
//     takes the already-derived HandoffLayout so there is a single derivation.
//
//   - Recoverable from the container spec (not from the runtime alone):
//     ExpectedIdentity. It comes from the prepared rootfs the container was
//     created with, so the runtime re-derives it when it reloads that spec.
//
//   - Best-effort, may be lost on restart: ObservedIdentity, Status, VerifiedAt.
//     These capture the *result* of a past verification. They are not required to
//     be durable because identity is a start invariant, not an ongoing property:
//     a container that was already Verified and is still running does not need to
//     re-read evidence, and a restarted runtime that cannot confirm a prior
//     verification treats Status as IdentityPending (via EffectiveStatus) rather
//     than trusting an unrecoverable result. Whether these are persisted at all
//     is a driver decision left to the CRI wiring task; this type only defines
//     the shape and the zero value.
//
// The JSON tags exist so a driver that chooses to persist this record (e.g.
// alongside its own runtime-local state) gets a stable on-disk shape. The CRI
// container store is deliberately not changed to carry these fields.
type HandoffMeta struct {
	// ContainerID is the sanitized runtime container/workload ID. Reconstructable.
	ContainerID string `json:"containerID"`
	// RootfsPath is the host-visible guest path of the prepared rootfs
	// (HandoffLayout.RootfsDir). Reconstructable.
	RootfsPath string `json:"rootfsPath"`
	// HandoffPath is the host-visible source of the handoff bind mount
	// (HandoffLayout.HandoffDir); the container writes evidence into it.
	// Reconstructable.
	HandoffPath string `json:"handoffPath"`
	// IdentityFile is the host path of the minimum required evidence file
	// (HandoffLayout.IdentityFile) the runtime reads back to verify identity.
	// Reconstructable.
	IdentityFile string `json:"identityFile"`
	// ExpectedIdentity is the rootfs identity the launched process must report.
	// Recoverable from the container's prepared rootfs spec.
	ExpectedIdentity string `json:"expectedIdentity"`
	// ObservedIdentity is the identity the process actually reported via the
	// handoff evidence file. Best-effort; empty until verification runs.
	ObservedIdentity string `json:"observedIdentity,omitempty"`
	// Status is the identity verification result. Best-effort; the zero value is
	// the empty string, which EffectiveStatus maps to IdentityPending.
	Status IdentityStatus `json:"status,omitempty"`
	// VerifiedAt is when verification last ran. Best-effort; zero until then.
	VerifiedAt time.Time `json:"verifiedAt,omitempty"`
	// Cleanup is the handoff resource lifecycle state. The zero value is the
	// empty string, which EffectiveCleanup maps to CleanupActive for a live
	// container.
	Cleanup CleanupState `json:"cleanup,omitempty"`
}

// NewHandoffMeta builds the metadata for a container from its (already
// sanitized) ID, the derived HandoffLayout (from HandoffManager.Layout/Create),
// and the expected rootfs identity. Status is left IdentityPending and Cleanup
// CleanupActive, reflecting a freshly created container whose evidence has not
// been read.
func NewHandoffMeta(containerID string, layout HandoffLayout, expectedIdentity string) HandoffMeta {
	return HandoffMeta{
		ContainerID:      containerID,
		RootfsPath:       layout.RootfsDir,
		HandoffPath:      layout.HandoffDir,
		IdentityFile:     layout.IdentityFile,
		ExpectedIdentity: expectedIdentity,
		Status:           IdentityPending,
		Cleanup:          CleanupActive,
	}
}

// Verify records the identity the process reported through the handoff evidence
// file and resolves the verification Status, stamping VerifiedAt with now. It
// uses exact equality (not substring matching, per CRI-R16): an empty observed
// value is IdentityMissing, an unequal value is IdentityMismatch, and an exact
// match is IdentityVerified. It returns the resolved status.
func (m *HandoffMeta) Verify(observed string, now time.Time) IdentityStatus {
	m.ObservedIdentity = observed
	m.VerifiedAt = now
	switch {
	case observed == "":
		m.Status = IdentityMissing
	case observed == m.ExpectedIdentity:
		m.Status = IdentityVerified
	default:
		m.Status = IdentityMismatch
	}
	return m.Status
}

// Verified reports whether identity verification has succeeded. A container must
// not be reported Running unless this is true.
func (m HandoffMeta) Verified() bool {
	return m.Status == IdentityVerified
}

// EffectiveStatus returns Status, mapping the zero value to IdentityPending so a
// restarted runtime that recovered metadata without a persisted result treats it
// as not-yet-verified rather than trusting an unknown state.
func (m HandoffMeta) EffectiveStatus() IdentityStatus {
	if m.Status == "" {
		return IdentityPending
	}
	return m.Status
}

// EffectiveCleanup returns Cleanup, mapping the zero value to CleanupActive so a
// recovered live container is treated as having present handoff resources.
func (m HandoffMeta) EffectiveCleanup() CleanupState {
	if m.Cleanup == "" {
		return CleanupActive
	}
	return m.Cleanup
}
