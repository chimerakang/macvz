package podnet

import (
	"context"
	"strings"
	"testing"
)

func startedRouter(t *testing.T, fr *fakeRunner) (*Router, context.Context) {
	t.Helper()
	rt := newTestRouter(fr)
	ctx := context.Background()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return rt, ctx
}

// A local backend (its Pod is attached on this node) redirects to the VM's
// host-only address, not the Pod IP, so it is delivered straight over vmnet.
func TestAttachServiceLocalBackendRedirectsToVMIP(t *testing.T) {
	fr := newFakeRunner()
	rt, ctx := startedRouter(t, fr)

	if err := rt.Attach(ctx, Endpoint{PodKey: "default/be", PodIP: "10.244.101.2", VMIP: "192.168.64.5"}); err != nil {
		t.Fatalf("Attach backend: %v", err)
	}
	rule := ServiceRule{
		ServiceKey: "default/hello", ClusterIP: "10.96.0.50", Protocol: "tcp",
		Port: 80, TargetPort: 8080, Backends: []string{"10.244.101.2"},
	}
	if err := rt.AttachService(ctx, "default/hello", []ServiceRule{rule}); err != nil {
		t.Fatalf("AttachService: %v", err)
	}
	rules, ok := fr.lastAnchorLoad()
	if !ok {
		t.Fatal("no anchor load recorded")
	}
	want := "rdr on bridge100 inet proto tcp from any to 10.96.0.50 port 80 -> 192.168.64.5 port 8080"
	if !strings.Contains(rules, want) {
		t.Errorf("anchor missing local rdr rule %q\n---\n%s", want, rules)
	}
	if strings.Contains(rules, "-> 10.244.101.2 port 8080") {
		t.Errorf("local backend should redirect to VMIP, not Pod IP\n---\n%s", rules)
	}
}

// A remote backend (no local endpoint) keeps its Pod IP, reached via the mesh.
func TestAttachServiceRemoteBackendKeepsPodIP(t *testing.T) {
	fr := newFakeRunner()
	rt, ctx := startedRouter(t, fr)

	rule := ServiceRule{
		ServiceKey: "default/hello", ClusterIP: "10.96.0.50", Protocol: "tcp",
		Port: 80, TargetPort: 8080, Backends: []string{"10.244.102.9"},
	}
	if err := rt.AttachService(ctx, "default/hello", []ServiceRule{rule}); err != nil {
		t.Fatalf("AttachService: %v", err)
	}
	rules, _ := fr.lastAnchorLoad()
	want := "rdr on bridge100 inet proto tcp from any to 10.96.0.50 port 80 -> 10.244.102.9 port 8080"
	if !strings.Contains(rules, want) {
		t.Errorf("anchor missing remote rdr rule %q\n---\n%s", want, rules)
	}
}

