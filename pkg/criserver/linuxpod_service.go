package criserver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
)

// linuxpod_service.go wires macvz-cri CRI serving onto the experimental LinuxPod
// backend (CRI-L2, #127), behind the --experimental-linuxpod-backend gate. It is
// a SEPARATE RuntimeService/ImageService implementation from the default
// apple/container Server: the gate registers one or the other, so the shipped
// apple/container path is untouched.
//
// Unlike the apple/container path (one micro-VM per single-container Pod), this
// maps a CRI sandbox to one LinuxPod VM that hosts multiple containers sharing a
// network namespace, and supports the kubelet ordering the phase requires:
// RunPodSandbox -> CreateContainer(app)/StartContainer(app) -> late
// CreateContainer(sidecar)/StartContainer(sidecar) after the app is Running, with
// StartContainer gated on rootfs identity verification (CRI-R16).
//
// Persistence reuses the sandbox/container stores so a restarted adapter can
// reconcile records against the live backend without trusting stale identity
// evidence (identity is a start invariant, #110/#117). It deliberately does not
// solve Pod networking (CRI-L3, #128) or logs/exec/stats (CRI-L4, #129); those
// return honest unsupported errors here and are tracked by their own issues.

// LinuxPodService serves the CRI RuntimeService and a minimal ImageService backed
// by a linuxpod.Backend. It is constructed only when the experimental gate is on.
type LinuxPodService struct {
	runtimeapi.UnimplementedRuntimeServiceServer
	runtimeapi.UnimplementedImageServiceServer

	backend    linuxpod.Backend
	sandboxes  *store.Store
	containers *store.ContainerStore
	version    string
	now        func() time.Time

	// podNet and ipam wire the MacVz Pod network path for LinuxPod sandboxes
	// (CRI-L3, #128). Both nil leaves Pod networking off: sandboxes run without a
	// Pod IP and NetworkReady is false. See linuxpod_network.go.
	podNet PodNetwork
	ipam   PodIPAllocator
	// addrPoll* bound how long ensureSandboxNetwork waits for the Pod VM's
	// host-reachable address before failing with an address-discovery diagnostic.
	// Tests shorten them.
	addrPollAttempts int
	addrPollInterval time.Duration

	mu            sync.Mutex
	statusProbeMu sync.Mutex
	mountPolicy   MountPolicy
	images        map[string]struct{} // images "pulled" through the minimal ImageService
	streamServer  StreamingServer
}

// LinuxPodOptions configures a LinuxPodService.
type LinuxPodOptions struct {
	Backend        linuxpod.Backend
	Sandboxes      *store.Store
	Containers     *store.ContainerStore
	RuntimeVersion string
	// PodNetwork and IPAM wire the MacVz Pod network path for LinuxPod sandboxes
	// (CRI-L3, #128). Both must be set to enable Pod networking; either nil leaves it
	// off and sandboxes run without a Pod IP. Satisfied by *podnet.Router and
	// *network.PodIPAM.
	PodNetwork PodNetwork
	IPAM       PodIPAllocator
	Mounts     MountPolicy
	// Now is injectable for deterministic tests; defaults to time.Now.
	Now func() time.Time
}

// NewLinuxPodService builds a LinuxPod-backed CRI service. Backend is required.
func NewLinuxPodService(opts LinuxPodOptions) (*LinuxPodService, error) {
	if opts.Backend == nil {
		return nil, errors.New("criserver: LinuxPod service requires a backend")
	}
	sandboxes := opts.Sandboxes
	if sandboxes == nil {
		sandboxes, _, _ = store.New("")
	}
	containers := opts.Containers
	if containers == nil {
		containers, _, _ = store.NewContainerStore("")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	v := opts.RuntimeVersion
	if v == "" {
		v = "0.0.0-experimental"
	}
	return &LinuxPodService{
		backend:          opts.Backend,
		sandboxes:        sandboxes,
		containers:       containers,
		version:          v,
		now:              now,
		podNet:           opts.PodNetwork,
		ipam:             opts.IPAM,
		mountPolicy:      opts.Mounts,
		addrPollAttempts: defaultVMIPPollAttempts,
		addrPollInterval: defaultVMIPPollInterval,
		images:           map[string]struct{}{},
	}, nil
}

// Register installs this service as the gRPC RuntimeService and ImageService,
// replacing the default apple/container Server for the gated process.
func (s *LinuxPodService) Register(g *grpc.Server) {
	runtimeapi.RegisterRuntimeServiceServer(g, s)
	runtimeapi.RegisterImageServiceServer(g, s)
}

const linuxpodRuntimeName = "macvz-cri-linuxpod"

// linuxpodUnknownImageSize is the smallest non-zero image size accepted by
// kubelet's ImageStatus validation. The real LinuxPod helper owns the image store
// today and does not expose authoritative image sizes yet; returning zero makes
// kubelet stop before CreateContainer with ImageInspectError. Keep this visibly
// non-authoritative until the helper grows a real image-status surface.
const linuxpodUnknownImageSize uint64 = 1

// Version reports the CRI runtime identity for the LinuxPod path.
func (s *LinuxPodService) Version(_ context.Context, req *runtimeapi.VersionRequest) (*runtimeapi.VersionResponse, error) {
	return &runtimeapi.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       linuxpodRuntimeName,
		RuntimeVersion:    s.version,
		RuntimeApiVersion: "v1",
	}, nil
}

