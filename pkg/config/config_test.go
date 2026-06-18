package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingPathReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if cfg.RuntimeSocket == "" {
		t.Fatal("expected default RuntimeSocket to be set")
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	const body = "nodeName: mac-mini-01\nlogLevel: debug\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeName != "mac-mini-01" {
		t.Errorf("NodeName = %q, want mac-mini-01", cfg.NodeName)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	// Unspecified field keeps its default.
	if cfg.RuntimeSocket == "" {
		t.Error("RuntimeSocket should retain its default when not overridden")
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
	c.NodeName = ""
	if err := c.Validate(); err == nil {
		t.Error("expected error when NodeName is empty")
	}
}

func TestRuntimeSocketIsOptional(t *testing.T) {
	c := Default()
	c.RuntimeSocket = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("RuntimeSocket is reserved for future service API use and should be optional: %v", err)
	}
}

func TestRestConfigMissingKubeconfigErrors(t *testing.T) {
	c := Default()
	c.KubeconfigPath = filepath.Join(t.TempDir(), "absent.kubeconfig")
	if _, err := c.RestConfig(); err == nil {
		t.Fatal("expected a clear error for a missing kubeconfig path")
	}
}

func TestRestConfigInvalidKubeconfigErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.kubeconfig")
	if err := os.WriteFile(p, []byte("not: [valid: yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.KubeconfigPath = p
	if _, err := c.RestConfig(); err == nil {
		t.Fatal("expected an error for an unparseable kubeconfig")
	}
}

func TestRestConfigLoadsValidKubeconfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "kubeconfig")
	const body = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
users:
- name: test
  user:
    token: abc123
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.KubeconfigPath = p

	rc, err := c.RestConfig()
	if err != nil {
		t.Fatalf("RestConfig: %v", err)
	}
	if rc.Host != "https://127.0.0.1:6443" {
		t.Errorf("Host = %q, want https://127.0.0.1:6443", rc.Host)
	}
}
