package criserver

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
)

// This file implements the CRI-P3 single-container Pod lifecycle (#75): one CRI
// Pod sandbox owns exactly one apple/container micro-VM. It deliberately stays
// narrow — no shared Pod network, no shared volumes, and no multi-container
// support — so the create/start/stop/remove/status/list path can be proven
// end-to-end before the data-plane work of CRI-P4/P5.
//
// A second container in a sandbox is rejected with an explicit FailedPrecondition
// rather than silently mismodeled, and every operation without a configured
// runtime fails the same honest way instead of faking success.

// ContainerRuntime is the subset of runtime.Runtime the CRI container lifecycle
// drives. *container.Driver (apple/container) satisfies it; tests inject a fake.
// It is defined here, rather than reusing runtime.Runtime wholesale, so the CRI
// adapter depends only on the operations it actually calls.
type ContainerRuntime interface {
	Pull(ctx context.Context, image string, auth *runtime.RegistryAuth) error
	Create(ctx context.Context, spec types.ContainerSpec) (id string, err error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string, timeout time.Duration) error
	Destroy(ctx context.Context, id string) error
	Status(ctx context.Context, id string) (runtime.Status, error)
	// Logs returns a reader over the workload's combined output for the CRI-P6
	// logging path (#78); the caller closes it.
	Logs(ctx context.Context, id string, opts runtime.LogOptions) (io.ReadCloser, error)
	// Exec runs a command inside the workload, wiring the given streams, for the
	// CRI-P6 exec path (#78).
	Exec(ctx context.Context, id string, cmd []string, sio runtime.ExecIO) error
}

// statsRuntime is the optional resource-usage capability the CRI-P6 stats
// surfaces use (#78). It mirrors runtime.Stater; *container.Driver satisfies it.
// When the configured runtime does not implement it, the stats surfaces degrade
// to empty/unavailable rather than faking samples.
type statsRuntime interface {
	Stats(ctx context.Context, id string) (runtime.ResourceStats, error)
}

// defaultStopTimeout is used when StopContainer is called with a non-positive
// timeout (kubelet sends 0 to request the runtime default).
const defaultStopTimeout = 10 * time.Second

