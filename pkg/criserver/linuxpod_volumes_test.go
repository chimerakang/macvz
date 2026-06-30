package criserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// linuxpod_volumes_test.go is the CRI-L8-3 (#145) hermetic half of the Kubernetes
// volume-projection matrix for the LinuxPod-backed CRI path. It proves the CRI-side
// mount translation honors every common kubelet-managed volume type a k3s workload
// relies on — configMap, secret, projected (incl. the service-account token
// projection), downward API, emptyDir (disk and Memory medium), and allowed
// hostPath — with correct read-only/read-write permission, that each realized mount
// reaches the LinuxPod backend's CreateRequest, that a shared emptyDir appears in
// every container that mounts it, and that the conservative mount policy still
// rejects the unsafe cases. The kubelet (not MacVz) materializes the volume content,
// file modes, and ownership on the host and passes the directories as bind mounts;
// the adapter's contract is to translate and bind them honestly, which is exactly
// what these tests assert. The live half (file modes/ownership, update behavior,
// cleanup) runs on the real k3s topology via test/e2e/cri-k3s/linuxpod-volumes.sh.

// kubeletVolume models one kubelet-materialized volume directory laid out the way
// the kubelet lays it out under <podsDir>/<uid>/volumes/<plugin>/<name>.
type kubeletVolume struct {
	plugin string // e.g. kubernetes.io~configmap
	name   string // the volume name under the plugin dir
	files  map[string]string
}

// materializeKubeletVolume creates the kubelet volume directory + files and returns
// its absolute host source path, the same path the kubelet would pass as HostPath.
func materializeKubeletVolume(t *testing.T, podsDir, uid string, v kubeletVolume) string {
	t.Helper()
	src := filepath.Join(podsDir, uid, "volumes", v.plugin, v.name)
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", src, err)
	}
	for rel, content := range v.files {
		full := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return src
}

