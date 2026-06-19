package wireguard

import (
	"strings"
	"testing"
)

func testKey(t *testing.T) Key {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return k
}

func testConfig(t *testing.T) InterfaceConfig {
	t.Helper()
	return InterfaceConfig{
		Name:       "utun7",
		PrivateKey: testKey(t),
		Address:    "10.99.0.1/32",
		ListenPort: 51820,
		Peers: []Peer{
			{
				Name:                "mac-02",
				PublicKey:           testKey(t),
				Endpoint:            "192.168.1.20:51820",
				AllowedIPs:          []string{"10.244.2.0/24", "10.99.0.2/32"},
				PersistentKeepalive: 25,
			},
		},
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	good := testConfig(t)
	cases := map[string]func(c *InterfaceConfig){
		"no name":        func(c *InterfaceConfig) { c.Name = "" },
		"no key":         func(c *InterfaceConfig) { c.PrivateKey = Key{} },
		"bad address":    func(c *InterfaceConfig) { c.Address = "10.99.0.1" },
		"bad port":       func(c *InterfaceConfig) { c.ListenPort = 0 },
		"negative mtu":   func(c *InterfaceConfig) { c.MTU = -1 },
		"peer no key":    func(c *InterfaceConfig) { c.Peers[0].PublicKey = Key{} },
		"peer no allow":  func(c *InterfaceConfig) { c.Peers[0].AllowedIPs = nil },
		"peer bad cidr":  func(c *InterfaceConfig) { c.Peers[0].AllowedIPs = []string{"nope"} },
		"peer bad endpt": func(c *InterfaceConfig) { c.Peers[0].Endpoint = "noport" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := good
			c.Peers = append([]Peer(nil), good.Peers...)
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("Validate() = nil, want error for %s", name)
			}
		})
	}
}

func TestValidateRejectsDuplicatePeerKeys(t *testing.T) {
	c := testConfig(t)
	dup := c.Peers[0]
	dup.Name = "mac-03"
	c.Peers = append(c.Peers, dup)
	if err := c.Validate(); err == nil {
		t.Error("expected error for duplicate peer public keys")
	}
}

func TestQuickConfigIncludesAddressAndMTU(t *testing.T) {
	c := testConfig(t)
	c.MTU = 1380
	out := c.QuickConfig()
	for _, want := range []string{"[Interface]", "Address = 10.99.0.1/32", "MTU = 1380", "ListenPort = 51820", "[Peer]", "# mac-02", "Endpoint = 192.168.1.20:51820", "AllowedIPs = 10.244.2.0/24, 10.99.0.2/32", "PersistentKeepalive = 25"} {
		if !strings.Contains(out, want) {
			t.Errorf("QuickConfig missing %q\n---\n%s", want, out)
		}
	}
}

func TestSyncConfigOmitsQuickExtensions(t *testing.T) {
	c := testConfig(t)
	c.MTU = 1380
	out := c.SyncConfig()
	if strings.Contains(out, "Address =") || strings.Contains(out, "MTU =") {
		t.Errorf("SyncConfig must not contain wg-quick keys\n---\n%s", out)
	}
	if !strings.Contains(out, "PublicKey =") || !strings.Contains(out, "ListenPort = 51820") {
		t.Errorf("SyncConfig missing required wg keys\n---\n%s", out)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	c := testConfig(t)
	// Add peers out of key order; render must be stable across calls.
	p2 := Peer{Name: "z", PublicKey: testKey(t), AllowedIPs: []string{"10.244.3.0/24"}}
	p3 := Peer{Name: "a", PublicKey: testKey(t), AllowedIPs: []string{"10.244.4.0/24"}}
	c.Peers = append(c.Peers, p2, p3)
	first := c.SyncConfig()
	second := c.SyncConfig()
	if first != second {
		t.Error("SyncConfig is not deterministic")
	}
	// A reordered peer slice must render identically (peers sort by key).
	c.Peers[1], c.Peers[2] = c.Peers[2], c.Peers[1]
	if reordered := c.SyncConfig(); reordered != first {
		t.Error("SyncConfig depends on peer slice order")
	}
}

func TestConfigBlockRendersStandalonePeer(t *testing.T) {
	p := Peer{
		Name:                "mac-02",
		PublicKey:           testKey(t),
		Endpoint:            "192.168.1.20:51820",
		AllowedIPs:          []string{"10.244.2.0/24", "10.99.0.2/32"},
		PersistentKeepalive: 25,
	}
	out := p.ConfigBlock()
	if !strings.HasPrefix(out, "[Peer]\n") {
		t.Errorf("ConfigBlock must start with [Peer], got:\n%s", out)
	}
	for _, want := range []string{"# mac-02", "PublicKey = " + p.PublicKey.String(), "Endpoint = 192.168.1.20:51820", "AllowedIPs = 10.244.2.0/24, 10.99.0.2/32", "PersistentKeepalive = 25"} {
		if !strings.Contains(out, want) {
			t.Errorf("ConfigBlock missing %q\n---\n%s", want, out)
		}
	}

	// A peer behind NAT (no endpoint, no keepalive) omits those keys.
	nat := Peer{Name: "nat", PublicKey: testKey(t), AllowedIPs: []string{"10.244.9.0/24"}}
	block := nat.ConfigBlock()
	if strings.Contains(block, "Endpoint") || strings.Contains(block, "PersistentKeepalive") {
		t.Errorf("NAT peer block must omit Endpoint/PersistentKeepalive:\n%s", block)
	}
}

func TestRouteTargetsDedupesAndSorts(t *testing.T) {
	c := testConfig(t)
	c.Peers = append(c.Peers, Peer{
		Name:       "mac-03",
		PublicKey:  testKey(t),
		AllowedIPs: []string{"10.244.3.0/24", "10.244.2.0/24"}, // 2.0 overlaps mac-02
	})
	got := c.RouteTargets()
	want := []string{"10.244.2.0/24", "10.244.3.0/24", "10.99.0.2/32"}
	if len(got) != len(want) {
		t.Fatalf("RouteTargets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RouteTargets[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
