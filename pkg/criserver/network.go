package criserver

import (
	"context"
	"fmt"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/network/podnet"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

// This file implements the CRI-P5 Pod networking lifecycle (#77). It deliberately
// reuses the shipped Virtual Kubelet networking primitives rather than inventing a
// CRI-only path: Pod IPs come from network.PodIPAM (the node's Kubernetes-assigned
// Pod CIDR) and the host packet-filter path comes from podnet.Router (pf binat per
// micro-VM). The CRI adapter only depends on the narrow operations it actually
// calls, so both primitives are reached through interfaces a test can fake.
//
// CRI splits the lifecycle differently from the provider: RunPodSandbox has no
// micro-VM yet, so it can only reserve the Pod IP. The host-only VM address — and
// therefore the binat attach — only becomes available after the single container
// starts, so the attach happens on the container start path (StartContainer). The
// sandbox is reported with a Pod IP (PodSandboxStatus.Network.Ip) only after that
// attach actually succeeds; nothing here fakes a Pod IP or NetworkReady.

// PodNetwork attaches a sandbox's micro-VM into the MacVz Pod network path so the
// Pod is reachable at its assigned Pod IP across the WireGuard mesh (#22). It is
// satisfied by *podnet.Router and is optional: when nil, the CRI adapter runs
// sandboxes without a Pod IP and reports NetworkReady=false.
type PodNetwork interface {
	Attach(ctx context.Context, ep podnet.Endpoint) error
	Detach(ctx context.Context, podKey string) error
}

type podNetworkEndpointLister interface {
	Endpoints() []podnet.Endpoint
}

// PodIPAllocator hands out stable Pod IPs from the node's Pod CIDR. It is
// satisfied by *network.PodIPAM and is optional: when nil, no Pod IP is assigned.
// The subset mirrors the operations the provider's IPAM helpers use, so the CRI
// adapter and the Virtual Kubelet provider share the same allocation semantics.
type PodIPAllocator interface {
	Allocate(key string) (string, error)
	Reserve(key, ip string) error
	Release(key string)
	IP(key string) string
	CIDR() string
}

// VM-IP polling mirrors the provider: apple/container assigns the micro-VM its
// host-only address over DHCP shortly after boot, so the address may not be
// readable the instant Start returns. StartContainer polls briefly for it before
// attaching the Pod network path. The fields are overridable so tests stay fast.
const (
	defaultVMIPPollAttempts = 20
	defaultVMIPPollInterval = 500 * time.Millisecond
)

// networkEnabled reports whether the Pod networking dependency is wired and thus
// usable. Both the allocator and the host path are required: a Pod IP with no
// binat rule (or vice versa) would not produce a reachable Pod, so reporting
// NetworkReady in that half-configured state would be dishonest.
func (s *Server) networkEnabled() bool {
	return s.ipam != nil && s.podNet != nil
}

// sandboxKey is the IPAM/podnet key for a sandbox: its Kubernetes Pod identity
// ("namespace/name"), matching the provider's podKey. Keying by Pod identity (not
// the per-attempt sandbox ID) keeps a Pod's IP stable when its sandbox is
// recreated, exactly as the provider does. CRI-P5 keeps one sandbox per Pod key.
func sandboxKey(sb *store.Sandbox) string {
	return sb.Metadata.Namespace + "/" + sb.Metadata.Name
}

// sandboxByKey finds an existing sandbox for a Kubernetes Pod key. The CRI-P5
// model keeps one sandbox per Pod key because IPAM and podnet rules are keyed by
// that identity, not by sandbox attempt ID.
func (s *Server) sandboxByKey(key string) (store.Sandbox, bool) {
	for _, sb := range s.sandboxes.List() {
		if sandboxKey(&sb) == key {
			return sb, true
		}
	}
	return store.Sandbox{}, false
}

// allocateSandboxIP reserves a Pod IP for a new sandbox. It returns the assigned
// IP and whether the key already held one before this call (a pre-existing
// reservation comes from restart recovery or a not-yet-removed prior sandbox and
// must not be released on a later failure). It is a no-op returning "" when IPAM
// is disabled.
func (s *Server) allocateSandboxIP(key string) (ip string, had bool, err error) {
	if s.ipam == nil {
		return "", false, nil
	}
	had = s.ipam.IP(key) != ""
	ip, err = s.ipam.Allocate(key)
	if err != nil {
		return "", had, status.Errorf(codes.ResourceExhausted, "allocate pod IP for %q: %v", key, err)
	}
	return ip, had, nil
}

// attachSandboxNetwork wires a started container's micro-VM into the Pod network
// path. It observes the VM's host-only address, programs the binat rule via the
// Router, and records the attachment on the sandbox so PodSandboxStatus can report
// the Pod IP honestly. A missing VM IP is transient (the guest is still acquiring
// DHCP) and surfaces as Unavailable so the caller can unwind and retry.
func (s *Server) attachSandboxNetwork(ctx context.Context, sb *store.Sandbox, workloadID string) error {
	key := sandboxKey(sb)
	vmIP := s.observeVMIP(ctx, workloadID)
	if vmIP == "" {
		return status.Errorf(codes.Unavailable,
			"pod %q: micro-VM address not available yet for network attach", key)
	}
	ep := podnet.Endpoint{PodKey: key, PodIP: sb.Network.PodIP, VMIP: vmIP}
	if err := s.podNet.Attach(ctx, ep); err != nil {
		return status.Errorf(codes.Internal,
			"attach pod %q network path (%s -> %s): %v", key, sb.Network.PodIP, vmIP, err)
	}
	if attached, ok := s.attachedPodNetworkEndpoint(key); ok {
		ep = attached
	}
	sb.Network.VMIP = vmIP
	sb.Network.Interface = ep.Interface
	sb.Network.Attached = true
	if err := s.sandboxes.Put(sb); err != nil {
		// The binat rule is live but the record did not persist: detach so we do
		// not leak host state behind a sandbox that will look unattached.
		if derr := s.podNet.Detach(context.WithoutCancel(ctx), key); derr != nil {
			klog.ErrorS(derr, "failed to detach after attach-persist error", "pod", key)
		}
		return status.Errorf(codes.Internal, "persist sandbox %q network state: %v", sb.ID, err)
	}
	klog.V(4).InfoS("CRI attached pod network", "pod", key, "podIP", sb.Network.PodIP, "vmIP", vmIP)
	return nil
}

// detachSandboxNetwork removes a sandbox's Pod network path and clears its
// attachment record. It is idempotent: detaching a sandbox that was never attached
// (or is already detached) is a no-op, matching the CRI stop/remove contract. The
// Pod IP reservation is retained — it is released only at RemovePodSandbox.
func (s *Server) detachSandboxNetwork(ctx context.Context, sandboxID string) error {
	sb, ok := s.sandboxes.Get(sandboxID)
	if !ok {
		return nil
	}
	if s.podNet != nil {
		if err := s.podNet.Detach(ctx, sandboxKey(&sb)); err != nil {
			return status.Errorf(codes.Internal, "detach pod %q network path: %v", sandboxKey(&sb), err)
		}
	}
	if sb.Network.Attached || sb.Network.VMIP != "" {
		sb.Network.Attached = false
		sb.Network.VMIP = ""
		sb.Network.Interface = ""
		if err := s.sandboxes.Put(&sb); err != nil {
			return status.Errorf(codes.Internal, "persist sandbox %q network state: %v", sandboxID, err)
		}
	}
	return nil
}

// releaseSandboxIP returns a sandbox's Pod IP to the pool. It is a no-op when IPAM
// is disabled or the sandbox holds no IP, so RemovePodSandbox stays idempotent.
func (s *Server) releaseSandboxIP(sb *store.Sandbox) {
	if s.ipam == nil || sb.Network.PodIP == "" {
		return
	}
	key := sandboxKey(sb)
	for _, other := range s.sandboxes.List() {
		if other.ID != sb.ID && sandboxKey(&other) == key {
			klog.InfoS("retaining CRI Pod IP reservation for remaining sandbox with same Pod key",
				"pod", key, "removedSandbox", sb.ID, "remainingSandbox", other.ID)
			return
		}
	}
	if s.ipam.IP(key) == sb.Network.PodIP {
		s.ipam.Release(key)
	}
}

// unwindContainerStart reverses a container start whose Pod network attach failed.
// It stops the workload and records the container as Exited with a clear reason,
// so the container is never left Running behind an unreachable Pod IP. It is
// best-effort: failures are logged, not returned, because the caller already has
// the original attach error to surface.
func (s *Server) unwindContainerStart(ctx context.Context, c *store.Container) {
	s.unwindContainerStartReason(ctx, c, "NetworkSetupFailed",
		"pod network attach failed during container start")
}

// unwindContainerStartReason stops a workload whose start could not complete and
// records it as Exited with the given reason/message, so a half-started container
// is never left Running. It is best-effort on both the stop and the persist: a
// failure to persist the Exited state (e.g. the store is unwritable) is logged,
// not returned, because the workload has already been stopped — the important
// cleanup — and the caller is already returning the original start error.
func (s *Server) unwindContainerStartReason(ctx context.Context, c *store.Container, reason, message string) {
	if err := s.containerRuntime.Stop(context.WithoutCancel(ctx), c.WorkloadID, defaultStopTimeout); err != nil {
		klog.ErrorS(err, "StartContainer: failed to stop workload after start failure",
			"containerID", c.ID, "workloadID", c.WorkloadID, "reason", reason)
	}
	c.State = store.ContainerExited
	c.FinishedAt = s.now().UnixNano()
	c.Reason = reason
	c.Message = message
	if err := s.containers.Put(c); err != nil {
		klog.ErrorS(err, "StartContainer: failed to persist exited state after start failure",
			"containerID", c.ID, "reason", reason)
	}
}

// observeVMIP polls the runtime for a workload's host-only address. It returns ""
// if the address never appears within the poll budget or the context is cancelled.
func (s *Server) observeVMIP(ctx context.Context, workloadID string) string {
	for attempt := 0; attempt < s.vmIPPollAttempts; attempt++ {
		if st, err := s.containerRuntime.Status(ctx, workloadID); err == nil && st.IP != "" {
			return st.IP
		}
		if attempt == s.vmIPPollAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(s.vmIPPollInterval):
		}
	}
	return ""
}

