package provider

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pod(ns, name string, spec corev1.PodSpec) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       spec,
	}
}

func oneContainer(c corev1.Container) corev1.PodSpec {
	return corev1.PodSpec{Containers: []corev1.Container{c}}
}

func TestTranslatePodSupported(t *testing.T) {
	// Mirror `kubectl run alpine --image=alpine -- sleep 3600`.
	p := pod("default", "alpine", oneContainer(corev1.Container{
		Name:    "alpine",
		Image:   "alpine",
		Command: []string{"sleep"},
		Args:    []string{"3600"},
	}))
	spec, err := translatePod(p)
	if err != nil {
		t.Fatalf("translatePod: %v", err)
	}
	if spec.Image != "alpine" {
		t.Errorf("image = %q, want alpine", spec.Image)
	}
	if strings.Join(spec.Command, " ") != "sleep" || strings.Join(spec.Args, " ") != "3600" {
		t.Errorf("command/args = %v / %v, want [sleep] / [3600]", spec.Command, spec.Args)
	}
	if spec.Name != "macvz-default-alpine-alpine" {
		t.Errorf("workload ID = %q, want macvz-default-alpine-alpine", spec.Name)
	}
}

func TestTranslateContainerFields(t *testing.T) {
	c := corev1.Container{
		Name:  "app",
		Image: "nginx:1.27",
		Env: []corev1.EnvVar{
			{Name: "FOO", Value: "bar"},
			{Name: "BAZ", Value: "qux"},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}
	spec := translateContainer(pod("ns", "n", oneContainer(c)), c)

	if spec.Env["FOO"] != "bar" || spec.Env["BAZ"] != "qux" {
		t.Errorf("env not translated: %v", spec.Env)
	}
	// Limits win over requests.
	if spec.CPUMillis != 500 {
		t.Errorf("CPUMillis = %d, want 500 (limit)", spec.CPUMillis)
	}
	if spec.MemoryBytes != 128*1024*1024 {
		t.Errorf("MemoryBytes = %d, want %d (limit)", spec.MemoryBytes, 128*1024*1024)
	}
}

func TestTranslateContainerFallsBackToRequests(t *testing.T) {
	c := corev1.Container{
		Name:  "app",
		Image: "x",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
		},
	}
	spec := translateContainer(pod("ns", "n", oneContainer(c)), c)
	if spec.CPUMillis != 1000 {
		t.Errorf("CPUMillis = %d, want 1000 (request)", spec.CPUMillis)
	}
	if spec.MemoryBytes != 32*1024*1024 {
		t.Errorf("MemoryBytes = %d, want %d (request)", spec.MemoryBytes, 32*1024*1024)
	}
}

func TestTranslatePodUnsupported(t *testing.T) {
	tests := []struct {
		name    string
		spec    corev1.PodSpec
		wantSub string
	}{
		{
			name:    "no containers",
			spec:    corev1.PodSpec{},
			wantSub: "no containers",
		},
		{
			name: "multi-container",
			spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "a", Image: "x"}, {Name: "b", Image: "y"},
			}},
			wantSub: "multi-container",
		},
		{
			name: "init container",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "init", Image: "x"}},
				Containers:     []corev1.Container{{Name: "a", Image: "x"}},
			},
			wantSub: "init containers",
		},
		{
			name: "user volume",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "a", Image: "x"}},
				Volumes:    []corev1.Volume{{Name: "data"}},
			},
			wantSub: `volume "data"`,
		},
		{
			name: "container securityContext",
			spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "a", Image: "x", SecurityContext: &corev1.SecurityContext{}},
			}},
			wantSub: "securityContext",
		},
		{
			name: "env valueFrom",
			spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:  "a",
				Image: "x",
				Env:   []corev1.EnvVar{{Name: "K", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}},
			}}},
			wantSub: "valueFrom",
		},
		{
			name: "hostNetwork",
			spec: corev1.PodSpec{
				HostNetwork: true,
				Containers:  []corev1.Container{{Name: "a", Image: "x"}},
			},
			wantSub: "hostNetwork",
		},
		{
			name: "restart policy always",
			spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyAlways,
				Containers:    []corev1.Container{{Name: "a", Image: "x"}},
			},
			wantSub: "restartPolicy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := translatePod(pod("ns", "n", tt.spec))
			if err == nil {
				t.Fatal("expected an unsupported error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantSub)
			}
		})
	}
}

func TestTranslatePodToleratesServiceAccountToken(t *testing.T) {
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{Name: "a", Image: "x"}},
		Volumes: []corev1.Volume{{
			Name: "kube-api-access-abcde",
			VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{}}},
			}},
		}},
	}
	if _, err := translatePod(pod("ns", "n", spec)); err != nil {
		t.Errorf("auto-mounted service-account token should be tolerated, got %v", err)
	}
}

func TestWorkloadIDIsStableAndDNSSafe(t *testing.T) {
	id1 := workloadID("default", "alpine", "alpine")
	id2 := workloadID("default", "alpine", "alpine")
	if id1 != id2 {
		t.Errorf("workloadID not stable: %q != %q", id1, id2)
	}
	if !isDNSLabel(id1) {
		t.Errorf("workloadID %q is not a valid DNS label", id1)
	}

	// Uppercase and illegal characters are sanitized.
	id := workloadID("Team_A", "My.Pod", "Web/Server")
	if !isDNSLabel(id) {
		t.Errorf("sanitized workloadID %q is not a valid DNS label", id)
	}
}

func TestWorkloadIDTruncatesLongNames(t *testing.T) {
	long := strings.Repeat("x", 100)
	id := workloadID("ns", long, "container")
	if len(id) > maxWorkloadIDLen {
		t.Errorf("workloadID length = %d, want <= %d", len(id), maxWorkloadIDLen)
	}
	if !isDNSLabel(id) {
		t.Errorf("truncated workloadID %q is not a valid DNS label", id)
	}
	// Still stable after truncation.
	if id != workloadID("ns", long, "container") {
		t.Error("truncated workloadID is not stable")
	}
	// Distinct inputs produce distinct IDs even when truncated.
	other := workloadID("ns", long+"y", "container")
	if id == other {
		t.Error("truncated workloadIDs collide for distinct inputs")
	}
}

// isDNSLabel reports whether s is a valid RFC 1123 DNS label: 1-63 chars,
// lowercase alphanumeric or '-', starting and ending alphanumeric.
func isDNSLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if i == 0 || i == len(s)-1 {
			if !alnum {
				return false
			}
			continue
		}
		if !alnum && r != '-' {
			return false
		}
	}
	return true
}
