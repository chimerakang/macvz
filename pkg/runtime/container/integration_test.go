package container

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/runtime"
)

// TestLifecycleIntegration exercises the real apple/container CLI end-to-end.
// It is skipped unless MACVZ_INTEGRATION=1 and a working `container` service is
// present, so the default `go test` stays hermetic.
func TestLifecycleIntegration(t *testing.T) {
	if os.Getenv("MACVZ_INTEGRATION") != "1" {
		t.Skip("set MACVZ_INTEGRATION=1 to run against a real apple/container service")
	}

	d := New(Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := d.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	const image = "docker.io/library/alpine:3.20"
	if err := d.Pull(ctx, image); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	spec := types.ContainerSpec{
		Name:    "macvz-it-probe",
		Image:   image,
		Command: []string{"sleep", "120"},
	}
	// Best-effort cleanup of any leftover from a previous run.
	_ = d.Destroy(ctx, spec.Name)

	id, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), id) })

	// Create is idempotent: a second call returns the same ID, no error.
	if id2, err := d.Create(ctx, spec); err != nil || id2 != id {
		t.Fatalf("idempotent Create: id=%q err=%v", id2, err)
	}

	if st, err := d.Status(ctx, id); err != nil {
		t.Fatalf("Status (created): %v", err)
	} else if st.Phase != runtime.PhaseCreated {
		t.Errorf("created phase = %q, want Created", st.Phase)
	}

	bootStart := time.Now()
	if err := d.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Start is idempotent: starting an already-running VM is a no-op success.
	if err := d.Start(ctx, id); err != nil {
		t.Fatalf("idempotent Start: %v", err)
	}

	// Poll until running and addressed; assert it boots within seconds.
	var st runtime.Status
	for i := 0; i < 30; i++ {
		st, err = d.Status(ctx, id)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.Phase == runtime.PhaseRunning && st.IP != "" {
			break
		}
		time.Sleep(time.Second)
	}
	if st.Phase != runtime.PhaseRunning {
		t.Fatalf("phase = %q, want Running", st.Phase)
	}
	if st.IP == "" {
		t.Error("expected an IP once running")
	}
	boot := time.Since(bootStart)
	t.Logf("Alpine micro-VM booted to Running with IP %s in %s", st.IP, boot.Round(time.Millisecond))
	if boot > 30*time.Second {
		t.Errorf("boot took %s, expected within seconds", boot)
	}

	// Exec a command and check streams + exit code mapping.
	var out strings.Builder
	if err := d.Exec(ctx, id, []string{"echo", "hello-macvz"},
		runtime.ExecIO{Stdout: &out}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(out.String(), "hello-macvz") {
		t.Errorf("exec stdout = %q, want it to contain hello-macvz", out.String())
	}

	// Logs should be readable.
	rc, err := d.Logs(ctx, id, runtime.LogOptions{Tail: 5})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	if err := d.Stop(ctx, id, 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop is idempotent: stopping an already-stopped VM is a no-op success.
	if err := d.Stop(ctx, id, 5*time.Second); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}

	if err := d.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	// Destroy is idempotent: destroying a missing VM is a no-op success.
	if err := d.Destroy(ctx, id); err != nil {
		t.Fatalf("idempotent Destroy: %v", err)
	}
	if _, err := d.Status(ctx, id); err == nil {
		t.Error("Status after Destroy should error (not found)")
	}
}

// TestLogStreamingIntegration verifies follow/tail semantics, stdout+stderr
// multiplexing, and context cancellation against a real running micro-VM.
func TestLogStreamingIntegration(t *testing.T) {
	if os.Getenv("MACVZ_INTEGRATION") != "1" {
		t.Skip("set MACVZ_INTEGRATION=1 to run against a real apple/container service")
	}

	d := New(Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := d.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	const image = "docker.io/library/alpine:3.20"
	if err := d.Pull(ctx, image); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	spec := types.ContainerSpec{
		Name:  "macvz-logs-probe",
		Image: image,
		// Emit to stdout AND stderr, then keep the VM alive for following.
		Command: []string{"sh", "-c", "echo OUT-line; echo ERR-line 1>&2; sleep 60"},
	}
	_ = d.Destroy(ctx, spec.Name)
	id, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), id) })
	if err := d.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Follow logs in a cancellable context; read until both streams appear.
	followCtx, followCancel := context.WithCancel(ctx)
	rc, err := d.Logs(followCtx, id, runtime.LogOptions{Follow: true})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 1024)
		for {
			n, rerr := rc.Read(buf)
			sb.Write(buf[:n])
			// Stop once we've multiplexed both stdout and stderr lines.
			if strings.Contains(sb.String(), "OUT-line") &&
				strings.Contains(sb.String(), "ERR-line") {
				done <- result{out: sb.String()}
				return
			}
			if rerr != nil {
				done <- result{out: sb.String(), err: rerr}
				return
			}
		}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		followCancel()
		_ = rc.Close()
		t.Fatal("timed out waiting for multiplexed log lines")
	}

	if !strings.Contains(got.out, "OUT-line") {
		t.Errorf("missing stdout line; got %q", got.out)
	}
	if !strings.Contains(got.out, "ERR-line") {
		t.Errorf("missing stderr line (multiplex failed); got %q", got.out)
	}

	// Cancellation must stop the follow without surfacing an error on Close.
	followCancel()
	if err := rc.Close(); err != nil {
		t.Errorf("Close after cancel = %v, want nil", err)
	}

	// Tail semantics: a non-follow read returns recent lines and closes clean.
	tail, err := d.Logs(ctx, id, runtime.LogOptions{Tail: 10})
	if err != nil {
		t.Fatalf("Logs(tail): %v", err)
	}
	data, _ := io.ReadAll(tail)
	if err := tail.Close(); err != nil {
		t.Errorf("Close(tail) = %v, want nil", err)
	}
	if !strings.Contains(string(data), "OUT-line") {
		t.Errorf("tail missing expected content; got %q", data)
	}
}

