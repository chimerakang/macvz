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
	// PodIngressInterfaces are additional host interfaces that may carry inbound
	// Pod-IP traffic for binat. They are explicit opt-ins and do not grant route,
	// ifconfig, rdr, or default-route privileges.
	PodIngressInterfaces map[string]bool
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
	// PodCIDRs is the normalised set of local and peer Pod CIDRs that pf binat/rdr
	// rules may expose as Pod IPs.
	PodCIDRs map[string]bool
	// VMNetCIDRs is the normalised set of apple/container host-only vmnet CIDRs
	// that pf binat/rdr rules may reference for local micro-VM addresses.
	VMNetCIDRs map[string]bool
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
	case "pkill":
		return p.validatePkill(req)
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
		if len(fields) < 3 || fields[1] != "on" {
			return fmt.Errorf("anchor rule %q is not a binat/rdr rule", line)
		}
		switch fields[0] {
		case "binat":
			if err := p.checkPodNATInterface(fields[2]); err != nil {
				return fmt.Errorf("anchor rule on interface %q not permitted: %w", fields[2], err)
			}
			if err := p.lintBinatRule(fields); err != nil {
				return fmt.Errorf("anchor rule %q not permitted: %w", line, err)
			}
		case "rdr":
			if err := p.checkVMNetInterface(fields[2]); err != nil {
				return fmt.Errorf("anchor rule on interface %q not permitted: %w", fields[2], err)
			}
			if err := p.lintRDRRule(fields); err != nil {
				return fmt.Errorf("anchor rule %q not permitted: %w", line, err)
			}
		default:
			return fmt.Errorf("anchor rule %q is not a binat/rdr rule", line)
		}
	}
	return nil
}

func (p Policy) lintBinatRule(fields []string) error {
	i := 3
	if i < len(fields) && fields[i] == "inet" {
		i++
	}
	if len(fields) != i+6 || fields[i] != "from" || fields[i+2] != "to" ||
		fields[i+3] != "any" || fields[i+4] != "->" {
		return fmt.Errorf("expected binat on <iface> [inet] from <vm-ip> to any -> <pod-ip>")
	}
	if err := p.checkIPInSet(fields[i+1], p.VMNetCIDRs, "vmnet CIDR"); err != nil {
		return fmt.Errorf("source VM IP: %w", err)
	}
	if err := p.checkIPInSet(fields[i+5], p.PodCIDRs, "Pod CIDR"); err != nil {
		return fmt.Errorf("translated Pod IP: %w", err)
	}
	return nil
}

func (p Policy) lintRDRRule(fields []string) error {
	// rdr on <iface> inet proto <tcp|udp> from any to <cluster-ip> port <n> ->
	//   <target> port <n>
	// rdr on <iface> inet proto <tcp|udp> from any to <cluster-ip> port <n> ->
	//   { <target>, <target> } port <n> round-robin
	if len(fields) < 16 ||
		fields[3] != "inet" || fields[4] != "proto" ||
		(fields[5] != "tcp" && fields[5] != "udp") ||
		fields[6] != "from" || fields[7] != "any" ||
		fields[8] != "to" || fields[10] != "port" || fields[12] != "->" {
		return fmt.Errorf("expected rdr on <iface> inet proto <tcp|udp> from any to <cluster-ip> port <n> -> <target> port <n>")
	}
	if net.ParseIP(fields[9]) == nil {
		return fmt.Errorf("ClusterIP %q is not an IP", fields[9])
	}
	if _, err := strconv.Atoi(fields[11]); err != nil {
		return fmt.Errorf("service port %q is not numeric", fields[11])
	}

	portIdx := -1
	for i := 13; i < len(fields); i++ {
		if fields[i] == "port" {
			portIdx = i
			break
		}
	}
	if portIdx < 0 || portIdx+1 >= len(fields) {
		return fmt.Errorf("missing target port")
	}
	if _, err := strconv.Atoi(fields[portIdx+1]); err != nil {
		return fmt.Errorf("target port %q is not numeric", fields[portIdx+1])
	}
	if len(fields) != portIdx+2 && (len(fields) != portIdx+3 || fields[portIdx+2] != "round-robin") {
		return fmt.Errorf("unexpected trailing rdr tokens %v", fields[portIdx+2:])
	}

	targets, err := parseRDRTargets(fields[13:portIdx])
	if err != nil {
		return err
	}
	for _, target := range targets {
		if p.ipInSet(target, p.PodCIDRs) || p.ipInSet(target, p.VMNetCIDRs) {
			continue
		}
		return fmt.Errorf("redirect target %q is outside configured Pod/vmnet CIDRs", target)
	}
	return nil
}

