package wireguard

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// Peer is a remote MacVz node in the mesh.
type Peer struct {
	// Name is the node name. It is emitted as a comment for human-readable
	// `wg show` / config diffs and is not part of the WireGuard protocol.
	Name string
	// PublicKey identifies the peer cryptographically.
	PublicKey Key
	// Endpoint is the peer's reachable "host:port". It may be empty for a peer
	// behind NAT that initiates the connection itself.
	Endpoint string
	// AllowedIPs are the CIDRs routed to this peer — its Pod CIDR plus its
	// address on the mesh network.
	AllowedIPs []string
	// PersistentKeepalive, in seconds, keeps NAT mappings alive (0 disables it).
	PersistentKeepalive int
}

// InterfaceConfig is the desired state of this node's WireGuard interface.
type InterfaceConfig struct {
	// Name is the interface name (e.g. "utun7"). On macOS wg-quick assigns a
	// utun device; this is the name the Mesh manages and routes through.
	Name string
	// PrivateKey is this node's WireGuard private key.
	PrivateKey Key
	// Address is this node's address on the mesh network in CIDR form
	// (e.g. "10.99.0.1/32").
	Address string
	// ListenPort is the UDP port WireGuard listens on.
	ListenPort int
	// MTU optionally overrides the interface MTU (0 leaves the default).
	MTU int
	// Peers are the other nodes in the mesh.
	Peers []Peer
}

// Validate checks the interface config is internally consistent before it is
// rendered or applied.
func (c InterfaceConfig) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("wireguard: interface name is required")
	}
	if c.PrivateKey.IsZero() {
		return fmt.Errorf("wireguard: interface %q has no private key", c.Name)
	}
	if c.Address == "" {
		return fmt.Errorf("wireguard: interface %q has no mesh address", c.Name)
	}
	if _, _, err := net.ParseCIDR(c.Address); err != nil {
		return fmt.Errorf("wireguard: interface %q address %q is not a CIDR: %w", c.Name, c.Address, err)
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return fmt.Errorf("wireguard: interface %q listenPort %d out of range", c.Name, c.ListenPort)
	}
	if c.MTU < 0 {
		return fmt.Errorf("wireguard: interface %q mtu is negative", c.Name)
	}
	seen := map[Key]string{}
	for _, p := range c.Peers {
		if err := p.validate(); err != nil {
			return err
		}
		if other, dup := seen[p.PublicKey]; dup {
			return fmt.Errorf("wireguard: peers %q and %q share a public key", other, p.Name)
		}
		seen[p.PublicKey] = p.Name
	}
	return nil
}

func (p Peer) validate() error {
	if p.PublicKey.IsZero() {
		return fmt.Errorf("wireguard: peer %q has no public key", p.Name)
	}
	if len(p.AllowedIPs) == 0 {
		return fmt.Errorf("wireguard: peer %q has no allowedIPs", p.Name)
	}
	for _, cidr := range p.AllowedIPs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("wireguard: peer %q allowedIP %q is not a CIDR: %w", p.Name, cidr, err)
		}
	}
	if p.Endpoint != "" {
		if _, _, err := net.SplitHostPort(p.Endpoint); err != nil {
			return fmt.Errorf("wireguard: peer %q endpoint %q is not host:port: %w", p.Name, p.Endpoint, err)
		}
	}
	if p.PersistentKeepalive < 0 {
		return fmt.Errorf("wireguard: peer %q persistentKeepalive is negative", p.Name)
	}
	return nil
}

// QuickConfig renders a wg-quick(8) configuration, including the Address/MTU
// extensions wg-quick understands. Used to bring the interface up.
func (c InterfaceConfig) QuickConfig() string {
	return c.render(true)
}

// SyncConfig renders a wg(8) configuration (no wg-quick Address/MTU keys), as
// consumed by `wg setconf` / `wg syncconf` to reconcile peers in place.
func (c InterfaceConfig) SyncConfig() string {
	return c.render(false)
}

// render builds the textual config. When quick is true the wg-quick-only keys
// (Address, MTU) are included. Peers are emitted in a stable order so repeated
// renders are byte-identical and diffs are meaningful.
func (c InterfaceConfig) render(quick bool) string {
	var b strings.Builder
	b.WriteString("[Interface]\n")
	b.WriteString("PrivateKey = " + c.PrivateKey.String() + "\n")
	if quick {
		b.WriteString("Address = " + c.Address + "\n")
		if c.MTU > 0 {
			b.WriteString("MTU = " + strconv.Itoa(c.MTU) + "\n")
		}
	}
	b.WriteString("ListenPort = " + strconv.Itoa(c.ListenPort) + "\n")

	for _, p := range sortedPeers(c.Peers) {
		b.WriteString("\n[Peer]\n")
		if p.Name != "" {
			b.WriteString("# " + p.Name + "\n")
		}
		b.WriteString("PublicKey = " + p.PublicKey.String() + "\n")
		if p.Endpoint != "" {
			b.WriteString("Endpoint = " + p.Endpoint + "\n")
		}
		b.WriteString("AllowedIPs = " + strings.Join(p.AllowedIPs, ", ") + "\n")
		if p.PersistentKeepalive > 0 {
			b.WriteString("PersistentKeepalive = " + strconv.Itoa(p.PersistentKeepalive) + "\n")
		}
	}
	return b.String()
}

// sortedPeers returns peers ordered by public key for deterministic rendering.
func sortedPeers(peers []Peer) []Peer {
	out := append([]Peer(nil), peers...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].PublicKey.String() < out[j].PublicKey.String()
	})
	return out
}

// RouteTargets returns the de-duplicated set of CIDRs that must be routed
// through the interface — the union of every peer's AllowedIPs. These are the
// host routes installed so the kernel forwards remote-Pod traffic into the
// tunnel.
func (c InterfaceConfig) RouteTargets() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range c.Peers {
		for _, cidr := range p.AllowedIPs {
			if !seen[cidr] {
				seen[cidr] = true
				out = append(out, cidr)
			}
		}
	}
	sort.Strings(out)
	return out
}
