// Command macvz-netd is the MacVz privileged network helper daemon (#38, #40).
//
// It runs as root and executes a fixed allowlist of network commands (pfctl,
// sysctl, route, ifconfig, wg, wireguard-go, pkill) on behalf of the user-run
// macvz-kubelet, which connects over a unix socket. This keeps the kubelet
// itself running as the operator's user — required because apple/container is a
// per-user service and refuses to run as root — while confining root to exactly
// the network operations the data plane needs.
//
// Usage:
//
//	macvz-netd serve   [--socket PATH] --config PATH [--owner uid:gid]   run the daemon (default)
//	macvz-netd install [--socket PATH] --config PATH [--owner uid:gid]   install + start the LaunchDaemon
//	macvz-netd uninstall                                   stop + remove the LaunchDaemon
//	macvz-netd load | unload                               (re)bootstrap an installed job
//	macvz-netd status                                      report install/run state
//
// Run it directly once, as root, before the kubelet:
//
//	sudo macvz-netd serve --socket /var/run/macvz-netd.sock --config /etc/macvz/config.yaml
//
// Or install it as a LaunchDaemon so it starts at boot and the kubelet never
// needs sudo (the installing user, captured via sudo, owns the socket):
//
//	sudo macvz-netd install --socket /var/run/macvz-netd.sock --config /etc/macvz/config.yaml
//
// When launched via sudo `serve` chowns the socket to $SUDO_UID/$SUDO_GID so the
// invoking (non-root) user can connect, and no other user can. Under launchd
// there is no SUDO_UID, so the owner baked into the plist via --owner is used.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/chimerakang/macvz/internal/version"
	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/network/privhelper"
	"k8s.io/klog/v2"
)

const defaultSocket = "/var/run/macvz-netd.sock"

