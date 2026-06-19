package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeConfigMaps is an in-memory ConfigMapGetter keyed by "namespace/name".
type fakeConfigMaps struct {
	cms map[string]*corev1.ConfigMap
}

func newFakeConfigMaps(cms ...*corev1.ConfigMap) *fakeConfigMaps {
	m := &fakeConfigMaps{cms: map[string]*corev1.ConfigMap{}}
	for _, cm := range cms {
		m.cms[cm.Namespace+"/"+cm.Name] = cm
	}
	return m
}

func (f *fakeConfigMaps) GetConfigMap(namespace, name string) (*corev1.ConfigMap, error) {
	if cm, ok := f.cms[namespace+"/"+name]; ok {
		return cm, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
}

func cm(namespace, name string, data map[string]string) *corev1.ConfigMap {
	c := &corev1.ConfigMap{Data: data}
	c.Namespace = namespace
	c.Name = name
	return c
}

func optionalRef(b bool) *bool { return &b }

func TestResolveEnvFromConfigMapKeyRef(t *testing.T) {
	getter := newFakeConfigMaps(cm("default", "app-config", map[string]string{"LOG_LEVEL": "debug", "OTHER": "x"}))
	c := corev1.Container{
		Name: "app", Image: "x",
		Env: []corev1.EnvVar{
			{Name: "LITERAL", Value: "lit"},
			{Name: "LEVEL", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"}, Key: "LOG_LEVEL",
			}}},
		},
	}
	env, err := resolveEnv(pod("default", "p", oneContainer(c)), c, getter, nil)
	if err != nil {
		t.Fatalf("resolveEnv: %v", err)
	}
	if env["LEVEL"] != "debug" {
		t.Errorf("LEVEL = %q, want debug", env["LEVEL"])
	}
	if env["LITERAL"] != "lit" {
		t.Errorf("LITERAL = %q, want lit", env["LITERAL"])
	}
}

func TestResolveEnvFromConfigMapRefWithPrefixAndPrecedence(t *testing.T) {
	getter := newFakeConfigMaps(cm("default", "cfg", map[string]string{"A": "fromcm", "B": "b", "bad-name": "skip"}))
	c := corev1.Container{
		Name: "app", Image: "x",
		EnvFrom: []corev1.EnvFromSource{{Prefix: "P_", ConfigMapRef: &corev1.ConfigMapEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"},
		}}},
		// An explicit env entry overrides the envFrom-derived value of the same name.
		Env: []corev1.EnvVar{{Name: "P_A", Value: "override"}},
	}
	env, err := resolveEnv(pod("default", "p", oneContainer(c)), c, getter, nil)
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

func TestResolveEnvMissingRequiredConfigMapIsPending(t *testing.T) {
	getter := newFakeConfigMaps() // empty
	c := corev1.Container{
		Name: "app", Image: "x",
		Env: []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Key: "k",
		}}}},
	}
	_, err := resolveEnv(pod("default", "p", oneContainer(c)), c, getter, nil)
	if err == nil || !isPending(err) {
		t.Fatalf("want errConfigPending, got %v", err)
	}
}

func TestResolveEnvOptionalConfigMapAbsentIsSkipped(t *testing.T) {
	getter := newFakeConfigMaps()
	c := corev1.Container{
		Name: "app", Image: "x",
		Env: []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Key: "k", Optional: optionalRef(true),
		}}}},
	}
	env, err := resolveEnv(pod("default", "p", oneContainer(c)), c, getter, nil)
	if err != nil {
		t.Fatalf("optional absent ConfigMap should be skipped: %v", err)
	}
	if _, ok := env["X"]; ok {
		t.Errorf("X should be absent, got %q", env["X"])
	}
}

func TestResolveEnvMissingRequiredKeyIsPending(t *testing.T) {
	getter := newFakeConfigMaps(cm("default", "cfg", map[string]string{"present": "1"}))
	c := corev1.Container{
		Name: "app", Image: "x",
		Env: []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}, Key: "absent",
		}}}},
	}
	if _, err := resolveEnv(pod("default", "p", oneContainer(c)), c, getter, nil); err == nil || !isPending(err) {
		t.Fatalf("missing required key should be pending, got %v", err)
	}
}

