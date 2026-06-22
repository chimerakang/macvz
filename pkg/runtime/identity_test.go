package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitForHandoffIdentityArrivesLate(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC) }
	m := metaWithEvidence(t, "id=late-alpha", "") // no evidence yet
	// Write the evidence shortly after the wait begins.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = os.WriteFile(m.IdentityFile, []byte("identity=id=late-alpha\n"), 0o644)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := WaitForHandoffIdentity(ctx, m, now, 5*time.Millisecond); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !m.Verified() {
		t.Errorf("meta not verified after late arrival: %+v", m)
	}
}

func TestWaitForHandoffIdentityTimeout(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC) }
	m := metaWithEvidence(t, "id=late-alpha", "") // evidence never written
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := WaitForHandoffIdentity(ctx, m, now, 5*time.Millisecond)
	if !errors.Is(err, ErrEvidenceMissing) {
		t.Fatalf("err = %v, want ErrEvidenceMissing", err)
	}
	if m.Status != IdentityMissing {
		t.Errorf("Status = %q, want Missing", m.Status)
	}
}

func TestWaitForHandoffIdentityMismatchIsTerminal(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC) }
	m := metaWithEvidence(t, "id=late-alpha", "identity=id=late-beta\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// A mismatch must return promptly, not block until the deadline.
	start := time.Now()
	_, err := WaitForHandoffIdentity(ctx, m, now, 50*time.Millisecond)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("err = %v, want ErrIdentityMismatch", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("mismatch should be terminal, took %v", time.Since(start))
	}
}

func TestWaitForHandoffIdentityBadInputs(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC) }
	if _, err := WaitForHandoffIdentity(context.Background(), nil, now, time.Millisecond); err == nil {
		t.Fatalf("nil metadata should error, not panic")
	}

	m := metaWithEvidence(t, "id=late-alpha", "") // evidence never written
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := WaitForHandoffIdentity(ctx, m, now, time.Second)
	if !errors.Is(err, ErrEvidenceMissing) {
		t.Fatalf("err = %v, want ErrEvidenceMissing", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled context waited for poll interval: %v", elapsed)
	}
}

