package criserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/tools/remotecommand"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	streaming "k8s.io/kubelet/pkg/cri/streaming"
	utilexec "k8s.io/utils/exec"
)

// This file implements the CRI-P6 streaming surfaces (#78): Exec, ExecSync,
// Attach, and PortForward.
//
// CRI splits streaming into two halves. The RuntimeService RPCs (Exec, Attach,
// PortForward) do not carry the bytes themselves; they hand the client a URL on a
// separate streaming HTTP server, and kubelet redirects `kubectl exec`/
// `port-forward` to it. That streaming server is the k8s.io/kubelet streaming
// library, configured with a Runtime backend that actually wires the guest's
// streams — implemented here by streamingRuntime over the apple/container driver.
//
// Honesty boundaries:
//   - Exec and PortForward are real: exec runs `container exec` with the client's
//     stdin/stdout/stderr and TTY; port-forward dials the micro-VM directly (the
//     kubelet shares the host with the guest) and proxies bytes both ways.
//   - Attach is not supported. apple/container exposes no honest way to attach to
//     a running container's primary process streams after start, so Attach returns
//     a documented Unimplemented rather than faking a stream that carries nothing.
//   - When no streaming server is wired (the default skeleton, or tests that do not
//     need it), Exec/PortForward return FailedPrecondition instead of a dead URL.

// portForwardDialTimeout bounds the connection attempt to the micro-VM.
const portForwardDialTimeout = 10 * time.Second

// StreamingServer is the subset of k8s.io/kubelet/pkg/cri/streaming.Server the CRI
// RuntimeService uses to mint per-request streaming URLs. The real streaming
// server satisfies it; tests inject a fake so the URL handoff can be asserted
// without standing up an HTTP listener.
type StreamingServer interface {
	GetExec(*runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error)
	GetAttach(*runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error)
	GetPortForward(*runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error)
}

// SetStreamingServer wires the streaming server that backs Exec/PortForward URL
// handoff (CRI-P6, #78). It is set once after New, because the streaming server is
// itself constructed with this Server's StreamingRuntime as its backend — the two
// reference each other, so the cycle is broken by setting the server afterwards.
func (s *Server) SetStreamingServer(srv StreamingServer) { s.streamServer = srv }

// StreamingRuntime returns the streaming.Runtime backend the kubelet streaming
// library drives to carry exec/attach/port-forward bytes. Pass it to
// streaming.NewServer and feed the result back via SetStreamingServer.
func (s *Server) StreamingRuntime() streaming.Runtime { return &streamingRuntime{s: s} }

// Exec returns a streaming URL for `kubectl exec`. The container must exist and be
// running; the actual command runs when kubelet connects to the URL and the
// streaming server calls back into streamingRuntime.Exec.
func (s *Server) Exec(_ context.Context, req *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error) {
	if s.streamServer == nil {
		return nil, errStreamingNotConfigured("Exec")
	}
	if _, err := s.runningContainer(req.GetContainerId(), "Exec"); err != nil {
		return nil, err
	}
	return s.streamServer.GetExec(req)
}

// Attach is not supported: apple/container offers no honest way to reattach to a
// started container's primary process streams, so faking a URL would hand kubectl
// a stream that carries nothing. It returns Unimplemented with that reason.
func (s *Server) Attach(_ context.Context, req *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error) {
	return nil, status.Errorf(codes.Unimplemented,
		"Attach: not supported by the MacVz apple/container runtime; a started micro-VM exposes no reattachable process stream. Use `kubectl exec` instead")
}

// PortForward returns a streaming URL for `kubectl port-forward`. The sandbox must
// exist; the connection to the Pod's micro-VM is dialed when kubelet connects to
// the URL and the streaming server calls back into streamingRuntime.PortForward.
func (s *Server) PortForward(_ context.Context, req *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error) {
	if s.streamServer == nil {
		return nil, errStreamingNotConfigured("PortForward")
	}
	if req.GetPodSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "PortForward: pod sandbox id is required")
	}
	if _, ok := s.sandboxes.Get(req.GetPodSandboxId()); !ok {
		return nil, status.Errorf(codes.NotFound, "PortForward: sandbox %q not found", req.GetPodSandboxId())
	}
	return s.streamServer.GetPortForward(req)
}

