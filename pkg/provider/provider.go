// Package provider implements the Virtual Kubelet PodLifecycleHandler, turning
// an Apple Silicon Mac into a Kubernetes node. Each Pod is realized as one or
// more micro-VMs through the runtime.Runtime abstraction.
//
// The provider keeps an in-memory store mapping each Pod (by namespace/name) to
// the runtime workload IDs backing its containers, and reconciles observed
// runtime state into Kubernetes Pod status on demand. Pod-spec translation is
// intentionally minimal here and is extended in #17.
package provider

import (
	"context"
	"crypto/tls"
	"net/http"
	"sync"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/metrics"
	"github.com/chimerakang/macvz/pkg/network"
	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	corev1 "k8s.io/api/core/v1"
)

// defaultStopTimeout is used when a Pod has no terminationGracePeriodSeconds.
const defaultStopTimeout = 30 * time.Second

// defaultProbeUnit is the wall-clock duration of one "second" of a probe's
// timing fields (#50). Production runs at real time; tests shrink it so probe
// loops converge in milliseconds.
const defaultProbeUnit = time.Second

// Provider realizes Kubernetes Pods as micro-VMs via a runtime.Runtime.
type Provider struct {
	nodeName string
	rt       runtime.Runtime

	mu   sync.RWMutex
	pods map[string]*podState

	// createMu serializes CreatePod. Virtual Kubelet can retry or race the same
	// Pod key; serializing keeps the idempotency check and runtime side effects
	// together so duplicate workloads are not leaked.
	createMu sync.Mutex

	// ipam, when set, allocates each Pod a stable IP from this node's
	// Kubernetes-assigned Pod CIDR. It is nil on clusters without coordinated
	// IPAM, in which case the Pod IP falls back to the runtime-reported address.
	ipam *network.PodIPAM

	// podNet, when set, wires each Pod's micro-VM into the Pod network path so it
	// is reachable at its assigned Pod IP across the mesh (#22). Nil disables it.
	podNet PodNetwork

	// hostIP is this node's reachable address, reported as each Pod's HostIP so
	// `kubectl get pod -o wide` and topology-aware routing resolve the host. Set
	// once at startup before the Pod controller runs; treated as immutable after.
	hostIP string

	// collector builds the node/Pod resource metrics served through the kubelet
	// stats and resource-metrics endpoints (#25).
	collector *metrics.Collector

	// volumes is the policy governing which Pod volumes are mounted into
	// micro-VMs and where ephemeral storage is backed (#26). The zero value is
	// safe: hostPath disabled, no ephemeral root.
	volumes VolumePolicy

	// dns is the cluster DNS injected into micro-VMs so in-guest Service name
	// resolution works (#37). The zero value injects nothing.
	dns DNSConfig

	// configMaps resolves ConfigMaps referenced by a Pod's env vars and volumes
	// (#46). Nil disables ConfigMap support: a Pod that references one then fails
	// fast with a clear message.
	configMaps ConfigMapGetter

	// secrets resolves Secrets referenced by a Pod, today the dockerconfigjson
	// pull secrets named in imagePullSecrets (#49). Nil disables Secret support: a
	// Pod that names an imagePullSecret then fails fast with a clear message.
	secrets SecretGetter

	// tokens issues bound service-account tokens for the projected
	// kube-api-access volume, giving Pods normal in-cluster API access (#51). Nil
	// disables it: the auto-injected token volume is tolerated but not mounted, so
	// a Pod gets no service-account credentials.
	tokens TokenRequester

	// restartBackoffBase is the first delay before a terminated workload is
	// restarted under an Always/OnFailure policy; subsequent restarts back off
	// exponentially up to restartBackoffMax (#45). New sets it to
	// defaultRestartBackoffBase; tests lower it for determinism.
	restartBackoffBase time.Duration

	// probeUnit scales a probe's whole-second timing fields (#50). New sets it to
	// defaultProbeUnit (one second); tests shrink it so probe loops run fast.
	probeUnit time.Duration

	// probeHTTPClient performs HTTP GET probes. It follows no redirects and skips
	// TLS verification (HTTPS probes target the Pod's own self-signed endpoint),
	// matching the kubelet; per-attempt deadlines come from the request context.
	probeHTTPClient *http.Client
}

// Option configures a Provider at construction time.
type Option func(*Provider)

// WithIPAM attaches a Pod IP allocator so Pods receive stable, collision-free
// addresses from this node's Kubernetes-assigned Pod CIDR.
func WithIPAM(ipam *network.PodIPAM) Option {
	return func(p *Provider) { p.ipam = ipam }
}

// WithPodNetwork attaches the Pod network path that makes each micro-VM
// reachable at its Pod IP across the mesh (#22).
func WithPodNetwork(pn PodNetwork) Option {
	return func(p *Provider) { p.podNet = pn }
}

// WithHostIP sets the node address reported as each Pod's HostIP.
func WithHostIP(ip string) Option {
	return func(p *Provider) { p.hostIP = ip }
}

// WithVolumePolicy sets the Pod volume policy (#26): the ephemeral storage root
// and the hostPath allowlist. Without it, hostPath is disabled and emptyDir is
// rejected for want of a root.
func WithVolumePolicy(policy VolumePolicy) Option {
	return func(p *Provider) { p.volumes = policy }
}

// WithDNS sets the cluster DNS injected into micro-VMs (#37) so Pods using the
// ClusterFirst DNS policy can resolve Service names. Without it, micro-VMs keep
// the DNS baked into their image.
func WithDNS(dns DNSConfig) Option {
	return func(p *Provider) { p.dns = dns }
}

