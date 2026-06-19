package main

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/health"
	"github.com/chimerakang/macvz/pkg/network/podnet"
	"github.com/chimerakang/macvz/pkg/network/privhelper"
	"github.com/chimerakang/macvz/pkg/network/wireguard"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// diagnosticsCollector aggregates a node health Report from the live kubelet
// components. It binds the dependency-light health.Checker interfaces to the
// concrete runtime, control-plane, helper, mesh, and pod-network state so the
// /healthz/diagnostics endpoint can explain why a node is or is not ready.
type diagnosticsCollector struct {
	node     string
	checkers []health.Checker
}

// newDiagnosticsCollector wires the checkers for this node from its config and
// live components. mesh and router may be nil when their subsystems are
// disabled; the corresponding checks then report "skipped". The runtime probe
// is the apple/container driver (a runtime.Pinger), and the clientset backs the
// control-plane registration/lease checks.
func newDiagnosticsCollector(
	cfg config.Config,
	runtimeProbe health.RuntimeProbe,
	clientset kubernetes.Interface,
	mesh *wireguard.Mesh,
	router *podnet.Router,
) *diagnosticsCollector {
	helperEnabled := cfg.PrivilegedHelperSocket != "" && (cfg.Mesh.Enabled || cfg.PodNetwork.Enabled)

	var helperProbe health.HelperProbe
	if cfg.PrivilegedHelperSocket != "" {
		helperProbe = helperProbeAdapter{client: privhelper.NewClient(cfg.PrivilegedHelperSocket)}
	}

	var meshProbe health.MeshProbe
	if mesh != nil {
		meshProbe = mesh // *wireguard.Mesh satisfies MeshProbe structurally.
	}

	var attachments health.AttachmentLister
	if router != nil {
		attachments = routerAttachments{router: router}
	}

	checkers := []health.Checker{
		health.NewRuntimeChecker(runtimeProbe),
		health.NewHelperChecker(helperEnabled, helperProbe),
		health.NewMeshChecker(cfg.Mesh.Enabled, meshProbe),
		health.NewForwardingChecker(cfg.PodNetwork.Enabled, forwardingProbe{}),
		health.NewPodNetworkChecker(cfg.PodNetwork.Enabled, cfg.PodNetwork.Interface, cfg.PodNetwork.Anchor, attachments),
	}
	checkers = append(checkers, health.NewControlPlaneCheckers(controlPlaneProbe{
		clientset:     clientset,
		nodeName:      cfg.NodeName,
		leaseEnabled:  cfg.Node.EnableLease,
		leaseDuration: time.Duration(cfg.Node.LeaseDurationSeconds) * time.Second,
	})...)

	return &diagnosticsCollector{node: cfg.NodeName, checkers: checkers}
}

// Report runs every checker and aggregates the result.
func (d *diagnosticsCollector) Report(ctx context.Context) health.Report {
	return health.Collect(ctx, d.node, d.checkers...)
}

// ServeHTTP renders the report. ?format=json returns machine-readable JSON;
// otherwise a human-readable text report. The HTTP status is 200 when the node
// is ready for workloads and 503 (Service Unavailable) when it is not, so a
// liveness/readiness probe or `curl -f` can gate on node health directly.
func (d *diagnosticsCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	report := d.Report(ctx)
	code := http.StatusOK
	if !report.Ready {
		code = http.StatusServiceUnavailable
	}

	if r.URL.Query().Get("format") == "json" {
		body, err := report.JSON()
		if err != nil {
			http.Error(w, fmt.Sprintf("render diagnostics: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(body)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(report.Text()))
}

// helperProbeAdapter adapts a privhelper.Client to health.HelperProbe, mapping
// the helper's self-report into the diagnostics-only HelperInfo.
type helperProbeAdapter struct {
	client *privhelper.Client
}

func (h helperProbeAdapter) Status(ctx context.Context) (health.HelperInfo, error) {
	st, err := h.client.Status(ctx)
	if err != nil {
		return health.HelperInfo{}, err
	}
	return health.HelperInfo{
		Version:         st.Version,
		Protocol:        st.Protocol,
		PolicyEnforced:  st.PolicyEnforced,
		AllowedCommands: st.AllowedCommands,
	}, nil
}

func (h helperProbeAdapter) Ping(ctx context.Context) error { return h.client.Ping(ctx) }

// routerAttachments adapts a podnet.Router to health.AttachmentLister.
type routerAttachments struct {
	router *podnet.Router
}

func (r routerAttachments) AttachmentCount() int { return len(r.router.Endpoints()) }

// forwardingProbe reads host IPv4 forwarding via sysctl. The read does not
// require root, so it queries the host directly rather than through the helper.
type forwardingProbe struct{}

func (forwardingProbe) IPForwardingEnabled(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "sysctl", "-n", "net.inet.ip.forwarding").Output()
	if err != nil {
		return false, fmt.Errorf("read net.inet.ip.forwarding: %w", err)
	}
	return strings.TrimSpace(string(out)) == "1", nil
}

// controlPlaneProbe reads node registration and lease state from the API server.
type controlPlaneProbe struct {
	clientset     kubernetes.Interface
	nodeName      string
	leaseEnabled  bool
	leaseDuration time.Duration
}

func (c controlPlaneProbe) NodeState(ctx context.Context) (health.NodeState, error) {
	if c.clientset == nil {
		return health.NodeState{}, fmt.Errorf("kubernetes client unavailable")
	}
	node, err := c.clientset.CoreV1().Nodes().Get(ctx, c.nodeName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return health.NodeState{Registered: false}, nil
		}
		return health.NodeState{}, err
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			reason := cond.Reason
			if cond.Message != "" {
				reason = cond.Reason + ": " + cond.Message
			}
			return health.NodeState{Registered: true, Ready: cond.Status == corev1.ConditionTrue, Reason: reason}, nil
		}
	}
	return health.NodeState{Registered: true, Ready: false, Reason: "node has no Ready condition"}, nil
}

func (c controlPlaneProbe) LeaseState(ctx context.Context) (health.LeaseState, error) {
	if !c.leaseEnabled {
		return health.LeaseState{Enabled: false}, nil
	}
	if c.clientset == nil {
		return health.LeaseState{}, fmt.Errorf("kubernetes client unavailable")
	}
	lease, err := c.clientset.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(ctx, c.nodeName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return health.LeaseState{Enabled: true, Found: false, Stale: c.leaseDuration}, nil
		}
		return health.LeaseState{}, err
	}
	st := health.LeaseState{Enabled: true, Found: true, Stale: c.leaseDuration}
	if lease.Spec.RenewTime != nil {
		st.Age = time.Since(lease.Spec.RenewTime.Time)
	}
	return st, nil
}
