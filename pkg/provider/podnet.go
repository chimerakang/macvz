package provider

import (
	"context"
	"time"

	"github.com/chimerakang/macvz/pkg/network/podnet"
	"k8s.io/klog/v2"
)

// PodNetwork wires a Pod's micro-VM into the MacVz Pod network path so the Pod
// is reachable at its assigned Pod IP across the WireGuard mesh (#22). It is
// satisfied by *podnet.Router and is optional: when nil, Pods keep the runtime's
// host-only address and cross-host routing is unavailable.
type PodNetwork interface {
	Attach(ctx context.Context, ep podnet.Endpoint) error
	Detach(ctx context.Context, podKey string) error
}

// VM-IP polling: apple/container assigns the micro-VM its host-only address over
// DHCP shortly after boot, so the address may not be readable the instant Start
// returns. CreatePod polls briefly for it before attaching the network path.
const (
	vmIPPollAttempts = 20
	vmIPPollInterval = 500 * time.Millisecond
)

// podNetRef returns the configured PodNetwork, or nil when it is disabled.
func (p *Provider) podNetRef() PodNetwork {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.podNet
}

// observeVMIP polls the runtime for the micro-VM's host-only address backing the
// Pod's (single, MVP) workload. It returns "" if the address never appears
// within the poll budget or the context is cancelled.
func (p *Provider) observeVMIP(ctx context.Context, st *podState) string {
	if len(st.workloads) == 0 {
		return ""
	}
	return p.observeVMIPByID(ctx, st.workloads[0].id)
}

// observeVMIPByID polls the runtime for a specific workload's host-only address.
// It returns "" if the address never appears within the poll budget or the
// context is cancelled. The restart path (#45) uses it to re-attach a freshly
// recreated micro-VM whose ID is not yet stored on the podState.
func (p *Provider) observeVMIPByID(ctx context.Context, id string) string {
	for attempt := 0; attempt < vmIPPollAttempts; attempt++ {
		if rs, err := p.rt.Status(ctx, id); err == nil && rs.IP != "" {
			return rs.IP
		}
		if attempt == vmIPPollAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(vmIPPollInterval):
		}
	}
	return ""
}

// detachPodNetwork removes a Pod's network mapping. It is a no-op when the
// network path is disabled or the Pod was never attached.
func (p *Provider) detachPodNetwork(ctx context.Context, st *podState, key string) {
	if !st.attached {
		return
	}
	if pn := p.podNetRef(); pn != nil {
		if err := pn.Detach(ctx, key); err != nil {
			klog.ErrorS(err, "failed to detach pod from network path", "pod", key)
		}
	}
}
