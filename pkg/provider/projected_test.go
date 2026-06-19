package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func i64(v int64) *int64 { return &v }

// fakeTokens is an in-memory TokenRequester recording the last request.
type fakeTokens struct {
	token  string
	err    error
	gotNS  string
	gotSA  string
	gotAud []string
	gotExp *int64
	gotPod *corev1.Pod
}

func (f *fakeTokens) RequestToken(_ context.Context, namespace, serviceAccountName string, pod *corev1.Pod, audiences []string, expirationSeconds *int64) (string, error) {
	f.gotNS, f.gotSA, f.gotAud, f.gotExp, f.gotPod = namespace, serviceAccountName, audiences, expirationSeconds, pod
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

// apiAccessVolume builds the projected volume Kubernetes auto-injects for
// in-cluster API access: a service-account token, the cluster CA, and the
// namespace, all under the standard mount path.
func apiAccessVolume() (corev1.Volume, corev1.VolumeMount) {
	v := corev1.Volume{
		Name: "kube-api-access-abcde",
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: i64(3607)}},
				{ConfigMap: &corev1.ConfigMapProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
					Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				}},
				{DownwardAPI: &corev1.DownwardAPIProjection{
					Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
				}},
			},
		}},
	}
	m := corev1.VolumeMount{Name: v.Name, MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}
	return v, m
}

// fileByBase finds a materialized file by its base name.
func fileByBase(files []fileToWrite, base string) (fileToWrite, bool) {
	for _, f := range files {
		if filepath.Base(f.path) == base {
			return f, true
		}
	}
	return fileToWrite{}, false
}

func TestResolveProjectedServiceAccountVolume(t *testing.T) {
	v, m := apiAccessVolume()
	pod := volPod("uid-1", []corev1.Volume{v}, []corev1.VolumeMount{m})

	tokens := &fakeTokens{token: "signed-jwt"}
	cms := newFakeConfigMaps(cm("ns", "kube-root-ca.crt", map[string]string{"ca.crt": "PEM-DATA"}))

	rv, err := resolveVolumes(context.Background(), pod, VolumePolicy{Root: "/var/lib/macvz/volumes"}, cms, nil, tokens)
	if err != nil {
		t.Fatalf("resolveVolumes: %v", err)
	}

	// One read-only mount at the standard service-account path.
	if len(rv.mounts) != 1 {
		t.Fatalf("mounts = %v, want exactly one", rv.mounts)
	}
	if got := rv.mounts[0].Target; got != "/var/run/secrets/kubernetes.io/serviceaccount" {
		t.Errorf("mount target = %q, want the standard SA path", got)
	}
	if !rv.mounts[0].ReadOnly {
		t.Error("projected SA mount must be read-only")
	}

	// Three files: token, ca.crt, namespace — with the expected content and mode.
	want := map[string]string{"token": "signed-jwt", "ca.crt": "PEM-DATA", "namespace": "ns"}
	for base, data := range want {
		f, ok := fileByBase(rv.configFiles, base)
		if !ok {
			t.Errorf("missing materialized file %q", base)
			continue
		}
		if string(f.data) != data {
			t.Errorf("file %q data = %q, want %q", base, f.data, data)
		}
		if f.mode != projectedFileMode {
			t.Errorf("file %q mode = %o, want %o", base, f.mode, projectedFileMode)
		}
	}
	if len(rv.configFiles) != 3 {
		t.Errorf("configFiles = %d, want 3 (%v)", len(rv.configFiles), rv.configFiles)
	}

	// The token request was for the default ServiceAccount in the Pod's namespace,
	// carrying the projection's requested expiration.
	if tokens.gotNS != "ns" || tokens.gotSA != "default" {
		t.Errorf("token request ns/sa = %q/%q, want ns/default", tokens.gotNS, tokens.gotSA)
	}
	if tokens.gotExp == nil || *tokens.gotExp != 3607 {
		t.Errorf("token request expiration = %v, want 3607", tokens.gotExp)
	}
	if tokens.gotPod == nil || tokens.gotPod.UID != pod.UID {
		t.Errorf("token request not bound to the Pod")
	}
}

func TestResolveProjectedHonorsExplicitServiceAccountAndAudience(t *testing.T) {
	v := corev1.Volume{
		Name: "kube-api-access-zzz",
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", Audience: "vault"}},
			},
		}},
	}
	m := corev1.VolumeMount{Name: v.Name, MountPath: "/var/run/secrets/tokens"}
	pod := volPod("uid-2", []corev1.Volume{v}, []corev1.VolumeMount{m})
	pod.Spec.ServiceAccountName = "builder"

	tokens := &fakeTokens{token: "t"}
	if _, err := resolveVolumes(context.Background(), pod, VolumePolicy{Root: "/x"}, nil, nil, tokens); err != nil {
		t.Fatalf("resolveVolumes: %v", err)
	}
	if tokens.gotSA != "builder" {
		t.Errorf("serviceAccount = %q, want builder", tokens.gotSA)
	}
	if len(tokens.gotAud) != 1 || tokens.gotAud[0] != "vault" {
		t.Errorf("audiences = %v, want [vault]", tokens.gotAud)
	}
}