// podState tracks one Pod and the runtime workloads backing its containers.
type podState struct {
	// pod is the tracked Pod, including the status the provider maintains.
	pod *corev1.Pod
	// workloads maps each container to its runtime workload ID, in spec order.
	workloads []workload
	// terminalStatus, when set, is a sticky status that overrides live
	// reconciliation. It is used for Pods that can never run on this node (an
	// unsupported spec, or an image with no arm64 variant), so they surface a
	// clear, stable Failed status instead of being re-derived as Pending.
	terminalStatus *corev1.PodStatus
	// vmIP is the micro-VM's apple/container host-only address, observed once the
	// VM has booted. It is mapped to the Pod IP by the Pod network path (#22).
	vmIP string
	// attached records whether the Pod's micro-VM has been wired into the Pod
	// network path, so DeletePod knows to tear the mapping down.
	attached bool
	// spec is the resolved single-container workload spec, retained so a restart
	// can recreate the micro-VM without re-resolving the Pod (#45).
	spec types.ContainerSpec
	// restartPolicy is the Pod's effective restart policy (an unset value
	// defaults to Always, matching Kubernetes), governing the restart loop (#45).
	restartPolicy corev1.RestartPolicy
	// restarts counts how many times each container has been restarted, reported
	// as ContainerStatus.RestartCount and used to grow the restart backoff (#45).
	restarts map[string]int32
	// restarting is true while an asynchronous restart of this Pod's workload is
	// in flight, so reconcileStatus reports CrashLoopBackOff and does not launch a
	// second restart goroutine (#45). Guarded by Provider.mu.
	restarting bool
	// probes holds the live results of this Pod's startup/readiness/liveness
	// probes, read by reconcileStatus to gate readiness (#50). Nil when the
	// container declares no probes. Guarded by Provider.mu.
	probes *probeState
	// probesCancel stops the prober goroutines. It is reset when the workload is
	// restarted (fresh probers replace it) and on Pod deletion (#50).
	probesCancel context.CancelFunc
}

// workload binds a Pod container to a runtime workload ID.
type workload struct {
	container string
	id        string
}

// New constructs a Provider bound to a node name and runtime driver.
func New(nodeName string, rt runtime.Runtime, opts ...Option) *Provider {
	p := &Provider{
		nodeName:           nodeName,
		rt:                 rt,
		pods:               make(map[string]*podState),
		restartBackoffBase: defaultRestartBackoffBase,
		probeUnit:          defaultProbeUnit,
		probeHTTPClient: &http.Client{
			// Probes evaluate a single endpoint; never chase redirects.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // kubelet HTTPS probes skip verification by design
				DisableKeepAlives: true,
			},
		},
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.collector == nil {
		p.collector = metrics.NewCollector(nodeName, metrics.DefaultMemorySampler())
	}
	return p
}

// WithCollector overrides the metrics collector, chiefly so tests can inject a
// fake host memory sampler. Production wiring uses the default in New.
func WithCollector(c *metrics.Collector) Option {
	return func(p *Provider) { p.collector = c }
}

// SetIPAM attaches a Pod IP allocator after construction. The node's Pod CIDR is
// only known once Kubernetes assigns it (after node registration), so the
// allocator is wired in then, before the Pod controller starts. It must not be
// called once Pods are being reconciled.
func (p *Provider) SetIPAM(ipam *network.PodIPAM) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ipam = ipam
}

// SetPodNetwork attaches the Pod network path after construction, before the Pod
// controller starts. It must not be called once Pods are being reconciled.
func (p *Provider) SetPodNetwork(pn PodNetwork) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.podNet = pn
}

// SetConfigMapGetter attaches the ConfigMap resolver after construction (#46).
// The pod controller owns the ConfigMap informer, so the getter is wired in once
// that informer exists, before the controller starts reconciling Pods.
func (p *Provider) SetConfigMapGetter(g ConfigMapGetter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.configMaps = g
}

// SetSecretGetter attaches the Secret resolver after construction (#49), wired in
// from the pod controller's own Secret informer once it exists and before the
// controller starts reconciling Pods. It is used to read dockerconfigjson pull
// secrets named in a Pod's imagePullSecrets.
func (p *Provider) SetSecretGetter(g SecretGetter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.secrets = g
}

// WithSecretGetter wires the Secret resolver at construction time (#49), chiefly
// for tests; production wiring uses SetSecretGetter once the informer exists.
func WithSecretGetter(g SecretGetter) Option {
	return func(p *Provider) { p.secrets = g }
}

// SetTokenRequester attaches the service-account token issuer after construction
// (#51), wired from the kubelet once the clientset exists and before the pod
// controller starts reconciling. It lets Pods consume the projected
// kube-api-access volume for in-cluster API access.
func (p *Provider) SetTokenRequester(t TokenRequester) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokens = t
}

// WithTokenRequester wires the service-account token issuer at construction time
// (#51), chiefly for tests; production wiring uses SetTokenRequester once the
// clientset exists.
func WithTokenRequester(t TokenRequester) Option {
	return func(p *Provider) { p.tokens = t }
}

// Compile-time assertion that Provider satisfies the Virtual Kubelet contract.
var _ node.PodLifecycleHandler = (*Provider)(nil)

// podKey is the store key for a Pod.
func podKey(namespace, name string) string {
	return namespace + "/" + name
}

// stopTimeout returns the graceful-stop timeout for a Pod.
func stopTimeout(pod *corev1.Pod) time.Duration {
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		return time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second
	}
	return defaultStopTimeout
}
