package podnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	"k8s.io/klog/v2"
)

// Default tool names, resolved through PATH (never hardcoded absolute paths).
const (
	defaultPfctl  = "pfctl"
	defaultSysctl = "sysctl"
)

// DefaultAnchor is the pf anchor MacVz owns. The operator must reference it from
// the main pf.conf (see docs/NETWORKING.md) so the kernel evaluates these rules.
const DefaultAnchor = "macvz/pods"

// ipForwardingSysctl toggles IPv4 forwarding so the host routes between the mesh
// interface and the vmnet interface that backs the micro-VMs.
const ipForwardingSysctl = "net.inet.ip.forwarding"

// Endpoint binds a Pod's assigned MacVz Pod IP to the host-only address that
// apple/container gave its micro-VM.
type Endpoint struct {
	// PodKey is the "namespace/name" identifying the Pod.
	PodKey string
	// PodIP is the MacVz-assigned Pod IP (from IPAM, #20).
	PodIP string
	// VMIP is the micro-VM's apple/container host-only address.
	VMIP string
}

func (e Endpoint) validate() error {
	if e.PodKey == "" {
		return fmt.Errorf("podnet: endpoint has no pod key")
	}
	if net.ParseIP(e.PodIP) == nil {
		return fmt.Errorf("podnet: endpoint %q has invalid PodIP %q", e.PodKey, e.PodIP)
	}
	if net.ParseIP(e.VMIP) == nil {
		return fmt.Errorf("podnet: endpoint %q has invalid VMIP %q", e.PodKey, e.VMIP)
	}
	return nil
}

// Config configures the Router.
type Config struct {
	// Interface is the host vmnet interface apple/container micro-VMs attach to
	// (e.g. "bridge100"). It scopes the binat rules.
	Interface string
	// Anchor is the pf anchor to manage. Defaults to DefaultAnchor.
	Anchor string
	// EnableForwarding turns on IPv4 forwarding during Start. Disable only when
	// forwarding is managed externally.
	EnableForwarding bool
}

// Router programs the host packet filter so each micro-VM is reachable at its
// MacVz Pod IP. It owns one pf anchor and regenerates it wholesale on every
// change. It is safe for concurrent use.
type Router struct {
	run    runner
	cfg    Config
	pfctl  string
	sysctl string

	mu        sync.Mutex
	started   bool
	endpoints map[string]Endpoint      // keyed by PodKey
	services  map[string][]ServiceRule // keyed by service "namespace/name"
}

// Option configures a Router.
type Option func(*Router)

// WithRunner injects a command runner (used by tests).
func WithRunner(r runner) Option { return func(rt *Router) { rt.run = r } }

// WithTools overrides external binary names. Empty values keep the default.
func WithTools(pfctl, sysctl string) Option {
	return func(rt *Router) {
		if pfctl != "" {
			rt.pfctl = pfctl
		}
		if sysctl != "" {
			rt.sysctl = sysctl
		}
	}
}

// New builds a Router for the given configuration.
func New(cfg Config, opts ...Option) *Router {
	if cfg.Anchor == "" {
		cfg.Anchor = DefaultAnchor
	}
	rt := &Router{
		run:       cliRunner{},
		cfg:       cfg,
		pfctl:     defaultPfctl,
		sysctl:    defaultSysctl,
		endpoints: map[string]Endpoint{},
		services:  map[string][]ServiceRule{},
	}
	for _, opt := range opts {
		opt(rt)
	}
	return rt
}

// Start prepares the host: it enables IPv4 forwarding (when configured), ensures
// pf is enabled, and loads an empty anchor so the ruleset has a known baseline.
func (rt *Router) Start(ctx context.Context) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.cfg.Interface == "" {
		return fmt.Errorf("podnet: interface is required")
	}
	if rt.cfg.EnableForwarding {
		if _, err := rt.run.run(ctx, command{Name: rt.sysctl, Args: []string{"-w", ipForwardingSysctl + "=1"}}); err != nil {
			return fmt.Errorf("enable ip forwarding: %w", err)
		}
	}
	// Enabling pf when it is already on is reported as an error we tolerate.
	if _, err := rt.runTolerating(ctx, command{Name: rt.pfctl, Args: []string{"-e"}}, pfAlreadyEnabled); err != nil {
		return fmt.Errorf("enable pf: %w", err)
	}
	rt.started = true
	if err := rt.loadAnchorLocked(ctx); err != nil {
		return err
	}
	klog.InfoS("pod network started", "interface", rt.cfg.Interface, "anchor", rt.cfg.Anchor, "forwarding", rt.cfg.EnableForwarding)
	return nil
}

