package criserver

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"

	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// fakeStreamingServer records the requests it is handed and returns canned URLs,
// so the CRI Exec/PortForward URL handoff can be asserted without an HTTP listener.
type fakeStreamingServer struct {
	execReq *runtimeapi.ExecRequest
	pfReq   *runtimeapi.PortForwardRequest
}

func (f *fakeStreamingServer) GetExec(req *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error) {
	f.execReq = req
	return &runtimeapi.ExecResponse{Url: "http://stream/exec/tok"}, nil
}

func (f *fakeStreamingServer) GetAttach(req *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error) {
	return &runtimeapi.AttachResponse{Url: "http://stream/attach/tok"}, nil
}

func (f *fakeStreamingServer) GetPortForward(req *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error) {
	f.pfReq = req
	return &runtimeapi.PortForwardResponse{Url: "http://stream/portforward/tok"}, nil
}

// startedContainer creates and starts one container in the server's sandbox,
// returning its CRI id. The fake runtime reports it Running with startIP, if set.
func startedContainer(t *testing.T, s *Server, sandboxID string) string {
	t.Helper()
	ctx := context.Background()
	cResp, err := s.CreateContainer(ctx, createReq(sandboxID, "app"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	id := cResp.GetContainerId()
	if _, err := s.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: id}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	return id
}

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("error code = %v, want %v (err=%v)", status.Code(err), code, err)
	}
}

func TestExecReturnsStreamingURL(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	fss := &fakeStreamingServer{}
	s.SetStreamingServer(fss)
	id := startedContainer(t, s, sandboxID)

	resp, err := s.Exec(context.Background(), &runtimeapi.ExecRequest{ContainerId: id, Cmd: []string{"ls"}, Stdout: true})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if resp.GetUrl() != "http://stream/exec/tok" {
		t.Errorf("Exec url = %q, want canned streaming url", resp.GetUrl())
	}
	if fss.execReq.GetContainerId() != id {
		t.Errorf("streaming server saw container %q, want %q", fss.execReq.GetContainerId(), id)
	}
}

func TestExecErrors(t *testing.T) {
	// No streaming server configured: FailedPrecondition, not a dead URL.
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	id := startedContainer(t, s, sandboxID)
	_, err := s.Exec(context.Background(), &runtimeapi.ExecRequest{ContainerId: id, Stdout: true})
	wantCode(t, err, codes.FailedPrecondition)

	// With a streaming server, an unknown container is NotFound.
	s.SetStreamingServer(&fakeStreamingServer{})
	_, err = s.Exec(context.Background(), &runtimeapi.ExecRequest{ContainerId: "nope", Stdout: true})
	wantCode(t, err, codes.NotFound)

	// A created-but-not-running container is FailedPrecondition. Use a fresh
	// sandbox, since CRI-P3 allows only one container per sandbox.
	rt2 := newFakeRuntime()
	s2, sb2 := newServerWithRuntime(t, rt2)
	s2.SetStreamingServer(&fakeStreamingServer{})
	cResp, err := s2.CreateContainer(context.Background(), createReq(sb2, "app"))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	_, err = s2.Exec(context.Background(), &runtimeapi.ExecRequest{ContainerId: cResp.GetContainerId(), Stdout: true})
	wantCode(t, err, codes.FailedPrecondition)
}

func TestAttachUnimplemented(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	s.SetStreamingServer(&fakeStreamingServer{})
	id := startedContainer(t, s, sandboxID)

	_, err := s.Attach(context.Background(), &runtimeapi.AttachRequest{ContainerId: id, Stdout: true})
	wantCode(t, err, codes.Unimplemented)
}

func TestPortForwardReturnsURL(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	fss := &fakeStreamingServer{}
	s.SetStreamingServer(fss)

	resp, err := s.PortForward(context.Background(), &runtimeapi.PortForwardRequest{PodSandboxId: sandboxID, Port: []int32{80}})
	if err != nil {
		t.Fatalf("PortForward: %v", err)
	}
	if resp.GetUrl() != "http://stream/portforward/tok" {
		t.Errorf("PortForward url = %q", resp.GetUrl())
	}
	if fss.pfReq.GetPodSandboxId() != sandboxID {
		t.Errorf("streaming server saw sandbox %q, want %q", fss.pfReq.GetPodSandboxId(), sandboxID)
	}

	// Unknown sandbox is NotFound; no streaming server is FailedPrecondition.
	_, err = s.PortForward(context.Background(), &runtimeapi.PortForwardRequest{PodSandboxId: "nope"})
	wantCode(t, err, codes.NotFound)
	s.streamServer = nil
	_, err = s.PortForward(context.Background(), &runtimeapi.PortForwardRequest{PodSandboxId: sandboxID})
	wantCode(t, err, codes.FailedPrecondition)
}

func TestExecSyncCapturesOutputAndExitCode(t *testing.T) {
	rt := newFakeRuntime()
	rt.startIP = "192.168.64.5"
	rt.execStdout = "hello\n"
	rt.execStderr = "warn\n"
	rt.execErr = &runtime.ExitError{Code: 7}
	s, sandboxID := newServerWithRuntime(t, rt)
	id := startedContainer(t, s, sandboxID)

	resp, err := s.ExecSync(context.Background(), &runtimeapi.ExecSyncRequest{ContainerId: id, Cmd: []string{"sh", "-c", "echo hi"}})
	if err != nil {
		t.Fatalf("ExecSync: %v", err)
	}
	if string(resp.GetStdout()) != "hello\n" || string(resp.GetStderr()) != "warn\n" {
		t.Errorf("ExecSync captured stdout=%q stderr=%q", resp.GetStdout(), resp.GetStderr())
	}
	if resp.GetExitCode() != 7 {
		t.Errorf("ExecSync exit code = %d, want 7", resp.GetExitCode())
	}
	if len(rt.execCalls) != 1 || rt.execCalls[0][0] != "sh" {
		t.Errorf("ExecSync passed cmd %v", rt.execCalls)
	}
}

