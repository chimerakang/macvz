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

// linuxpod_network.go integrates MacVz Pod networking into the LinuxPod-backed CRI
// service (CRI-L3, #128). It reuses the same primitives the apple/container path
// uses (network.go): Pod IPs come from the node's PodIPAM (reached through
// PodIPAllocator) and the host pf/route path comes from podnet.Router (reached
// through PodNetwork). It does not change the Mac host default route — that
// guarantee lives in podnet.Router, which only ever removes apple/container's
// scoped (-ifscope) vmnet default route, never the global default.
//
// The LinuxPod model attaches earlier than the apple/container model. A LinuxPod
// sandbox is a Pod VM that boots at RunPodSandbox time, so its host-reachable
// address is discovered from the LinuxPod backend (Backend.PodStatus) and the host
// binat path is attached as soon as the Pod VM has an address — before any
// container starts. PodSandboxStatus then reports the Pod IP through the shared
// toCRIStatus, which surfaces it only once the attach is actually recorded, so the
// status never claims a reachable Pod the host cannot deliver.
//
// IPAM keys by Pod identity (namespace/name via sandboxKey), matching the
// apple/container path and the provider, so a Pod's IP is stable across sandbox
// recreation. Detach, IP release, and restart recovery live here too; they are the
// LinuxPod counterparts of the network.go helpers.

// LinuxPodNetworkFailure classifies why LinuxPod Pod network attach failed, so the
// adapter reports an honest, actionable diagnostic instead of a flat error and the
// gRPC layer maps each class to the right CRI status code.
type LinuxPodNetworkFailure string

const (
	// LinuxPodNetHelper means the LinuxPod backend/helper call failed (unreachable,
	// protocol error, or unknown pod). The Pod VM address could not be queried.
	LinuxPodNetHelper LinuxPodNetworkFailure = "helper"
	// LinuxPodNetIPReservation means a Pod IP could not be reserved from the node's
	// Pod CIDR (IPAM exhausted or an unwritable store).
	LinuxPodNetIPReservation LinuxPodNetworkFailure = "ip-reservation"
	// LinuxPodNetAddressDiscovery means the LinuxPod backend answered, but the Pod VM
	// never reported a host-reachable address within the poll budget.
	LinuxPodNetAddressDiscovery LinuxPodNetworkFailure = "address-discovery"
	// LinuxPodNetRoutePF means the host route/pf path (podnet.Router) failed to
	// program the binat rule for the discovered address.
	LinuxPodNetRoutePF LinuxPodNetworkFailure = "route-pf"
)

// LinuxPodNetworkError is a classified LinuxPod Pod network attach failure. It
// implements GRPCStatus so returning it from a CRI handler yields the right status
// code automatically, while callers and tests branch on Class via errors.As.
type LinuxPodNetworkError struct {
	Class LinuxPodNetworkFailure
	Err   error
}

func (e *LinuxPodNetworkError) Error() string {
	return fmt.Sprintf("linuxpod pod network %s failure: %v", e.Class, e.Err)
}

func (e *LinuxPodNetworkError) Unwrap() error { return e.Err }

// code maps a failure class to the CRI/gRPC status code kubelet sees. Transient
// classes (helper unreachable, address not yet discovered) are Unavailable so
// kubelet retries; IP exhaustion is ResourceExhausted; a host route/pf failure is
// Internal.
func (e *LinuxPodNetworkError) code() codes.Code {
	switch e.Class {
	case LinuxPodNetIPReservation:
		return codes.ResourceExhausted
	case LinuxPodNetHelper, LinuxPodNetAddressDiscovery:
		return codes.Unavailable
	case LinuxPodNetRoutePF:
		return codes.Internal
	default:
		return codes.Internal
	}
}

// GRPCStatus lets google.golang.org/grpc/status recognize this error's code.
func (e *LinuxPodNetworkError) GRPCStatus() *status.Status {
	return status.New(e.code(), e.Error())
}

func linuxPodNetFailure(class LinuxPodNetworkFailure, err error) error {
	return &LinuxPodNetworkError{Class: class, Err: err}
}

