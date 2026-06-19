package provider

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestResolveFieldRefSupportedPaths covers the Downward API fieldRef paths whose
// value is known at translation time (#48), including the metadata.labels /
// metadata.annotations subscript forms.
func TestResolveFieldRefSupportedPaths(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web-0",
			Namespace:   "shop",
			UID:         "abc-123",
			Labels:      map[string]string{"app": "web", "tier": "frontend"},
			Annotations: map[string]string{"team": "payments"},
		},
		Spec: corev1.PodSpec{NodeName: "mac-node-1", ServiceAccountName: "deployer"},
	}
	tests := []struct {
		path string
		want string
	}{
		{"metadata.name", "web-0"},
		{"metadata.namespace", "shop"},
		{"metadata.uid", "abc-123"},
		{"spec.nodeName", "mac-node-1"},
		{"spec.serviceAccountName", "deployer"},
		{"metadata.labels['app']", "web"},
		{`metadata.labels["tier"]`, "frontend"},
		{"metadata.annotations['team']", "payments"},
		{"metadata.labels['missing']", ""}, // absent key resolves to empty, like Kubernetes
	}
	for _, tt := range tests {
		got, err := resolveFieldRef(p, tt.path)
		if err != nil {
			t.Errorf("resolveFieldRef(%q): unexpected error %v", tt.path, err)
			continue
		}
		if got != tt.want {
			t.Errorf("resolveFieldRef(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// TestResolveFieldRefRejectsRuntimePaths verifies that status fields not known
// until after scheduling (and any unknown path) produce a clear, terminal error
// rather than an empty value.
func TestResolveFieldRefRejectsRuntimePaths(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns"}}
	for _, path := range []string{"status.podIP", "status.hostIP", "status.podIPs", "metadata.bogus", "spec.unknown"} {
		if _, err := resolveFieldRef(p, path); err == nil {
			t.Errorf("resolveFieldRef(%q): expected an error, got nil", path)
		}
	}
}

// TestResolveResourceFieldRef covers the resourceFieldRef divisor and rounding
// semantics for cpu, memory, and ephemeral-storage (#48).
func TestResolveResourceFieldRef(t *testing.T) {
	c := corev1.Container{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse("250m"),
				corev1.ResourceMemory:           resource.MustParse("64Mi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}
	tests := []struct {
		name     string
		resource string
		divisor  string
		want     string
	}{
		{"cpu limit whole", "limits.cpu", "", "2"},
		{"cpu request rounds up to a core", "requests.cpu", "", "1"},
		{"cpu request in milli divisor", "requests.cpu", "1m", "250"},
		{"memory limit bytes", "limits.memory", "", "134217728"},
		{"memory request in Mi divisor", "requests.memory", "1Mi", "64"},
		{"ephemeral request in Mi divisor", "requests.ephemeral-storage", "1Mi", "1024"},
		{"unset limit resolves to zero", "limits.ephemeral-storage", "1Mi", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := &corev1.ResourceFieldSelector{Resource: tt.resource}
			if tt.divisor != "" {
				ref.Divisor = resource.MustParse(tt.divisor)
			}
			got, err := resolveResourceFieldRef(c, ref)
			if err != nil {
				t.Fatalf("resolveResourceFieldRef: %v", err)
			}
			if got != tt.want {
				t.Errorf("resourceFieldRef %q (divisor %q) = %q, want %q", tt.resource, tt.divisor, got, tt.want)
			}
		})
	}

	if _, err := resolveResourceFieldRef(c, &corev1.ResourceFieldSelector{Resource: "limits.hugepages"}); err == nil {
		t.Error("expected an error for an unsupported resourceFieldRef")
	}
}

// TestExpandEnv covers $(VAR) substitution, the $$ escape, and the verbatim
// handling of unknown or syntactically incomplete references (#48).
func TestExpandEnv(t *testing.T) {
	vars := map[string]string{"HOST": "db", "PORT": "5432"}
	tests := []struct {
		in   string
		want string
	}{
		{"postgres://$(HOST):$(PORT)", "postgres://db:5432"},
		{"plain", "plain"},
		{"$(HOST)", "db"},
		{"$$(HOST)", "$(HOST)"},          // escaped: not expanded
		{"$$VALUE", "$VALUE"},            // $$ collapses to a single literal $
		{"$(MISSING)", "$(MISSING)"},     // unknown var left verbatim
		{"$(UNCLOSED", "$(UNCLOSED"},     // unterminated reference left verbatim
		{"price is $5", "price is $5"},   // lone $ not followed by ( is literal
		{"$(HOST)-$(MISSING)", "db-$(MISSING)"},
	}
	for _, tt := range tests {
		if got := expandEnv(tt.in, vars); got != tt.want {
			t.Errorf("expandEnv(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestResolveEnvDownwardAndExpansion exercises the Downward API sources and
// $(VAR) expansion through the real resolveEnv, including envFrom→env precedence
// and a literal value that references both an envFrom var and a fieldRef var
// resolved earlier in the list (#48).
func TestResolveEnvDownwardAndExpansion(t *testing.T) {
	getter := newFakeConfigMaps(cm("shop", "common", map[string]string{
		"REGION":   "tw",
		"LOG_LEVEL": "info", // overridden by an explicit env entry below
	}))
	p := pod("shop", "web-0", corev1.PodSpec{
		NodeName: "mac-node-1",
		Containers: []corev1.Container{{
			Name:  "web",
			Image: "x",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
			},
		}},
	})
	c := p.Spec.Containers[0]
	c.EnvFrom = []corev1.EnvFromSource{{
		ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "common"}},
	}}
	c.Env = []corev1.EnvVar{
		// Explicit env overrides the envFrom value of the same name (precedence).
		{Name: "LOG_LEVEL", Value: "debug"},
		{Name: "POD", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "NODE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		{Name: "MEM_MB", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
			Resource: "limits.memory", Divisor: resource.MustParse("1Mi"),
		}}},
		// Expansion references an envFrom var (REGION) and an earlier fieldRef (POD).
		{Name: "TAG", Value: "$(REGION)-$(POD)"},
	}

	env, err := resolveEnv(p, c, getter, nil)
	if err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	want := map[string]string{
		"REGION":    "tw",
		"LOG_LEVEL": "debug", // env wins over envFrom
		"POD":       "web-0",
		"NODE":      "mac-node-1",
		"MEM_MB":    "256",
		"TAG":       "tw-web-0",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, env[k], v)
		}
	}
}

// TestResolveEnvUnsupportedFieldRefErrors confirms an unsupported fieldRef path
// surfaces a clear error through resolveEnv (terminal, not a silent empty value).
func TestResolveEnvUnsupportedFieldRefErrors(t *testing.T) {
	c := corev1.Container{Name: "web", Image: "x", Env: []corev1.EnvVar{
		{Name: "IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
	}}
	_, err := resolveEnv(pod("ns", "p", oneContainer(c)), c, nil, nil)
	if err == nil {
		t.Fatal("expected an error for status.podIP fieldRef")
	}
	if errors.Is(err, errConfigPending) || errors.Is(err, errSecretUnavailable) {
		t.Errorf("status.podIP should be terminal, not a retryable pending error: %v", err)
	}
}

// TestSubscriptKey checks the metadata.labels['key'] / metadata.annotations["key"]
// parsing used by fieldRef resolution.
func TestSubscriptKey(t *testing.T) {
	tests := []struct {
		path   string
		prefix string
		want   string
		ok     bool
	}{
		{"metadata.labels['app']", "metadata.labels", "app", true},
		{`metadata.labels["app"]`, "metadata.labels", "app", true},
		{"metadata.annotations['a.b/c']", "metadata.annotations", "a.b/c", true},
		{"metadata.labels", "metadata.labels", "", false},
		{"metadata.labels[app]", "metadata.labels", "", false}, // unquoted: not matched
		{"metadata.name", "metadata.labels", "", false},
	}
	for _, tt := range tests {
		got, ok := subscriptKey(tt.path, tt.prefix)
		if ok != tt.ok || got != tt.want {
			t.Errorf("subscriptKey(%q, %q) = (%q, %v), want (%q, %v)", tt.path, tt.prefix, got, ok, tt.want, tt.ok)
		}
	}
}
