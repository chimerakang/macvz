package linuxpod

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// These tests cover the CRI-L4 kubelet surfaces (#129): capability negotiation in
// Ping, CRI-format container logs, synchronous exec, and per-container stats. Each
// surface is exercised on both the supported path and the honest unsupported path
// (Capabilities.<X>=false -> ErrUnsupported), and the unsupported classification is
// proven to survive the NDJSON wire so the adapter can branch with errors.Is.

// startedContainer drives CreatePod -> Prepare -> Create(logPath) -> Start and
// returns the running container's ref. It runs against any Backend.
func startedContainer(t *testing.T, b Backend, podID, name, logPath string) Ref {
	t.Helper()
	ctx := context.Background()
	if _, err := b.CreatePod(ctx, PodSpec{ID: podID}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	rf, err := b.PrepareContainerRootfs(ctx, RootfsRequest{
		PodID: podID, ContainerName: name, ExpectedIdentity: "macvz-rootfs-id=" + name,
	})
	if err != nil {
		t.Fatalf("PrepareContainerRootfs: %v", err)
	}
	created, err := b.CreateContainer(ctx, CreateRequest{
		PodID: podID, Name: name, RootfsToken: rf.Token, LogPath: logPath,
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	ref := Ref{PodID: podID, ContainerID: created.ID}
	if _, err := b.StartContainer(ctx, ref); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	return ref
}

// TestPingAdvertisesCapabilities proves the handshake reports the kubelet surfaces
// the backend backs, both in-process and over the wire.
func TestPingAdvertisesCapabilities(t *testing.T) {
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
			if !info.Capabilities.Logs || !info.Capabilities.Exec || !info.Capabilities.Stats ||
				!info.Capabilities.Attach || !info.Capabilities.PortForward {
				t.Errorf("default fake should advertise all surfaces, got %+v", info.Capabilities)
			}
		})
	}
}

// TestContainerLogsCRIFormat proves the backend creates the kubelet log file and
// writes parseable CRI-format lines across the create/start lifecycle.
func TestContainerLogsCRIFormat(t *testing.T) {
	b := NewFakeBackend()
	logPath := filepath.Join(t.TempDir(), "pod", "app_0.log")
	ref := startedContainer(t, b, "p", "app", logPath)

	info, err := b.ContainerLogPath(context.Background(), ref)
	if err != nil {
		t.Fatalf("ContainerLogPath: %v", err)
	}
	if info.Path != logPath {
		t.Errorf("log path = %q, want %q", info.Path, logPath)
	}
	if info.Format != "cri" {
		t.Errorf("log format = %q, want cri", info.Format)
	}

	f, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open log file the backend should have created: %v", err)
	}
	defer f.Close()
	var lines int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines++
		// CRI log format: "<rfc3339nano> <stream> <P|F> <message>".
		parts := strings.SplitN(sc.Text(), " ", 4)
		if len(parts) < 4 {
			t.Fatalf("line %d not CRI format: %q", lines, sc.Text())
		}
		if _, err := time.Parse(time.RFC3339Nano, parts[0]); err != nil {
			t.Errorf("line %d timestamp %q not RFC3339Nano: %v", lines, parts[0], err)
		}
		if parts[1] != "stdout" && parts[1] != "stderr" {
			t.Errorf("line %d stream = %q, want stdout/stderr", lines, parts[1])
		}
		if parts[2] != "F" && parts[2] != "P" {
			t.Errorf("line %d tag = %q, want F/P", lines, parts[2])
		}
	}
	if lines < 2 {
		t.Errorf("expected create+start log lines, got %d", lines)
	}
}

// TestContainerLogPathUnsupported proves an honest ErrUnsupported when the backend
// does not back logs, and that it survives the wire.
func TestContainerLogPathUnsupported(t *testing.T) {
	be := NewFakeBackend()
	be.Capabilities.Logs = false
	ref := startedContainer(t, be, "p", "app", filepath.Join(t.TempDir(), "x.log"))

	for _, tc := range []struct {
		name string
		b    Backend
	}{
		{"fake", be},
		{"overPipe", newPipeClient(t, be)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.b.ContainerLogPath(context.Background(), ref); !errors.Is(err, ErrUnsupported) {
				t.Errorf("ContainerLogPath = %v, want ErrUnsupported", err)
			}
		})
	}
}

// TestContainerLogPathNoLogPath proves a container created without a log path is an
// ErrInvalid, not a silent empty path.
func TestContainerLogPathNoLogPath(t *testing.T) {
	b := NewFakeBackend()
	ref := startedContainer(t, b, "p", "app", "") // no log path
	if _, err := b.ContainerLogPath(context.Background(), ref); !errors.Is(err, ErrInvalid) {
		t.Errorf("ContainerLogPath without a log path = %v, want ErrInvalid", err)
	}
}

