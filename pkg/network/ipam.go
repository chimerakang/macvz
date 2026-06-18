package network

import (
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync"
)

// Pod IPAM — MVP allocation model
//
// Cross-host clusters must not let two Macs hand out the same Pod IP. Rather than
// inventing a new coordination layer, MacVz leans on state Kubernetes already
// owns: when kube-controller-manager runs with --allocate-node-cidrs, every Node
// is assigned a disjoint Spec.PodCIDR. Each macvz-kubelet allocates Pod IPs only
// from its own node's PodCIDR, so two nodes can never produce the same address
// for different Pods — collision avoidance is a property of the (Kubernetes-owned)
// CIDR partitioning, not of any peer-to-peer agreement.
//
// Within a single node, PodIPAM hands out host addresses from the CIDR in order,
// skipping the network address (and, for IPv4, the broadcast address). The first
// usable host address is treated as the gateway and reserved, mirroring typical
// CNI bridge conventions; the cross-host data path that uses it is wired in #22.
//
// Allocation is keyed by "namespace/name" and is idempotent: allocating the same
// key twice returns the same IP, so Virtual Kubelet's at-least-once CreatePod is
// safe. Released addresses are returned to the pool and reused. Durability lives
// in Kubernetes: a restarted provider rebuilds its in-memory allocations by
// reserving the PodIPs already recorded on the API server (see Reserve), so a
// restart neither leaks addresses nor reassigns a live Pod's IP.
//
// PodIPAM is safe for concurrent use.

// ErrExhausted is returned when the CIDR has no free host address left.
var ErrExhausted = errors.New("network: pod CIDR exhausted")

// ErrOutOfRange is returned by Reserve when the IP is not a usable host address
// within the managed CIDR.
var ErrOutOfRange = errors.New("network: address outside managed CIDR")

// PodIPAM allocates Pod IPs from a single node's Kubernetes-assigned Pod CIDR.
type PodIPAM struct {
	ipnet *net.IPNet
	bytes int // address width in bytes (4 for IPv4, 16 for IPv6)

	base  *big.Int // network address as an integer
	first *big.Int // first usable host offset (inclusive), relative to base
	last  *big.Int // last usable host offset (inclusive), relative to base

	mu     sync.Mutex
	byKey  map[string]string // key -> ip
	byIP   map[string]string // ip -> key
	cursor *big.Int          // next offset to try (relative to base)
}

// NewPodIPAM builds an allocator over the given CIDR (e.g. a node's
// Spec.PodCIDR, "10.244.1.0/24"). It reserves the network address, the IPv4
// broadcast address, and the first usable host address (the gateway).
func NewPodIPAM(cidr string) (*PodIPAM, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("network: parse pod CIDR %q: %w", cidr, err)
	}
	// Normalize to the canonical address width: IPv4 CIDRs parse to a 4-byte
	// mask, IPv6 to 16. Use the mask length as the source of truth.
	width := len(ipnet.Mask)
	isIPv4 := ip.To4() != nil && width == net.IPv4len

	ones, bits := ipnet.Mask.Size()
	hostBits := bits - ones
	size := new(big.Int).Lsh(big.NewInt(1), uint(hostBits)) // 2^hostBits

	base := ipToBig(ipnet.IP)

	// Usable host range: skip the network address (offset 0). Reserve the first
	// host (offset 1) as the gateway. For IPv4 also skip the broadcast (last).
	first := big.NewInt(2)
	last := new(big.Int).Sub(size, big.NewInt(1)) // size-1 (last offset)
	if isIPv4 {
		last.Sub(last, big.NewInt(1)) // skip broadcast -> size-2
	}

	a := &PodIPAM{
		ipnet:  ipnet,
		bytes:  width,
		base:   base,
		first:  first,
		last:   last,
		byKey:  map[string]string{},
		byIP:   map[string]string{},
		cursor: new(big.Int).Set(first),
	}
	return a, nil
}

// CIDR returns the managed CIDR in canonical form.
func (a *PodIPAM) CIDR() string { return a.ipnet.String() }