// networkEnabled reports whether the LinuxPod Pod network path is fully wired: the
// host pf/route path (PodNetwork) and the Pod IPAM. When false, a LinuxPod sandbox
// runs without a Pod IP and reports NetworkReady=false honestly, exactly like the
// apple/container path with networking off.
func (s *LinuxPodService) networkEnabled() bool {
	return s.podNet != nil && s.ipam != nil
}

// ensureSandboxNetwork wires a LinuxPod-backed sandbox into the Pod network path so
// kubelet sees its Pod IP and NetworkReady. It is idempotent: a sandbox already
// attached is a no-op success, so a kubelet RunPodSandbox retry re-affirms state
// without reprogramming a duplicate rule. It reserves a stable Pod IP (keyed by Pod
// identity), discovers the Pod VM's host-reachable address from the backend,
// programs the host binat rule, and records the attachment. It is a no-op success
// when the Pod network path is not wired. Every failure is a classified
// *LinuxPodNetworkError. Callers hold s.mu.
func (s *LinuxPodService) ensureSandboxNetwork(ctx context.Context, sandboxID string) error {
	if !s.networkEnabled() {
		return nil
	}
	sb, ok := s.sandboxes.Get(sandboxID)
	if !ok {
		return status.Errorf(codes.NotFound, "attach LinuxPod network: sandbox %q not found", sandboxID)
	}
	if sb.Network.Attached {
		return nil
	}
	key := sandboxKey(&sb)

	// Reserve a Pod IP if the sandbox does not already hold one. Persist before
	// attaching so a Pod IP is never programmed into the host path without a durable
	// reservation behind it.
	if sb.Network.PodIP == "" {
		had := s.ipam.IP(key) != ""
		ip, err := s.ipam.Allocate(key)
		if err != nil {
			return linuxPodNetFailure(LinuxPodNetIPReservation,
				fmt.Errorf("allocate pod IP for %q: %w", key, err))
		}
		sb.Network.PodIP = ip
		if err := s.sandboxes.Put(&sb); err != nil {
			if !had && s.ipam.IP(key) == ip {
				s.ipam.Release(key)
			}
			return linuxPodNetFailure(LinuxPodNetIPReservation,
				fmt.Errorf("persist reserved pod IP for %q: %w", key, err))
		}
	}

	// Discover the Pod VM's host-reachable address. A backend error is a helper
	// failure; an address that never appears within the budget is an
	// address-discovery failure. Either way the Pod IP reservation is retained.
	addr, err := s.discoverSandboxAddress(ctx, sandboxID)
	if err != nil {
		return err
	}

	ep := podnet.Endpoint{PodKey: key, PodIP: sb.Network.PodIP, VMIP: addr}
	if err := s.podNet.Attach(ctx, ep); err != nil {
		return linuxPodNetFailure(LinuxPodNetRoutePF,
			fmt.Errorf("attach pod %q host path (%s -> %s): %w", key, sb.Network.PodIP, addr, err))
	}
	if resolved, ok := attachedPodNetEndpoint(s.podNet, key); ok {
		ep = resolved
	}
	sb.Network.VMIP = addr
	sb.Network.Interface = ep.Interface
	sb.Network.Attached = true
	if err := s.sandboxes.Put(&sb); err != nil {
		// The binat rule is live but the record did not persist: detach so we do not
		// leak host state behind a sandbox that will look unattached.
		if derr := s.podNet.Detach(context.WithoutCancel(ctx), key); derr != nil {
			klog.ErrorS(derr, "failed to detach after LinuxPod attach-persist error", "pod", key)
		}
		return linuxPodNetFailure(LinuxPodNetRoutePF,
			fmt.Errorf("persist sandbox %q network state: %w", sb.ID, err))
	}
	klog.V(4).InfoS("CRI(LinuxPod) attached pod network",
		"pod", key, "podIP", sb.Network.PodIP, "sandboxAddr", addr, "interface", sb.Network.Interface)
	return nil
}

