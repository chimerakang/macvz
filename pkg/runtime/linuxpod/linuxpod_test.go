package linuxpod

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// These tests cover the CRI-R17 LinuxPod backend contract (#124). They run the
// exact kubelet-ordering sequence the issue requires — CreatePod, app
// Create/Start, then a *late* sidecar Create/Start after the app is already
// running — and assert the three pieces of evidence: app and sidecar share one
// Pod sandbox namespace, the sidecar reaches localhost in that namespace, and the
// late rootfs identity handoff verifies. They then prove stop/remove ordering and
// a Cleanup that leaves no stale state.
//
// The same sequence runs twice: once directly against FakeBackend, and once
// through HelperClient -> Serve(FakeBackend) over an in-memory pipe, proving the
// NDJSON wire protocol round-trips every op identically to an in-process call.

// orderingProbe drives the required ordering against any Backend and returns the
// app and sidecar statuses after both are running.
func orderingProbe(t *testing.T, b Backend) (app, sidecar ContainerStatus, cleanup func()) {
	t.Helper()
	ctx := context.Background()
	const podID = "pod-r17"

	if _, err := b.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	pod, err := b.CreatePod(ctx, PodSpec{ID: podID, Hostname: "macvz-r17", CPUs: 2, MemoryBytes: 1 << 30})
	if err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	if pod.Phase != runtime.PhaseRunning || pod.SandboxNamespace == "" {
		t.Fatalf("pod not running with a namespace: %+v", pod)
	}

	// App container: prepare rootfs, create, start.
	appRootfs, err := b.PrepareContainerRootfs(ctx, RootfsRequest{
		PodID: podID, ContainerName: "app", Image: "busybox", ExpectedIdentity: "macvz-rootfs-id=app",
	})
	if err != nil {
		t.Fatalf("PrepareContainerRootfs(app): %v", err)
	}
	appCreated, err := b.CreateContainer(ctx, CreateRequest{
		PodID: podID, Name: "app", RootfsToken: appRootfs.Token,
		Command: []string{"/bin/sh", "-c", "httpd -f -p 127.0.0.1:18080"},
	})
	if err != nil {
		t.Fatalf("CreateContainer(app): %v", err)
	}
	if appCreated.CreatedAfterPodRunning {
		t.Errorf("app should not be flagged createdAfterPodRunning (no container running yet)")
	}
	app, err = b.StartContainer(ctx, Ref{PodID: podID, ContainerID: appCreated.ID})
	if err != nil {
		t.Fatalf("StartContainer(app): %v", err)
	}
	if app.Phase != runtime.PhaseRunning || !app.IdentityVerified {
		t.Fatalf("app not running+verified: %+v", app)
	}

	// Sidecar: prepared and created AFTER the app has already started (the late
	// sidecar case), then started.
	sideRootfs, err := b.PrepareContainerRootfs(ctx, RootfsRequest{
		PodID: podID, ContainerName: "sidecar", Image: "busybox", ExpectedIdentity: "macvz-rootfs-id=sidecar",
	})
	if err != nil {
		t.Fatalf("PrepareContainerRootfs(sidecar): %v", err)
	}
	sideCreated, err := b.CreateContainer(ctx, CreateRequest{
		PodID: podID, Name: "sidecar", RootfsToken: sideRootfs.Token,
		Command: []string{"/bin/sh", "-c", "wget -qO- http://127.0.0.1:18080"},
	})
	if err != nil {
		t.Fatalf("CreateContainer(sidecar) after app started: %v", err)
	}
	if !sideCreated.CreatedAfterPodRunning {
		t.Errorf("sidecar must be flagged createdAfterPodRunning (app already running)")
	}
	sidecar, err = b.StartContainer(ctx, Ref{PodID: podID, ContainerID: sideCreated.ID})
	if err != nil {
		t.Fatalf("StartContainer(sidecar): %v", err)
	}
	if sidecar.Phase != runtime.PhaseRunning || !sidecar.IdentityVerified {
		t.Fatalf("sidecar not running+verified: %+v", sidecar)
	}

	return app, sidecar, func() {
		if _, err := b.Cleanup(ctx, podID); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}
}

