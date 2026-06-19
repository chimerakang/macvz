package health

import (
	"context"
	"errors"
	"testing"
	"time"
)

// --- runtime ------------------------------------------------------------------

type fakeRuntime struct{ err error }

func (f fakeRuntime) Ready(context.Context) error { return f.err }

func TestRuntimeChecker(t *testing.T) {
	tests := []struct {
		name   string
		probe  RuntimeProbe
		status Status
	}{
		{"nil probe skipped", nil, StatusSkipped},
		{"ready passes", fakeRuntime{nil}, StatusPass},
		{"down fails", fakeRuntime{errors.New("not ready")}, StatusFail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NewRuntimeChecker(tc.probe).Check(context.Background())
			if got.Status != tc.status {
				t.Fatalf("status=%q want %q", got.Status, tc.status)
			}
			if got.Class != ClassRuntime {
				t.Fatalf("class=%q", got.Class)
			}
		})
	}
}

// --- control plane ------------------------------------------------------------

type fakeControlPlane struct {
	node     NodeState
	nodeErr  error
	lease    LeaseState
	leaseErr error
}

func (f fakeControlPlane) NodeState(context.Context) (NodeState, error) {
	return f.node, f.nodeErr
}
func (f fakeControlPlane) LeaseState(context.Context) (LeaseState, error) {
	return f.lease, f.leaseErr
}

func registrationCheck(t *testing.T, probe ControlPlaneProbe) Check {
	t.Helper()
	for _, c := range NewControlPlaneCheckers(probe) {
		if got := c.Check(context.Background()); got.Name == "kubelet-registration" {
			return got
		}
	}
	t.Fatal("no registration check")
	return Check{}
}

func leaseCheck(t *testing.T, probe ControlPlaneProbe) Check {
	t.Helper()
	for _, c := range NewControlPlaneCheckers(probe) {
		if got := c.Check(context.Background()); got.Name == "node-lease" {
			return got
		}
	}
	t.Fatal("no lease check")
	return Check{}
}

func TestNodeRegistrationChecker(t *testing.T) {
	if got := registrationCheck(t, nil); got.Status != StatusFail {
		t.Fatalf("nil probe should fail, got %q", got.Status)
	}
	if got := registrationCheck(t, fakeControlPlane{nodeErr: errors.New("unreachable")}); got.Status != StatusFail {
		t.Fatalf("api error should fail, got %q", got.Status)
	}
	if got := registrationCheck(t, fakeControlPlane{node: NodeState{Registered: false}}); got.Status != StatusFail {
		t.Fatalf("unregistered should fail, got %q", got.Status)
	}
	if got := registrationCheck(t, fakeControlPlane{node: NodeState{Registered: true, Ready: false, Reason: "RuntimeNotReady"}}); got.Status != StatusFail {
		t.Fatalf("not-ready should fail, got %q", got.Status)
	}
	if got := registrationCheck(t, fakeControlPlane{node: NodeState{Registered: true, Ready: true}}); got.Status != StatusPass {
		t.Fatalf("registered+ready should pass, got %q", got.Status)
	}
}

func TestNodeLeaseChecker(t *testing.T) {
	ready := NodeState{Registered: true, Ready: true}

	if got := leaseCheck(t, fakeControlPlane{node: ready, lease: LeaseState{Enabled: false}}); got.Status != StatusSkipped {
		t.Fatalf("disabled lease should skip, got %q", got.Status)
	}
	if got := leaseCheck(t, fakeControlPlane{node: ready, lease: LeaseState{Enabled: true, Found: false}}); got.Status != StatusFail {
		t.Fatalf("missing lease should fail, got %q", got.Status)
	}
	stale := LeaseState{Enabled: true, Found: true, Age: 2 * time.Minute, Stale: 40 * time.Second}
	if got := leaseCheck(t, fakeControlPlane{node: ready, lease: stale}); got.Status != StatusFail {
		t.Fatalf("stale lease should fail, got %q", got.Status)
	}
	fresh := LeaseState{Enabled: true, Found: true, Age: 5 * time.Second, Stale: 40 * time.Second}
	if got := leaseCheck(t, fakeControlPlane{node: ready, lease: fresh}); got.Status != StatusPass {
		t.Fatalf("fresh lease should pass, got %q", got.Status)
	}
}

// --- privileged helper --------------------------------------------------------

type fakeHelper struct {
	info    HelperInfo
	infoErr error
	pingErr error
}

func (f fakeHelper) Status(context.Context) (HelperInfo, error) { return f.info, f.infoErr }
func (f fakeHelper) Ping(context.Context) error                 { return f.pingErr }

