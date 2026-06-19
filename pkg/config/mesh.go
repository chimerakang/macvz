package config

import (
	"fmt"
	"net"

	"github.com/chimerakang/macvz/pkg/network/wireguard"
)

// MeshConfig configures this node's WireGuard mesh interface and its peers. The
// mesh is opt-in: when Enabled is false the kubelet skips all networking setup,
// keeping single-host development zero-config.
type MeshConfig struct {
	// Enabled turns the cross-host WireGuard mesh on.
	Enabled bool `yaml:"enabled"`

	// Interface is the WireGuard interface name to manage (e.g. "utun7").
	Interface string `yaml:"interface"`

	// PrivateKeyFile holds this node's WireGuard private key (base64). It is
	// generated on first start if absent, giving the node a stable identity
	// without keys ever living in this config or in git.
	PrivateKeyFile string `yaml:"privateKeyFile"`

	// Address is this node's address on the mesh network in CIDR form
	// (e.g. "10.99.0.1/32").
	Address string `yaml:"address"`

	// ListenPort is the UDP port WireGuard listens on.
	ListenPort int `yaml:"listenPort"`

	// MTU optionally overrides the interface MTU (0 keeps the default).
	MTU int `yaml:"mtu"`

	// Peers are the other MacVz nodes in the mesh.
	Peers []MeshPeerConfig `yaml:"peers"`
}

// MeshPeerConfig describes one remote node in the mesh.
type MeshPeerConfig struct {
	// Name is the peer's node name (for diagnostics).
	Name string `yaml:"name"`
	// PublicKey is the peer's WireGuard public key (base64).
	PublicKey string `yaml:"publicKey"`
	// Endpoint is the peer's reachable "host:port". May be empty for a peer that
	// initiates the connection from behind NAT.
	Endpoint string `yaml:"endpoint,omitempty"`
	// PodCIDR is the peer's Kubernetes-assigned Pod CIDR, routed through the
	// tunnel so traffic to its Pods is encrypted and delivered to that node.
	PodCIDR string `yaml:"podCIDR"`
	// Address is the peer's mesh address in CIDR form (e.g. "10.99.0.2/32"),
	// also routed through the tunnel.
	Address string `yaml:"address,omitempty"`
	// PersistentKeepalive, in seconds, keeps NAT mappings alive (0 disables it).
	PersistentKeepalive int `yaml:"persistentKeepalive,omitempty"`
}

// DefaultMeshListenPort is the standard WireGuard UDP port, used when a mesh is
// enabled without an explicit listenPort.
const DefaultMeshListenPort = 51820

// validateMesh checks mesh fields when the mesh is enabled. Disabled meshes are
// not validated so an incomplete stanza never blocks single-host startup.
func (c Config) validateMesh() error {
	m := c.Mesh
	if !m.Enabled {
		return nil
	}
	if m.Interface == "" {
		return fmt.Errorf("mesh.interface is required when the mesh is enabled")
	}
	if m.PrivateKeyFile == "" {
		return fmt.Errorf("mesh.privateKeyFile is required when the mesh is enabled")
	}
	if _, _, err := net.ParseCIDR(m.Address); err != nil {
		return fmt.Errorf("mesh.address %q is not a CIDR: %w", m.Address, err)
	}
	if m.ListenPort < 0 || m.ListenPort > 65535 {
		return fmt.Errorf("mesh.listenPort %d out of range", m.ListenPort)
	}
	for i, p := range m.Peers {
		if _, err := wireguard.ParseKey(p.PublicKey); err != nil {
			return fmt.Errorf("mesh.peers[%d] (%q) publicKey is invalid: %w", i, p.Name, err)
		}
		if _, _, err := net.ParseCIDR(p.PodCIDR); err != nil {
			return fmt.Errorf("mesh.peers[%d] (%q) podCIDR %q is not a CIDR: %w", i, p.Name, p.PodCIDR, err)
		}
		if p.Address != "" {
			if _, _, err := net.ParseCIDR(p.Address); err != nil {
				return fmt.Errorf("mesh.peers[%d] (%q) address %q is not a CIDR: %w", i, p.Name, p.Address, err)
			}
		}
		if p.Endpoint != "" {
			if _, _, err := net.SplitHostPort(p.Endpoint); err != nil {
				return fmt.Errorf("mesh.peers[%d] (%q) endpoint %q is not host:port: %w", i, p.Name, p.Endpoint, err)
			}
		}
	}
	return nil
}

// MeshInterfaceConfig resolves the YAML mesh config into a
// wireguard.InterfaceConfig, loading (or generating) the node's private key.
// Call only when Mesh.Enabled is true.
func (c Config) MeshInterfaceConfig() (wireguard.InterfaceConfig, error) {
	m := c.Mesh
	priv, err := wireguard.LoadOrCreateKey(m.PrivateKeyFile)
	if err != nil {
		return wireguard.InterfaceConfig{}, err
	}

	port := m.ListenPort
	if port == 0 {
		port = DefaultMeshListenPort
	}

	peers, err := m.resolvePeers()
	if err != nil {
		return wireguard.InterfaceConfig{}, err
	}

	return wireguard.InterfaceConfig{
		Name:       m.Interface,
		PrivateKey: priv,
		Address:    m.Address,
		ListenPort: port,
		MTU:        m.MTU,
		Peers:      peers,
	}, nil
}

// MeshPeers resolves the configured mesh peers into wireguard.Peer values,
// without loading this node's private key. It is the input to peer
// reconciliation (#42): on a config reload the kubelet feeds the result to
// wireguard.Mesh.Sync to add/remove peers (and their routes) in place, never
// recreating the interface. Call only when Mesh.Enabled is true.
func (c Config) MeshPeers() ([]wireguard.Peer, error) {
	return c.Mesh.resolvePeers()
}

// resolvePeers maps the YAML peer entries to wireguard.Peer values. Each peer's
// AllowedIPs are its Pod CIDR plus (when set) its mesh address, exactly the
// CIDRs routed through the tunnel to that node.
func (m MeshConfig) resolvePeers() ([]wireguard.Peer, error) {
	peers := make([]wireguard.Peer, 0, len(m.Peers))
	for _, p := range m.Peers {
		pub, err := wireguard.ParseKey(p.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("mesh peer %q: %w", p.Name, err)
		}
		allowed := []string{p.PodCIDR}
		if p.Address != "" {
			allowed = append(allowed, p.Address)
		}
		peers = append(peers, wireguard.Peer{
			Name:                p.Name,
			PublicKey:           pub,
			Endpoint:            p.Endpoint,
			AllowedIPs:          allowed,
			PersistentKeepalive: p.PersistentKeepalive,
		})
	}
	return peers, nil
}
