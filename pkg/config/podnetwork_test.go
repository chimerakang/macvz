package config

import "testing"

func TestValidatePodNetworkDisabledSkipsChecks(t *testing.T) {
	c := Default()
	c.PodNetwork = PodNetworkConfig{Enabled: false} // no interface, but disabled
	if err := c.Validate(); err != nil {
		t.Errorf("disabled Pod network should not be validated, got %v", err)
	}
}

func TestValidatePodNetworkRequiresInterface(t *testing.T) {
	c := Default()
	c.PodNetwork = PodNetworkConfig{Enabled: true}
	if err := c.Validate(); err == nil {
		t.Error("enabled Pod network without an interface should fail validation")
	}
}

func TestValidatePodNetworkWithHelperRequiresStaticPodCIDR(t *testing.T) {
	c := Default()
	c.PrivilegedHelperSocket = "/var/run/macvz-netd.sock"
	c.PodNetwork = PodNetworkConfig{Enabled: true, Interface: "bridge100"}
	c.Node.PodCIDR = ""
	if err := c.Validate(); err == nil {
		t.Fatal("podNetwork with an enforcing privileged helper should require node.podCIDR")
	}

	c.Node.PodCIDR = "10.244.101.0/24"
	if err := c.Validate(); err != nil {
		t.Fatalf("static node.podCIDR should satisfy helper policy validation: %v", err)
	}
}

func TestPodNetworkRouterConfigDefaultsForwardingTrue(t *testing.T) {
	c := Default()
	c.PodNetwork = PodNetworkConfig{Enabled: true, Interface: "bridge100", IngressInterfaces: []string{"en0"}}
	rc := c.PodNetworkRouterConfig()
	if rc.Interface != "bridge100" {
		t.Errorf("Interface = %q, want bridge100", rc.Interface)
	}
	if len(rc.IngressInterfaces) != 1 || rc.IngressInterfaces[0] != "en0" {
		t.Errorf("IngressInterfaces = %v, want [en0]", rc.IngressInterfaces)
	}
	if !rc.EnableForwarding {
		t.Error("EnableForwarding should default to true")
	}
}

func TestValidatePodNetworkRejectsBlankIngressInterface(t *testing.T) {
	c := Default()
	c.PodNetwork = PodNetworkConfig{Enabled: true, Interface: "bridge100", IngressInterfaces: []string{" en0"}}
	if err := c.Validate(); err == nil {
		t.Error("enabled Pod network with whitespace-padded ingress interface should fail validation")
	}
}

func TestPodNetworkRouterConfigForwardingOverride(t *testing.T) {
	off := false
	c := Default()
	c.PodNetwork = PodNetworkConfig{Enabled: true, Interface: "bridge100", EnableForwarding: &off}
	if c.PodNetworkRouterConfig().EnableForwarding {
		t.Error("EnableForwarding override to false was not honored")
	}
}

func TestRoutableServiceCIDRsDefaultsVMNetWhenPodNetworkEnabled(t *testing.T) {
	c := Default()
	c.Node.PodCIDR = "10.244.101.0/24"
	c.PodNetwork = PodNetworkConfig{Enabled: true, Interface: "bridge100"}

	got := c.RoutableServiceCIDRs()
	if !containsString(got, "10.244.101.0/24") {
		t.Errorf("RoutableServiceCIDRs missing node PodCIDR: %v", got)
	}
	if !containsString(got, DefaultVMNetCIDR) {
		t.Errorf("RoutableServiceCIDRs missing default vmnet CIDR %q: %v", DefaultVMNetCIDR, got)
	}
}

func TestRoutableServiceCIDRsIncludesMeshPeers(t *testing.T) {
	c := Default()
	c.Node.PodCIDR = "10.244.101.0/24"
	c.PodNetwork = PodNetworkConfig{Enabled: true, Interface: "bridge100", VMNetCIDRs: []string{"172.31.0.0/24"}}
	c.Mesh.Enabled = true
	c.Mesh.Peers = []MeshPeerConfig{
		{Name: "mac-02", PodCIDR: "10.244.102.0/24"},
		{Name: "mac-03"}, // incomplete peers are ignored here; mesh validation owns shape checks.
	}

	got := c.RoutableServiceCIDRs()
	for _, want := range []string{"10.244.101.0/24", "172.31.0.0/24", "10.244.102.0/24"} {
		if !containsString(got, want) {
			t.Errorf("RoutableServiceCIDRs missing %q: %v", want, got)
		}
	}
	if containsString(got, DefaultVMNetCIDR) {
		t.Errorf("custom vmnet CIDRs should replace the default %q: %v", DefaultVMNetCIDR, got)
	}
}

func TestRoutableServiceCIDRsIgnoresMeshPeersWhenMeshDisabled(t *testing.T) {
	c := Default()
	c.Node.PodCIDR = "10.244.101.0/24"
	c.PodNetwork = PodNetworkConfig{Enabled: true, Interface: "bridge100"}
	c.Mesh.Enabled = false
	c.Mesh.Peers = []MeshPeerConfig{{Name: "mac-02", PodCIDR: "10.244.102.0/24"}}

	got := c.RoutableServiceCIDRs()
	if containsString(got, "10.244.102.0/24") {
		t.Fatalf("disabled mesh peer CIDR must not be treated as routable: %v", got)
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