// Status reports the runtime as ready; network readiness is per-sandbox and lives
// in PodSandboxStatus (Pod networking is CRI-L3, #128).
func (s *LinuxPodService) Status(_ context.Context, req *runtimeapi.StatusRequest) (*runtimeapi.StatusResponse, error) {
	// NetworkReady reflects whether the MacVz Pod network path (IPAM + pf binat) is
	// wired (CRI-L3, #128): true only when both are configured and thus usable. It is
	// deliberately honest — never set true for a path that cannot produce a reachable
	// Pod; per-sandbox readiness is reported in PodSandboxStatus.
	netReady := s.networkEnabled()
	netCond := &runtimeapi.RuntimeCondition{
		Type: runtimeapi.NetworkReady, Status: netReady,
		Reason:  "LinuxPodNetworkNotConfigured",
		Message: "Pod networking is not wired; LinuxPod sandboxes run without a Pod IP",
	}
	if netReady {
		netCond.Reason = "MacVzPodNetwork"
		netCond.Message = "MacVz Pod networking (IPAM + pf binat) is configured and usable for LinuxPod sandboxes"
	}
	conds := []*runtimeapi.RuntimeCondition{
		{Type: runtimeapi.RuntimeReady, Status: true},
		netCond,
	}
	return &runtimeapi.StatusResponse{Status: &runtimeapi.RuntimeStatus{Conditions: conds}}, nil
}

// RunPodSandbox creates a LinuxPod sandbox VM and persists its record. The CRI
// sandbox id is used as the backend pod id, so no extra mapping is stored.
func (s *LinuxPodService) RunPodSandbox(ctx context.Context, req *runtimeapi.RunPodSandboxRequest) (*runtimeapi.RunPodSandboxResponse, error) {
	cfg := req.GetConfig()
	md := cfg.GetMetadata()
	if cfg == nil || md == nil || md.GetName() == "" || md.GetNamespace() == "" || md.GetUid() == "" {
		return nil, status.Error(codes.InvalidArgument, "RunPodSandbox: config metadata name, namespace, and uid are required")
	}
	if reason, bad := unsupportedSandboxShape(cfg); bad {
		return nil, status.Errorf(codes.InvalidArgument,
			"RunPodSandbox: Pod %s/%s uses a shape the LinuxPod CRI adapter cannot honor: %s",
			md.GetNamespace(), md.GetName(), reason)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	podKey := md.GetNamespace() + "/" + md.GetName()
	for _, sb := range s.sandboxes.List() {
		if sb.Metadata.Namespace == md.GetNamespace() && sb.Metadata.Name == md.GetName() {
			if sb.Metadata.UID == md.GetUid() && sb.Metadata.Attempt == md.GetAttempt() && sb.State == store.StateReady {
				// Idempotent retry: re-affirm the Pod network attach so a sandbox whose
				// earlier attach failed (and returned an error) is completed on retry.
				if err := s.ensureSandboxNetwork(ctx, sb.ID); err != nil {
					return nil, err
				}
				return &runtimeapi.RunPodSandboxResponse{PodSandboxId: sb.ID}, nil
			}
			if sb.State == store.StateNotReady {
				if err := s.discardNotReadySandboxForRecreate(ctx, &sb); err != nil {
					return nil, err
				}
				continue
			}
			return nil, status.Errorf(codes.FailedPrecondition,
				"RunPodSandbox: Pod %q already has sandbox %q", podKey, sb.ID)
		}
	}

	id, err := store.NewID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "RunPodSandbox: %v", err)
	}
	podStatus, err := s.backend.CreatePod(ctx, linuxpod.PodSpec{ID: id, Hostname: cfg.GetHostname()})
	if err != nil {
		return nil, linuxpodToCRIError("RunPodSandbox", err)
	}

	sb := &store.Sandbox{
		ID:                id,
		State:             store.StateReady,
		CreatedAt:         s.now().UnixNano(),
		Hostname:          cfg.GetHostname(),
		LogDirectory:      cfg.GetLogDirectory(),
		RuntimeHandler:    req.GetRuntimeHandler(),
		Labels:            cfg.GetLabels(),
		Annotations:       cfg.GetAnnotations(),
		LinuxPodNamespace: podStatus.SandboxNamespace,
	}
	sb.Metadata.Name = md.GetName()
	sb.Metadata.UID = md.GetUid()
	sb.Metadata.Namespace = md.GetNamespace()
	sb.Metadata.Attempt = md.GetAttempt()
	if err := s.sandboxes.Put(sb); err != nil {
		// The record did not persist, so reclaim the just-created Pod VM.
		if _, cerr := s.backend.Cleanup(context.WithoutCancel(ctx), id); cerr != nil {
			klog.ErrorS(cerr, "RunPodSandbox: cleanup after persist failure", "podID", id)
		}
		return nil, status.Errorf(codes.Internal, "RunPodSandbox: persist: %v", err)
	}
	// Attach the Pod network path now that the Pod VM is up (CRI-L3, #128). On
	// failure the sandbox record is retained so a kubelet RunPodSandbox retry hits
	// the idempotent branch above and re-attempts the attach; the classified error
	// tells kubelet (and an operator) exactly which stage failed.
	if err := s.ensureSandboxNetwork(ctx, id); err != nil {
		return nil, err
	}
	klog.V(4).InfoS("CRI(LinuxPod) RunPodSandbox", "id", id, "namespace", md.GetNamespace(),
		"name", md.GetName(), "namespace_ns", podStatus.SandboxNamespace)
	return &runtimeapi.RunPodSandboxResponse{PodSandboxId: id}, nil
}

