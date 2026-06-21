package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestValidateVolumes(t *testing.T) {
	c := Default()
	if c.Node.Volumes.Root != "/var/lib/macvz/volumes" {
		t.Errorf("default volume root = %q, want /var/lib/macvz/volumes", c.Node.Volumes.Root)
	}

	c.Node.Volumes.Root = "relative/path"
	if err := c.Validate(); err == nil {
		t.Error("expected error for relative volumes.root")
	}

	c = Default()
	c.Node.Volumes.HostPathAllowedPrefixes = []string{"relative"}
	if err := c.Validate(); err == nil {
		t.Error("expected error for relative hostPath allowlist prefix")
	}

	c = Default()
	c.Node.Volumes.HostPathAllowedPrefixes = []string{"/srv/data"}
	if err := c.Validate(); err != nil {
		t.Errorf("absolute prefix should validate: %v", err)
	}
}

func TestValidateServingClientCARequiresServingTLS(t *testing.T) {
	c := Default()
	c.Node.ServingClientCAFile = "/etc/macvz/client-ca.pem"
	// No serving cert/key set: client auth has no endpoint to guard.
	if err := c.Validate(); err == nil {
		t.Error("servingClientCAFile without servingTLSCertFile/KeyFile should fail validation")
	}

	c.Node.ServingTLSCertFile = "/etc/macvz/tls.crt"
	c.Node.ServingTLSKeyFile = "/etc/macvz/tls.key"
	if err := c.Validate(); err != nil {
		t.Errorf("servingClientCAFile with serving TLS should validate: %v", err)
	}
}

func TestRosettaDefaultsOff(t *testing.T) {
	if Default().RuntimeRosetta {
		t.Error("Rosetta must be disabled by default (amd64 images rejected unless opted in)")
	}
}

func TestRuntimeSocketIsOptional(t *testing.T) {
	c := Default()
	c.RuntimeSocket = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("RuntimeSocket is reserved for future service API use and should be optional: %v", err)
	}
}

func TestRuntimeDataRootMustBeAbsoluteWhenSet(t *testing.T) {
	c := Default()
	c.RuntimeDataRoot = "relative/data"
	if err := c.Validate(); err == nil {
		t.Fatal("expected relative runtimeDataRoot to fail validation")
	}

	c.RuntimeDataRoot = "/Users/operator/.container"
	if err := c.Validate(); err != nil {
		t.Fatalf("absolute runtimeDataRoot should validate: %v", err)
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

func TestDefaultHeartbeat(t *testing.T) {
	c := Default()
	if !c.Node.EnableLease {
		t.Error("lease should be enabled by default")
	}
	if c.Node.LeaseDurationSeconds != 40 {
		t.Errorf("LeaseDurationSeconds = %d, want 40", c.Node.LeaseDurationSeconds)
	}
	ping, status, err := c.HeartbeatIntervals()
	if err != nil {
		t.Fatalf("HeartbeatIntervals: %v", err)
	}
	if ping != 10*time.Second {
		t.Errorf("ping = %v, want 10s", ping)
	}
	if status != 10*time.Second {
		t.Errorf("status = %v, want 10s", status)
	}
}

func TestHeartbeatIntervalsRejectsBadDuration(t *testing.T) {
	c := Default()
	c.Node.PingInterval = "soon"
	if _, _, err := c.HeartbeatIntervals(); err == nil {
		t.Fatal("expected error for invalid ping interval")
	}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate should reject an invalid interval")
	}
}

func TestHeartbeatIntervalsRejectsNonPositive(t *testing.T) {
	c := Default()
	c.Node.StatusUpdateInterval = "0s"
	if _, _, err := c.HeartbeatIntervals(); err == nil {
		t.Fatal("expected error for non-positive status interval")
	}
}

func TestValidateRejectsBadLeaseDuration(t *testing.T) {
	c := Default()
	c.Node.LeaseDurationSeconds = 0
	if err := c.Validate(); err == nil {
		t.Fatal("Validate should reject a non-positive lease duration when leases enabled")
	}
	// Disabling leases makes the duration irrelevant.
	c.Node.EnableLease = false
	if err := c.Validate(); err != nil {
		t.Fatalf("lease duration should be ignored when leases disabled: %v", err)
	}
}

func TestValidateRejectsStatusIntervalLongerThanLease(t *testing.T) {
	c := Default()
	c.Node.LeaseDurationSeconds = 40
	c.Node.StatusUpdateInterval = "40s"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate should reject status interval equal to lease duration")
	}
	c.Node.EnableLease = false
	if err := c.Validate(); err != nil {
		t.Fatalf("status interval may exceed lease duration when leases disabled: %v", err)
	}
}

func TestDefaultKubeletPort(t *testing.T) {
	if Default().Node.KubeletPort != 10250 {
		t.Errorf("default KubeletPort = %d, want 10250", Default().Node.KubeletPort)
	}
}

func TestValidateRejectsBadKubeletPort(t *testing.T) {
	c := Default()
	c.Node.KubeletPort = 0
	if err := c.Validate(); err == nil {
		t.Fatal("Validate should reject KubeletPort 0")
	}
	c.Node.KubeletPort = 70000
	if err := c.Validate(); err == nil {
		t.Fatal("Validate should reject KubeletPort > 65535")
	}
}

func TestValidateRejectsUnpairedServingTLS(t *testing.T) {
	c := Default()
	c.Node.ServingTLSCertFile = "/tmp/cert.pem"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate should reject cert without key")
	}
	c.Node.ServingTLSKeyFile = "/tmp/key.pem"
	if err := c.Validate(); err != nil {
		t.Fatalf("cert+key together should validate: %v", err)
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
