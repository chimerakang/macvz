package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/network/privhelper"
	"github.com/chimerakang/macvz/pkg/network/wireguard"
	"k8s.io/klog/v2"
)

// reconcileMeshPeers reloads the config from disk and re-applies its peer set to
// the running mesh, adding/removing WireGuard peers and their Pod-CIDR routes in
// place (#42). It never recreates the interface, so existing tunnels and the
// host Pod network attachments (owned by a separate podnet.Router) are
// undisturbed. wireguard.Mesh.Sync is idempotent, so reconciling an unchanged
// peer set is a no-op beyond re-applying the wg config.
func reconcileMeshPeers(ctx context.Context, mesh *wireguard.Mesh, configPath string) error {
	peers, err := loadMeshPeers(configPath)
	if err != nil {
		return err
	}
	before := mesh.Peers()
	if err := mesh.Sync(ctx, peers); err != nil {
		return fmt.Errorf("sync mesh peers: %w", err)
	}
	klog.InfoS("mesh peers reconciled",
		"interface", mesh.InterfaceName(),
		"before", before, "after", mesh.Peers(),
		"routes", mesh.InstalledRoutes())
	return nil
}

// loadMeshPeers reloads the config and resolves the desired peer set for
// reconciliation. It refuses a config that fails validation or disables the
// mesh, so a bad edit or an accidental `enabled: false` cannot silently tear
// down a live data plane on SIGHUP — the caller keeps the running peers instead.
func loadMeshPeers(configPath string) ([]wireguard.Peer, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("reload config %q: %w", configPath, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("reloaded config is invalid: %w", err)
	}
	if !cfg.Mesh.Enabled {
		return nil, fmt.Errorf("reloaded config disables the mesh; restart to tear it down")
	}
	peers, err := cfg.MeshPeers()
	if err != nil {
		return nil, fmt.Errorf("resolve mesh peers: %w", err)
	}
	return peers, nil
}

// watchMeshReload reconciles the mesh peer set whenever SIGHUP arrives, so an
// operator can add or remove MacVz nodes by editing the config and signalling
// the kubelet (`kill -HUP <pid>`) — no restart, no dropped Pod attachments. It
// runs until ctx is done. A failed reconcile is logged and the current peer set
// is kept, so a bad edit never takes the mesh down.
func watchMeshReload(ctx context.Context, mesh *wireguard.Mesh, configPath, helperSocket string) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)
	defer signal.Stop(sig)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sig:
			klog.InfoS("SIGHUP received; reconciling mesh peers from config", "config", configPath)
			if helperSocket != "" {
				if err := privhelper.NewClient(helperSocket).ReloadPolicy(ctx); err != nil {
					klog.ErrorS(err, "privileged helper policy reload failed; keeping current mesh peers")
					continue
				}
			}
			if err := reconcileMeshPeers(ctx, mesh, configPath); err != nil {
				klog.ErrorS(err, "mesh peer reconciliation failed; keeping current peers")
			}
		}
	}
}
