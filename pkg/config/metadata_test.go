package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/pkg/network/wireguard"
	"gopkg.in/yaml.v3"
)

// exportConfig builds a minimal mesh-enabled config whose private key lives in a
// temp file, so ExportMeshMetadata generates a stable key without side effects.
func exportConfig(t *testing.T) Config {
	t.Helper()
	c := Default()
	c.NodeName = "macvz-a"
	c.Node.InternalIP = "192.168.1.110"
	c.Node.PodCIDR = "10.244.101.0/24"
	c.Mesh = MeshConfig{
		Enabled:        true,
		Interface:      "utun7",
		PrivateKeyFile: filepath.Join(t.TempDir(), "wg.key"),
		Address:        "10.99.0.1/32",
		ListenPort:     51820,
	}
	return c
}

func TestExportMeshMetadata(t *testing.T) {
	c := exportConfig(t)

	md, err := c.ExportMeshMetadata()
	if err != nil {
		t.Fatalf("ExportMeshMetadata: %v", err)
	}
	if md.Name != "macvz-a" {
		t.Errorf("Name = %q, want macvz-a", md.Name)
	}
	if md.Endpoint != "192.168.1.110:51820" {
		t.Errorf("Endpoint = %q, want 192.168.1.110:51820", md.Endpoint)
	}
	if md.Address != "10.99.0.1/32" || md.PodCIDR != "10.244.101.0/24" {
		t.Errorf("Address/PodCIDR = %q/%q", md.Address, md.PodCIDR)
	}
	if md.ListenPort != 51820 {
		t.Errorf("ListenPort = %d, want 51820", md.ListenPort)
	}

	// The exported public key must match the generated private key, and the
	// private key must never appear in the document.
	priv, err := wireguard.LoadOrCreateKey(c.Mesh.PrivateKeyFile)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if md.PublicKey != priv.PublicKey().String() {
		t.Errorf("PublicKey = %q, want derived %q", md.PublicKey, priv.PublicKey().String())
	}
	data, err := md.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), priv.String()) {
		t.Fatal("marshalled metadata leaks the private key")
	}
}

func TestExportMeshMetadataRequiresPodCIDR(t *testing.T) {
	c := exportConfig(t)
	c.Node.PodCIDR = ""
	if _, err := c.ExportMeshMetadata(); err == nil {
		t.Fatal("expected error when node.podCIDR is unset")
	}
}

func TestExportMeshMetadataDisabled(t *testing.T) {
	c := exportConfig(t)
	c.Mesh.Enabled = false
	if _, err := c.ExportMeshMetadata(); err == nil {
		t.Fatal("expected error when mesh is disabled")
	}
}

func TestMeshMetadataMarshalRoundTrip(t *testing.T) {
	md, err := exportConfig(t).ExportMeshMetadata()
	if err != nil {
		t.Fatalf("ExportMeshMetadata: %v", err)
	}
	data, err := md.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := ParseMeshNodeMetadata(data)
	if err != nil {
		t.Fatalf("ParseMeshNodeMetadata: %v", err)
	}
	if got != md {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, md)
	}
}

func TestParseMeshNodeMetadataRejectsBadSchema(t *testing.T) {
	doc := "schema: some.other/format\nnode: x\npublicKey: bad\npodCIDR: 10.0.0.0/24\n"
	if _, err := ParseMeshNodeMetadata([]byte(doc)); err == nil {
		t.Fatal("expected unsupported-schema error")
	}
}

func TestParseMeshNodeMetadataRejectsBadKey(t *testing.T) {
	doc := "node: x\npublicKey: not-base64!!\npodCIDR: 10.0.0.0/24\n"
	if _, err := ParseMeshNodeMetadata([]byte(doc)); err == nil {
		t.Fatal("expected invalid publicKey error")
	}
}

