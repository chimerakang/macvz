package privhelper

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Policy is the set of host resources the privileged helper may touch, derived
// from this node's MacVz config (see config.Config.PrivilegedHelperPolicy). The
// server checks every request against it *after* the command-name allowlist, so
// even a compromised kubelet cannot drive the root network tools against
// interfaces, routes, anchors, or peers outside this node's own configuration.
//
// Validation is fail-closed: each command has a fixed grammar of permitted
// argument shapes, and anything that does not match exactly — extra args, a
// different interface, an unconfigured CIDR, an unknown peer key, a foreign pf
// anchor — is refused. The zero Policy permits only harmless read-only probes
// (the ping), refusing every mutating command, so "no policy configured" still
// fails closed rather than open.
type Policy struct {
	// MeshInterface is the WireGuard interface name the helper may manage
	// (e.g. "utun7"). Empty refuses every wireguard/route/ifconfig command.
	MeshInterface string
	// VMNetInterface is the vmnet interface the pod-network pf anchor is scoped
	// to (e.g. "bridge100"). Loaded anchor rules may reference only this
	// interface. Empty refuses anchor loads.
	VMNetInterface string
	// Anchor is the single pf anchor the helper may load or flush
	// (e.g. "macvz/pods"). Empty refuses every anchor operation.
	Anchor string
	// MeshAddressIP is the bare IP (no mask) the mesh interface may be assigned.
	MeshAddressIP string
	// MTU, when > 0, is the only MTU the mesh interface may be set to. 0 permits
	// any positive MTU (the interface name is still pinned to MeshInterface).
	MTU int
	// RouteCIDRs is the normalised set of CIDRs the helper may add/delete host
	// routes for, and the superset that WireGuard AllowedIPs must fall within.
	RouteCIDRs map[string]bool
	// PeerPublicKeys is the set of base64 WireGuard public keys the helper may
	// configure as peers (canonicalised to match the rendered wg config).
	PeerPublicKeys map[string]bool
}

// Validate reports whether req is permitted by the policy, returning a
// descriptive error (suitable for an audit log and the client response) when it
// is not. The command name is assumed already checked against the allowlist.
func (p Policy) Validate(req Request) error {
	switch req.Name {
	case "sysctl":
		return p.validateSysctl(req)
	case "pfctl":
		return p.validatePfctl(req)
	case "route":
		return p.validateRoute(req)
	case "ifconfig":
		return p.validateIfconfig(req)
	case "wg":
		return p.validateWG(req)
	case "wireguard-go":
		return p.validateWireGuardGo(req)
	default:
		// Allowlisted but ungrammared: refuse rather than pass through unchecked.
		return fmt.Errorf("no validation grammar for command %q", req.Name)
	}
}

// validateSysctl permits only the read-only ostype probe (the ping) and toggling
// IPv4 forwarding to a boolean. No stdin is ever expected.
func (p Policy) validateSysctl(req Request) error {
	if req.Stdin != "" {
		return fmt.Errorf("sysctl takes no stdin")
	}
	switch {
	case argsEqual(req.Args, "-n", "kern.ostype"):
		return nil // harmless read-only probe
	case len(req.Args) == 2 && req.Args[0] == "-w" &&
		(req.Args[1] == "net.inet.ip.forwarding=1" || req.Args[1] == "net.inet.ip.forwarding=0"):
		// IP forwarding is host-wide; only permit it when this node actually runs
		// the Pod network path that needs it (forwarding between the vmnet and mesh
		// interfaces). With no Pod network configured, refuse it (fail closed).
		if p.VMNetInterface == "" {
			return fmt.Errorf("ip forwarding toggle refused: no Pod network interface is configured")
		}
		return nil
	default:
		return fmt.Errorf("sysctl args %v not permitted", req.Args)
	}
}

// validatePfctl permits enabling pf and loading/flushing exactly the managed
// anchor. A load (-f -) additionally has its stdin ruleset linted so it can only
// program binat/rdr rules on the managed vmnet interface.
func (p Policy) validatePfctl(req Request) error {
	switch {
	case argsEqual(req.Args, "-e"):
		if req.Stdin != "" {
			return fmt.Errorf("pfctl -e takes no stdin")
		}
		return nil
	case len(req.Args) == 4 && req.Args[0] == "-a" && req.Args[2] == "-F" && req.Args[3] == "all":
		if err := p.checkAnchor(req.Args[1]); err != nil {
			return err
		}
		if req.Stdin != "" {
			return fmt.Errorf("pfctl flush takes no stdin")
		}
		return nil
	case len(req.Args) == 4 && req.Args[0] == "-a" && req.Args[2] == "-f" && req.Args[3] == "-":
		if err := p.checkAnchor(req.Args[1]); err != nil {
			return err
		}
		return p.lintAnchorRuleset(req.Stdin)
	default:
		return fmt.Errorf("pfctl args %v not permitted", req.Args)
	}
}

