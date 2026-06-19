package podnet

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	"k8s.io/klog/v2"
)

// ServiceRule is one ClusterIP:port DNAT to its ready backends. It maps a
// Kubernetes ClusterIP Service port to the set of ready backend Pod IPs, so a
// micro-VM that dials the ClusterIP reaches a backend. There is no kube-proxy on
// a MacVz node (it is a macOS host and the kube-proxy Pod spec is unsupported),
// so MacVz programs this translation in the same pf anchor that owns the Pod
// binat rules (#37/P5).
type ServiceRule struct {
	// ServiceKey is the "namespace/name" of the owning Service (diagnostics).
	ServiceKey string
	// ClusterIP is the Service's stable virtual IP.
	ClusterIP string
	// Protocol is "tcp" or "udp" (lowercased pf token).
	Protocol string
	// Port is the Service port the ClusterIP listens on.
	Port int
	// TargetPort is the backend container port traffic is redirected to.
	TargetPort int
	// Backends are the ready backend Pod IPs (MacVz Pod IPs). An empty list
	// means the Service has no ready endpoints and emits no rule.
	Backends []string
}

func (r ServiceRule) validate() error {
	if r.ServiceKey == "" {
		return fmt.Errorf("podnet: service rule has no service key")
	}
	if net.ParseIP(r.ClusterIP) == nil {
		return fmt.Errorf("podnet: service %q has invalid ClusterIP %q", r.ServiceKey, r.ClusterIP)
	}
	switch r.Protocol {
	case "tcp", "udp":
	default:
		return fmt.Errorf("podnet: service %q has unsupported protocol %q", r.ServiceKey, r.Protocol)
	}
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("podnet: service %q port %d out of range", r.ServiceKey, r.Port)
	}
	if r.TargetPort < 1 || r.TargetPort > 65535 {
		return fmt.Errorf("podnet: service %q targetPort %d out of range", r.ServiceKey, r.TargetPort)
	}
	for _, b := range r.Backends {
		if net.ParseIP(b) == nil {
			return fmt.Errorf("podnet: service %q has invalid backend IP %q", r.ServiceKey, b)
		}
	}
	return nil
}

// AttachService installs (or replaces) the DNAT rules for one Service, keyed by
// its "namespace/name". Passing rules with no backends, or an empty slice,
// removes the Service's rules — so a Service that loses all ready endpoints
// stops redirecting. It is idempotent.
func (rt *Router) AttachService(ctx context.Context, key string, rules []ServiceRule) error {
	if key == "" {
		return fmt.Errorf("podnet: AttachService requires a service key")
	}
	kept := make([]ServiceRule, 0, len(rules))
	for _, r := range rules {
		if err := r.validate(); err != nil {
			return err
		}
		if len(r.Backends) == 0 {
			continue
		}
		kept = append(kept, r)
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.started {
		return fmt.Errorf("podnet: AttachService %q before Start", key)
	}
	if len(kept) == 0 {
		delete(rt.services, key)
	} else {
		rt.services[key] = kept
	}
	if err := rt.loadAnchorLocked(ctx); err != nil {
		return fmt.Errorf("attach service %q: %w", key, err)
	}
	klog.InfoS("attached service to network path", "service", key, "rules", len(kept))
	return nil
}

// DetachService removes a Service's DNAT rules. Detaching an unknown Service is
// a no-op so deletion stays idempotent.
func (rt *Router) DetachService(ctx context.Context, key string) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if _, ok := rt.services[key]; !ok {
		return nil
	}
	delete(rt.services, key)
	if !rt.started {
		return nil
	}
	if err := rt.loadAnchorLocked(ctx); err != nil {
		return fmt.Errorf("detach service %q: %w", key, err)
	}
	klog.InfoS("detached service from network path", "service", key)
	return nil
}

// Services returns the service keys with active rules, sorted, for diagnostics
// and tests.
func (rt *Router) Services() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]string, 0, len(rt.services))
	for k := range rt.services {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// renderServiceRules builds the pf `rdr` rules for all services. Each backend
// Pod IP is resolved against the local endpoint set: a backend whose Pod runs on
// this node is redirected straight to its micro-VM's host-only address (directly
// attached to the vmnet interface, so no extra route is needed), while a backend
// on another Mac keeps its Pod IP and is reached over the WireGuard mesh route.
// Output is deterministic: services, rules, and backends are all sorted.
func renderServiceRules(iface string, services map[string][]ServiceRule, vmipByPodIP map[string]string) string {
	keys := make([]string, 0, len(services))
	for k := range services {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, key := range keys {
		rules := append([]ServiceRule(nil), services[key]...)
		sort.Slice(rules, func(i, j int) bool {
			if rules[i].ClusterIP != rules[j].ClusterIP {
				return rules[i].ClusterIP < rules[j].ClusterIP
			}
			return rules[i].Port < rules[j].Port
		})
		for _, r := range rules {
			targets := make([]string, 0, len(r.Backends))
			for _, podIP := range r.Backends {
				if vmIP, ok := vmipByPodIP[podIP]; ok {
					targets = append(targets, vmIP) // local backend, via vmnet
				} else {
					targets = append(targets, podIP) // remote backend, via mesh
				}
			}
			sort.Strings(targets)

			fmt.Fprintf(&b, "# %s %s:%d -> :%d\n", r.ServiceKey, r.ClusterIP, r.Port, r.TargetPort)
			if len(targets) == 1 {
				fmt.Fprintf(&b, "rdr on %s inet proto %s from any to %s port %d -> %s port %d\n",
					iface, r.Protocol, r.ClusterIP, r.Port, targets[0], r.TargetPort)
				continue
			}
			fmt.Fprintf(&b, "rdr on %s inet proto %s from any to %s port %d -> { %s } port %d round-robin\n",
				iface, r.Protocol, r.ClusterIP, r.Port, strings.Join(targets, ", "), r.TargetPort)
		}
	}
	return b.String()
}

// vmipByPodIPLocked maps each attached Pod IP to its micro-VM address. Caller
// holds rt.mu.
func (rt *Router) vmipByPodIPLocked() map[string]string {
	m := make(map[string]string, len(rt.endpoints))
	for _, ep := range rt.endpoints {
		m[ep.PodIP] = ep.VMIP
	}
	return m
}
