// Package bootstrap implements the node join workflow for MacVz (#54): a
// preflight verifier (Doctor) that checks a fresh Mac has every prerequisite to
// register as a Kubernetes node, and a config generator that turns the minimum
// join inputs into a ready-to-edit macvz-kubelet config.
//
// It adds no Kubernetes control plane: a MacVz node joins an existing cluster
// using its kubeconfig, exactly like the kubelet does at runtime. The Doctor
// reuses the same config and runtime/helper clients the kubelet uses so a green
// preflight means the same code paths will succeed at startup.
package bootstrap

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/network/privhelper"
	"github.com/chimerakang/macvz/pkg/runtime/container"
)

// Status is the outcome of a single preflight check.
type Status int

const (
	// StatusOK means the prerequisite is satisfied.
	StatusOK Status = iota
	// StatusWarn means the node can still register, but a capability is degraded
	// or could not be confirmed (e.g. logs/exec disabled, API unreachable until
	// the data plane comes up).
	StatusWarn
	// StatusFail means a required prerequisite is missing and the node would not
	// register (or would flap) until it is fixed.
	StatusFail
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusWarn:
		return "WARN"
	case StatusFail:
		return "FAIL"
	default:
		return "?"
	}
}

// Check is the result of one preflight probe. Remediation is the actionable
// next step shown when a check is not OK, so errors identify the missing
// prerequisite rather than just reporting failure.
type Check struct {
	Name        string
	Status      Status
	Detail      string
	Remediation string
}

// Result aggregates every preflight check for a node.
type Result struct {
	Checks []Check
}

// OK reports whether the node is clear to join: no check failed. Warnings do
// not block a join (the capability is optional or confirmable only at runtime).
func (r Result) OK() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			return false
		}
	}
	return true
}

// Doctor verifies a node's join prerequisites against its loaded config. The
// network-dependent probes are fields so tests can inject fakes; NewDoctor
// wires them to the real runtime, privileged helper, and API server clients.
type Doctor struct {
	cfg        config.Config
	internalIP string

	// lookPath resolves a tool on PATH (exec.LookPath by default).
	lookPath func(string) (string, error)
	// runtimeReady reports apple/container readiness.
	runtimeReady func(ctx context.Context) error
	// helperStatus / helperPing probe the privileged network helper.
	helperStatus func(ctx context.Context) (*privhelper.HelperStatus, error)
	helperPing   func(ctx context.Context) error
	// apiReachable confirms the Kubernetes API server answers over the resolved
	// client config. It is allowed to fail before the data plane is up.
	apiReachable func(ctx context.Context) error
	// readForwarding returns the value of net.inet.ip.forwarding ("0"/"1").
	readForwarding func() (string, error)
	// now is the clock used for certificate expiry checks.
	now func() time.Time
}

// NewDoctor builds a Doctor for cfg wired to the real runtime/helper/API
// clients. internalIP is the address the node will advertise (already resolved
// by the caller, which may have auto-detected it).
func NewDoctor(cfg config.Config, internalIP string) *Doctor {
	d := &Doctor{
		cfg:            cfg,
		internalIP:     internalIP,
		lookPath:       exec.LookPath,
		readForwarding: readForwardingSysctl,
		now:            time.Now,
	}
	driver := container.New(container.Config{Binary: cfg.RuntimeBinary, Rosetta: cfg.RuntimeRosetta})
	d.runtimeReady = driver.Ready

	if cfg.PrivilegedHelperSocket != "" {
		hc := privhelper.NewClient(cfg.PrivilegedHelperSocket)
		d.helperStatus = hc.Status
		d.helperPing = hc.Ping
	}

	d.apiReachable = func(ctx context.Context) error {
		restCfg, err := cfg.RestConfig()
		if err != nil {
			return err
		}
		return dialAPIServer(ctx, restCfg.Host)
	}
	return d
}