// Attach maps a Pod's IP to its micro-VM's address. It is idempotent: attaching
// the same endpoint twice reloads the same ruleset.
func (rt *Router) Attach(ctx context.Context, ep Endpoint) error {
	if err := ep.validate(); err != nil {
		return err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.started {
		return fmt.Errorf("podnet: Attach %q before Start", ep.PodKey)
	}
	rt.endpoints[ep.PodKey] = ep
	if err := rt.loadAnchorLocked(ctx); err != nil {
		return fmt.Errorf("attach %q (%s -> %s): %w", ep.PodKey, ep.PodIP, ep.VMIP, err)
	}
	klog.InfoS("attached pod to network path", "pod", ep.PodKey, "podIP", ep.PodIP, "vmIP", ep.VMIP)
	return nil
}

// Detach removes a Pod's mapping. Detaching an unknown Pod is a no-op, so
// deletion stays idempotent.
func (rt *Router) Detach(ctx context.Context, podKey string) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if _, ok := rt.endpoints[podKey]; !ok {
		return nil
	}
	delete(rt.endpoints, podKey)
	if !rt.started {
		return nil
	}
	if err := rt.loadAnchorLocked(ctx); err != nil {
		return fmt.Errorf("detach %q: %w", podKey, err)
	}
	klog.InfoS("detached pod from network path", "pod", podKey)
	return nil
}

// Endpoints returns a snapshot of the currently attached endpoints, sorted by
// Pod key, so callers and tests can confirm the active mappings.
func (rt *Router) Endpoints() []Endpoint {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]Endpoint, 0, len(rt.endpoints))
	for _, ep := range rt.endpoints {
		out = append(out, ep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PodKey < out[j].PodKey })
	return out
}

// Stop flushes the anchor, removing all MacVz rules. Best-effort.
func (rt *Router) Stop(ctx context.Context) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.endpoints = map[string]Endpoint{}
	rt.services = map[string][]ServiceRule{}
	if !rt.started {
		return nil
	}
	if _, err := rt.run.run(ctx, command{Name: rt.pfctl, Args: []string{"-a", rt.cfg.Anchor, "-F", "all"}}); err != nil {
		klog.ErrorS(err, "failed to flush pod network anchor", "anchor", rt.cfg.Anchor)
	}
	rt.started = false
	klog.InfoS("pod network stopped", "anchor", rt.cfg.Anchor)
	return nil
}

// loadAnchorLocked renders the current ruleset and loads it into the anchor
// atomically via `pfctl -a <anchor> -f -`. Caller holds rt.mu.
func (rt *Router) loadAnchorLocked(ctx context.Context) error {
	rules := renderAnchor(rt.cfg.Interface, rt.endpointsSortedLocked())
	rules += renderServiceRules(rt.cfg.Interface, rt.services, rt.vmipByPodIPLocked())
	_, err := rt.run.run(ctx, command{
		Name:  rt.pfctl,
		Args:  []string{"-a", rt.cfg.Anchor, "-f", "-"},
		Stdin: rules,
	})
	if err != nil {
		return fmt.Errorf("load pf anchor %q: %w", rt.cfg.Anchor, err)
	}
	return nil
}

func (rt *Router) endpointsSortedLocked() []Endpoint {
	out := make([]Endpoint, 0, len(rt.endpoints))
	for _, ep := range rt.endpoints {
		out = append(out, ep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PodKey < out[j].PodKey })
	return out
}

// renderAnchor builds the pf anchor body: one bidirectional NAT rule per Pod,
// mapping the VM's host-only address to its Pod IP. Rendering is deterministic
// (endpoints are pre-sorted) so identical state yields identical rules.
func renderAnchor(iface string, endpoints []Endpoint) string {
	var b strings.Builder
	b.WriteString("# Managed by macvz (issue #22). Do not edit by hand.\n")
	for _, ep := range endpoints {
		fmt.Fprintf(&b, "# %s\n", ep.PodKey)
		fmt.Fprintf(&b, "binat on %s from %s to any -> %s\n", iface, ep.VMIP, ep.PodIP)
	}
	return b.String()
}

// runTolerating runs a command, treating an error whose stderr contains any of
// the benign substrings as success (e.g. "pf already enabled").
func (rt *Router) runTolerating(ctx context.Context, c command, benign ...string) (string, error) {
	out, err := rt.run.run(ctx, c)
	if err == nil {
		return out, nil
	}
	var ce *CommandError
	if errors.As(err, &ce) {
		msg := strings.ToLower(ce.Stderr)
		for _, b := range benign {
			if b != "" && strings.Contains(msg, b) {
				return out, nil
			}
		}
	}
	return out, err
}

// pfAlreadyEnabled is the stderr pfctl prints when pf is already on.
const pfAlreadyEnabled = "pf already enabled"
