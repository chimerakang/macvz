package criserver

import (
	"context"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
)

// This file implements the CRI-P2 state-only Pod sandbox lifecycle (#74).
//
// A sandbox here is a metadata record with a lifecycle — nothing more. It owns
// no micro-VM, no image, and no Pod network. This is intentional: the spike
// validates whether MacVz can honour the kubelet/crictl sandbox lifecycle and
// status contract before any data-plane work is attempted, and it must never
// hide a missing capability behind a fake success. Container creation, CNI
// ADD/DEL, and host networking remain Unimplemented (via the embedded server)
// or out of scope for this phase.

// RunPodSandbox creates a state-only sandbox in the Ready state and returns its
// ID. No image is pulled and no micro-VM is booted; only the Pod sandbox
// identity and lifecycle are modelled. The record captures enough CRI metadata
// to map the sandbox ID back to its Kubernetes Pod namespace/name/UID.
func (s *Server) RunPodSandbox(_ context.Context, req *runtimeapi.RunPodSandboxRequest) (*runtimeapi.RunPodSandboxResponse, error) {
	cfg := req.GetConfig()
	md := cfg.GetMetadata()
	if cfg == nil || md == nil {
		return nil, status.Error(codes.InvalidArgument, "RunPodSandbox: config and metadata are required")
	}
	if md.GetName() == "" || md.GetNamespace() == "" || md.GetUid() == "" {
		return nil, status.Error(codes.InvalidArgument, "RunPodSandbox: metadata name, namespace, and uid are required")
	}

	id, err := store.NewID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "RunPodSandbox: %v", err)
	}

	sb := &store.Sandbox{
		ID:             id,
		State:          store.StateReady,
		CreatedAt:      s.now().UnixNano(),
		Hostname:       cfg.GetHostname(),
		LogDirectory:   cfg.GetLogDirectory(),
		RuntimeHandler: req.GetRuntimeHandler(),
		Labels:         cfg.GetLabels(),
		Annotations:    cfg.GetAnnotations(),
	}
	sb.Metadata.Name = md.GetName()
	sb.Metadata.UID = md.GetUid()
	sb.Metadata.Namespace = md.GetNamespace()
	sb.Metadata.Attempt = md.GetAttempt()
	if dns := cfg.GetDnsConfig(); dns != nil {
		sb.DNS.Servers = dns.GetServers()
		sb.DNS.Searches = dns.GetSearches()
		sb.DNS.Options = dns.GetOptions()
	}

	if err := s.sandboxes.Put(sb); err != nil {
		return nil, status.Errorf(codes.Internal, "RunPodSandbox: persist: %v", err)
	}
	klog.V(4).InfoS("CRI RunPodSandbox", "id", id,
		"namespace", md.GetNamespace(), "name", md.GetName(), "uid", md.GetUid())
	return &runtimeapi.RunPodSandboxResponse{PodSandboxId: id}, nil
}

// StopPodSandbox stops any containers owned by the sandbox, then transitions the
// sandbox to NotReady. It is idempotent: stopping an already-stopped or absent
// sandbox succeeds. kubelet relies on this idempotency — it may call
// StopPodSandbox many times before RemovePodSandbox.
func (s *Server) StopPodSandbox(ctx context.Context, req *runtimeapi.StopPodSandboxRequest) (*runtimeapi.StopPodSandboxResponse, error) {
	if req.GetPodSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "StopPodSandbox: pod sandbox id is required")
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if err := s.stopSandboxContainers(ctx, req.GetPodSandboxId(), "StopPodSandbox"); err != nil {
		return nil, err
	}
	if _, err := s.sandboxes.SetState(req.GetPodSandboxId(), store.StateNotReady); err != nil {
		return nil, status.Errorf(codes.Internal, "StopPodSandbox: %v", err)
	}
	return &runtimeapi.StopPodSandboxResponse{}, nil
}