func isPending(err error) bool {
	return errors.Is(err, errConfigPending)
}

func TestConfigMapVolumeMaterializesFiles(t *testing.T) {
	root := t.TempDir()
	getter := newFakeConfigMaps(cm("default", "files", map[string]string{
		"app.conf": "key=value",
		"nested":   "n",
	}))
	p := New("mac-1", newRecordingRuntime(),
		WithVolumePolicy(VolumePolicy{Root: root}),
		WithConfigMapGetter(getter),
	)
	pod := &corev1.Pod{}
	pod.Namespace = "default"
	pod.Name = "p1"
	pod.UID = "uid-cm"
	pod.Spec = corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Volumes: []corev1.Volume{{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "files"},
		}}}},
		Containers: []corev1.Container{{
			Name: "app", Image: "x",
			VolumeMounts: []corev1.VolumeMount{{Name: "cfg", MountPath: "/etc/app"}},
		}},
	}
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	dir := filepath.Join(root, "uid-cm", "cfg")
	got, err := os.ReadFile(filepath.Join(dir, "app.conf"))
	if err != nil {
		t.Fatalf("read app.conf: %v", err)
	}
	if string(got) != "key=value" {
		t.Errorf("app.conf = %q, want key=value", got)
	}

	// The mount is read-only and points at the materialized dir.
	rt := p.rt.(*recordingRuntime)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.createdSpecs) != 1 || len(rt.createdSpecs[0].Mounts) != 1 {
		t.Fatalf("want one mount, got %+v", rt.createdSpecs)
	}
	m := rt.createdSpecs[0].Mounts[0]
	if m.Target != "/etc/app" || m.Source != dir || !m.ReadOnly {
		t.Errorf("mount = %+v, want target=/etc/app source=%s readOnly", m, dir)
	}
}

func TestConfigMapVolumeItemsAndOptional(t *testing.T) {
	root := t.TempDir()
	getter := newFakeConfigMaps(cm("default", "src", map[string]string{"a": "1", "b": "2"}))
	dir := filepath.Join(root, "uid", "v")
	// Only key "a" is projected, to a custom relative path.
	files, err := configMapVolumeFiles("default", &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "src"},
		Items:                []corev1.KeyToPath{{Key: "a", Path: "sub/a.txt"}},
	}, dir, getter)
	if err != nil {
		t.Fatalf("configMapVolumeFiles: %v", err)
	}
	if len(files) != 1 || files[0].path != filepath.Join(dir, "sub", "a.txt") || string(files[0].data) != "1" {
		t.Fatalf("items projection wrong: %+v", files)
	}

	// Optional, absent ConfigMap yields no files (empty dir), no error.
	none, err := configMapVolumeFiles("default", &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "gone"},
		Optional:             optionalRef(true),
	}, dir, getter)
	if err != nil || none != nil {
		t.Fatalf("optional absent configMap: files=%v err=%v", none, err)
	}

	// Required, absent ConfigMap is pending.
	if _, err := configMapVolumeFiles("default", &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "gone"},
	}, dir, getter); err == nil || !isPending(err) {
		t.Fatalf("required absent configMap should be pending, got %v", err)
	}
}

func TestCreatePodMissingConfigMapRetries(t *testing.T) {
	getter := newFakeConfigMaps() // ConfigMap not present yet
	p := New("mac-1", newRecordingRuntime(), WithConfigMapGetter(getter))
	c := corev1.Container{
		Name: "app", Image: "x",
		Env: []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "later"}, Key: "k",
		}}}},
	}
	pod := pod("default", "p1", oneContainer(c))
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways

	// A missing required ConfigMap must NOT be terminal: CreatePod returns an
	// error so Virtual Kubelet keeps the Pod Pending and retries.
	if err := p.CreatePod(context.Background(), pod); err == nil {
		t.Fatal("CreatePod should return a retryable error while the ConfigMap is absent")
	}
	if _, err := p.GetPod(context.Background(), "default", "p1"); err == nil {
		t.Error("a not-ready Pod should not be tracked (so the retry re-resolves)")
	}
}
