package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/drain"
	"github.com/chimerakang/macvz/pkg/network/wireguard"
	"github.com/chimerakang/macvz/pkg/noderemove"
	"github.com/chimerakang/macvz/pkg/runtime/container"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// removeTimeout bounds the whole removal so a hung API server, runtime, or pf
// command cannot stall the teardown indefinitely.
const removeTimeout = 90 * time.Second

// runRemove permanently removes this Mac from the node pool (#58): it deletes the
// Kubernetes Node object, destroys every MacVz micro-VM, flushes the pod-network
// pf anchor, and tears down the WireGuard mesh — in that order, best-effort, so a
// failure in one step never strands the others. It is idempotent and safe to
// re-run after a partial removal.
//
// Stop the kubelet first: while macvz-kubelet runs, Virtual Kubelet re-registers
// the Node object and recreates micro-VMs, so removal would not converge.
//
// Because removal is irreversible, it does nothing without an explicit --yes
// (unless --dry-run); without it, it prints the plan and exits, so an accidental
// invocation cannot remove a node.
func runRemove(args []string) int {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	var (
		configPath = fs.String("config", "", "path to the macvz-kubelet config")
		dryRun     = fs.Bool("dry-run", false, "report what would be removed without changing anything")
		yes        = fs.Bool("yes", false, "confirm permanent removal (required to make changes)")
		keepNode   = fs.Bool("keep-node", false, "do not delete the Kubernetes Node object (leave it for manual handling)")
		timeout    = fs.Duration("stop-timeout", 10*time.Second, "graceful stop timeout per micro-VM before force-destroy")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "remove: load config: %v\n", err)
		return 1
	}

	// Safety gate: without --yes (and not a dry run) we plan but do not act.
	effectiveDryRun := *dryRun
	if !*yes && !*dryRun {
		fmt.Fprintf(os.Stderr, "remove: planning only — re-run with --yes to permanently remove node %q.\n", cfg.NodeName)
		fmt.Fprintln(os.Stderr, "  (stop macvz-kubelet first, or it will re-register the node and recreate VMs.)")
		effectiveDryRun = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()

	opts := noderemove.Options{
		Node:   cfg.NodeName,
		DryRun: effectiveDryRun,
		VMs:    buildVMReaper(cfg, *timeout),
		PF:     buildPFFlusher(cfg),
		Mesh:   buildMeshRemover(cfg),
	}
	if !*keepNode {
		nodes, err := buildNodeDeleter(cfg)
		if err != nil {
			// Without the API we cannot delete the Node object, but the local
			// teardown (VMs, pf, mesh) can still proceed; warn and continue.
			fmt.Fprintf(os.Stderr, "remove: Kubernetes API unavailable, skipping Node-object delete: %v\n", err)
			fmt.Fprintln(os.Stderr, "  delete it from a machine with cluster access: kubectl delete node "+cfg.NodeName)
		} else {
			opts.Nodes = nodes
		}
	}

	res := noderemove.Run(ctx, opts)
	fmt.Print(res.Render())
	if !res.OK() {
		fmt.Fprintln(os.Stderr, "remove: one or more steps failed; resolve the cause and re-run (removal is idempotent).")
		return 1
	}
	return 0
}

// buildVMReaper adapts a drain.Cleaner (the #57 reaper) to noderemove.VMReaper:
// on a node being removed every MacVz micro-VM is an orphan (no Pod should
// remain), so it scans with an empty expected set and reaps all. pf flushing is
// handled by the separate flush-pf step, so the Cleaner's own Flusher is left
// nil here to avoid a double flush.
func buildVMReaper(cfg config.Config, timeout time.Duration) noderemove.VMReaper {
	driver := container.New(container.Config{Binary: cfg.RuntimeBinary, Rosetta: cfg.RuntimeRosetta})
	return &cleanerReaper{cleaner: &drain.Cleaner{Lister: driver, Reaper: driver, Timeout: timeout}}
}

type cleanerReaper struct{ cleaner *drain.Cleaner }

func (r *cleanerReaper) ReapAll(ctx context.Context, dryRun bool) ([]string, error) {
	orphans, err := r.cleaner.Scan(ctx, nil) // nil expected → every MacVz VM is an orphan
	if err != nil {
		return nil, err
	}
	return r.cleaner.Reap(ctx, orphans, dryRun)
}

// buildPFFlusher reuses the cleanup command's helper/pfctl anchor flusher. It is
// nil when the Pod network is disabled, which skips the flush-pf step.
func buildPFFlusher(cfg config.Config) noderemove.PFFlusher {
	f := anchorFlusher(cfg, true)
	if f == nil {
		return nil // typed-nil guard: return an untyped nil so the step is skipped
	}
	return f
}

// buildMeshRemover builds the WireGuard mesh teardown for this node. It is nil
// when the mesh is disabled, which skips the mesh-down step. Privileged route/
// interface operations route through the helper when a socket is configured,
// matching the running kubelet.
func buildMeshRemover(cfg config.Config) noderemove.MeshRemover {
	if !cfg.Mesh.Enabled {
		return nil
	}
	ifc, err := cfg.MeshInterfaceConfig()
	if err != nil {
		// A malformed mesh config should not strand the rest of removal; warn and
		// skip mesh-down (the operator can tear the interface down by hand).
		fmt.Fprintf(os.Stderr, "remove: cannot resolve mesh config, skipping mesh-down: %v\n", err)
		return nil
	}
	var meshOpts []wireguard.Option
	if cfg.PrivilegedHelperSocket != "" {
		meshOpts = append(meshOpts, wireguard.WithHelperSocket(cfg.PrivilegedHelperSocket))
	}
	return wireguard.New(ifc, meshOpts...)
}

// buildNodeDeleter builds a Kubernetes Node deleter from the config's kubeconfig.
func buildNodeDeleter(cfg config.Config) (noderemove.NodeDeleter, error) {
	restCfg, err := cfg.RestConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	return &nodeDeleter{cs: cs}, nil
}

// nodeDeleter deletes the Kubernetes Node object, treating an already-absent node
// as success so removal is idempotent.
type nodeDeleter struct{ cs kubernetes.Interface }

func (d *nodeDeleter) DeleteNode(ctx context.Context, name string) error {
	err := d.cs.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