func TestHelperChecker(t *testing.T) {
	if got := NewHelperChecker(false, nil).Check(context.Background()); got.Status != StatusSkipped {
		t.Fatalf("disabled helper should skip, got %q", got.Status)
	}
	if got := NewHelperChecker(true, nil).Check(context.Background()); got.Status != StatusFail {
		t.Fatalf("required-but-nil helper should fail, got %q", got.Status)
	}
	if got := NewHelperChecker(true, fakeHelper{infoErr: errors.New("dial: no socket")}).Check(context.Background()); got.Status != StatusFail {
		t.Fatalf("unreachable helper should fail, got %q", got.Status)
	}
	noPolicy := fakeHelper{info: HelperInfo{PolicyEnforced: false}}
	if got := NewHelperChecker(true, noPolicy).Check(context.Background()); got.Status != StatusFail {
		t.Fatalf("unenforced policy should fail, got %q", got.Status)
	}
	cantRun := fakeHelper{info: HelperInfo{PolicyEnforced: true}, pingErr: errors.New("denied")}
	if got := NewHelperChecker(true, cantRun).Check(context.Background()); got.Status != StatusFail {
		t.Fatalf("ping failure should fail, got %q", got.Status)
	}
	ok := fakeHelper{info: HelperInfo{Version: "v1", Protocol: 1, PolicyEnforced: true, AllowedCommands: []string{"wg", "route"}}}
	if got := NewHelperChecker(true, ok).Check(context.Background()); got.Status != StatusPass {
		t.Fatalf("healthy helper should pass, got %q (%s)", got.Status, got.Detail)
	}
}

// --- mesh ---------------------------------------------------------------------

type fakeMesh struct {
	iface  string
	peers  []string
	routes []string
}

func (f fakeMesh) InterfaceName() string     { return f.iface }
func (f fakeMesh) Peers() []string           { return f.peers }
func (f fakeMesh) InstalledRoutes() []string { return f.routes }

func TestMeshChecker(t *testing.T) {
	if got := NewMeshChecker(false, nil).Check(context.Background()); got.Status != StatusSkipped {
		t.Fatalf("disabled mesh should skip, got %q", got.Status)
	}
	noPeers := fakeMesh{iface: "utun7"}
	if got := NewMeshChecker(true, noPeers).Check(context.Background()); got.Status != StatusWarn {
		t.Fatalf("no-peer mesh should warn, got %q", got.Status)
	}
	noRoutes := fakeMesh{iface: "utun7", peers: []string{"b"}}
	if got := NewMeshChecker(true, noRoutes).Check(context.Background()); got.Status != StatusFail {
		t.Fatalf("peers-without-routes should fail, got %q", got.Status)
	}
	healthy := fakeMesh{iface: "utun7", peers: []string{"b"}, routes: []string{"10.0.0.0/24"}}
	if got := NewMeshChecker(true, healthy).Check(context.Background()); got.Status != StatusPass {
		t.Fatalf("healthy mesh should pass, got %q", got.Status)
	}
}

// --- forwarding ---------------------------------------------------------------

type fakeForwarding struct {
	on  bool
	err error
}

func (f fakeForwarding) IPForwardingEnabled(context.Context) (bool, error) { return f.on, f.err }

func TestForwardingChecker(t *testing.T) {
	if got := NewForwardingChecker(false, nil).Check(context.Background()); got.Status != StatusSkipped {
		t.Fatalf("disabled should skip, got %q", got.Status)
	}
	if got := NewForwardingChecker(true, fakeForwarding{err: errors.New("x")}).Check(context.Background()); got.Status != StatusFail {
		t.Fatalf("read error should fail, got %q", got.Status)
	}
	if got := NewForwardingChecker(true, fakeForwarding{on: false}).Check(context.Background()); got.Status != StatusFail {
		t.Fatalf("forwarding off should fail, got %q", got.Status)
	}
	if got := NewForwardingChecker(true, fakeForwarding{on: true}).Check(context.Background()); got.Status != StatusPass {
		t.Fatalf("forwarding on should pass, got %q", got.Status)
	}
}

// --- pod network --------------------------------------------------------------

type fakeAttachments struct{ n int }

func (f fakeAttachments) AttachmentCount() int { return f.n }

func TestPodNetworkChecker(t *testing.T) {
	if got := NewPodNetworkChecker(false, "", "", nil).Check(context.Background()); got.Status != StatusSkipped {
		t.Fatalf("disabled should skip, got %q", got.Status)
	}
	got := NewPodNetworkChecker(true, "bridge100", "macvz/pods", fakeAttachments{n: 3}).Check(context.Background())
	if got.Status != StatusPass {
		t.Fatalf("configured path should pass, got %q", got.Status)
	}
	if got.Detail == "" {
		t.Fatal("expected detail with attachment count")
	}
}
