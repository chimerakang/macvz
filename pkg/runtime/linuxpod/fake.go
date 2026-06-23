package linuxpod

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// fake.go is the in-process FakeBackend: a hermetic model of the LinuxPod
// lifecycle used by tests and served behind the helper protocol in client tests.
// It is not a simulation of Apple Containerization internals — it models exactly
// the contract guarantees the issue requires: a Pod VM with a single shared
// network namespace, late-binding container creation after the pod is running,
// rootfs identity verification at start (using the production exact-match logic in
// pkg/runtime), and a Cleanup that leaves no state. A real helper replaces this
// with calls into LinuxPod; the contract and its tests stay identical.

// FakeBackend is a concurrency-safe in-memory Backend. The zero value is not
// usable; construct it with NewFakeBackend. It is exported so cmd/macvz-cri and
// integration harnesses can run the contract without a Swift toolchain.
type FakeBackend struct {
	mu     sync.Mutex
	now    func() time.Time
	seq    int
	pods   map[string]*fakePod
	rootfs map[string]*fakeRootfs

	// ObservedIdentityFor overrides the identity a container's late process reports
	// at StartContainer, keyed by container name. Absent names report the expected
	// identity (verification passes). Tests set a wrong value to exercise the
	// identity-mismatch path. Read under mu.
	ObservedIdentityFor map[string]string
}

type fakePod struct {
	spec       PodSpec
	namespace  string
	phase      runtime.Phase
	containers map[string]*fakeContainer // keyed by container id
}

type fakeRootfs struct {
	token            string
	podID            string
	name             string
	image            string
	expectedIdentity string
	path             string
	bound            bool // a container has been created against this token
}

type fakeContainer struct {
	id                     string
	name                   string
	podID                  string
	rootfsToken            string
	phase                  runtime.Phase
	exitCode               int
	message                string
	expectedIdentity       string
	observedIdentity       string
	identityVerified       bool
	createdAfterPodRunning bool
}

// NewFakeBackend returns an empty fake. now defaults to time.Now when nil.
func NewFakeBackend() *FakeBackend {
	return &FakeBackend{
		now:                 time.Now,
		pods:                map[string]*fakePod{},
		rootfs:              map[string]*fakeRootfs{},
		ObservedIdentityFor: map[string]string{},
	}
}

var _ Backend = (*FakeBackend)(nil)

func (f *FakeBackend) nextSeq() int {
	f.seq++
	return f.seq
}

func (f *FakeBackend) Ping(context.Context) (HelperInfo, error) {
	return HelperInfo{Name: "linuxpod-fake-backend", ProtocolVersion: ProtocolVersion, Simulated: true}, nil
}