// TestLinuxPodVolumeProjectionMatrix is the core CRI-L8-3 matrix: every common
// kubelet volume type translates to a backend mount with the right source, target,
// and permission, and reaches the LinuxPod backend per container.
func TestLinuxPodVolumeProjectionMatrix(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	podsDir := t.TempDir()
	hostShare := t.TempDir() // an operator-allowlisted hostPath outside the pods dir

	const uid = "uid-1"
	configMap := materializeKubeletVolume(t, podsDir, uid, kubeletVolume{
		plugin: "kubernetes.io~configmap", name: "app-config",
		files: map[string]string{"app.conf": "k=v\n"},
	})
	secret := materializeKubeletVolume(t, podsDir, uid, kubeletVolume{
		plugin: "kubernetes.io~secret", name: "app-secret",
		files: map[string]string{"token": "s3cr3t\n"},
	})
	downward := materializeKubeletVolume(t, podsDir, uid, kubeletVolume{
		plugin: "kubernetes.io~downward-api", name: "podinfo",
		files: map[string]string{"labels": "app=\"web\"\n"},
	})
	// Projected volume carrying the service-account token projection (bound token +
	// cluster CA + namespace), the standard in-cluster API access volume.
	saToken := materializeKubeletVolume(t, podsDir, uid, kubeletVolume{
		plugin: "kubernetes.io~projected", name: "kube-api-access-abcde",
		files: map[string]string{
			"token":     "jwt\n",
			"ca.crt":    "-----BEGIN CERTIFICATE-----\n",
			"namespace": "default\n",
		},
	})
	diskEmptyDir := materializeKubeletVolume(t, podsDir, uid, kubeletVolume{
		plugin: "kubernetes.io~empty-dir", name: "scratch",
	})
	if err := os.MkdirAll(hostShare, 0o755); err != nil {
		t.Fatalf("mkdir hostShare: %v", err)
	}

	svc, err := NewLinuxPodService(LinuxPodOptions{
		Backend: backend,
		Mounts: MountPolicy{
			KubeletPodsDir:          podsDir,
			HostPathAllowedPrefixes: []string{hostShare},
		},
	})
	if err != nil {
		t.Fatalf("NewLinuxPodService: %v", err)
	}
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)

	// Each matrix entry: a CRI mount and the realized backend mount it must produce.
	type want struct {
		source string
		target string
		ro     bool
		tmpfs  bool
	}
	cases := []struct {
		desc  string
		mount *runtimeapi.Mount
		want  want
	}{
		{"configMap read-only",
			&runtimeapi.Mount{HostPath: configMap, ContainerPath: "/etc/config", Readonly: true},
			want{source: configMap, target: "/etc/config", ro: true}},
		{"secret read-only",
			&runtimeapi.Mount{HostPath: secret, ContainerPath: "/etc/secret", Readonly: true},
			want{source: secret, target: "/etc/secret", ro: true}},
		{"projected SA token read-only",
			&runtimeapi.Mount{HostPath: saToken, ContainerPath: "/var/run/secrets/kubernetes.io/serviceaccount", Readonly: true},
			want{source: saToken, target: "/var/run/secrets/kubernetes.io/serviceaccount", ro: true}},
		{"downward API read-only",
			&runtimeapi.Mount{HostPath: downward, ContainerPath: "/etc/podinfo", Readonly: true},
			want{source: downward, target: "/etc/podinfo", ro: true}},
		{"disk emptyDir read-write",
			&runtimeapi.Mount{HostPath: diskEmptyDir, ContainerPath: "/scratch"},
			want{source: diskEmptyDir, target: "/scratch"}},
		{"memory emptyDir -> guest tmpfs",
			&runtimeapi.Mount{HostPath: "", ContainerPath: "/cache"},
			want{target: "/cache", tmpfs: true}},
		{"allowlisted hostPath read-write",
			&runtimeapi.Mount{HostPath: hostShare, ContainerPath: "/host"},
			want{source: hostShare, target: "/host"}},
	}

	criMounts := make([]*runtimeapi.Mount, 0, len(cases))
	for _, c := range cases {
		criMounts = append(criMounts, c.mount)
	}

	cresp, err := svc.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId: sandboxID,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: "app"},
			Image:    &runtimeapi.ImageSpec{Image: "docker.io/library/busybox:1.36.1"},
			Mounts:   criMounts,
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}

	// The realized mounts reached the LinuxPod backend's CreateRequest in order, and
	// each carries the right source/target/permission/tmpfs flag.
	got, ok := backend.ContainerMounts(sandboxID, "app")
	if !ok {
		t.Fatalf("backend has no container 'app'")
	}
	if len(got) != len(cases) {
		t.Fatalf("backend mounts = %d, want %d: %+v", len(got), len(cases), got)
	}
	for i, c := range cases {
		g := got[i]
		if g.Source != c.want.source || g.Target != c.want.target || g.ReadOnly != c.want.ro || g.Tmpfs != c.want.tmpfs {
			t.Errorf("%s: backend mount = %+v, want source=%q target=%q ro=%v tmpfs=%v",
				c.desc, g, c.want.source, c.want.target, c.want.ro, c.want.tmpfs)
		}
	}

	// The same mounts also surface in ContainerStatus so `crictl inspect` reports the
	// volume set the container was created with.
	st, err := svc.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: cresp.GetContainerId()})
	if err != nil {
		t.Fatalf("ContainerStatus: %v", err)
	}
	if len(st.GetStatus().GetMounts()) != len(cases) {
		t.Errorf("status mounts = %d, want %d", len(st.GetStatus().GetMounts()), len(cases))
	}
}

