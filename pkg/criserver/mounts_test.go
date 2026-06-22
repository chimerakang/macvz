package criserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// mountReq builds a CreateContainerRequest with the given CRI mounts.
func mountReq(sandboxID, name string, mounts []*runtimeapi.Mount) *runtimeapi.CreateContainerRequest {
	req := createReq(sandboxID, name)
	req.Config.Mounts = mounts
	return req
}

func newServerWithMountRoot(t *testing.T, rt ContainerRuntime, podsDir string) (*Server, string) {
	t.Helper()
	s := New(Options{Runtime: rt, Mounts: MountPolicy{KubeletPodsDir: podsDir}})
	return s, mustRunSandbox(t, s)
}

func touchMountSource(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir source parent: %v", err)
	}
	if err := os.WriteFile(path, []byte("mount-source"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
}

func TestCreateContainerTranslatesKubeletManagedMounts(t *testing.T) {
	rt := newFakeRuntime()
	podsDir := t.TempDir()
	s, sandboxID := newServerWithMountRoot(t, rt, podsDir)
	ctx := context.Background()

	cfgPath := filepath.Join(podsDir, "uid-1/volumes/kubernetes.io~configmap/cfg")
	scratchPath := filepath.Join(podsDir, "uid-1/volumes/kubernetes.io~empty-dir/scratch")
	touchMountSource(t, cfgPath)
	if err := os.MkdirAll(scratchPath, 0o755); err != nil {
		t.Fatalf("mkdir scratch source: %v", err)
	}
	mounts := []*runtimeapi.Mount{
		// A projected ConfigMap/Secret/SA-token volume the kubelet materialized
		// under its pods dir, mounted read-only.
		{HostPath: cfgPath, ContainerPath: "/etc/app", Readonly: true},
		// An emptyDir the kubelet backs under its pods dir, writable.
		{HostPath: scratchPath, ContainerPath: "/scratch"},
		// A Memory-medium emptyDir: empty host path -> guest tmpfs.
		{HostPath: "", ContainerPath: "/cache"},
	}
	resp, err := s.CreateContainer(ctx, mountReq(sandboxID, "app", mounts))
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if len(rt.created) != 1 {
		t.Fatalf("expected one create, got %d", len(rt.created))
	}
	got := rt.created[0].Mounts
	if len(got) != 3 {
		t.Fatalf("expected 3 runtime mounts, got %d: %+v", len(got), got)
	}
	if got[0].Source != cfgPath ||
		got[0].Target != "/etc/app" || !got[0].ReadOnly {
		t.Errorf("configmap mount = %+v", got[0])
	}
	if got[1].ReadOnly {
		t.Errorf("emptyDir mount should be writable: %+v", got[1])
	}
	if !got[2].Tmpfs || got[2].Source != "" || got[2].Target != "/cache" {
		t.Errorf("memory emptyDir should be tmpfs at /cache: %+v", got[2])
	}

	// Mounts are persisted and surface in ContainerStatus.
	st, err := s.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: resp.GetContainerId()})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	if len(st.GetStatus().GetMounts()) != 3 {
		t.Errorf("status mounts = %d, want 3", len(st.GetStatus().GetMounts()))
	}
}

func TestCreateContainerWaitsForKubeletMountSource(t *testing.T) {
	rt := newFakeRuntime()
	podsDir := t.TempDir()
	s, sandboxID := newServerWithMountRoot(t, rt, podsDir)
	ctx := context.Background()
	source := filepath.Join(podsDir, "uid-1/containers/app/termination-log")

	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.MkdirAll(filepath.Dir(source), 0o755)
		_ = os.WriteFile(source, []byte(""), 0o644)
	}()

	if _, err := s.CreateContainer(ctx, mountReq(sandboxID, "app", []*runtimeapi.Mount{
		{HostPath: source, ContainerPath: "/dev/termination-log"},
	})); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if len(rt.created) != 1 {
		t.Fatalf("expected one create, got %d", len(rt.created))
	}
}