// Mixed local + remote backends become a round-robin pool with VMIP for the
// local one and Pod IP for the remote one.
func TestAttachServiceMixedBackendsRoundRobin(t *testing.T) {
	fr := newFakeRunner()
	rt, ctx := startedRouter(t, fr)
	if err := rt.Attach(ctx, Endpoint{PodKey: "default/be", PodIP: "10.244.101.2", VMIP: "192.168.64.5"}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	rule := ServiceRule{
		ServiceKey: "default/hello", ClusterIP: "10.96.0.50", Protocol: "tcp",
		Port: 80, TargetPort: 8080, Backends: []string{"10.244.101.2", "10.244.102.9"},
	}
	if err := rt.AttachService(ctx, "default/hello", []ServiceRule{rule}); err != nil {
		t.Fatalf("AttachService: %v", err)
	}
	rules, _ := fr.lastAnchorLoad()
	// targets are sorted; "10.244.102.9" < "192.168.64.5"
	want := "-> { 10.244.102.9, 192.168.64.5 } port 8080 round-robin"
	if !strings.Contains(rules, want) {
		t.Errorf("anchor missing round-robin pool %q\n---\n%s", want, rules)
	}
}

// A Service with no ready backends emits no rule, and replacing a Service's
// rules with an empty set removes it.
func TestAttachServiceNoBackendsRemovesRule(t *testing.T) {
	fr := newFakeRunner()
	rt, ctx := startedRouter(t, fr)
	rule := ServiceRule{
		ServiceKey: "default/hello", ClusterIP: "10.96.0.50", Protocol: "tcp",
		Port: 80, TargetPort: 8080, Backends: []string{"10.244.101.2"},
	}
	if err := rt.AttachService(ctx, "default/hello", []ServiceRule{rule}); err != nil {
		t.Fatalf("AttachService: %v", err)
	}
	// now drop all backends
	rule.Backends = nil
	if err := rt.AttachService(ctx, "default/hello", []ServiceRule{rule}); err != nil {
		t.Fatalf("AttachService empty: %v", err)
	}
	if svcs := rt.Services(); len(svcs) != 0 {
		t.Errorf("Services = %v, want empty after losing all backends", svcs)
	}
	rules, _ := fr.lastAnchorLoad()
	if strings.Contains(rules, "10.96.0.50") {
		t.Errorf("rule still present after losing all backends\n---\n%s", rules)
	}
}

func TestDetachServiceRemovesRule(t *testing.T) {
	fr := newFakeRunner()
	rt, ctx := startedRouter(t, fr)
	a := ServiceRule{ServiceKey: "default/a", ClusterIP: "10.96.0.1", Protocol: "tcp", Port: 80, TargetPort: 8080, Backends: []string{"10.244.102.2"}}
	b := ServiceRule{ServiceKey: "default/b", ClusterIP: "10.96.0.2", Protocol: "tcp", Port: 80, TargetPort: 8080, Backends: []string{"10.244.102.3"}}
	if err := rt.AttachService(ctx, "default/a", []ServiceRule{a}); err != nil {
		t.Fatalf("AttachService a: %v", err)
	}
	if err := rt.AttachService(ctx, "default/b", []ServiceRule{b}); err != nil {
		t.Fatalf("AttachService b: %v", err)
	}
	if err := rt.DetachService(ctx, "default/a"); err != nil {
		t.Fatalf("DetachService: %v", err)
	}
	rules, _ := fr.lastAnchorLoad()
	if strings.Contains(rules, "10.96.0.1") {
		t.Errorf("detached service rule still present\n---\n%s", rules)
	}
	if !strings.Contains(rules, "10.96.0.2") {
		t.Errorf("remaining service rule missing\n---\n%s", rules)
	}
}

func TestDetachServiceUnknownIsNoop(t *testing.T) {
	fr := newFakeRunner()
	rt, ctx := startedRouter(t, fr)
	before := len(fr.strings())
	if err := rt.DetachService(ctx, "default/ghost"); err != nil {
		t.Fatalf("DetachService unknown: %v", err)
	}
	if after := len(fr.strings()); after != before {
		t.Errorf("DetachService of unknown reloaded the anchor (%d -> %d)", before, after)
	}
}

func TestAttachServiceValidates(t *testing.T) {
	fr := newFakeRunner()
	rt, ctx := startedRouter(t, fr)
	bad := []ServiceRule{
		{ServiceKey: "", ClusterIP: "10.96.0.1", Protocol: "tcp", Port: 80, TargetPort: 8080, Backends: []string{"10.244.1.2"}},
		{ServiceKey: "default/x", ClusterIP: "nope", Protocol: "tcp", Port: 80, TargetPort: 8080, Backends: []string{"10.244.1.2"}},
		{ServiceKey: "default/x", ClusterIP: "10.96.0.1", Protocol: "sctp", Port: 80, TargetPort: 8080, Backends: []string{"10.244.1.2"}},
		{ServiceKey: "default/x", ClusterIP: "10.96.0.1", Protocol: "tcp", Port: 0, TargetPort: 8080, Backends: []string{"10.244.1.2"}},
		{ServiceKey: "default/x", ClusterIP: "10.96.0.1", Protocol: "tcp", Port: 80, TargetPort: 0, Backends: []string{"10.244.1.2"}},
		{ServiceKey: "default/x", ClusterIP: "10.96.0.1", Protocol: "tcp", Port: 80, TargetPort: 8080, Backends: []string{"bad-ip"}},
	}
	for _, r := range bad {
		if err := rt.AttachService(ctx, "default/x", []ServiceRule{r}); err == nil {
			t.Errorf("AttachService(%+v) = nil, want validation error", r)
		}
	}
}

func TestAttachServiceBeforeStartFails(t *testing.T) {
	rt := newTestRouter(newFakeRunner())
	r := ServiceRule{ServiceKey: "default/x", ClusterIP: "10.96.0.1", Protocol: "tcp", Port: 80, TargetPort: 8080, Backends: []string{"10.244.1.2"}}
	if err := rt.AttachService(context.Background(), "default/x", []ServiceRule{r}); err == nil {
		t.Fatal("AttachService before Start should fail")
	}
}

func TestRenderServiceRulesDeterministic(t *testing.T) {
	services := map[string][]ServiceRule{
		"default/a": {{ServiceKey: "default/a", ClusterIP: "10.96.0.1", Protocol: "tcp", Port: 80, TargetPort: 8080, Backends: []string{"10.244.1.2"}}},
		"default/b": {{ServiceKey: "default/b", ClusterIP: "10.96.0.2", Protocol: "tcp", Port: 80, TargetPort: 8080, Backends: []string{"10.244.1.3"}}},
	}
	vmip := map[string]string{}
	if renderServiceRules("bridge100", services, vmip) != renderServiceRules("bridge100", services, vmip) {
		t.Error("renderServiceRules is not deterministic")
	}
}

// Service rules and Pod binat rules coexist in the same anchor load.
func TestServiceAndBinatRulesCoexist(t *testing.T) {
	fr := newFakeRunner()
	rt, ctx := startedRouter(t, fr)
	if err := rt.Attach(ctx, Endpoint{PodKey: "default/be", PodIP: "10.244.101.2", VMIP: "192.168.64.5"}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	rule := ServiceRule{ServiceKey: "default/hello", ClusterIP: "10.96.0.50", Protocol: "tcp", Port: 80, TargetPort: 8080, Backends: []string{"10.244.101.2"}}
	if err := rt.AttachService(ctx, "default/hello", []ServiceRule{rule}); err != nil {
		t.Fatalf("AttachService: %v", err)
	}
	rules, _ := fr.lastAnchorLoad()
	if !strings.Contains(rules, "binat on bridge100 from 192.168.64.5 to any -> 10.244.101.2") {
		t.Errorf("binat rule missing\n---\n%s", rules)
	}
	if !strings.Contains(rules, "rdr on bridge100 inet proto tcp from any to 10.96.0.50") {
		t.Errorf("rdr rule missing\n---\n%s", rules)
	}
}
