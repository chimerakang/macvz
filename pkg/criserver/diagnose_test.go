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
		{
			// kubelet sends a PortMapping for every declared containerPort with
			// HostPort left zero; that is not a host-port request and must pass.
			name: "containerPort-only port mappings are supported",
			cfg: &runtimeapi.PodSandboxConfig{
				PortMappings: []*runtimeapi.PortMapping{
					{ContainerPort: 8080},
					{ContainerPort: 9090, Protocol: runtimeapi.Protocol_UDP},
				},
			},
			wantBad: false,
		},
		{
			name: "hostPort request is rejected loudly",
			cfg: &runtimeapi.PodSandboxConfig{
				PortMappings: []*runtimeapi.PortMapping{
					{ContainerPort: 8080},
					{ContainerPort: 8080, HostPort: 30080},
				},
			},
			wantBad:   true,
			wantWords: []string{"hostPort", "1 host port mapping"},
		},
		{
			name: "hostPort combines with host namespace reasons",
			cfg: func() *runtimeapi.PodSandboxConfig {
				cfg := sandboxConfigWithNS(node, pod, pod)
				cfg.PortMappings = []*runtimeapi.PortMapping{{ContainerPort: 80, HostPort: 80}}
				return cfg
			}(),
			wantBad:   true,
			wantWords: []string{"hostNetwork", "hostPort"},
		},
		{
			name: "sysctls are rejected",
			cfg: &runtimeapi.PodSandboxConfig{
				Linux: &runtimeapi.LinuxPodSandboxConfig{
					Sysctls: map[string]string{"net.core.somaxconn": "1024"},
				},
			},
			wantBad:   true,
			wantWords: []string{"sysctls"},
		},
		{
			name: "sandbox-level privileged is rejected",
			cfg: &runtimeapi.PodSandboxConfig{
				Linux: &runtimeapi.LinuxPodSandboxConfig{
					SecurityContext: &runtimeapi.LinuxSandboxSecurityContext{Privileged: true},
				},
			},
			wantBad:   true,
			wantWords: []string{"privileged"},
		},
		{
			// hostUsers unset: kubelet leaves userns options at host mode; must pass.
			name: "default userns mode is supported",
			cfg: &runtimeapi.PodSandboxConfig{
				Linux: &runtimeapi.LinuxPodSandboxConfig{
					SecurityContext: &runtimeapi.LinuxSandboxSecurityContext{
						NamespaceOptions: &runtimeapi.NamespaceOption{
							UsernsOptions: &runtimeapi.UserNamespace{Mode: runtimeapi.NamespaceMode_NODE},
						},
					},
				},
			},
			wantBad: false,
		},
		{
			name: "hostUsers=false userns request is rejected",
			cfg: &runtimeapi.PodSandboxConfig{
				Linux: &runtimeapi.LinuxPodSandboxConfig{
					SecurityContext: &runtimeapi.LinuxSandboxSecurityContext{
						NamespaceOptions: &runtimeapi.NamespaceOption{
							UsernsOptions: &runtimeapi.UserNamespace{Mode: runtimeapi.NamespaceMode_POD},
						},
					},
				},
			},
			wantBad:   true,
			wantWords: []string{"user namespaces"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, bad := unsupportedSandboxShape(tt.cfg)
			checkShape(t, reason, bad, tt.wantBad, tt.wantWords)
		})
	}
}

func checkShape(t *testing.T, reason string, bad, wantBad bool, wantWords []string) {
	t.Helper()
	if bad != wantBad {
		t.Fatalf("shape guard bad = %v, want %v (reason %q)", bad, wantBad, reason)
	}
	if !bad {
		if reason != "" {
			t.Fatalf("expected empty reason for supported shape, got %q", reason)
		}
		return
	}
	for _, w := range wantWords {
		if !strings.Contains(reason, w) {
			t.Errorf("reason %q does not mention %q", reason, w)
		}
	}
}

