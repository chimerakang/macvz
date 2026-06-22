package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/runtime"
)

// TestHandoffLaunchIntegration is the gated live proof for CRI-I2-3 (#114). It
// moves the R15 harness-only evidence proof onto the production handoff helpers
// and spec shape: it stages the runtime-private handoff layout with
// runtime.HandoffManager (#109), injects the production handoff bind mount with
// runtime.InjectHandoffMount (#112), launches a real apple/container workload
// whose VirtioFS-shared handoff directory is bind-mounted at the production
// runtime.HandoffMountPoint, has the launched process write its rootfs identity
// into that handoff path in the runtime.FormatIdentity format (#113, the R15
// evidence channel), and verifies identity with runtime.VerifyHandoffIdentity
// exactly as the future CRI StartContainer path will.
//
// It is skipped unless MACVZ_INTEGRATION=1 and a working `container` service is
// present, so the default `go test ./...` stays hermetic and never requires
// apple/container (CRI-I2-3 acceptance). On success it logs the outcome
// runtimeHandoffLaunchSucceeded; every failure path logs a precise outcome so an
// operator report records exactly which stage failed.
//
// Run it (and write a live report) with:
//
//	MACVZ_INTEGRATION=1 go test ./pkg/runtime/container/ \
//	    -run TestHandoffLaunchIntegration -v
//
// This deliberately does NOT use the vminitd late-rootfs primitive path (that
// remains the gated Swift harness, docs/CRI_RUNTIME_R15_EVIDENCE_CHANNEL_REPORT.md):
// it proves the production Go handoff helpers, bind-mount shape, identity file
// format, and verification end to end against the shipped runtime backend.
func TestHandoffLaunchIntegration(t *testing.T) {
	if os.Getenv("MACVZ_INTEGRATION") != "1" {
		t.Skip("set MACVZ_INTEGRATION=1 to run against a real apple/container service")
	}

	d := New(Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := d.Ready(ctx); err != nil {
		t.Fatalf("[outcome=runtimeHandoffServiceUnavailable] Ready: %v", err)
	}

	const image = "docker.io/library/alpine:3.20"
	if err := d.Pull(ctx, image, nil); err != nil {
		t.Fatalf("[outcome=runtimeHandoffImagePullFailed] Pull: %v", err)
	}

	// Root the handoff manager at a host temp dir rather than the production
	// /run/macvz/containers: the per-container layout shape (rootfs/handoff
	// subdirs, identity file) and the in-guest HandoffMountPoint are still the
	// production constants, but the host source stays test-owned so the run needs
	// no root and cleans up with the test. apple/container shares this directory
	// into the guest over VirtioFS.
	const containerID = "macvz-handoff-it"
	mgr := runtime.NewHandoffManager(t.TempDir())
	layout, err := mgr.Create(containerID)
	if err != nil {
		t.Fatalf("[outcome=runtimeHandoffPrepareFailed] HandoffManager.Create: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Cleanup(containerID) })

	// The identity the late process must report. In production this is staged into
	// the prepared rootfs (runtime.StageIdentityFile) and recovered into
	// HandoffMeta.ExpectedIdentity; here it is the start invariant the process
	// echoes back through the handoff evidence channel. The value contains '=', so
	// it also exercises the first-'=' split in the evidence parser.
	const expectedIdentity = "macvz-handoff-id=late-alpha"
	meta := runtime.NewHandoffMeta(containerID, layout, expectedIdentity)

	// Build the workload spec and inject the production handoff bind mount (#112)
	// rather than hand-rolling a types.Mount: the handoff directory is bind-mounted
	// writable at the production HandoffMountPoint over VirtioFS.
	spec := types.ContainerSpec{Name: containerID, Image: image}
	if err := runtime.InjectHandoffMount(&spec, layout); err != nil {
		t.Fatalf("[outcome=runtimeHandoffPrepareFailed] InjectHandoffMount: %v", err)
	}

	// The launched process writes the canonical identity line (runtime.FormatIdentity
	// shape) plus a proc_root diagnostic into the bind-mounted handoff file, then
	// syncs so VirtioFS write-back reaches the host before we read it.
	guestIdentityPath := filepath.Join(runtime.HandoffMountPoint, runtime.IdentityFile)
	writeScript := fmt.Sprintf(
		"printf '%sexpected=%s\\nproc_root=/\\n' > %s && sync",
		runtime.FormatIdentity(expectedIdentity), expectedIdentity, guestIdentityPath)
	spec.Command = []string{"sh", "-c", writeScript}

	_ = d.Destroy(ctx, spec.Name)

	id, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("[outcome=runtimeHandoffCreateFailed] Create: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), id) })

	if err := d.Start(ctx, id); err != nil {
		t.Fatalf("[outcome=runtimeHandoffStartFailed] Start: %v", err)
	}

	// The process writes the identity file and exits. Wait for the host-side
	// handoff file (written through VirtioFS) to appear and become non-empty
	// rather than racing the process; the workload phase is checked alongside so a
	// crashed process surfaces as a precise failure instead of a timeout.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if b, rerr := os.ReadFile(layout.IdentityFile); rerr == nil && len(strings.TrimSpace(string(b))) > 0 {
			break
		}
		st, serr := d.Status(ctx, id)
		if serr == nil && st.Phase == runtime.PhaseFailed {
			t.Fatalf("[outcome=runtimeHandoffProcessFailed] workload failed before writing evidence: phase=%q exit=%d msg=%q",
				st.Phase, st.ExitCode, st.Message)
		}
		// A non-zero exit with no evidence on the host means the handoff write or
		// VirtioFS write-back did not work — a precise, distinct failure.
		if serr == nil && st.Phase == runtime.PhaseStopped && st.ExitCode != 0 {
			t.Fatalf("[outcome=runtimeHandoffProcessFailed] workload exited non-zero before evidence: exit=%d msg=%q",
				st.ExitCode, st.Message)
		}
		if time.Now().After(deadline) {
			t.Fatalf("[outcome=runtimeHandoffEvidenceMissing] no identity evidence at host path %q within timeout",
				layout.IdentityFile)
		}
		time.Sleep(time.Second)
	}

	// Verify identity through the production contract (#113): exact-match against
	// meta.ExpectedIdentity, with proc_root carried only as a diagnostic.
	ev, err := runtime.VerifyHandoffIdentity(&meta, time.Now())
	switch {
	case err == nil:
		t.Logf("handoff identity verified: observed=%q expected=%q diagnostics=%v",
			ev.Identity, expectedIdentity, ev.DiagnosticKeys())
		t.Logf("[outcome=runtimeHandoffLaunchSucceeded] handoff=%s mount=%s status=%s",
			layout.HandoffDir, runtime.HandoffMountPoint, meta.Status)
	case errors.Is(err, runtime.ErrIdentityMismatch):
		t.Fatalf("[outcome=runtimeHandoffIdentityMismatch] %v", err)
	case errors.Is(err, runtime.ErrEvidenceMissing):
		t.Fatalf("[outcome=runtimeHandoffEvidenceMissing] %v", err)
	case errors.Is(err, runtime.ErrEvidenceMalformed):
		t.Fatalf("[outcome=runtimeHandoffEvidenceMalformed] %v", err)
	default:
		t.Fatalf("[outcome=runtimeHandoffVerifyFailed] VerifyHandoffIdentity: %v", err)
	}
}
