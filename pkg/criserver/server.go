// Package criserver implements an experimental Kubernetes CRI server for the
// MacVz CRI feasibility track (see docs/CRI_FEASIBILITY.md, CRI-P1/CRI-P2).
//
// CRI-P1 proved the CRI server process, gRPC wiring, and basic
// RuntimeService/ImageService handshake (Version/Status/empty lists). CRI-P2
// (this iteration) adds a state-only Pod sandbox lifecycle — RunPodSandbox,
// StopPodSandbox, RemovePodSandbox, PodSandboxStatus, ListPodSandbox — backed by
// the restart-tolerant store in pkg/criserver/store (see sandbox.go).
//
// It still does NOT run containers, pull images, or drive apple/container: a
// sandbox here is a metadata record with a lifecycle, not a micro-VM, and every
// unmodelled CRI method returns codes.Unimplemented rather than a fake success.
//
// This path is intentionally separate from the shipped Virtual Kubelet provider
// (cmd/macvz-kubelet) and is not the default MacVz runtime mode.
package criserver

import (
	"context"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"google.golang.org/grpc"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
)

// runtimeAPIVersion is the CRI API version this skeleton speaks. kubelet and
// crictl negotiate against "v1"; it is not the MacVz build version.
const runtimeAPIVersion = "v1"

// defaultRuntimeName identifies this runtime to CRI clients.
const defaultRuntimeName = "macvz"

// Options configures the CRI skeleton server. Zero values fall back to sane
// defaults so the server is usable with `New(Options{})`.
type Options struct {
	// RuntimeName is reported in VersionResponse.RuntimeName. Defaults to "macvz".
	RuntimeName string
	// RuntimeVersion is reported in VersionResponse.RuntimeVersion. Defaults to
	// "dev". Callers should pass the build version (internal/version.Version).
	RuntimeVersion string
	// Sandboxes is the state store backing the Pod sandbox lifecycle (#74). Nil
	// installs an in-memory, non-persistent store — fine for the default skeleton
	// and tests, but the long-running adapter passes a disk-backed store so the
	// kubelet's sandbox view survives an adapter restart.
	Sandboxes *store.Store
	// Now overrides the clock for sandbox CreatedAt timestamps in tests. Nil uses
	// time.Now.
	Now func() time.Time
}

// Server is the experimental MacVz CRI server. It implements the CRI
// RuntimeService and ImageService gRPC interfaces. The embedded Unimplemented
// servers provide forward-compatible defaults (codes.Unimplemented) for every
// method this skeleton does not override, so the binary keeps compiling and
// serving as the CRI API grows.
type Server struct {
	runtimeapi.UnimplementedRuntimeServiceServer
	runtimeapi.UnimplementedImageServiceServer

	runtimeName    string
	runtimeVersion string
	sandboxes      *store.Store
	now            func() time.Time
}

// New builds a CRI skeleton server with the given options.
func New(opts Options) *Server {
	name := opts.RuntimeName
	if name == "" {
		name = defaultRuntimeName
	}
	version := opts.RuntimeVersion
	if version == "" {
		version = "dev"
	}
	sandboxes := opts.Sandboxes
	if sandboxes == nil {
		// An in-memory store never errors (empty dir), so the panic-free New
		// signature is preserved.
		sandboxes, _, _ = store.New("")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Server{
		runtimeName:    name,
		runtimeVersion: version,
		sandboxes:      sandboxes,
		now:            now,
	}
}

// Register wires the server's RuntimeService and ImageService onto a gRPC
// server. Both services share the same Server value.
func (s *Server) Register(grpcServer *grpc.Server) {
	runtimeapi.RegisterRuntimeServiceServer(grpcServer, s)
	runtimeapi.RegisterImageServiceServer(grpcServer, s)
}

// Version returns the CRI runtime version handshake. crictl and kubelet call
// this first to confirm the socket speaks a compatible CRI API version.
func (s *Server) Version(_ context.Context, req *runtimeapi.VersionRequest) (*runtimeapi.VersionResponse, error) {
	klog.V(4).InfoS("CRI Version", "clientVersion", req.GetVersion())
	return &runtimeapi.VersionResponse{
		Version:           runtimeAPIVersion,
		RuntimeName:       s.runtimeName,
		RuntimeVersion:    s.runtimeVersion,
		RuntimeApiVersion: runtimeAPIVersion,
	}, nil
}

// Status reports runtime readiness conditions for `crictl info`. The skeleton
// reports RuntimeReady=true (the CRI server process is up) but NetworkReady=false
// with an explicit reason, because CNI/Pod networking is out of scope for CRI-P1.
// This is deliberately honest: nothing here would actually run a networked Pod.
func (s *Server) Status(_ context.Context, req *runtimeapi.StatusRequest) (*runtimeapi.StatusResponse, error) {
	klog.V(4).InfoS("CRI Status", "verbose", req.GetVerbose())
	status := &runtimeapi.RuntimeStatus{
		Conditions: []*runtimeapi.RuntimeCondition{
			{
				Type:    runtimeapi.RuntimeReady,
				Status:  true,
				Reason:  "MacVzCriSkeleton",
				Message: "experimental MacVz CRI skeleton is serving; no workloads are run in this phase",
			},
			{
				Type:    runtimeapi.NetworkReady,
				Status:  false,
				Reason:  "NetworkPluginNotConfigured",
				Message: "Pod networking/CNI is out of scope for the CRI-P1 skeleton",
			},
		},
	}
	resp := &runtimeapi.StatusResponse{Status: status}
	if req.GetVerbose() {
		resp.Info = map[string]string{
			"experimental": "true",
			"track":        "CRI-P1 skeleton (docs/CRI_FEASIBILITY.md)",
			"runtimeName":  s.runtimeName,
		}
	}
	return resp, nil
}

// ListContainers returns an empty list. The skeleton owns no containers.
func (s *Server) ListContainers(_ context.Context, _ *runtimeapi.ListContainersRequest) (*runtimeapi.ListContainersResponse, error) {
	return &runtimeapi.ListContainersResponse{Containers: nil}, nil
}

// ListImages returns an empty list. The skeleton manages no images.
func (s *Server) ListImages(_ context.Context, _ *runtimeapi.ListImagesRequest) (*runtimeapi.ListImagesResponse, error) {
	return &runtimeapi.ListImagesResponse{Images: nil}, nil
}

// ImageFsInfo returns no filesystem usage. The skeleton has no image store.
// An empty (non-error) response keeps `crictl info` and kubelet image GC probes
// from erroring while honestly reporting that nothing is tracked.
func (s *Server) ImageFsInfo(_ context.Context, _ *runtimeapi.ImageFsInfoRequest) (*runtimeapi.ImageFsInfoResponse, error) {
	return &runtimeapi.ImageFsInfoResponse{}, nil
}