// CreateContainer creates (but does not start) the single container of a Pod
// sandbox. It pulls the image, provisions an apple/container workload, and
// records the mapping. The image pull happens here because the ImageService is
// out of scope for this spike, so CreateContainer is self-sufficient for the
// crictl-driven validation flow.
//
// CRI-P3 supports exactly one container per sandbox: a second CreateContainer in
// the same sandbox returns FailedPrecondition rather than pretending to model a
// multi-container Pod.
func (s *Server) CreateContainer(ctx context.Context, req *runtimeapi.CreateContainerRequest) (*runtimeapi.CreateContainerResponse, error) {
	if s.containerRuntime == nil {
		return nil, errRuntimeNotConfigured("CreateContainer")
	}
	sandboxID := req.GetPodSandboxId()
	if sandboxID == "" {
		return nil, status.Error(codes.InvalidArgument, "CreateContainer: pod sandbox id is required")
	}
	cfg := req.GetConfig()
	md := cfg.GetMetadata()
	if cfg == nil || md == nil || md.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "CreateContainer: config and metadata name are required")
	}
	image := cfg.GetImage().GetImage()
	if image == "" {
		return nil, status.Error(codes.InvalidArgument, "CreateContainer: image reference is required")
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	sb, ok := s.sandboxes.Get(sandboxID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "CreateContainer: sandbox %q not found", sandboxID)
	}
	if sb.State != store.StateReady {
		return nil, status.Errorf(codes.FailedPrecondition, "CreateContainer: sandbox %q is not Ready", sandboxID)
	}
	if existing := s.containers.ListBySandbox(sandboxID); len(existing) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"CreateContainer: sandbox %q already has a container (%s); CRI-P3 supports one container per Pod sandbox",
			sandboxID, existing[0].ID)
	}

	id, err := store.NewID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "CreateContainer: %v", err)
	}
	workloadID := store.DeriveWorkloadID(id)

	c := &store.Container{
		ID:          id,
		SandboxID:   sandboxID,
		WorkloadID:  workloadID,
		Image:       image,
		Command:     cfg.GetCommand(),
		Args:        cfg.GetArgs(),
		Env:         envMap(cfg.GetEnvs()),
		Labels:      cfg.GetLabels(),
		Annotations: cfg.GetAnnotations(),
		LogPath:     cfg.GetLogPath(),
		State:       store.ContainerCreated,
		CreatedAt:   s.now().UnixNano(),
	}
	c.Metadata.Name = md.GetName()
	c.Metadata.Attempt = md.GetAttempt()
	c.Pod.Name = sb.Metadata.Name
	c.Pod.UID = sb.Metadata.UID
	c.Pod.Namespace = sb.Metadata.Namespace

	// Image acquisition: once the ImageService is wired (CRI-P4), kubelet and
	// crictl pull via PullImage before CreateContainer, so we must not pull
	// implicitly here — that would duplicate ImageService behavior. Instead verify
	// the image is already present and fail with a clear FailedPrecondition if not.
	// With no ImageService configured (the container-runtime-only skeleton), fall
	// back to pulling here so the single-container path still works end to end.
	if s.imageRuntime != nil {
		if _, err := s.imageRuntime.ImageStatus(ctx, image); err != nil {
			if errors.Is(err, runtime.ErrNotFound) {
				return nil, status.Errorf(codes.FailedPrecondition,
					"CreateContainer: image %q is not present; pull it via the ImageService (PullImage) before creating the container", image)
			}
			return nil, runtimeError("CreateContainer", "inspect image", err)
		}
	} else if err := s.containerRuntime.Pull(ctx, image, nil); err != nil {
		// Pull verifies the arm64 variant, so an arch-incompatible image fails with
		// a clear error before a workload is provisioned.
		return nil, runtimeError("CreateContainer", "pull image", err)
	}

	spec := types.ContainerSpec{
		Name:    workloadID,
		Image:   image,
		Command: cfg.GetCommand(),
		Args:    cfg.GetArgs(),
		Env:     c.Env,
	}
	if _, err := s.containerRuntime.Create(ctx, spec); err != nil {
		return nil, runtimeError("CreateContainer", "create workload", err)
	}

	// Persist only after the workload exists. If persistence fails, reclaim the
	// workload so the create leaves neither an orphan record nor an orphan VM.
	if err := s.containers.Put(c); err != nil {
		if derr := s.containerRuntime.Destroy(context.WithoutCancel(ctx), workloadID); derr != nil {
			klog.ErrorS(derr, "CreateContainer: failed to reclaim workload after persist error",
				"containerID", id, "workloadID", workloadID)
		}
		return nil, status.Errorf(codes.Internal, "CreateContainer: persist: %v", err)
	}
	klog.V(4).InfoS("CRI CreateContainer", "containerID", id, "sandboxID", sandboxID,
		"workloadID", workloadID, "image", image)
	return &runtimeapi.CreateContainerResponse{ContainerId: id}, nil
}

// StartContainer boots the container's micro-VM. The container must be in the
// Created state; starting a running or exited container is a FailedPrecondition.
func (s *Server) StartContainer(ctx context.Context, req *runtimeapi.StartContainerRequest) (*runtimeapi.StartContainerResponse, error) {
	if s.containerRuntime == nil {
		return nil, errRuntimeNotConfigured("StartContainer")
	}
	id := req.GetContainerId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "StartContainer: container id is required")
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	c, ok := s.containers.Get(id)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "StartContainer: container %q not found", id)
	}
	if c.State != store.ContainerCreated {
		return nil, status.Errorf(codes.FailedPrecondition,
			"StartContainer: container %q is %s, expected Created", id, c.State)
	}
	if err := s.containerRuntime.Start(ctx, c.WorkloadID); err != nil {
		return nil, runtimeError("StartContainer", "start workload", err)
	}
	c.State = store.ContainerRunning
	c.StartedAt = s.now().UnixNano()
	if err := s.containers.Put(&c); err != nil {
		return nil, status.Errorf(codes.Internal, "StartContainer: persist: %v", err)
	}

	// Attach the Pod network path now that the micro-VM is booting (CRI-P5, #77).
	// The container start path is where the host-only VM address first becomes
	// observable, so this is where the binat rule is programmed and the sandbox's
	// Pod IP becomes reportable. A failure here unwinds the start: the workload is
	// stopped and the container is marked Exited with a clear reason rather than
	// left Running behind an unreachable Pod IP.
	sb, sbOK := s.sandboxes.Get(c.SandboxID)
	if s.networkEnabled() {
		if sbOK && sb.Network.PodIP != "" {
			if err := s.attachSandboxNetwork(ctx, &sb, c.WorkloadID); err != nil {
				s.unwindContainerStart(ctx, &c)
				return nil, err
			}
		}
	}

	// Begin streaming the container's output into its CRI log file (CRI-P6, #78) so
	// `kubectl logs` works. This is best-effort and runs on a background context, so
	// it neither fails nor blocks the start.
	if sbOK {
		s.startLogPump(&c, &sb)
	}
	klog.V(4).InfoS("CRI StartContainer", "containerID", id, "workloadID", c.WorkloadID)
	return &runtimeapi.StartContainerResponse{}, nil
}

