package podnet

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// fakeRunner records commands and can inject failures.
type fakeRunner struct {
	mu     sync.Mutex
	cmds   []command
	failOn map[string]string // substring of command string -> stderr to fail with
}

func newFakeRunner() *fakeRunner { return &fakeRunner{failOn: map[string]string{}} }

func (f *fakeRunner) run(_ context.Context, c command) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, c)
	for sub, stderr := range f.failOn {
		if strings.Contains(c.String(), sub) {
			return "", &CommandError{Cmd: c.String(), Stderr: stderr, ExitCode: 1}
		}
	}
	return "", nil
}

func (f *fakeRunner) strings() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.cmds))
	for i, c := range f.cmds {
		out[i] = c.String()
	}
	return out
}

// lastAnchorLoad returns the stdin of the most recent `pfctl -a ... -f -` call.
func (f *fakeRunner) lastAnchorLoad() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.cmds) - 1; i >= 0; i-- {
		if strings.Contains(f.cmds[i].String(), "-f -") {
			return f.cmds[i].Stdin, true
		}
	}
	return "", false
}

func contains(haystack []string, sub string) bool {
	for _, s := range haystack {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func newTestRouter(fr *fakeRunner) *Router {
	return New(Config{Interface: "bridge100", EnableForwarding: true}, WithRunner(fr))
}

func TestStartEnablesForwardingAndPF(t *testing.T) {
	fr := newFakeRunner()
	rt := newTestRouter(fr)
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cmds := fr.strings()
	if !contains(cmds, "sysctl -w net.inet.ip.forwarding=1") {
		t.Errorf("Start did not enable forwarding\nran: %v", cmds)
	}
	if !contains(cmds, "pfctl -e") {
		t.Errorf("Start did not enable pf\nran: %v", cmds)
	}
	if !contains(cmds, "pfctl -a macvz/pods -f -") {
		t.Errorf("Start did not load a baseline anchor\nran: %v", cmds)
	}
	if !contains(cmds, "route -q -n delete -inet default -interface bridge100") {
		t.Errorf("Start did not remove vmnet default route\nran: %v", cmds)
	}
}

func TestStartToleratesPFAlreadyEnabled(t *testing.T) {
	fr := newFakeRunner()
	fr.failOn["pfctl -e"] = "pfctl: pf already enabled"
	rt := newTestRouter(fr)
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start should tolerate 'pf already enabled', got: %v", err)
	}
}

func TestStartToleratesMissingVMNetDefaultRoute(t *testing.T) {
	fr := newFakeRunner()
	fr.failOn["route -q -n delete -inet default -interface bridge100"] = "route: writing to routing socket: not in table"
	rt := newTestRouter(fr)
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start should tolerate missing vmnet default route, got: %v", err)
	}
}

func TestStartFailsWhenVMNetDefaultRouteCleanupFails(t *testing.T) {
	fr := newFakeRunner()
	fr.failOn["route -q -n delete -inet default -interface bridge100"] = "route: permission denied"
	rt := newTestRouter(fr)
	if err := rt.Start(context.Background()); err == nil {
		t.Fatal("Start should fail when vmnet default route cleanup fails")
	}
}

func TestStartRequiresInterface(t *testing.T) {
	rt := New(Config{}, WithRunner(newFakeRunner()))
	if err := rt.Start(context.Background()); err == nil {
		t.Fatal("Start should require an interface")
	}
}

func TestAttachInstallsBinatRule(t *testing.T) {
	fr := newFakeRunner()
	rt := newTestRouter(fr)
	ctx := context.Background()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ep := Endpoint{PodKey: "default/web", PodIP: "10.244.1.2", VMIP: "192.168.64.5"}
	if err := rt.Attach(ctx, ep); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got := countContaining(fr.strings(), "route -q -n delete -inet default -interface bridge100"); got < 2 {
		t.Errorf("Attach should re-remove vmnet default route after VM start; got %d cleanup calls", got)
	}
	rules, ok := fr.lastAnchorLoad()
	if !ok {
		t.Fatal("no anchor load recorded")
	}
	want := "binat on bridge100 from 192.168.64.5 to any -> 10.244.1.2"
	if !strings.Contains(rules, want) {
		t.Errorf("anchor missing rule %q\n---\n%s", want, rules)
	}
	if eps := rt.Endpoints(); len(eps) != 1 || eps[0].PodKey != "default/web" {
		t.Errorf("Endpoints = %v, want [default/web]", eps)
	}
}