// ExecSync runs a command to completion inside a container and returns its
// captured stdout, stderr, and exit code. kubelet uses it for exec liveness and
// readiness probes, so it does not go through the streaming server; it runs the
// command synchronously and buffers the output.
func (s *Server) ExecSync(ctx context.Context, req *runtimeapi.ExecSyncRequest) (*runtimeapi.ExecSyncResponse, error) {
	if s.containerRuntime == nil {
		return nil, errRuntimeNotConfigured("ExecSync")
	}
	if len(req.GetCmd()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ExecSync: command is required")
	}
	c, err := s.runningContainer(req.GetContainerId(), "ExecSync")
	if err != nil {
		return nil, err
	}

	if t := req.GetTimeout(); t > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(t)*time.Second)
		defer cancel()
	}

	var stdout, stderr capBuffer
	execErr := s.containerRuntime.Exec(ctx, c.WorkloadID, req.GetCmd(), runtime.ExecIO{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	resp := &runtimeapi.ExecSyncResponse{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if execErr == nil {
		return resp, nil
	}
	// A clean non-zero exit is a successful ExecSync that reports the code; only a
	// failure to run the command at all is an RPC error.
	var exit *runtime.ExitError
	if errors.As(execErr, &exit) {
		resp.ExitCode = int32(exit.Code)
		return resp, nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, status.Errorf(codes.DeadlineExceeded, "ExecSync: command timed out after %ds", req.GetTimeout())
	}
	return nil, runtimeError("ExecSync", "run command", execErr)
}

// runningContainer resolves a container by CRI id and verifies it is Running, the
// precondition exec/attach share. It returns NotFound for an unknown id and
// FailedPrecondition for a container that is not running.
func (s *Server) runningContainer(id, method string) (store.Container, error) {
	if id == "" {
		return store.Container{}, status.Errorf(codes.InvalidArgument, "%s: container id is required", method)
	}
	c, ok := s.containers.Get(id)
	if !ok {
		return store.Container{}, status.Errorf(codes.NotFound, "%s: container %q not found", method, id)
	}
	if c.State != store.ContainerRunning {
		return store.Container{}, status.Errorf(codes.FailedPrecondition,
			"%s: container %q is %s, expected Running", method, id, c.State)
	}
	return c, nil
}

// errStreamingNotConfigured is returned by Exec/PortForward when no streaming
// server is wired, so the surface fails with a clear precondition rather than
// handing the client a URL that resolves nowhere.
func errStreamingNotConfigured(method string) error {
	return status.Errorf(codes.FailedPrecondition,
		"%s: no streaming server is configured (experimental adapter started without --streaming-addr)", method)
}

// streamingRuntime adapts this Server to the kubelet streaming library's Runtime
// interface, mapping the CRI container id to the apple/container workload id and
// the sandbox id to its Pod micro-VM endpoint.
type streamingRuntime struct{ s *Server }

var _ streaming.Runtime = (*streamingRuntime)(nil)

// Exec runs a command in the container's micro-VM, wiring the client streams. A
// clean non-zero exit is surfaced as a utilexec.ExitError so kubectl prints
// "command terminated with exit code N" instead of a generic failure.
func (r *streamingRuntime) Exec(ctx context.Context, containerID string, cmd []string, in io.Reader, out, errOut io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	c, err := r.s.runningContainer(containerID, "Exec")
	if err != nil {
		return err
	}
	// The runtime cannot resize the guest PTY; drain resize events so the streaming
	// server's producer never blocks.
	if resize != nil {
		go drainResize(ctx, resize)
	}
	execErr := r.s.containerRuntime.Exec(ctx, c.WorkloadID, cmd, runtime.ExecIO{
		Stdin:  in,
		Stdout: out,
		Stderr: errOut,
		TTY:    tty,
	})
	if execErr == nil {
		return nil
	}
	var exit *runtime.ExitError
	if errors.As(execErr, &exit) {
		return execCodeError{code: exit.Code}
	}
	return execErr
}

// Attach is unreachable in practice: the Attach RPC returns Unimplemented before a
// URL is ever minted. It is implemented for interface completeness and returns the
// same documented error.
func (r *streamingRuntime) Attach(_ context.Context, _ string, _ io.Reader, _, _ io.WriteCloser, _ bool, _ <-chan remotecommand.TerminalSize) error {
	return status.Error(codes.Unimplemented, "attach is not supported by the MacVz apple/container runtime")
}

// PortForward dials a port inside the sandbox's Pod micro-VM and proxies bytes
// both ways with the kubelet stream until either side closes or ctx is cancelled.
func (r *streamingRuntime) PortForward(ctx context.Context, podSandboxID string, port int32, stream io.ReadWriteCloser) error {
	if port < 1 || port > 65535 {
		return status.Errorf(codes.InvalidArgument, "port-forward: port %d is out of range", port)
	}
	host, err := r.s.portForwardTarget(ctx, podSandboxID)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(host, strconv.Itoa(int(port)))
	dialer := net.Dialer{Timeout: portForwardDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return status.Errorf(codes.Unavailable, "port-forward: dial %s: %v", addr, err)
	}
	klog.V(4).InfoS("CRI port-forward established", "sandbox", podSandboxID, "port", port, "target", addr)
	err = proxyStream(ctx, stream, conn)
	klog.V(4).InfoS("CRI port-forward closed", "sandbox", podSandboxID, "port", port)
	return err
}

// portForwardTarget resolves the host:addr to dial for a sandbox's Pod: the live
// runtime-reported micro-VM address of its single container, falling back to the
// VM IP observed at network-attach time. The kubelet runs on the same Mac as the
// guest, so the host-only address is always reachable.
func (s *Server) portForwardTarget(ctx context.Context, sandboxID string) (string, error) {
	sb, ok := s.sandboxes.Get(sandboxID)
	if !ok {
		return "", status.Errorf(codes.NotFound, "port-forward: sandbox %q not found", sandboxID)
	}
	containers := s.containers.ListBySandbox(sandboxID)
	if len(containers) == 0 {
		return "", status.Errorf(codes.FailedPrecondition, "port-forward: sandbox %q has no container", sandboxID)
	}
	foundLive := false
	for i := range containers {
		c := containers[i]
		if c.State == store.ContainerExited {
			continue
		}
		foundLive = true
		if c.State != store.ContainerRunning {
			return "", status.Errorf(codes.FailedPrecondition,
				"port-forward: sandbox %q container %q is %s, expected Running", sandboxID, c.ID, c.State)
		}
		if st, err := s.containerRuntime.Status(ctx, c.WorkloadID); err == nil {
			if st.Phase != runtime.PhaseRunning {
				return "", status.Errorf(codes.FailedPrecondition,
					"port-forward: sandbox %q container %q is not running (%s)", sandboxID, c.ID, st.Phase)
			}
			if st.IP != "" {
				return st.IP, nil
			}
		}
		if sb.Network.VMIP != "" {
			return sb.Network.VMIP, nil
		}
	}
	if !foundLive {
		return "", status.Errorf(codes.FailedPrecondition, "port-forward: sandbox %q has no running container", sandboxID)
	}
	if sb.Network.VMIP != "" {
		return sb.Network.VMIP, nil
	}
	return "", status.Errorf(codes.Unavailable, "port-forward: sandbox %q micro-VM has no address yet", sandboxID)
}

// proxyStream copies bytes both ways between the port-forward stream and the
// micro-VM connection. It returns when either direction ends or ctx is cancelled,
// then closes both endpoints and waits for both copies so nothing is leaked.
func proxyStream(ctx context.Context, stream io.ReadWriteCloser, conn net.Conn) error {
	done := make(chan error, 2)
	cp := func(dst io.Writer, src io.Reader) { _, err := io.Copy(dst, src); done <- err }
	go cp(conn, stream)
	go cp(stream, conn)

	var first error
	select {
	case <-ctx.Done():
		first = ctx.Err()
	case first = <-done:
	}
	_ = conn.Close()
	_ = stream.Close()
	<-done

	if first == io.EOF {
		return nil
	}
	return first
}

// drainResize discards terminal resize events until the channel closes or ctx is
// done, so the streaming server never blocks producing them for a runtime that
// cannot resize the guest PTY.
func drainResize(ctx context.Context, resize <-chan remotecommand.TerminalSize) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-resize:
			if !ok {
				return
			}
		}
	}
}