func TestCreateContainerRejectsUnallowedHostPath(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()

	// A host path outside the kubelet pods dir with no allowlist is rejected.
	mounts := []*runtimeapi.Mount{{HostPath: "/Users/me/secrets", ContainerPath: "/data"}}
	_, err := s.CreateContainer(ctx, mountReq(sandboxID, "app", mounts))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
	// The rejected create must not have provisioned a workload.
	if len(rt.created) != 0 {
		t.Errorf("workload created despite rejected mount: %d", len(rt.created))
	}
}

func TestCreateContainerAllowsAllowlistedHostPath(t *testing.T) {
	rt := newFakeRuntime()
	s := New(Options{
		Runtime: rt,
		Mounts:  MountPolicy{HostPathAllowedPrefixes: []string{"/Users/me/data"}},
	})
	sandboxID := mustRunSandbox(t, s)
	ctx := context.Background()

	// Exact prefix and a child path are allowed; a sibling that only shares a
	// string prefix ("/Users/me/database") is not.
	cases := []struct {
		path string
		ok   bool
	}{
		{"/Users/me/data", true},
		{"/Users/me/data/sub", true},
		{"/Users/me/database", false},
	}
	for _, tc := range cases {
		req := mountReq(sandboxID, "app", []*runtimeapi.Mount{{HostPath: tc.path, ContainerPath: "/data"}})
		// Each case needs a fresh sandbox-free slate; remove any prior container.
		for _, c := range s.containers.ListBySandbox(sandboxID) {
			_, _ = s.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: c.ID})
		}
		_, err := s.CreateContainer(ctx, req)
		if tc.ok && err != nil {
			t.Errorf("path %q: unexpected error %v", tc.path, err)
		}
		if !tc.ok && status.Code(err) != codes.FailedPrecondition {
			t.Errorf("path %q: err = %v, want FailedPrecondition", tc.path, err)
		}
	}
}

func TestCreateContainerRejectsRelativeMountPaths(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()

	for _, m := range []*runtimeapi.Mount{
		{HostPath: "relative/src", ContainerPath: "/data"},
		{HostPath: "/var/lib/kubelet/pods/uid-1/x", ContainerPath: "rel/target"},
	} {
		_, err := s.CreateContainer(ctx, mountReq(sandboxID, "app", []*runtimeapi.Mount{m}))
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("mount %+v: err = %v, want InvalidArgument", m, err)
		}
	}
}

func TestCreateContainerRejectsBidirectionalPropagation(t *testing.T) {
	rt := newFakeRuntime()
	s, sandboxID := newServerWithRuntime(t, rt)
	ctx := context.Background()

	mounts := []*runtimeapi.Mount{{
		HostPath:      "/var/lib/kubelet/pods/uid-1/volumes/kubernetes.io~empty-dir/scratch",
		ContainerPath: "/scratch",
		Propagation:   runtimeapi.MountPropagation_PROPAGATION_BIDIRECTIONAL,
	}}
	_, err := s.CreateContainer(ctx, mountReq(sandboxID, "app", mounts))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

func TestCreateContainerRejectsReservedRuntimeMountTargets(t *testing.T) {
	// Every reserved target must be rejected even when its host source is an
	// otherwise-allowed kubelet pods path or an allowlisted hostPath: the guard
	// is on the container_path destination, not the source.
	rt := newFakeRuntime()
	s := New(Options{
		Runtime: rt,
		Mounts:  MountPolicy{HostPathAllowedPrefixes: []string{"/Users/me/data"}},
	})
	sandboxID := mustRunSandbox(t, s)
	ctx := context.Background()

	cases := []struct {
		name  string
		mount *runtimeapi.Mount
	}{
		{
			name:  "handoff bind point",
			mount: &runtimeapi.Mount{HostPath: "/Users/me/data", ContainerPath: "/run/macvz/handoff"},
		},
		{
			name:  "child of handoff",
			mount: &runtimeapi.Mount{HostPath: "/var/lib/kubelet/pods/uid-1/volumes/x/cfg", ContainerPath: "/run/macvz/handoff/identity"},
		},
		{
			name:  "reserved namespace root",
			mount: &runtimeapi.Mount{HostPath: "/Users/me/data", ContainerPath: "/run/macvz"},
		},
		{
			name:  "other reserved runtime path",
			mount: &runtimeapi.Mount{HostPath: "/var/lib/kubelet/pods/uid-1/volumes/x/cfg", ContainerPath: "/run/macvz/containers/c1/rootfs"},
		},
		{
			name:  "memory emptyDir at reserved target",
			mount: &runtimeapi.Mount{HostPath: "", ContainerPath: "/run/macvz/handoff"},
		},
		{
			name:  "uncleaned path resolving into reserved namespace",
			mount: &runtimeapi.Mount{HostPath: "/Users/me/data", ContainerPath: "/run/macvz/../macvz/handoff"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range s.containers.ListBySandbox(sandboxID) {
				_, _ = s.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: c.ID})
			}
			before := len(rt.created)
			_, err := s.CreateContainer(ctx, mountReq(sandboxID, "app", []*runtimeapi.Mount{tc.mount}))
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("err = %v, want FailedPrecondition", err)
			}
			if len(rt.created) != before {
				t.Errorf("workload created despite reserved target: %d", len(rt.created)-before)
			}
		})
	}

	// A non-reserved target that merely shares a string prefix with the reserved
	// namespace ("/run/macvz-data") is still allowed: matching is segment-aware.
	for _, c := range s.containers.ListBySandbox(sandboxID) {
		_, _ = s.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: c.ID})
	}
	ok := mountReq(sandboxID, "app", []*runtimeapi.Mount{
		{HostPath: "/Users/me/data", ContainerPath: "/run/macvz-data"},
	})
	if _, err := s.CreateContainer(ctx, ok); err != nil {
		t.Errorf("sibling of reserved namespace rejected: %v", err)
	}
}

