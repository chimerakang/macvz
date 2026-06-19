package health

import (
	"context"
	"fmt"
	"time"
)

// The checkers below each probe one subsystem and translate its observed state
// into a Check. They depend on narrow, locally-defined interfaces (not the
// concrete runtime/privhelper/mesh types) so the health package stays
// dependency-light and every checker is unit-testable with a fake. Adapters that
// bind these interfaces to the live components live in the kubelet command.

// --- runtime -----------------------------------------------------------------

// RuntimeProbe reports whether the apple/container host runtime is reachable and
// healthy. It is satisfied structurally by runtime.Pinger.
type RuntimeProbe interface {
	// Ready returns nil when the runtime is reachable and healthy, else an error.
	Ready(ctx context.Context) error
}

// runtimeChecker reports apple/container runtime readiness.
type runtimeChecker struct {
	probe RuntimeProbe
}

// NewRuntimeChecker builds a checker over the apple/container runtime. A nil
// probe yields a checker that reports the runtime readiness probe is
// unavailable (skipped), mirroring the provider's assume-ready fallback.
func NewRuntimeChecker(probe RuntimeProbe) Checker {
	return runtimeChecker{probe: probe}
}

func (c runtimeChecker) Check(ctx context.Context) Check {
	const name = "container-runtime"
	if c.probe == nil {
		return Check{
			Name: name, Class: ClassRuntime, Status: StatusSkipped,
			Summary: "runtime readiness probe unavailable; assuming ready",
		}
	}
	if err := c.probe.Ready(ctx); err != nil {
		return Check{
			Name: name, Class: ClassRuntime, Status: StatusFail,
			Summary: "apple/container runtime is not ready",
			Detail:  err.Error(),
			Hint:    "ensure the `container` CLI is installed and run `container system start`",
		}
	}
	return Check{
		Name: name, Class: ClassRuntime, Status: StatusPass,
		Summary: "apple/container runtime is reachable and healthy",
	}
}

// --- control plane -----------------------------------------------------------

// NodeState is the kubelet's registration and Ready-condition view from the API
// server.
type NodeState struct {
	// Registered reports whether the Node object exists in the API server.
	Registered bool
	// Ready is the value of the node's Ready condition.
	Ready bool
	// Reason carries the Ready condition's reason/message for diagnostics.
	Reason string
}

// LeaseState is the kubelet's node-lease heartbeat freshness.
type LeaseState struct {
	// Enabled reports whether lease-based heartbeat is configured on this node.
	Enabled bool
	// Found reports whether the Lease object exists.
	Found bool
	// Age is the time since the lease was last renewed.
	Age time.Duration
	// Stale is the threshold past which Age is considered unhealthy (the lease
	// duration). Zero disables the staleness check.
	Stale time.Duration
}

// ControlPlaneProbe reads this node's registration and lease state from the API
// server. Adapters wrap a Kubernetes clientset to satisfy it.
type ControlPlaneProbe interface {
	NodeState(ctx context.Context) (NodeState, error)
	LeaseState(ctx context.Context) (LeaseState, error)
}

// nodeRegistrationChecker reports whether the node is registered and Ready.
type nodeRegistrationChecker struct {
	probe ControlPlaneProbe
}

// nodeLeaseChecker reports whether the node-lease heartbeat is fresh.
type nodeLeaseChecker struct {
	probe ControlPlaneProbe
}

// NewControlPlaneCheckers builds the registration and lease checkers over a
// single control-plane probe. A nil probe yields checkers that report the
// control plane could not be inspected (e.g. the API client was never built),
// which is itself a failure — a node that cannot reach its API server is not
// ready for workloads.
func NewControlPlaneCheckers(probe ControlPlaneProbe) []Checker {
	return []Checker{
		nodeRegistrationChecker{probe: probe},
		nodeLeaseChecker{probe: probe},
	}
}

func (c nodeRegistrationChecker) Check(ctx context.Context) Check {
	const name = "kubelet-registration"
	if c.probe == nil {
		return Check{
			Name: name, Class: ClassControlPlane, Status: StatusFail,
			Summary: "control plane not inspected; API client unavailable",
			Hint:    "verify kubeconfig and API server reachability",
		}
	}
	st, err := c.probe.NodeState(ctx)
	if err != nil {
		return Check{
			Name: name, Class: ClassControlPlane, Status: StatusFail,
			Summary: "could not query node registration from the API server",
			Detail:  err.Error(),
			Hint:    "verify kubeconfig and API server reachability",
		}
	}
	if !st.Registered {
		return Check{
			Name: name, Class: ClassControlPlane, Status: StatusFail,
			Summary: "node is not registered with the API server",
			Hint:    "check that the node controller started and the kubeconfig is valid",
		}
	}
	if !st.Ready {
		return Check{
			Name: name, Class: ClassControlPlane, Status: StatusFail,
			Summary: "node is registered but its Ready condition is not True",
			Detail:  st.Reason,
		}
	}
	return Check{
		Name: name, Class: ClassControlPlane, Status: StatusPass,
		Summary: "node is registered and Ready",
	}
}

