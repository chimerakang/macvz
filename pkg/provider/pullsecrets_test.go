package provider

import (
	"encoding/base64"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeSecretGetter serves Secrets from an in-memory map for pull-secret tests. A
// missing name returns a Kubernetes NotFound so loadSecret takes its
// errSecretUnavailable path.
type fakeSecretGetter struct {
	secrets map[string]*corev1.Secret
	err     error // when set, returned for every lookup
}

func (g fakeSecretGetter) GetSecret(namespace, name string) (*corev1.Secret, error) {
	if g.err != nil {
		return nil, g.err
	}
	if s, ok := g.secrets[namespace+"/"+name]; ok {
		return s, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
}

func dockerConfigSecret(name string, body string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(body)},
	}
}

func podWithPullSecrets(image string, names ...string) *corev1.Pod {
	refs := make([]corev1.LocalObjectReference, 0, len(names))
	for _, n := range names {
		refs = append(refs, corev1.LocalObjectReference{Name: n})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			ImagePullSecrets: refs,
			Containers:       []corev1.Container{{Name: "c", Image: image}},
		},
	}
}

func TestRegistryHost(t *testing.T) {
	cases := map[string]string{
		"nginx":                              defaultRegistryHost,
		"library/nginx":                      defaultRegistryHost,
		"library/nginx:1.27":                 defaultRegistryHost,
		"user/repo":                          defaultRegistryHost,
		"docker.io/library/nginx":            "docker.io",
		"registry.example.com/team/app:v1":   "registry.example.com",
		"registry.example.com:5000/team/app": "registry.example.com:5000",
		"localhost/app":                      "localhost",
		"localhost:5000/app":                 "localhost:5000",
		"ghcr.io/owner/app@sha256:abc":       "ghcr.io",
	}
	for image, want := range cases {
		if got := registryHost(image); got != want {
			t.Errorf("registryHost(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestNormalizeRegistryKey(t *testing.T) {
	cases := map[string]string{
		"registry.example.com":                "registry.example.com",
		"https://registry.example.com":        "registry.example.com",
		"http://registry.example.com/v2/":     "registry.example.com",
		"https://index.docker.io/v1/":         "index.docker.io",
		"registry.example.com:5000/path/here": "registry.example.com:5000",
	}
	for key, want := range cases {
		if got := normalizeRegistryKey(key); got != want {
			t.Errorf("normalizeRegistryKey(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestResolvePullAuthMatchesRegistry(t *testing.T) {
	body := `{"auths":{"registry.example.com":{"username":"alice","password":"s3cret"}}}`
	g := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/reg": dockerConfigSecret("reg", body),
	}}
	pod := podWithPullSecrets("registry.example.com/team/app:v1", "reg")

	auth, err := resolvePullAuth(g, pod, pod.Spec.Containers[0].Image)
	if err != nil {
		t.Fatalf("resolvePullAuth: %v", err)
	}
	if auth == nil {
		t.Fatal("expected a credential, got nil")
	}
	if auth.Server != "registry.example.com" || auth.Username != "alice" || auth.Password != "s3cret" {
		t.Errorf("got %+v", auth)
	}
}

func TestResolvePullAuthAuthFieldBase64(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("bob:hunter2"))
	body := `{"auths":{"registry.example.com":{"auth":"` + enc + `"}}}`
	g := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/reg": dockerConfigSecret("reg", body),
	}}
	pod := podWithPullSecrets("registry.example.com/app", "reg")

	auth, err := resolvePullAuth(g, pod, pod.Spec.Containers[0].Image)
	if err != nil {
		t.Fatalf("resolvePullAuth: %v", err)
	}
	if auth == nil || auth.Username != "bob" || auth.Password != "hunter2" {
		t.Fatalf("got %+v", auth)
	}
}

func TestResolvePullAuthDockerHubAliases(t *testing.T) {
	// Credential keyed under the canonical Docker Hub spelling must match a plain
	// Docker Hub short-name image.
	body := `{"auths":{"https://index.docker.io/v1/":{"username":"u","password":"p"}}}`
	g := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/hub": dockerConfigSecret("hub", body),
	}}
	pod := podWithPullSecrets("library/nginx:1.27", "hub")

	auth, err := resolvePullAuth(g, pod, pod.Spec.Containers[0].Image)
	if err != nil {
		t.Fatalf("resolvePullAuth: %v", err)
	}
	if auth == nil || auth.Username != "u" {
		t.Fatalf("expected Docker Hub credential, got %+v", auth)
	}
	if auth.Server != defaultRegistryHost {
		t.Errorf("Server = %q, want %q", auth.Server, defaultRegistryHost)
	}
}

