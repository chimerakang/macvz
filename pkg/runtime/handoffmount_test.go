package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/chimerakang/macvz/internal/types"
)

func testLayout() HandoffLayout {
	l, err := NewHandoffManager("/run/macvz/containers").Layout("macvz-cri-deadbeef")
	if err != nil {
		panic(err)
	}
	return l
}

func TestHandoffMountShape(t *testing.T) {
	layout := testLayout()
	m := HandoffMount(layout)

	// The MacVz realization of the R16 OCI bind mount {type:bind, options:
	// [rbind,rw]}: writable VirtioFS share from the handoff dir to HandoffMountPoint.
	if m.Source != layout.HandoffDir {
		t.Errorf("Source = %q, want %q", m.Source, layout.HandoffDir)
	}
	if m.Target != HandoffMountPoint {
		t.Errorf("Target = %q, want %q", m.Target, HandoffMountPoint)
	}
	if m.ReadOnly {
		t.Error("handoff mount must be writable (ReadOnly=false) so the late process can write evidence")
	}
	if m.Tmpfs {
		t.Error("handoff mount must be a host bind mount, not a tmpfs")
	}
}

func TestHandoffIdentityMountShape(t *testing.T) {
	layout := testLayout()
	m, err := HandoffIdentityMount(layout)
	if err != nil {
		t.Fatalf("HandoffIdentityMount: %v", err)
	}
	wantSource := filepath.Join(layout.RootfsDir, "etc", "macvz-container-identity")
	if m.Source != wantSource {
		t.Errorf("Source = %q, want %q", m.Source, wantSource)
	}
	if m.Target != RootfsIdentityPath {
		t.Errorf("Target = %q, want %q", m.Target, RootfsIdentityPath)
	}
	if !m.ReadOnly {
		t.Error("staged identity mount must be read-only")
	}
	if m.Tmpfs {
		t.Error("staged identity mount must be a host bind mount, not a tmpfs")
	}
}

func TestInjectHandoffMountAppends(t *testing.T) {
	layout := testLayout()
	spec := &types.ContainerSpec{
		Name:  "w",
		Image: "alpine",
		Mounts: []types.Mount{
			{Source: "/var/lib/kubelet/pods/x/vol", Target: "/data"},
		},
	}
	if err := InjectHandoffMount(spec, layout); err != nil {
		t.Fatalf("InjectHandoffMount: %v", err)
	}
	if len(spec.Mounts) != 3 {
		t.Fatalf("Mounts len = %d, want 3", len(spec.Mounts))
	}
	if got := spec.Mounts[1]; got != HandoffMount(layout) {
		t.Errorf("handoff mount = %+v, want %+v", got, HandoffMount(layout))
	}
	wantIdentity, err := HandoffIdentityMount(layout)
	if err != nil {
		t.Fatalf("HandoffIdentityMount: %v", err)
	}
	if got := spec.Mounts[2]; got != wantIdentity {
		t.Errorf("identity mount = %+v, want %+v", got, wantIdentity)
	}
	// The original mount is preserved and untouched.
	if spec.Mounts[0].Target != "/data" {
		t.Errorf("existing mount clobbered: %+v", spec.Mounts[0])
	}
}

func TestInjectHandoffMountIdempotent(t *testing.T) {
	layout := testLayout()
	spec := &types.ContainerSpec{Name: "w", Image: "alpine"}
	for i := 0; i < 3; i++ {
		if err := InjectHandoffMount(spec, layout); err != nil {
			t.Fatalf("InjectHandoffMount call %d: %v", i, err)
		}
	}
	if len(spec.Mounts) != 2 {
		t.Fatalf("repeated injection produced %d mounts, want 2 (idempotent)", len(spec.Mounts))
	}
	if !hasMount(spec.Mounts, HandoffMount(layout)) {
		t.Errorf("missing handoff mount: %+v", spec.Mounts)
	}
	wantIdentity, err := HandoffIdentityMount(layout)
	if err != nil {
		t.Fatalf("HandoffIdentityMount: %v", err)
	}
	if !hasMount(spec.Mounts, wantIdentity) {
		t.Errorf("missing identity mount: %+v", spec.Mounts)
	}
}

func TestInjectHandoffMountRejectsReservedTargetConflict(t *testing.T) {
	layout := testLayout()
	cases := map[string]string{
		"exact mountpoint": HandoffMountPoint,
		"under namespace":  HandoffRuntimeRoot + "/something",
		"namespace root":   HandoffRuntimeRoot,
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			spec := &types.ContainerSpec{
				Name:   "w",
				Image:  "alpine",
				Mounts: []types.Mount{{Source: "/evil", Target: target}},
			}
			err := InjectHandoffMount(spec, layout)
			if !errors.Is(err, ErrHandoffMountConflict) {
				t.Fatalf("error = %v, want ErrHandoffMountConflict", err)
			}
			// On conflict the spec is left unchanged (no handoff mount appended).
			if len(spec.Mounts) != 1 {
				t.Errorf("Mounts len = %d, want 1 (unchanged on conflict)", len(spec.Mounts))
			}
		})
	}
}

