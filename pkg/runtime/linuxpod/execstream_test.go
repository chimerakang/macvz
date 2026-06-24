package linuxpod

import (
	"context"
	"errors"
	"testing"
)

// These tests cover the interactive/streaming exec negotiation surface (CRI-L4
// follow-up #132): ExecStream is capability-gated, validates the target, reports
// the negotiated session flagged Simulated, and survives the NDJSON wire. They run
// against both FakeBackend directly and HelperClient -> Serve(FakeBackend) over a
// pipe, the same way the other surface tests do.

// TestExecStreamCapabilityAdvertised proves the new ExecStream capability is
// reported in the handshake, in-process and over the wire.
func TestExecStreamCapabilityAdvertised(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    Backend
	}{
		{"fake", NewFakeBackend()},
		{"overPipe", newPipeClient(t, NewFakeBackend())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := tc.b.Ping(context.Background())
			if err != nil {
				t.Fatalf("Ping: %v", err)
			}
			if !info.Capabilities.ExecStream {
				t.Errorf("default fake should advertise ExecStream, got %+v", info.Capabilities)
			}
		})
	}
}

// TestExecStreamNegotiation proves a supported session reports the requested
// streams as attachable, flags the result simulated, and folds stderr into stdout
// for a TTY session — over the wire too.
func TestExecStreamNegotiation(t *testing.T) {
	ctx := context.Background()

	t.Run("nonTTYEchoesStreams", func(t *testing.T) {
		b := NewFakeBackend()
		ref := startedContainer(t, b, "p", "app", "")
		resp, err := b.ExecStream(ctx, ExecStreamRequest{
			PodID: ref.PodID, ContainerID: ref.ContainerID,
			Command: []string{"/bin/sh"}, Stdin: true, Stdout: true, Stderr: true,
		})
		if err != nil {
			t.Fatalf("ExecStream: %v", err)
		}
		if !resp.Simulated {
			t.Errorf("fake ExecStream must be flagged Simulated: %+v", resp)
		}
		if !resp.Stdin || !resp.Stdout || !resp.Stderr || resp.TTY {
			t.Errorf("non-TTY negotiation should attach stdin/stdout/stderr, no TTY: %+v", resp)
		}
	})

	t.Run("ttyFoldsStderrOverPipe", func(t *testing.T) {
		b := NewFakeBackend()
		ref := startedContainer(t, b, "p", "app", "")
		client := newPipeClient(t, b)
		resp, err := client.ExecStream(ctx, ExecStreamRequest{
			PodID: ref.PodID, ContainerID: ref.ContainerID,
			Command: []string{"/bin/sh"}, Stdin: true, Stdout: true, Stderr: true, TTY: true,
		})
		if err != nil {
			t.Fatalf("ExecStream over pipe: %v", err)
		}
		if !resp.TTY || resp.Stderr {
			t.Errorf("TTY session must fold stderr into stdout (Stderr=false): %+v", resp)
		}
		if !resp.Stdin || !resp.Stdout || !resp.Simulated {
			t.Errorf("unexpected TTY negotiation: %+v", resp)
		}
	})
}

// TestExecStreamGated proves the honest failure modes: ErrUnsupported when the
// surface is off (over the wire too), ErrInvalid for a non-running container and an
// empty command.
func TestExecStreamGated(t *testing.T) {
	ctx := context.Background()

	t.Run("unsupportedOverPipe", func(t *testing.T) {
		be := NewFakeBackend()
		be.Capabilities.ExecStream = false
		ref := startedContainer(t, be, "p", "app", "")
		client := newPipeClient(t, be)
		if _, err := client.ExecStream(ctx, ExecStreamRequest{
			PodID: ref.PodID, ContainerID: ref.ContainerID, Command: []string{"true"},
		}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("ExecStream = %v, want ErrUnsupported", err)
		}
	})

	t.Run("emptyCommand", func(t *testing.T) {
		b := NewFakeBackend()
		ref := startedContainer(t, b, "p", "app", "")
		if _, err := b.ExecStream(ctx, ExecStreamRequest{
			PodID: ref.PodID, ContainerID: ref.ContainerID,
		}); !errors.Is(err, ErrInvalid) {
			t.Errorf("ExecStream(empty command) = %v, want ErrInvalid", err)
		}
	})

	t.Run("notRunning", func(t *testing.T) {
		b := NewFakeBackend()
		if _, err := b.CreatePod(ctx, PodSpec{ID: "p"}); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		rf, _ := b.PrepareContainerRootfs(ctx, RootfsRequest{PodID: "p", ContainerName: "c", ExpectedIdentity: "macvz-rootfs-id=c"})
		created, _ := b.CreateContainer(ctx, CreateRequest{PodID: "p", Name: "c", RootfsToken: rf.Token})
		if _, err := b.ExecStream(ctx, ExecStreamRequest{
			PodID: "p", ContainerID: created.ID, Command: []string{"true"},
		}); !errors.Is(err, ErrInvalid) {
			t.Errorf("ExecStream on Created container = %v, want ErrInvalid", err)
		}
	})

	t.Run("unknownContainer", func(t *testing.T) {
		b := NewFakeBackend()
		if _, err := b.CreatePod(ctx, PodSpec{ID: "p"}); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		if _, err := b.ExecStream(ctx, ExecStreamRequest{
			PodID: "p", ContainerID: "nope", Command: []string{"true"},
		}); !errors.Is(err, ErrContainerNotFound) {
			t.Errorf("ExecStream(unknown) = %v, want ErrContainerNotFound", err)
		}
	})
}