// CreateContainer prepares a rootfs and late-binds a container in the sandbox's
// LinuxPod VM. It does not start it. A container can be created after others are
// already running — the late-sidecar case.
func (s *LinuxPodService) CreateContainer(ctx context.Context, req *runtimeapi.CreateContainerRequest) (*runtimeapi.CreateContainerResponse, error) {
	sandboxID := req.GetPodSandboxId()
	cfg := req.GetConfig()
	if sandboxID == "" || cfg == nil || cfg.GetMetadata() == nil || cfg.GetMetadata().GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "CreateContainer: sandbox id and config metadata name are required")
	}
	name := cfg.GetMetadata().GetName()
	image := cfg.GetImage().GetImage()

	s.mu.Lock()
	defer s.mu.Unlock()

	sb, ok := s.sandboxes.Get(sandboxID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "CreateContainer: sandbox %q not found", sandboxID)
	}
	if sb.State != store.StateReady {
		return nil, status.Errorf(codes.FailedPrecondition, "CreateContainer: sandbox %q is %s", sandboxID, sb.State)
	}
	for _, existing := range s.containers.ListBySandbox(sandboxID) {
		if existing.Metadata.Name != name {
			continue
		}
		if existing.State != store.ContainerExited {
			return nil, status.Errorf(codes.AlreadyExists,
				"CreateContainer: container %q already exists in sandbox %q", name, sandboxID)
		}
		if existing.LinuxPod != nil {
			err := s.backend.RemoveContainer(ctx, linuxpod.Ref{PodID: sandboxID, ContainerID: existing.LinuxPod.BackendContainerID})
			if err != nil && !errors.Is(err, linuxpod.ErrContainerNotFound) && !errors.Is(err, linuxpod.ErrPodNotFound) {
				return nil, linuxpodToCRIError("CreateContainer: cleanup exited replacement", err)
			}
		}
	}

	id, err := store.NewID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "CreateContainer: %v", err)
	}
	expectedIdentity := linuxpodExpectedIdentity(id)
	rtMounts, recMounts, err := translateMountsWithPolicy(s.mountPolicy, cfg.GetMounts())
	if err != nil {
		return nil, err
	}
	if err := waitForKubeletMountSourcesWithPolicy(ctx, s.mountPolicy, rtMounts); err != nil {
		return nil, err
	}

	rootfs, err := s.backend.PrepareContainerRootfs(ctx, linuxpod.RootfsRequest{
		PodID: sandboxID, ContainerName: name, Image: image, ExpectedIdentity: expectedIdentity,
	})
	if err != nil {
		return nil, linuxpodToCRIError("CreateContainer: prepare rootfs", err)
	}
	created, err := s.backend.CreateContainer(ctx, linuxpod.CreateRequest{
		PodID:       sandboxID,
		Name:        name,
		RootfsToken: rootfs.Token,
		Command:     cfg.GetCommand(),
		Args:        cfg.GetArgs(),
		Env:         kvToMap(cfg.GetEnvs()),
		Mounts:      linuxpodMounts(rtMounts),
		LogPath:     linuxpodLogPath(sb, cfg),
	})
	if err != nil {
		return nil, linuxpodToCRIError("CreateContainer", err)
	}

	c := &store.Container{
		ID:          id,
		SandboxID:   sandboxID,
		Image:       image,
		Command:     cfg.GetCommand(),
		Args:        cfg.GetArgs(),
		Env:         kvToMap(cfg.GetEnvs()),
		Labels:      cfg.GetLabels(),
		Annotations: cfg.GetAnnotations(),
		LogPath:     cfg.GetLogPath(),
		Mounts:      recMounts,
		State:       store.ContainerCreated,
		CreatedAt:   s.now().UnixNano(),
		LinuxPod: &store.LinuxPodContainer{
			BackendContainerID: created.ID,
			RootfsToken:        rootfs.Token,
			ExpectedIdentity:   expectedIdentity,
		},
	}
	c.Metadata.Name = name
	c.Metadata.Attempt = cfg.GetMetadata().GetAttempt()
	c.Pod.Name = sb.Metadata.Name
	c.Pod.UID = sb.Metadata.UID
	c.Pod.Namespace = sb.Metadata.Namespace
	if err := s.containers.Put(c); err != nil {
		if rerr := s.backend.RemoveContainer(context.WithoutCancel(ctx), linuxpod.Ref{PodID: sandboxID, ContainerID: created.ID}); rerr != nil {
			klog.ErrorS(rerr, "CreateContainer: backend cleanup after persist failure", "containerID", id)
		}
		return nil, status.Errorf(codes.Internal, "CreateContainer: persist: %v", err)
	}
	klog.V(4).InfoS("CRI(LinuxPod) CreateContainer", "id", id, "sandbox", sandboxID, "name", name,
		"createdAfterPodRunning", created.CreatedAfterPodRunning)
	return &runtimeapi.CreateContainerResponse{ContainerId: id}, nil
}

// StartContainer starts the container and gates Running on rootfs identity
// verification. On a mismatch it returns FailedPrecondition and the record is
// marked Exited — never silently Running (CRI-R16).
func (s *LinuxPodService) StartContainer(ctx context.Context, req *runtimeapi.StartContainerRequest) (*runtimeapi.StartContainerResponse, error) {
	id := req.GetContainerId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "StartContainer: container id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.containers.Get(id)
	if !ok || c.LinuxPod == nil {
		return nil, status.Errorf(codes.NotFound, "StartContainer: container %q not found", id)
	}
	if c.State != store.ContainerCreated {
		return nil, status.Errorf(codes.FailedPrecondition, "StartContainer: container %q is %s, expected Created", id, c.State)
	}

	st, err := s.backend.StartContainer(ctx, linuxpod.Ref{PodID: c.SandboxID, ContainerID: c.LinuxPod.BackendContainerID})
	if err != nil {
		// Record the failed identity outcome so status is honest, then surface a
		// precise CRI error. A verification failure is FailedPrecondition.
		c.State = store.ContainerExited
		c.FinishedAt = s.now().UnixNano()
		c.ExitCode = nonZero(st.ExitCode)
		c.Reason = "IdentityVerificationFailed"
		c.Message = st.Message
		c.LinuxPod.ObservedIdentity = st.ObservedIdentity
		c.LinuxPod.IdentityVerified = false
		_ = s.containers.Put(&c)
		return nil, linuxpodToCRIError("StartContainer", err)
	}

	c.State = store.ContainerRunning
	c.StartedAt = s.now().UnixNano()
	c.LinuxPod.ObservedIdentity = st.ObservedIdentity
	c.LinuxPod.IdentityVerified = st.IdentityVerified
	if err := s.containers.Put(&c); err != nil {
		// Unwind so a verified-but-unrecorded container is not leaked Running.
		if _, serr := s.backend.StopContainer(context.WithoutCancel(ctx),
			linuxpod.StopRequest{PodID: c.SandboxID, ContainerID: c.LinuxPod.BackendContainerID}); serr != nil {
			klog.ErrorS(serr, "StartContainer: backend stop after persist failure", "containerID", id)
		}
		return nil, status.Errorf(codes.Internal, "StartContainer: persist running state: %v", err)
	}
	return &runtimeapi.StartContainerResponse{}, nil
}