// StopContainer stops the container's micro-VM and records its exit information.
// It is idempotent: stopping an already-exited container succeeds.
func (s *Server) StopContainer(ctx context.Context, req *runtimeapi.StopContainerRequest) (*runtimeapi.StopContainerResponse, error) {
	if s.containerRuntime == nil {
		return nil, errRuntimeNotConfigured("StopContainer")
	}
	id := req.GetContainerId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "StopContainer: container id is required")
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	c, ok := s.containers.Get(id)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "StopContainer: container %q not found", id)
	}

	timeout := defaultStopTimeout
	if req.GetTimeout() > 0 {
		timeout = time.Duration(req.GetTimeout()) * time.Second
	}
	if err := s.stopContainerRecord(ctx, c, timeout, "StopContainer"); err != nil {
		return nil, err
	}
	klog.V(4).InfoS("CRI StopContainer", "containerID", id, "workloadID", c.WorkloadID)
	return &runtimeapi.StopContainerResponse{}, nil
}

// RemoveContainer destroys the container's micro-VM and deletes its record. It is
// idempotent: removing an absent container succeeds, matching the CRI contract.
func (s *Server) RemoveContainer(ctx context.Context, req *runtimeapi.RemoveContainerRequest) (*runtimeapi.RemoveContainerResponse, error) {
	if s.containerRuntime == nil {
		return nil, errRuntimeNotConfigured("RemoveContainer")
	}
	id := req.GetContainerId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "RemoveContainer: container id is required")
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	c, ok := s.containers.Get(id)
	if !ok {
		return &runtimeapi.RemoveContainerResponse{}, nil
	}
	if err := s.removeContainerRecord(ctx, c, "RemoveContainer"); err != nil {
		return nil, err
	}
	klog.V(4).InfoS("CRI RemoveContainer", "containerID", id, "workloadID", c.WorkloadID)
	return &runtimeapi.RemoveContainerResponse{}, nil
}

// ContainerStatus returns the status of a container, erroring NotFound if absent.
// When a runtime is configured it reconciles the stored state with the live
// workload, so a container that exited on its own (or after an adapter restart)
// is reported as Exited with its real exit code rather than a stale Running.
func (s *Server) ContainerStatus(ctx context.Context, req *runtimeapi.ContainerStatusRequest) (*runtimeapi.ContainerStatusResponse, error) {
	if s.containerRuntime == nil {
		return nil, errRuntimeNotConfigured("ContainerStatus")
	}
	id := req.GetContainerId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "ContainerStatus: container id is required")
	}
	c, ok := s.containers.Get(id)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "ContainerStatus: container %q not found", id)
	}
	if reconciled, changed := s.reconcile(ctx, c); changed {
		if err := s.containers.Put(&reconciled); err == nil {
			c = reconciled
			if c.State == store.ContainerExited {
				if err := s.detachContainerNetwork(ctx, c.SandboxID, "ContainerStatus"); err != nil {
					return nil, err
				}
			}
		}
	}
	resp := &runtimeapi.ContainerStatusResponse{Status: toCRIContainerStatus(&c)}
	if req.GetVerbose() {
		resp.Info = map[string]string{
			"model":      "single-container-pod-spike",
			"workloadID": c.WorkloadID,
			"sandboxID":  c.SandboxID,
		}
	}
	return resp, nil
}

