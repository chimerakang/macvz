package criserver

import (
	"context"
	"io"
	"net"
	"strconv"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/tools/remotecommand"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	streaming "k8s.io/kubelet/pkg/cri/streaming"
)

// SetStreamingServer wires the kubelet streaming URL server for the LinuxPod
// service. The callback runtime is LinuxPodService.StreamingRuntime.
func (s *LinuxPodService) SetStreamingServer(srv StreamingServer) { s.streamServer = srv }

// StreamingRuntime returns the streaming backend used by kubelet's CRI streaming
// server for LinuxPod-backed Pods.
func (s *LinuxPodService) StreamingRuntime() streaming.Runtime {
	return &linuxpodStreamingRuntime{s: s}
}

// Exec returns a kubelet streaming URL for `kubectl exec`. The current LinuxPod
// helper backs a synchronous in-VM exec primitive (#133), so the streaming callback
// runs the command to completion and writes the captured stdout/stderr to the
// client. Interactive stdin/TTY byte plumbing remains a future transport concern.
func (s *LinuxPodService) Exec(_ context.Context, req *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error) {
	if s.streamServer == nil {
		return nil, errStreamingNotConfigured("Exec")
	}
	if len(req.GetCmd()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Exec: command is required")
	}
	if _, err := s.linuxpodRunningContainer(req.GetContainerId(), "Exec"); err != nil {
		return nil, err
	}
	return s.streamServer.GetExec(req)
}

func (s *LinuxPodService) Attach(context.Context, *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error) {
	return nil, status.Error(codes.Unimplemented, "Attach: LinuxPod backend attach negotiation exists, but kubelet byte streams are not wired yet")
}

// PortForward returns a kubelet streaming URL. The callback dials the LinuxPod
// sandbox's host-reachable VM address, falling back to the Pod IP when needed.
func (s *LinuxPodService) PortForward(_ context.Context, req *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error) {
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

type linuxpodStreamingRuntime struct{ s *LinuxPodService }

var _ streaming.Runtime = (*linuxpodStreamingRuntime)(nil)

func (r *linuxpodStreamingRuntime) Exec(ctx context.Context, containerID string, cmd []string, in io.Reader, out, errOut io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	if len(cmd) == 0 {
		return status.Error(codes.InvalidArgument, "Exec: command is required")
	}
	if in != nil {
		// Drain stdin so a kubectl client that sent a body is not left blocked, but
		// do not claim interactive stdin support until the helper has a real stream.
		go func() { _, _ = io.Copy(io.Discard, in) }()
	}
	if resize != nil {
		go drainResize(ctx, resize)
	}
	c, err := r.s.linuxpodRunningContainer(containerID, "Exec")
	if err != nil {
		return err
	}
	if c.LinuxPod == nil {
		return status.Errorf(codes.Internal, "Exec: container %q has no LinuxPod mapping", c.ID)
	}
	res, err := r.s.backend.ExecSync(ctx, linuxpod.ExecRequest{
		PodID:       c.SandboxID,
		ContainerID: c.LinuxPod.BackendContainerID,
		Command:     cmd,
	})
	if err != nil {
		return linuxpodToCRIError("Exec", err)
	}
	if out != nil && len(res.Stdout) > 0 {
		if _, werr := out.Write(res.Stdout); werr != nil {
			return werr
		}
	}
	if !tty && errOut != nil && len(res.Stderr) > 0 {
		if _, werr := errOut.Write(res.Stderr); werr != nil {
			return werr
		}
	}
	if res.ExitCode != 0 {
		return execCodeError{code: res.ExitCode}
	}
	return nil
}

func (r *linuxpodStreamingRuntime) Attach(context.Context, string, io.Reader, io.WriteCloser, io.WriteCloser, bool, <-chan remotecommand.TerminalSize) error {
	return status.Error(codes.Unimplemented, "attach is not supported by the LinuxPod CRI streaming runtime yet")
}

func (r *linuxpodStreamingRuntime) PortForward(ctx context.Context, podSandboxID string, port int32, stream io.ReadWriteCloser) error {
	if port < 1 || port > 65535 {
		return status.Errorf(codes.InvalidArgument, "port-forward: port %d is out of range", port)
	}
	target, err := r.s.linuxpodPortForwardTarget(podSandboxID)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(target.host, strconv.Itoa(int(port)))
	dialer := net.Dialer{Timeout: portForwardDialTimeout}
	if err := setPortForwardDialerInterface(&dialer, target.iface); err != nil {
		klog.V(3).InfoS("CRI(LinuxPod) port-forward could not bind socket to interface",
			"sandbox", podSandboxID, "target", addr, "interface", target.iface, "err", err)
	}
	if local, err := portForwardLocalAddr(target.host, target.iface); err == nil && local != nil {
		dialer.LocalAddr = local
	} else if err != nil {
		klog.V(3).InfoS("CRI(LinuxPod) port-forward using kernel-selected source address",
			"sandbox", podSandboxID, "target", addr, "interface", target.iface, "err", err)
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		if fallbackErr := portForwardDialFallback(ctx, stream, target.host, int(port), err); fallbackErr == nil {
			return nil
		} else {
			return status.Errorf(codes.Unavailable, "port-forward: dial %s: %v; nc fallback: %v", addr, err, fallbackErr)
		}
	}
	klog.V(4).InfoS("CRI(LinuxPod) port-forward established", "sandbox", podSandboxID, "port", port, "target", addr)
	err = proxyStream(ctx, stream, conn)
	klog.V(4).InfoS("CRI(LinuxPod) port-forward closed", "sandbox", podSandboxID, "port", port)
	return err
}

func (s *LinuxPodService) linuxpodPortForwardTarget(sandboxID string) (portForwardEndpoint, error) {
	sb, ok := s.sandboxes.Get(sandboxID)
	if !ok {
		return portForwardEndpoint{}, status.Errorf(codes.NotFound, "port-forward: sandbox %q not found", sandboxID)
	}
	containers := s.containers.ListBySandbox(sandboxID)
	if len(containers) == 0 {
		return portForwardEndpoint{}, status.Errorf(codes.FailedPrecondition, "port-forward: sandbox %q has no container", sandboxID)
	}
	foundLive := false
	for i := range containers {
		c := containers[i]
		if c.State == store.ContainerExited {
			continue
		}
		foundLive = true
		if c.State != store.ContainerRunning {
			return portForwardEndpoint{}, status.Errorf(codes.FailedPrecondition,
				"port-forward: sandbox %q container %q is %s, expected Running", sandboxID, c.ID, c.State)
		}
	}
	if !foundLive {
		return portForwardEndpoint{}, status.Errorf(codes.FailedPrecondition, "port-forward: sandbox %q has no running container", sandboxID)
	}
	if sb.Network.VMIP != "" {
		return portForwardEndpoint{host: sb.Network.VMIP, iface: sb.Network.Interface}, nil
	}
	if sb.Network.PodIP != "" {
		return portForwardEndpoint{host: sb.Network.PodIP, iface: sb.Network.Interface}, nil
	}
	return portForwardEndpoint{}, status.Errorf(codes.Unavailable, "port-forward: sandbox %q has no VM or Pod IP yet", sandboxID)
}
