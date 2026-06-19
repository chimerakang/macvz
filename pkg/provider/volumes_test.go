package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func volPod(uid string, volumes []corev1.Volume, mounts []corev1.VolumeMount) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: types.UID(uid)},
		Spec: corev1.PodSpec{
			Volumes:    volumes,
			Containers: []corev1.Container{{Name: "c", Image: "img", VolumeMounts: mounts}},
		},
	}
}

func hostPathPtr(t corev1.HostPathType) *corev1.HostPathType { return &t }

func TestResolveEmptyDirMount(t *testing.T) {
	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		[]corev1.VolumeMount{{Name: "scratch", MountPath: "/data"}},
	)
	rv, err := resolveVolumes(pod, VolumePolicy{Root: "/var/lib/macvz/volumes"})
	if err != nil {
		t.Fatalf("resolveVolumes: %v", err)
	}
	if len(rv.mounts) != 1 {
		t.Fatalf("mounts = %d, want 1", len(rv.mounts))
	}
	m := rv.mounts[0]
	want := "/var/lib/macvz/volumes/uid-1/scratch"
	if m.Source != want || m.Target != "/data" || m.Tmpfs {
		t.Errorf("mount = %+v, want source %q target /data bind", m, want)
	}
	if len(rv.ephemeralDirs) != 1 || rv.ephemeralDirs[0] != want {
		t.Errorf("ephemeralDirs = %v, want [%s]", rv.ephemeralDirs, want)
	}
}

func TestResolveEmptyDirMemoryIsTmpfs(t *testing.T) {
	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}},
		[]corev1.VolumeMount{{Name: "cache", MountPath: "/cache"}},
	)
	rv, err := resolveVolumes(pod, VolumePolicy{Root: "/var/lib/macvz/volumes"})
	if err != nil {
		t.Fatalf("resolveVolumes: %v", err)
	}
	if len(rv.mounts) != 1 || !rv.mounts[0].Tmpfs || rv.mounts[0].Source != "" {
		t.Errorf("memory emptyDir should be tmpfs with no source; got %+v", rv.mounts)
	}
	if len(rv.ephemeralDirs) != 0 {
		t.Errorf("tmpfs needs no host dir; got %v", rv.ephemeralDirs)
	}
}

func TestResolveEmptyDirNeedsRoot(t *testing.T) {
	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		nil,
	)
	if _, err := resolveVolumes(pod, VolumePolicy{}); err == nil || !strings.Contains(err.Error(), "node.volumes.root") {
		t.Errorf("expected emptyDir-without-root error, got %v", err)
	}
}

func TestResolveEmptyDirRejectsRelativeRoot(t *testing.T) {
	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		[]corev1.VolumeMount{{Name: "scratch", MountPath: "/data"}},
	)
	if _, err := resolveVolumes(pod, VolumePolicy{Root: "relative"}); err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Errorf("expected relative-root rejection, got %v", err)
	}
}

func TestResolveEmptyDirRejectsMissingPodUID(t *testing.T) {
	pod := volPod("",
		[]corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		[]corev1.VolumeMount{{Name: "scratch", MountPath: "/data"}},
	)
	if _, err := resolveVolumes(pod, VolumePolicy{Root: "/var/lib/macvz/volumes"}); err == nil || !strings.Contains(err.Error(), "Pod UID") {
		t.Errorf("expected missing-UID rejection, got %v", err)
	}
}

func TestResolveEmptyDirRejectsPodUIDEscape(t *testing.T) {
	pod := volPod("..",
		[]corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		[]corev1.VolumeMount{{Name: "scratch", MountPath: "/data"}},
	)
	if _, err := resolveVolumes(pod, VolumePolicy{Root: "/var/lib/macvz/volumes"}); err == nil || !strings.Contains(err.Error(), "escapes volume root") {
		t.Errorf("expected UID escape rejection, got %v", err)
	}
}

func TestResolveHostPathDisabledByDefault(t *testing.T) {
	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/data"}}}},
		[]corev1.VolumeMount{{Name: "h", MountPath: "/data"}},
	)
	_, err := resolveVolumes(pod, VolumePolicy{})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("hostPath should be disabled by default, got %v", err)
	}
}

func TestResolveHostPathAllowlist(t *testing.T) {
	policy := VolumePolicy{HostPathAllowedPrefixes: []string{"/srv"}}

	// Within the allowed prefix: admitted, read-only honored.
	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/data"}}}},
		[]corev1.VolumeMount{{Name: "h", MountPath: "/data", ReadOnly: true}},
	)
	rv, err := resolveVolumes(pod, policy)
	if err != nil {
		t.Fatalf("resolveVolumes: %v", err)
	}
	if m := rv.mounts[0]; m.Source != "/srv/data" || !m.ReadOnly {
		t.Errorf("mount = %+v, want /srv/data read-only", m)
	}

	// Outside the allowed prefix: rejected. "/srvother" must not match "/srv".
	bad := volPod("uid-1",
		[]corev1.Volume{{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srvother/data"}}}},
		[]corev1.VolumeMount{{Name: "h", MountPath: "/data"}},
	)
	if _, err := resolveVolumes(bad, policy); err == nil || !strings.Contains(err.Error(), "not within an allowed prefix") {
		t.Errorf("expected prefix rejection for /srvother, got %v", err)
	}
}

func TestResolveHostPathRejectsTraversalEscape(t *testing.T) {
	policy := VolumePolicy{HostPathAllowedPrefixes: []string{"/srv/app"}}
	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/app/../../etc"}}}},
		[]corev1.VolumeMount{{Name: "h", MountPath: "/data"}},
	)
	if _, err := resolveVolumes(pod, policy); err == nil {
		t.Error("path escaping the allowed prefix via .. should be rejected")
	}
}

