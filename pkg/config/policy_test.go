package config

import (
	"path/filepath"
	"testing"

	"github.com/chimerakang/macvz/pkg/network/wireguard"
)

func TestPrivilegedHelperPolicyFromConfig(t *testing.T) {
	c := Default()
	c.Mesh = enabledMesh(t, filepath.Join(t.TempDir(), "wg.key"))
	c.PodNetwork = PodNetworkConfig{Enabled: true, Interface: "bridge100"}
	c.Node.PodCIDR = "10.244.101.0/24"

	p, err := c.PrivilegedHelperPolicy()
	if err != nil {
		t.Fatalf("PrivilegedHelperPolicy: %v", err)
	}

	if p.MeshInterface != "utun7" {
		t.Errorf("MeshInterface = %q, want utun7", p.MeshInterface)
	}
	if p.VMNetInterface != "bridge100" {
		t.Errorf("VMNetInterface = %q, want bridge100", p.VMNetInterface)
	}
	if p.Anchor != "macvz/pods" {
		t.Errorf("Anchor = %q, want macvz/pods", p.Anchor)
	}
	if p.MeshAddressIP != "10.99.0.1" {
		t.Errorf("MeshAddressIP = %q, want 10.99.0.1", p.MeshAddressIP)
	}
	// The peer's PodCIDR and mesh Address are the route targets.
	for _, want := range []string{"10.244.2.0/24", "10.99.0.2/32"} {
		if !p.RouteCIDRs[want] {
			t.Errorf("RouteCIDRs missing %q (have %v)", want, p.RouteCIDRs)
		}
	}
	for _, want := range []string{"10.244.101.0/24", "10.244.2.0/24"} {
		if !p.PodCIDRs[want] {
			t.Errorf("PodCIDRs missing %q (have %v)", want, p.PodCIDRs)
		}
	}
	if !p.VMNetCIDRs[DefaultVMNetCIDR] {
		t.Errorf("VMNetCIDRs missing default %q (have %v)", DefaultVMNetCIDR, p.VMNetCIDRs)
	}
	// The peer's public key is the only configured peer.
	pub := c.Mesh.Peers[0].PublicKey
	if k, err := wireguard.ParseKey(pub); err != nil {
		t.Fatalf("ParseKey: %v", err)
	} else if !p.PeerPublicKeys[k.String()] {
		t.Errorf("PeerPublicKeys missing the configured peer key")
	}
}

func TestPrivilegedHelperPolicyMeshDisabledFailsClosed(t *testing.T) {
	c := Default() // mesh and pod network off by default
	p, err := c.PrivilegedHelperPolicy()
	if err != nil {
		t.Fatalf("PrivilegedHelperPolicy: %v", err)
	}
	if p.MeshInterface != "" {
		t.Errorf("disabled mesh should leave MeshInterface empty, got %q", p.MeshInterface)
	}
	if len(p.RouteCIDRs) != 0 || len(p.PeerPublicKeys) != 0 {
		t.Errorf("disabled mesh should configure no routes/peers, got %v / %v", p.RouteCIDRs, p.PeerPublicKeys)
	}
	// Anchor defaults even when disabled, but no interface/peers means the helper
	// refuses every mesh and Pod-network command.
	if p.Anchor != "macvz/pods" {
		t.Errorf("Anchor default = %q, want macvz/pods", p.Anchor)
	}
}

func TestPrivilegedHelperPolicyCustomAnchor(t *testing.T) {
	c := Default()
	c.PodNetwork = PodNetworkConfig{Enabled: true, Interface: "bridge100", Anchor: "macvz/custom"}
	p, err := c.PrivilegedHelperPolicy()
	if err != nil {
		t.Fatalf("PrivilegedHelperPolicy: %v", err)
	}
	if p.Anchor != "macvz/custom" {
		t.Errorf("Anchor = %q, want macvz/custom", p.Anchor)
	}
}

func TestPrivilegedHelperPolicyCustomVMNetCIDRs(t *testing.T) {
	c := Default()
	c.PodNetwork = PodNetworkConfig{
		Enabled:    true,
		Interface:  "bridge100",
		VMNetCIDRs: []string{"172.31.0.0/24"},
	}
	p, err := c.PrivilegedHelperPolicy()
	if err != nil {
		t.Fatalf("PrivilegedHelperPolicy: %v", err)
	}
	if !p.VMNetCIDRs["172.31.0.0/24"] || p.VMNetCIDRs[DefaultVMNetCIDR] {
		t.Errorf("VMNetCIDRs = %v, want only custom range", p.VMNetCIDRs)
	}
}