func (c nodeLeaseChecker) Check(ctx context.Context) Check {
	const name = "node-lease"
	if c.probe == nil {
		return Check{
			Name: name, Class: ClassControlPlane, Status: StatusFail,
			Summary: "control plane not inspected; API client unavailable",
		}
	}
	st, err := c.probe.LeaseState(ctx)
	if err != nil {
		return Check{
			Name: name, Class: ClassControlPlane, Status: StatusFail,
			Summary: "could not query the node lease from the API server",
			Detail:  err.Error(),
		}
	}
	if !st.Enabled {
		return Check{
			Name: name, Class: ClassControlPlane, Status: StatusSkipped,
			Summary: "node-lease heartbeat is disabled",
		}
	}
	if !st.Found {
		return Check{
			Name: name, Class: ClassControlPlane, Status: StatusFail,
			Summary: "node-lease heartbeat is enabled but no Lease object was found",
			Hint:    "the node may not have completed registration",
		}
	}
	if st.Stale > 0 && st.Age > st.Stale {
		return Check{
			Name: name, Class: ClassControlPlane, Status: StatusFail,
			Summary: "node lease is stale; the heartbeat has stopped",
			Detail:  fmt.Sprintf("last renewed %s ago (lease duration %s)", st.Age.Round(time.Second), st.Stale),
			Hint:    "the kubelet cannot reach the API server, or its status loop is wedged",
		}
	}
	return Check{
		Name: name, Class: ClassControlPlane, Status: StatusPass,
		Summary: "node lease heartbeat is fresh",
		Detail:  fmt.Sprintf("last renewed %s ago", st.Age.Round(time.Second)),
	}
}

// --- data plane: privileged helper -------------------------------------------

// HelperInfo is the privileged helper's self-report needed for diagnostics.
type HelperInfo struct {
	Version         string
	Protocol        int
	PolicyEnforced  bool
	AllowedCommands []string
}

// HelperProbe reaches the privileged network helper (macvz-netd). Adapters wrap
// a privhelper.Client to satisfy it.
type HelperProbe interface {
	// Status returns the helper's self-report (also exercises protocol negotiation).
	Status(ctx context.Context) (HelperInfo, error)
	// Ping verifies the helper can actually execute commands, not just answer status.
	Ping(ctx context.Context) error
}

type helperChecker struct {
	enabled bool
	probe   HelperProbe
}

// NewHelperChecker builds a checker over the privileged network helper. When
// enabled is false (no mesh or pod network configured, so no helper is needed),
// the check is skipped. A helper that is required but unreachable, refusing
// commands, or running without policy enforcement is a data-plane failure: Pods
// cannot get cross-host networking.
func NewHelperChecker(enabled bool, probe HelperProbe) Checker {
	return helperChecker{enabled: enabled, probe: probe}
}

func (c helperChecker) Check(ctx context.Context) Check {
	const name = "privileged-helper"
	if !c.enabled {
		return Check{
			Name: name, Class: ClassDataPlane, Status: StatusSkipped,
			Summary: "privileged helper not required (mesh and pod network disabled)",
		}
	}
	if c.probe == nil {
		return Check{
			Name: name, Class: ClassDataPlane, Status: StatusFail,
			Summary: "privileged helper is required but not configured",
			Hint:    "set privilegedHelperSocket and run macvz-netd as root",
		}
	}
	st, err := c.probe.Status(ctx)
	if err != nil {
		return Check{
			Name: name, Class: ClassDataPlane, Status: StatusFail,
			Summary: "privileged helper is not reachable",
			Detail:  err.Error(),
			Hint:    "start macvz-netd as root and check the socket path",
		}
	}
	if !st.PolicyEnforced {
		return Check{
			Name: name, Class: ClassDataPlane, Status: StatusFail,
			Summary: "privileged helper is running without per-request policy validation",
			Hint:    "start macvz-netd with --config so it enforces a policy",
		}
	}
	if err := c.probe.Ping(ctx); err != nil {
		return Check{
			Name: name, Class: ClassDataPlane, Status: StatusFail,
			Summary: "privileged helper answered status but cannot run commands",
			Detail:  err.Error(),
			Hint:    "ensure macvz-netd is running as root with the required privileges",
		}
	}
	return Check{
		Name: name, Class: ClassDataPlane, Status: StatusPass,
		Summary: "privileged helper is reachable and enforcing policy",
		Detail: fmt.Sprintf("version %s, protocol %d, %d allowed commands",
			st.Version, st.Protocol, len(st.AllowedCommands)),
	}
}

// --- data plane: WireGuard mesh ----------------------------------------------

// MeshProbe reports the WireGuard mesh's configured interface, peers, and the
// host routes it installed. *wireguard.Mesh satisfies it structurally.
type MeshProbe interface {
	InterfaceName() string
	Peers() []string
	InstalledRoutes() []string
}

type meshChecker struct {
	enabled bool
	probe   MeshProbe
}

