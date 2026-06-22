package criserver

import (
	"context"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func runReq(ns, name, uid string) *runtimeapi.RunPodSandboxRequest {
	return &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{
				Name:      name,
				Uid:       uid,
				Namespace: ns,
				Attempt:   0,
			},
			Hostname:     name,
			LogDirectory: "/var/log/pods/" + ns + "_" + name,
			Labels:       map[string]string{"app": name},
			Annotations:  map[string]string{"note": "spike"},
			DnsConfig:    &runtimeapi.DNSConfig{Servers: []string{"10.0.0.10"}},
		},
		RuntimeHandler: "macvz",
	}
}

func mustRun(t *testing.T, s *Server, ns, name, uid string) string {
	t.Helper()
	resp, err := s.RunPodSandbox(context.Background(), runReq(ns, name, uid))
	if err != nil {
		t.Fatalf("RunPodSandbox(%s/%s): %v", ns, name, err)
	}
	if resp.GetPodSandboxId() == "" {
		t.Fatal("RunPodSandbox returned empty id")
	}
	return resp.GetPodSandboxId()
}

func TestSandboxLifecycle(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()

	id := mustRun(t, s, "default", "web", "uid-web")

	// Status reflects the persisted metadata and Ready state.
	stResp, err := s.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: id, Verbose: true})
	if err != nil {
		t.Fatalf("PodSandboxStatus: %v", err)
	}
	got := stResp.GetStatus()
	if got.GetState() != runtimeapi.PodSandboxState_SANDBOX_READY {
		t.Errorf("state = %v, want READY", got.GetState())
	}
	if got.GetMetadata().GetNamespace() != "default" || got.GetMetadata().GetName() != "web" || got.GetMetadata().GetUid() != "uid-web" {
		t.Errorf("metadata round-trip failed: %+v", got.GetMetadata())
	}
	if got.GetNetwork() != nil {
		t.Error("state-only sandbox must not report a Pod IP")
	}
	if stResp.GetInfo()["model"] != "state-only-sandbox-spike" {
		t.Errorf("verbose info missing model marker: %v", stResp.GetInfo())
	}

	// ListPodSandbox shows it.
	list, err := s.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{})
	if err != nil || len(list.GetItems()) != 1 {
		t.Fatalf("ListPodSandbox = (%v, %v), want 1 item", list, err)
	}

	// Stop -> NotReady, idempotent.
	for i := 0; i < 2; i++ {
		if _, err := s.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: id}); err != nil {
			t.Fatalf("StopPodSandbox (call %d): %v", i, err)
		}
	}
	stResp, _ = s.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: id})
	if stResp.GetStatus().GetState() != runtimeapi.PodSandboxState_SANDBOX_NOTREADY {
		t.Errorf("state after Stop = %v, want NOTREADY", stResp.GetStatus().GetState())
	}

	// Remove -> gone, idempotent.
	for i := 0; i < 2; i++ {
		if _, err := s.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: id}); err != nil {
			t.Fatalf("RemovePodSandbox (call %d): %v", i, err)
		}
	}
	if _, err := s.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{PodSandboxId: id}); status.Code(err) != codes.NotFound {
		t.Errorf("PodSandboxStatus after Remove code = %v, want NotFound", status.Code(err))
	}
	list, _ = s.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{})
	if len(list.GetItems()) != 0 {
		t.Errorf("ListPodSandbox after Remove = %d items, want 0", len(list.GetItems()))
	}
}

func TestStopAndRemoveAbsentSandboxSucceed(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	if _, err := s.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: "ghost"}); err != nil {
		t.Errorf("StopPodSandbox(absent): %v", err)
	}
	if _, err := s.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: "ghost"}); err != nil {
		t.Errorf("RemovePodSandbox(absent): %v", err)
	}
}