func TestResolveHostPathRejectsNonDirectoryType(t *testing.T) {
	policy := VolumePolicy{HostPathAllowedPrefixes: []string{"/srv"}}
	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/sock", Type: hostPathPtr(corev1.HostPathSocket)}}}},
		[]corev1.VolumeMount{{Name: "h", MountPath: "/data"}},
	)
	if _, err := resolveVolumes(pod, policy); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("socket hostPath type should be rejected, got %v", err)
	}
}

func TestResolveRejectsSubPath(t *testing.T) {
	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		[]corev1.VolumeMount{{Name: "scratch", MountPath: "/data", SubPath: "sub"}},
	)
	if _, err := resolveVolumes(pod, VolumePolicy{Root: "/var/lib/macvz/volumes"}); err == nil || !strings.Contains(err.Error(), "subPath") {
		t.Errorf("subPath should be rejected, got %v", err)
	}
}

func TestResolveRejectsUnsupportedSource(t *testing.T) {
	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "cm", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}}},
		nil,
	)
	if _, err := resolveVolumes(pod, VolumePolicy{Root: "/x"}); err == nil || !strings.Contains(err.Error(), "unsupported source type") {
		t.Errorf("configMap should be rejected as unsupported, got %v", err)
	}
}

func TestResolveRejectsRelativeMountPath(t *testing.T) {
	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		[]corev1.VolumeMount{{Name: "scratch", MountPath: "rel/path"}},
	)
	if _, err := resolveVolumes(pod, VolumePolicy{Root: "/x"}); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Errorf("relative mountPath should be rejected, got %v", err)
	}
}

func TestCreatePodMaterializesEmptyDirAndDeleteCleansUp(t *testing.T) {
	root := t.TempDir()
	p := New("mac-1", newRecordingRuntime(), WithVolumePolicy(VolumePolicy{Root: root}))

	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		[]corev1.VolumeMount{{Name: "scratch", MountPath: "/data"}},
	)
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	dir := filepath.Join(root, "uid-1", "scratch")
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("expected emptyDir backing dir %q to exist: %v", dir, err)
	}

	if err := p.DeletePod(context.Background(), pod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "uid-1")); !os.IsNotExist(err) {
		t.Errorf("expected Pod volume root to be removed on delete, stat err = %v", err)
	}
}

func TestCleanupVolumeDirsRefusesMissingPodUID(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "keep")
	if err := os.MkdirAll(keep, 0o770); err != nil {
		t.Fatalf("mkdir keep: %v", err)
	}
	p := New("mac-1", newRecordingRuntime(), WithVolumePolicy(VolumePolicy{Root: root}))

	p.cleanupVolumeDirs(volPod("", nil, nil))

	if fi, err := os.Stat(keep); err != nil || !fi.IsDir() {
		t.Fatalf("cleanup with empty UID removed unrelated root content: %v", err)
	}
}

func TestCreatePodUnsupportedVolumeIsTerminal(t *testing.T) {
	p := New("mac-1", newRecordingRuntime(), WithVolumePolicy(VolumePolicy{Root: t.TempDir()}))
	pod := volPod("uid-2",
		[]corev1.Volume{{Name: "cm", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}}},
		nil,
	)
	// CreatePod returns nil (terminal) but records a Failed status.
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod should record terminal failure, not error: %v", err)
	}
	st, err := p.GetPodStatus(context.Background(), "ns", "p")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if st.Phase != corev1.PodFailed || !strings.Contains(st.Message, "unsupported source type") {
		t.Errorf("status = %s/%q, want Failed mentioning unsupported source type", st.Phase, st.Message)
	}
}

func TestResolveToleratesServiceAccountTokenMount(t *testing.T) {
	pod := volPod("uid-1",
		[]corev1.Volume{{Name: "kube-api-access-x", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{}}},
		}}}},
		[]corev1.VolumeMount{{Name: "kube-api-access-x", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount"}},
	)
	rv, err := resolveVolumes(pod, VolumePolicy{Root: "/x"})
	if err != nil {
		t.Fatalf("SA token should be tolerated: %v", err)
	}
	if len(rv.mounts) != 0 {
		t.Errorf("SA token must not produce a MacVz mount; got %v", rv.mounts)
	}
}
