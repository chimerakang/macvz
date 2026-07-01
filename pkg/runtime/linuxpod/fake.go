package linuxpod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// journal is the durable, restart-surviving record a real helper would persist
	// to disk (#138). SimulateHelperRestart populates it from the live pods, drops
	// the live (in-memory) handles, then runs the startup adoption pass that
	// reattaches the pods whose micro-VM survived. It models the helper losing its
	// process state while the Pod VMs keep running. Read/written under mu.
	journal map[string]*journalPod
	// adoption records the most recent startup adoption pass so Ping can report it.
	// Read under mu.
	adoption AdoptionStatus

	// VMSurvivesRestart overrides, per pod id, whether that pod's micro-VM survives
	// the next SimulateHelperRestart. Absent pods survive (the common case: the VM
	// outlives the helper process). A test sets false to model a VM that died with
	// the helper and so cannot be adopted, exercising the fall-back-to-recreate path.
	// Read under mu.
	VMSurvivesRestart map[string]bool

	// ObservedIdentityFor overrides the identity a container's late process reports
	// at StartContainer, keyed by container name. Absent names report the expected
	// identity (verification passes). Tests set a wrong value to exercise the
	// identity-mismatch path. Read under mu.
	ObservedIdentityFor map[string]string

	// Capabilities advertises which kubelet surfaces this fake backs. NewFakeBackend
	// turns them all on; a test sets a field false to exercise the ErrUnsupported
	// path for that surface. Read under mu.
	Capabilities Capabilities

	// SandboxAddressFor overrides the host-reachable address a pod's VM reports,
	// keyed by pod id (CRI-L3, #128). Absent pods get a deterministic default. Read
	// under mu.
	SandboxAddressFor map[string]string

	// SandboxAddressReadyAfter withholds a pod's SandboxAddress until this many
	// PodStatus calls have been made for it, modeling the real Pod VM acquiring its
	// address a few polls after boot. 0 (the default) makes the address available
	// immediately. Read under mu.
	SandboxAddressReadyAfter map[string]int
}

type fakePod struct {
	spec        PodSpec
	namespace   string
	phase       runtime.Phase
	sandboxAddr string                    // host-reachable Pod VM address (CRI-L3, #128)
	statusCalls int                       // PodStatus calls, for SandboxAddressReadyAfter latency
	containers  map[string]*fakeContainer // keyed by container id
}

type fakeRootfs struct {
	token            string
	podID            string
	name             string
	image            string
	dns              DNSConfig
	expectedIdentity string
	path             string
	bound            bool // a container has been created against this token
}

type fakeContainer struct {
	id                     string
	name                   string
	podID                  string
	rootfsToken            string
	logPath                string
	phase                  runtime.Phase
	exitCode               int
	message                string
	expectedIdentity       string
	observedIdentity       string
	identityVerified       bool
	createdAfterPodRunning bool
	// mounts is the kubelet-provided mount set the backend was asked to realize in
	// this container's rootfs namespace (CreateRequest.Mounts). The fake does not
	// materialize them — the kubelet already wrote the content on the host — but it
	// records them verbatim so tests can prove the CRI-side volume-projection
	// translation reaches the backend per container (incl. a shared emptyDir
	// appearing in every container that mounts it).
	mounts []Mount
}

// journalPod is one pod's durable record (#138): everything a restarted helper
// needs to readopt the live Pod VM and reconstruct its containers without rerunning
// identity verification (identity is a start invariant). vmAlive records whether the
// micro-VM survived the helper restart and can therefore be reacquired.
type journalPod struct {
	pod     *fakePod
	rootfs  []*fakeRootfs
	vmAlive bool
}

// NewFakeBackend returns an empty fake. now defaults to time.Now when nil.
func NewFakeBackend() *FakeBackend {
	return &FakeBackend{
		now:                      time.Now,
		pods:                     map[string]*fakePod{},
		rootfs:                   map[string]*fakeRootfs{},
		journal:                  map[string]*journalPod{},
		ObservedIdentityFor:      map[string]string{},
		Capabilities:             Capabilities{Logs: true, Exec: true, ExecStream: true, Stats: true, Attach: true, PortForward: true, Adopt: true},
		SandboxAddressFor:        map[string]string{},
		SandboxAddressReadyAfter: map[string]int{},
		VMSurvivesRestart:        map[string]bool{},
	}
}

var _ Backend = (*FakeBackend)(nil)

func (f *FakeBackend) nextSeq() int {
	f.seq++
	return f.seq
}

