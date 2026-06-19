package provider

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// defaultClusterDomain is used when DNS injection is enabled without an explicit
// domain.
const defaultClusterDomain = "cluster.local"

// defaultNdots mirrors the kubelet default so short Service names (e.g.
// "hello") expand through the cluster search domains before being tried as
// absolute.
const defaultNdots = "5"

// DNSConfig is the node-level cluster DNS configuration the provider injects
// into micro-VMs so in-guest Service name resolution works (#37). It is empty by
// default, leaving each micro-VM with the DNS baked into its image.
type DNSConfig struct {
	// ClusterDNS are the cluster DNS server IPs (typically the CoreDNS ClusterIP).
	ClusterDNS []string
	// ClusterDomain is the cluster DNS domain (defaults to "cluster.local").
	ClusterDomain string
}

// enabled reports whether cluster DNS injection is configured.
func (d DNSConfig) enabled() bool { return len(d.ClusterDNS) > 0 }

// domain returns the configured cluster domain or the default.
func (d DNSConfig) domain() string {
	if d.ClusterDomain != "" {
		return d.ClusterDomain
	}
	return defaultClusterDomain
}

// resolveDNS computes the nameservers, search domains, and options for a Pod's
// micro-VM, following the Pod's DNSPolicy:
//
//   - ClusterFirst (and the empty default): cluster DNS servers plus the
//     standard "<ns>.svc.<domain>", "svc.<domain>", "<domain>" search list and
//     ndots:5, merged with any per-Pod dnsConfig extras.
//   - None: exactly the Pod's dnsConfig.
//   - Default: nothing injected (the micro-VM keeps the image/runtime default).
//
// When cluster DNS is not configured, ClusterFirst falls back to "nothing
// injected" so behavior is unchanged on single-host setups.
func resolveDNS(pod *corev1.Pod, cfg DNSConfig) (nameservers, search, options []string) {
	switch pod.Spec.DNSPolicy {
	case corev1.DNSNone:
		return fromPodDNSConfig(pod)
	case corev1.DNSDefault:
		return nil, nil, nil
	default: // ClusterFirst, ClusterFirstWithHostNet, or unset
		if !cfg.enabled() {
			return nil, nil, nil
		}
		domain := cfg.domain()
		ns := append([]string(nil), cfg.ClusterDNS...)
		srch := []string{
			fmt.Sprintf("%s.svc.%s", pod.Namespace, domain),
			fmt.Sprintf("svc.%s", domain),
			domain,
		}
		opts := []string{"ndots:" + defaultNdots}
		// Merge any per-Pod dnsConfig extras (Kubernetes appends them).
		en, es, eo := fromPodDNSConfig(pod)
		ns = dedupeAppend(ns, en)
		srch = dedupeAppend(srch, es)
		opts = dedupeAppend(opts, eo)
		return ns, srch, opts
	}
}

// fromPodDNSConfig extracts nameservers/search/options from pod.Spec.DNSConfig.
func fromPodDNSConfig(pod *corev1.Pod) (nameservers, search, options []string) {
	dc := pod.Spec.DNSConfig
	if dc == nil {
		return nil, nil, nil
	}
	opts := make([]string, 0, len(dc.Options))
	for _, o := range dc.Options {
		if o.Value != nil {
			opts = append(opts, fmt.Sprintf("%s:%s", o.Name, *o.Value))
		} else {
			opts = append(opts, o.Name)
		}
	}
	return append([]string(nil), dc.Nameservers...), append([]string(nil), dc.Searches...), opts
}

// dedupeAppend appends only items not already present in base.
func dedupeAppend(base, extra []string) []string {
	seen := make(map[string]struct{}, len(base))
	for _, b := range base {
		seen[b] = struct{}{}
	}
	for _, e := range extra {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		base = append(base, e)
	}
	return base
}
