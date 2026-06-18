package wireguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"k8s.io/klog/v2"
)

// Default tool names. They are resolved through PATH (Homebrew's wireguard-tools
// and the system route/ifconfig), never hardcoded absolute paths.
const (
	defaultWG          = "wg"
	defaultWireGuardGo = "wireguard-go"
	defaultIfconfig    = "ifconfig"
	defaultRoute       = "route"
)

// tools names the external binaries the Mesh drives.
type tools struct {
	wg          string
	wireguardGo string
	ifconfig    string
	route       string
}

// Mesh manages this node's WireGuard interface and the host routes that steer
// remote-Pod traffic into the tunnel. It is safe for concurrent use.
//
// The lifecycle is: Up (create interface, apply config, assign address, install
// routes), Sync (reconcile peers and routes in place as nodes join/leave), and
// Down (tear everything back down). Sync never recreates the interface, so the
// data path is not interrupted for unrelated peers.
type Mesh struct {
	run   runner
	tools tools

	mu     sync.Mutex
	cfg    InterfaceConfig
	routes map[string]bool // route targets currently installed
	up     bool
}

// Option configures a Mesh.
type Option func(*Mesh)

// WithRunner injects a command runner (used by tests).
func WithRunner(r runner) Option { return func(m *Mesh) { m.run = r } }

// WithTools overrides the external binary names. Empty values keep the default.
func WithTools(wg, wireguardGo, ifconfig, route string) Option {
	return func(m *Mesh) {
		if wg != "" {
			m.tools.wg = wg
		}
		if wireguardGo != "" {
			m.tools.wireguardGo = wireguardGo
		}
		if ifconfig != "" {
			m.tools.ifconfig = ifconfig
		}
		if route != "" {
			m.tools.route = route
		}
	}
}