func (f *FakeBackend) Ping(context.Context) (HelperInfo, error) {
	f.mu.Lock()
	caps := f.Capabilities
	adoption := f.adoption
	adoption.Supported = caps.Adopt
	f.mu.Unlock()
	return HelperInfo{
		Name:            "linuxpod-fake-backend",
		ProtocolVersion: ProtocolVersion,
		Simulated:       true,
		Capabilities:    caps,
		Adoption:        adoption,
	}, nil
}

// SimulateHelperRestart models the LinuxPod helper process restarting while the Pod
// VMs keep running (#138). It snapshots the live pods into the durable journal,
// drops the live in-memory handles (so an un-adopted pod answers ErrPodNotFound,
// exactly as the pre-#138 fail-fast path), then runs the startup adoption pass:
// every pod whose micro-VM survived (VMSurvivesRestart, default true) is reattached
// into the live set and counted Adopted; the rest are counted Lost and left for the
// adapter's BackendLost/recreate fallback. It records the pass in adoption for Ping.
func (f *FakeBackend) SimulateHelperRestart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Capabilities.Adopt {
		// A helper without the capability loses everything on restart (legacy path).
		f.pods = map[string]*fakePod{}
		f.rootfs = map[string]*fakeRootfs{}
		f.journal = map[string]*journalPod{}
		f.adoption = AdoptionStatus{}
		return
	}
	// Snapshot live state into the durable journal, deciding per pod whether its VM
	// survived the restart.
	journal := map[string]*journalPod{}
	for id, p := range f.pods {
		alive := true
		if v, ok := f.VMSurvivesRestart[id]; ok {
			alive = v
		}
		jp := &journalPod{pod: p, vmAlive: alive}
		for _, rf := range f.rootfs {
			if rf.podID == id {
				jp.rootfs = append(jp.rootfs, rf)
			}
		}
		journal[id] = jp
	}
	f.journal = journal

	// Drop live handles: the restarted helper has no in-memory state yet.
	f.pods = map[string]*fakePod{}
	f.rootfs = map[string]*fakeRootfs{}

	// Startup adoption pass: reattach the pods whose VM survived.
	adopted, lost := 0, 0
	for _, jp := range f.journal {
		if jp.vmAlive {
			f.reattachLocked(jp)
			adopted++
		} else {
			lost++
		}
	}
	f.adoption = AdoptionStatus{Supported: true, AdoptedPods: adopted, LostPods: lost}
}

// reattachLocked restores a journaled pod and its rootfs into the live set so
// subsequent PodStatus/Status/Adopt calls observe the reacquired VM. Caller holds mu.
func (f *FakeBackend) reattachLocked(jp *journalPod) {
	f.pods[jp.pod.spec.ID] = jp.pod
	for _, rf := range jp.rootfs {
		f.rootfs[rf.token] = rf
	}
}

// Adopt reports whether the restarted helper reattached to a Pod VM (#138). A pod
// in the live set (reacquired by the startup adoption pass) is Adopted with its
// containers' current status; a journaled pod whose VM did not survive is not
// adopted (the adapter falls back); an entirely unknown pod is ErrPodNotFound.
func (f *FakeBackend) Adopt(_ context.Context, podID string) (AdoptionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Capabilities.Adopt {
		return AdoptionResult{}, fmt.Errorf("%w: adopt", ErrUnsupported)
	}
	if p, ok := f.pods[podID]; ok {
		res := AdoptionResult{PodID: podID, Adopted: true}
		for _, c := range p.containers {
			res.Containers = append(res.Containers, f.statusLocked(p, c))
		}
		return res, nil
	}
	if jp, ok := f.journal[podID]; ok && !jp.vmAlive {
		return AdoptionResult{
			PodID:   podID,
			Adopted: false,
			Reason:  "pod VM did not survive helper restart; recreate required",
		}, nil
	}
	return AdoptionResult{}, fmt.Errorf("%w: %s", ErrPodNotFound, podID)
}

