package svcroute

import (
	"context"
	"testing"

	"github.com/chimerakang/macvz/pkg/network/podnet"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ptrBool(b bool) *bool    { return &b }
func ptrI32(i int32) *int32   { return &i }
func ptrStr(s string) *string { return &s }

func clusterIPService(ns, name, clusterIP string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: clusterIP, Ports: ports},
	}
}

func slice(ns, svcName string, ready bool, addr string, ports ...discoveryv1.EndpointPort) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: ns, Name: svcName + "-x", Labels: map[string]string{discoveryv1.LabelServiceName: svcName}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{addr}, Conditions: discoveryv1.EndpointConditions{Ready: ptrBool(ready)}}},
		Ports:       ports,
	}
}

func TestBuildServiceRulesBasic(t *testing.T) {
	svc := clusterIPService("default", "hello", "10.96.0.50",
		corev1.ServicePort{Name: "", Protocol: corev1.ProtocolTCP, Port: 80})
	sl := slice("default", "hello", true, "10.244.101.2",
		discoveryv1.EndpointPort{Name: ptrStr(""), Port: ptrI32(8080)})

	rules := BuildServiceRules(svc, []*discoveryv1.EndpointSlice{sl})
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d (%+v)", len(rules), rules)
	}
	r := rules[0]
	if r.ClusterIP != "10.96.0.50" || r.Port != 80 || r.TargetPort != 8080 || r.Protocol != "tcp" {
		t.Errorf("unexpected rule: %+v", r)
	}
	if len(r.Backends) != 1 || r.Backends[0] != "10.244.101.2" {
		t.Errorf("unexpected backends: %v", r.Backends)
	}
}

func TestBuildServiceRulesNamedPortMatch(t *testing.T) {
	svc := clusterIPService("default", "multi", "10.96.0.7",
		corev1.ServicePort{Name: "http", Protocol: corev1.ProtocolTCP, Port: 80},
		corev1.ServicePort{Name: "metrics", Protocol: corev1.ProtocolTCP, Port: 9090})
	sl := slice("default", "multi", true, "10.244.101.3",
		discoveryv1.EndpointPort{Name: ptrStr("http"), Port: ptrI32(8080)},
		discoveryv1.EndpointPort{Name: ptrStr("metrics"), Port: ptrI32(9100)})

	rules := BuildServiceRules(svc, []*discoveryv1.EndpointSlice{sl})
	if len(rules) != 2 {
		t.Fatalf("want 2 rules, got %d", len(rules))
	}
	got := map[int]int{}
	for _, r := range rules {
		got[r.Port] = r.TargetPort
	}
	if got[80] != 8080 || got[9090] != 9100 {
		t.Errorf("named ports mismatched: %v", got)
	}
}

func TestBuildServiceRulesSkipsHeadlessAndExternalName(t *testing.T) {
	sl := slice("default", "h", true, "10.244.1.2", discoveryv1.EndpointPort{Name: ptrStr(""), Port: ptrI32(8080)})
	headless := clusterIPService("default", "h", "None", corev1.ServicePort{Protocol: corev1.ProtocolTCP, Port: 80})
	if r := BuildServiceRules(headless, []*discoveryv1.EndpointSlice{sl}); r != nil {
		t.Errorf("headless service should yield no rules, got %+v", r)
	}
	ext := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "e"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "x.com"}}
	if r := BuildServiceRules(ext, nil); r != nil {
		t.Errorf("ExternalName service should yield no rules, got %+v", r)
	}
	empty := clusterIPService("default", "p", "", corev1.ServicePort{Protocol: corev1.ProtocolTCP, Port: 80})
	if r := BuildServiceRules(empty, []*discoveryv1.EndpointSlice{sl}); r != nil {
		t.Errorf("unallocated ClusterIP should yield no rules, got %+v", r)
	}
}