// New builds a Mesh for the given interface configuration.
func New(cfg InterfaceConfig, opts ...Option) *Mesh {
	m := &Mesh{
		run:    cliRunner{},
		tools:  tools{wg: defaultWG, wireguardGo: defaultWireGuardGo, ifconfig: defaultIfconfig, route: defaultRoute},
		cfg:    cfg,
		routes: map[string]bool{},
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// InterfaceName returns the managed interface name.
func (m *Mesh) InterfaceName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Name
}

// Peers returns the names of the peers currently configured.
func (m *Mesh) Peers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.cfg.Peers))
	for _, p := range m.cfg.Peers {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

// Up creates the WireGuard interface, applies the configuration, assigns the
// node's mesh address, and installs host routes for every peer's AllowedIPs.
func (m *Mesh) Up(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.cfg.Validate(); err != nil {
		return err
	}
	ip, err := addressIP(m.cfg.Address)
	if err != nil {
		return err
	}

	// Create the userspace interface. Tolerate "already exists" so Up is safe to
	// re-run after a partial start.
	if _, err := m.runTolerating(ctx, command{Name: m.tools.wireguardGo, Args: []string{m.cfg.Name}}, errInterfaceExists); err != nil {
		return fmt.Errorf("create interface %q: %w", m.cfg.Name, err)
	}
	// Apply WireGuard peers/keys via stdin (no config file on disk).
	if _, err := m.run.run(ctx, command{Name: m.tools.wg, Args: []string{"setconf", m.cfg.Name, "/dev/stdin"}, Stdin: m.cfg.SyncConfig()}); err != nil {
		return fmt.Errorf("apply wireguard config to %q: %w", m.cfg.Name, err)
	}
	// Assign the mesh address (macOS point-to-point alias) and bring the link up.
	if _, err := m.runTolerating(ctx, command{Name: m.tools.ifconfig, Args: []string{m.cfg.Name, "inet", ip, ip, "alias"}}, errAddressExists); err != nil {
		return fmt.Errorf("assign address %s to %q: %w", ip, m.cfg.Name, err)
	}
	if m.cfg.MTU > 0 {
		if _, err := m.run.run(ctx, command{Name: m.tools.ifconfig, Args: []string{m.cfg.Name, "mtu", strconv.Itoa(m.cfg.MTU)}}); err != nil {
			return fmt.Errorf("set mtu %d on %q: %w", m.cfg.MTU, m.cfg.Name, err)
		}
	}
	if _, err := m.run.run(ctx, command{Name: m.tools.ifconfig, Args: []string{m.cfg.Name, "up"}}); err != nil {
		return fmt.Errorf("bring up %q: %w", m.cfg.Name, err)
	}

	m.up = true
	if err := m.reconcileRoutesLocked(ctx, m.cfg.RouteTargets()); err != nil {
		return err
	}
	klog.InfoS("wireguard mesh up", "interface", m.cfg.Name, "address", m.cfg.Address, "peers", len(m.cfg.Peers), "routes", len(m.routes))
	return nil
}

// Sync reconciles the mesh to a new peer set without recreating the interface:
// it re-applies the WireGuard config with `wg syncconf` (which adds and removes
// peers atomically) and adds/removes host routes to match. It is the path used
// when nodes join or leave the cluster.
func (m *Mesh) Sync(ctx context.Context, peers []Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := m.cfg
	next.Peers = peers
	if err := next.Validate(); err != nil {
		return err
	}
	if !m.up {
		return fmt.Errorf("wireguard: Sync called before Up on %q", m.cfg.Name)
	}

	if _, err := m.run.run(ctx, command{Name: m.tools.wg, Args: []string{"syncconf", m.cfg.Name, "/dev/stdin"}, Stdin: next.SyncConfig()}); err != nil {
		return fmt.Errorf("sync wireguard config on %q: %w", m.cfg.Name, err)
	}
	m.cfg = next
	if err := m.reconcileRoutesLocked(ctx, next.RouteTargets()); err != nil {
		return err
	}
	klog.InfoS("wireguard mesh synced", "interface", m.cfg.Name, "peers", len(peers), "routes", len(m.routes))
	return nil
}

// Down removes installed routes and tears the interface down. It is best-effort:
// it logs and continues past individual failures so a partially-applied mesh can
// always be cleaned up.
func (m *Mesh) Down(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for target := range m.routes {
		if _, err := m.runTolerating(ctx, m.routeCmd("delete", target), errRouteMissing); err != nil {
			klog.ErrorS(err, "wireguard: failed to delete route", "target", target, "interface", m.cfg.Name)
		}
	}
	m.routes = map[string]bool{}

	if _, err := m.runTolerating(ctx, command{Name: m.tools.ifconfig, Args: []string{m.cfg.Name, "down"}}, errInterfaceMissing); err != nil {
		klog.ErrorS(err, "wireguard: failed to bring interface down", "interface", m.cfg.Name)
	}
	// wireguard-go installs the utun; remove it so the name is reusable.
	if _, err := m.runTolerating(ctx, command{Name: m.tools.ifconfig, Args: []string{m.cfg.Name, "destroy"}}, errInterfaceMissing); err != nil {
		klog.ErrorS(err, "wireguard: failed to destroy interface", "interface", m.cfg.Name)
	}
	m.up = false
	klog.InfoS("wireguard mesh down", "interface", m.cfg.Name)
	return nil
}

// InstalledRoutes returns the route targets currently installed, sorted. It
// lets operators (and tests) confirm remote Pod CIDRs are routed through the
// tunnel, satisfying the "routes installed and visible" acceptance criterion.
func (m *Mesh) InstalledRoutes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.routes))
	for r := range m.routes {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// reconcileRoutesLocked installs routes present in desired but missing, and
// removes routes installed but no longer desired. Caller holds m.mu.
func (m *Mesh) reconcileRoutesLocked(ctx context.Context, desired []string) error {
	want := make(map[string]bool, len(desired))
	for _, t := range desired {
		want[t] = true
	}
	// Remove stale routes first so a CIDR that moved to another peer is not
	// briefly routed to two interfaces.
	for target := range m.routes {
		if want[target] {
			continue
		}
		if _, err := m.runTolerating(ctx, m.routeCmd("delete", target), errRouteMissing); err != nil {
			return fmt.Errorf("delete route %s: %w", target, err)
		}
		delete(m.routes, target)
	}
	for _, target := range desired {
		if m.routes[target] {
			continue
		}
		if _, err := m.runTolerating(ctx, m.routeCmd("add", target), errRouteExists); err != nil {
			return fmt.Errorf("add route %s: %w", target, err)
		}
		m.routes[target] = true
	}
	return nil
}

// routeCmd builds a macOS `route` command for add/delete of a CIDR through the
// managed interface.
func (m *Mesh) routeCmd(op, target string) command {
	return command{Name: m.tools.route, Args: []string{"-q", "-n", op, routeFamily(target), target, "-interface", m.cfg.Name}}
}

func routeFamily(target string) string {
	ip, _, err := net.ParseCIDR(target)
	if err != nil || ip.To4() != nil {
		return "-inet"
	}
	return "-inet6"
}

// runTolerating runs a command, treating an error whose stderr contains any of
// the given benign substrings as success. This makes interface/route operations
// idempotent (e.g. "route already in table", "interface already exists").
func (m *Mesh) runTolerating(ctx context.Context, c command, benign ...string) (string, error) {
	out, err := m.run.run(ctx, c)
	if err == nil {
		return out, nil
	}
	var ce *CommandError
	if errors.As(err, &ce) {
		msg := strings.ToLower(ce.Stderr)
		for _, b := range benign {
			if b != "" && strings.Contains(msg, b) {
				return out, nil
			}
		}
	}
	return out, err
}

// Benign stderr fragments that make operations idempotent across re-runs.
const (
	errInterfaceExists  = "already exists"
	errInterfaceMissing = "does not exist"
	errAddressExists    = "already in list"
	errRouteExists      = "file exists"
	errRouteMissing     = "not in table"
)

// addressIP extracts the bare IP from a CIDR mesh address.
func addressIP(cidr string) (string, error) {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("wireguard: parse mesh address %q: %w", cidr, err)
	}
	return ip.String(), nil
}