// StopContainer stops a running container; idempotent for already-stopped ones.
func (s *LinuxPodService) StopContainer(ctx context.Context, req *runtimeapi.StopContainerRequest) (*runtimeapi.StopContainerResponse, error) {
	id := req.GetContainerId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "StopContainer: container id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.containers.Get(id)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "StopContainer: container %q not found", id)
	}
	if c.LinuxPod == nil {
		return nil, status.Errorf(codes.Internal, "StopContainer: container %q has no LinuxPod mapping", id)
	}
	if c.State == store.ContainerExited {
		return &runtimeapi.StopContainerResponse{}, nil
	}
	if _, err := s.backend.StopContainer(ctx, linuxpod.StopRequest{
		PodID: c.SandboxID, ContainerID: c.LinuxPod.BackendContainerID, TimeoutSeconds: int(req.GetTimeout()),
	}); err != nil {
		if !linuxpodBackendMissing(err) {
			return nil, linuxpodToCRIError("StopContainer", err)
		}
	}
	c.State = store.ContainerExited
	c.FinishedAt = s.now().UnixNano()
	c.Reason = "Stopped"
	if err := s.containers.Put(&c); err != nil {
		return nil, status.Errorf(codes.Internal, "StopContainer: persist: %v", err)
	}
	return &runtimeapi.StopContainerResponse{}, nil
}

// RemoveContainer removes a container and its prepared rootfs. Idempotent.
func (s *LinuxPodService) RemoveContainer(ctx context.Context, req *runtimeapi.RemoveContainerRequest) (*runtimeapi.RemoveContainerResponse, error) {
	id := req.GetContainerId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "RemoveContainer: container id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.containers.Get(id)
	if !ok {
		return &runtimeapi.RemoveContainerResponse{}, nil // idempotent
	}
	if c.LinuxPod != nil {
		if err := s.backend.RemoveContainer(ctx, linuxpod.Ref{PodID: c.SandboxID, ContainerID: c.LinuxPod.BackendContainerID}); err != nil {
			if !linuxpodBackendMissing(err) {
				return nil, linuxpodToCRIError("RemoveContainer", err)
			}
		}
	}
	if err := s.containers.Delete(id); err != nil {
		return nil, status.Errorf(codes.Internal, "RemoveContainer: %v", err)
	}
	return &runtimeapi.RemoveContainerResponse{}, nil
}

// ContainerStatus reports a container's status, including LinuxPod identity and
// shared-namespace evidence under verbose.
func (s *LinuxPodService) ContainerStatus(ctx context.Context, req *runtimeapi.ContainerStatusRequest) (*runtimeapi.ContainerStatusResponse, error) {
	id := req.GetContainerId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "ContainerStatus: container id is required")
	}
	c, ok := s.containers.Get(id)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "ContainerStatus: container %q not found", id)
	}
	s.reconcileSandboxBackendState(ctx, c.SandboxID)
	c, ok = s.containers.Get(id)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "ContainerStatus: container %q not found", id)
	}
	var logPathAbs string
	if sb, ok := s.sandboxes.Get(c.SandboxID); ok {
		logPathAbs = linuxpodStoredLogPath(sb, c)
	}
	resp := &runtimeapi.ContainerStatusResponse{Status: toCRIContainerStatus(&c, logPathAbs)}
	if req.GetVerbose() {
		resp.Info = map[string]string{"model": "linuxpod-backed", "sandboxID": c.SandboxID}
		if sb, ok := s.sandboxes.Get(c.SandboxID); ok && sb.LinuxPodNamespace != "" {
			resp.Info["sandboxNamespace"] = sb.LinuxPodNamespace
		}
		if c.LinuxPod != nil {
			resp.Info["backendContainerID"] = c.LinuxPod.BackendContainerID
			resp.Info["expectedIdentity"] = c.LinuxPod.ExpectedIdentity
			resp.Info["observedIdentity"] = c.LinuxPod.ObservedIdentity
			resp.Info["identityVerified"] = fmt.Sprintf("%t", c.LinuxPod.IdentityVerified)
		}
	}
	return resp, nil
}

// ListContainers returns containers matching the filter.
func (s *LinuxPodService) ListContainers(ctx context.Context, req *runtimeapi.ListContainersRequest) (*runtimeapi.ListContainersResponse, error) {
	s.reconcileAllSandboxBackendState(ctx)
	var items []*runtimeapi.Container
	for _, c := range s.containers.List() {
		c := c
		if !matchesContainerFilter(&c, req.GetFilter()) {
			continue
		}
		items = append(items, &runtimeapi.Container{
			Id:           c.ID,
			PodSandboxId: c.SandboxID,
			Metadata:     &runtimeapi.ContainerMetadata{Name: c.Metadata.Name, Attempt: c.Metadata.Attempt},
			State:        toCRIContainerState(c.State),
			CreatedAt:    c.CreatedAt,
			Image:        &runtimeapi.ImageSpec{Image: c.Image},
			ImageRef:     c.Image,
			Labels:       c.Labels,
		})
	}
	return &runtimeapi.ListContainersResponse{Containers: items}, nil
}

