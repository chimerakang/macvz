package linuxpod

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// adopt_test.go covers the live-VM adoption contract (#138): a helper that kept its
// Pod VMs across its own process restart reattaches to them so the adapter need not
// recreate the Pod, while a helper whose VM did not survive falls back honestly to
// "not found" (the supported BackendLost/recreate path). Both the in-process
// FakeBackend and the NDJSON wire path (HelperClient -> Serve(FakeBackend)) are
// exercised so the new op round-trips identically.

// adoptSeedPod creates a running pod with one running container against b and
// returns the pod id and the backend container id.
func adoptSeedPod(t *testing.T, b Backend, podID, name string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := b.CreatePod(ctx, PodSpec{ID: podID}); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	rf, err := b.PrepareContainerRootfs(ctx, RootfsRequest{
		PodID: podID, ContainerName: name, Image: "busybox", ExpectedIdentity: "macvz-rootfs-id=" + name,
	})
	if err != nil {
		t.Fatalf("PrepareContainerRootfs: %v", err)
	}
	created, err := b.CreateContainer(ctx, CreateRequest{PodID: podID, Name: name, RootfsToken: rf.Token})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if _, err := b.StartContainer(ctx, Ref{PodID: podID, ContainerID: created.ID}); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	return created.ID
}

func TestFakeBackendAdoptsLiveVMAfterRestart(t *testing.T) {
	ctx := context.Background()
	b := NewFakeBackend()
	const podID = "pod-adopt"
	cID := adoptSeedPod(t, b, podID, "app")

	// The helper restarts but the Pod VM survives.
	b.SimulateHelperRestart()

	// Ping advertises a successful adoption pass.
	info, err := b.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !info.Capabilities.Adopt {
		t.Fatalf("fake backend must advertise the Adopt capability")
	}
	if !info.Adoption.Supported || info.Adoption.AdoptedPods != 1 || info.Adoption.LostPods != 0 {
		t.Fatalf("adoption status = %+v, want supported with 1 adopted / 0 lost", info.Adoption)
	}

	// Adopt reports the pod reattached with its container still running.
	res, err := b.Adopt(ctx, podID)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !res.Adopted {
		t.Fatalf("Adopt(%s) not adopted: %q", podID, res.Reason)
	}
	if len(res.Containers) != 1 || res.Containers[0].ID != cID || res.Containers[0].Phase != runtime.PhaseRunning {
		t.Fatalf("adopted containers = %+v, want one running %s", res.Containers, cID)
	}

	// The live surfaces work after adoption: PodStatus and Status both observe the
	// reattached VM rather than ErrPodNotFound.
	if _, err := b.PodStatus(ctx, podID); err != nil {
		t.Fatalf("PodStatus after adoption: %v", err)
	}
	st, err := b.Status(ctx, Ref{PodID: podID, ContainerID: cID})
	if err != nil {
		t.Fatalf("Status after adoption: %v", err)
	}
	if st.Phase != runtime.PhaseRunning || !st.IdentityVerified {
		t.Fatalf("post-adoption status = %+v, want running and identity-verified (start invariant)", st)
	}
}

func TestFakeBackendAdoptionFallsBackWhenVMGone(t *testing.T) {
	ctx := context.Background()
	b := NewFakeBackend()
	const podID = "pod-lost"
	adoptSeedPod(t, b, podID, "app")

	// Model the Pod VM dying with the helper process.
	b.VMSurvivesRestart[podID] = false
	b.SimulateHelperRestart()

	info, err := b.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if info.Adoption.AdoptedPods != 0 || info.Adoption.LostPods != 1 {
		t.Fatalf("adoption status = %+v, want 0 adopted / 1 lost", info.Adoption)
	}

	// Adopt reports the pod could not be reacquired — no error, just Adopted=false so
	// the adapter falls back to recreate.
	res, err := b.Adopt(ctx, podID)
	if err != nil {
		t.Fatalf("Adopt should not error on a recoverable-by-recreate pod: %v", err)
	}
	if res.Adopted || res.Reason == "" {
		t.Fatalf("Adopt(%s) = %+v, want not-adopted with a reason", podID, res)
	}

	// And the live surface is honestly gone, driving the fail-fast path.
	if _, err := b.PodStatus(ctx, podID); !errors.Is(err, ErrPodNotFound) {
		t.Fatalf("PodStatus after lost VM = %v, want ErrPodNotFound", err)
	}
}

func TestFakeBackendCleanupRemovesAdoptionJournal(t *testing.T) {
	ctx := context.Background()
	b := NewFakeBackend()
	const podID = "pod-clean-journal"
	adoptSeedPod(t, b, podID, "app")
	b.SimulateHelperRestart()

	if len(b.journal) != 1 {
		t.Fatalf("precondition: journal entries = %d, want 1", len(b.journal))
	}
	if _, err := b.Cleanup(ctx, podID); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(b.journal) != 0 {
		t.Fatalf("cleanup left adoption journal entries: %+v", b.journal)
	}
	if _, err := b.Adopt(ctx, podID); !errors.Is(err, ErrPodNotFound) {
		t.Fatalf("Adopt after cleanup = %v, want ErrPodNotFound", err)
	}

	// Reusing the same Pod id must start from a clean journal.
	adoptSeedPod(t, b, podID, "app")
	if len(b.journal) != 0 {
		t.Fatalf("CreatePod reused stale adoption journal entries: %+v", b.journal)
	}
}

func TestFakeBackendAdoptUnknownPodNotFound(t *testing.T) {
	b := NewFakeBackend()
	if _, err := b.Adopt(context.Background(), "never-existed"); !errors.Is(err, ErrPodNotFound) {
		t.Fatalf("Adopt(unknown) = %v, want ErrPodNotFound", err)
	}
}

func TestFakeBackendAdoptUnsupportedWhenCapabilityOff(t *testing.T) {
	b := NewFakeBackend()
	b.Capabilities.Adopt = false
	if _, err := b.Adopt(context.Background(), "p"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Adopt with capability off = %v, want ErrUnsupported", err)
	}
	// A restart on a non-adopting helper loses everything (the legacy path).
	b.SimulateHelperRestart()
	info, _ := b.Ping(context.Background())
	if info.Adoption.Supported {
		t.Fatalf("adoption must not be reported supported when the capability is off: %+v", info.Adoption)
	}
}

// TestAdoptRoundTripsOverWire proves the Adopt op round-trips over the NDJSON
// protocol identically to the in-process call, so HelperClient and the Swift helper
// agree on the new op.
func TestAdoptRoundTripsOverWire(t *testing.T) {
	ctx := context.Background()
	backend := NewFakeBackend()
	const podID = "pod-wire"
	cID := adoptSeedPod(t, backend, podID, "app")
	backend.SimulateHelperRestart()

	clientConn, serverConn := net.Pipe()
	go func() { _ = Serve(ctx, serverConn, backend) }()
	defer clientConn.Close()
	client := NewHelperClient(func(context.Context) (net.Conn, error) { return clientConn, nil })

	res, err := client.Adopt(ctx, podID)
	if err != nil {
		t.Fatalf("client.Adopt: %v", err)
	}
	if !res.Adopted || len(res.Containers) != 1 || res.Containers[0].ID != cID {
		t.Fatalf("wire Adopt = %+v, want adopted with container %s", res, cID)
	}
}