func TestBuildServiceRulesNoReadyBackends(t *testing.T) {
	svc := clusterIPService("default", "hello", "10.96.0.50", corev1.ServicePort{Protocol: corev1.ProtocolTCP, Port: 80})
	sl := slice("default", "hello", false, "10.244.101.2", discoveryv1.EndpointPort{Name: ptrStr(""), Port: ptrI32(8080)})
	if r := BuildServiceRules(svc, []*discoveryv1.EndpointSlice{sl}); r != nil {
		t.Errorf("no ready backends should yield no rules, got %+v", r)
	}
}

func TestBuildServiceRulesSkipsTerminating(t *testing.T) {
	svc := clusterIPService("default", "hello", "10.96.0.50", corev1.ServicePort{Protocol: corev1.ProtocolTCP, Port: 80})
	sl := slice("default", "hello", true, "10.244.101.2", discoveryv1.EndpointPort{Name: ptrStr(""), Port: ptrI32(8080)})
	sl.Endpoints[0].Conditions.Terminating = ptrBool(true)
	if r := BuildServiceRules(svc, []*discoveryv1.EndpointSlice{sl}); r != nil {
		t.Errorf("terminating endpoint should be excluded, got %+v", r)
	}
}

func TestBuildServiceRulesSkipsSCTP(t *testing.T) {
	svc := clusterIPService("default", "s", "10.96.0.9", corev1.ServicePort{Protocol: corev1.ProtocolSCTP, Port: 80})
	sl := slice("default", "s", true, "10.244.1.2", discoveryv1.EndpointPort{Name: ptrStr(""), Port: ptrI32(8080)})
	if r := BuildServiceRules(svc, []*discoveryv1.EndpointSlice{sl}); r != nil {
		t.Errorf("SCTP should be skipped, got %+v", r)
	}
}

func TestBuildServiceRulesDedupesAcrossSlices(t *testing.T) {
	svc := clusterIPService("default", "hello", "10.96.0.50", corev1.ServicePort{Protocol: corev1.ProtocolTCP, Port: 80})
	p := discoveryv1.EndpointPort{Name: ptrStr(""), Port: ptrI32(8080)}
	s1 := slice("default", "hello", true, "10.244.101.2", p)
	s2 := slice("default", "hello", true, "10.244.101.2", p) // duplicate addr
	s3 := slice("default", "hello", true, "10.244.102.9", p)
	rules := BuildServiceRules(svc, []*discoveryv1.EndpointSlice{s1, s2, s3})
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	if len(rules[0].Backends) != 2 {
		t.Errorf("want 2 deduped backends, got %v", rules[0].Backends)
	}
}

// --- reconcile with fakes ----------------------------------------------------

type fakeRouter struct {
	attached map[string][]podnet.ServiceRule
	detached map[string]int
}

func newFakeRouter() *fakeRouter {
	return &fakeRouter{attached: map[string][]podnet.ServiceRule{}, detached: map[string]int{}}
}
func (f *fakeRouter) AttachService(_ context.Context, key string, rules []podnet.ServiceRule) error {
	f.attached[key] = rules
	return nil
}
func (f *fakeRouter) DetachService(_ context.Context, key string) error {
	f.detached[key]++
	delete(f.attached, key)
	return nil
}

type fakeServices map[string]*corev1.Service

func (f fakeServices) get(key string) (*corev1.Service, bool) { s, ok := f[key]; return s, ok }

type fakeSlices map[string][]*discoveryv1.EndpointSlice

func (f fakeSlices) listForService(ns, name string) ([]*discoveryv1.EndpointSlice, error) {
	return f[serviceKey(ns, name)], nil
}