// RemovePodSandbox destroys and removes containers owned by the sandbox, then
// deletes the sandbox record. It is idempotent: removing an absent sandbox
// succeeds, matching the CRI contract.
func (s *Server) RemovePodSandbox(ctx context.Context, req *runtimeapi.RemovePodSandboxRequest) (*runtimeapi.RemovePodSandboxResponse, error) {
	if req.GetPodSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "RemovePodSandbox: pod sandbox id is required")
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if err := s.removeSandboxContainers(ctx, req.GetPodSandboxId(), "RemovePodSandbox"); err != nil {
		return nil, err
	}
	if err := s.sandboxes.Delete(req.GetPodSandboxId()); err != nil {
		return nil, status.Errorf(codes.Internal, "RemovePodSandbox: %v", err)
	}
	return &runtimeapi.RemovePodSandboxResponse{}, nil
}

// PodSandboxStatus returns the status of a sandbox, erroring with NotFound if it
// is absent — the CRI contract for this method. The Network field is left nil
// because the state-only model owns no Pod IP; reporting a fake address would
// hide the gap this spike exists to surface.
func (s *Server) PodSandboxStatus(_ context.Context, req *runtimeapi.PodSandboxStatusRequest) (*runtimeapi.PodSandboxStatusResponse, error) {
	if req.GetPodSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "PodSandboxStatus: pod sandbox id is required")
	}
	sb, ok := s.sandboxes.Get(req.GetPodSandboxId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "PodSandboxStatus: sandbox %q not found", req.GetPodSandboxId())
	}
	resp := &runtimeapi.PodSandboxStatusResponse{Status: toCRIStatus(&sb)}
	if req.GetVerbose() {
		resp.Info = map[string]string{
			"model":          "state-only-sandbox-spike",
			"runtimeHandler": sb.RuntimeHandler,
		}
	}
	return resp, nil
}

// ListPodSandbox returns sandboxes matching the optional filter (id, state, and
// label selector).
func (s *Server) ListPodSandbox(_ context.Context, req *runtimeapi.ListPodSandboxRequest) (*runtimeapi.ListPodSandboxResponse, error) {
	filter := req.GetFilter()
	var items []*runtimeapi.PodSandbox
	for _, sb := range s.sandboxes.List() {
		if !matchesFilter(&sb, filter) {
			continue
		}
		items = append(items, &runtimeapi.PodSandbox{
			Id:             sb.ID,
			Metadata:       toCRIMetadata(&sb),
			State:          toCRIState(sb.State),
			CreatedAt:      sb.CreatedAt,
			Labels:         sb.Labels,
			Annotations:    sb.Annotations,
			RuntimeHandler: sb.RuntimeHandler,
		})
	}
	return &runtimeapi.ListPodSandboxResponse{Items: items}, nil
}

func toCRIState(st store.State) runtimeapi.PodSandboxState {
	if st == store.StateReady {
		return runtimeapi.PodSandboxState_SANDBOX_READY
	}
	return runtimeapi.PodSandboxState_SANDBOX_NOTREADY
}

func toCRIMetadata(sb *store.Sandbox) *runtimeapi.PodSandboxMetadata {
	return &runtimeapi.PodSandboxMetadata{
		Name:      sb.Metadata.Name,
		Uid:       sb.Metadata.UID,
		Namespace: sb.Metadata.Namespace,
		Attempt:   sb.Metadata.Attempt,
	}
}

func toCRIStatus(sb *store.Sandbox) *runtimeapi.PodSandboxStatus {
	return &runtimeapi.PodSandboxStatus{
		Id:             sb.ID,
		Metadata:       toCRIMetadata(sb),
		State:          toCRIState(sb.State),
		CreatedAt:      sb.CreatedAt,
		Labels:         sb.Labels,
		Annotations:    sb.Annotations,
		RuntimeHandler: sb.RuntimeHandler,
		// Network is intentionally nil: the state-only model owns no Pod IP.
	}
}

// matchesFilter applies a CRI PodSandboxFilter (id, state, label selector). A nil
// filter matches everything.
func matchesFilter(sb *store.Sandbox, f *runtimeapi.PodSandboxFilter) bool {
	if f == nil {
		return true
	}
	if f.GetId() != "" && f.GetId() != sb.ID {
		return false
	}
	if sv := f.GetState(); sv != nil && sv.GetState() != toCRIState(sb.State) {
		return false
	}
	for k, v := range f.GetLabelSelector() {
		if sb.Labels[k] != v {
			return false
		}
	}
	return true
}