func validMetadata(t *testing.T, name, podCIDR, addr, endpoint string) MeshNodeMetadata {
	t.Helper()
	k, err := wireguard.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return MeshNodeMetadata{
		Schema:    meshMetadataSchema,
		Name:      name,
		PublicKey: k.PublicKey().String(),
		Endpoint:  endpoint,
		Address:   addr,
		PodCIDR:   podCIDR,
	}
}

func TestRenderMeshPeersRoundTripsIntoConfig(t *testing.T) {
	nodes := []MeshNodeMetadata{
		validMetadata(t, "macvz-b", "10.244.102.0/24", "10.99.0.2/32", "192.168.1.122:51820"),
		validMetadata(t, "macvz-c", "10.244.103.0/24", "10.99.0.3/32", ""), // behind NAT
	}

	out, err := RenderMeshPeers(nodes, DefaultPeerKeepalive)
	if err != nil {
		t.Fatalf("RenderMeshPeers: %v", err)
	}
	// The NAT peer must not emit an empty endpoint key.
	if strings.Contains(out, "endpoint: \"\"") || strings.Contains(out, "endpoint: ''") {
		t.Errorf("rendered an empty endpoint:\n%s", out)
	}

	// The fragment must parse back into the canonical peer config, and a node
	// loading it must validate.
	var frag struct {
		Peers []MeshPeerConfig `yaml:"peers"`
	}
	if err := yaml.Unmarshal([]byte(out), &frag); err != nil {
		t.Fatalf("re-parse rendered peers: %v\n%s", err, out)
	}
	if len(frag.Peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(frag.Peers))
	}
	if frag.Peers[0].Name != "macvz-b" || frag.Peers[0].PersistentKeepalive != DefaultPeerKeepalive {
		t.Errorf("peer[0] = %+v", frag.Peers[0])
	}
	if frag.Peers[1].Endpoint != "" {
		t.Errorf("NAT peer should have empty endpoint, got %q", frag.Peers[1].Endpoint)
	}

	cfg := exportConfig(t)
	cfg.Mesh.Peers = frag.Peers
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config with rendered peers failed validation: %v", err)
	}
}

func TestRenderWireGuardPeers(t *testing.T) {
	nodes := []MeshNodeMetadata{
		validMetadata(t, "macvz-b", "10.244.102.0/24", "10.99.0.2/32", "192.168.1.122:51820"),
	}
	out, err := RenderWireGuardPeers(nodes, 0)
	if err != nil {
		t.Fatalf("RenderWireGuardPeers: %v", err)
	}
	for _, want := range []string{
		"[Peer]",
		"# macvz-b",
		"Endpoint = 192.168.1.122:51820",
		// AllowedIPs is the union of Pod CIDR and mesh address.
		"AllowedIPs = 10.244.102.0/24, 10.99.0.2/32",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered wg block missing %q:\n%s", want, out)
		}
	}
	// keepalive 0 must be omitted.
	if strings.Contains(out, "PersistentKeepalive") {
		t.Errorf("keepalive 0 should be omitted:\n%s", out)
	}
}

func TestMeshNodeMetadataValidate(t *testing.T) {
	cases := map[string]MeshNodeMetadata{
		"no name":      {PublicKey: validMetadata(t, "x", "10.0.0.0/24", "", "").PublicKey, PodCIDR: "10.0.0.0/24"},
		"bad key":      {Name: "x", PublicKey: "nope", PodCIDR: "10.0.0.0/24"},
		"bad podCIDR":  {Name: "x", PublicKey: validMetadata(t, "x", "10.0.0.0/24", "", "").PublicKey, PodCIDR: "10.0.0.0"},
		"bad address":  {Name: "x", PublicKey: validMetadata(t, "x", "10.0.0.0/24", "", "").PublicKey, PodCIDR: "10.0.0.0/24", Address: "garbage"},
		"bad endpoint": {Name: "x", PublicKey: validMetadata(t, "x", "10.0.0.0/24", "", "").PublicKey, PodCIDR: "10.0.0.0/24", Endpoint: "noport"},
	}
	for name, md := range cases {
		if err := md.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}
