package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// secret builds an in-memory Secret with the given byte data, for #47 tests.
func secret(namespace, name string, data map[string][]byte) *corev1.Secret {
	s := &corev1.Secret{Data: data}
	s.Namespace = namespace
	s.Name = name
	return s
}

// isSecretPending reports whether err is the transient "Secret not ready" signal
// that keeps a Pod Pending and retrying instead of failing terminally.
func isSecretPending(err error) bool { return errors.Is(err, errSecretUnavailable) }

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return fi.Mode().Perm()
}

func TestResolveEnvFromSecretKeyRef(t *testing.T) {
	getter := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/db": secret("default", "db", map[string][]byte{"password": []byte("s3cr3t"), "other": []byte("x")}),
	}}
	c := corev1.Container{
		Name: "app", Image: "x",
		Env: []corev1.EnvVar{
			{Name: "LITERAL", Value: "lit"},
			{Name: "PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "password",
			}}},
		},
	}
	env, err := resolveEnv(pod("default", "p", oneContainer(c)), c, nil, getter)
	if err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	if env["PASSWORD"] != "s3cr3t" {
		t.Errorf("PASSWORD = %q, want s3cr3t", env["PASSWORD"])
	}
	if env["LITERAL"] != "lit" {
		t.Errorf("LITERAL = %q, want lit", env["LITERAL"])
	}
}

func TestResolveEnvFromSecretRefWithPrefixAndPrecedence(t *testing.T) {
	getter := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/creds": secret("default", "creds", map[string][]byte{"A": []byte("fromsecret"), "B": []byte("b"), "bad-name": []byte("skip")}),
	}}
	c := corev1.Container{
		Name: "app", Image: "x",
		EnvFrom: []corev1.EnvFromSource{{Prefix: "P_", SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "creds"},
		}}},
		// An explicit env entry overrides the envFrom-derived value of the same name.
		Env: []corev1.EnvVar{{Name: "P_A", Value: "override"}},
	}
	env, err := resolveEnv(pod("default", "p", oneContainer(c)), c, nil, getter)
	if err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	if env["P_A"] != "override" {
		t.Errorf("P_A = %q, want override (env beats envFrom)", env["P_A"])
	}
	if env["P_B"] != "b" {
		t.Errorf("P_B = %q, want b", env["P_B"])
	}
	if _, ok := env["P_bad-name"]; ok {
		t.Errorf("invalid env var name should be skipped, got %q", env["P_bad-name"])
	}
}

func TestResolveEnvMissingRequiredSecretIsPending(t *testing.T) {
	getter := fakeSecretGetter{secrets: map[string]*corev1.Secret{}}
	c := corev1.Container{
		Name: "app", Image: "x",
		Env: []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Key: "k",
		}}}},
	}
	_, err := resolveEnv(pod("default", "p", oneContainer(c)), c, nil, getter)
	if err == nil || !isSecretPending(err) {
		t.Fatalf("want errSecretUnavailable, got %v", err)
	}
}

func TestResolveEnvOptionalSecretAbsentIsSkipped(t *testing.T) {
	getter := fakeSecretGetter{secrets: map[string]*corev1.Secret{}}
	c := corev1.Container{
		Name: "app", Image: "x",
		Env: []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Key: "k", Optional: optionalRef(true),
		}}}},
	}
	env, err := resolveEnv(pod("default", "p", oneContainer(c)), c, nil, getter)
	if err != nil {
		t.Fatalf("optional absent Secret should be skipped: %v", err)
	}
	if _, ok := env["X"]; ok {
		t.Errorf("X should be absent, got %q", env["X"])
	}
}

// TestResolveEnvMissingKeyDoesNotLeakSecretValues guards the acceptance rule that
// Secret values never appear in errors: a reference to an absent key must fail
// without echoing the values held in the same Secret.
func TestResolveEnvMissingKeyDoesNotLeakSecretValues(t *testing.T) {
	getter := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/db": secret("default", "db", map[string][]byte{"password": []byte("top-secret-value")}),
	}}
	c := corev1.Container{
		Name: "app", Image: "x",
		Env: []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "absent",
		}}}},
	}
	_, err := resolveEnv(pod("default", "p", oneContainer(c)), c, nil, getter)
	if err == nil || !isSecretPending(err) {
		t.Fatalf("missing required key should be pending, got %v", err)
	}
	if strings.Contains(err.Error(), "top-secret-value") {
		t.Errorf("error must not leak secret values: %q", err.Error())
	}
}

func secretVolPod(uid, secretName string, mount corev1.VolumeMount, src *corev1.SecretVolumeSource) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p1", UID: "uid-sec"},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes:       []corev1.Volume{{Name: mount.Name, VolumeSource: corev1.VolumeSource{Secret: src}}},
			Containers: []corev1.Container{{
				Name: "app", Image: "x", VolumeMounts: []corev1.VolumeMount{mount},
			}},
		},
	}
}