// RecoverNetwork rebuilds Pod networking state from persisted sandboxes so a
// restarted adapter neither leaks Pod IP allocations nor wipes other Pods' host
// rules. It re-reserves each sandbox's recorded Pod IP and re-attaches every
// sandbox that was attached before the restart (the Router's in-memory ruleset is
// empty on a fresh process, so the next Attach/Detach would otherwise drop the
// surviving binat rules). It is best-effort: a per-sandbox failure is logged and
// skipped rather than failing adapter startup. Call it once at startup, after the
// stores load and the Router has been Started.
func (s *Server) RecoverNetwork(ctx context.Context) {
	if s.ipam == nil && s.podNet == nil {
		return
	}
	reserved, reattached := 0, 0
	for _, sb := range s.sandboxes.List() {
		key := sandboxKey(&sb)
		if s.ipam != nil && sb.Network.PodIP != "" {
			if err := s.ipam.Reserve(key, sb.Network.PodIP); err != nil {
				klog.ErrorS(err, "skipping CRI Pod IP recovery", "pod", key, "ip", sb.Network.PodIP)
			} else {
				reserved++
			}
		}
		if s.podNet != nil && sb.Network.Attached && sb.Network.PodIP != "" && sb.Network.VMIP != "" {
			ep := podnet.Endpoint{PodKey: key, PodIP: sb.Network.PodIP, VMIP: sb.Network.VMIP, Interface: sb.Network.Interface}
			if err := s.podNet.Attach(ctx, ep); err != nil {
				klog.ErrorS(err, "failed to re-attach CRI Pod network after restart", "pod", key)
			} else {
				reattached++
			}
		}
	}
	if reserved > 0 || reattached > 0 {
		klog.InfoS("recovered CRI Pod networking state", "reservedIPs", reserved, "reattached", reattached)
	}
}

func (s *Server) attachedPodNetworkEndpoint(key string) (podnet.Endpoint, bool) {
	lister, ok := s.podNet.(podNetworkEndpointLister)
	if !ok {
		return podnet.Endpoint{}, false
	}
	for _, ep := range lister.Endpoints() {
		if ep.PodKey == key {
			return ep, true
		}
	}
	return podnet.Endpoint{}, false
}

// networkInfo returns adapter-level network detail for verbose Status/PodSandboxStatus.
func (s *Server) networkInfo() string {
	if !s.networkEnabled() {
		return "disabled"
	}
	return fmt.Sprintf("macvz-podnet (cidr=%s)", s.ipam.CIDR())
}
