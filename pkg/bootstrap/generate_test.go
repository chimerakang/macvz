package bootstrap

import (
	"os"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/pkg/config"
)

func TestGenerateConfigMinimal(t *testing.T) {
	p := JoinParams{
		NodeName:       "mac-mini-01",
		InternalIP:     "192.168.1.110",
		KubeconfigPath: "/etc/macvz/kubeconfig",
	}
	out, err := GenerateConfig(p)
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}
	for _, want := range []string{
		"nodeName: mac-mini-01",
		"kubeconfigPath: /etc/macvz/kubeconfig",
		`internalIP: "192.168.1.110"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "mesh:") || strings.Contains(out, "podNetwork:") {
		t.Errorf("minimal config should not emit mesh/podNetwork:\n%s", out)
	}
	// The generator self-validates, but assert the round-trip explicitly too.
	mustLoad(t, out)
}

func TestGenerateConfigMeshAndPodNetwork(t *testing.T) {
	// A valid base64 WireGuard public key (32 zero bytes).
	const pub = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAEA="
	p := JoinParams{
		NodeName:               "macvz-a",
		InternalIP:             "192.168.1.110",
		KubeconfigPath:         "/etc/macvz/kubeconfig",
		PodCIDR:                "10.244.101.0/24",
		ClusterDNS:             []string{"10.96.0.10"},
		PrivilegedHelperSocket: "/var/run/macvz-netd.sock",
		Mesh: &MeshParams{
			Address: "10.99.0.1/32",
			Peers: []PeerParams{{
				Name:      "macvz-b",
				PublicKey: pub,
				Endpoint:  "192.168.1.122:51820",
				PodCIDR:   "10.244.102.0/24",
				Address:   "10.99.0.2/32",
			}},
		},
		PodNetwork: &PodNetworkParams{Interface: "bridge100"},
	}
	out, err := GenerateConfig(p)
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}
	for _, want := range []string{
		"privilegedHelperSocket: /var/run/macvz-netd.sock",
		"mesh:\n  enabled: true",
		"interface: utun7",           // default applied
		"listenPort: 51820",          // default applied
		"privateKeyFile: /etc/macvz/wireguard.key", // default applied
		"name: macvz-b",
		"podNetwork:\n  enabled: true",
		"interface: bridge100",
		`vmNetCIDRs: ["192.168.64.0/24"]`, // default applied
		`clusterDNS: ["10.96.0.10"]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	cfg := mustLoad(t, out)
	if !cfg.Mesh.Enabled || !cfg.PodNetwork.Enabled {
		t.Errorf("expected mesh and podNetwork enabled in loaded config")
	}
	if len(cfg.Mesh.Peers) != 1 || cfg.Mesh.Peers[0].Name != "macvz-b" {
		t.Errorf("peer not round-tripped: %+v", cfg.Mesh.Peers)
	}
}

func TestGenerateConfigValidation(t *testing.T) {
	cases := map[string]JoinParams{
		"no node name":   {InternalIP: "1.2.3.4", KubeconfigPath: "/k"},
		"bad ip":         {NodeName: "n", InternalIP: "nope", KubeconfigPath: "/k"},
		"no kubeconfig":  {NodeName: "n", InternalIP: "1.2.3.4"},
		"bad pod cidr":   {NodeName: "n", InternalIP: "1.2.3.4", KubeconfigPath: "/k", PodCIDR: "10.0.0.0"},
		"tls cert only":  {NodeName: "n", InternalIP: "1.2.3.4", KubeconfigPath: "/k", ServingTLSCertFile: "c"},
		"mesh no helper": {NodeName: "n", InternalIP: "1.2.3.4", KubeconfigPath: "/k", Mesh: &MeshParams{Address: "10.99.0.1/32"}},
		"mesh bad addr":  {NodeName: "n", InternalIP: "1.2.3.4", KubeconfigPath: "/k", PrivilegedHelperSocket: "/s", Mesh: &MeshParams{Address: "10.99.0.1"}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := GenerateConfig(p); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func mustLoad(t *testing.T, yaml string) config.Config {
	t.Helper()
	f := t.TempDir() + "/cfg.yaml"
	if err := os.WriteFile(f, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("config.Load on generated YAML failed: %v\n---\n%s", err, yaml)
	}
	return cfg
}