func (f *FakeBackend) CreatePod(_ context.Context, spec PodSpec) (PodStatus, error) {
	if spec.ID == "" {
		return PodStatus{}, fmt.Errorf("%w: pod id is required", ErrInvalid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.pods[spec.ID]; ok {
		return PodStatus{}, fmt.Errorf("%w: pod %q already exists", ErrAlreadyExists, spec.ID)
	}
	delete(f.journal, spec.ID)
	addr := f.SandboxAddressFor[spec.ID]
	if addr == "" {
		// Deterministic, plausible host-only address so a test reading it sees an
		// IP-shaped value. A real helper returns the VM's actual vmnet address.
		addr = fmt.Sprintf("192.168.66.%d", (f.nextSeq()%253)+2)
	}
	p := &fakePod{
		spec:        spec,
		namespace:   "linuxpod-ns-" + spec.ID,
		phase:       runtime.PhaseRunning,
		sandboxAddr: addr,
		containers:  map[string]*fakeContainer{},
	}
	f.pods[spec.ID] = p
	return f.podStatusLocked(p), nil
}

// PodStatus returns a pod's current status, withholding SandboxAddress until
// SandboxAddressReadyAfter[podID] PodStatus calls have been made for it so tests
// can exercise the "address not discovered yet" path (CRI-L3, #128).
func (f *FakeBackend) PodStatus(_ context.Context, podID string) (PodStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[podID]
	if !ok {
		return PodStatus{}, fmt.Errorf("%w: %s", ErrPodNotFound, podID)
	}
	p.statusCalls++
	return f.podStatusLocked(p), nil
}

// podStatusLocked renders a pod's public status, applying the address-discovery
// latency model. Caller holds mu.
func (f *FakeBackend) podStatusLocked(p *fakePod) PodStatus {
	st := PodStatus{ID: p.spec.ID, Phase: p.phase, SandboxNamespace: p.namespace}
	if p.statusCalls >= f.SandboxAddressReadyAfter[p.spec.ID] {
		st.SandboxAddress = p.sandboxAddr
	}
	return st
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
		dns:              cloneDNSConfig(req.DNS),
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
			return ContainerStatus{}, fmt.Errorf("%w: container %q already exists in pod %q", ErrAlreadyExists, req.Name, req.PodID)
		}
	}
	rf.bound = true
	c := &fakeContainer{
		id:                     fmt.Sprintf("%s/%s-%d", req.PodID, req.Name, f.nextSeq()),
		name:                   req.Name,
		podID:                  req.PodID,
		rootfsToken:            req.RootfsToken,
		logPath:                req.LogPath,
		phase:                  runtime.PhaseCreated,
		expectedIdentity:       rf.expectedIdentity,
		createdAfterPodRunning: f.podHasRunningLocked(p),
		mounts:                 append([]Mount(nil), req.Mounts...),
	}
	p.containers[c.id] = c
	// Best-effort: stamp the CRI log file at creation. A log-write failure must not
	// wedge CreateContainer (#129), so the error is intentionally ignored here.
	f.appendCRILog(c, "stdout", "container created (simulated LinuxPod backend; no real stdout)")
	return f.statusLocked(p, c), nil
}

// appendCRILog writes one CRI-format log line for a container when the Logs
// capability is on and the container has a kubelet log path. CRI log format is one
// "<rfc3339nano> <stream> F <message>" line per entry. It is best-effort: it
// returns any I/O error for callers that care, but lifecycle paths ignore it so a
// log failure never wedges a Pod. Caller holds mu.
func (f *FakeBackend) appendCRILog(c *fakeContainer, stream, msg string) error {
	if !f.Capabilities.Logs || c.logPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.logPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	line := fmt.Sprintf("%s %s F %s\n", f.now().UTC().Format(time.RFC3339Nano), stream, msg)
	_, err = file.WriteString(line)
	return err
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
	// Best-effort lifecycle log line (ignored on error so it cannot wedge start).
	f.appendCRILog(c, "stdout", "container started (identity verified)")
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
		delete(f.journal, podID)
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
	delete(f.journal, podID)
	return rep, nil
}

func (f *FakeBackend) ContainerLogPath(_ context.Context, ref Ref) (LogInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Capabilities.Logs {
		return LogInfo{}, fmt.Errorf("%w: logs", ErrUnsupported)
	}
	_, c, err := f.lookupLocked(ref)
	if err != nil {
		return LogInfo{}, err
	}
	if c.logPath == "" {
		return LogInfo{}, fmt.Errorf("%w: container %q was created without a log path", ErrInvalid, ref.ContainerID)
	}
	return LogInfo{PodID: c.podID, ContainerID: c.id, Path: c.logPath, Format: "cri"}, nil
}

func (f *FakeBackend) ExecSync(_ context.Context, req ExecRequest) (ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Capabilities.Exec {
		return ExecResult{}, fmt.Errorf("%w: exec", ErrUnsupported)
	}
	_, c, err := f.lookupLocked(Ref{PodID: req.PodID, ContainerID: req.ContainerID})
	if err != nil {
		return ExecResult{}, err
	}
	if len(req.Command) == 0 {
		return ExecResult{}, fmt.Errorf("%w: exec command is required", ErrInvalid)
	}
	if c.phase != runtime.PhaseRunning {
		return ExecResult{}, fmt.Errorf("%w: container %q is %s, exec requires Running", ErrInvalid, req.ContainerID, c.phase)
	}
	// Simulated exec: echo the command back so the path is provable without a real
	// VM. A real backend runs the command in the Pod VM through the helper.
	return ExecResult{
		Stdout:   []byte(strings.Join(req.Command, " ") + "\n"),
		Stderr:   []byte("linuxpod: simulated exec (no real Pod VM)\n"),
		ExitCode: 0,
	}, nil
}