func TestParseHandoffEvidenceSuccess(t *testing.T) {
	// R15's exact format: identity value itself contains '=', plus an expected
	// self-report and a proc_root diagnostic that must not affect success.
	in := "identity=macvz-r9-id=late-alpha\nexpected=macvz-r9-id=late-alpha\nproc_root=/\n"
	ev, err := ParseHandoffEvidence(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Identity != "macvz-r9-id=late-alpha" {
		t.Errorf("Identity = %q, want value past the first '='", ev.Identity)
	}
	if got, ok := ev.Get("expected"); !ok || got != "macvz-r9-id=late-alpha" {
		t.Errorf("expected diagnostic = %q,%v", got, ok)
	}
	if got, ok := ev.Get("proc_root"); !ok || got != "/" {
		t.Errorf("proc_root diagnostic = %q,%v", got, ok)
	}
	// identity must not leak into the diagnostics map.
	if _, ok := ev.Get(EvidenceIdentityKey); ok {
		t.Errorf("identity key should not appear in diagnostics")
	}
}

func TestParseHandoffEvidenceExtraDiagnostics(t *testing.T) {
	// A long mounts listing and unknown keys are captured, never fatal.
	mounts := "/ /proc proc rw 0 0; / /sys sysfs rw 0 0"
	in := "proc_root=/\nidentity=id-1\nmounts=" + mounts + "\npid=1234\n# a comment\n\nstartedAt=2026-06-22T10:00:00Z\n"
	ev, err := ParseHandoffEvidence(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Identity != "id-1" {
		t.Errorf("Identity = %q", ev.Identity)
	}
	if got, _ := ev.Get("mounts"); got != mounts {
		t.Errorf("mounts diagnostic = %q, want %q", got, mounts)
	}
	if got, _ := ev.Get("pid"); got != "1234" {
		t.Errorf("pid = %q", got)
	}
	// Order is first-seen: proc_root before mounts before pid before startedAt.
	want := []string{"proc_root", "mounts", "pid", "startedAt"}
	got := ev.DiagnosticKeys()
	if len(got) != len(want) {
		t.Fatalf("DiagnosticKeys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DiagnosticKeys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseHandoffEvidenceMissingIdentity(t *testing.T) {
	// Present diagnostics but no identity key.
	_, err := ParseHandoffEvidence(strings.NewReader("proc_root=/\nexpected=id-1\n"))
	if !errors.Is(err, ErrEvidenceMissing) {
		t.Fatalf("err = %v, want ErrEvidenceMissing", err)
	}
}

func TestParseHandoffEvidenceEmpty(t *testing.T) {
	_, err := ParseHandoffEvidence(strings.NewReader(""))
	if !errors.Is(err, ErrEvidenceMissing) {
		t.Fatalf("err = %v, want ErrEvidenceMissing", err)
	}
}

func TestParseHandoffEvidenceMalformed(t *testing.T) {
	cases := map[string]string{
		"line without separator": "identity=id-1\nthis-line-has-no-equals\n",
		"empty key":              "=value-without-key\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseHandoffEvidence(strings.NewReader(in))
			if !errors.Is(err, ErrEvidenceMalformed) {
				t.Fatalf("err = %v, want ErrEvidenceMalformed", err)
			}
		})
	}
}

func TestParseHandoffEvidenceDuplicateIdentityFirstWins(t *testing.T) {
	ev, err := ParseHandoffEvidence(strings.NewReader("identity=first\nidentity=second\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.Identity != "first" {
		t.Errorf("Identity = %q, want first-wins", ev.Identity)
	}
	if got, ok := ev.Get("identity#2"); !ok || got != "second" {
		t.Errorf("duplicate identity not preserved: %q,%v", got, ok)
	}
}

func TestStageAndReadStagedIdentity(t *testing.T) {
	rootfs := t.TempDir()
	const id = "macvz-r9-id=late-alpha"
	if err := StageIdentityFile(rootfs, id); err != nil {
		t.Fatalf("StageIdentityFile: %v", err)
	}
	// File lands at <rootfs>/etc/macvz-container-identity in the canonical format.
	p := filepath.Join(rootfs, "etc", "macvz-container-identity")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	if string(b) != "identity="+id+"\n" {
		t.Errorf("staged content = %q", string(b))
	}
	got, err := ReadStagedIdentity(rootfs)
	if err != nil {
		t.Fatalf("ReadStagedIdentity: %v", err)
	}
	if got != id {
		t.Errorf("ReadStagedIdentity = %q, want %q", got, id)
	}
}

func TestStageIdentityFileRejectsEmpty(t *testing.T) {
	if err := StageIdentityFile(t.TempDir(), ""); err == nil {
		t.Fatalf("expected error for empty identity")
	}
	if err := StageIdentityFile("", "id=late-alpha"); err == nil {
		t.Fatalf("expected error for empty rootfs dir")
	}
}

func TestReadStagedIdentityMissing(t *testing.T) {
	_, err := ReadStagedIdentity(t.TempDir())
	if !errors.Is(err, ErrEvidenceMissing) {
		t.Fatalf("err = %v, want ErrEvidenceMissing", err)
	}
	if _, err := ReadStagedIdentity(""); err == nil {
		t.Fatalf("expected error for empty rootfs dir")
	}
}

// writeEvidence stages a handoff evidence file and returns metadata pointing at
// it, mirroring how the runtime would derive paths via the #109 helper.
func metaWithEvidence(t *testing.T, expected, evidenceBody string) *HandoffMeta {
	t.Helper()
	dir := t.TempDir()
	m := NewHandoffMeta("macvz-cri-c1", HandoffLayout{
		HandoffDir:   dir,
		IdentityFile: filepath.Join(dir, IdentityFile),
	}, expected)
	if evidenceBody != "" {
		if err := os.WriteFile(m.IdentityFile, []byte(evidenceBody), 0o644); err != nil {
			t.Fatalf("write evidence: %v", err)
		}
	}
	return &m
}

func TestVerifyHandoffIdentityVerified(t *testing.T) {
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	m := metaWithEvidence(t, "id=late-alpha", "identity=id=late-alpha\nproc_root=/\n")
	ev, err := VerifyHandoffIdentity(m, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !m.Verified() || m.Status != IdentityVerified {
		t.Errorf("meta not verified: %+v", m)
	}
	if m.ObservedIdentity != "id=late-alpha" {
		t.Errorf("ObservedIdentity = %q", m.ObservedIdentity)
	}
	if !m.VerifiedAt.Equal(now) {
		t.Errorf("VerifiedAt = %v", m.VerifiedAt)
	}
	if got, _ := ev.Get("proc_root"); got != "/" {
		t.Errorf("diagnostics dropped: %q", got)
	}
}

func TestVerifyHandoffIdentityMismatch(t *testing.T) {
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	m := metaWithEvidence(t, "id=late-alpha", "identity=id=late-beta\n")
	_, err := VerifyHandoffIdentity(m, now)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("err = %v, want ErrIdentityMismatch", err)
	}
	// Error must carry both expected and observed where safe.
	if !strings.Contains(err.Error(), "id=late-alpha") || !strings.Contains(err.Error(), "id=late-beta") {
		t.Errorf("error missing expected/observed: %v", err)
	}
	if m.Status != IdentityMismatch {
		t.Errorf("Status = %q, want Mismatch", m.Status)
	}
}

func TestVerifyHandoffIdentityMissingFile(t *testing.T) {
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	m := metaWithEvidence(t, "id=late-alpha", "") // no file written
	_, err := VerifyHandoffIdentity(m, now)
	if !errors.Is(err, ErrEvidenceMissing) {
		t.Fatalf("err = %v, want ErrEvidenceMissing", err)
	}
	if !strings.Contains(err.Error(), "id=late-alpha") {
		t.Errorf("error should include expected identity: %v", err)
	}
	if m.Status != IdentityMissing {
		t.Errorf("Status = %q, want Missing", m.Status)
	}
}

func TestVerifyHandoffIdentityMalformed(t *testing.T) {
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	m := metaWithEvidence(t, "id=late-alpha", "garbage-without-separator\n")
	_, err := VerifyHandoffIdentity(m, now)
	if !errors.Is(err, ErrEvidenceMalformed) {
		t.Fatalf("err = %v, want ErrEvidenceMalformed", err)
	}
	if m.Status != IdentityMissing {
		t.Errorf("Status = %q, want Missing (unusable channel)", m.Status)
	}
}

func TestVerifyHandoffIdentityBadMetadata(t *testing.T) {
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	if _, err := VerifyHandoffIdentity(nil, now); err == nil {
		t.Fatalf("nil metadata should error, not panic")
	}
	m := &HandoffMeta{ExpectedIdentity: "id=late-alpha"}
	if _, err := VerifyHandoffIdentity(m, now); !errors.Is(err, ErrEvidenceMissing) {
		t.Fatalf("empty identity path err = %v, want ErrEvidenceMissing", err)
	}
	if m.Status != IdentityMissing {
		t.Errorf("Status = %q, want Missing", m.Status)
	}
}

func TestReadStderrDiagnostics(t *testing.T) {
	dir := t.TempDir()
	// Absent stderr is best-effort empty, no error.
	if got := ReadStderrDiagnostics(dir); got != "" {
		t.Errorf("absent stderr = %q, want empty", got)
	}
	if err := os.WriteFile(filepath.Join(dir, StderrFile), []byte("exec: not found\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadStderrDiagnostics(dir); got != "exec: not found" {
		t.Errorf("stderr = %q", got)
	}
}

func TestReadStderrDiagnosticsBounded(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 32*1024)
	if err := os.WriteFile(filepath.Join(dir, StderrFile), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadStderrDiagnostics(dir); len(got) > 16*1024 {
		t.Errorf("stderr not bounded: %d bytes", len(got))
	}
}