// NewMeshChecker builds a checker over the WireGuard mesh. When the mesh is
// disabled the check is skipped (Pods are reachable only on their local node, a
// valid single-host configuration). An enabled mesh with no peers is a warning,
// not a failure: a freshly-joined or single-node mesh is still functional.
func NewMeshChecker(enabled bool, probe MeshProbe) Checker {
	return meshChecker{enabled: enabled, probe: probe}
}

func (c meshChecker) Check(ctx context.Context) Check {
	const name = "wireguard-mesh"
	if !c.enabled || c.probe == nil {
		return Check{
			Name: name, Class: ClassDataPlane, Status: StatusSkipped,
			Summary: "WireGuard mesh disabled; Pods are reachable only on their local node",
		}
	}
	iface := c.probe.InterfaceName()
	peers := c.probe.Peers()
	routes := c.probe.InstalledRoutes()
	detail := fmt.Sprintf("interface %s, %d peers, %d routes", iface, len(peers), len(routes))

	if len(peers) == 0 {
		return Check{
			Name: name, Class: ClassDataPlane, Status: StatusWarn,
			Summary: "WireGuard mesh is up but has no peers",
			Detail:  detail,
			Hint:    "add peers to the config and `kill -HUP` the kubelet, or ignore on a single node",
		}
	}
	if len(routes) == 0 {
		return Check{
			Name: name, Class: ClassDataPlane, Status: StatusFail,
			Summary: "WireGuard mesh has peers but installed no host routes",
			Detail:  detail,
			Hint:    "check peer AllowedIPs and that the privileged helper can add routes",
		}
	}
	return Check{
		Name: name, Class: ClassDataPlane, Status: StatusPass,
		Summary: "WireGuard mesh is up with peers and routes",
		Detail:  detail,
	}
}

// --- data plane: IP forwarding -----------------------------------------------

// ForwardingProbe reports whether host IPv4 forwarding is enabled. Reading the
// sysctl does not require root, so the adapter can query it directly.
type ForwardingProbe interface {
	IPForwardingEnabled(ctx context.Context) (bool, error)
}

type forwardingChecker struct {
	enabled bool
	probe   ForwardingProbe
}

// NewForwardingChecker builds a checker for host IPv4 forwarding, which the Pod
// network path needs to route between the vmnet and mesh interfaces. It is
// skipped when the Pod network is disabled (forwarding is then irrelevant).
func NewForwardingChecker(enabled bool, probe ForwardingProbe) Checker {
	return forwardingChecker{enabled: enabled, probe: probe}
}

func (c forwardingChecker) Check(ctx context.Context) Check {
	const name = "ip-forwarding"
	if !c.enabled || c.probe == nil {
		return Check{
			Name: name, Class: ClassDataPlane, Status: StatusSkipped,
			Summary: "IP forwarding not required (pod network path disabled)",
		}
	}
	on, err := c.probe.IPForwardingEnabled(ctx)
	if err != nil {
		return Check{
			Name: name, Class: ClassDataPlane, Status: StatusFail,
			Summary: "could not read IP forwarding state",
			Detail:  err.Error(),
		}
	}
	if !on {
		return Check{
			Name: name, Class: ClassDataPlane, Status: StatusFail,
			Summary: "host IPv4 forwarding is disabled; Pod traffic will not route",
			Hint:    "the pod network path enables net.inet.ip.forwarding via the helper; check macvz-netd",
		}
	}
	return Check{
		Name: name, Class: ClassDataPlane, Status: StatusPass,
		Summary: "host IPv4 forwarding is enabled",
	}
}

// --- data plane: pod network attachments -------------------------------------

// AttachmentLister reports how many Pods are currently attached to the host Pod
// network path. *podnet.Router satisfies it via an adapter.
type AttachmentLister interface {
	AttachmentCount() int
}

type podNetworkChecker struct {
	enabled bool
	iface   string
	anchor  string
	lister  AttachmentLister
}

// NewPodNetworkChecker builds a checker for the host Pod network path: the pf
// anchor and vmnet interface it programs, and the count of attached Pods. It is
// skipped when the path is disabled. Zero attachments is normal (no Pods are
// scheduled yet), so it never fails on count alone; the check confirms the path
// is configured and reports the live attachment count for diagnostics.
func NewPodNetworkChecker(enabled bool, iface, anchor string, lister AttachmentLister) Checker {
	return podNetworkChecker{enabled: enabled, iface: iface, anchor: anchor, lister: lister}
}

func (c podNetworkChecker) Check(ctx context.Context) Check {
	const name = "pod-network"
	if !c.enabled || c.lister == nil {
		return Check{
			Name: name, Class: ClassDataPlane, Status: StatusSkipped,
			Summary: "pod network path disabled; Pods keep the runtime host-only address",
		}
	}
	count := c.lister.AttachmentCount()
	detail := fmt.Sprintf("interface %s, anchor %s, %d pod attachment(s)",
		c.iface, c.anchor, count)
	return Check{
		Name: name, Class: ClassDataPlane, Status: StatusPass,
		Summary: "pod network path is configured",
		Detail:  detail,
	}
}
