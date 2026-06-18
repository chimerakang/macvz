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

func TestPodNetworkRouterConfigDefaultsForwardingTrue(t *testing.T) {
	c := Default()
	c.PodNetwork = PodNetworkConfig{Enabled: true, Interface: "bridge100"}
	rc := c.PodNetworkRouterConfig()
	if rc.Interface != "bridge100" {
		t.Errorf("Interface = %q, want bridge100", rc.Interface)
	}
	if !rc.EnableForwarding {
		t.Error("EnableForwarding should default to true")
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
