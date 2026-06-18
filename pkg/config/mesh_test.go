package config

import (
	"path/filepath"
	"testing"

	"github.com/chimerakang/macvz/pkg/network/wireguard"
)

func enabledMesh(t *testing.T, keyPath string) MeshConfig {
	t.Helper()
	peerKey, err := wireguard.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return MeshConfig{
		Enabled:        true,
		Interface:      "utun7",
		PrivateKeyFile: keyPath,
		Address:        "10.99.0.1/32",
		ListenPort:     51820,
		Peers: []MeshPeerConfig{{
			Name:      "mac-02",
			PublicKey: peerKey.PublicKey().String(),
			Endpoint:  "192.168.1.20:51820",
			PodCIDR:   "10.244.2.0/24",
			Address:   "10.99.0.2/32",
		}},
	}
}

func TestValidateMeshDisabledSkipsChecks(t *testing.T) {
	c := Default()
	c.Mesh = MeshConfig{Enabled: false, Address: "garbage"} // invalid, but disabled
	if err := c.Validate(); err != nil {
		t.Errorf("disabled mesh should not be validated, got %v", err)
	}
}

func TestValidateMeshEnabled(t *testing.T) {
	c := Default()
	c.Mesh = enabledMesh(t, filepath.Join(t.TempDir(), "wg.key"))
	if err := c.Validate(); err != nil {
		t.Errorf("valid mesh rejected: %v", err)
	}
}

func TestValidateMeshRejectsBadFields(t *testing.T) {
	base := enabledMesh(t, filepath.Join(t.TempDir(), "wg.key"))
	cases := map[string]func(m *MeshConfig){
		"no interface":  func(m *MeshConfig) { m.Interface = "" },
		"no key file":   func(m *MeshConfig) { m.PrivateKeyFile = "" },
		"bad address":   func(m *MeshConfig) { m.Address = "10.99.0.1" },
		"bad port":      func(m *MeshConfig) { m.ListenPort = 70000 },
		"peer bad key":  func(m *MeshConfig) { m.Peers[0].PublicKey = "nope" },
		"peer bad cidr": func(m *MeshConfig) { m.Peers[0].PodCIDR = "nope" },
		"peer bad endp": func(m *MeshConfig) { m.Peers[0].Endpoint = "noport" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := Default()
			m := base
			m.Peers = append([]MeshPeerConfig(nil), base.Peers...)
			mutate(&m)
			c.Mesh = m
			if err := c.Validate(); err == nil {
				t.Errorf("Validate() = nil, want error for %s", name)
			}
		})
	}
}

func TestMeshInterfaceConfigTranslates(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "wg.key")
	c := Default()
	c.Mesh = enabledMesh(t, keyPath)

	ifc, err := c.MeshInterfaceConfig()
	if err != nil {
		t.Fatalf("MeshInterfaceConfig: %v", err)
	}
	if ifc.Name != "utun7" || ifc.Address != "10.99.0.1/32" || ifc.ListenPort != 51820 {
		t.Errorf("interface fields not translated: %+v", ifc)
	}
	if ifc.PrivateKey.IsZero() {
		t.Error("private key was not loaded/generated")
	}
	if len(ifc.Peers) != 1 {
		t.Fatalf("peers = %d, want 1", len(ifc.Peers))
	}
	// AllowedIPs must combine the peer's Pod CIDR and mesh address.
	want := map[string]bool{"10.244.2.0/24": true, "10.99.0.2/32": true}
	for _, a := range ifc.Peers[0].AllowedIPs {
		if !want[a] {
			t.Errorf("unexpected AllowedIP %q", a)
		}
	}
	if len(ifc.Peers[0].AllowedIPs) != 2 {
		t.Errorf("AllowedIPs = %v, want pod CIDR + address", ifc.Peers[0].AllowedIPs)
	}
	// The resolved config must satisfy the wireguard package's own validation.
	if err := ifc.Validate(); err != nil {
		t.Errorf("translated config fails wireguard validation: %v", err)
	}
}

func TestMeshInterfaceConfigDefaultsPort(t *testing.T) {
	c := Default()
	m := enabledMesh(t, filepath.Join(t.TempDir(), "wg.key"))
	m.ListenPort = 0
	c.Mesh = m
	ifc, err := c.MeshInterfaceConfig()
	if err != nil {
		t.Fatalf("MeshInterfaceConfig: %v", err)
	}
	if ifc.ListenPort != DefaultMeshListenPort {
		t.Errorf("ListenPort = %d, want default %d", ifc.ListenPort, DefaultMeshListenPort)
	}
}