func (f *FakeBackend) CreatePod(_ context.Context, spec PodSpec) (PodStatus, error) {
	if spec.ID == "" {
		return PodStatus{}, fmt.Errorf("%w: pod id is required", ErrInvalid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.pods[spec.ID]; ok {
		return PodStatus{}, fmt.Errorf("%w: pod %q already exists", ErrInvalid, spec.ID)
	}
	p := &fakePod{
		spec:       spec,
		namespace:  "linuxpod-ns-" + spec.ID,
		phase:      runtime.PhaseRunning,
		containers: map[string]*fakeContainer{},
	}
	f.pods[spec.ID] = p
	return PodStatus{ID: spec.ID, Phase: p.phase, SandboxNamespace: p.namespace}, nil
}

func (f *FakeBackend) PrepareContainerRootfs(_ context.Context, req RootfsRequest) (RootfsHandle, error) {
	if req.PodID == "" || req.ContainerName == "" || req.ExpectedIdentity == "" {
		return RootfsHandle{}, fmt.Errorf("%w: podID, containerName, and expectedIdentity are required", ErrInvalid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[req.PodID]
	if !ok {
		return RootfsHandle{}, fmt.Errorf("%w: %s", ErrPodNotFound, req.PodID)
	}
	if p.phase != runtime.PhaseRunning {
		return RootfsHandle{}, fmt.Errorf("%w: pod %q is %s, cannot stage rootfs", ErrInvalid, req.PodID, p.phase)
	}
	token := fmt.Sprintf("rootfs-%s-%s-%d", req.PodID, req.ContainerName, f.nextSeq())
	f.rootfs[token] = &fakeRootfs{
		token:            token,
		podID:            req.PodID,
		name:             req.ContainerName,
		image:            req.Image,
		expectedIdentity: req.ExpectedIdentity,
		path:             "/run/macvz/containers/" + token + "/rootfs",
	}
	return RootfsHandle{Token: token, RootfsPath: f.rootfs[token].path}, nil
}

func (f *FakeBackend) CreateContainer(_ context.Context, req CreateRequest) (ContainerStatus, error) {
	if req.PodID == "" || req.Name == "" || req.RootfsToken == "" {
		return ContainerStatus{}, fmt.Errorf("%w: podID, name, and rootfsToken are required", ErrInvalid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[req.PodID]
	if !ok {
		return ContainerStatus{}, fmt.Errorf("%w: %s", ErrPodNotFound, req.PodID)
	}
	rf, ok := f.rootfs[req.RootfsToken]
	if !ok || rf.podID != req.PodID {
		return ContainerStatus{}, fmt.Errorf("%w: %s", ErrRootfsNotFound, req.RootfsToken)
	}
	if rf.bound {
		return ContainerStatus{}, fmt.Errorf("%w: rootfs token %q already bound", ErrInvalid, req.RootfsToken)
	}
	for _, c := range p.containers {
		if c.name == req.Name {
			return ContainerStatus{}, fmt.Errorf("%w: container %q already exists in pod %q", ErrInvalid, req.Name, req.PodID)
		}
	}
	rf.bound = true
	c := &fakeContainer{
		id:                     fmt.Sprintf("%s/%s-%d", req.PodID, req.Name, f.nextSeq()),
		name:                   req.Name,
		podID:                  req.PodID,
		rootfsToken:            req.RootfsToken,
		phase:                  runtime.PhaseCreated,
		expectedIdentity:       rf.expectedIdentity,
		createdAfterPodRunning: f.podHasRunningLocked(p),
	}
	p.containers[c.id] = c
	return f.statusLocked(p, c), nil
}

func (f *FakeBackend) StartContainer(_ context.Context, ref Ref) (ContainerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, c, err := f.lookupLocked(ref)
	if err != nil {
		return ContainerStatus{}, err
	}
	if c.phase == runtime.PhaseRunning {
		return f.statusLocked(p, c), nil
	}
	if c.phase != runtime.PhaseCreated {
		return ContainerStatus{}, fmt.Errorf("%w: container %q is %s, expected Created", ErrInvalid, ref.ContainerID, c.phase)
	}

	// Simulate the late process reporting its rootfs identity through the handoff
	// channel, then verify with the production exact-match logic so the fake never
	// diverges from how the real adapter decides verification (CRI-R16).
	observed := c.expectedIdentity
	if v, ok := f.ObservedIdentityFor[c.name]; ok {
		observed = v
	}
	c.observedIdentity = observed
	meta := runtime.NewHandoffMeta(c.id, runtime.HandoffLayout{}, c.expectedIdentity)
	if meta.Verify(observed, f.now()) != runtime.IdentityVerified {
		c.phase = runtime.PhaseFailed
		c.exitCode = 1
		c.identityVerified = false
		c.message = "rootfs identity not verified: expected " + c.expectedIdentity + ", observed " + observed
		return f.statusLocked(p, c), fmt.Errorf("%w: container %q expected %q observed %q",
			ErrIdentityUnverified, ref.ContainerID, c.expectedIdentity, observed)
	}
	c.phase = runtime.PhaseRunning
	c.identityVerified = true
	c.message = ""
	return f.statusLocked(p, c), nil
}

func (f *FakeBackend) StopContainer(_ context.Context, req StopRequest) (ContainerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, c, err := f.lookupLocked(Ref{PodID: req.PodID, ContainerID: req.ContainerID})
	if err != nil {
		return ContainerStatus{}, err
	}
	if c.phase == runtime.PhaseRunning {
		c.phase = runtime.PhaseStopped
		c.exitCode = 0
	}
	return f.statusLocked(p, c), nil
}

func (f *FakeBackend) RemoveContainer(_ context.Context, ref Ref) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[ref.PodID]
	if !ok {
		return nil // idempotent: pod gone implies container gone
	}
	c, ok := p.containers[ref.ContainerID]
	if !ok {
		return nil // idempotent: unknown container
	}
	delete(p.containers, ref.ContainerID)
	delete(f.rootfs, c.rootfsToken)
	return nil
}

func (f *FakeBackend) Status(_ context.Context, ref Ref) (ContainerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, c, err := f.lookupLocked(ref)
	if err != nil {
		return ContainerStatus{}, err
	}
	return f.statusLocked(p, c), nil
}

func (f *FakeBackend) Cleanup(_ context.Context, podID string) (CleanupReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[podID]
	if !ok {
		return CleanupReport{PodID: podID}, nil // idempotent
	}
	rep := CleanupReport{PodID: podID, PodRemoved: true, RemovedContainers: len(p.containers)}
	for token, rf := range f.rootfs {
		if rf.podID == podID {
			delete(f.rootfs, token)
			rep.RemovedRootfs++
		}
	}
	delete(f.pods, podID)
	return rep, nil
}

// lookupLocked resolves a ref to its pod and container. Caller holds mu.
func (f *FakeBackend) lookupLocked(ref Ref) (*fakePod, *fakeContainer, error) {
	p, ok := f.pods[ref.PodID]
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrPodNotFound, ref.PodID)
	}
	c, ok := p.containers[ref.ContainerID]
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrContainerNotFound, ref.ContainerID)
	}
	return p, c, nil
}

func (f *FakeBackend) podHasRunningLocked(p *fakePod) bool {
	for _, c := range p.containers {
		if c.phase == runtime.PhaseRunning {
			return true
		}
	}
	return false
}

// statusLocked renders a container's public status. Caller holds mu.
func (f *FakeBackend) statusLocked(p *fakePod, c *fakeContainer) ContainerStatus {
	return ContainerStatus{
		PodID:                  c.podID,
		ID:                     c.id,
		Name:                   c.name,
		Phase:                  c.phase,
		ExitCode:               c.exitCode,
		Message:                c.message,
		SandboxNamespace:       p.namespace,
		CreatedAfterPodRunning: c.createdAfterPodRunning,
		LocalhostReachable:     c.phase == runtime.PhaseRunning,
		ExpectedIdentity:       c.expectedIdentity,
		ObservedIdentity:       c.observedIdentity,
		IdentityVerified:       c.identityVerified,
	}
}
