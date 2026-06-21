package provider

import (
	"github.com/chimerakang/macvz/pkg/network"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// ipamRef returns the configured allocator, or nil when coordinated IPAM is off.
func (p *Provider) ipamRef() *network.PodIPAM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ipam
}

// allocateIP assigns a stable Pod IP for key from the node's Pod CIDR. When IPAM
// is disabled it returns "" with no error, leaving the Pod IP to be derived from
// the runtime-reported address.
func (p *Provider) allocateIP(key string) (string, error) {
	ipam := p.ipamRef()
	if ipam == nil {
		return "", nil
	}
	return ipam.Allocate(key)
}

// hasAllocatedIP reports whether key already holds an IP before this create
// attempt. Startup recovery pre-reserves API-observed PodIPs; a failed adoption
// retry must not release those durable reservations back to the pool.
func (p *Provider) hasAllocatedIP(key string) bool {
	ipam := p.ipamRef()
	if ipam == nil {
		return false
	}
	return ipam.IP(key) != ""
}

// releaseIP returns key's Pod IP to the pool. It is a no-op when IPAM is
// disabled or key holds no address, so deletion stays idempotent.
func (p *Provider) releaseIP(key string) {
	if ipam := p.ipamRef(); ipam != nil {
		ipam.Release(key)
	}
}

// RecoverAllocations rebuilds the allocator from Kubernetes state so a restarted
// provider neither leaks addresses nor reassigns a live Pod's IP. It reserves
// the PodIP already recorded for each Pod that this node owns. It is a no-op when
// IPAM is disabled.
//
// Call it once at startup, after SetIPAM and before the Pod controller runs.
func (p *Provider) RecoverAllocations(pods []*corev1.Pod) {
	ipam := p.ipamRef()
	if ipam == nil {
		return
	}
	recovered := 0
	for _, pod := range pods {
		ip := pod.Status.PodIP
		if ip == "" {
			continue
		}
		key := podKey(pod.Namespace, pod.Name)
		if err := ipam.Reserve(key, ip); err != nil {
			// An out-of-range or conflicting address predates this CIDR (e.g. a
			// CIDR change). Skip it rather than fail startup; the Pod will be
			// reassigned on its next create.
			klog.ErrorS(err, "skipping IPAM recovery for Pod", "pod", key, "ip", ip)
			continue
		}
		recovered++
	}
	if recovered > 0 {
		klog.InfoS("recovered Pod IP allocations from Kubernetes", "count", recovered, "cidr", ipam.CIDR())
	}
}
