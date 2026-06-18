// Package podnet connects each apple/container micro-VM to the MacVz-controlled
// Pod network path (issue #22, milestone P3), so a Pod is reachable at its
// assigned MacVz Pod IP (pkg/network IPAM, #20) and can route across the
// WireGuard mesh (pkg/network/wireguard, #21).
//
// # The apple/container constraint
//
// apple/container attaches each micro-VM to a vmnet-backed network and assigns
// it a host-only address (e.g. 192.168.64.x) over DHCP. The CLI does not let us
// push an arbitrary guest IP, so the micro-VM cannot natively own its Kubernetes
// Pod IP. MacVz therefore bridges the gap on the host.
//
// # Chosen MVP path: host-side 1:1 NAT (pf binat)
//
// For each Pod, MacVz installs a packet-filter binat rule that maps the Pod's
// assigned Pod IP to the micro-VM's host-only address bidirectionally:
//
//		binat on <iface> from <vmIP> to any -> <podIP>
//
//	  - Inbound: packets arriving for the Pod IP (delivered to this Mac by the
//	    mesh, which routes the node's Pod CIDR here) are DNAT'd to the VM address
//	    and forwarded to the vmnet interface.
//	  - Outbound: packets the VM sends are SNAT'd so they appear to originate from
//	    the Pod IP; the route for a remote Pod CIDR (installed by the mesh) sends
//	    them into the WireGuard tunnel.
//
// IP forwarding is enabled so the host routes between the mesh interface and the
// vmnet interface. The result: a Pod on one Mac reaches a Pod IP hosted on
// another Mac at L3, and every Pod is addressed by its MacVz Pod IP rather than
// an opaque host-only address.
//
// The Router owns a single pf anchor whose ruleset is regenerated wholesale on
// every Attach/Detach (pf anchors load atomically). All command execution goes
// through the runner interface, so the rule generation and lifecycle are
// unit-tested against a fake without touching the host's packet filter.
//
// An alternative fully-userspace path (gvisor-tap-vsock over a vsock/file-handle
// attachment, terminating the guest's traffic in a userspace network stack) is
// evaluated in docs/NETWORKING.md; it avoids pf and root but requires a guest
// agent and is deferred past the MVP.
package podnet