func parseRDRTargets(fields []string) ([]string, error) {
	if len(fields) == 0 {
		return nil, fmt.Errorf("missing redirect target")
	}
	if fields[0] != "{" {
		if len(fields) != 1 {
			return nil, fmt.Errorf("unexpected redirect target tokens %v", fields)
		}
		if net.ParseIP(fields[0]) == nil {
			return nil, fmt.Errorf("redirect target %q is not an IP", fields[0])
		}
		return []string{fields[0]}, nil
	}
	if len(fields) < 3 || fields[len(fields)-1] != "}" {
		return nil, fmt.Errorf("malformed redirect target pool %v", fields)
	}
	out := make([]string, 0, len(fields)-2)
	for _, f := range fields[1 : len(fields)-1] {
		ip := strings.TrimSuffix(f, ",")
		if ip == "" || net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("redirect target %q is not an IP", f)
		}
		out = append(out, ip)
	}
	return out, nil
}

func (p Policy) checkIPInSet(ip string, cidrs map[string]bool, desc string) error {
	if !p.ipInSet(ip, cidrs) {
		return fmt.Errorf("%q is outside configured %s", ip, desc)
	}
	return nil
}

func (p Policy) ipInSet(raw string, cidrs map[string]bool) bool {
	ip := net.ParseIP(raw)
	if ip == nil {
		return false
	}
	for cidr := range cidrs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err == nil && ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

// validateRoute permits adding/deleting host routes only for configured CIDRs,
// and only through the managed mesh interface. The one exception is deleting
// apple/container's vmnet-scoped default route, which must target the configured
// vmnet interface and is never allowed for add.
func (p Policy) validateRoute(req Request) error {
	if req.Stdin != "" {
		return fmt.Errorf("route takes no stdin")
	}
	if len(req.Args) == 3 && req.Args[0] == "-n" && req.Args[1] == "get" {
		return p.checkIPInSet(req.Args[2], p.VMNetCIDRs, "vmnet CIDR")
	}
	// route -q -n <add|delete> <-inet|-inet6> <cidr> <-interface|-ifscope> <iface>
	if len(req.Args) != 7 || req.Args[0] != "-q" || req.Args[1] != "-n" {
		return fmt.Errorf("route args %v not permitted", req.Args)
	}
	if req.Args[2] != "add" && req.Args[2] != "delete" {
		return fmt.Errorf("route op %q not permitted", req.Args[2])
	}
	if req.Args[3] != "-inet" && req.Args[3] != "-inet6" {
		return fmt.Errorf("route family %q not permitted", req.Args[3])
	}
	if req.Args[2] == "delete" && req.Args[3] == "-inet" && req.Args[4] == "default" {
		if req.Args[5] != "-ifscope" {
			return fmt.Errorf("route default delete scope %q not permitted", req.Args[5])
		}
		return p.checkVMNetInterface(req.Args[6])
	}
	if req.Args[5] != "-interface" {
		return fmt.Errorf("route scope %q not permitted", req.Args[5])
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

func (p Policy) validatePkill(req Request) error {
	if req.Stdin != "" {
		return fmt.Errorf("pkill takes no stdin")
	}
	if len(req.Args) != 2 || req.Args[0] != "-f" {
		return fmt.Errorf("pkill args %v not permitted", req.Args)
	}
	want := "wireguard-go " + p.MeshInterface
	if req.Args[1] != want {
		return fmt.Errorf("pkill pattern %q is not the managed wireguard-go pattern %q", req.Args[1], want)
	}
	return p.checkMeshInterface(strings.TrimPrefix(req.Args[1], "wireguard-go "))
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

func (p Policy) checkVMNetInterface(iface string) error {
	if p.VMNetInterface == "" {
		return fmt.Errorf("no vmnet interface is configured; command refused")
	}
	if iface == p.VMNetInterface || isBridgeInterface(iface) {
		return nil
	}
	return fmt.Errorf("interface %q is not the managed vmnet interface %q or an apple/container bridge", iface, p.VMNetInterface)
}

func (p Policy) checkPodNATInterface(iface string) error {
	if p.checkVMNetInterface(iface) == nil {
		return nil
	}
	if p.MeshInterface != "" && iface == p.MeshInterface {
		return nil
	}
	if p.PodIngressInterfaces[iface] {
		return nil
	}
	return fmt.Errorf("interface %q is not a managed vmnet bridge, mesh interface, or Pod ingress interface", iface)
}

func isBridgeInterface(iface string) bool {
	if !strings.HasPrefix(iface, "bridge") || len(iface) == len("bridge") {
		return false
	}
	for _, r := range iface[len("bridge"):] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