func TestInjectHandoffMountRejectsIdentityTargetConflict(t *testing.T) {
	layout := testLayout()
	spec := &types.ContainerSpec{
		Name:   "w",
		Image:  "alpine",
		Mounts: []types.Mount{{Source: "/user/identity", Target: RootfsIdentityPath}},
	}
	err := InjectHandoffMount(spec, layout)
	if !errors.Is(err, ErrHandoffMountConflict) {
		t.Fatalf("error = %v, want ErrHandoffMountConflict", err)
	}
	if len(spec.Mounts) != 1 {
		t.Errorf("Mounts len = %d, want 1 (unchanged on conflict)", len(spec.Mounts))
	}
}

func TestInjectHandoffMountAllowsSiblingNamespace(t *testing.T) {
	layout := testLayout()
	// /run/macvz-data is a sibling, not inside /run/macvz; it must be allowed.
	spec := &types.ContainerSpec{
		Name:   "w",
		Image:  "alpine",
		Mounts: []types.Mount{{Source: "/host/data", Target: "/run/macvz-data"}},
	}
	if err := InjectHandoffMount(spec, layout); err != nil {
		t.Fatalf("InjectHandoffMount with sibling namespace: %v", err)
	}
	if len(spec.Mounts) != 3 {
		t.Errorf("Mounts len = %d, want 3", len(spec.Mounts))
	}
}

func TestInjectHandoffMountRejectsBadInput(t *testing.T) {
	layout := testLayout()
	if err := InjectHandoffMount(nil, layout); err == nil {
		t.Error("nil spec should error")
	}
	spec := &types.ContainerSpec{Name: "w", Image: "alpine"}
	if err := InjectHandoffMount(spec, HandoffLayout{}); err == nil {
		t.Error("empty layout should error")
	}
	if _, err := HandoffIdentityMount(HandoffLayout{}); err == nil {
		t.Error("empty layout should error for identity mount")
	}
	if err := InjectHandoffMount(spec, HandoffLayout{HandoffDir: "/h", MountPoint: "run/macvz/handoff"}); err == nil {
		t.Error("relative mount point should error")
	}
	if err := InjectHandoffMount(spec, HandoffLayout{HandoffDir: "/h", MountPoint: "/tmp/handoff"}); err == nil {
		t.Error("mount point outside reserved namespace should error")
	}
}

func TestPrepareHandoffMountpointCreatesDestination(t *testing.T) {
	layout, err := NewHandoffManager(t.TempDir()).Create("macvz-cri-mp")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dest, err := PrepareHandoffMountpoint(layout)
	if err != nil {
		t.Fatalf("PrepareHandoffMountpoint: %v", err)
	}
	want := filepath.Join(layout.RootfsDir, "run", "macvz", "handoff")
	if dest != want {
		t.Errorf("dest = %q, want %q", dest, want)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat dest: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("dest is not a directory")
	}
	// Idempotent.
	if _, err := PrepareHandoffMountpoint(layout); err != nil {
		t.Errorf("second PrepareHandoffMountpoint: %v", err)
	}
}

func TestPrepareHandoffMountpointReportsFailure(t *testing.T) {
	layout, err := NewHandoffManager(t.TempDir()).Create("macvz-cri-blocked")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Block the destination by planting a regular file where "run" must be a dir,
	// so MkdirAll fails and the error names the offending path.
	blocker := filepath.Join(layout.RootfsDir, "run")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareHandoffMountpoint(layout); err == nil {
		t.Fatal("expected error when destination cannot be prepared")
	}
}

func TestPrepareHandoffMountpointRejectsEmptyLayout(t *testing.T) {
	if _, err := PrepareHandoffMountpoint(HandoffLayout{}); err == nil {
		t.Error("empty layout should error")
	}
}

func TestPrepareHandoffMountpointRejectsBadMountPoint(t *testing.T) {
	layout, err := NewHandoffManager(t.TempDir()).Create("macvz-cri-bad-mp")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, mp := range []string{"run/macvz/handoff", "/tmp/handoff"} {
		t.Run(mp, func(t *testing.T) {
			layout.MountPoint = mp
			if _, err := PrepareHandoffMountpoint(layout); err == nil {
				t.Fatalf("PrepareHandoffMountpoint(%q) unexpectedly succeeded", mp)
			}
		})
	}
}

func TestRootfsGuestPathCleansInsideRootfs(t *testing.T) {
	rootfs := t.TempDir()
	got, err := rootfsGuestPath(rootfs, "/run/macvz/../macvz/handoff")
	if err != nil {
		t.Fatalf("rootfsGuestPath: %v", err)
	}
	want := filepath.Join(rootfs, "run", "macvz", "handoff")
	if got != want {
		t.Errorf("rootfsGuestPath = %q, want %q", got, want)
	}

	got, err = rootfsGuestPath(rootfs, "/../../etc/macvz-container-identity")
	if err != nil {
		t.Fatalf("rootfsGuestPath with parent refs: %v", err)
	}
	want = filepath.Join(rootfs, "etc", "macvz-container-identity")
	if got != want {
		t.Errorf("rootfsGuestPath escaped rootfs: got %q, want %q", got, want)
	}
}

func hasMount(mounts []types.Mount, want types.Mount) bool {
	for _, m := range mounts {
		if m == want {
			return true
		}
	}
	return false
}