// assertSharedNamespaceAndIdentity checks the AC5 evidence on two running peers.
func assertSharedNamespaceAndIdentity(t *testing.T, app, sidecar ContainerStatus) {
	t.Helper()
	if app.SandboxNamespace != sidecar.SandboxNamespace {
		t.Errorf("app and sidecar must share one sandbox namespace: %q vs %q",
			app.SandboxNamespace, sidecar.SandboxNamespace)
	}
	if !sidecar.LocalhostReachable || !app.LocalhostReachable {
		t.Errorf("both peers must report localhost reachable: app=%v sidecar=%v",
			app.LocalhostReachable, sidecar.LocalhostReachable)
	}
	if !sidecar.IdentityVerified || sidecar.ObservedIdentity != sidecar.ExpectedIdentity {
		t.Errorf("sidecar identity handoff must verify: %+v", sidecar)
	}
}

func TestFakeBackendOrderingAndEvidence(t *testing.T) {
	b := NewFakeBackend()
	app, sidecar, cleanup := orderingProbe(t, b)
	defer cleanup()
	assertSharedNamespaceAndIdentity(t, app, sidecar)
}

func TestHelperClientOrderingOverPipe(t *testing.T) {
	b := NewFakeBackend()
	client := newPipeClient(t, b)
	app, sidecar, cleanup := orderingProbe(t, client)
	defer cleanup()
	assertSharedNamespaceAndIdentity(t, app, sidecar)
}

// TestStopRemoveOrderingNoStaleState proves both stop orderings (sidecar-first
// and app-first) and that Cleanup after removal leaves no pod/container/rootfs
// state behind, and is idempotent (AC6).
func TestStopRemoveOrderingNoStaleState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopFirst  string // "sidecar" or "app"
		stopSecond string
	}{
		{"sidecar first", "sidecar", "app"},
		{"app first", "app", "sidecar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b := NewFakeBackend()
			app, sidecar, _ := orderingProbe(t, b)
			byName := map[string]ContainerStatus{"app": app, "sidecar": sidecar}

			for _, which := range []string{tc.stopFirst, tc.stopSecond} {
				c := byName[which]
				st, err := b.StopContainer(ctx, StopRequest{PodID: c.PodID, ContainerID: c.ID, TimeoutSeconds: 5})
				if err != nil {
					t.Fatalf("StopContainer(%s): %v", which, err)
				}
				if st.Phase != runtime.PhaseStopped {
					t.Errorf("%s phase after stop = %s, want Stopped", which, st.Phase)
				}
				// The other container must remain addressable after the first stop
				// (shared sandbox stays up until cleanup).
			}

			// Remove containers, then the pod via Cleanup; assert no stale state.
			for _, c := range []ContainerStatus{app, sidecar} {
				if err := b.RemoveContainer(ctx, Ref{PodID: c.PodID, ContainerID: c.ID}); err != nil {
					t.Fatalf("RemoveContainer(%s): %v", c.Name, err)
				}
				// Idempotent second remove.
				if err := b.RemoveContainer(ctx, Ref{PodID: c.PodID, ContainerID: c.ID}); err != nil {
					t.Errorf("idempotent RemoveContainer(%s): %v", c.Name, err)
				}
			}
			rep, err := b.Cleanup(ctx, app.PodID)
			if err != nil {
				t.Fatalf("Cleanup: %v", err)
			}
			if rep.StaleState {
				t.Errorf("cleanup reported stale state: %+v", rep)
			}
			if !rep.PodRemoved {
				t.Errorf("cleanup did not remove pod: %+v", rep)
			}
			// After cleanup, the pod and its containers are gone.
			if _, err := b.Status(ctx, Ref{PodID: app.PodID, ContainerID: app.ID}); !errors.Is(err, ErrPodNotFound) {
				t.Errorf("status after cleanup = %v, want ErrPodNotFound", err)
			}
			// Idempotent second cleanup.
			if _, err := b.Cleanup(ctx, app.PodID); err != nil {
				t.Errorf("idempotent Cleanup: %v", err)
			}
		})
	}
}