func TestCreateContainerCustomKubeletPodsDir(t *testing.T) {
	rt := newFakeRuntime()
	podsDir := filepath.Join(t.TempDir(), "pods")
	s := New(Options{
		Runtime: rt,
		Mounts:  MountPolicy{KubeletPodsDir: podsDir},
	})
	sandboxID := mustRunSandbox(t, s)
	ctx := context.Background()

	// A mount under the custom pods dir is allowed; the default dir is not implied.
	okPath := filepath.Join(podsDir, "uid-1/volumes/x/cfg")
	touchMountSource(t, okPath)
	ok := mountReq(sandboxID, "app", []*runtimeapi.Mount{
		{HostPath: okPath, ContainerPath: "/etc/app"},
	})
	if _, err := s.CreateContainer(ctx, ok); err != nil {
		t.Fatalf("custom pods dir mount rejected: %v", err)
	}
	for _, c := range s.containers.ListBySandbox(sandboxID) {
		_, _ = s.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: c.ID})
	}
	bad := mountReq(sandboxID, "app", []*runtimeapi.Mount{
		{HostPath: "/var/lib/kubelet/pods/uid-1/volumes/x/cfg", ContainerPath: "/etc/app"},
	})
	if _, err := s.CreateContainer(ctx, bad); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("default pods dir should not be implied: err = %v", err)
	}
}

// mustRunSandbox starts a ready sandbox on s and returns its id.
func mustRunSandbox(t *testing.T, s *Server) string {
	t.Helper()
	resp, err := s.RunPodSandbox(context.Background(), &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	return resp.GetPodSandboxId()
}

func TestStoreMountRoundTrip(t *testing.T) {
	c := &store.Container{Mounts: []store.Mount{{HostPath: "/h", ContainerPath: "/c", ReadOnly: true}}}
	got := toCRIMounts(c.Mounts)
	if len(got) != 1 || got[0].HostPath != "/h" || got[0].ContainerPath != "/c" || !got[0].Readonly {
		t.Errorf("toCRIMounts = %+v", got)
	}
	if mountSummary(nil) != "none" {
		t.Errorf("mountSummary(nil) = %q", mountSummary(nil))
	}
	if mountSummary(c.Mounts) != "1 mount(s)" {
		t.Errorf("mountSummary = %q", mountSummary(c.Mounts))
	}
}