// TestExecIntegration verifies one-shot exec with exit-code propagation, stdin
// streaming, and context cancellation against a real running micro-VM.
func TestExecIntegration(t *testing.T) {
	if os.Getenv("MACVZ_INTEGRATION") != "1" {
		t.Skip("set MACVZ_INTEGRATION=1 to run against a real apple/container service")
	}

	d := New(Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := d.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	const image = "docker.io/library/alpine:3.20"
	if err := d.Pull(ctx, image); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	spec := types.ContainerSpec{Name: "macvz-exec-probe", Image: image, Command: []string{"sleep", "120"}}
	_ = d.Destroy(ctx, spec.Name)
	id, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), id) })
	if err := d.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait for running.
	for i := 0; i < 30; i++ {
		st, err := d.Status(ctx, id)
		if err == nil && st.Phase == runtime.PhaseRunning {
			break
		}
		time.Sleep(time.Second)
	}

	// One-shot exec, clean exit, stdout captured.
	var out strings.Builder
	if err := d.Exec(ctx, id, []string{"echo", "exec-ok"}, runtime.ExecIO{Stdout: &out}); err != nil {
		t.Fatalf("Exec(echo): %v", err)
	}
	if !strings.Contains(out.String(), "exec-ok") {
		t.Errorf("stdout = %q, want exec-ok", out.String())
	}

	// Non-zero exit must surface as *runtime.ExitError with the real code.
	err = d.Exec(ctx, id, []string{"sh", "-c", "exit 7"}, runtime.ExecIO{})
	var ee *runtime.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("Exec(exit 7) err = %v, want *runtime.ExitError", err)
	}
	if ee.Code != 7 {
		t.Errorf("exit code = %d, want 7", ee.Code)
	}

	// Stdin streaming: cat echoes what we feed it.
	var catOut strings.Builder
	if err := d.Exec(ctx, id, []string{"cat"},
		runtime.ExecIO{Stdin: strings.NewReader("piped-input\n"), Stdout: &catOut}); err != nil {
		t.Fatalf("Exec(cat): %v", err)
	}
	if !strings.Contains(catOut.String(), "piped-input") {
		t.Errorf("cat stdout = %q, want piped-input", catOut.String())
	}

	// Context cancellation aborts a long-running exec promptly.
	cctx, ccancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer ccancel()
	start := time.Now()
	err = d.Exec(cctx, id, []string{"sleep", "30"}, runtime.ExecIO{})
	if err == nil {
		t.Error("expected cancellation error for interrupted exec")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancellation took %s, expected prompt abort", elapsed)
	}

	// Exec against a stopped VM maps to ErrNotRunning.
	if err := d.Stop(ctx, id, 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := d.Exec(ctx, id, []string{"echo", "hi"}, runtime.ExecIO{}); !errors.Is(err, runtime.ErrNotRunning) {
		t.Errorf("exec on stopped VM err = %v, want ErrNotRunning", err)
	}
}

// TestArchVerificationIntegration confirms an arm64 image pulls and boots, and a
// non-arm64 image is rejected with a clear ErrIncompatibleArch — both at Pull
// (inspect-based) and at Create (auto-pull path).
func TestArchVerificationIntegration(t *testing.T) {
	if os.Getenv("MACVZ_INTEGRATION") != "1" {
		t.Skip("set MACVZ_INTEGRATION=1 to run against a real apple/container service")
	}

	d := New(Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := d.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// arm64 image: pulls and boots.
	const arm64Image = "docker.io/library/alpine:3.20"
	if err := d.Pull(ctx, arm64Image); err != nil {
		t.Fatalf("Pull(arm64): %v", err)
	}
	spec := types.ContainerSpec{Name: "macvz-arch-arm64", Image: arm64Image, Command: []string{"sleep", "30"}}
	_ = d.Destroy(ctx, spec.Name)
	id, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create(arm64): %v", err)
	}
	t.Cleanup(func() { _ = d.Destroy(context.Background(), id) })
	if err := d.Start(ctx, id); err != nil {
		t.Fatalf("Start(arm64): %v", err)
	}

	// amd64-only image: Pull must reject with a clear, actionable arch error.
	const amd64Image = "docker.io/amd64/alpine:3.20"
	err = d.Pull(ctx, amd64Image)
	if !errors.Is(err, runtime.ErrIncompatibleArch) {
		t.Fatalf("Pull(amd64) err = %v, want ErrIncompatibleArch", err)
	}
	t.Logf("non-arm64 Pull error (actionable): %v", err)
	if !strings.Contains(err.Error(), "linux/arm64") {
		t.Errorf("error should name the missing target arch; got %q", err.Error())
	}

	// Create path (auto-pull) must also surface ErrIncompatibleArch, not the
	// runtime's cryptic platform message.
	_ = d.Destroy(ctx, "macvz-arch-amd64")
	_, err = d.Create(ctx, types.ContainerSpec{Name: "macvz-arch-amd64", Image: amd64Image, Command: []string{"true"}})
	t.Cleanup(func() { _ = d.Destroy(context.Background(), "macvz-arch-amd64") })
	if !errors.Is(err, runtime.ErrIncompatibleArch) {
		t.Errorf("Create(amd64) err = %v, want ErrIncompatibleArch", err)
	}
}