// StopPodSandbox stops all containers in the sandbox and marks it NotReady.
func (s *LinuxPodService) StopPodSandbox(ctx context.Context, req *runtimeapi.StopPodSandboxRequest) (*runtimeapi.StopPodSandboxResponse, error) {
	sandboxID := req.GetPodSandboxId()
	if sandboxID == "" {
		return nil, status.Error(codes.InvalidArgument, "StopPodSandbox: pod sandbox id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, c := range s.containers.ListBySandbox(sandboxID) {
		if c.State == store.ContainerExited || c.LinuxPod == nil {
			continue
		}
		if _, err := s.backend.StopContainer(ctx, linuxpod.StopRequest{PodID: sandboxID, ContainerID: c.LinuxPod.BackendContainerID}); err != nil {
			if !linuxpodBackendMissing(err) {
				return nil, linuxpodToCRIError("StopPodSandbox", err)
			}
		}
		c.State = store.ContainerExited
		c.FinishedAt = s.now().UnixNano()
		c.Reason = "SandboxStopped"
		if err := s.containers.Put(&c); err != nil {
			return nil, status.Errorf(codes.Internal, "StopPodSandbox: persist container: %v", err)
		}
	}
	// Tear down the Pod network host path (idempotent); retain the Pod IP reservation
	// until RemovePodSandbox (CRI-L3, #128).
	if err := s.detachSandboxNetwork(ctx, sandboxID); err != nil {
		return nil, err
	}
	// StopPodSandbox is the point where the CRI sandbox must no longer be
	// running. Kubelet may defer RemovePodSandbox, so tear down the LinuxPod VM
	// and helper-side rootfs/work state here while retaining CRI metadata until
	// RemovePodSandbox deletes it.
	rep, err := s.backend.Cleanup(ctx, sandboxID)
	if err != nil {
		if !linuxpodBackendMissing(err) {
			return nil, linuxpodToCRIError("StopPodSandbox", err)
		}
		rep = linuxpod.CleanupReport{PodID: sandboxID}
	}
	if rep.StaleState {
		klog.ErrorS(errors.New("backend reported stale state"), "StopPodSandbox: cleanup left residual state", "sandbox", sandboxID)
	}
	if _, err := s.sandboxes.SetState(sandboxID, store.StateNotReady); err != nil {
		return nil, status.Errorf(codes.Internal, "StopPodSandbox: %v", err)
	}
	return &runtimeapi.StopPodSandboxResponse{}, nil
}

// RemovePodSandbox removes all containers, tears down the Pod VM via Cleanup, and
// deletes the sandbox record. Idempotent and leaves no backend state.
func (s *LinuxPodService) RemovePodSandbox(ctx context.Context, req *runtimeapi.RemovePodSandboxRequest) (*runtimeapi.RemovePodSandboxResponse, error) {
	sandboxID := req.GetPodSandboxId()
	if sandboxID == "" {
		return nil, status.Error(codes.InvalidArgument, "RemovePodSandbox: pod sandbox id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Tear down the Pod network host path and release the Pod IP reservation before
	// the sandbox record is removed (CRI-L3, #128). Both are idempotent, so a retry
	// after a partial RemovePodSandbox leaks neither host rules nor addresses.
	if err := s.detachSandboxNetwork(ctx, sandboxID); err != nil {
		return nil, err
	}
	if sb, ok := s.sandboxes.Get(sandboxID); ok {
		s.releaseSandboxIP(&sb)
	}

	for _, c := range s.containers.ListBySandbox(sandboxID) {
		if err := s.containers.Delete(c.ID); err != nil {
			return nil, status.Errorf(codes.Internal, "RemovePodSandbox: delete container %q: %v", c.ID, err)
		}
	}
	// Cleanup tears down the Pod VM and all its container/rootfs state; idempotent.
	rep, err := s.backend.Cleanup(ctx, sandboxID)
	if err != nil {
		if !linuxpodBackendMissing(err) {
			return nil, linuxpodToCRIError("RemovePodSandbox", err)
		}
		rep = linuxpod.CleanupReport{PodID: sandboxID}
	}
	if rep.StaleState {
		klog.ErrorS(errors.New("backend reported stale state"), "RemovePodSandbox: cleanup left residual state", "sandbox", sandboxID)
	}
	if err := s.sandboxes.Delete(sandboxID); err != nil {
		return nil, status.Errorf(codes.Internal, "RemovePodSandbox: %v", err)
	}
	return &runtimeapi.RemovePodSandboxResponse{}, nil
}

// PodSandboxStatus reports the sandbox, surfacing the shared namespace under
// verbose. The Pod IP is reported by toCRIStatus only once the Pod network path is
// actually attached (CRI-L3, #128) — never a reserved-but-unattached address.
func (s *LinuxPodService) PodSandboxStatus(ctx context.Context, req *runtimeapi.PodSandboxStatusRequest) (*runtimeapi.PodSandboxStatusResponse, error) {
	if req.GetPodSandboxId() == "" {
		return nil, status.Error(codes.InvalidArgument, "PodSandboxStatus: pod sandbox id is required")
	}
	s.reconcileSandboxBackendState(ctx, req.GetPodSandboxId())
	sb, ok := s.sandboxes.Get(req.GetPodSandboxId())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "PodSandboxStatus: sandbox %q not found", req.GetPodSandboxId())
	}
	resp := &runtimeapi.PodSandboxStatusResponse{Status: toCRIStatus(&sb)}
	if req.GetVerbose() {
		resp.Info = map[string]string{"model": "linuxpod-backed", "sandboxNamespace": sb.LinuxPodNamespace}
		if sb.Network.Attached {
			resp.Info["networkAttached"] = "true"
		} else {
			resp.Info["networkAttached"] = "false"
		}
		if sb.Network.PodIP != "" {
			resp.Info["podIP"] = sb.Network.PodIP
		}
		if sb.Network.VMIP != "" {
			resp.Info["vmIP"] = sb.Network.VMIP
		}
		if sb.Network.Interface != "" {
			resp.Info["interface"] = sb.Network.Interface
		}
	}
	return resp, nil
}

// ListPodSandbox returns sandboxes matching the filter.
func (s *LinuxPodService) ListPodSandbox(ctx context.Context, req *runtimeapi.ListPodSandboxRequest) (*runtimeapi.ListPodSandboxResponse, error) {
	s.reconcileAllSandboxBackendState(ctx)
	var items []*runtimeapi.PodSandbox
	for _, sb := range s.sandboxes.List() {
		sb := sb
		if !matchesFilter(&sb, req.GetFilter()) {
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

// --- Minimal ImageService ---
//
// The real image pull happens inside the LinuxPod helper when it stages a rootfs;
// this minimal ImageService satisfies the kubelet PullImage->CreateContainer
// ordering by recording the reference. It is honest about being a thin record,
// not a content-addressable store.

func (s *LinuxPodService) PullImage(_ context.Context, req *runtimeapi.PullImageRequest) (*runtimeapi.PullImageResponse, error) {
	ref := req.GetImage().GetImage()
	if ref == "" {
		return nil, status.Error(codes.InvalidArgument, "PullImage: image reference is required")
	}
	s.mu.Lock()
	s.images[ref] = struct{}{}
	s.mu.Unlock()
	return &runtimeapi.PullImageResponse{ImageRef: ref}, nil
}

func (s *LinuxPodService) ImageStatus(_ context.Context, req *runtimeapi.ImageStatusRequest) (*runtimeapi.ImageStatusResponse, error) {
	ref := req.GetImage().GetImage()
	s.mu.Lock()
	_, ok := s.images[ref]
	s.mu.Unlock()
	if !ok {
		return &runtimeapi.ImageStatusResponse{}, nil // absent: nil image, per CRI
	}
	return &runtimeapi.ImageStatusResponse{Image: linuxpodCRIImage(ref)}, nil
}

func (s *LinuxPodService) ListImages(_ context.Context, _ *runtimeapi.ListImagesRequest) (*runtimeapi.ListImagesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*runtimeapi.Image
	for ref := range s.images {
		out = append(out, linuxpodCRIImage(ref))
	}
	return &runtimeapi.ListImagesResponse{Images: out}, nil
}

func (s *LinuxPodService) RemoveImage(_ context.Context, req *runtimeapi.RemoveImageRequest) (*runtimeapi.RemoveImageResponse, error) {
	s.mu.Lock()
	delete(s.images, req.GetImage().GetImage())
	s.mu.Unlock()
	return &runtimeapi.RemoveImageResponse{}, nil
}

func linuxpodCRIImage(ref string) *runtimeapi.Image {
	return &runtimeapi.Image{Id: ref, RepoTags: []string{ref}, Size: linuxpodUnknownImageSize}
}

func linuxpodMounts(mounts []types.Mount) []linuxpod.Mount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]linuxpod.Mount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, linuxpod.Mount{
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
			Tmpfs:    m.Tmpfs,
		})
	}
	return out
}