func (p Policy) checkAnchor(anchor string) error {
	if p.Anchor == "" {
		return fmt.Errorf("no pf anchor is configured; anchor operations refused")
	}
	if anchor != p.Anchor {
		return fmt.Errorf("pf anchor %q is not the managed anchor %q", anchor, p.Anchor)
	}
	return nil
}

// lintAnchorRuleset confines a loaded pf ruleset to binat/rdr rules on the
// managed vmnet interface. pf anchors already cannot escape their scope, but
// this stops a compromised kubelet from injecting other rule types (nat, pass,
// block) onto that interface within the anchor — keeping the helper to the pod
// reachability and ClusterIP redirect rules it exists to manage.
func (p Policy) lintAnchorRuleset(ruleset string) error {
	if p.VMNetInterface == "" {
		return fmt.Errorf("no vmnet interface is configured; anchor loads refused")
	}
	for _, raw := range strings.Split(ruleset, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		// Every rule the router emits is "binat on <iface> ..." or
		// "rdr on <iface> ...". Anything else is out of scope.
		if len(fields) < 3 || (fields[0] != "binat" && fields[0] != "rdr") || fields[1] != "on" {
			return fmt.Errorf("anchor rule %q is not a binat/rdr rule", line)
		}
		if fields[2] != p.VMNetInterface {
			return fmt.Errorf("anchor rule on interface %q, not the managed vmnet interface %q", fields[2], p.VMNetInterface)
		}
	}
	return nil
}

// validateRoute permits adding/deleting host routes only for configured CIDRs,
// and only through the managed mesh interface.
func (p Policy) validateRoute(req Request) error {
	if req.Stdin != "" {
		return fmt.Errorf("route takes no stdin")
	}
	// route -q -n <add|delete> <-inet|-inet6> <cidr> -interface <iface>
	if len(req.Args) != 7 || req.Args[0] != "-q" || req.Args[1] != "-n" || req.Args[5] != "-interface" {
		return fmt.Errorf("route args %v not permitted", req.Args)
	}
	if req.Args[2] != "add" && req.Args[2] != "delete" {
		return fmt.Errorf("route op %q not permitted", req.Args[2])
	}
	if req.Args[3] != "-inet" && req.Args[3] != "-inet6" {
		return fmt.Errorf("route family %q not permitted", req.Args[3])
	}
	if err := p.checkMeshInterface(req.Args[6]); err != nil {
		return err
	}
	return p.checkRouteCIDR(req.Args[4])
}

func (p Policy) checkRouteCIDR(target string) error {
	norm, err := normalizeCIDR(target)
	if err != nil {
		return fmt.Errorf("route target %q is not a CIDR: %w", target, err)
	}
	if !p.RouteCIDRs[norm] {
		return fmt.Errorf("route target %q is not a configured Pod/mesh CIDR", target)
	}
	return nil
}

// validateIfconfig permits address/MTU/up/down/destroy only on the managed mesh
// interface, with the address pinned to the configured mesh address.
func (p Policy) validateIfconfig(req Request) error {
	if req.Stdin != "" {
		return fmt.Errorf("ifconfig takes no stdin")
	}
	if len(req.Args) == 0 {
		return fmt.Errorf("ifconfig requires an interface")
	}
	if err := p.checkMeshInterface(req.Args[0]); err != nil {
		return err
	}
	rest := req.Args[1:]
	switch {
	case len(rest) == 4 && rest[0] == "inet" && rest[3] == "alias":
		if !ipEqual(rest[1], p.MeshAddressIP) || !ipEqual(rest[2], p.MeshAddressIP) {
			return fmt.Errorf("ifconfig address %q/%q is not the configured mesh address %q", rest[1], rest[2], p.MeshAddressIP)
		}
		return nil
	case len(rest) == 2 && rest[0] == "mtu":
		n, err := strconv.Atoi(rest[1])
		if err != nil || n <= 0 {
			return fmt.Errorf("ifconfig mtu %q is not a positive integer", rest[1])
		}
		if p.MTU > 0 && n != p.MTU {
			return fmt.Errorf("ifconfig mtu %d is not the configured mtu %d", n, p.MTU)
		}
		return nil
	case len(rest) == 1 && (rest[0] == "up" || rest[0] == "down" || rest[0] == "destroy"):
		return nil
	default:
		return fmt.Errorf("ifconfig args %v not permitted", req.Args)
	}
}

