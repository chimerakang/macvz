package config

import (
	"net"

	"github.com/chimerakang/macvz/pkg/network/podnet"
	"github.com/chimerakang/macvz/pkg/network/privhelper"
)

// PrivilegedHelperPolicy derives the privhelper.Policy that confines the root
// network helper to exactly the resources this node's config references (#41):
// its mesh interface, mesh address, vmnet interface, pf anchor, the Pod/mesh
// CIDRs routed through the tunnel, and the configured peers' public keys.
//
// It draws only on the mesh and Pod-network stanzas, so it is safe to call even
// when those are disabled — the result simply has empty sets and refuses the
// corresponding commands (fail closed). Errors surface only from resolving the
// mesh interface config (e.g. an unreadable/invalid private key file), which the
// helper needs to canonicalise peer keys and route targets.
func (c Config) PrivilegedHelperPolicy() (privhelper.Policy, error) {
	p := privhelper.Policy{
		VMNetInterface: c.PodNetwork.Interface,
		Anchor:         podnet.DefaultAnchor,
		RouteCIDRs:     map[string]bool{},
		PeerPublicKeys: map[string]bool{},
	}
	if c.PodNetwork.Anchor != "" {
		p.Anchor = c.PodNetwork.Anchor
	}

	if !c.Mesh.Enabled {
		return p, nil
	}

	ifc, err := c.MeshInterfaceConfig()
	if err != nil {
		return privhelper.Policy{}, err
	}
	p.MeshInterface = ifc.Name
	p.MTU = ifc.MTU
	if ip, _, err := net.ParseCIDR(ifc.Address); err == nil {
		p.MeshAddressIP = ip.String()
	}
	// Route targets and AllowedIPs are confined to the union of every peer's
	// AllowedIPs (Pod CIDR + mesh address), exactly what the mesh installs routes
	// for and what its wg config advertises.
	p.RouteCIDRs = privhelper.NormalizeCIDRSet(ifc.RouteTargets())
	for _, peer := range ifc.Peers {
		p.PeerPublicKeys[peer.PublicKey.String()] = true
	}
	return p, nil
}
