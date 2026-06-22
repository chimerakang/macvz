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
	if cfg == nil {
		return nil, status.Error(codes.InvalidArgument, "RunPodSandbox: config and metadata are required")
	}
	md := cfg.GetMetadata()
	if md == nil {
		return nil, status.Error(codes.InvalidArgument, "RunPodSandbox: config and metadata are required")
	}
	if md.GetName() == "" || md.GetNamespace() == "" || md.GetUid() == "" {
		return nil, status.Error(codes.InvalidArgument, "RunPodSandbox: metadata name, namespace, and uid are required")
	}
	// Reject Pod shapes the isolated micro-VM model cannot honor (CRI-P8, #80)
	// before reserving any state, so the operator gets one clear diagnostic rather
	// than a Pod that boots while silently ignoring its host-sharing request.
	if reason, bad := unsupportedSandboxShape(cfg); bad {
		return nil, status.Errorf(codes.InvalidArgument,
			"RunPodSandbox: Pod %s/%s uses a shape the experimental CRI adapter cannot honor: %s; %s",
			md.GetNamespace(), md.GetName(), reason, hostNamespaceSchedulingHint())
	}

	id, err := store.NewID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "RunPodSandbox: %v", err)
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	// Reserve the Pod IP now (CRI-P5, #77). The host-only VM address — and thus the
	// binat attach — only exists once the container starts, so RunPodSandbox only
	// reserves the address; the network is reported ready (PodSandboxStatus.Network)
	// after the attach on the container start path. When IPAM is disabled this is a
	// no-op and the sandbox runs without a Pod IP, exactly as before CRI-P5.
	podKey := md.GetNamespace() + "/" + md.GetName()
	if existing, ok := s.sandboxByKey(podKey); ok {
		if existing.Metadata.UID == md.GetUid() && existing.Metadata.Attempt == md.GetAttempt() && existing.State == store.StateReady {
			klog.V(4).InfoS("CRI RunPodSandbox returning existing sandbox for idempotent retry",
				"id", existing.ID, "namespace", md.GetNamespace(), "name", md.GetName(), "uid", md.GetUid())
			return &runtimeapi.RunPodSandboxResponse{PodSandboxId: existing.ID}, nil
		}
		return nil, status.Errorf(codes.FailedPrecondition,
			"RunPodSandbox: Pod %q already has sandbox %q; CRI-P5 supports one sandbox per Pod key",
			podKey, existing.ID)
	}
	podIP, hadIP, err := s.allocateSandboxIP(podKey)
	if err != nil {
		return nil, err
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
	sb.Network.PodIP = podIP

	if err := s.sandboxes.Put(sb); err != nil {
		// The record did not persist, so this sandbox owns nothing. Release a
		// freshly-allocated IP so it does not leak; keep a pre-existing reservation
		// (from restart recovery or a prior sandbox) intact.
		if podIP != "" && !hadIP {
			s.ipam.Release(podKey)
		}
		return nil, status.Errorf(codes.Internal, "RunPodSandbox: persist: %v", err)
	}
	klog.V(4).InfoS("CRI RunPodSandbox", "id", id,
		"namespace", md.GetNamespace(), "name", md.GetName(), "uid", md.GetUid(), "podIP", podIP)
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
	// Reclaim the Pod network path once the container is stopped (CRI-P5, #77). The
	// Pod IP reservation is retained until RemovePodSandbox so a stop/start cycle
	// keeps the same address. Detach is idempotent.
	if err := s.detachSandboxNetwork(ctx, req.GetPodSandboxId()); err != nil {
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
	// Tear down the network path (in case StopPodSandbox was skipped) and release
	// the Pod IP before deleting the record (CRI-P5, #77), so removal leaks neither
	// host pf state nor an address. Both steps are idempotent.
	if err := s.detachSandboxNetwork(ctx, req.GetPodSandboxId()); err != nil {
		return nil, err
	}
	if sb, ok := s.sandboxes.Get(req.GetPodSandboxId()); ok {
		s.releaseSandboxIP(&sb)
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
	st := &runtimeapi.PodSandboxStatus{
		Id:             sb.ID,
		Metadata:       toCRIMetadata(sb),
		State:          toCRIState(sb.State),
		CreatedAt:      sb.CreatedAt,
		Labels:         sb.Labels,
		Annotations:    sb.Annotations,
		RuntimeHandler: sb.RuntimeHandler,
	}
	// Report the Pod IP only once the network path is actually attached (CRI-P5,
	// #77). A reserved-but-not-attached IP (sandbox started, container not yet up)
	// is deliberately withheld so the status never claims reachability the host
	// cannot yet deliver.
	if sb.Network.Attached && sb.Network.PodIP != "" {
		st.Network = &runtimeapi.PodSandboxNetworkStatus{Ip: sb.Network.PodIP}
	}
	return st
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