func TestResolvePullAuthNoMatchFallsBackAnonymous(t *testing.T) {
	body := `{"auths":{"other.example.com":{"username":"u","password":"p"}}}`
	g := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/reg": dockerConfigSecret("reg", body),
	}}
	pod := podWithPullSecrets("registry.example.com/app", "reg")

	auth, err := resolvePullAuth(g, pod, pod.Spec.Containers[0].Image)
	if err != nil {
		t.Fatalf("resolvePullAuth: %v", err)
	}
	if auth != nil {
		t.Errorf("expected nil (anonymous), got %+v", auth)
	}
}

func TestResolvePullAuthNoPullSecrets(t *testing.T) {
	pod := podWithPullSecrets("registry.example.com/app") // none named
	auth, err := resolvePullAuth(fakeSecretGetter{}, pod, pod.Spec.Containers[0].Image)
	if err != nil || auth != nil {
		t.Fatalf("got auth=%+v err=%v, want nil,nil", auth, err)
	}
}

func TestResolvePullAuthMissingSecretIsTransient(t *testing.T) {
	g := fakeSecretGetter{secrets: map[string]*corev1.Secret{}}
	pod := podWithPullSecrets("registry.example.com/app", "absent")

	_, err := resolvePullAuth(g, pod, pod.Spec.Containers[0].Image)
	if !errors.Is(err, errSecretUnavailable) {
		t.Fatalf("err = %v, want errSecretUnavailable", err)
	}
}

func TestResolvePullAuthMalformedJSON(t *testing.T) {
	g := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/reg": dockerConfigSecret("reg", "{not json"),
	}}
	pod := podWithPullSecrets("registry.example.com/app", "reg")

	if _, err := resolvePullAuth(g, pod, pod.Spec.Containers[0].Image); err == nil {
		t.Fatal("expected an error for malformed dockerconfigjson")
	}
}

func TestResolvePullAuthMatchedButUnusableIsError(t *testing.T) {
	// A matching registry entry with no usable credential is a user mistake, not a
	// silent fall-through to an anonymous pull.
	body := `{"auths":{"registry.example.com":{}}}`
	g := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/reg": dockerConfigSecret("reg", body),
	}}
	pod := podWithPullSecrets("registry.example.com/app", "reg")

	if _, err := resolvePullAuth(g, pod, pod.Spec.Containers[0].Image); err == nil {
		t.Fatal("expected an error for an entry with no credential")
	}
}

func TestResolvePullAuthLegacyDockercfg(t *testing.T) {
	body := `{"registry.example.com":{"username":"u","password":"p"}}`
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "default"},
		Type:       corev1.SecretTypeDockercfg,
		Data:       map[string][]byte{corev1.DockerConfigKey: []byte(body)},
	}
	g := fakeSecretGetter{secrets: map[string]*corev1.Secret{"default/legacy": sec}}
	pod := podWithPullSecrets("registry.example.com/app", "legacy")

	auth, err := resolvePullAuth(g, pod, pod.Spec.Containers[0].Image)
	if err != nil {
		t.Fatalf("resolvePullAuth: %v", err)
	}
	if auth == nil || auth.Username != "u" || auth.Password != "p" {
		t.Fatalf("got %+v", auth)
	}
}

func TestResolvePullAuthFirstMatchWins(t *testing.T) {
	// Two pull secrets named; only the second has a matching registry.
	none := dockerConfigSecret("none", `{"auths":{"other.example.com":{"username":"x","password":"y"}}}`)
	match := dockerConfigSecret("match", `{"auths":{"registry.example.com":{"username":"u","password":"p"}}}`)
	g := fakeSecretGetter{secrets: map[string]*corev1.Secret{
		"default/none":  none,
		"default/match": match,
	}}
	pod := podWithPullSecrets("registry.example.com/app", "none", "match")

	auth, err := resolvePullAuth(g, pod, pod.Spec.Containers[0].Image)
	if err != nil {
		t.Fatalf("resolvePullAuth: %v", err)
	}
	if auth == nil || auth.Username != "u" {
		t.Fatalf("expected the matching secret's credential, got %+v", auth)
	}
}