// ImageFsInfo returns image-filesystem usage. crictl and the kubelet call it
// during image validation, so an Unimplemented here blocks them (the CRI-L5 #130
// crictl E2E hit exactly this). The LinuxPod helper owns the real image store, so
// the adapter cannot report authoritative usage yet; it returns one timestamped
// filesystem entry with zeroed usage rather than faking numbers or erroring. Use
// "/" as the portable mountpoint because the kubelet stats path stats this value
// on the kubelet host, which may be a Linux control-plane node reaching a remote
// macOS CRI socket over a tunnel. Runtime-private paths such as
// /run/macvz/containers need not exist on that host and make kubelet imageFs stats
// fail before the workload is even scheduled.
func (s *LinuxPodService) ImageFsInfo(_ context.Context, _ *runtimeapi.ImageFsInfoRequest) (*runtimeapi.ImageFsInfoResponse, error) {
	entry := &runtimeapi.FilesystemUsage{
		Timestamp:  s.now().UnixNano(),
		FsId:       &runtimeapi.FilesystemIdentifier{Mountpoint: "/"},
		UsedBytes:  &runtimeapi.UInt64Value{Value: 0},
		InodesUsed: &runtimeapi.UInt64Value{Value: 0},
	}
	return &runtimeapi.ImageFsInfoResponse{ImageFilesystems: []*runtimeapi.FilesystemUsage{entry}}, nil
}

// RecoverContainers reconciles persisted containers against the live backend after
// an adapter restart, without rereading identity evidence: identity is a start
// invariant (#110/#117), so a container that verified at start stays verified.
// A container the backend no longer knows is marked Exited.
func (s *LinuxPodService) RecoverContainers(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reconciled := 0
	for _, c := range s.containers.List() {
		if c.LinuxPod == nil || c.State == store.ContainerExited {
			continue
		}
		st, err := s.backend.Status(ctx, linuxpod.Ref{PodID: c.SandboxID, ContainerID: c.LinuxPod.BackendContainerID})
		changed := false
		if errors.Is(err, linuxpod.ErrContainerNotFound) || errors.Is(err, linuxpod.ErrPodNotFound) {
			c.State = store.ContainerExited
			c.Reason = "NotFound"
			c.FinishedAt = s.now().UnixNano()
			changed = true
		} else if err == nil && st.Phase == "Stopped" || (err == nil && st.Phase == "Failed") {
			c.State = store.ContainerExited
			c.FinishedAt = s.now().UnixNano()
			changed = true
		}
		if changed {
			if perr := s.containers.Put(&c); perr != nil {
				klog.ErrorS(perr, "LinuxPod recovery: persist reconciled container", "containerID", c.ID)
				continue
			}
			reconciled++
		}
	}
	if reconciled > 0 {
		klog.InfoS("recovered LinuxPod CRI container state after restart", "reconciled", reconciled)
	}
}

