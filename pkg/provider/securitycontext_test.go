package provider

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func int64Ptr(v int64) *int64 { return &v }

func scPod(podSC *corev1.PodSecurityContext, cSC *corev1.SecurityContext) *corev1.Pod {
	c := corev1.Container{Name: "app", Image: "x", SecurityContext: cSC}
	spec := corev1.PodSpec{Containers: []corev1.Container{c}, SecurityContext: podSC}
	return pod("default", "p", spec)
}

// --- Supported fields are mapped onto the runtime spec. ---

func TestSecurityContextMapsUserGroup(t *testing.T) {
	spec, _, err := translatePod(t.Context(), scPod(nil, &corev1.SecurityContext{
		RunAsUser:  int64Ptr(1000),
		RunAsGroup: int64Ptr(2000),
	}), VolumePolicy{}, DNSConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("translatePod: %v", err)
	}
	if spec.User != "1000:2000" {
		t.Errorf("User = %q, want 1000:2000", spec.User)
	}
}

func TestSecurityContextUserFromPodLevelAndContainerOverride(t *testing.T) {
	// Container runAsUser overrides the pod-level value; the group falls back to
	// the pod level when the container does not set it.
	spec, _, err := translatePod(t.Context(), scPod(
		&corev1.PodSecurityContext{RunAsUser: int64Ptr(1), RunAsGroup: int64Ptr(9)},
		&corev1.SecurityContext{RunAsUser: int64Ptr(1000)},
	), VolumePolicy{}, DNSConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("translatePod: %v", err)
	}
	if spec.User != "1000:9" {
		t.Errorf("User = %q, want 1000:9 (container uid, pod gid)", spec.User)
	}
}

func TestSecurityContextMapsReadOnlyRootAndCapabilities(t *testing.T) {
	spec, _, err := translatePod(t.Context(), scPod(nil, &corev1.SecurityContext{
		ReadOnlyRootFilesystem: optionalRef(true),
		Capabilities: &corev1.Capabilities{
			Add:  []corev1.Capability{"NET_ADMIN"},
			Drop: []corev1.Capability{"ALL"},
		},
	}), VolumePolicy{}, DNSConfig{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("translatePod: %v", err)
	}
	if !spec.ReadOnlyRootFS {
		t.Error("ReadOnlyRootFS = false, want true")
	}
	if len(spec.CapAdd) != 1 || spec.CapAdd[0] != "CAP_NET_ADMIN" {
		t.Errorf("CapAdd = %v, want [CAP_NET_ADMIN]", spec.CapAdd)
	}
	if len(spec.CapDrop) != 1 || spec.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", spec.CapDrop)
	}
}

// --- Accepted-as-no-op fields do not block the Pod. ---

func TestSecurityContextAcceptsHardeningNoOps(t *testing.T) {
	pod := scPod(
		&corev1.PodSecurityContext{
			FSGroup:            int64Ptr(3000),
			SupplementalGroups: []int64{4000},
			RunAsNonRoot:       optionalRef(true),
		},
		&corev1.SecurityContext{
			AllowPrivilegeEscalation: optionalRef(false),
			Privileged:               optionalRef(false),
			RunAsNonRoot:             optionalRef(true),
			RunAsUser:                int64Ptr(1000),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			AppArmorProfile:          &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeRuntimeDefault},
		},
	)
	if reasons := securityContextReasons(pod); len(reasons) != 0 {
		t.Errorf("expected no rejections for hardening no-ops, got %v", reasons)
	}
}

// --- Unsupported fields are rejected with clear, field-specific reasons. ---

func TestSecurityContextRejections(t *testing.T) {
	tests := []struct {
		name    string
		podSC   *corev1.PodSecurityContext
		cSC     *corev1.SecurityContext
		wantSub string
	}{
		{
			name:    "privileged",
			cSC:     &corev1.SecurityContext{Privileged: optionalRef(true)},
			wantSub: "privileged",
		},
		{
			name:    "seLinuxOptions",
			cSC:     &corev1.SecurityContext{SELinuxOptions: &corev1.SELinuxOptions{Level: "s0"}},
			wantSub: "seLinuxOptions",
		},
		{
			name:    "windowsOptions",
			cSC:     &corev1.SecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{}},
			wantSub: "windowsOptions",
		},
		{
			name:    "procMount",
			cSC:     &corev1.SecurityContext{ProcMount: procMountPtr(corev1.UnmaskedProcMount)},
			wantSub: "procMount",
		},
		{
			name:    "localhost seccomp",
			cSC:     &corev1.SecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeLocalhost}},
			wantSub: "seccompProfile",
		},
		{
			name:    "localhost appArmor",
			cSC:     &corev1.SecurityContext{AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeLocalhost}},
			wantSub: "appArmorProfile",
		},
		{
			name:    "pod sysctls",
			podSC:   &corev1.PodSecurityContext{Sysctls: []corev1.Sysctl{{Name: "net.core.somaxconn", Value: "1024"}}},
			wantSub: "sysctls",
		},
		{
			name:    "runAsNonRoot with uid 0",
			cSC:     &corev1.SecurityContext{RunAsNonRoot: optionalRef(true), RunAsUser: int64Ptr(0)},
			wantSub: "runAsNonRoot",
		},
		{
			name:    "runAsGroup without runAsUser",
			cSC:     &corev1.SecurityContext{RunAsGroup: int64Ptr(2000)},
			wantSub: "runAsGroup",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reasons := securityContextReasons(scPod(tt.podSC, tt.cSC))
			if len(reasons) == 0 {
				t.Fatalf("expected a rejection mentioning %q, got none", tt.wantSub)
			}
			joined := strings.Join(reasons, "; ")
			if !strings.Contains(joined, tt.wantSub) {
				t.Errorf("reasons %q do not mention %q", joined, tt.wantSub)
			}
		})
	}
}

// TestCreatePodRejectsPrivilegedTerminally confirms an unsupported securityContext
// surfaces as a terminal Failed status (a clear surprise-free signal), not a silent
// run.
func TestCreatePodRejectsPrivilegedTerminally(t *testing.T) {
	p := New("mac-1", newRecordingRuntime())
	pod := scPod(nil, &corev1.SecurityContext{Privileged: optionalRef(true)})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("CreatePod should record terminal failure, not error: %v", err)
	}
	st, err := p.GetPodStatus(t.Context(), "default", "p")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if st.Phase != corev1.PodFailed || !strings.Contains(st.Message, "privileged") {
		t.Errorf("status = %s/%q, want Failed mentioning privileged", st.Phase, st.Message)
	}
}

func procMountPtr(t corev1.ProcMountType) *corev1.ProcMountType { return &t }
