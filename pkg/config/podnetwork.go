package config

import (
	"fmt"

	"github.com/chimerakang/macvz/pkg/network/podnet"
)

// PodNetworkConfig configures the host Pod network path that makes each
// micro-VM reachable at its assigned Pod IP across the mesh (#22). It is opt-in:
// when disabled, Pods keep apple/container's host-only address and cross-host
// routing is unavailable.
type PodNetworkConfig struct {
	// Enabled turns the host Pod network path on. It is only meaningful together
	// with an enabled mesh and Kubernetes-assigned Pod CIDR.
	Enabled bool `yaml:"enabled"`

	// Interface is the host vmnet interface apple/container micro-VMs attach to
	// (e.g. "bridge100"). The binat rules are scoped to it.
	Interface string `yaml:"interface"`

	// Anchor is the pf anchor MacVz manages. Defaults to podnet.DefaultAnchor.
	// The operator must reference this anchor from the main pf.conf.
	Anchor string `yaml:"anchor"`

	// EnableForwarding turns on IPv4 forwarding so the host routes between the
	// mesh interface and the vmnet interface. Defaults to true when enabled;
	// set false only when forwarding is managed externally.
	EnableForwarding *bool `yaml:"enableForwarding"`
}

// validatePodNetwork checks the Pod network fields when enabled.
func (c Config) validatePodNetwork() error {
	pn := c.PodNetwork
	if !pn.Enabled {
		return nil
	}
	if pn.Interface == "" {
		return fmt.Errorf("podNetwork.interface is required when the Pod network is enabled")
	}
	return nil
}

// PodNetworkRouterConfig resolves the YAML Pod network config into a
// podnet.Config. Call only when PodNetwork.Enabled is true.
func (c Config) PodNetworkRouterConfig() podnet.Config {
	pn := c.PodNetwork
	forwarding := true
	if pn.EnableForwarding != nil {
		forwarding = *pn.EnableForwarding
	}
	return podnet.Config{
		Interface:        pn.Interface,
		Anchor:           pn.Anchor,
		EnableForwarding: forwarding,
	}
}