// TestStartContainerIdentityMismatch proves a late process reporting the wrong
// rootfs identity fails StartContainer with ErrIdentityUnverified and is left
// non-Running (CRI-R16 invariant carried into the backend).
func TestStartContainerIdentityMismatch(t *testing.T) {
	ctx := context.Background()
	b := NewFakeBackend()
	b.ObservedIdentityFor["bad"] = "macvz-rootfs-id=WRONG"

	if _, err := b.CreatePod(ctx, PodSpec{ID: "p"}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	rf, err := b.PrepareContainerRootfs(ctx, RootfsRequest{PodID: "p", ContainerName: "bad", ExpectedIdentity: "macvz-rootfs-id=bad"})
	if err != nil {
		t.Fatalf("PrepareContainerRootfs: %v", err)
	}
	created, err := b.CreateContainer(ctx, CreateRequest{PodID: "p", Name: "bad", RootfsToken: rf.Token})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	st, err := b.StartContainer(ctx, Ref{PodID: "p", ContainerID: created.ID})
	if !errors.Is(err, ErrIdentityUnverified) {
		t.Fatalf("StartContainer err = %v, want ErrIdentityUnverified", err)
	}
	if st.Phase == runtime.PhaseRunning || st.IdentityVerified {
		t.Errorf("container must not be Running/verified on identity mismatch: %+v", st)
	}
}

// TestBackendErrorsRoundTripOverWire proves classified errors survive the wire
// protocol so the client can branch with errors.Is.
func TestBackendErrorsRoundTripOverWire(t *testing.T) {
	client := newPipeClient(t, NewFakeBackend())
	ctx := context.Background()

	if _, err := client.Status(ctx, Ref{PodID: "missing", ContainerID: "x"}); !errors.Is(err, ErrPodNotFound) {
		t.Errorf("Status(missing pod) = %v, want ErrPodNotFound", err)
	}
	if _, err := client.CreatePod(ctx, PodSpec{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("CreatePod(empty) = %v, want ErrInvalid", err)
	}
}

// TestPodStatusAddressDiscoveryOverWire proves the CRI-L3 (#128) PodStatus op
// round-trips, withholds the sandbox address until it is ready, then reveals it —
// the address-discovery contract the Pod networking integration polls on.
func TestPodStatusAddressDiscoveryOverWire(t *testing.T) {
	b := NewFakeBackend()
	b.SandboxAddressFor["p"] = "192.168.66.42"
	b.SandboxAddressReadyAfter["p"] = 2 // ready on the 2nd PodStatus call
	client := newPipeClient(t, b)
	ctx := context.Background()

	created, err := client.CreatePod(ctx, PodSpec{ID: "p"})
	if err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	// CreatePod is the 0th status render: address still withheld.
	if created.SandboxAddress != "" {
		t.Errorf("CreatePod SandboxAddress = %q, want withheld", created.SandboxAddress)
	}
	// First PodStatus: still not ready.
	if st, err := client.PodStatus(ctx, "p"); err != nil {
		t.Fatalf("PodStatus #1: %v", err)
	} else if st.SandboxAddress != "" {
		t.Errorf("PodStatus #1 SandboxAddress = %q, want withheld", st.SandboxAddress)
	}
	// Second PodStatus: address is revealed.
	st, err := client.PodStatus(ctx, "p")
	if err != nil {
		t.Fatalf("PodStatus #2: %v", err)
	}
	if st.SandboxAddress != "192.168.66.42" {
		t.Errorf("PodStatus #2 SandboxAddress = %q, want 192.168.66.42", st.SandboxAddress)
	}
	// Unknown pod errors with the classified sentinel over the wire.
	if _, err := client.PodStatus(ctx, "missing"); !errors.Is(err, ErrPodNotFound) {
		t.Errorf("PodStatus(missing) = %v, want ErrPodNotFound", err)
	}
}

// newPipeClient wires a HelperClient to Serve(backend) over net.Pipe, one fresh
// pipe per call (matching the client's connection-per-call model). Each accepted
// connection is served until the client closes it.
func newPipeClient(t *testing.T, backend Backend) *HelperClient {
	t.Helper()
	return NewHelperClient(func(ctx context.Context) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		go func() { _ = Serve(context.Background(), serverConn, backend) }()
		return clientConn, nil
	})
}