func TestReconcileAttachesAndDetaches(t *testing.T) {
	ctx := context.Background()
	router := newFakeRouter()
	svcs := fakeServices{
		"default/hello": clusterIPService("default", "hello", "10.96.0.50", corev1.ServicePort{Protocol: corev1.ProtocolTCP, Port: 80}),
	}
	slices := fakeSlices{
		"default/hello": {slice("default", "hello", true, "10.244.101.2", discoveryv1.EndpointPort{Name: ptrStr(""), Port: ptrI32(8080)})},
	}
	c := newController(router, svcs, slices)

	if err := c.reconcile(ctx, "default/hello"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := router.attached["default/hello"]; !ok {
		t.Fatalf("expected service attached, attached=%v", router.attached)
	}

	// Service deleted -> detach.
	delete(svcs, "default/hello")
	if err := c.reconcile(ctx, "default/hello"); err != nil {
		t.Fatalf("reconcile after delete: %v", err)
	}
	if router.detached["default/hello"] == 0 {
		t.Errorf("expected detach after service deletion")
	}
}

func TestReconcileFiltersUnroutableBackends(t *testing.T) {
	ctx := context.Background()
	router := newFakeRouter()
	svcs := fakeServices{
		// default/kubernetes-style Service: its backend is the host-network
		// apiserver, outside any MacVz Pod/vmnet CIDR.
		"default/kubernetes": clusterIPService("default", "kubernetes", "10.96.0.1", corev1.ServicePort{Protocol: corev1.ProtocolTCP, Port: 443}),
		// A normal Service backed by a local MacVz Pod.
		"default/hello": clusterIPService("default", "hello", "10.96.0.50", corev1.ServicePort{Protocol: corev1.ProtocolTCP, Port: 80}),
	}
	slices := fakeSlices{
		"default/kubernetes": {slice("default", "kubernetes", true, "172.21.0.2", discoveryv1.EndpointPort{Name: ptrStr(""), Port: ptrI32(6443)})},
		"default/hello":      {slice("default", "hello", true, "10.244.101.2", discoveryv1.EndpointPort{Name: ptrStr(""), Port: ptrI32(8080)})},
	}
	c := newController(router, svcs, slices)
	WithRoutableCIDRs([]string{"10.244.101.0/24"})(c)

	// The unroutable Service must be detached, never attached: its sole backend is
	// dropped, leaving no rule, so it cannot poison the shared anchor.
	if err := c.reconcile(ctx, "default/kubernetes"); err != nil {
		t.Fatalf("reconcile kubernetes: %v", err)
	}
	if _, ok := router.attached["default/kubernetes"]; ok {
		t.Errorf("unroutable Service must not be attached, attached=%v", router.attached)
	}
	if router.detached["default/kubernetes"] == 0 {
		t.Errorf("unroutable Service should be detached")
	}

	// The routable Service is attached, with its local backend preserved.
	if err := c.reconcile(ctx, "default/hello"); err != nil {
		t.Fatalf("reconcile hello: %v", err)
	}
	rules, ok := router.attached["default/hello"]
	if !ok {
		t.Fatalf("routable Service should be attached, attached=%v", router.attached)
	}
	if len(rules) != 1 || len(rules[0].Backends) != 1 || rules[0].Backends[0] != "10.244.101.2" {
		t.Errorf("expected the local backend kept, got %+v", rules)
	}
}

func TestReconcileDetachesWhenBackendsGone(t *testing.T) {
	ctx := context.Background()
	router := newFakeRouter()
	svcs := fakeServices{
		"default/hello": clusterIPService("default", "hello", "10.96.0.50", corev1.ServicePort{Protocol: corev1.ProtocolTCP, Port: 80}),
	}
	// Service exists but its only endpoint is not ready.
	slices := fakeSlices{
		"default/hello": {slice("default", "hello", false, "10.244.101.2", discoveryv1.EndpointPort{Name: ptrStr(""), Port: ptrI32(8080)})},
	}
	c := newController(router, svcs, slices)
	if err := c.reconcile(ctx, "default/hello"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if router.detached["default/hello"] == 0 {
		t.Errorf("a service with no ready backends should be detached")
	}
}