func (f *FakeBackend) ContainerStats(_ context.Context, ref Ref) (ContainerStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Capabilities.Stats {
		return ContainerStats{}, fmt.Errorf("%w: stats", ErrUnsupported)
	}
	_, c, err := f.lookupLocked(ref)
	if err != nil {
		return ContainerStats{}, err
	}
	// Simulated sample: a real Pod VM measures cgroup usage. Zeroed-but-timestamped
	// and flagged Simulated so the adapter never reports modeled numbers as real.
	return ContainerStats{
		PodID:                 c.podID,
		ContainerID:           c.id,
		TimestampNanos:        f.now().UnixNano(),
		CPUUsageNanoCores:     0,
		MemoryWorkingSetBytes: 0,
		Simulated:             true,
	}, nil
}

func (f *FakeBackend) Attach(_ context.Context, req AttachRequest) (AttachResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Capabilities.Attach {
		return AttachResponse{}, fmt.Errorf("%w: attach", ErrUnsupported)
	}
	_, c, err := f.lookupLocked(Ref{PodID: req.PodID, ContainerID: req.ContainerID})
	if err != nil {
		return AttachResponse{}, err
	}
	if c.phase != runtime.PhaseRunning {
		return AttachResponse{}, fmt.Errorf("%w: container %q is %s, attach requires Running", ErrInvalid, req.ContainerID, c.phase)
	}
	// Simulated negotiation: echo the requested streams as attachable. A real backend
	// wires bidirectional vminitd streams here; that plumbing is the #131 non-goal.
	return AttachResponse{
		Stdin:     req.Stdin,
		Stdout:    req.Stdout,
		Stderr:    req.Stderr,
		TTY:       req.TTY,
		Simulated: true,
		Message:   "simulated attach negotiation (no real VM-internal streams)",
	}, nil
}

func (f *FakeBackend) PortForward(_ context.Context, req PortForwardRequest) (PortForwardResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Capabilities.PortForward {
		return PortForwardResponse{}, fmt.Errorf("%w: portforward", ErrUnsupported)
	}
	if _, ok := f.pods[req.PodID]; !ok {
		return PortForwardResponse{}, fmt.Errorf("%w: %s", ErrPodNotFound, req.PodID)
	}
	for _, p := range req.Ports {
		if p <= 0 || p > 65535 {
			return PortForwardResponse{}, fmt.Errorf("%w: port %d out of range", ErrInvalid, p)
		}
	}
	// Simulated negotiation: report the requested ports as forwardable. A real backend
	// forwards host<->VM byte streams here; that plumbing is the #131 non-goal.
	return PortForwardResponse{
		Ports:     append([]int32(nil), req.Ports...),
		Simulated: true,
		Message:   "simulated port-forward negotiation (no real byte streams)",
	}, nil
}

// ContainerMounts returns the mount set the backend recorded for the named
// container in podID (the verbatim CreateRequest.Mounts), and whether such a
// container exists. It lets a hermetic test assert the CRI-side volume-projection
// translation reached the backend per container — including a shared volume that
// must appear in every container that mounts it. The returned slice is a copy.
func (f *FakeBackend) ContainerMounts(podID, name string) ([]Mount, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[podID]
	if !ok {
		return nil, false
	}
	for _, c := range p.containers {
		if c.name == name {
			return append([]Mount(nil), c.mounts...), true
		}
	}
	return nil, false
}

// RootfsDNS returns the DNS config recorded at PrepareContainerRootfs for the
// named container. It exists so CRI service tests can assert kubelet sandbox DNS
// reached the LinuxPod backend before the helper writes /etc/resolv.conf.
func (f *FakeBackend) RootfsDNS(podID, name string) (DNSConfig, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rf := range f.rootfs {
		if rf.podID == podID && rf.name == name {
			return cloneDNSConfig(rf.dns), true
		}
	}
	return DNSConfig{}, false
}

func cloneDNSConfig(in DNSConfig) DNSConfig {
	return DNSConfig{
		Servers:  append([]string(nil), in.Servers...),
		Searches: append([]string(nil), in.Searches...),
		Options:  append([]string(nil), in.Options...),
	}
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
