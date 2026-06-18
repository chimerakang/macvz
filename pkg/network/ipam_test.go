package network

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
)

func mustIPAM(t *testing.T, cidr string) *PodIPAM {
	t.Helper()
	a, err := NewPodIPAM(cidr)
	if err != nil {
		t.Fatalf("NewPodIPAM(%q): %v", cidr, err)
	}
	return a
}

func TestNewPodIPAMRejectsBadCIDR(t *testing.T) {
	if _, err := NewPodIPAM("not-a-cidr"); err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
}

func TestAllocateSkipsNetworkGatewayAndBroadcast(t *testing.T) {
	a := mustIPAM(t, "10.244.1.0/24")
	ip, err := a.Allocate("default/p1")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	// .0 network and .1 gateway are reserved, so the first handout is .2.
	if ip != "10.244.1.2" {
		t.Errorf("first allocation = %q, want 10.244.1.2", ip)
	}
	if !a.usable(net.ParseIP(ip)) {
		t.Errorf("%s should be within the usable range", ip)
	}
	// Network, gateway, and broadcast must never be in range.
	for _, bad := range []string{"10.244.1.0", "10.244.1.1", "10.244.1.255"} {
		if a.usable(net.ParseIP(bad)) {
			t.Errorf("%s should not be usable", bad)
		}
	}
}

func TestAllocateIsIdempotentPerKey(t *testing.T) {
	a := mustIPAM(t, "10.244.1.0/24")
	first, err := a.Allocate("default/p1")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	again, err := a.Allocate("default/p1")
	if err != nil {
		t.Fatalf("Allocate (retry): %v", err)
	}
	if first != again {
		t.Errorf("idempotent Allocate returned %q then %q", first, again)
	}
	if got := len(a.Allocations()); got != 1 {
		t.Errorf("idempotent Allocate produced %d allocations, want 1", got)
	}
}

func TestAllocateHandsOutDistinctIPs(t *testing.T) {
	a := mustIPAM(t, "10.244.1.0/24")
	seen := map[string]string{}
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("default/p%d", i)
		ip, err := a.Allocate(key)
		if err != nil {
			t.Fatalf("Allocate %s: %v", key, err)
		}
		if other, dup := seen[ip]; dup {
			t.Fatalf("IP %s handed to both %s and %s", ip, other, key)
		}
		seen[ip] = key
	}
}

func TestReleaseReturnsIPToPool(t *testing.T) {
	// /29 yields exactly 5 usable hosts (offsets 2..6), so once the pool is full
	// the only address a new allocation can get is the one we just released.
	a := mustIPAM(t, "10.0.0.0/29")
	const usable = 5
	keys := make([]string, usable)
	for i := range keys {
		keys[i] = fmt.Sprintf("default/p%d", i)
		if _, err := a.Allocate(keys[i]); err != nil {
			t.Fatalf("Allocate %s: %v", keys[i], err)
		}
	}
	freed := a.IP(keys[2])
	a.Release(keys[2])
	if a.IP(keys[2]) != "" {
		t.Error("Release did not clear the key")
	}
	reused, err := a.Allocate("default/new")
	if err != nil {
		t.Fatalf("Allocate after release: %v", err)
	}
	if reused != freed {
		t.Errorf("freed IP %s not reused (got %s)", freed, reused)
	}
}

func TestReleaseUnknownKeyIsNoop(t *testing.T) {
	a := mustIPAM(t, "10.244.1.0/24")
	a.Release("default/ghost") // must not panic
}

func TestExhaustion(t *testing.T) {
	// /30 has one usable host after reserving network/gateway/broadcast.
	a := mustIPAM(t, "10.0.0.0/30")
	if _, err := a.Allocate("default/p1"); err != nil {
		t.Fatalf("first Allocate: %v", err)
	}
	_, err := a.Allocate("default/p2")
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("expected ErrExhausted, got %v", err)
	}
}

func TestReserveRecoversState(t *testing.T) {
	a := mustIPAM(t, "10.244.1.0/24")
	// Simulate restart recovery: a Pod already recorded at .5 in Kubernetes.
	if err := a.Reserve("default/p1", "10.244.1.5"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if a.IP("default/p1") != "10.244.1.5" {
		t.Errorf("Reserve did not bind key, got %q", a.IP("default/p1"))
	}
	// A fresh allocation must not collide with the reserved address.
	for i := 0; i < 100; i++ {
		ip, err := a.Allocate(fmt.Sprintf("default/x%d", i))
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		if ip == "10.244.1.5" {
			t.Fatalf("allocator reissued reserved IP 10.244.1.5")
		}
	}
}

func TestReserveIdempotentAndConflict(t *testing.T) {
	a := mustIPAM(t, "10.244.1.0/24")
	if err := a.Reserve("default/p1", "10.244.1.5"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// Same (key, ip) again is fine.
	if err := a.Reserve("default/p1", "10.244.1.5"); err != nil {
		t.Fatalf("idempotent Reserve: %v", err)
	}
	// Different key, same IP -> conflict.
	if err := a.Reserve("default/p2", "10.244.1.5"); err == nil {
		t.Error("expected conflict reserving a held IP for a different key")
	}
}

func TestReserveRejectsOutOfRange(t *testing.T) {
	a := mustIPAM(t, "10.244.1.0/24")
	for _, ip := range []string{"10.244.2.5", "10.244.1.1", "10.244.1.0"} {
		if err := a.Reserve("default/p1", ip); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("Reserve(%s): expected ErrOutOfRange, got %v", ip, err)
		}
	}
}

// TestCrossNodeCollisionAvoidance is the core P3 acceptance property: two nodes
// with disjoint Kubernetes-assigned PodCIDRs can never produce the same Pod IP.
func TestCrossNodeCollisionAvoidance(t *testing.T) {
	nodeA := mustIPAM(t, "10.244.1.0/24")
	nodeB := mustIPAM(t, "10.244.2.0/24")

	seen := map[string]string{}
	for i := 0; i < 100; i++ {
		for name, a := range map[string]*PodIPAM{"A": nodeA, "B": nodeB} {
			ip, err := a.Allocate(fmt.Sprintf("ns/p-%s-%d", name, i))
			if err != nil {
				t.Fatalf("node %s Allocate: %v", name, err)
			}
			if owner, dup := seen[ip]; dup {
				t.Fatalf("IP %s allocated on both node %s and node %s", ip, owner, name)
			}
			seen[ip] = name
		}
	}
}

func TestIPv6Allocation(t *testing.T) {
	a := mustIPAM(t, "fd00:10:244:1::/64")
	ip, err := a.Allocate("default/p1")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if net.ParseIP(ip) == nil || net.ParseIP(ip).To4() != nil {
		t.Errorf("expected an IPv6 address, got %q", ip)
	}
	if !a.usable(net.ParseIP(ip)) {
		t.Errorf("%s should be usable", ip)
	}
}

func TestConcurrentAllocateDistinct(t *testing.T) {
	a := mustIPAM(t, "10.244.1.0/24") // ~253 usable
	const n = 200
	var wg sync.WaitGroup
	ips := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ips[i], errs[i] = a.Allocate(fmt.Sprintf("default/p%d", i))
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("Allocate p%d: %v", i, errs[i])
		}
		if seen[ips[i]] {
			t.Fatalf("duplicate IP %s under concurrency", ips[i])
		}
		seen[ips[i]] = true
	}
}
