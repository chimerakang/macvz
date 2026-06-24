package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/chimerakang/macvz/internal/version"
	"github.com/chimerakang/macvz/pkg/criserver"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

// linuxpod.go wires the experimental LinuxPod backend gate (CRI-R17, #124). The
// LinuxPod late-rootfs backend is reached through a helper that speaks the NDJSON
// contract in pkg/runtime/linuxpod. This adapter does not yet serve the full CRI
// surface onto that backend — that is deliberately out of scope for the R17
// prototype (no k3s in-loop, no production claim). What the gate does today is an
// honest, loud startup handshake: when enabled it connects to the helper socket
// and verifies the contract is answerable, so an operator learns immediately
// whether the LinuxPod helper is reachable rather than discovering it mid-Pod.
// The shipped apple/container CRI serving path is unchanged.

// linuxpodConfig collects the experimental LinuxPod backend flags.
type linuxpodConfig struct {
	enabled      bool
	helperSocket string
}

// linuxpodHandshakeTimeout bounds the startup Ping so an unresponsive helper
// fails the adapter fast instead of hanging before it serves.
const linuxpodHandshakeTimeout = 5 * time.Second

// validate checks the flag combination is usable. Enabling the backend without a
// helper socket is a configuration error, reported honestly rather than silently
// ignored.
func (lc linuxpodConfig) validate() error {
	if !lc.enabled {
		return nil
	}
	if lc.helperSocket == "" {
		return fmt.Errorf("--experimental-linuxpod-backend requires --linuxpod-helper-socket to point at a running LinuxPod helper")
	}
	return nil
}

// handshake validates the config and, when the backend is enabled, connects to
// the helper and performs a Ping so a misconfigured or unreachable helper fails
// loudly at startup. It returns the helper info on success. When the backend is
// disabled it is a no-op returning a zero HelperInfo and ok=false.
func (lc linuxpodConfig) handshake(ctx context.Context) (info linuxpod.HelperInfo, ok bool, err error) {
	if err := lc.validate(); err != nil {
		return linuxpod.HelperInfo{}, false, err
	}
	if !lc.enabled {
		return linuxpod.HelperInfo{}, false, nil
	}
	client := linuxpod.NewSocketClient(lc.helperSocket)
	hctx, cancel := context.WithTimeout(ctx, linuxpodHandshakeTimeout)
	defer cancel()
	info, err = client.Ping(hctx)
	if err != nil {
		return linuxpod.HelperInfo{}, false, fmt.Errorf(
			"LinuxPod helper handshake on %s failed: %w; start the helper or pass the correct --linuxpod-helper-socket",
			lc.helperSocket, err)
	}
	return info, true, nil
}

// serveLinuxPod serves the CRI lifecycle through the LinuxPod backend on lis
// (CRI-L2, #127). It connects to the helper socket, builds the LinuxPodService
// over the persisted stores, reconciles recovered records against the live
// backend, and serves until the context is cancelled. The default apple/container
// path is not constructed in this mode.
func serveLinuxPod(ctx context.Context, lis net.Listener, socketPath string, sandboxes *store.Store, containers *store.ContainerStore, pn podNetConfig, mc mountConfig, lc linuxpodConfig) error {
	backend := linuxpod.NewSocketClient(lc.helperSocket)

	// Wire the Pod network path (CRI-L3, #128) only when explicitly configured. The
	// returned cleanup flushes host pf state on shutdown; it is a no-op when off, in
	// which case LinuxPod sandboxes run without a Pod IP and report NetworkReady=false.
	podNet, ipam, netCleanup, err := setupPodNetwork(ctx, pn)
	if err != nil {
		return err
	}
	defer netCleanup()

	svc, err := criserver.NewLinuxPodService(criserver.LinuxPodOptions{
		Backend:        backend,
		Sandboxes:      sandboxes,
		Containers:     containers,
		RuntimeVersion: version.Version,
		PodNetwork:     podNet,
		IPAM:           ipam,
		Mounts: criserver.MountPolicy{
			KubeletPodsDir:          mc.kubeletPodsDir,
			HostPathAllowedPrefixes: mc.hostPathAllowed,
		},
	})
	if err != nil {
		return fmt.Errorf("build LinuxPod CRI service: %w", err)
	}

	grpcServer := grpc.NewServer()
	svc.Register(grpcServer)
	// Rebuild Pod IP reservations and re-attach surviving sandboxes so a restart
	// neither leaks addresses nor wipes other Pods' host rules. No-op when off.
	svc.RecoverNetwork(ctx)
	// Reconcile persisted records against the live backend after a restart without
	// trusting stale identity evidence (identity is a start invariant).
	svc.RecoverContainers(ctx)

	klog.InfoS("serving experimental LinuxPod-backed CRI (prototype; not the shipped Virtual Kubelet runtime)",
		"socket", socketPath, "helper", lc.helperSocket,
		"note", "lifecycle served through pkg/runtime/linuxpod backend; logs/exec/stats per helper capabilities (CRI-L2/#127)")

	go func() {
		<-ctx.Done()
		klog.InfoS("shutdown requested; stopping LinuxPod CRI server")
		grpcServer.GracefulStop()
	}()
	if err := grpcServer.Serve(lis); err != nil {
		return fmt.Errorf("serve LinuxPod CRI gRPC: %w", err)
	}
	klog.InfoS("macvz-cri (LinuxPod backend) stopped cleanly")
	return nil
}