func TestExecSyncErrors(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)

	// Empty command is rejected.
	id := startedContainer(t, s, sandboxID)
	_, err := s.ExecSync(context.Background(), &runtimeapi.ExecSyncRequest{ContainerId: id})
	wantCode(t, err, codes.InvalidArgument)

	// A run failure (not a clean exit) is an RPC error.
	rt.execErr = runtime.ErrNotRunning
	_, err = s.ExecSync(context.Background(), &runtimeapi.ExecSyncRequest{ContainerId: id, Cmd: []string{"true"}})
	if err == nil {
		t.Fatal("ExecSync: expected error for run failure")
	}
}

func TestPortForwardTargetResolvesVMIP(t *testing.T) {
	rt := newFakeRuntime()
	rt.startIP = "192.168.64.9"
	s, sandboxID := newServerWithRuntime(t, rt)
	startedContainer(t, s, sandboxID)

	target, err := s.portForwardTarget(context.Background(), sandboxID)
	if err != nil {
		t.Fatalf("portForwardTarget: %v", err)
	}
	if target.host != "192.168.64.9" {
		t.Errorf("portForwardTarget host = %q, want runtime-reported VM IP", target.host)
	}
}

func TestPortForwardTargetSkipsExitedRestartHistory(t *testing.T) {
	rt := newFakeRuntime()
	rt.startIP = "192.168.64.20"
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()

	first := startedContainer(t, s, sandboxID)
	if _, err := s.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: first}); err != nil {
		t.Fatalf("StopContainer(first): %v", err)
	}

	rt.startIP = "192.168.64.21"
	second := startedContainer(t, s, sandboxID)
	if second == first {
		t.Fatalf("restart returned same container id")
	}

	target, err := s.portForwardTarget(ctx, sandboxID)
	if err != nil {
		t.Fatalf("portForwardTarget: %v", err)
	}
	if target.host != "192.168.64.21" {
		t.Errorf("portForwardTarget host = %q, want running replacement VM IP", target.host)
	}
}

func TestPortForwardTargetIncludesAttachedInterface(t *testing.T) {
	rt := newFakeRuntime()
	rt.startIP = "192.168.65.20"
	pnet := newFakePodNet()
	pnet.attachIface = "bridge101"
	s, _, _ := newNetworkedServer(t, rt)
	s.podNet = pnet
	ctx := context.Background()
	sandboxID := runSandbox(t, s, "web", "default", "uid-web")
	startContainerInSandbox(t, s, sandboxID)

	target, err := s.portForwardTarget(ctx, sandboxID)
	if err != nil {
		t.Fatalf("portForwardTarget: %v", err)
	}
	if target.host != "192.168.65.20" || target.iface != "bridge101" {
		t.Errorf("portForwardTarget = {host:%q iface:%q}, want {host:192.168.65.20 iface:bridge101}", target.host, target.iface)
	}
}

func TestTCPAddrOnInterfaceSelectsSameSubnetIPv4(t *testing.T) {
	_, same, err := net.ParseCIDR("192.168.65.1/24")
	if err != nil {
		t.Fatalf("ParseCIDR same: %v", err)
	}
	same.IP = net.ParseIP("192.168.65.1")
	_, other, err := net.ParseCIDR("10.0.0.1/24")
	if err != nil {
		t.Fatalf("ParseCIDR other: %v", err)
	}
	other.IP = net.ParseIP("10.0.0.1")

	got := tcpAddrOnInterface(net.ParseIP("192.168.65.64"), []net.Addr{other, same})
	if got == nil || !got.IP.Equal(net.ParseIP("192.168.65.1")) {
		t.Fatalf("tcpAddrOnInterface = %v, want 192.168.65.1", got)
	}
}

func TestStreamingPortForwardProxiesBytes(t *testing.T) {
	rt := newFakeRuntime()
	// Stand up a local echo server and report its address as the VM IP so the
	// port-forward path dials a real, reachable endpoint.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 5)
		if _, err := conn.Read(buf); err == nil {
			_, _ = conn.Write(buf)
		}
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	rt.startIP = host
	s, sandboxID := newServerWithRuntime(t, rt)
	startedContainer(t, s, sandboxID)

	clientEnd, serverEnd := net.Pipe()
	sr := &streamingRuntime{s: s}
	done := make(chan error, 1)
	port, _ := strconv.Atoi(portStr)
	go func() { done <- sr.PortForward(context.Background(), sandboxID, int32(port), serverEnd) }()

	if _, err := clientEnd.Write([]byte("ping!")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(clientEnd, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping!" {
		t.Errorf("echo = %q, want ping!", buf)
	}
	_ = clientEnd.Close()
	<-done
}

func TestCapBufferTruncates(t *testing.T) {
	var b capBuffer
	big := make([]byte, execSyncOutputCap+100)
	n, _ := b.Write(big)
	if n != len(big) {
		t.Errorf("Write reported %d, want full %d", n, len(big))
	}
	if len(b.Bytes()) != execSyncOutputCap {
		t.Errorf("buffered %d bytes, want cap %d", len(b.Bytes()), execSyncOutputCap)
	}
	if !b.overflow {
		t.Error("expected overflow flag set")
	}
}
