package criserver

import (
	"strings"
	"testing"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func sandboxConfigWithNS(network, pid, ipc runtimeapi.NamespaceMode) *runtimeapi.PodSandboxConfig {
	return &runtimeapi.PodSandboxConfig{
		Linux: &runtimeapi.LinuxPodSandboxConfig{
			SecurityContext: &runtimeapi.LinuxSandboxSecurityContext{
				NamespaceOptions: &runtimeapi.NamespaceOption{
					Network: network,
					Pid:     pid,
					Ipc:     ipc,
				},
			},
		},
	}
}

func TestUnsupportedSandboxShape(t *testing.T) {
	pod := runtimeapi.NamespaceMode_POD
	node := runtimeapi.NamespaceMode_NODE

	tests := []struct {
		name      string
		cfg       *runtimeapi.PodSandboxConfig
		wantBad   bool
		wantWords []string
	}{
		{
			name:    "nil config",
			cfg:     nil,
			wantBad: false,
		},
		{
			name:    "no linux section",
			cfg:     &runtimeapi.PodSandboxConfig{},
			wantBad: false,
		},
		{
			name:    "default pod namespaces",
			cfg:     sandboxConfigWithNS(pod, pod, pod),
			wantBad: false,
		},
		{
			name:      "host network",
			cfg:       sandboxConfigWithNS(node, pod, pod),
			wantBad:   true,
			wantWords: []string{"hostNetwork"},
		},
		{
			name:      "host pid",
			cfg:       sandboxConfigWithNS(pod, node, pod),
			wantBad:   true,
			wantWords: []string{"hostPID"},
		},
		{
			name:      "host ipc",
			cfg:       sandboxConfigWithNS(pod, pod, node),
			wantBad:   true,
			wantWords: []string{"hostIPC"},
		},
		{
			name:      "all host namespaces report every reason",
			cfg:       sandboxConfigWithNS(node, node, node),
			wantBad:   true,
			wantWords: []string{"hostNetwork", "hostPID", "hostIPC"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, bad := unsupportedSandboxShape(tt.cfg)
			if bad != tt.wantBad {
				t.Fatalf("unsupportedSandboxShape() bad = %v, want %v (reason %q)", bad, tt.wantBad, reason)
			}
			if !bad {
				if reason != "" {
					t.Fatalf("expected empty reason for supported shape, got %q", reason)
				}
				return
			}
			for _, w := range tt.wantWords {
				if !strings.Contains(reason, w) {
					t.Errorf("reason %q does not mention %q", reason, w)
				}
			}
		})
	}
}
