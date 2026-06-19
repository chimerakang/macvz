package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chimerakang/macvz/internal/version"
	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/diagbundle"
	"github.com/chimerakang/macvz/pkg/network/podnet"
	"github.com/chimerakang/macvz/pkg/network/privhelper"
	"github.com/chimerakang/macvz/pkg/network/wireguard"
	"github.com/chimerakang/macvz/pkg/runtime/container"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// bundleTimeout bounds the whole collection so a hung command (e.g. an
// unreachable API server or a wedged runtime) cannot stall the bundle.
const bundleTimeout = 60 * time.Second

// runBundle assembles a redacted diagnostic bundle for support and bug reports
// (#59). It collects config, control-plane, runtime, helper, and data-plane
// state into a timestamped directory, redacts every recognised secret, and
// (unless --no-archive) packages it into a tar.gz safe to attach to an issue.
//
// The bundle is intentionally fail-soft: every source is best-effort, and a
// source that errors records the failure in the bundle rather than aborting it,
// so a broken subsystem (the very thing being debugged) still yields a bundle.
func runBundle(args []string) int {
	fs := flag.NewFlagSet("bundle", flag.ContinueOnError)
	var (
		configPath = fs.String("config", "", "path to the macvz-kubelet config to summarise")
		out        = fs.String("out", "", "directory to write the bundle into (default: OS temp dir)")
		noArchive  = fs.Bool("no-archive", false, "leave the bundle as a directory; do not create a tar.gz")
		logFiles   = fs.String("log-file", "", "comma-separated extra log files to include (e.g. the kubelet or macvz-netd log)")
		maxEvents  = fs.Int("events", 50, "maximum recent Kubernetes events to include")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bundle: load config: %v\n", err)
		return 1
	}

	parentDir := *out
	if parentDir == "" {
		parentDir = os.TempDir()
	}
	if err := os.MkdirAll(parentDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "bundle: prepare output dir: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), bundleTimeout)
	defer cancel()

	b := diagbundle.NewBuilder()
	addBundleSources(b, cfg, *configPath, *logFiles, *maxEvents)

	res, err := b.Build(ctx, parentDir, cfg.NodeName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bundle: %v\n", err)
		return 1
	}

	final := res.Dir
	if !*noArchive {
		archive := res.Dir + ".tar.gz"
		if err := diagbundle.Archive(res.Dir, archive); err != nil {
			fmt.Fprintf(os.Stderr, "bundle: archive: %v (the directory at %s is still usable)\n", err, res.Dir)
		} else {
			_ = os.RemoveAll(res.Dir)
			final = archive
		}
	}

	var failed int
	for _, f := range res.Files {
		if f.Err != "" {
			failed++
		}
	}
	fmt.Fprintf(os.Stderr, "diagnostic bundle written: %s\n", final)
	fmt.Fprintf(os.Stderr, "  %d sources collected, %d with errors (recorded in the bundle).\n", len(res.Files), failed)
	fmt.Fprintf(os.Stderr, "  Secrets were redacted, but review the contents before sharing.\n")
	return 0
}

// addBundleSources registers every collector on the builder. Sources are scoped
// to what the config enables (mesh/podNetwork/helper), so a single-host node's
// bundle is not cluttered with skipped data-plane sections.
func addBundleSources(b *diagbundle.Builder, cfg config.Config, configPath, logFiles string, maxEvents int) {
	internalIP := cfg.Node.InternalIP
	if internalIP == "" {
		internalIP = detectInternalIP()
	}
	runtimeBin := cfg.RuntimeBinary
	if runtimeBin == "" {
		runtimeBin = container.DefaultBinary
	}

	// --- metadata & config -----------------------------------------------------
	b.Add("metadata.txt", func(context.Context) ([]byte, error) {
		host, _ := os.Hostname()
		return []byte(fmt.Sprintf(
			"macvz-kubelet %s\nnode: %s\nhost: %s\ninternalIP: %s\nruntimeBinary: %s\nmesh.enabled: %t\npodNetwork.enabled: %t\nhelperSocket: %s\n",
			version.String(), cfg.NodeName, host, internalIP, runtimeBin,
			cfg.Mesh.Enabled, cfg.PodNetwork.Enabled, emptyOr(cfg.PrivilegedHelperSocket, "(none)"),
		)), nil
	})
	b.Add("config/config-loaded.yaml", func(context.Context) ([]byte, error) {
		// Marshal the parsed config (normalised, with defaults applied). Redaction
		// strips any inline secrets; file-path references are kept for context.
		return yaml.Marshal(cfg)
	})
	if configPath != "" {
		b.Add("config/config-raw.yaml", fileSource(configPath))
	}

	// --- runtime ---------------------------------------------------------------
	b.Add("runtime/system-status.txt", cmdSource(runtimeBin, "system", "status"))
	b.Add("runtime/containers.txt", cmdSource(runtimeBin, "list", "--all"))
	b.Add("runtime/images.txt", cmdSource(runtimeBin, "image", "ls"))

	// --- control plane: node, lease, events, health ----------------------------
	addControlPlaneSources(b, cfg, internalIP, maxEvents)

	// --- data plane: helper ----------------------------------------------------
	if cfg.PrivilegedHelperSocket != "" {
		b.Add("network/helper-status.json", func(ctx context.Context) ([]byte, error) {
			st, err := privhelper.NewClient(cfg.PrivilegedHelperSocket).Status(ctx)
			if err != nil {
				return nil, err
			}
			return json.MarshalIndent(st, "", "  ")
		})
	}

	// --- data plane: routes & forwarding (no root needed) ----------------------
	b.Add("network/routes.txt", cmdSource("netstat", "-rn"))
	b.Add("network/ip-forwarding.txt", cmdSource("sysctl", "net.inet.ip.forwarding"))

	// --- data plane: mesh (interface + wg; wg/pfctl need root, captured best-effort) ---
	if cfg.Mesh.Enabled && cfg.Mesh.Interface != "" {
		b.Add("network/mesh-interface.txt", cmdSource("ifconfig", cfg.Mesh.Interface))
		b.Add("network/wireguard.txt", cmdSource("wg", "show", cfg.Mesh.Interface))
	}

	// --- data plane: pf anchor (needs root; captured best-effort) ---------------
	if cfg.PodNetwork.Enabled {
		anchor := cfg.PodNetwork.Anchor
		if anchor == "" {
			anchor = podnet.DefaultAnchor
		}
		b.Add("network/pf-anchor-rules.txt", cmdSource("pfctl", "-a", anchor, "-s", "all"))
	}

	// --- logs ------------------------------------------------------------------
	for _, lf := range splitNonEmpty(logFiles) {
		b.Add("logs/"+filepath.Base(lf), fileSource(lf))
	}
}

// addControlPlaneSources adds the API-server-backed sources (node object, lease,
// events) and the live node health report. They share one clientset, built once;
// when the API is unreachable each source records the dial error.
func addControlPlaneSources(b *diagbundle.Builder, cfg config.Config, internalIP string, maxEvents int) {
	clientFor := func() (kubernetes.Interface, error) {
		restCfg, err := cfg.RestConfig()
		if err != nil {
			return nil, err
		}
		return kubernetes.NewForConfig(restCfg)
	}

	b.Add("kubernetes/node.yaml", func(ctx context.Context) ([]byte, error) {
		cs, err := clientFor()
		if err != nil {
			return nil, err
		}
		node, err := cs.CoreV1().Nodes().Get(ctx, cfg.NodeName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return yaml.Marshal(node)
	})

	b.Add("kubernetes/events.txt", func(ctx context.Context) ([]byte, error) {
		cs, err := clientFor()
		if err != nil {
			return nil, err
		}
		list, err := cs.CoreV1().Events(corev1.NamespaceAll).List(ctx, metav1.ListOptions{Limit: int64(maxEvents)})
		if err != nil {
			return nil, err
		}
		var sb strings.Builder
		for i := range list.Items {
			e := &list.Items[i]
			fmt.Fprintf(&sb, "%s\t%s/%s\t%s\t%s: %s\n",
				e.LastTimestamp.Format(time.RFC3339), e.InvolvedObject.Kind, e.InvolvedObject.Name,
				e.Type, e.Reason, e.Message)
		}
		if sb.Len() == 0 {
			return []byte("(no events)\n"), nil
		}
		return []byte(sb.String()), nil
	})

	// Live node health report (#56): reuses the diagnostics collector so the
	// bundle states, in one place, why the node is or is not ready for workloads.
	b.Add("health/diagnostics.txt", func(ctx context.Context) ([]byte, error) {
		if data, err := fetchLiveDiagnostics(ctx, cfg, internalIP); err == nil {
			return data, nil
		} else {
			fallback, ferr := fallbackDiagnostics(ctx, cfg)
			if ferr != nil {
				return fallback, fmt.Errorf("live diagnostics unavailable: %v; fallback diagnostics failed: %w", err, ferr)
			}
			return fallback, fmt.Errorf("live diagnostics unavailable: %w", err)
		}
	})
}

func fallbackDiagnostics(ctx context.Context, cfg config.Config) ([]byte, error) {
	cs, err := func() (kubernetes.Interface, error) {
		restCfg, err := cfg.RestConfig()
		if err != nil {
			return nil, err
		}
		return kubernetes.NewForConfig(restCfg)
	}()
	if err != nil {
		// Health still reports runtime/data-plane even without the API; pass a
		// nil clientset so the control-plane checks fail cleanly rather than
		// aborting the whole report.
		cs = nil
	}
	driver := container.New(container.Config{Binary: cfg.RuntimeBinary, Rosetta: cfg.RuntimeRosetta})
	collector := newDiagnosticsCollector(cfg, driver, cs, nilMesh(), nilRouter())
	report := collector.Report(ctx)
	return []byte(report.Text()), nil
}

func fetchLiveDiagnostics(ctx context.Context, cfg config.Config, internalIP string) ([]byte, error) {
	port := fmt.Sprintf("%d", cfg.Node.KubeletPort)
	scheme := "http"
	host := "127.0.0.1"
	client := http.DefaultClient
	if cfg.Node.ServingTLSCertFile != "" && cfg.Node.ServingTLSKeyFile != "" {
		scheme = "https"
		if internalIP != "" {
			host = internalIP
		}
		client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	}
	url := fmt.Sprintf("%s://%s/healthz/diagnostics", scheme, netJoinHostPort(host, port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	body, readErr := io.ReadAll(res.Body)
	if readErr != nil {
		return nil, readErr
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusServiceUnavailable {
		return body, fmt.Errorf("GET %s returned %s", url, res.Status)
	}
	return body, nil
}

func netJoinHostPort(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// nilMesh / nilRouter make the bundle's intent explicit: the bundle command is a
// separate process from the running kubelet, so it cannot see the live in-memory
// mesh/router state. They are used only by the fallback diagnostics report when
// the running kubelet's /healthz/diagnostics endpoint cannot be reached.
func nilMesh() *wireguard.Mesh  { return nil }
func nilRouter() *podnet.Router { return nil }

// cmdSource returns a source that runs a command and captures its combined
// output, prefixed with the invocation. A non-zero exit is returned as the
// source error (recorded in the bundle) but the output is still captured, so a
// command that needs root (wg/pfctl) yields its permission error as context.
func cmdSource(name string, args ...string) func(ctx context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		header := fmt.Sprintf("$ %s %s\n", name, strings.Join(args, " "))
		if _, err := exec.LookPath(name); err != nil {
			return []byte(header), fmt.Errorf("%s not found on PATH: %w", name, err)
		}
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		return append([]byte(header), out...), err
	}
}

// fileSource returns a source that reads a file verbatim (redaction is applied
// by the builder). A missing/unreadable file is recorded as a source error.
func fileSource(path string) func(ctx context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
}

func emptyOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func splitNonEmpty(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
