package criserver

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestNewDefaults(t *testing.T) {
	s := New(Options{})
	if s.runtimeName != defaultRuntimeName {
		t.Errorf("runtimeName = %q, want %q", s.runtimeName, defaultRuntimeName)
	}
	if s.runtimeVersion != "dev" {
		t.Errorf("runtimeVersion = %q, want %q", s.runtimeVersion, "dev")
	}
}

func TestVersion(t *testing.T) {
	s := New(Options{RuntimeVersion: "1.2.3"})
	resp, err := s.Version(context.Background(), &runtimeapi.VersionRequest{Version: "v1"})
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if resp.GetRuntimeApiVersion() != runtimeAPIVersion {
		t.Errorf("RuntimeApiVersion = %q, want %q", resp.GetRuntimeApiVersion(), runtimeAPIVersion)
	}
	if resp.GetRuntimeName() != defaultRuntimeName {
		t.Errorf("RuntimeName = %q, want %q", resp.GetRuntimeName(), defaultRuntimeName)
	}
	if resp.GetRuntimeVersion() != "1.2.3" {
		t.Errorf("RuntimeVersion = %q, want %q", resp.GetRuntimeVersion(), "1.2.3")
	}
}

func TestStatusConditions(t *testing.T) {
	s := New(Options{})
	resp, err := s.Status(context.Background(), &runtimeapi.StatusRequest{Verbose: true})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	conds := map[string]bool{}
	for _, c := range resp.GetStatus().GetConditions() {
		conds[c.GetType()] = c.GetStatus()
	}
	if ready, ok := conds[runtimeapi.RuntimeReady]; !ok || !ready {
		t.Errorf("RuntimeReady = %v (present=%v), want true", ready, ok)
	}
	if netReady, ok := conds[runtimeapi.NetworkReady]; !ok || netReady {
		t.Errorf("NetworkReady = %v (present=%v), want false", netReady, ok)
	}
	if resp.GetInfo()["experimental"] != "true" {
		t.Errorf("verbose Info missing experimental marker: %v", resp.GetInfo())
	}
}

func TestStatusNonVerboseHasNoInfo(t *testing.T) {
	s := New(Options{})
	resp, err := s.Status(context.Background(), &runtimeapi.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(resp.GetInfo()) != 0 {
		t.Errorf("non-verbose Status returned Info: %v", resp.GetInfo())
	}
}

func TestEmptyLists(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()

	if r, err := s.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{}); err != nil || len(r.GetItems()) != 0 {
		t.Errorf("ListPodSandbox = (%v, %v), want empty", r, err)
	}
	if r, err := s.ListContainers(ctx, &runtimeapi.ListContainersRequest{}); err != nil || len(r.GetContainers()) != 0 {
		t.Errorf("ListContainers = (%v, %v), want empty", r, err)
	}
	if r, err := s.ListImages(ctx, &runtimeapi.ListImagesRequest{}); err != nil || len(r.GetImages()) != 0 {
		t.Errorf("ListImages = (%v, %v), want empty", r, err)
	}
	if _, err := s.ImageFsInfo(ctx, &runtimeapi.ImageFsInfoRequest{}); err != nil {
		t.Errorf("ImageFsInfo: %v", err)
	}
}

// TestUnimplementedMethodReturnsCode verifies the embedded Unimplemented servers
// answer un-overridden methods with codes.Unimplemented rather than panicking.
// UpdateContainerResources is out of scope for this phase, so it must still
// report Unimplemented — the spike never fakes success for a capability it lacks.
func TestUnimplementedMethodReturnsCode(t *testing.T) {
	s := New(Options{})
	_, err := s.UpdateContainerResources(context.Background(), &runtimeapi.UpdateContainerResourcesRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("UpdateContainerResources code = %v, want Unimplemented", status.Code(err))
	}
}

// TestContainerMethodsRequireRuntime verifies the container surface is honest
// without a backend: CreateContainer reports FailedPrecondition (the method is
// implemented but cannot act) rather than Unimplemented or a fake success.
func TestContainerMethodsRequireRuntime(t *testing.T) {
	s := New(Options{})
	_, err := s.CreateContainer(context.Background(), &runtimeapi.CreateContainerRequest{PodSandboxId: "x"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CreateContainer without runtime: code = %v, want FailedPrecondition", status.Code(err))
	}
}

// TestServeOverSocket exercises the full gRPC path: register the server on a
// real Unix socket and drive Version/Status through a CRI client, the same way
// crictl/kubelet connect.
func TestServeOverSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "macvz-cri.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	New(Options{RuntimeVersion: "test"}).Register(grpcServer)
	go func() { _ = grpcServer.Serve(lis) }()
	defer grpcServer.Stop()

	conn, err := grpc.NewClient("unix://"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rtClient := runtimeapi.NewRuntimeServiceClient(conn)
	vResp, err := rtClient.Version(ctx, &runtimeapi.VersionRequest{Version: "v1"})
	if err != nil {
		t.Fatalf("client Version: %v", err)
	}
	if vResp.GetRuntimeApiVersion() != runtimeAPIVersion {
		t.Errorf("RuntimeApiVersion = %q, want %q", vResp.GetRuntimeApiVersion(), runtimeAPIVersion)
	}

	sResp, err := rtClient.Status(ctx, &runtimeapi.StatusRequest{})
	if err != nil {
		t.Fatalf("client Status: %v", err)
	}
	if len(sResp.GetStatus().GetConditions()) == 0 {
		t.Error("Status returned no conditions")
	}

	imgClient := runtimeapi.NewImageServiceClient(conn)
	if _, err := imgClient.ListImages(ctx, &runtimeapi.ListImagesRequest{}); err != nil {
		t.Fatalf("client ListImages: %v", err)
	}
}