func TestSecretVolumeMaterializesFilesReadOnly(t *testing.T) {
	root := t.TempDir()
	getter := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/tls": secret("default", "tls", map[string][]byte{"tls.crt": []byte("CERT"), "tls.key": []byte("KEY")}),
	}}
	p := New("mac-1", newRecordingRuntime(),
		WithVolumePolicy(VolumePolicy{Root: root}),
		WithSecretGetter(getter),
	)
	pod := secretVolPod("uid-sec", "tls",
		corev1.VolumeMount{Name: "certs", MountPath: "/etc/tls"},
		&corev1.SecretVolumeSource{SecretName: "tls"},
	)
	pod.Spec.Volumes[0].Name = "certs"

	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	dir := filepath.Join(root, "uid-sec", "certs")
	got, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil || string(got) != "CERT" {
		t.Fatalf("tls.crt = %q (err %v), want CERT", got, err)
	}
	if m := fileMode(t, filepath.Join(dir, "tls.key")); m != secretFileMode {
		t.Errorf("tls.key mode = %o, want %o", m, secretFileMode)
	}

	rt := p.rt.(*recordingRuntime)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.createdSpecs) != 1 || len(rt.createdSpecs[0].Mounts) != 1 {
		t.Fatalf("want one mount, got %+v", rt.createdSpecs)
	}
	if m := rt.createdSpecs[0].Mounts[0]; m.Target != "/etc/tls" || m.Source != dir || !m.ReadOnly {
		t.Errorf("mount = %+v, want target=/etc/tls source=%s readOnly", m, dir)
	}
}

func TestSecretVolumeItemsAndModes(t *testing.T) {
	getter := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/src": secret("default", "src", map[string][]byte{"a": []byte("1"), "b": []byte("2")}),
	}}
	dir := filepath.Join(t.TempDir(), "v")
	mode := int32(0o400)
	// Only key "a" is projected, to a custom relative path with a tight mode.
	files, err := secretVolumeFiles("default", &corev1.SecretVolumeSource{
		SecretName: "src",
		Items:      []corev1.KeyToPath{{Key: "a", Path: "sub/a.txt", Mode: &mode}},
	}, dir, getter)
	if err != nil {
		t.Fatalf("secretVolumeFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file (only item key), got %d: %+v", len(files), files)
	}
	if files[0].path != filepath.Join(dir, "sub", "a.txt") || string(files[0].data) != "1" {
		t.Errorf("item projection wrong: %+v", files[0])
	}
	if files[0].mode != os.FileMode(0o400) {
		t.Errorf("item mode = %o, want 400", files[0].mode)
	}
}

func TestSecretVolumeOptionalAbsentIsEmptyDir(t *testing.T) {
	getter := fakeSecretGetter{secrets: map[string]*corev1.Secret{}}
	files, err := secretVolumeFiles("default", &corev1.SecretVolumeSource{
		SecretName: "missing", Optional: optionalRef(true),
	}, "/tmp/x", getter)
	if err != nil {
		t.Fatalf("optional absent Secret volume should yield no files, got %v", err)
	}
	if len(files) != 0 {
		t.Errorf("want no files for optional absent Secret, got %+v", files)
	}
}

func TestSecretVolumeMissingRequiredKeyIsPending(t *testing.T) {
	getter := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/src": secret("default", "src", map[string][]byte{"present": []byte("1")}),
	}}
	_, err := secretVolumeFiles("default", &corev1.SecretVolumeSource{
		SecretName: "src", Items: []corev1.KeyToPath{{Key: "absent", Path: "a"}},
	}, "/tmp/x", getter)
	if err == nil || !isSecretPending(err) {
		t.Fatalf("missing required item key should be pending, got %v", err)
	}
}

// TestCreatePodMissingRequiredSecretIsTransient verifies a Pod referencing an
// absent required Secret is not failed terminally: CreatePod returns an error so
// Virtual Kubelet keeps it Pending and retries until the Secret appears.
func TestCreatePodMissingRequiredSecretIsTransient(t *testing.T) {
	p := New("mac-1", newRecordingRuntime(),
		WithVolumePolicy(VolumePolicy{Root: t.TempDir()}),
		WithSecretGetter(fakeSecretGetter{secrets: map[string]*corev1.Secret{}}),
	)
	pod := secretVolPod("uid-sec", "missing",
		corev1.VolumeMount{Name: "certs", MountPath: "/etc/tls"},
		&corev1.SecretVolumeSource{SecretName: "missing"},
	)
	pod.Spec.Volumes[0].Name = "certs"

	err := p.CreatePod(context.Background(), pod)
	if err == nil || !isSecretPending(err) {
		t.Fatalf("missing required Secret should be transient (pending), got %v", err)
	}
	// It must not have been recorded as a terminal Failed Pod.
	st, gerr := p.GetPodStatus(context.Background(), "default", "p1")
	if gerr == nil && st.Phase == corev1.PodFailed {
		t.Errorf("Pod should stay Pending/untracked, not terminal Failed: %+v", st)
	}
}