// TestExecSyncSupportedAndGated proves the supported exec path returns output for a
// running container, rejects exec on a non-running container and an empty command,
// and reports ErrUnsupported (over the wire too) when the surface is off.
func TestExecSyncSupportedAndGated(t *testing.T) {
	ctx := context.Background()

	t.Run("supported", func(t *testing.T) {
		b := NewFakeBackend()
		ref := startedContainer(t, b, "p", "app", "")
		res, err := b.ExecSync(ctx, ExecRequest{PodID: ref.PodID, ContainerID: ref.ContainerID, Command: []string{"echo", "hi"}})
		if err != nil {
			t.Fatalf("ExecSync: %v", err)
		}
		if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "echo hi") {
			t.Errorf("unexpected exec result: %+v stdout=%q", res, res.Stdout)
		}
	})

	t.Run("overPipeRoundTripsBytes", func(t *testing.T) {
		b := NewFakeBackend()
		ref := startedContainer(t, b, "p", "app", "")
		client := newPipeClient(t, b)
		res, err := client.ExecSync(ctx, ExecRequest{PodID: ref.PodID, ContainerID: ref.ContainerID, Command: []string{"id"}})
		if err != nil {
			t.Fatalf("ExecSync over pipe: %v", err)
		}
		if !strings.Contains(string(res.Stdout), "id") {
			t.Errorf("stdout did not round-trip: %q", res.Stdout)
		}
	})

	t.Run("emptyCommand", func(t *testing.T) {
		b := NewFakeBackend()
		ref := startedContainer(t, b, "p", "app", "")
		if _, err := b.ExecSync(ctx, ExecRequest{PodID: ref.PodID, ContainerID: ref.ContainerID}); !errors.Is(err, ErrInvalid) {
			t.Errorf("ExecSync(empty command) = %v, want ErrInvalid", err)
		}
	})

	t.Run("notRunning", func(t *testing.T) {
		b := NewFakeBackend()
		if _, err := b.CreatePod(ctx, PodSpec{ID: "p"}); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		rf, _ := b.PrepareContainerRootfs(ctx, RootfsRequest{PodID: "p", ContainerName: "c", ExpectedIdentity: "macvz-rootfs-id=c"})
		created, _ := b.CreateContainer(ctx, CreateRequest{PodID: "p", Name: "c", RootfsToken: rf.Token})
		if _, err := b.ExecSync(ctx, ExecRequest{PodID: "p", ContainerID: created.ID, Command: []string{"true"}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("ExecSync on Created container = %v, want ErrInvalid", err)
		}
	})

	t.Run("unsupportedOverPipe", func(t *testing.T) {
		be := NewFakeBackend()
		be.Capabilities.Exec = false
		ref := startedContainer(t, be, "p", "app", "")
		client := newPipeClient(t, be)
		if _, err := client.ExecSync(ctx, ExecRequest{PodID: ref.PodID, ContainerID: ref.ContainerID, Command: []string{"true"}}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("ExecSync = %v, want ErrUnsupported", err)
		}
	})
}

// TestContainerStats proves stats are returned for a known container, flagged
// Simulated so modeled numbers are never read as measured, and gated honestly.
func TestContainerStats(t *testing.T) {
	ctx := context.Background()

	t.Run("supported", func(t *testing.T) {
		b := NewFakeBackend()
		ref := startedContainer(t, b, "p", "app", "")
		st, err := b.ContainerStats(ctx, ref)
		if err != nil {
			t.Fatalf("ContainerStats: %v", err)
		}
		if !st.Simulated {
			t.Errorf("fake stats must be flagged Simulated: %+v", st)
		}
		if st.TimestampNanos == 0 || st.ContainerID != ref.ContainerID {
			t.Errorf("unexpected stats: %+v", st)
		}
	})

	t.Run("unknownContainer", func(t *testing.T) {
		b := NewFakeBackend()
		if _, err := b.CreatePod(ctx, PodSpec{ID: "p"}); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		if _, err := b.ContainerStats(ctx, Ref{PodID: "p", ContainerID: "nope"}); !errors.Is(err, ErrContainerNotFound) {
			t.Errorf("ContainerStats(unknown) = %v, want ErrContainerNotFound", err)
		}
	})

	t.Run("unsupportedOverPipe", func(t *testing.T) {
		be := NewFakeBackend()
		be.Capabilities.Stats = false
		ref := startedContainer(t, be, "p", "app", "")
		client := newPipeClient(t, be)
		if _, err := client.ContainerStats(ctx, ref); !errors.Is(err, ErrUnsupported) {
			t.Errorf("ContainerStats = %v, want ErrUnsupported", err)
		}
	})
}

