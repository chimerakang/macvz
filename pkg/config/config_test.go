package config

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

func TestDefaultNodeCapacity(t *testing.T) {
	c := Default()
	cap, err := c.Capacity()
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if cap.Cpu().String() != "2" {
		t.Errorf("default cpu = %q, want 2", cap.Cpu().String())
	}
	if _, ok := cap[corev1.ResourcePods]; !ok {
		t.Error("pods capacity missing")
	}
}

func TestDefaultNodeTaint(t *testing.T) {
	c := Default()
	taints, err := c.Taints()
	if err != nil {
		t.Fatalf("Taints: %v", err)
	}
	if len(taints) != 1 || taints[0].Key != DefaultProviderTaintKey {
		t.Fatalf("expected default provider taint, got %v", taints)
	}
	if string(taints[0].Effect) != "NoSchedule" {
		t.Errorf("default taint effect = %q, want NoSchedule", taints[0].Effect)
	}
}

func TestCapacityRejectsBadQuantity(t *testing.T) {
	c := Default()
	c.Node.Memory = "not-a-quantity"
	if _, err := c.Capacity(); err == nil {
		t.Fatal("expected error for invalid memory quantity")
	}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate should reject an invalid capacity quantity")
	}
}

func TestTaintsRejectsBadEffect(t *testing.T) {
	c := Default()
	c.Node.Taints = []TaintConfig{{Key: "k", Effect: "Nonsense"}}
	if _, err := c.Taints(); err == nil {
		t.Fatal("expected error for invalid taint effect")
	}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate should reject an invalid taint effect")
	}
}

func TestLoadOverridesNodeCapacity(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	const body = "node:\n  cpu: \"8\"\n  memory: 16Gi\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Node.CPU != "8" || cfg.Node.Memory != "16Gi" {
		t.Errorf("node overrides not applied: %+v", cfg.Node)
	}
	// Unspecified node fields keep their defaults.
	if cfg.Node.Pods != "20" {
		t.Errorf("Pods = %q, want default 20", cfg.Node.Pods)
	}
	if cfg.Node.OS != "linux" {
		t.Errorf("OS = %q, want default linux", cfg.Node.OS)
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