func main() {
	klog.InitFlags(nil)

	// Subcommand dispatch. With no subcommand (or a leading flag) we default to
	// `serve` so `sudo macvz-netd --socket ...` keeps working as before.
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "install":
		err = runInstall(args)
	case "uninstall":
		err = runInstaller(args, "uninstall")
	case "load":
		err = runInstaller(args, "load")
	case "unload":
		err = runInstaller(args, "unload")
	case "status":
		err = runStatus(args)
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "macvz-netd: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		klog.ErrorS(err, "macvz-netd "+cmd+" failed")
		os.Exit(1)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func usage() {
	fmt.Fprint(os.Stderr, `macvz-netd — MacVz privileged network helper daemon

Commands:
  serve      [--socket PATH] --config PATH [--owner uid:gid]   run the daemon (default)
  install    [--socket PATH] --config PATH [--owner uid:gid]   install and start the LaunchDaemon (sudo)
  uninstall                                       stop and remove the LaunchDaemon (sudo)
  load                                            bootstrap an installed job (sudo)
  unload                                          boot out a running job (sudo)
  status                                          report install and run state

Development only: pass --allow-unsafe-no-config with serve/install to skip
config-derived request policy.
`)
}

// cmdFlags holds the flags a subcommand parsed.
type cmdFlags struct {
	socket              string
	owner               string
	config              string
	allowUnsafeNoConfig bool
}

// parseFlags builds and parses a FlagSet for a subcommand, wiring the shared
// --socket/--owner/--config flags when requested.
func parseFlags(name string, args []string, socket, owner bool) (cmdFlags, error) {
	var f cmdFlags
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	if socket {
		fs.StringVar(&f.socket, "socket", defaultSocket, "unix socket path to listen on")
		// --config travels with --socket: both serve and install accept it.
		fs.StringVar(&f.config, "config", "", "path to the MacVz config; restricts privileged commands to its interfaces, CIDRs, peers, and pf anchor (#41)")
		fs.BoolVar(&f.allowUnsafeNoConfig, "allow-unsafe-no-config", false, "development only: allow helper startup without config-derived request policy")
	}
	if owner {
		fs.StringVar(&f.owner, "owner", "", "uid:gid to own the socket (defaults to $SUDO_UID:$SUDO_GID)")
	}
	return f, fs.Parse(args)
}

// helperServer builds the helper server, enforcing a config-derived policy in
// production and allowing name-allowlist-only mode only behind an explicit unsafe
// development flag.
func helperServer(socket, configPath string, allowUnsafeNoConfig bool) (*privhelper.Server, error) {
	if configPath == "" {
		if !allowUnsafeNoConfig {
			return nil, fmt.Errorf("macvz-netd requires --config to enforce per-request network policy; pass --allow-unsafe-no-config only for local development")
		}
		klog.Warning("macvz-netd started with --allow-unsafe-no-config: privileged commands are restricted to the command allowlist only, not to this node's interfaces/CIDRs/peers/anchor.")
		return privhelper.NewServer(socket).SetVersion(version.Version), nil
	}
	loader := func() (privhelper.Policy, error) {
		cfg, err := config.Load(configPath)
		if err != nil {
			return privhelper.Policy{}, fmt.Errorf("load config %q: %w", configPath, err)
		}
		policy, err := cfg.PrivilegedHelperPolicy()
		if err != nil {
			return privhelper.Policy{}, fmt.Errorf("derive privileged helper policy from %q: %w", configPath, err)
		}
		klog.InfoS("macvz-netd enforcing request policy",
			"config", configPath, "meshInterface", policy.MeshInterface,
			"vmnetInterface", policy.VMNetInterface, "anchor", policy.Anchor,
			"routeCIDRs", len(policy.RouteCIDRs), "podCIDRs", len(policy.PodCIDRs),
			"vmNetCIDRs", len(policy.VMNetCIDRs), "peers", len(policy.PeerPublicKeys))
		return policy, nil
	}
	srv, err := privhelper.NewServerWithPolicyLoader(socket, loader)
	if err != nil {
		return nil, err
	}
	return srv.SetVersion(version.Version), nil
}

// runServe runs the helper daemon until SIGINT/SIGTERM.
func runServe(args []string) error {
	f, err := parseFlags("serve", args, true, true)
	if err != nil {
		return err
	}

	if os.Geteuid() != 0 {
		klog.Warning("macvz-netd is not running as root; privileged commands will fail. Start it with sudo.")
	}

	uid, gid, err := resolveOwner(f.owner)
	if err != nil {
		return err
	}

	srv, err := helperServer(f.socket, f.config, f.allowUnsafeNoConfig)
	if err != nil {
		return err
	}
	if err := srv.Listen(uid, gid); err != nil {
		return fmt.Errorf("create helper socket %q: %w", f.socket, err)
	}
	defer func() { _ = srv.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := srv.Serve(ctx); err != nil {
		return fmt.Errorf("helper server stopped: %w", err)
	}
	klog.InfoS("macvz-netd stopped")
	return nil
}

// runInstall copies this binary to the system location, writes the LaunchDaemon
// plist, and starts the job.
func runInstall(args []string) error {
	f, err := parseFlags("install", args, true, true)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("install must run as root: sudo macvz-netd install")
	}

	uid, gid, err := resolveOwner(f.owner)
	if err != nil {
		return err
	}
	if uid < 0 {
		return fmt.Errorf("install: cannot determine the owning user; run under sudo or pass --owner uid:gid")
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running binary: %w", err)
	}

	cfg := privhelper.DefaultLaunchdConfig(f.socket)
	cfg.OwnerUID, cfg.OwnerGID = uid, gid
	cfg.ConfigPath = f.config
	cfg.AllowUnsafeNoConfig = f.allowUnsafeNoConfig

	if err := privhelper.NewInstaller(cfg).Install(context.Background(), self); err != nil {
		return err
	}
	klog.InfoS("macvz-netd installed and started",
		"plist", cfg.PlistPath(), "binary", cfg.BinaryPath, "socket", cfg.SocketPath,
		"config", cfg.ConfigPath, "owner", fmt.Sprintf("%d:%d", uid, gid),
		"logs", cfg.StdoutPath, "logRotation", cfg.NewsyslogPath)
	if cfg.AllowUnsafeNoConfig {
		klog.Warning("installed with --allow-unsafe-no-config: per-request policy validation is disabled. Reinstall with --config <path> to confine the helper to this node's interfaces/CIDRs/peers/anchor (#41).")
	}
	return nil
}

// runInstaller handles uninstall/load/unload, which need only the default
// layout (label + plist path), not the socket/owner.
func runInstaller(args []string, action string) error {
	if _, err := parseFlags(action, args, false, false); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("%s must run as root: sudo macvz-netd %s", action, action)
	}
	inst := privhelper.NewInstaller(privhelper.DefaultLaunchdConfig(defaultSocket))
	ctx := context.Background()
	switch action {
	case "uninstall":
		if err := inst.Uninstall(ctx); err != nil {
			return err
		}
		klog.InfoS("macvz-netd uninstalled")
	case "load":
		if err := inst.Load(ctx); err != nil {
			return err
		}
		klog.InfoS("macvz-netd loaded")
	case "unload":
		if err := inst.Unload(ctx); err != nil {
			return err
		}
		klog.InfoS("macvz-netd unloaded")
	}
	return nil
}

// runStatus prints whether the daemon is installed and loaded.
func runStatus(args []string) error {
	if _, err := parseFlags("status", args, false, false); err != nil {
		return err
	}
	inst := privhelper.NewInstaller(privhelper.DefaultLaunchdConfig(defaultSocket))
	st := inst.Status(context.Background())
	fmt.Printf("plist installed:  %t (%s)\n", st.PlistInstalled, inst.Cfg.PlistPath())
	fmt.Printf("binary installed: %t (%s)\n", st.BinaryInstalled, inst.Cfg.BinaryPath)
	fmt.Printf("socket present:   %t (%s)\n", st.SocketPresent, inst.Cfg.SocketPath)
	fmt.Printf("launchd loaded:   %t\n", st.Loaded)
	if st.Detail != "" {
		fmt.Printf("detail:\n%s\n", st.Detail)
	}
	return nil
}

// resolveOwner picks the socket owner: an explicit --owner uid:gid wins,
// otherwise fall back to the sudo-provided $SUDO_UID/$SUDO_GID.
func resolveOwner(ownerFlag string) (uid, gid int, err error) {
	if ownerFlag != "" {
		return privhelper.ResolveOwnerSpec(ownerFlag)
	}
	uid, gid = privhelper.ResolveOwner(os.Getenv("SUDO_UID"), os.Getenv("SUDO_GID"))
	return uid, gid, nil
}