// TestLinuxPodVolumeSharedEmptyDirAcrossContainers proves the multi-container
// sharing case: an app and a late sidecar in one Pod both mount the same kubelet
// emptyDir source, and the realized mount reaches the backend for each container —
// the translation does not drop or rewrite the shared source per container.
func TestLinuxPodVolumeSharedEmptyDirAcrossContainers(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	podsDir := t.TempDir()
	shared := materializeKubeletVolume(t, podsDir, "uid-1", kubeletVolume{
		plugin: "kubernetes.io~empty-dir", name: "shared",
	})

	svc, err := NewLinuxPodService(LinuxPodOptions{
		Backend: backend,
		Mounts:  MountPolicy{KubeletPodsDir: podsDir},
	})
	if err != nil {
		t.Fatalf("NewLinuxPodService: %v", err)
	}
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)

	create := func(name, target string) {
		t.Helper()
		cresp, err := svc.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
			PodSandboxId: sandboxID,
			Config: &runtimeapi.ContainerConfig{
				Metadata: &runtimeapi.ContainerMetadata{Name: name},
				Image:    &runtimeapi.ImageSpec{Image: "docker.io/library/busybox:1.36.1"},
				Mounts:   []*runtimeapi.Mount{{HostPath: shared, ContainerPath: target}},
			},
		})
		if err != nil {
			t.Fatalf("CreateContainer(%s): %v", name, err)
		}
		if _, err := svc.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: cresp.GetContainerId()}); err != nil {
			t.Fatalf("StartContainer(%s): %v", name, err)
		}
	}
	// app mounts the shared dir, then a late sidecar mounts the same source after the
	// app is running (the LinuxPod late-container shape).
	create("app", "/shared")
	create("sidecar", "/data")

	for name, target := range map[string]string{"app": "/shared", "sidecar": "/data"} {
		got, ok := backend.ContainerMounts(sandboxID, name)
		if !ok {
			t.Fatalf("backend has no container %q", name)
		}
		if len(got) != 1 || got[0].Source != shared || got[0].Target != target {
			t.Errorf("%s: backend mounts = %+v, want shared source %q at %q", name, got, shared, target)
		}
	}
}

// TestLinuxPodVolumePolicyErrors proves the conservative mount policy still rejects
// the unsafe cases on the LinuxPod path, with no backend container left behind, so a
// k3s workload that requests an unsupported/unsafe volume gets an honest CRI error
// rather than a silently-dropped or hijacked volume.
func TestLinuxPodVolumePolicyErrors(t *testing.T) {
	backend := linuxpod.NewFakeBackend()
	podsDir := t.TempDir()
	svc, err := NewLinuxPodService(LinuxPodOptions{
		Backend: backend,
		Mounts:  MountPolicy{KubeletPodsDir: podsDir},
	})
	if err != nil {
		t.Fatalf("NewLinuxPodService: %v", err)
	}
	ctx := context.Background()
	sandboxID := lpRunSandbox(t, svc)

	cases := []struct {
		desc  string
		mount *runtimeapi.Mount
		code  codes.Code
	}{
		{"unallowed hostPath",
			&runtimeapi.Mount{HostPath: "/Users/me/secrets", ContainerPath: "/data"}, codes.FailedPrecondition},
		{"reserved runtime target",
			&runtimeapi.Mount{HostPath: filepath.Join(podsDir, "uid-1/volumes/x/cfg"), ContainerPath: "/run/macvz/handoff"}, codes.FailedPrecondition},
		{"bidirectional propagation",
			&runtimeapi.Mount{HostPath: filepath.Join(podsDir, "uid-1/volumes/kubernetes.io~empty-dir/s"), ContainerPath: "/s", Propagation: runtimeapi.MountPropagation_PROPAGATION_BIDIRECTIONAL}, codes.FailedPrecondition},
		{"relative container path",
			&runtimeapi.Mount{HostPath: filepath.Join(podsDir, "uid-1/x"), ContainerPath: "rel/target"}, codes.InvalidArgument},
		{"relative host path",
			&runtimeapi.Mount{HostPath: "rel/src", ContainerPath: "/data"}, codes.InvalidArgument},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			_, err := svc.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
				PodSandboxId: sandboxID,
				Config: &runtimeapi.ContainerConfig{
					Metadata: &runtimeapi.ContainerMetadata{Name: "app"},
					Image:    &runtimeapi.ImageSpec{Image: "docker.io/library/busybox:1.36.1"},
					Mounts:   []*runtimeapi.Mount{c.mount},
				},
			})
			if status.Code(err) != c.code {
				t.Fatalf("err = %v, want %s", err, c.code)
			}
			if _, ok := backend.ContainerMounts(sandboxID, "app"); ok {
				t.Errorf("backend container created despite rejected mount %q", c.desc)
			}
		})
	}
}