// ListContainers returns containers matching the optional filter (id, sandbox id,
// state, and label selector). It overrides the CRI-P1 always-empty stub.
func (s *Server) ListContainers(_ context.Context, req *runtimeapi.ListContainersRequest) (*runtimeapi.ListContainersResponse, error) {
	filter := req.GetFilter()
	var items []*runtimeapi.Container
	for _, c := range s.containers.List() {
		if !matchesContainerFilter(&c, filter) {
			continue
		}
		items = append(items, &runtimeapi.Container{
			Id:           c.ID,
			PodSandboxId: c.SandboxID,
			Metadata:     &runtimeapi.ContainerMetadata{Name: c.Metadata.Name, Attempt: c.Metadata.Attempt},
			Image:        &runtimeapi.ImageSpec{Image: c.Image},
			ImageRef:     c.Image,
			State:        toCRIContainerState(c.State),
			CreatedAt:    c.CreatedAt,
			Labels:       c.Labels,
			Annotations:  c.Annotations,
		})
	}
	return &runtimeapi.ListContainersResponse{Containers: items}, nil
}

// reconcile refreshes a stored container against the live workload. It only ever
// advances a non-terminal container to Exited; it never resurrects an exited one
// (the workload may already be gone). It returns the possibly-updated copy and
// whether anything changed. Runtime errors are treated as "no live info".
func (s *Server) reconcile(ctx context.Context, c store.Container) (store.Container, bool) {
	if c.State == store.ContainerExited {
		return c, false
	}
	st, err := s.containerRuntime.Status(ctx, c.WorkloadID)
	if err != nil {
		if errors.Is(err, runtime.ErrNotFound) {
			c.State = store.ContainerExited
			c.FinishedAt = s.now().UnixNano()
			c.Reason = "NotFound"
			c.Message = "runtime workload not found"
			return c, true
		}
		return c, false
	}
	switch st.Phase {
	case runtime.PhaseStopped, runtime.PhaseFailed:
		c.State = store.ContainerExited
		c.ExitCode = int32(st.ExitCode)
		c.FinishedAt = s.now().UnixNano()
		if st.Phase == runtime.PhaseFailed {
			c.Reason = "Error"
		} else {
			c.Reason = "Completed"
		}
		return c, true
	case runtime.PhaseRunning:
		if c.State != store.ContainerRunning {
			c.State = store.ContainerRunning
			return c, true
		}
	}
	return c, false
}

