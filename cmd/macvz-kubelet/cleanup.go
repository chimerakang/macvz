package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/drain"
	"github.com/chimerakang/macvz/pkg/network/podnet"
	"github.com/chimerakang/macvz/pkg/network/privhelper"
	"github.com/chimerakang/macvz/pkg/runtime/container"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const cleanupTimeout = 60 * time.Second

// runCleanup reaps orphan MacVz micro-VMs and flushes the pf anchor left behind
// after a node drain or an abrupt kubelet exit (#57). It is the verification and
// recovery pass for the drain workflow: the normal eviction path already cleans
// each Pod up through DeletePod, so on a cleanly drained node this finds nothing.
//
// Safety: by default it consults the API server for the Pods still assigned to
// this node and only reaps VMs with no backing Pod. It refuses to guess when the
// API is unreachable unless --all is given (explicit "every MacVz VM is an
// orphan", for a node already drained and removed). --dry-run reports without
// changing anything.
func runCleanup(args []string) int {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	var (
		configPath  = fs.String("config", "", "path to the macvz-kubelet config")
		dryRun      = fs.Bool("dry-run", false, "report what would be reaped without changing anything")
		all         = fs.Bool("all", false, "treat every MacVz micro-VM as an orphan without consulting the API (post-drain teardown)")
		flushAnchor = fs.Bool("flush-anchor", true, "flush the pf pod-network anchor after reaping VMs")
		timeout     = fs.Duration("stop-timeout", 10*time.Second, "graceful stop timeout per micro-VM before force-destroy")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cleanup: load config: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	// Build the set of workload IDs that legitimately back live Pods on this node.
	// Skipped under --all, which intentionally reaps everything MacVz created.
	var expected map[string]bool
	if !*all {
		expected, err = expectedWorkloads(ctx, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cleanup: determine live Pods on node %q: %v\n", cfg.NodeName, err)
			fmt.Fprintln(os.Stderr, "  the API server must be reachable to tell orphans from live Pods.")
			fmt.Fprintln(os.Stderr, "  on a node already drained and removed from the cluster, re-run with --all.")
			return 1
		}
	}

	driver := container.New(container.Config{Binary: cfg.RuntimeBinary, Rosetta: cfg.RuntimeRosetta, DataRoot: cfg.RuntimeDataRoot})
	cleaner := &drain.Cleaner{
		Lister:  driver,
		Reaper:  driver,
		Flusher: anchorFlusher(cfg, *flushAnchor),
		Timeout: *timeout,
	}

	orphans, err := cleaner.Scan(ctx, expected)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cleanup: scan: %v\n", err)
		return 1
	}

	if len(orphans) == 0 {
		fmt.Printf("No orphan MacVz micro-VMs on node %q.\n", cfg.NodeName)
		// Still flush the anchor so stale pf rules from a crashed kubelet do not linger.
		if !*dryRun {
			flushOnlyAnchor(ctx, cleaner)
		}
		return 0
	}

	verb := "Reaping"
	if *dryRun {
		verb = "Would reap"
	}
	fmt.Printf("%s %d orphan MacVz micro-VM(s) on node %q:\n", verb, len(orphans), cfg.NodeName)
	for _, o := range orphans {
		fmt.Printf("  - %s (%s)\n", o.ID, o.Phase)
	}

	reaped, err := cleaner.Reap(ctx, orphans, *dryRun)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cleanup: %d of %d reaped; errors:\n%v\n", len(reaped), len(orphans), err)
		return 1
	}
	if *dryRun {
		fmt.Printf("\nDry run: nothing changed. Re-run without --dry-run to reap %d VM(s).\n", len(reaped))
		return 0
	}
	fmt.Printf("\nReaped %d micro-VM(s).", len(reaped))
	if *flushAnchor && anchorFlusher(cfg, true) != nil {
		fmt.Print(" Flushed pf anchor.")
	}
	fmt.Println()
	fmt.Println("Verify with: container list --all  (expect no macvz-* entries)")
	return 0
}

// expectedWorkloads returns the workload IDs backing Pods still assigned to this
// node, by listing them from the API. A Pod being deleted is excluded, since its
// VM is on its way out and should be reaped if it lingers.
func expectedWorkloads(ctx context.Context, cfg config.Config) (map[string]bool, error) {
	restCfg, err := cfg.RestConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	pods, err := cs.CoreV1().Pods(corev1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + cfg.NodeName,
	})
	if err != nil {
		return nil, err
	}
	expected := map[string]bool{}
	for i := range pods.Items {
		for _, id := range expectedWorkloadIDs(&pods.Items[i]) {
			expected[id] = true
		}
	}
	return expected, nil
}

// anchorFlusher returns the pf-anchor flusher for the node, or nil when flushing
// is disabled or the Pod network is not configured. It prefers the privileged
// helper (the kubelet runs unprivileged and routes pf through macvz-netd); when
// no helper socket is set it falls back to invoking pfctl directly, which the
// operator must run with sudo.
func anchorFlusher(cfg config.Config, enabled bool) drain.AnchorFlusher {
	if !enabled || !cfg.PodNetwork.Enabled {
		return nil
	}
	anchor := cfg.PodNetwork.Anchor
	if anchor == "" {
		anchor = podnet.DefaultAnchor
	}
	if cfg.PrivilegedHelperSocket != "" {
		return &helperFlusher{client: privhelper.NewClient(cfg.PrivilegedHelperSocket), anchor: anchor}
	}
	return &pfctlFlusher{anchor: anchor}
}

func flushOnlyAnchor(ctx context.Context, c *drain.Cleaner) {
	if c.Flusher == nil {
		return
	}
	if err := c.Flusher.FlushAnchor(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup: flush pf anchor: %v\n", err)
	}
}

// helperFlusher flushes the anchor through the privileged helper.
type helperFlusher struct {
	client *privhelper.Client
	anchor string
}

func (h *helperFlusher) FlushAnchor(ctx context.Context) error {
	_, stderr, code, err := h.client.Run(ctx, "pfctl", []string{"-a", h.anchor, "-F", "all"}, "")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("pfctl -a %s -F all exited %d: %s", h.anchor, code, stderr)
	}
	return nil
}

// pfctlFlusher flushes the anchor by running pfctl directly (needs root).
type pfctlFlusher struct{ anchor string }

func (p *pfctlFlusher) FlushAnchor(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "pfctl", "-a", p.anchor, "-F", "all").CombinedOutput()
	if err != nil {
		return fmt.Errorf("pfctl -a %s -F all: %v: %s", p.anchor, err, out)
	}
	return nil
}