// AdoptSandboxes runs the live-VM adoption pass after an adapter/helper restart
// (#138), before the fail-fast reconciler can mark sandboxes BackendLost. For each
// Ready sandbox it asks the backend to reattach to the existing Pod VM. When the
// helper reacquires the live VM and every recorded-Running container is confirmed
// live, the sandbox stays Ready and kubelet never recreates the Pod. When the helper
// cannot reacquire the VM, or adoption is incomplete (a recorded-Running container is
// not live), it falls back to the supported BackendLost/recreate path - never leaving
// a stale Running-but-unusable Pod. A backend without the adoption capability
// (ErrUnsupported) makes this an immediate no-op, so the legacy fallback is unchanged.
// Call it once at startup, after RecoverNetwork, before RecoverContainers.
func (s *LinuxPodService) AdoptSandboxes(ctx context.Context) {
	var ready []string
	for _, sb := range s.sandboxes.List() {
		if sb.State == store.StateReady {
			ready = append(ready, sb.ID)
		}
	}
	adopted, fellBack := 0, 0
	for _, sandboxID := range ready {
		res, err := s.backend.Adopt(ctx, sandboxID)
		if errors.Is(err, linuxpod.ErrUnsupported) {
			// The backend does not support adoption: leave everything to the reconciler's
			// BackendLost/recreate path so behavior matches the pre-#138 helper exactly.
			return
		}
		if err != nil && !linuxpodBackendMissing(err) {
			klog.ErrorS(err, "LinuxPod sandbox adoption probe failed; retaining last known CRI state",
				"sandbox", sandboxID)
			continue
		}

		s.mu.Lock()
		sb, ok := s.sandboxes.Get(sandboxID)
		if !ok || sb.State != store.StateReady {
			s.mu.Unlock()
			continue
		}
		if err != nil || !res.Adopted {
			reason := res.Reason
			if reason == "" && err != nil {
				reason = err.Error()
			}
			if reason == "" {
				reason = "LinuxPod helper could not adopt this sandbox after restart"
			}
			s.markSandboxBackendLostLocked(ctx, sandboxID, reason)
			fellBack++
			klog.InfoS("LinuxPod sandbox could not be adopted after restart; will recreate",
				"sandbox", sandboxID, "reason", reason, "err", err)
			s.mu.Unlock()
			continue
		}
		if s.applyAdoptionLocked(ctx, sandboxID, res) {
			adopted++
		} else {
			fellBack++
		}
		s.mu.Unlock()
	}
	if adopted > 0 || fellBack > 0 {
		klog.InfoS("LinuxPod live-VM adoption pass after restart",
			"adopted", adopted, "fellBackToRecreate", fellBack)
	}
}

// applyAdoptionLocked reconciles a sandbox's container records against the live
// status the helper returned for an adopted Pod VM. It confirms every recorded-Running
// container is still live; if any is missing or not Running it funnels the whole
// sandbox into the BackendLost/recreate fallback and returns false, so adoption never
// leaves a Running-but-unusable Pod. Identity evidence is not re-read - it is a start
// invariant (#110/#117), so a container that verified at start stays verified. Caller
// holds mu.
func (s *LinuxPodService) applyAdoptionLocked(ctx context.Context, sandboxID string, res linuxpod.AdoptionResult) bool {
	live := make(map[string]linuxpod.ContainerStatus, len(res.Containers))
	for _, st := range res.Containers {
		live[st.ID] = st
	}
	for _, c := range s.containers.ListBySandbox(sandboxID) {
		if c.LinuxPod == nil || c.State != store.ContainerRunning {
			continue
		}
		if st, ok := live[c.LinuxPod.BackendContainerID]; !ok || st.Phase != runtime.PhaseRunning {
			s.markSandboxBackendLostLocked(ctx, sandboxID,
				"LinuxPod adoption incomplete after helper restart: a running container was not reacquired")
			return false
		}
	}
	klog.InfoS("adopted LinuxPod sandbox after helper restart; Pod preserved without recreate",
		"sandbox", sandboxID, "containers", len(res.Containers))
	return true
}