// Run executes every applicable preflight check and returns the aggregated
// result. Checks are scoped to the config: mesh/podNetwork tooling is only
// checked when those features are enabled.
func (d *Doctor) Run(ctx context.Context) Result {
	var checks []Check
	checks = append(checks, d.checkRuntime(ctx))
	checks = append(checks, d.checkKubeconfig(ctx))
	if c, ok := d.checkWireGuardTooling(); ok {
		checks = append(checks, c...)
	}
	if c, ok := d.checkPrivilegedHelper(ctx); ok {
		checks = append(checks, c)
	}
	if c, ok := d.checkPodNetworkTooling(); ok {
		checks = append(checks, c...)
	}
	checks = append(checks, d.checkServingTLS())
	return Result{Checks: checks}
}

func (d *Doctor) checkRuntime(ctx context.Context) Check {
	c := Check{Name: "apple/container runtime"}
	if err := d.runtimeReady(ctx); err != nil {
		c.Status = StatusFail
		c.Detail = err.Error()
		c.Remediation = "install apple/container and run `container system start`; ensure the CLI named " +
			quoteOrDefault(d.cfg.RuntimeBinary, container.DefaultBinary) + " is on PATH"
		return c
	}
	c.Status = StatusOK
	c.Detail = "runtime reachable and healthy"
	return c
}

func (d *Doctor) checkKubeconfig(ctx context.Context) Check {
	c := Check{Name: "cluster kubeconfig / API server"}
	restCfg, err := d.cfg.RestConfig()
	if err != nil {
		c.Status = StatusFail
		c.Detail = err.Error()
		c.Remediation = "set kubeconfigPath to a kubeconfig with credentials for the target cluster " +
			"(or export KUBECONFIG); the node joins the existing control plane, none is created"
		return c
	}
	if d.apiReachable != nil {
		if err := d.apiReachable(ctx); err != nil {
			// The data plane (mesh/podNetwork) may need to come up before the API
			// server is routable, so this is a warning, not a hard failure.
			c.Status = StatusWarn
			c.Detail = fmt.Sprintf("kubeconfig resolves to %s but it is not reachable yet: %v", restCfg.Host, err)
			c.Remediation = "confirm the API server address is correct and reachable from this host " +
				"(if it is routed over the mesh, it becomes reachable once macvz-kubelet brings the data plane up)"
			return c
		}
	}
	c.Status = StatusOK
	c.Detail = "API server reachable at " + restCfg.Host
	return c
}

func (d *Doctor) checkWireGuardTooling() ([]Check, bool) {
	if !d.cfg.Mesh.Enabled {
		return nil, false
	}
	var out []Check
	for _, tool := range []string{"wg", "wireguard-go"} {
		c := Check{Name: "wireguard tooling: " + tool}
		if path, err := d.lookPath(tool); err != nil {
			c.Status = StatusFail
			c.Detail = tool + " not found on PATH"
			c.Remediation = "install wireguard-tools (provides `wg`) and wireguard-go, e.g. `brew install wireguard-tools wireguard-go`"
		} else {
			c.Status = StatusOK
			c.Detail = path
		}
		out = append(out, c)
	}
	return out, true
}

func (d *Doctor) checkPrivilegedHelper(ctx context.Context) (Check, bool) {
	// The helper is only exercised when it is configured and a feature needs it.
	if d.cfg.PrivilegedHelperSocket == "" || !(d.cfg.Mesh.Enabled || d.cfg.PodNetwork.Enabled) {
		return Check{}, false
	}
	c := Check{Name: "privileged network helper (macvz-netd)"}
	socket := d.cfg.PrivilegedHelperSocket
	st, err := d.helperStatus(ctx)
	if err != nil {
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("not reachable at %s: %v", socket, err)
		c.Remediation = "start macvz-netd as root with --config (see docs/PRIVILEGED_NETWORKING.md); " +
			"the kubelet runs as your user and routes pf/wg/route through this socket"
		return c, true
	}
	if err := d.helperPing(ctx); err != nil {
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("socket at %s answers status but cannot run commands: %v", socket, err)
		c.Remediation = "ensure macvz-netd runs as root so it can execute pf/wg/route/sysctl"
		return c, true
	}
	if !st.PolicyEnforced {
		c.Status = StatusFail
		c.Detail = "helper is running without per-request policy validation"
		c.Remediation = "restart macvz-netd with --config so inputs are restricted to configured CIDRs/peers/anchors"
		return c, true
	}
	c.Status = StatusOK
	c.Detail = fmt.Sprintf("reachable at %s (version %s, policy enforced, uptime %s)", socket, st.Version, st.Uptime)
	return c, true
}