// Allocate returns the Pod IP for key, assigning a fresh one if needed. It is
// idempotent: repeated calls for the same key return the same IP. ErrExhausted
// is returned when no host address is free.
func (a *PodIPAM) Allocate(key string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if ip, ok := a.byKey[key]; ok {
		return ip, nil
	}
	if a.first.Cmp(a.last) > 0 {
		return "", ErrExhausted
	}

	span := new(big.Int).Sub(a.last, a.first)
	span.Add(span, big.NewInt(1)) // number of usable offsets
	off := new(big.Int).Set(a.cursor)
	for i := new(big.Int).SetInt64(0); i.Cmp(span) < 0; i.Add(i, big.NewInt(1)) {
		if off.Cmp(a.last) > 0 {
			off.Set(a.first) // wrap
		}
		ip := a.offsetIP(off)
		if _, taken := a.byIP[ip]; !taken {
			a.byKey[key] = ip
			a.byIP[ip] = key
			a.cursor.Add(off, big.NewInt(1)) // advance past the one we took
			return ip, nil
		}
		off.Add(off, big.NewInt(1))
	}
	return "", ErrExhausted
}

// Reserve binds key to a specific IP, used to rebuild allocator state from
// Kubernetes on restart. It is idempotent when (key, ip) already match. It
// returns ErrOutOfRange if ip is not a usable host address in the CIDR, or an
// error if the IP is already held by a different key.
func (a *PodIPAM) Reserve(key, ip string) error {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return fmt.Errorf("network: reserve %q: invalid IP %q", key, ip)
	}
	canon := canonIP(parsed, a.bytes)
	if canon == "" || !a.usable(parsed) {
		return fmt.Errorf("network: reserve %q at %s: %w", key, ip, ErrOutOfRange)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if owner, ok := a.byIP[canon]; ok {
		if owner == key {
			return nil
		}
		return fmt.Errorf("network: reserve %s for %q: already held by %q", canon, key, owner)
	}
	if existing, ok := a.byKey[key]; ok && existing != canon {
		return fmt.Errorf("network: reserve %q at %s: key already holds %s", key, canon, existing)
	}
	a.byKey[key] = canon
	a.byIP[canon] = key
	return nil
}

// Release frees the IP held by key, returning it to the pool. Releasing an
// unknown key is a no-op, so deletion is idempotent.
func (a *PodIPAM) Release(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ip, ok := a.byKey[key]
	if !ok {
		return
	}
	delete(a.byKey, key)
	delete(a.byIP, ip)
}

// IP returns the IP currently held by key, or "" if none.
func (a *PodIPAM) IP(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.byKey[key]
}

// Allocations returns a snapshot copy of the current key->IP assignments.
func (a *PodIPAM) Allocations() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]string, len(a.byKey))
	for k, v := range a.byKey {
		out[k] = v
	}
	return out
}

// offsetIP renders the host address at base+off as a canonical string.
func (a *PodIPAM) offsetIP(off *big.Int) string {
	addr := new(big.Int).Add(a.base, off)
	return bigToIP(addr, a.bytes).String()
}

// usable reports whether ip falls within the managed CIDR's usable host range.
func (a *PodIPAM) usable(ip net.IP) bool {
	if !a.ipnet.Contains(ip) {
		return false
	}
	off := new(big.Int).Sub(ipToBig(ip), a.base)
	return off.Cmp(a.first) >= 0 && off.Cmp(a.last) <= 0
}

// canonIP renders ip at the allocator's width, or "" if it does not fit.
func canonIP(ip net.IP, width int) string {
	switch width {
	case net.IPv4len:
		v4 := ip.To4()
		if v4 == nil {
			return ""
		}
		return v4.String()
	default:
		return ip.String()
	}
}

// ipToBig converts an IP to a big.Int using its canonical byte width.
func ipToBig(ip net.IP) *big.Int {
	if v4 := ip.To4(); v4 != nil {
		return new(big.Int).SetBytes(v4)
	}
	return new(big.Int).SetBytes(ip.To16())
}

// bigToIP converts an integer back to an IP of the given byte width.
func bigToIP(n *big.Int, width int) net.IP {
	b := n.Bytes()
	if len(b) < width {
		pad := make([]byte, width-len(b))
		b = append(pad, b...)
	}
	return net.IP(b[len(b)-width:])
}