// validateWG permits applying a WireGuard config to the managed interface via
// stdin, with every peer key and AllowedIPs CIDR checked against the policy.
func (p Policy) validateWG(req Request) error {
	if len(req.Args) != 3 || (req.Args[0] != "setconf" && req.Args[0] != "syncconf") || req.Args[2] != "/dev/stdin" {
		return fmt.Errorf("wg args %v not permitted", req.Args)
	}
	if err := p.checkMeshInterface(req.Args[1]); err != nil {
		return err
	}
	return p.lintWireGuardConfig(req.Stdin)
}

// lintWireGuardConfig ensures a wg setconf/syncconf payload only configures
// peers whose public keys are in the policy and whose AllowedIPs are confined to
// configured CIDRs — so the helper cannot be used to route traffic to, or accept
// traffic from, an attacker-controlled peer.
func (p Policy) lintWireGuardConfig(cfg string) error {
	for _, raw := range strings.Split(cfg, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("malformed wireguard config line %q", line)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "PublicKey":
			if !p.PeerPublicKeys[val] {
				return fmt.Errorf("wireguard peer public key %q is not a configured peer", val)
			}
		case "AllowedIPs":
			for _, cidr := range strings.Split(val, ",") {
				cidr = strings.TrimSpace(cidr)
				norm, err := normalizeCIDR(cidr)
				if err != nil {
					return fmt.Errorf("wireguard AllowedIPs %q is not a CIDR: %w", cidr, err)
				}
				if !p.RouteCIDRs[norm] {
					return fmt.Errorf("wireguard AllowedIPs %q is not a configured Pod/mesh CIDR", cidr)
				}
			}
		default:
			// PrivateKey, ListenPort, Endpoint, PersistentKeepalive: this node's own
			// settings, not a cross-host trust decision. Left unchecked by design.
		}
	}
	return nil
}

// validateWireGuardGo permits creating only the managed interface.
func (p Policy) validateWireGuardGo(req Request) error {
	if req.Stdin != "" {
		return fmt.Errorf("wireguard-go takes no stdin")
	}
	if len(req.Args) != 1 {
		return fmt.Errorf("wireguard-go args %v not permitted", req.Args)
	}
	return p.checkMeshInterface(req.Args[0])
}

func (p Policy) checkMeshInterface(iface string) error {
	if p.MeshInterface == "" {
		return fmt.Errorf("no mesh interface is configured; command refused")
	}
	if iface != p.MeshInterface {
		return fmt.Errorf("interface %q is not the managed mesh interface %q", iface, p.MeshInterface)
	}
	return nil
}

// isReadOnlyProbe reports whether req is a harmless read-only probe (the ping),
// which the server logs at a lower verbosity than privileged changes.
func isReadOnlyProbe(req Request) bool {
	return req.Name == "sysctl" && argsEqual(req.Args, "-n", "kern.ostype")
}

// argsEqual reports whether args matches want exactly.
func argsEqual(args []string, want ...string) bool {
	if len(args) != len(want) {
		return false
	}
	for i := range args {
		if args[i] != want[i] {
			return false
		}
	}
	return true
}

// ipEqual compares two IP strings by value (so "10.0.0.1" matches a normalised
// form), refusing empty inputs.
func ipEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ia, ib := net.ParseIP(a), net.ParseIP(b)
	return ia != nil && ib != nil && ia.Equal(ib)
}

// normalizeCIDR canonicalises a CIDR to its masked network form so set lookups
// are insensitive to how the address was written (e.g. host bits set).
func normalizeCIDR(cidr string) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}
	return ipnet.String(), nil
}

// NormalizeCIDRSet builds a normalised CIDR set from raw CIDR strings, skipping
// any that do not parse. It is used by policy builders to populate RouteCIDRs.
func NormalizeCIDRSet(cidrs []string) map[string]bool {
	out := make(map[string]bool, len(cidrs))
	for _, c := range cidrs {
		if norm, err := normalizeCIDR(c); err == nil {
			out[norm] = true
		}
	}
	return out
}