// discoverSandboxAddress polls the LinuxPod backend for a Pod VM's host-reachable
// address. A backend error surfaces immediately as a helper failure (distinct from
// "not ready yet"); an address that never appears surfaces as an address-discovery
// failure. The bounds are addrPoll* so tests stay fast.
func (s *LinuxPodService) discoverSandboxAddress(ctx context.Context, podID string) (string, error) {
	for attempt := 0; attempt < s.addrPollAttempts; attempt++ {
		st, err := s.backend.PodStatus(ctx, podID)
		if err != nil {
			return "", linuxPodNetFailure(LinuxPodNetHelper,
				fmt.Errorf("query LinuxPod sandbox %q address: %w", podID, err))
		}
		if st.SandboxAddress != "" {
			return st.SandboxAddress, nil
		}
		if attempt == s.addrPollAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return "", linuxPodNetFailure(LinuxPodNetAddressDiscovery,
				fmt.Errorf("pod %q: %w", podID, ctx.Err()))
		case <-time.After(s.addrPollInterval):
		}
	}
	return "", linuxPodNetFailure(LinuxPodNetAddressDiscovery,
		fmt.Errorf("pod %q: LinuxPod sandbox address not available within poll budget", podID))
}

// detachSandboxNetwork removes a sandbox's host pf/route path and clears its
// attachment record. It is idempotent: detaching a never-attached (or
// already-detached) sandbox is a no-op, matching the CRI stop/remove contract. The
// Pod IP reservation is retained — it is released only at RemovePodSandbox. Callers
// hold s.mu.
func (s *LinuxPodService) detachSandboxNetwork(ctx context.Context, sandboxID string) error {
	if s.podNet == nil {
		return nil
	}
	sb, ok := s.sandboxes.Get(sandboxID)
	if !ok {
		return nil
	}
	if err := s.podNet.Detach(ctx, sandboxKey(&sb)); err != nil {
		return status.Errorf(codes.Internal, "detach pod %q network path: %v", sandboxKey(&sb), err)
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
// is off or the sandbox holds no IP, so RemovePodSandbox stays idempotent. Callers
// hold s.mu.
func (s *LinuxPodService) releaseSandboxIP(sb *store.Sandbox) {
	if s.ipam == nil || sb.Network.PodIP == "" {
		return
	}
	key := sandboxKey(sb)
	if s.ipam.IP(key) == sb.Network.PodIP {
		s.ipam.Release(key)
	}
}

// RecoverNetwork rebuilds Pod networking state from persisted sandboxes so a
// restarted adapter neither leaks Pod IP allocations nor wipes other Pods' host
// rules. It re-reserves each sandbox's recorded Pod IP and re-attaches every
// sandbox that was attached before the restart (the Router's in-memory ruleset is
// empty on a fresh process, so the next Attach/Detach would otherwise drop the
// surviving binat rules). It is best-effort: a per-sandbox failure is logged and
// skipped rather than failing adapter startup. Call it once at startup, after the
// stores load and the Router has been Started.
func (s *LinuxPodService) RecoverNetwork(ctx context.Context) {
	if !s.networkEnabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reserved, reattached := 0, 0
	for _, sb := range s.sandboxes.List() {
		key := sandboxKey(&sb)
		if sb.Network.PodIP != "" {
			if err := s.ipam.Reserve(key, sb.Network.PodIP); err != nil {
				klog.ErrorS(err, "skipping LinuxPod Pod IP recovery", "pod", key, "ip", sb.Network.PodIP)
			} else {
				reserved++
			}
		}
		if sb.Network.Attached && sb.Network.PodIP != "" && sb.Network.VMIP != "" {
			ep := podnet.Endpoint{PodKey: key, PodIP: sb.Network.PodIP, VMIP: sb.Network.VMIP, Interface: sb.Network.Interface}
			if err := s.podNet.Attach(ctx, ep); err != nil {
				klog.ErrorS(err, "failed to re-attach LinuxPod Pod network after restart", "pod", key)
			} else {
				reattached++
			}
		}
	}
	if reserved > 0 || reattached > 0 {
		klog.InfoS("recovered LinuxPod CRI Pod networking state", "reservedIPs", reserved, "reattached", reattached)
	}
}

// attachedPodNetEndpoint returns the Router-resolved endpoint for a Pod key (with
// the concrete vmnet Interface the Router chose), when the PodNetwork exposes its
// endpoints. It lets the attach path persist the resolved Interface for recovery.
func attachedPodNetEndpoint(pn PodNetwork, key string) (podnet.Endpoint, bool) {
	lister, ok := pn.(podNetworkEndpointLister)
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
