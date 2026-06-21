package criserver

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
)

// This file implements the CRI-P6 container logging path (#78).
//
// CRI logging is file-based, not RPC-based: the runtime is responsible for
// writing each container's stdout/stderr to a log file, and kubelet reads that
// file directly to serve `kubectl logs` (current and follow). There is no
// GetContainerLogs RPC. So when a container starts, the adapter opens a follow
// stream over the apple/container workload and pumps every line into the CRI log
// file in the kubelet-expected format:
//
//	2006-01-02T15:04:05.000000000Z07:00 stdout F <message>\n
//
// Honest limitation: `container logs` merges the guest's stdout and stderr into
// one stream, so every line is tagged `stdout`. The `F` (full) tag is always used
// because the line scanner already reassembles complete lines. ReopenContainerLog
// lets kubelet's log rotation swap the file out from under a running pump.

// logPump copies one container's workload output into its CRI log file until the
// container stops or the adapter shuts down. reopen swaps the destination file
// when kubelet rotates the log.
type logPump struct {
	path   string
	cancel context.CancelFunc
	done   chan struct{}

	mu   sync.Mutex
	file *os.File
}

// startLogPump begins streaming a started container's output into its CRI log
// file. It is a no-op (returning nil) when the container has no log path or the
// sandbox no log directory, which is the case for crictl flows that do not request
// logging. A pump already running for the container is left in place. The pump
// runs on a background context so it outlives the StartContainer RPC; StopContainer
// and RemoveContainer stop it.
func (s *Server) startLogPump(c *store.Container, sb *store.Sandbox) {
	if c.LogPath == "" || sb.LogDirectory == "" {
		return
	}
	fullPath := filepath.Join(sb.LogDirectory, c.LogPath)

	s.logMu.Lock()
	defer s.logMu.Unlock()
	if _, ok := s.logPumps[c.ID]; ok {
		return
	}
	f, err := openLogFile(fullPath)
	if err != nil {
		// Logging is best-effort: a failure to open the log file must not fail the
		// container start, but it is surfaced so the operator can fix the path.
		klog.ErrorS(err, "CRI: failed to open container log file; logs will be unavailable",
			"containerID", c.ID, "path", fullPath)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	p := &logPump{path: fullPath, cancel: cancel, done: make(chan struct{}), file: f}
	s.logPumps[c.ID] = p
	go s.runLogPump(ctx, c.ID, c.WorkloadID, p)
}

// runLogPump streams the workload's combined output into the pump's current file,
// reformatting each line into the CRI log format. It exits when the follow stream
// ends (container stopped) or the context is cancelled, always closing the file
// and signalling done.
func (s *Server) runLogPump(ctx context.Context, containerID, workloadID string, p *logPump) {
	defer close(p.done)
	defer func() {
		p.mu.Lock()
		if p.file != nil {
			_ = p.file.Close()
			p.file = nil
		}
		p.mu.Unlock()
	}()

	rc, err := s.containerRuntime.Logs(ctx, workloadID, runtime.LogOptions{Follow: true})
	if err != nil {
		if ctx.Err() == nil {
			klog.ErrorS(err, "CRI: failed to open workload log stream", "containerID", containerID, "workloadID", workloadID)
		}
		return
	}
	defer rc.Close()

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := formatCRILogLine(s.now(), scanner.Text())
		p.mu.Lock()
		if p.file != nil {
			if _, werr := p.file.WriteString(line); werr != nil {
				klog.ErrorS(werr, "CRI: failed to write container log line", "containerID", containerID, "path", p.path)
			}
		}
		p.mu.Unlock()
	}
	if serr := scanner.Err(); serr != nil && ctx.Err() == nil {
		klog.V(4).InfoS("CRI: container log stream ended with error", "containerID", containerID, "err", serr)
	}
}

// stopLogPump stops and reaps a container's log pump if one is running. It is a
// no-op when no pump exists, so the stop/remove paths stay idempotent.
func (s *Server) stopLogPump(containerID string) {
	s.logMu.Lock()
	p, ok := s.logPumps[containerID]
	if ok {
		delete(s.logPumps, containerID)
	}
	s.logMu.Unlock()
	if !ok {
		return
	}
	p.cancel()
	<-p.done
}

// ReopenContainerLog reopens a container's log file so kubelet's log rotation can
// swap the underlying file. The running pump redirects its writes to the freshly
// opened file. It errors NotFound for an unknown container and FailedPrecondition
// when the container has no active log pump (e.g. it never started or has exited).
func (s *Server) ReopenContainerLog(_ context.Context, req *runtimeapi.ReopenContainerLogRequest) (*runtimeapi.ReopenContainerLogResponse, error) {
	id := req.GetContainerId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "ReopenContainerLog: container id is required")
	}
	if _, ok := s.containers.Get(id); !ok {
		return nil, status.Errorf(codes.NotFound, "ReopenContainerLog: container %q not found", id)
	}

	s.logMu.Lock()
	p, ok := s.logPumps[id]
	s.logMu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition,
			"ReopenContainerLog: container %q has no active log stream", id)
	}

	f, err := openLogFile(p.path)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ReopenContainerLog: open %s: %v", p.path, err)
	}
	p.mu.Lock()
	old := p.file
	p.file = f
	p.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	klog.V(4).InfoS("CRI ReopenContainerLog", "containerID", id, "path", p.path)
	return &runtimeapi.ReopenContainerLogResponse{}, nil
}

// openLogFile opens (creating and creating parent dirs as needed) a CRI log file
// for append. kubelet sets the log directory; the parent dirs may not exist yet
// when the first container in a sandbox starts.
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// formatCRILogLine renders one line of guest output in the CRI log file format
// kubelet parses. The stream is always tagged stdout (the apple/container log
// stream merges stdout and stderr) and the tag is always F (the scanner emits
// complete lines).
func formatCRILogLine(ts time.Time, msg string) string {
	return fmt.Sprintf("%s stdout F %s\n", ts.UTC().Format(time.RFC3339Nano), msg)
}