// TestAttachSupportedAndGated covers the CRI-L4 follow-up (#131) Attach surface:
// the supported path negotiates streams for a running container (flagged Simulated),
// rejects a non-running/unknown container, and reports ErrUnsupported — over the
// wire too — when the capability is off.
func TestAttachSupportedAndGated(t *testing.T) {
	ctx := context.Background()

	t.Run("supported", func(t *testing.T) {
		b := NewFakeBackend()
		ref := startedContainer(t, b, "p", "app", "")
		res, err := b.Attach(ctx, AttachRequest{PodID: ref.PodID, ContainerID: ref.ContainerID, Stdin: true, Stdout: true, TTY: true})
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		if !res.Simulated || !res.Stdin || !res.Stdout || !res.TTY {
			t.Errorf("attach negotiation should echo requested streams and be Simulated: %+v", res)
		}
	})

	t.Run("overPipe", func(t *testing.T) {
		b := NewFakeBackend()
		ref := startedContainer(t, b, "p", "app", "")
		client := newPipeClient(t, b)
		res, err := client.Attach(ctx, AttachRequest{PodID: ref.PodID, ContainerID: ref.ContainerID, Stderr: true})
		if err != nil {
			t.Fatalf("Attach over pipe: %v", err)
		}
		if !res.Stderr || !res.Simulated {
			t.Errorf("attach negotiation did not round-trip: %+v", res)
		}
	})

	t.Run("notRunning", func(t *testing.T) {
		b := NewFakeBackend()
		if _, err := b.CreatePod(ctx, PodSpec{ID: "p"}); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		rf, _ := b.PrepareContainerRootfs(ctx, RootfsRequest{PodID: "p", ContainerName: "c", ExpectedIdentity: "macvz-rootfs-id=c"})
		created, _ := b.CreateContainer(ctx, CreateRequest{PodID: "p", Name: "c", RootfsToken: rf.Token})
		if _, err := b.Attach(ctx, AttachRequest{PodID: "p", ContainerID: created.ID, Stdout: true}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Attach on Created container = %v, want ErrInvalid", err)
		}
	})

	t.Run("unsupportedOverPipe", func(t *testing.T) {
		be := NewFakeBackend()
		be.Capabilities.Attach = false
		ref := startedContainer(t, be, "p", "app", "")
		client := newPipeClient(t, be)
		if _, err := client.Attach(ctx, AttachRequest{PodID: ref.PodID, ContainerID: ref.ContainerID, Stdout: true}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Attach = %v, want ErrUnsupported", err)
		}
	})
}

// TestPortForwardSupportedAndGated covers the #131 PortForward surface: the
// supported path negotiates forwardable ports for a known pod (flagged Simulated),
// rejects an unknown pod and an out-of-range port, and reports ErrUnsupported —
// over the wire too — when the capability is off.
func TestPortForwardSupportedAndGated(t *testing.T) {
	ctx := context.Background()

	t.Run("supportedOverPipe", func(t *testing.T) {
		b := NewFakeBackend()
		if _, err := b.CreatePod(ctx, PodSpec{ID: "p"}); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		client := newPipeClient(t, b)
		res, err := client.PortForward(ctx, PortForwardRequest{PodID: "p", Ports: []int32{8080, 9090}})
		if err != nil {
			t.Fatalf("PortForward: %v", err)
		}
		if !res.Simulated || len(res.Ports) != 2 || res.Ports[0] != 8080 || res.Ports[1] != 9090 {
			t.Errorf("port-forward negotiation did not round-trip: %+v", res)
		}
	})

	t.Run("unknownPod", func(t *testing.T) {
		b := NewFakeBackend()
		if _, err := b.PortForward(ctx, PortForwardRequest{PodID: "missing", Ports: []int32{80}}); !errors.Is(err, ErrPodNotFound) {
			t.Errorf("PortForward(unknown pod) = %v, want ErrPodNotFound", err)
		}
	})

	t.Run("badPort", func(t *testing.T) {
		b := NewFakeBackend()
		if _, err := b.CreatePod(ctx, PodSpec{ID: "p"}); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		if _, err := b.PortForward(ctx, PortForwardRequest{PodID: "p", Ports: []int32{70000}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("PortForward(bad port) = %v, want ErrInvalid", err)
		}
	})

	t.Run("unsupportedOverPipe", func(t *testing.T) {
		be := NewFakeBackend()
		be.Capabilities.PortForward = false
		if _, err := be.CreatePod(ctx, PodSpec{ID: "p"}); err != nil {
			t.Fatalf("CreatePod: %v", err)
		}
		client := newPipeClient(t, be)
		if _, err := client.PortForward(ctx, PortForwardRequest{PodID: "p", Ports: []int32{80}}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("PortForward = %v, want ErrUnsupported", err)
		}
	})
}

// TestLogFailureDoesNotWedgeLifecycle proves that an unwritable log path does not
// fail CreateContainer/StartContainer — a kubelet surface failure must not wedge
// the Pod lifecycle (#129).
func TestLogFailureDoesNotWedgeLifecycle(t *testing.T) {
	b := NewFakeBackend()
	// A path under a file (not a dir) makes MkdirAll/Open fail inside appendCRILog.
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	badLog := filepath.Join(file, "nested", "c.log")
	ref := startedContainer(t, b, "p", "app", badLog) // must not panic or fail
	st, err := b.Status(context.Background(), ref)
	if err != nil || st.Phase != runtime.PhaseRunning {
		t.Errorf("container should be Running despite log write failure: phase=%v err=%v", st.Phase, err)
	}
}