func TestRunPodSandboxValidation(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	cases := []*runtimeapi.RunPodSandboxRequest{
		{},                                       // nil config
		{Config: &runtimeapi.PodSandboxConfig{}}, // nil metadata
		{Config: &runtimeapi.PodSandboxConfig{Metadata: &runtimeapi.PodSandboxMetadata{Name: "x"}}}, // missing ns/uid
	}
	for i, req := range cases {
		if _, err := s.RunPodSandbox(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("case %d: code = %v, want InvalidArgument", i, status.Code(err))
		}
	}
	if _, err := s.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("PodSandboxStatus empty id code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestRunPodSandboxRejectsDuplicatePodKey(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	first := mustRun(t, s, "default", "web", "uid-web")

	_, err := s.RunPodSandbox(ctx, runReq("default", "web", "uid-web-retry"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("duplicate RunPodSandbox code = %v, want FailedPrecondition", status.Code(err))
	}
	list, err := s.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{})
	if err != nil {
		t.Fatalf("ListPodSandbox: %v", err)
	}
	if len(list.GetItems()) != 1 || list.GetItems()[0].GetId() != first {
		t.Fatalf("duplicate RunPodSandbox changed sandbox list: %+v", list.GetItems())
	}
}

func TestRunPodSandboxIdempotentRetryReturnsExistingSandbox(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	req := runReq("default", "web", "uid-web")
	req.Config.Metadata.Attempt = 2

	first, err := s.RunPodSandbox(ctx, req)
	if err != nil {
		t.Fatalf("first RunPodSandbox: %v", err)
	}
	second, err := s.RunPodSandbox(ctx, req)
	if err != nil {
		t.Fatalf("retry RunPodSandbox: %v", err)
	}
	if second.GetPodSandboxId() != first.GetPodSandboxId() {
		t.Fatalf("retry id = %q, want existing %q", second.GetPodSandboxId(), first.GetPodSandboxId())
	}
	list, err := s.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{})
	if err != nil {
		t.Fatalf("ListPodSandbox: %v", err)
	}
	if len(list.GetItems()) != 1 {
		t.Fatalf("sandbox count after retry = %d, want 1", len(list.GetItems()))
	}
}

func TestRunPodSandboxRejectsHostNamespaceWithSchedulingHint(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	req := runReq("kube-system", "cni-agent", "uid-cni")
	req.Config.Linux = &runtimeapi.LinuxPodSandboxConfig{
		SecurityContext: &runtimeapi.LinuxSandboxSecurityContext{
			NamespaceOptions: &runtimeapi.NamespaceOption{
				Network: runtimeapi.NamespaceMode_NODE,
			},
		},
	}
	_, err := s.RunPodSandbox(ctx, req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("host-namespace RunPodSandbox code = %v, want InvalidArgument", status.Code(err))
	}
	// The rejection must name the offending field AND point at the honest
	// scheduling-exclusion scheme (#84) so the failure is actionable, not opaque.
	msg := status.Convert(err).Message()
	for _, want := range []string{"hostNetwork", NodeTaint(), NodeHostNamespaceLabel} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection %q does not mention %q", msg, want)
		}
	}
	// Nothing should be left reserved after a rejected sandbox.
	list, _ := s.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{})
	if len(list.GetItems()) != 0 {
		t.Errorf("rejected host-namespace sandbox left %d items", len(list.GetItems()))
	}
}

func TestListPodSandboxFilters(t *testing.T) {
	s := New(Options{})
	ctx := context.Background()
	webID := mustRun(t, s, "default", "web", "uid-web")
	dbID := mustRun(t, s, "default", "db", "uid-db")
	_, _ = s.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: dbID})

	// Filter by ID.
	list, _ := s.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{Filter: &runtimeapi.PodSandboxFilter{Id: webID}})
	if len(list.GetItems()) != 1 || list.GetItems()[0].GetId() != webID {
		t.Errorf("id filter = %+v, want only web", list.GetItems())
	}

	// Filter by state READY -> only web.
	list, _ = s.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{
		Filter: &runtimeapi.PodSandboxFilter{State: &runtimeapi.PodSandboxStateValue{State: runtimeapi.PodSandboxState_SANDBOX_READY}},
	})
	if len(list.GetItems()) != 1 || list.GetItems()[0].GetId() != webID {
		t.Errorf("ready filter = %+v, want only web", list.GetItems())
	}

	// Filter by label.
	list, _ = s.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{
		Filter: &runtimeapi.PodSandboxFilter{LabelSelector: map[string]string{"app": "db"}},
	})
	if len(list.GetItems()) != 1 || list.GetItems()[0].GetId() != dbID {
		t.Errorf("label filter = %+v, want only db", list.GetItems())
	}
}

// TestSandboxPersistenceAcrossServerRestart proves a disk-backed store keeps the
// kubelet's sandbox view across an adapter restart — the CRI restart-tolerance
// requirement this spike must demonstrate.
func TestSandboxPersistenceAcrossServerRestart(t *testing.T) {
	dir := t.TempDir()
	st1, _, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s1 := New(Options{Sandboxes: st1})
	id := mustRun(t, s1, "default", "web", "uid-web")

	// Simulate a restart: fresh store + server over the same dir.
	st2, skipped, err := store.New(dir)
	if err != nil || skipped != 0 {
		t.Fatalf("reopen store = (%v, %v)", skipped, err)
	}
	s2 := New(Options{Sandboxes: st2})
	resp, err := s2.PodSandboxStatus(context.Background(), &runtimeapi.PodSandboxStatusRequest{PodSandboxId: id})
	if err != nil {
		t.Fatalf("PodSandboxStatus after restart: %v", err)
	}
	if resp.GetStatus().GetMetadata().GetName() != "web" {
		t.Errorf("metadata lost across restart: %+v", resp.GetStatus().GetMetadata())
	}
}
