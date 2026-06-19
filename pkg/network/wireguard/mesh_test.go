package wireguard

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// fakeRunner records every command and can inject failures.
type fakeRunner struct {
	mu   sync.Mutex
	cmds []command
	// failOn maps a substring of command.String() to the stderr it should fail
	// with, so tests can exercise tolerated and fatal error paths.
	failOn map[string]string
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

func (f *fakeRunner) find(sub string) *command {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.cmds {
		if strings.Contains(f.cmds[i].String(), sub) {
			return &f.cmds[i]
		}
	}
	return nil
}

func contains(haystack []string, sub string) bool {
	for _, s := range haystack {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestUpBringsInterfaceUpConfiguresAndRoutes(t *testing.T) {
	fr := newFakeRunner()
	cfg := testConfig(t)
	cfg.MTU = 1380
	m := New(cfg, WithRunner(fr))
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	cmds := fr.strings()

	for _, want := range []string{
		"wireguard-go utun7",
		"wg setconf utun7 /dev/stdin",
		"ifconfig utun7 inet 10.99.0.1 10.99.0.1 alias",
		"ifconfig utun7 mtu 1380",
		"ifconfig utun7 up",
		"route -q -n add -inet 10.244.2.0/24 -interface utun7",
		"route -q -n add -inet 10.99.0.2/32 -interface utun7",
	} {
		if !contains(cmds, want) {
			t.Errorf("Up did not run %q\nran: %v", want, cmds)
		}
	}

	// The applied config must be passed via stdin, not a temp file.
	setconf := fr.find("wg setconf")
	if setconf == nil || !strings.Contains(setconf.Stdin, "PublicKey =") {
		t.Errorf("wg setconf did not receive config on stdin")
	}

	routes := m.InstalledRoutes()
	if len(routes) != 2 {
		t.Errorf("InstalledRoutes = %v, want 2", routes)
	}
}

func TestRouteCmdUsesIPv6Family(t *testing.T) {
	m := New(testConfig(t), WithRunner(newFakeRunner()))
	cmd := m.routeCmd("add", "fd00:10:244:2::/64")
	if got := cmd.String(); !strings.Contains(got, "add -inet6 fd00:10:244:2::/64") {
		t.Errorf("IPv6 route command = %q, want -inet6", got)
	}
}

func TestUpToleratesExistingInterfaceAndRoutes(t *testing.T) {
	fr := newFakeRunner()
	fr.failOn["wireguard-go utun7"] = "interface already exists"
	fr.failOn["route -q -n add -inet 10.244.2.0/24"] = "add net 10.244.2.0: gateway utun7: File exists"
	m := New(testConfig(t), WithRunner(fr))
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up should tolerate benign 'already exists'/'File exists', got: %v", err)
	}
	if got := len(m.InstalledRoutes()); got != 2 {
		t.Errorf("InstalledRoutes = %d, want 2", got)
	}
}

func TestUpFailsOnFatalError(t *testing.T) {
	fr := newFakeRunner()
	fr.failOn["wg setconf"] = "permission denied"
	m := New(testConfig(t), WithRunner(fr))
	if err := m.Up(context.Background()); err == nil {
		t.Fatal("Up should fail when wg setconf fails fatally")
	}
}

func TestUpValidatesConfig(t *testing.T) {
	bad := testConfig(t)
	bad.Address = "" // invalid
	m := New(bad, WithRunner(newFakeRunner()))
	if err := m.Up(context.Background()); err == nil {
		t.Fatal("Up should reject an invalid config before running commands")
	}
}

func TestSyncAddsAndRemovesPeersAndRoutes(t *testing.T) {
	fr := newFakeRunner()
	m := New(testConfig(t), WithRunner(fr))
	ctx := context.Background()
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// Replace the single peer (mac-02, CIDR 10.244.2.0/24) with a new peer
	// (mac-09, CIDR 10.244.9.0/24). The old route must go, the new one appear.
	newPeer := Peer{
		Name:       "mac-09",
		PublicKey:  testKey(t),
		Endpoint:   "192.168.1.90:51820",
		AllowedIPs: []string{"10.244.9.0/24", "10.99.0.9/32"},
	}
	if err := m.Sync(ctx, []Peer{newPeer}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	cmds := fr.strings()
	if !contains(cmds, "wg syncconf utun7 /dev/stdin") {
		t.Errorf("Sync did not run wg syncconf\nran: %v", cmds)
	}
	if !contains(cmds, "route -q -n delete -inet 10.244.2.0/24 -interface utun7") {
		t.Errorf("Sync did not remove the departed peer's route\nran: %v", cmds)
	}
	if !contains(cmds, "route -q -n add -inet 10.244.9.0/24 -interface utun7") {
		t.Errorf("Sync did not add the new peer's route\nran: %v", cmds)
	}

	got := m.InstalledRoutes()
	want := map[string]bool{"10.244.9.0/24": true, "10.99.0.9/32": true}
	if len(got) != len(want) {
		t.Fatalf("InstalledRoutes = %v, want %v", got, want)
	}
	for _, r := range got {
		if !want[r] {
			t.Errorf("unexpected route %q still installed", r)
		}
	}
	if names := m.Peers(); len(names) != 1 || names[0] != "mac-09" {
		t.Errorf("Peers = %v, want [mac-09]", names)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	fr := newFakeRunner()
	cfg := testConfig(t)
	m := New(cfg, WithRunner(fr))
	ctx := context.Background()
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	routesAfterUp := append([]string(nil), m.InstalledRoutes()...)
	mark := len(fr.strings())

	// Re-applying the identical peer set must not add or delete any route.
	for i := 0; i < 2; i++ {
		if err := m.Sync(ctx, cfg.Peers); err != nil {
			t.Fatalf("Sync #%d: %v", i, err)
		}
	}
	for _, c := range fr.strings()[mark:] {
		if strings.Contains(c, "route -q -n add") || strings.Contains(c, "route -q -n delete") {
			t.Errorf("idempotent Sync issued a route change: %q", c)
		}
	}
	if got := m.InstalledRoutes(); len(got) != len(routesAfterUp) {
		t.Errorf("routes changed after idempotent sync: %v -> %v", routesAfterUp, got)
	}
	if names := m.Peers(); len(names) != 1 || names[0] != "mac-02" {
		t.Errorf("Peers = %v, want [mac-02]", names)
	}
}

func TestSyncAddingPeerPreservesExisting(t *testing.T) {
	fr := newFakeRunner()
	cfg := testConfig(t)
	m := New(cfg, WithRunner(fr))
	ctx := context.Background()
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mark := len(fr.strings())

	// Add a second peer while keeping the original. Adding a node must never tear
	// down an existing peer's route (which is what a local Pod attachment relies
	// on), so no `route delete` may run.
	extra := Peer{Name: "mac-09", PublicKey: testKey(t), AllowedIPs: []string{"10.244.9.0/24"}}
	next := append(append([]Peer(nil), cfg.Peers...), extra)
	if err := m.Sync(ctx, next); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	for _, c := range fr.strings()[mark:] {
		if strings.Contains(c, "route -q -n delete") {
			t.Errorf("adding a peer must not delete existing routes: %q", c)
		}
	}
	if !contains(fr.strings(), "route -q -n add -inet 10.244.9.0/24 -interface utun7") {
		t.Errorf("new peer's route was not added\nran: %v", fr.strings())
	}
	routes := map[string]bool{}
	for _, r := range m.InstalledRoutes() {
		routes[r] = true
	}
	for _, want := range []string{"10.244.2.0/24", "10.99.0.2/32", "10.244.9.0/24"} {
		if !routes[want] {
			t.Errorf("route %q missing after add (have %v)", want, m.InstalledRoutes())
		}
	}
}

func TestSyncBeforeUpFails(t *testing.T) {
	m := New(testConfig(t), WithRunner(newFakeRunner()))
	if err := m.Sync(context.Background(), nil); err == nil {
		t.Fatal("Sync before Up should fail")
	}
}

func TestDownRemovesRoutesAndInterface(t *testing.T) {
	fr := newFakeRunner()
	m := New(testConfig(t), WithRunner(fr))
	ctx := context.Background()
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := m.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}
	cmds := fr.strings()
	if !contains(cmds, "route -q -n delete -inet 10.244.2.0/24 -interface utun7") {
		t.Errorf("Down did not delete routes\nran: %v", cmds)
	}
	if !contains(cmds, "ifconfig utun7 destroy") {
		t.Errorf("Down did not destroy the interface\nran: %v", cmds)
	}
	if !contains(cmds, "pkill -f wireguard-go utun7") {
		t.Errorf("Down did not stop wireguard-go\nran: %v", cmds)
	}
	if got := m.InstalledRoutes(); len(got) != 0 {
		t.Errorf("routes remain after Down: %v", got)
	}
}

func TestDownIsBestEffort(t *testing.T) {
	fr := newFakeRunner()
	m := New(testConfig(t), WithRunner(fr))
	ctx := context.Background()
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	// Even if teardown commands fail, Down must not error (cleanup is best-effort).
	fr.failOn["ifconfig utun7 destroy"] = "some unexpected failure"
	fr.failOn["pkill -f wireguard-go utun7"] = "pkill: no matching processes were found"
	if err := m.Down(ctx); err != nil {
		t.Fatalf("Down should be best-effort, got: %v", err)
	}
}
