package config

import (
	"fmt"
	"net"
	"strings"

	"github.com/chimerakang/macvz/pkg/network/podnet"
)

// DefaultVMNetCIDR is apple/container's usual host-only vmnet range. Operators
// with a different vmnet range should set podNetwork.vmNetCIDRs explicitly.
const DefaultVMNetCIDR = "192.168.64.0/22"

// PodNetworkConfig configures the host Pod network path that makes each
// micro-VM reachable at its assigned Pod IP across the mesh (#22). It is opt-in:
// when disabled, Pods keep apple/container's host-only address and cross-host
// routing is unavailable.
type PodNetworkConfig struct {
	// Enabled turns the host Pod network path on. It is only meaningful together
	// with an enabled mesh and Kubernetes-assigned Pod CIDR.
	Enabled bool `yaml:"enabled"`

	// Interface is the host vmnet interface apple/container micro-VMs attach to
	// (e.g. "bridge100"). The binat rules are scoped to it.
	Interface string `yaml:"interface"`

	// IngressInterfaces are extra host interfaces where Pod-IP traffic may arrive
	// outside the WireGuard mesh, such as a local test bridge. They only add
	// Pod binat rules; they do not grant route/default-route control.
	IngressInterfaces []string `yaml:"ingressInterfaces"`

	// Anchor is the pf anchor MacVz manages. Defaults to podnet.DefaultAnchor.
	// The operator must reference this anchor from the main pf.conf.
	Anchor string `yaml:"anchor"`

	// EnableForwarding turns on IPv4 forwarding so the host routes between the
	// mesh interface and the vmnet interface. Defaults to true when enabled;
	// set false only when forwarding is managed externally.
	EnableForwarding *bool `yaml:"enableForwarding"`

	// VMNetCIDRs are the host-only apple/container address ranges that local
	// micro-VMs may receive on the vmnet interface. The privileged helper uses
	// these to reject pf rules that redirect ClusterIP traffic outside the local
	// vmnet and Pod networks. Defaults to 192.168.64.0/22 when omitted.
	VMNetCIDRs []string `yaml:"vmNetCIDRs"`
}

// validatePodNetwork checks the Pod network fields when enabled.
func (c Config) validatePodNetwork() error {
	pn := c.PodNetwork
	if !pn.Enabled {
		return nil
	}
	if pn.Interface == "" {
		return fmt.Errorf("podNetwork.interface is required when the Pod network is enabled")
	}
	for _, cidr := range pn.VMNetCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("podNetwork.vmNetCIDRs entry %q is not a CIDR: %w", cidr, err)
		}
	}
	for _, iface := range pn.IngressInterfaces {
		if iface == "" || iface != strings.TrimSpace(iface) {
			return fmt.Errorf("podNetwork.ingressInterfaces entry %q is not a valid interface name", iface)
		}
	}
	return nil
}

// PodNetworkRouterConfig resolves the YAML Pod network config into a
// podnet.Config. Call only when PodNetwork.Enabled is true.
func (c Config) PodNetworkRouterConfig() podnet.Config {
	pn := c.PodNetwork
	forwarding := true
	if pn.EnableForwarding != nil {
		forwarding = *pn.EnableForwarding
	}
	return podnet.Config{
		Interface:         pn.Interface,
		MeshInterface:     c.Mesh.Interface,
		IngressInterfaces: pn.IngressInterfaces,
		Anchor:            pn.Anchor,
		EnableForwarding:  forwarding,
	}
}

// RoutableServiceCIDRs returns the address ranges a MacVz node can DNAT a
// ClusterIP Service to: this node's own Pod CIDR, the local apple/container
// vmnet ranges, and every enabled mesh-peer Pod CIDR. The service-route
// controller uses it to drop Service backends MacVz cannot reach — most
// importantly the host-network kube-apiserver behind the always-present
// default/kubernetes Service, whose redirect target the privileged helper
// rejects and which would otherwise fail the whole pf anchor load and block Pod
// attachment. Empty entries are omitted; the result may be empty (no filtering)
// when none are set.
func (c Config) RoutableServiceCIDRs() []string {
	cidrs := make([]string, 0, 1+len(c.PodNetwork.VMNetCIDRs)+len(c.Mesh.Peers))
	if c.Node.PodCIDR != "" {
		cidrs = append(cidrs, c.Node.PodCIDR)
	}
	vmnet := c.PodNetwork.VMNetCIDRs
	if c.PodNetwork.Enabled && len(vmnet) == 0 {
		vmnet = []string{DefaultVMNetCIDR}
	}
	cidrs = append(cidrs, vmnet...)
	if c.Mesh.Enabled {
		for _, p := range c.Mesh.Peers {
			if p.PodCIDR != "" {
				cidrs = append(cidrs, p.PodCIDR)
			}
		}
	}
	return cidrs
}