// execCodeError adapts a command exit code to utilexec.ExitError, the interface
// the streaming remotecommand server inspects to report a non-zero exit status.
type execCodeError struct{ code int }

var _ utilexec.ExitError = execCodeError{}

func (e execCodeError) Error() string {
	return fmt.Sprintf("command terminated with exit code %d", e.code)
}
func (e execCodeError) String() string  { return e.Error() }
func (e execCodeError) Exited() bool    { return true }
func (e execCodeError) ExitStatus() int { return e.code }

// capBuffer is a bounded in-memory buffer for ExecSync output. Probe commands
// produce little output, but a runaway command must not exhaust adapter memory, so
// writes past the cap are discarded while the command is still allowed to finish.
type capBuffer struct {
	buf      []byte
	overflow bool
}

// execSyncOutputCap bounds each of ExecSync's stdout/stderr buffers (16 MiB),
// matching the order of magnitude other CRI runtimes allow for sync exec output.
const execSyncOutputCap = 16 << 20

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := execSyncOutputCap - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:room]...)
			b.overflow = true
		}
	} else if len(p) > 0 {
		b.overflow = true
	}
	// Always report a full write so Exec keeps draining the command's output.
	return len(p), nil
}

func (b *capBuffer) Bytes() []byte { return b.buf }
