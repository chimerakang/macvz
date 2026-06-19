package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/network/privhelper"
)

// defaultTestCfg is config.Default() pointed at a minimal valid kubeconfig so
// the kubeconfig check passes and tests can assert on overall readiness.
func defaultTestCfg(t *testing.T) config.Config {
	t.Helper()
	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: https://127.0.0.1:6443}
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user: {}
`
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	cfg := config.Default()
	cfg.KubeconfigPath = path
	return cfg
}

// newTestDoctor builds a Doctor with all probes stubbed to a healthy default so
// each test only overrides the dimension it exercises.
func newTestDoctor(cfg config.Config, internalIP string) *Doctor {
	return &Doctor{
		cfg:            cfg,
		internalIP:     internalIP,
		lookPath:       func(s string) (string, error) { return "/usr/bin/" + s, nil },
		runtimeReady:   func(context.Context) error { return nil },
		helperStatus:   func(context.Context) (*privhelper.HelperStatus, error) { return &privhelper.HelperStatus{Version: "test", PolicyEnforced: true, Uptime: "1m"}, nil },
		helperPing:     func(context.Context) error { return nil },
		apiReachable:   func(context.Context) error { return nil },
		readForwarding: func() (string, error) { return "1\n", nil },
		now:            func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
}

func find(r Result, name string) (Check, bool) {
	for _, c := range r.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

func TestDoctorRuntimeFail(t *testing.T) {
	d := newTestDoctor(config.Default(), "")
	d.runtimeReady = func(context.Context) error { return errors.New("not running") }
	r := d.Run(context.Background())
	c, ok := find(r, "apple/container runtime")
	if !ok || c.Status != StatusFail {
		t.Fatalf("expected runtime FAIL, got %+v ok=%v", c, ok)
	}
	if c.Remediation == "" {
		t.Errorf("runtime failure should carry remediation")
	}
	if r.OK() {
		t.Errorf("result should not be OK when runtime fails")
	}
}

func TestDoctorMeshToolingMissing(t *testing.T) {
	cfg := config.Default()
	cfg.Mesh.Enabled = true
	cfg.Mesh.Interface = "utun7"
	cfg.Mesh.PrivateKeyFile = "/etc/macvz/wireguard.key"
	cfg.Mesh.Address = "10.99.0.1/32"
	cfg.PrivilegedHelperSocket = "/var/run/macvz-netd.sock"

	d := newTestDoctor(cfg, "")
	d.lookPath = func(s string) (string, error) {
		if s == "wireguard-go" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + s, nil
	}
	r := d.Run(context.Background())
	c, ok := find(r, "wireguard tooling: wireguard-go")
	if !ok || c.Status != StatusFail {
		t.Fatalf("expected wireguard-go FAIL, got %+v ok=%v", c, ok)
	}
	// helper checked because mesh enabled + socket set
	if _, ok := find(r, "privileged network helper (macvz-netd)"); !ok {
		t.Errorf("expected privileged helper check when mesh enabled with socket")
	}
}

func TestDoctorHelperPolicyNotEnforced(t *testing.T) {
	cfg := config.Default()
	cfg.PodNetwork.Enabled = true
	cfg.PodNetwork.Interface = "bridge100"
	cfg.PrivilegedHelperSocket = "/var/run/macvz-netd.sock"

	d := newTestDoctor(cfg, "")
	d.helperStatus = func(context.Context) (*privhelper.HelperStatus, error) {
		return &privhelper.HelperStatus{Version: "test", PolicyEnforced: false}, nil
	}
	r := d.Run(context.Background())
	c, _ := find(r, "privileged network helper (macvz-netd)")
	if c.Status != StatusFail {
		t.Fatalf("expected helper FAIL when policy not enforced, got %v", c.Status)
	}
}

func TestDoctorServingTLSUnconfiguredWarns(t *testing.T) {
	d := newTestDoctor(defaultTestCfg(t), "192.168.1.110")
	r := d.Run(context.Background())
	c, _ := find(r, "kubelet serving TLS (logs/exec)")
	if c.Status != StatusWarn {
		t.Fatalf("expected serving TLS WARN when unconfigured, got %v", c.Status)
	}
	if !r.OK() {
		t.Errorf("warnings should not block readiness")
	}
}

func TestDoctorServingTLSValidAndSANMismatch(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "kubelet.crt")
	key := filepath.Join(dir, "kubelet.key")
	now := time.Unix(1_700_000_000, 0)
	if err := GenerateServingTLS("macvz-a", "192.168.1.110", cert, key, 365*24*time.Hour, now); err != nil {
		t.Fatalf("GenerateServingTLS: %v", err)
	}

	cfg := config.Default()
	cfg.Node.ServingTLSCertFile = cert
	cfg.Node.ServingTLSKeyFile = key

	// Matching IP -> OK.
	d := newTestDoctor(cfg, "192.168.1.110")
	d.now = func() time.Time { return now }
	c, _ := find(d.Run(context.Background()), "kubelet serving TLS (logs/exec)")
	if c.Status != StatusOK {
		t.Fatalf("expected serving TLS OK with matching SAN, got %v (%s)", c.Status, c.Detail)
	}

	// Different node IP -> WARN about SAN.
	d2 := newTestDoctor(cfg, "10.0.0.9")
	d2.now = func() time.Time { return now }
	c2, _ := find(d2.Run(context.Background()), "kubelet serving TLS (logs/exec)")
	if c2.Status != StatusWarn {
		t.Fatalf("expected serving TLS WARN on SAN mismatch, got %v", c2.Status)
	}
}

func TestDoctorServingTLSExpired(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "kubelet.crt")
	key := filepath.Join(dir, "kubelet.key")
	issued := time.Unix(1_600_000_000, 0)
	if err := GenerateServingTLS("macvz-a", "192.168.1.110", cert, key, time.Hour, issued); err != nil {
		t.Fatalf("GenerateServingTLS: %v", err)
	}
	cfg := config.Default()
	cfg.Node.ServingTLSCertFile = cert
	cfg.Node.ServingTLSKeyFile = key

	d := newTestDoctor(cfg, "192.168.1.110")
	d.now = func() time.Time { return issued.Add(48 * time.Hour) } // well past expiry
	c, _ := find(d.Run(context.Background()), "kubelet serving TLS (logs/exec)")
	if c.Status != StatusFail {
		t.Fatalf("expected serving TLS FAIL when expired, got %v", c.Status)
	}
}

func TestDoctorForwardingWarn(t *testing.T) {
	cfg := config.Default()
	cfg.PodNetwork.Enabled = true
	cfg.PodNetwork.Interface = "bridge100"
	cfg.PrivilegedHelperSocket = "/var/run/macvz-netd.sock"

	d := newTestDoctor(cfg, "")
	d.readForwarding = func() (string, error) { return "0\n", nil }
	c, _ := find(d.Run(context.Background()), "IPv4 forwarding")
	if c.Status != StatusWarn {
		t.Fatalf("expected forwarding WARN when disabled, got %v", c.Status)
	}
}

func TestGenerateServingTLSWritesUsableFiles(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "k.crt")
	key := filepath.Join(dir, "k.key")
	if err := GenerateServingTLS("n", "1.2.3.4", cert, key, time.Hour, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("GenerateServingTLS: %v", err)
	}
	leaf, err := loadLeafCert(cert)
	if err != nil {
		t.Fatalf("loadLeafCert: %v", err)
	}
	if !certCoversIP(leaf, "1.2.3.4") {
		t.Errorf("cert SAN should cover 1.2.3.4")
	}
	if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key should exist with 0600 perms, err=%v", err)
	}
}