func (s *Server) stopSandboxContainers(ctx context.Context, sandboxID string, method string) error {
	for _, c := range s.containers.ListBySandbox(sandboxID) {
		if err := s.stopContainerRecord(ctx, c, defaultStopTimeout, method); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) removeSandboxContainers(ctx context.Context, sandboxID string, method string) error {
	for _, c := range s.containers.ListBySandbox(sandboxID) {
		if err := s.removeContainerRecord(ctx, c, method); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) stopContainerRecord(ctx context.Context, c store.Container, timeout time.Duration, method string) error {
	// Reap the log pump regardless of state: once the workload stops its follow
	// stream ends on its own, but stopping here removes the tracking entry promptly
	// and deterministically. It is a no-op when no pump is running.
	s.stopLogPump(c.ID)
	if c.State == store.ContainerExited {
		return s.detachContainerNetwork(ctx, c.SandboxID, method)
	}
	if s.containerRuntime == nil {
		return errRuntimeNotConfigured(method)
	}
	if err := s.containerRuntime.Stop(ctx, c.WorkloadID, timeout); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return runtimeError(method, "stop workload", err)
	}

	c.State = store.ContainerExited
	c.FinishedAt = s.now().UnixNano()
	if st, err := s.containerRuntime.Status(ctx, c.WorkloadID); err == nil {
		c.ExitCode = int32(st.ExitCode)
		if st.Phase == runtime.PhaseFailed {
			c.Reason = "Error"
		} else {
			c.Reason = "Completed"
		}
	} else if errors.Is(err, runtime.ErrNotFound) {
		c.Reason = "NotFound"
		c.Message = "runtime workload not found"
	} else {
		c.Reason = "Completed"
	}
	if err := s.containers.Put(&c); err != nil {
		return status.Errorf(codes.Internal, "%s: persist container %q: %v", method, c.ID, err)
	}
	if err := s.detachContainerNetwork(ctx, c.SandboxID, method); err != nil {
		return err
	}
	return nil
}

func (s *Server) removeContainerRecord(ctx context.Context, c store.Container, method string) error {
	if s.containerRuntime == nil {
		return errRuntimeNotConfigured(method)
	}
	if err := s.containerRuntime.Destroy(ctx, c.WorkloadID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return runtimeError(method, "destroy workload", err)
	}
	if err := s.detachContainerNetwork(ctx, c.SandboxID, method); err != nil {
		return err
	}
	if err := s.containers.Delete(c.ID); err != nil {
		return status.Errorf(codes.Internal, "%s: delete container %q: %v", method, c.ID, err)
	}
	return nil
}

func (s *Server) detachContainerNetwork(ctx context.Context, sandboxID string, method string) error {
	if !s.networkEnabled() {
		return nil
	}
	if err := s.detachSandboxNetwork(ctx, sandboxID); err != nil {
		st, ok := status.FromError(err)
		if !ok {
			return status.Errorf(codes.Internal, "%s: detach network: %v", method, err)
		}
		return status.Error(st.Code(), method+": "+st.Message())
	}
	return nil
}

func toCRIContainerState(st store.ContainerState) runtimeapi.ContainerState {
	switch st {
	case store.ContainerCreated:
		return runtimeapi.ContainerState_CONTAINER_CREATED
	case store.ContainerRunning:
		return runtimeapi.ContainerState_CONTAINER_RUNNING
	case store.ContainerExited:
		return runtimeapi.ContainerState_CONTAINER_EXITED
	default:
		return runtimeapi.ContainerState_CONTAINER_UNKNOWN
	}
}

func toCRIContainerStatus(c *store.Container) *runtimeapi.ContainerStatus {
	return &runtimeapi.ContainerStatus{
		Id:          c.ID,
		Metadata:    &runtimeapi.ContainerMetadata{Name: c.Metadata.Name, Attempt: c.Metadata.Attempt},
		State:       toCRIContainerState(c.State),
		CreatedAt:   c.CreatedAt,
		StartedAt:   c.StartedAt,
		FinishedAt:  c.FinishedAt,
		ExitCode:    c.ExitCode,
		Image:       &runtimeapi.ImageSpec{Image: c.Image},
		ImageRef:    c.Image,
		Reason:      c.Reason,
		Message:     c.Message,
		Labels:      c.Labels,
		Annotations: c.Annotations,
		LogPath:     c.LogPath,
	}
}

// matchesContainerFilter applies a CRI ContainerFilter. A nil filter matches all.
func matchesContainerFilter(c *store.Container, f *runtimeapi.ContainerFilter) bool {
	if f == nil {
		return true
	}
	if f.GetId() != "" && f.GetId() != c.ID {
		return false
	}
	if f.GetPodSandboxId() != "" && f.GetPodSandboxId() != c.SandboxID {
		return false
	}
	if sv := f.GetState(); sv != nil && sv.GetState() != toCRIContainerState(c.State) {
		return false
	}
	for k, v := range f.GetLabelSelector() {
		if c.Labels[k] != v {
			return false
		}
	}
	return true
}

// envMap flattens CRI's ordered KeyValue env list into the map ContainerSpec
// carries. Env order is not preserved — acceptable for the single-container
// spike, where no env depends on evaluation order.
func envMap(kvs []*runtimeapi.KeyValue) map[string]string {
	if len(kvs) == 0 {
		return nil
	}
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.GetKey()] = kv.GetValue()
	}
	return m
}

// errRuntimeNotConfigured is returned by container methods when no container
// runtime is wired (e.g. the default skeleton). The methods are implemented but
// cannot act without a backend, so FailedPrecondition is the honest code.
func errRuntimeNotConfigured(method string) error {
	return status.Errorf(codes.FailedPrecondition,
		"%s: no container runtime is configured (experimental adapter started without apple/container backend)", method)
}

// runtimeError maps a runtime driver failure to a CRI status code, surfacing
// NotFound and arch-incompatibility distinctly and defaulting to Internal.
func runtimeError(method, stage string, err error) error {
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		return status.Errorf(codes.NotFound, "%s: %s: %v", method, stage, err)
	case errors.Is(err, runtime.ErrIncompatibleArch):
		return status.Errorf(codes.FailedPrecondition, "%s: %s: %v", method, stage, err)
	case errors.Is(err, runtime.ErrNotReady):
		return status.Errorf(codes.Unavailable, "%s: %s: %v", method, stage, err)
	default:
		return status.Errorf(codes.Internal, "%s: %s: %v", method, stage, err)
	}
}
