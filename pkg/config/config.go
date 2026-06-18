// Package config loads macvz-kubelet configuration from a YAML file, applying
// sensible defaults and validating required fields.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the on-disk configuration for a macvz-kubelet node process.
type Config struct {
	// NodeName is the Kubernetes node name this process registers as.
	// Defaults to the OS hostname when empty.
	NodeName string `yaml:"nodeName"`

	// KubeconfigPath points at the kubeconfig used to reach the API server.
	// When empty, in-cluster config (or KUBECONFIG env) is used.
	KubeconfigPath string `yaml:"kubeconfigPath"`

	// RuntimeSocket is the path to the apple/container service API socket.
	RuntimeSocket string `yaml:"runtimeSocket"`

	// LogLevel is the klog verbosity ("info", "debug") or a numeric V level.
	LogLevel string `yaml:"logLevel"`
}

// Default returns a Config populated with built-in defaults.
func Default() Config {
	host, _ := os.Hostname()
	return Config{
		NodeName:      host,
		RuntimeSocket: "/var/run/com.apple.container.sock",
		LogLevel:      "info",
	}
}

// Load reads and parses the YAML config at path, layering it over defaults.
// A non-existent path is not an error: defaults are returned. This lets the
// binary run with zero config during early development.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config %q: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate checks that required fields are present and coherent.
func (c Config) Validate() error {
	if c.NodeName == "" {
		return fmt.Errorf("nodeName must be set (hostname lookup failed)")
	}
	if c.RuntimeSocket == "" {
		return fmt.Errorf("runtimeSocket must be set")
	}
	return nil
}
