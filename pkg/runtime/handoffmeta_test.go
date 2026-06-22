package runtime

import (
	"encoding/json"
	"testing"
	"time"
)

// layoutFor derives a HandoffLayout the same way the runtime would, using the
// #109 helper. Tests stay hermetic: Layout touches no filesystem.
func layoutFor(t *testing.T, id string) HandoffLayout {
	t.Helper()
	l, err := NewHandoffManager("").Layout(id)
	if err != nil {
		t.Fatalf("Layout(%q): %v", id, err)
	}
	return l
}

func TestHandoffPathsAgreeWithLayout(t *testing.T) {
	const id = "macvz-cri-abcdef0123456789abcdef01"
	rootfs, handoff := HandoffPaths(id)
	if want := HandoffContainersRoot + "/" + id + "/rootfs"; rootfs != want {
		t.Errorf("rootfs = %q, want %q", rootfs, want)
	}
	if want := HandoffContainersRoot + "/" + id + "/handoff"; handoff != want {
		t.Errorf("handoff = %q, want %q", handoff, want)
	}
	// The pure helper and the filesystem helper must derive identical paths so a
	// caller that only needs the layout never drifts from the one that creates it.
	l := layoutFor(t, id)
	if l.RootfsDir != rootfs || l.HandoffDir != handoff {
		t.Errorf("HandoffPaths disagrees with Layout:\n paths=(%q,%q)\n layout=%+v", rootfs, handoff, l)
	}
}

func TestNewHandoffMetaFromLayout(t *testing.T) {
	id := "macvz-cri-abc123"
	layout := layoutFor(t, id)
	m := NewHandoffMeta(id, layout, "macvz-r9-id=late-alpha")

	if m.ContainerID != id {
		t.Errorf("ContainerID = %q, want %q", m.ContainerID, id)
	}
	if m.ExpectedIdentity != "macvz-r9-id=late-alpha" {
		t.Errorf("ExpectedIdentity = %q", m.ExpectedIdentity)
	}
	// Reconstructable fields must come straight from the derived layout.
	if m.RootfsPath != layout.RootfsDir || m.HandoffPath != layout.HandoffDir || m.IdentityFile != layout.IdentityFile {
		t.Errorf("paths not taken from layout:\n meta=%+v\n layout=%+v", m, layout)
	}
	if m.Status != IdentityPending {
		t.Errorf("Status = %q, want Pending", m.Status)
	}
	if m.Cleanup != CleanupActive {
		t.Errorf("Cleanup = %q, want Active", m.Cleanup)
	}
	if !m.VerifiedAt.IsZero() {
		t.Errorf("VerifiedAt should be zero before verification")
	}
	if m.Verified() {
		t.Errorf("a pending container must not report Verified")
	}
}

func TestVerifyExactIdentity(t *testing.T) {
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		expected string
		observed string
		want     IdentityStatus
		verified bool
	}{
		{"match", "id=late-alpha", "id=late-alpha", IdentityVerified, true},
		{"mismatch", "id=late-alpha", "id=late-beta", IdentityMismatch, false},
		{"missing", "id=late-alpha", "", IdentityMissing, false},
		// Substring of the expected value must NOT pass: R16 requires exact match.
		{"substring not enough", "id=late-alpha", "late-alpha", IdentityMismatch, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewHandoffMeta("macvz-cri-c1", layoutFor(t, "macvz-cri-c1"), tc.expected)
			got := m.Verify(tc.observed, now)
			if got != tc.want {
				t.Errorf("Verify status = %q, want %q", got, tc.want)
			}
			if m.Status != tc.want {
				t.Errorf("stored Status = %q, want %q", m.Status, tc.want)
			}
			if m.ObservedIdentity != tc.observed {
				t.Errorf("ObservedIdentity = %q, want %q", m.ObservedIdentity, tc.observed)
			}
			if !m.VerifiedAt.Equal(now) {
				t.Errorf("VerifiedAt = %v, want %v", m.VerifiedAt, now)
			}
			if m.Verified() != tc.verified {
				t.Errorf("Verified() = %v, want %v", m.Verified(), tc.verified)
			}
		})
	}
}

func TestEffectiveStatusZeroValueIsPending(t *testing.T) {
	// A restarted runtime that recovered metadata without a persisted result must
	// treat it as not-yet-verified, never as trusted.
	var m HandoffMeta
	if got := m.EffectiveStatus(); got != IdentityPending {
		t.Errorf("EffectiveStatus zero = %q, want Pending", got)
	}
	m.Status = IdentityVerified
	if got := m.EffectiveStatus(); got != IdentityVerified {
		t.Errorf("EffectiveStatus = %q, want Verified", got)
	}
}

func TestEffectiveCleanupZeroValueIsActive(t *testing.T) {
	var m HandoffMeta
	if got := m.EffectiveCleanup(); got != CleanupActive {
		t.Errorf("EffectiveCleanup zero = %q, want Active", got)
	}
	m.Cleanup = CleanupRemoved
	if got := m.EffectiveCleanup(); got != CleanupRemoved {
		t.Errorf("EffectiveCleanup = %q, want Removed", got)
	}
}

// TestReconstructableFromID proves the contract that the deterministic fields can
// be rebuilt from the container ID alone after a restart, while a best-effort
// result (ObservedIdentity/Status/VerifiedAt) may legitimately be lost.
func TestReconstructableFromID(t *testing.T) {
	const id = "macvz-cri-abc123"
	const expected = "id=late-alpha"

	// Original, fully verified.
	orig := NewHandoffMeta(id, layoutFor(t, id), expected)
	orig.Verify(expected, time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC))

	// After a restart that lost the best-effort result, recompute from the ID and
	// the recovered expected identity.
	recovered := NewHandoffMeta(id, layoutFor(t, id), expected)

	if recovered.ContainerID != orig.ContainerID ||
		recovered.RootfsPath != orig.RootfsPath ||
		recovered.HandoffPath != orig.HandoffPath ||
		recovered.IdentityFile != orig.IdentityFile ||
		recovered.ExpectedIdentity != orig.ExpectedIdentity {
		t.Errorf("deterministic fields did not reconstruct:\n orig=%+v\n recovered=%+v", orig, recovered)
	}
	// The recovered record is intentionally back to Pending: identity must be
	// re-established, never assumed from an unrecoverable prior result.
	if recovered.EffectiveStatus() != IdentityPending {
		t.Errorf("recovered status = %q, want Pending", recovered.EffectiveStatus())
	}
}

func TestJSONRoundTrip(t *testing.T) {
	id := "macvz-cri-c1"
	m := NewHandoffMeta(id, layoutFor(t, id), "id=late-alpha")
	m.Verify("id=late-alpha", time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC))
	m.Cleanup = CleanupStopped

	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got HandoffMeta
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != m {
		t.Errorf("round trip mismatch:\n got=%+v\n want=%+v", got, m)
	}
}