// StartBackendReconciler periodically probes Ready LinuxPod sandboxes against the
// helper. A restarted helper currently cannot adopt the old helper's live VM
// handles, so it answers ErrPodNotFound for those sandboxes. The reconciler turns
// that into an honest CRI NotReady/Exited view, letting kubelet recreate the Pod
// instead of leaving a stale Running status that only fails later on exec/logs.
func (s *LinuxPodService) StartBackendReconciler(ctx context.Context, interval time.Duration) func() {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	reconcileCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-reconcileCtx.Done():
				return
			case <-ticker.C:
				s.reconcileAllSandboxBackendState(reconcileCtx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (s *LinuxPodService) discardNotReadySandboxForRecreate(ctx context.Context, sb *store.Sandbox) error {
	if sb == nil || sb.State != store.StateNotReady {
		return nil
	}
	if err := s.detachSandboxNetwork(ctx, sb.ID); err != nil {
		return err
	}
	for _, c := range s.containers.ListBySandbox(sb.ID) {
		if err := s.containers.Delete(c.ID); err != nil {
			return status.Errorf(codes.Internal, "RunPodSandbox: discard stale container %q: %v", c.ID, err)
		}
	}
	if rep, err := s.backend.Cleanup(ctx, sb.ID); err != nil {
		if !linuxpodBackendMissing(err) {
			return linuxpodToCRIError("RunPodSandbox: discard stale sandbox", err)
		}
	} else if rep.StaleState {
		klog.ErrorS(errors.New("backend reported stale state"), "RunPodSandbox: stale sandbox cleanup left residual state", "sandbox", sb.ID)
	}
	if err := s.sandboxes.Delete(sb.ID); err != nil {
		return status.Errorf(codes.Internal, "RunPodSandbox: discard stale sandbox %q: %v", sb.ID, err)
	}
	klog.InfoS("discarded NotReady LinuxPod sandbox before recreation",
		"sandbox", sb.ID, "namespace", sb.Metadata.Namespace, "name", sb.Metadata.Name)
	return nil
}

func (s *LinuxPodService) reconcileAllSandboxBackendState(ctx context.Context) {
	for _, sb := range s.sandboxes.List() {
		s.reconcileSandboxBackendState(ctx, sb.ID)
	}
}

func (s *LinuxPodService) reconcileSandboxBackendState(ctx context.Context, sandboxID string) {
	sb, ok := s.sandboxes.Get(sandboxID)
	if !ok || sb.State != store.StateReady {
		return
	}
	s.statusProbeMu.Lock()
	_, err := s.backend.PodStatus(ctx, sandboxID)
	s.statusProbeMu.Unlock()
	if err == nil {
		return
	} else if !linuxpodBackendMissing(err) {
		klog.V(4).InfoS("LinuxPod backend status probe failed; retaining last known CRI state",
			"sandbox", sandboxID, "err", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok = s.sandboxes.Get(sandboxID)
	if !ok || sb.State != store.StateReady {
		return
	}
	s.markSandboxBackendLostLocked(ctx, sandboxID,
		"LinuxPod helper no longer has live backend state for this sandbox")
}

// markSandboxBackendLostLocked drives the fail-fast/recreate fallback for a sandbox
// whose live backend state is gone or whose adoption was incomplete (#138): it tears
// down the Pod network host path, marks every still-live container Exited/BackendLost,
// and sets the sandbox NotReady so kubelet recreates the Pod. It is the single
// fallback both the backend reconciler and the adoption pass funnel through, so the
// supported recreate behavior stays intact regardless of which path detected the
// loss. Caller holds mu.
func (s *LinuxPodService) markSandboxBackendLostLocked(ctx context.Context, sandboxID, reason string) {
	if err := s.detachSandboxNetwork(ctx, sandboxID); err != nil {
		klog.ErrorS(err, "failed to detach LinuxPod network after backend state loss", "sandbox", sandboxID)
	}
	for _, c := range s.containers.ListBySandbox(sandboxID) {
		if c.State == store.ContainerExited {
			continue
		}
		c.State = store.ContainerExited
		c.FinishedAt = s.now().UnixNano()
		c.Reason = "BackendLost"
		c.Message = reason
		if err := s.containers.Put(&c); err != nil {
			klog.ErrorS(err, "failed to persist LinuxPod container backend-loss state", "containerID", c.ID, "sandbox", sandboxID)
		}
	}
	if _, err := s.sandboxes.SetState(sandboxID, store.StateNotReady); err != nil {
		klog.ErrorS(err, "failed to persist LinuxPod sandbox backend-loss state", "sandbox", sandboxID)
		return
	}
	klog.InfoS("marked LinuxPod sandbox not ready after backend state loss", "sandbox", sandboxID, "reason", reason)
}

// --- helpers ---

// linuxpodExpectedIdentity is the deterministic rootfs identity for a CRI
// container id, mirroring the apple/container handoff path's scheme so the two
// stay consistent.
func linuxpodExpectedIdentity(containerID string) string {
	return "macvz-rootfs-id=" + containerID
}

func linuxpodBackendMissing(err error) bool {
	return errors.Is(err, linuxpod.ErrPodNotFound) || errors.Is(err, linuxpod.ErrContainerNotFound)
}

// linuxpodToCRIError maps a backend error to a CRI status error, preserving the
// classification callers and kubelet rely on.
func linuxpodToCRIError(op string, err error) error {
	switch {
	case errors.Is(err, linuxpod.ErrUnsupported):
		return status.Errorf(codes.Unimplemented, "%s: %v", op, err)
	case errors.Is(err, linuxpod.ErrIdentityUnverified):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", op, err)
	case errors.Is(err, linuxpod.ErrPodNotFound), errors.Is(err, linuxpod.ErrContainerNotFound), errors.Is(err, linuxpod.ErrRootfsNotFound):
		return status.Errorf(codes.NotFound, "%s: %v", op, err)
	case errors.Is(err, linuxpod.ErrInvalid):
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

func linuxpodLogPath(sb store.Sandbox, cfg *runtimeapi.ContainerConfig) string {
	if cfg == nil || cfg.GetLogPath() == "" || sb.LogDirectory == "" {
		return ""
	}
	return filepath.Join(sb.LogDirectory, cfg.GetLogPath())
}

func linuxpodStoredLogPath(sb store.Sandbox, c store.Container) string {
	if c.LogPath == "" || sb.LogDirectory == "" {
		return ""
	}
	return filepath.Join(sb.LogDirectory, c.LogPath)
}

func kvToMap(kvs []*runtimeapi.KeyValue) map[string]string {
	if len(kvs) == 0 {
		return nil
	}
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.GetKey()] = kv.GetValue()
	}
	return m
}

func nonZero(code int) int32 {
	if code == 0 {
		return 1
	}
	return int32(code)
}