func TestUnsupportedContainerShape(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *runtimeapi.ContainerConfig
		wantBad   bool
		wantWords []string
	}{
		{
			// kubelet sends security_context with privileged=false and
			// pid=CONTAINER for every vanilla container; presence must pass.
			name: "vanilla kubelet container shape is supported",
			cfg: &runtimeapi.ContainerConfig{
				Linux: &runtimeapi.LinuxContainerConfig{
					SecurityContext: &runtimeapi.LinuxContainerSecurityContext{
						Privileged: false,
						NamespaceOptions: &runtimeapi.NamespaceOption{
							Pid: runtimeapi.NamespaceMode_CONTAINER,
						},
					},
				},
			},
			wantBad: false,
		},
		{
			name: "privileged container is rejected",
			cfg: &runtimeapi.ContainerConfig{
				Linux: &runtimeapi.LinuxContainerConfig{
					SecurityContext: &runtimeapi.LinuxContainerSecurityContext{Privileged: true},
				},
			},
			wantBad:   true,
			wantWords: []string{"privileged"},
		},
		{
			name: "host devices are rejected",
			cfg: &runtimeapi.ContainerConfig{
				Devices: []*runtimeapi.Device{{HostPath: "/dev/kvm", ContainerPath: "/dev/kvm"}},
			},
			wantBad:   true,
			wantWords: []string{"devices"},
		},
		{
			name: "CDI devices are rejected",
			cfg: &runtimeapi.ContainerConfig{
				CDIDevices: []*runtimeapi.CDIDevice{{Name: "vendor.com/gpu=0"}},
			},
			wantBad:   true,
			wantWords: []string{"CDI"},
		},
		{
			name: "targeted PID namespace is rejected",
			cfg: &runtimeapi.ContainerConfig{
				Linux: &runtimeapi.LinuxContainerConfig{
					SecurityContext: &runtimeapi.LinuxContainerSecurityContext{
						NamespaceOptions: &runtimeapi.NamespaceOption{
							Pid:      runtimeapi.NamespaceMode_TARGET,
							TargetId: "other-container",
						},
					},
				},
			},
			wantBad:   true,
			wantWords: []string{"PID namespace"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, bad := unsupportedContainerShape(tt.cfg)
			checkShape(t, reason, bad, tt.wantBad, tt.wantWords)
		})
	}
}

// TestIgnoredContainerFields proves the warn list fires only on explicit,
// non-default requests: kubelet zero-value noise (cpu_shares, oom_score_adj,
// masked_paths, pid=CONTAINER, privileged=false) must produce no warnings,
// while standard hardening boilerplate produces them without rejecting.
func TestIgnoredContainerFields(t *testing.T) {
	vanilla := &runtimeapi.ContainerConfig{
		Linux: &runtimeapi.LinuxContainerConfig{
			Resources: &runtimeapi.LinuxContainerResources{
				CpuShares:   2,
				OomScoreAdj: 1000,
			},
			SecurityContext: &runtimeapi.LinuxContainerSecurityContext{
				Privileged:  false,
				MaskedPaths: []string{"/proc/kcore"},
				NamespaceOptions: &runtimeapi.NamespaceOption{
					Pid: runtimeapi.NamespaceMode_CONTAINER,
				},
			},
		},
	}
	if warns := ignoredContainerFields(vanilla); len(warns) != 0 {
		t.Fatalf("vanilla kubelet container produced warnings: %v", warns)
	}

	runAsUser := int64(1000)
	hardened := &runtimeapi.ContainerConfig{
		WorkingDir: "/app",
		Linux: &runtimeapi.LinuxContainerConfig{
			Resources: &runtimeapi.LinuxContainerResources{
				MemoryLimitInBytes: 512 << 20,
				CpuQuota:           50000,
				CpuPeriod:          100000,
			},
			SecurityContext: &runtimeapi.LinuxContainerSecurityContext{
				RunAsUser:      &runtimeapi.Int64Value{Value: runAsUser},
				ReadonlyRootfs: true,
				NoNewPrivs:     true,
				Capabilities:   &runtimeapi.Capability{DropCapabilities: []string{"ALL"}},
				Seccomp:        &runtimeapi.SecurityProfile{ProfileType: runtimeapi.SecurityProfile_RuntimeDefault},
			},
		},
	}
	warns := ignoredContainerFields(hardened)
	if len(warns) < 5 {
		t.Fatalf("hardened container warnings = %d (%v), want >= 5", len(warns), warns)
	}
	joined := strings.Join(warns, "; ")
	for _, w := range []string{"workingDir", "runAsUser", "readOnlyRootFilesystem", "capability", "seccomp", "limits"} {
		if !strings.Contains(joined, w) {
			t.Errorf("warnings %q missing %q", joined, w)
		}
	}
}

// TestLinuxPodPodSpecSizing proves the Pod VM spec is sized from kubelet's
// Pod-level resource sum: quota/period rounds up to whole CPUs, memory limit
// carries through, and absent resources leave the helper defaults (zero).
func TestLinuxPodPodSpecSizing(t *testing.T) {
	cfg := &runtimeapi.PodSandboxConfig{
		Hostname: "h",
		Linux: &runtimeapi.LinuxPodSandboxConfig{
			Resources: &runtimeapi.LinuxContainerResources{
				CpuQuota:           250000, // 2.5 CPUs -> 3
				CpuPeriod:          100000,
				MemoryLimitInBytes: 2 << 30,
			},
		},
	}
	spec := linuxpodPodSpec("id-1", cfg)
	if spec.CPUs != 3 || spec.MemoryBytes != 2<<30 || spec.Hostname != "h" {
		t.Fatalf("spec = %+v, want CPUs=3 MemoryBytes=%d Hostname=h", spec, int64(2<<30))
	}
	empty := linuxpodPodSpec("id-2", &runtimeapi.PodSandboxConfig{})
	if empty.CPUs != 0 || empty.MemoryBytes != 0 {
		t.Fatalf("empty resources should leave helper defaults, got %+v", empty)
	}
}
