package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
)

// Probe handling (#50). MacVz honors a container's startupProbe, readinessProbe,
// and livenessProbe with exec, HTTP GET, and TCP socket handlers. Each configured
// probe runs in its own goroutine for the lifetime of the workload:
//
//   - startup gates the others: until it succeeds the container is not "started",
//     and liveness/readiness are suspended (mirroring the kubelet). A startup
//     failure past its failureThreshold kills the workload like a liveness failure.
//   - readiness drives the container's Ready flag and therefore Service endpoint
//     membership; it never restarts the workload.
//   - liveness restarts the workload (per restartPolicy) once it fails past its
//     failureThreshold.
//
// Probes are bound to a single micro-VM. When a workload is restarted (#45) its
// probers are cancelled and fresh ones start against the new VM, so startup and
// readiness are re-evaluated from scratch — exactly as the kubelet does.
//
// Limitations: gRPC probes and any handler MacVz does not recognize are ignored
// (the container is treated as if that probe were absent). HTTP probes follow no
// redirects and accept any 2xx/3xx status as success.

// probeKind identifies which of a container's three probes a loop is running.
type probeKind int

const (
	probeStartup probeKind = iota
	probeReadiness
	probeLiveness
)

func (k probeKind) String() string {
	switch k {
	case probeStartup:
		return "startup"
	case probeReadiness:
		return "readiness"
	default:
		return "liveness"
	}
}

// Kubernetes probe field defaults, applied when a hand-built Pod omits them (the
// API server populates these on write, but tests and other clients may not).
const (
	defaultProbePeriodSeconds  = 10
	defaultProbeTimeoutSeconds = 1
	defaultProbeFailureThresh  = 3
	defaultProbeSuccessThresh  = 1
)

// probeState tracks the live results of a Pod's probes. It is created when a
// workload starts and is read by reconcileStatus to gate readiness; all fields
// are guarded by Provider.mu.
type probeState struct {
	hasStartup   bool
	hasReadiness bool
	hasLiveness  bool

	// startupDone is true once the startup probe has succeeded (or when no startup
	// probe is configured). It gates readiness and liveness.
	startupDone bool
	// ready reflects the latest readiness-probe result. Meaningful only when
	// hasReadiness is true; otherwise readiness follows the running state.
	ready bool
}

// startProbes launches the prober goroutines for a Pod's container and records
// the resulting probeState on st. It is a no-op when the container declares no
// probes. Must be called with Provider.mu held: it mutates st and the launched
// goroutines read st under the same lock.
func (p *Provider) startProbes(st *podState) {
	if len(st.pod.Spec.Containers) == 0 {
		return
	}
	c := st.pod.Spec.Containers[0]
	if c.StartupProbe == nil && c.ReadinessProbe == nil && c.LivenessProbe == nil {
		return
	}

	key := podKey(st.pod.Namespace, st.pod.Name)
	host := st.pod.Status.PodIP // HTTP/TCP probes default to the Pod IP when no Host is set.

	ps := &probeState{
		hasStartup:   c.StartupProbe != nil,
		hasReadiness: c.ReadinessProbe != nil,
		hasLiveness:  c.LivenessProbe != nil,
		startupDone:  c.StartupProbe == nil,
	}
	ctx, cancel := context.WithCancel(context.Background())
	st.probes = ps
	st.probesCancel = cancel

	if ps.hasStartup {
		go p.runProbe(ctx, key, c, host, c.StartupProbe, probeStartup)
	}
	if ps.hasReadiness {
		go p.runProbe(ctx, key, c, host, c.ReadinessProbe, probeReadiness)
	}
	if ps.hasLiveness {
		go p.runProbe(ctx, key, c, host, c.LivenessProbe, probeLiveness)
	}
}

// stopProbes cancels any running probers for a Pod and clears its probe state.
// Must be called with Provider.mu held.
func (p *Provider) stopProbes(st *podState) {
	if st.probesCancel != nil {
		st.probesCancel()
		st.probesCancel = nil
	}
	st.probes = nil
}

