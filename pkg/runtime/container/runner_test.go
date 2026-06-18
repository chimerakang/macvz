package container

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCappedBufferBounds(t *testing.T) {
	b := &cappedBuffer{limit: 8}
	n, _ := b.Write([]byte("hello"))
	if n != 5 {
		t.Fatalf("Write reported %d, want full 5", n)
	}
	// Second write overflows the cap; only 3 more bytes are retained.
	n, _ = b.Write([]byte("world!!"))
	if n != 7 {
		t.Fatalf("Write reported %d, want full 7 (reports all consumed)", n)
	}
	if got := b.String(); got != "hellowor" {
		t.Errorf("buffer = %q, want %q (capped at 8)", got, "hellowor")
	}
}

func TestCommandErrorWithoutErrIsSafeToFormat(t *testing.T) {
	err := (&CommandError{Args: []string{"exec", "pod-x", "false"}, ExitCode: 1}).Error()
	if !strings.Contains(err, "command failed") {
		t.Fatalf("Error() = %q, want fallback message", err)
	}
}

// shRunner is a cliRunner that drives /bin/sh, so the streaming machinery
// (pipe + cmdReadCloser) can be exercised hermetically without the container
// service.
func shRunner() *cliRunner { return &cliRunner{bin: "/bin/sh"} }

func TestPipeStreamsAndCleanExit(t *testing.T) {
	rc, err := shRunner().pipe(context.Background(), "-c", "echo out; echo err 1>&2")
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	data, _ := io.ReadAll(rc)
	if !strings.Contains(string(data), "out") {
		t.Errorf("stdout not streamed; got %q", data)
	}
	// Clean (exit 0) command: Close returns nil.
	if err := rc.Close(); err != nil {
		t.Errorf("Close after clean exit = %v, want nil", err)
	}
}

func TestPipeSurfacesFailureOnClose(t *testing.T) {
	rc, err := shRunner().pipe(context.Background(), "-c", "echo boom 1>&2; exit 3")
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	_, _ = io.Copy(io.Discard, rc)
	// Let the process exit on its own before Close inspects it.
	time.Sleep(20 * time.Millisecond)
	err = rc.Close()
	var ce *CommandError
	if !errors.As(err, &ce) {
		t.Fatalf("Close error = %v, want *CommandError", err)
	}
	if ce.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", ce.ExitCode)
	}
	if !strings.Contains(ce.Stderr, "boom") {
		t.Errorf("captured stderr = %q, want it to contain boom", ce.Stderr)
	}
}

func TestPipeCloseDuringFollowIsNotError(t *testing.T) {
	// A long-running "follow" the caller stops early: Close must not error.
	rc, err := shRunner().pipe(context.Background(), "-c", "sleep 30")
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Errorf("Close during follow = %v, want nil", err)
	}
}

func TestPipeContextCancelIsNotError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rc, err := shRunner().pipe(ctx, "-c", "sleep 30")
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	cancel()
	time.Sleep(20 * time.Millisecond)
	if err := rc.Close(); err != nil {
		t.Errorf("Close after ctx cancel = %v, want nil", err)
	}
}
