package criserver

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// newServerWithLogDir builds a server whose sandbox writes logs under dir, and
// returns the server and the sandbox id. The container LogPath is "app.log".
func newServerWithLogDir(t *testing.T, rt ContainerRuntime, dir string) (*Server, string) {
	t.Helper()
	s := New(Options{Runtime: rt})
	resp, err := s.RunPodSandbox(context.Background(), &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata:     &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
			LogDirectory: dir,
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	return s, resp.GetPodSandboxId()
}

func createReqWithLog(sandboxID, name, logPath string) *runtimeapi.CreateContainerRequest {
	req := createReq(sandboxID, name)
	req.Config.LogPath = logPath
	return req
}

func TestLogPumpWritesCRIFormat(t *testing.T) {
	dir := t.TempDir()
	rt := newFakeRuntime()
	rt.logsData = "first line\nsecond line\n"
	s, sandboxID := newServerWithLogDir(t, rt, dir)
	ctx := context.Background()

	cResp, err := s.CreateContainer(ctx, createReqWithLog(sandboxID, "app", "app.log"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	// The pump streams a finite reader and exits; stopping it waits for completion.
	s.stopLogPump(id)

	if !rt.logsFollow {
		t.Error("expected Logs to be opened with Follow=true")
	}
	data, err := os.ReadFile(filepath.Join(dir, "app.log"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2: %q", len(lines), data)
	}
	for i, want := range []string{"first line", "second line"} {
		fields := strings.SplitN(lines[i], " ", 4)
		if len(fields) != 4 {
			t.Fatalf("line %d not CRI-formatted: %q", i, lines[i])
		}
		if _, err := time.Parse(time.RFC3339Nano, fields[0]); err != nil {
			t.Errorf("line %d timestamp %q not RFC3339Nano: %v", i, fields[0], err)
		}
		if fields[1] != "stdout" || fields[2] != "F" {
			t.Errorf("line %d stream/tag = %q %q, want stdout F", i, fields[1], fields[2])
		}
		if fields[3] != want {
			t.Errorf("line %d message = %q, want %q", i, fields[3], want)
		}
	}
}

func TestLogPumpSelfExitIsNotReopenable(t *testing.T) {
	dir := t.TempDir()
	rt := newFakeRuntime()
	rt.logsData = "one-shot\n"
	s, sandboxID := newServerWithLogDir(t, rt, dir)
	ctx := context.Background()

	cResp, err := s.CreateContainer(ctx, createReqWithLog(sandboxID, "app", "app.log"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	waitForPumpGone(t, s, id)

	_, err = s.ReopenContainerLog(ctx, &runtimeapi.ReopenContainerLogRequest{ContainerId: id})
	wantCode(t, err, codes.FailedPrecondition)
}

func TestLogPumpAbsentWithoutLogPath(t *testing.T) {
	rt := newFakeRuntime()
	rt.logsData = "x\n"
	s, sandboxID := newServerWithRuntime(t, rt) // sandbox has no LogDirectory
	id := startedContainer(t, s, sandboxID)

	s.logMu.Lock()
	_, ok := s.logPumps[id]
	s.logMu.Unlock()
	if ok {
		t.Error("expected no log pump when sandbox has no log directory")
	}
}

func TestReopenContainerLog(t *testing.T) {
	dir := t.TempDir()
	rt := newFakeRuntime()
	// A pipe keeps the follow stream open so the pump stays alive across the reopen.
	pr, pw := io.Pipe()
	rt.logsReader = pr
	s, sandboxID := newServerWithLogDir(t, rt, dir)
	ctx := context.Background()

	cResp, _ := s.CreateContainer(ctx, createReqWithLog(sandboxID, "app", "app.log"))
	id := cResp.GetContainerId()
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}

	logFile := filepath.Join(dir, "app.log")
	if _, err := pw.Write([]byte("before rotate\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitForFileContains(t, logFile, "before rotate")

	// Rotate: move the current file aside, then reopen so the pump writes to a fresh
	// file at the same path.
	if err := os.Rename(logFile, logFile+".1"); err != nil {
		t.Fatalf("rotate rename: %v", err)
	}
	if _, err := s.ReopenContainerLog(ctx, &runtimeapi.ReopenContainerLogRequest{ContainerId: id}); err != nil {
		t.Fatalf("ReopenContainerLog: %v", err)
	}
	if _, err := pw.Write([]byte("after rotate\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitForFileContains(t, logFile, "after rotate")

	// The pre-rotation line must be in the rotated-away file, not the new one.
	if data, _ := os.ReadFile(logFile); strings.Contains(string(data), "before rotate") {
		t.Error("new log file unexpectedly contains pre-rotation output")
	}
	_ = pw.Close()
	s.stopLogPump(id)
}

func TestReopenContainerLogErrors(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)

	// Unknown container.
	_, err := s.ReopenContainerLog(context.Background(), &runtimeapi.ReopenContainerLogRequest{ContainerId: "nope"})
	wantCode(t, err, codes.NotFound)

	// Known container with no active pump (never started with a log path).
	id := startedContainer(t, s, sandboxID)
	_, err = s.ReopenContainerLog(context.Background(), &runtimeapi.ReopenContainerLogRequest{ContainerId: id})
	wantCode(t, err, codes.FailedPrecondition)
}

// waitForFileContains polls a file until it contains want or the deadline passes.
func waitForFileContains(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q to contain %q", path, want)
}

func waitForPumpGone(t *testing.T, s *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.logMu.Lock()
		_, ok := s.logPumps[id]
		s.logMu.Unlock()
		if !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for log pump %q to exit", id)
}