// runProbe is the per-probe loop: it waits out the initial delay, then probes on
// the configured period, tracking consecutive successes and failures against the
// probe's thresholds and acting on the result according to its kind. It exits
// when the context is cancelled (Pod deleted or workload restarted), and — for
// startup and liveness — once it has acted on a threshold breach.
func (p *Provider) runProbe(ctx context.Context, key string, c corev1.Container, host string, probe *corev1.Probe, kind probeKind) {
	handler := p.probeHandler(key, c, host, probe)
	if handler == nil {
		// An unrecognized handler (e.g. gRPC) cannot be evaluated. A startup probe
		// that can never succeed would block the container forever, so treat it as
		// satisfied; readiness/liveness simply do not gate.
		klog.InfoS("ignoring probe with unsupported handler", "pod", key, "kind", kind.String())
		if kind == probeStartup {
			p.setStartupDone(key)
		}
		return
	}

	period := p.probeDuration(orDefault(probe.PeriodSeconds, defaultProbePeriodSeconds))
	timeout := p.probeDuration(orDefault(probe.TimeoutSeconds, defaultProbeTimeoutSeconds))
	failureThreshold := orDefault(probe.FailureThreshold, defaultProbeFailureThresh)
	successThreshold := orDefault(probe.SuccessThreshold, defaultProbeSuccessThresh)
	// The kubelet pins successThreshold to 1 for startup and liveness probes; only
	// readiness may require multiple consecutive successes.
	if kind != probeReadiness {
		successThreshold = 1
	}

	if !sleepCtx(ctx, p.probeDuration(probe.InitialDelaySeconds)) {
		return
	}

	var successes, failures int32
	for {
		// Readiness and liveness are suspended until the startup probe completes.
		if kind != probeStartup && p.hasStartupProbe(key) && !p.startupDone(key) {
			if !sleepCtx(ctx, period) {
				return
			}
			continue
		}

		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		err := handler(attemptCtx)
		cancel()

		if err == nil {
			failures = 0
			successes++
			if successes >= successThreshold {
				switch kind {
				case probeStartup:
					klog.InfoS("startup probe succeeded", "pod", key)
					p.setStartupDone(key)
					return
				case probeReadiness:
					p.setReady(key, true)
				}
			}
		} else {
			successes = 0
			failures++
			if failures >= failureThreshold {
				switch kind {
				case probeReadiness:
					p.setReady(key, false)
				case probeStartup, probeLiveness:
					klog.InfoS("probe failed past threshold; failing workload", "pod", key, "kind", kind.String(), "threshold", failureThreshold, "err", err.Error())
					p.failProbe(key, c.Name)
					return
				}
			}
		}

		if !sleepCtx(ctx, period) {
			return
		}
	}
}

// failProbe handles a container that failed its liveness or startup probe: it is
// killed and restarted per the Pod's restartPolicy. A liveness/startup failure is
// treated as an unhealthy exit, reusing the restart loop (#45). Under a Never
// policy the workload is stopped and not recreated, so the Pod becomes Failed —
// matching the kubelet.
func (p *Provider) failProbe(key, container string) {
	p.mu.Lock()
	st, ok := p.pods[key]
	if !ok || st.terminalStatus != nil {
		p.mu.Unlock()
		return
	}
	var id string
	for _, w := range st.workloads {
		if w.container == container {
			id = w.id
		}
	}
	if p.maybeTriggerRestart(st, container, id, 1) {
		p.mu.Unlock()
		klog.InfoS("probe failure restarting workload per restartPolicy", "pod", key, "container", container)
		return
	}

	// restartPolicy Never: the workload is killed and not recreated. A probe kill
	// is a failure, not a clean exit, so record a sticky Failed status — otherwise
	// the next reconcile would see the destroyed workload as "Lost" with exit 0 and
	// derive Succeeded. Stop probing too.
	p.stopProbes(st)
	var c corev1.Container
	if len(st.pod.Spec.Containers) > 0 {
		c = st.pod.Spec.Containers[0]
	}
	const reason = "ProbeFailure"
	const msg = "container failed its liveness/startup probe"
	status := corev1.PodStatus{
		Phase:             corev1.PodFailed,
		Reason:            reason,
		Message:           msg,
		StartTime:         st.pod.Status.StartTime,
		PodIP:             st.pod.Status.PodIP,
		PodIPs:            st.pod.Status.PodIPs,
		HostIP:            st.pod.Status.HostIP,
		HostIPs:           st.pod.Status.HostIPs,
		ContainerStatuses: []corev1.ContainerStatus{terminatedStatus(c, id, 137, reason, msg)},
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
			{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, Reason: reason},
			{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: reason},
		},
	}
	st.terminalStatus = &status
	st.pod.Status = status
	p.mu.Unlock()

	klog.InfoS("probe failure under restartPolicy Never; stopping workload", "pod", key, "container", container)
	ctx, cancel := context.WithTimeout(context.Background(), restartOpTimeout)
	defer cancel()
	_ = p.rt.Stop(ctx, id, defaultStopTimeout)
	if err := p.rt.Destroy(ctx, id); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		klog.ErrorS(err, "probe: destroy workload after probe failure", "pod", key, "id", id)
	}
}