func countContaining(haystack []string, sub string) int {
	n := 0
	for _, s := range haystack {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func TestAttachValidatesEndpoint(t *testing.T) {
	fr := newFakeRunner()
	rt := newTestRouter(fr)
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	bad := []Endpoint{
		{PodKey: "", PodIP: "10.244.1.2", VMIP: "192.168.64.5"},
		{PodKey: "default/x", PodIP: "nope", VMIP: "192.168.64.5"},
		{PodKey: "default/x", PodIP: "10.244.1.2", VMIP: "nope"},
	}
	for _, ep := range bad {
		if err := rt.Attach(context.Background(), ep); err == nil {
			t.Errorf("Attach(%+v) = nil, want validation error", ep)
		}
	}
}

func TestAttachBeforeStartFails(t *testing.T) {
	rt := newTestRouter(newFakeRunner())
	ep := Endpoint{PodKey: "default/web", PodIP: "10.244.1.2", VMIP: "192.168.64.5"}
	if err := rt.Attach(context.Background(), ep); err == nil {
		t.Fatal("Attach before Start should fail")
	}
}

func TestDetachRemovesRule(t *testing.T) {
	fr := newFakeRunner()
	rt := newTestRouter(fr)
	ctx := context.Background()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := Endpoint{PodKey: "default/a", PodIP: "10.244.1.2", VMIP: "192.168.64.5"}
	b := Endpoint{PodKey: "default/b", PodIP: "10.244.1.3", VMIP: "192.168.64.6"}
	if err := rt.Attach(ctx, a); err != nil {
		t.Fatalf("Attach a: %v", err)
	}
	if err := rt.Attach(ctx, b); err != nil {
		t.Fatalf("Attach b: %v", err)
	}
	if err := rt.Detach(ctx, "default/a"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	rules, _ := fr.lastAnchorLoad()
	if strings.Contains(rules, "10.244.1.2") {
		t.Errorf("detached pod's rule still present\n---\n%s", rules)
	}
	if !strings.Contains(rules, "10.244.1.3") {
		t.Errorf("remaining pod's rule missing\n---\n%s", rules)
	}
	if eps := rt.Endpoints(); len(eps) != 1 || eps[0].PodKey != "default/b" {
		t.Errorf("Endpoints = %v, want [default/b]", eps)
	}
}

func TestDetachUnknownIsNoop(t *testing.T) {
	fr := newFakeRunner()
	rt := newTestRouter(fr)
	ctx := context.Background()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	before := len(fr.strings())
	if err := rt.Detach(ctx, "default/ghost"); err != nil {
		t.Fatalf("Detach unknown: %v", err)
	}
	if after := len(fr.strings()); after != before {
		t.Errorf("Detach of unknown pod reloaded the anchor (%d -> %d cmds)", before, after)
	}
}

func TestRenderAnchorDeterministic(t *testing.T) {
	eps := []Endpoint{
		{PodKey: "default/a", PodIP: "10.244.1.2", VMIP: "192.168.64.5"},
		{PodKey: "default/b", PodIP: "10.244.1.3", VMIP: "192.168.64.6"},
	}
	first := renderAnchor("bridge100", eps)
	second := renderAnchor("bridge100", eps)
	if first != second {
		t.Error("renderAnchor is not deterministic")
	}
}

func TestStopFlushesAnchor(t *testing.T) {
	fr := newFakeRunner()
	rt := newTestRouter(fr)
	ctx := context.Background()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := rt.Attach(ctx, Endpoint{PodKey: "default/a", PodIP: "10.244.1.2", VMIP: "192.168.64.5"}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := rt.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !contains(fr.strings(), "pfctl -a macvz/pods -F all") {
		t.Errorf("Stop did not flush the anchor\nran: %v", fr.strings())
	}
	if eps := rt.Endpoints(); len(eps) != 0 {
		t.Errorf("endpoints remain after Stop: %v", eps)
	}
}