func TestResolveProjectedTokenErrorIsPending(t *testing.T) {
	v, m := apiAccessVolume()
	pod := volPod("uid-3", []corev1.Volume{v}, []corev1.VolumeMount{m})
	cms := newFakeConfigMaps(cm("ns", "kube-root-ca.crt", map[string]string{"ca.crt": "PEM"}))

	tokens := &fakeTokens{err: errors.New("apiserver unreachable")}
	_, err := resolveVolumes(context.Background(), pod, VolumePolicy{Root: "/x"}, cms, nil, tokens)
	if err == nil || !errors.Is(err, errConfigPending) {
		t.Fatalf("err = %v, want a pending (retryable) error", err)
	}
}

func TestResolveProjectedToleratedWithoutRequester(t *testing.T) {
	v, m := apiAccessVolume()
	pod := volPod("uid-4", []corev1.Volume{v}, []corev1.VolumeMount{m})

	// No TokenRequester: the volume is tolerated but not mounted, matching a node
	// that grants Pods no in-cluster credentials.
	rv, err := resolveVolumes(context.Background(), pod, VolumePolicy{Root: "/x"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("resolveVolumes: %v", err)
	}
	if len(rv.mounts) != 0 || len(rv.configFiles) != 0 {
		t.Errorf("without a token issuer the SA volume must not be materialized; got mounts=%v files=%v", rv.mounts, rv.configFiles)
	}
}

// fakeTokenCreator is a serviceAccountTokenCreator recording its last request.
type fakeTokenCreator struct {
	got *authnv1.TokenRequest
	ns  string
	sa  string
}

func (f *fakeTokenCreator) CreateToken(_ context.Context, namespace, serviceAccountName string, tr *authnv1.TokenRequest, _ metav1.CreateOptions) (*authnv1.TokenRequest, error) {
	f.ns, f.sa, f.got = namespace, serviceAccountName, tr
	out := tr.DeepCopy()
	out.Status.Token = "minted"
	return out, nil
}

func TestClientTokenRequesterBindsAndClampsExpiration(t *testing.T) {
	fc := &fakeTokenCreator{}
	r := NewTokenRequester(fc)
	pod := volPod("uid-5", nil, nil)

	// A sub-minimum expiration is raised to the floor the apiserver enforces.
	tok, err := r.RequestToken(context.Background(), "ns", "default", pod, []string{"api"}, i64(5))
	if err != nil {
		t.Fatalf("RequestToken: %v", err)
	}
	if tok != "minted" {
		t.Errorf("token = %q, want minted", tok)
	}
	if fc.ns != "ns" || fc.sa != "default" {
		t.Errorf("created token for %q/%q, want ns/default", fc.ns, fc.sa)
	}
	if got := fc.got.Spec.ExpirationSeconds; got == nil || *got != minTokenExpirationSeconds {
		t.Errorf("expiration = %v, want clamp to %d", got, minTokenExpirationSeconds)
	}
	ref := fc.got.Spec.BoundObjectRef
	if ref == nil || ref.Kind != "Pod" || ref.UID != pod.UID {
		t.Errorf("bound object ref = %+v, want a Pod binding", ref)
	}
}

func TestClientTokenRequesterDefaultsExpiration(t *testing.T) {
	fc := &fakeTokenCreator{}
	r := NewTokenRequester(fc)
	if _, err := r.RequestToken(context.Background(), "ns", "default", volPod("u", nil, nil), nil, nil); err != nil {
		t.Fatalf("RequestToken: %v", err)
	}
	if got := fc.got.Spec.ExpirationSeconds; got == nil || *got != defaultTokenExpirationSeconds {
		t.Errorf("expiration = %v, want default %d", got, defaultTokenExpirationSeconds)
	}
}

func TestWriteProjectedFilesMaterializesOnDisk(t *testing.T) {
	v, m := apiAccessVolume()
	pod := volPod("uid-6", []corev1.Volume{v}, []corev1.VolumeMount{m})
	root := t.TempDir()
	cms := newFakeConfigMaps(cm("ns", "kube-root-ca.crt", map[string]string{"ca.crt": "PEM"}))

	rv, err := resolveVolumes(context.Background(), pod, VolumePolicy{Root: root}, cms, nil, &fakeTokens{token: "jwt"})
	if err != nil {
		t.Fatalf("resolveVolumes: %v", err)
	}
	if err := (&Provider{}).writeConfigFiles(rv.configFiles); err != nil {
		t.Fatalf("writeConfigFiles: %v", err)
	}
	tokenPath := filepath.Join(root, "uid-6", v.Name, "token")
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if string(data) != "jwt" {
		t.Errorf("token file = %q, want jwt", data)
	}
}