func (d *Doctor) checkPodNetworkTooling() ([]Check, bool) {
	if !d.cfg.PodNetwork.Enabled {
		return nil, false
	}
	var out []Check

	pf := Check{Name: "pod network tooling: pfctl"}
	if path, err := d.lookPath("pfctl"); err != nil {
		pf.Status = StatusFail
		pf.Detail = "pfctl not found on PATH"
		pf.Remediation = "pfctl ships with macOS; ensure /sbin is on PATH"
	} else {
		pf.Status = StatusOK
		pf.Detail = path
	}
	out = append(out, pf)

	fwd := Check{Name: "IPv4 forwarding"}
	val, err := d.readForwarding()
	switch {
	case err != nil:
		fwd.Status = StatusWarn
		fwd.Detail = "could not read net.inet.ip.forwarding: " + err.Error()
		fwd.Remediation = "macvz-netd enables forwarding at startup; verify with `sysctl net.inet.ip.forwarding`"
	case strings.TrimSpace(val) != "1":
		fwd.Status = StatusWarn
		fwd.Detail = "net.inet.ip.forwarding is " + strings.TrimSpace(val)
		fwd.Remediation = "macvz-netd enables it when podNetwork.enableForwarding is true; " +
			"to set it manually run `sudo sysctl -w net.inet.ip.forwarding=1`"
	default:
		fwd.Status = StatusOK
		fwd.Detail = "net.inet.ip.forwarding=1"
	}
	out = append(out, fwd)
	return out, true
}

func (d *Doctor) checkServingTLS() Check {
	c := Check{Name: "kubelet serving TLS (logs/exec)"}
	cert, key := d.cfg.Node.ServingTLSCertFile, d.cfg.Node.ServingTLSKeyFile
	if cert == "" || key == "" {
		c.Status = StatusWarn
		c.Detail = "serving TLS not configured; `kubectl logs`/`exec` will be unavailable"
		c.Remediation = "set node.servingTLSCertFile/servingTLSKeyFile to a cert whose SAN includes the node InternalIP " +
			"(`macvz-kubelet bootstrap --gen-tls` can generate a self-signed pair)"
		return c
	}
	if _, err := os.Stat(key); err != nil {
		c.Status = StatusFail
		c.Detail = "serving TLS key not readable: " + err.Error()
		c.Remediation = "ensure node.servingTLSKeyFile exists and is readable by the kubelet user"
		return c
	}
	leaf, err := loadLeafCert(cert)
	if err != nil {
		c.Status = StatusFail
		c.Detail = "serving TLS cert not usable: " + err.Error()
		c.Remediation = "regenerate the serving cert (e.g. `macvz-kubelet bootstrap --gen-tls`)"
		return c
	}
	now := d.now()
	if now.After(leaf.NotAfter) {
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("serving cert expired on %s", leaf.NotAfter.Format(time.RFC3339))
		c.Remediation = "regenerate the serving cert"
		return c
	}
	if d.internalIP != "" && !certCoversIP(leaf, d.internalIP) {
		c.Status = StatusWarn
		c.Detail = fmt.Sprintf("serving cert SAN does not include node InternalIP %s; the API server may reject logs/exec", d.internalIP)
		c.Remediation = fmt.Sprintf("reissue the cert with `subjectAltName=IP:%s`", d.internalIP)
		return c
	}
	c.Status = StatusOK
	c.Detail = fmt.Sprintf("valid until %s", leaf.NotAfter.Format(time.RFC3339))
	return c
}

// loadLeafCert reads a PEM cert file and returns its first certificate.
func loadLeafCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for {
		block, rest := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("no CERTIFICATE block in %s", path)
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
		data = rest
	}
}

// certCoversIP reports whether the certificate lists ip among its IP SANs.
func certCoversIP(cert *x509.Certificate, ip string) bool {
	target := net.ParseIP(ip)
	if target == nil {
		return false
	}
	for _, sanIP := range cert.IPAddresses {
		if sanIP.Equal(target) {
			return true
		}
	}
	return false
}

func quoteOrDefault(v, def string) string {
	if v == "" {
		v = def
	}
	return fmt.Sprintf("%q", v)
}