// probeHandler compiles a probe into a single-attempt function that returns nil
// on success. It returns nil for a probe with no handler MacVz supports.
func (p *Provider) probeHandler(key string, c corev1.Container, host string, probe *corev1.Probe) func(context.Context) error {
	switch {
	case probe.Exec != nil:
		cmd := append([]string(nil), probe.Exec.Command...)
		container := c.Name
		return func(ctx context.Context) error {
			id, ok := p.currentWorkloadID(key, container)
			if !ok {
				return fmt.Errorf("no running workload for container %q", container)
			}
			return p.rt.Exec(ctx, id, cmd, runtime.ExecIO{Stdout: io.Discard, Stderr: io.Discard})
		}

	case probe.HTTPGet != nil:
		g := probe.HTTPGet
		h := g.Host
		if h == "" {
			h = host
		}
		scheme := strings.ToLower(string(g.Scheme))
		if scheme == "" {
			scheme = "http"
		}
		url := fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(h, strconv.Itoa(resolvePort(g.Port, c))), pathOrRoot(g.Path))
		headers := g.HTTPHeaders
		return func(ctx context.Context) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			for _, hd := range headers {
				req.Header.Add(hd.Name, hd.Value)
			}
			resp, err := p.probeHTTPClient.Do(req)
			if err != nil {
				return err
			}
			defer func() {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
			return fmt.Errorf("HTTP probe returned status %d", resp.StatusCode)
		}

	case probe.TCPSocket != nil:
		t := probe.TCPSocket
		h := t.Host
		if h == "" {
			h = host
		}
		addr := net.JoinHostPort(h, strconv.Itoa(resolvePort(t.Port, c)))
		return func(ctx context.Context) error {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				return err
			}
			return conn.Close()
		}

	default:
		return nil
	}
}

// currentWorkloadID returns the runtime workload ID currently backing a Pod's
// container. Probers read it per attempt so a restart that swaps the workload is
// picked up transparently.
func (p *Provider) currentWorkloadID(key, container string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	st, ok := p.pods[key]
	if !ok {
		return "", false
	}
	for _, w := range st.workloads {
		if w.container == container && w.id != "" {
			return w.id, true
		}
	}
	return "", false
}

// hasStartupProbe reports whether the Pod declares a startup probe, so the
// readiness/liveness loops only wait on startup when there is one to wait for.
func (p *Provider) hasStartupProbe(key string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	st, ok := p.pods[key]
	return ok && st.probes != nil && st.probes.hasStartup
}

func (p *Provider) startupDone(key string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	st, ok := p.pods[key]
	return ok && (st.probes == nil || st.probes.startupDone)
}

func (p *Provider) setStartupDone(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.pods[key]; ok && st.probes != nil {
		st.probes.startupDone = true
	}
}

func (p *Provider) setReady(key string, ready bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.pods[key]; ok && st.probes != nil {
		st.probes.ready = ready
	}
}

// probeReadiness derives a container's started and ready state from its probe
// results. With no probes configured, both follow the running state — preserving
// the pre-probe behavior. Callers fold in the Pod-IP requirement for endpoint
// readiness separately.
func (p *Provider) probeReadiness(st *podState, phase corev1.PodPhase) (started, ready bool) {
	running := phase == corev1.PodRunning
	started, ready = running, running
	ps := st.probes
	if ps == nil || !running {
		return started, ready
	}
	if ps.hasStartup {
		started = ps.startupDone
	}
	switch {
	case !started:
		ready = false
	case ps.hasReadiness:
		ready = ps.ready
	default:
		ready = true
	}
	return started, ready
}

// probeDuration scales a probe's whole-second field by the provider's probe unit
// (one second in production; shortened in tests for deterministic, fast runs).
func (p *Provider) probeDuration(seconds int32) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * p.probeUnit
}

// resolvePort resolves a probe port that may be numeric or a named container
// port. An unresolved name yields 0, which fails the probe's dial — surfacing the
// misconfiguration rather than silently probing the wrong port.
func resolvePort(port intstr.IntOrString, c corev1.Container) int {
	if port.Type == intstr.Int {
		return port.IntValue()
	}
	for _, cp := range c.Ports {
		if cp.Name == port.StrVal {
			return int(cp.ContainerPort)
		}
	}
	return 0
}

func pathOrRoot(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

func orDefault(v, def int32) int32 {
	if v <= 0 {
		return def
	}
	return v
}

// sleepCtx waits for d or until ctx is done, reporting true if the full duration
// elapsed and false if the context was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
