package provider

import (
	"context"
	"fmt"
	"os"
	"strings"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// projectedFileMode is the default permission for a file materialized from a
// projected volume source when the volume sets no defaultMode, matching
// Kubernetes (0644).
const projectedFileMode os.FileMode = 0o644

// defaultTokenExpirationSeconds is the lifetime requested for a projected
// service-account token when the projection itself names none. It matches the
// kube-apiserver's own default (1h) requested ceiling for bound tokens.
//
// MacVz materializes the token once, when the Pod's micro-VM is created, and
// does not rotate the on-disk file afterward (a running micro-VM holds the file
// read-only). The token therefore stays valid for this lifetime; long-lived
// workloads that outlive it must tolerate re-issue on Pod recreation. This is
// the documented lifetime behavior called for by issue #51.
const defaultTokenExpirationSeconds int64 = 3600

// minTokenExpirationSeconds is the floor MacVz requests, mirroring the
// kube-apiserver minimum (10m); a projection asking for less is raised to it so
// the API server does not reject the request.
const minTokenExpirationSeconds int64 = 600

// TokenRequester issues a bound service-account token for a Pod, as the
// kubelet does for the projected `serviceAccountToken` volume source (#51). It
// is satisfied by a clientset-backed implementation; a nil requester disables
// projected-token materialization, in which case the auto-injected
// kube-api-access volume is tolerated but not mounted.
type TokenRequester interface {
	// RequestToken returns a signed token for the named ServiceAccount, bound to
	// the Pod, for the given audiences and expiration. A nil expiration uses the
	// node default.
	RequestToken(ctx context.Context, namespace, serviceAccountName string, pod *corev1.Pod, audiences []string, expirationSeconds *int64) (token string, err error)
}

// clientTokenRequester satisfies TokenRequester via the TokenRequest API
// (ServiceAccounts.CreateToken), the same subresource the real kubelet uses.
type clientTokenRequester struct {
	core serviceAccountTokenCreator
}

// serviceAccountTokenCreator mints a bound token for a namespaced
// ServiceAccount. It is the one call MacVz needs from the TokenRequest API,
// isolated behind an interface so the requester is unit-testable without a full
// clientset. The kubelet's adapter forwards to
// clientset.CoreV1().ServiceAccounts(namespace).CreateToken.
type serviceAccountTokenCreator interface {
	CreateToken(ctx context.Context, namespace, serviceAccountName string, tokenRequest *authnv1.TokenRequest, opts metav1.CreateOptions) (*authnv1.TokenRequest, error)
}

// NewTokenRequester wires the projected service-account token issuer to a
// namespaced token creator. It is used by the kubelet to enable in-cluster API
// access for Pods (#51).
func NewTokenRequester(core serviceAccountTokenCreator) TokenRequester {
	return &clientTokenRequester{core: core}
}

func (c *clientTokenRequester) RequestToken(ctx context.Context, namespace, serviceAccountName string, pod *corev1.Pod, audiences []string, expirationSeconds *int64) (string, error) {
	exp := defaultTokenExpirationSeconds
	if expirationSeconds != nil {
		exp = *expirationSeconds
	}
	if exp < minTokenExpirationSeconds {
		exp = minTokenExpirationSeconds
	}
	tr := &authnv1.TokenRequest{
		Spec: authnv1.TokenRequestSpec{
			Audiences:         audiences,
			ExpirationSeconds: &exp,
		},
	}
	// Bind the token to the Pod so it is invalidated when the Pod is deleted,
	// exactly as the real kubelet does. A bound token cannot outlive its Pod.
	if pod != nil && pod.UID != "" {
		tr.Spec.BoundObjectRef = &authnv1.BoundObjectReference{
			Kind:       "Pod",
			APIVersion: "v1",
			Name:       pod.Name,
			UID:        pod.UID,
		}
	}
	out, err := c.core.CreateToken(ctx, namespace, serviceAccountName, tr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("request service-account token for %q: %w", serviceAccountName, err)
	}
	return out.Status.Token, nil
}

// projectedVolumeFiles resolves a projected volume source into the files to
// materialize under dir, covering the three sources that make up the
// auto-injected kube-api-access volume — serviceAccountToken, the cluster CA
// ConfigMap, and the downward-API namespace — plus projected ConfigMap and
// Secret sources. Each file's mode defaults to the volume's defaultMode (0644
// when unset) unless the source item overrides it. Every returned path is
// confined to dir.
func projectedVolumeFiles(ctx context.Context, pod *corev1.Pod, src *corev1.ProjectedVolumeSource, dir string, cms ConfigMapGetter, secrets SecretGetter, tokens TokenRequester) ([]fileToWrite, error) {
	mode := projectedFileMode
	if src.DefaultMode != nil {
		mode = os.FileMode(*src.DefaultMode) & os.ModePerm
	}

	var files []fileToWrite
	add := func(relPath string, data []byte, fileMode os.FileMode) error {
		dest, err := safeJoinUnderRoot(dir, strings.Split(relPath, "/")...)
		if err != nil {
			return fmt.Errorf("projected volume path %q: %w", relPath, err)
		}
		files = append(files, fileToWrite{path: dest, mode: fileMode, data: data})
		return nil
	}

	for _, proj := range src.Sources {
		switch {
		case proj.ServiceAccountToken != nil:
			sat := proj.ServiceAccountToken
			if tokens == nil {
				return nil, fmt.Errorf("pod %q projects a service-account token but this node has no token issuer configured", pod.Name)
			}
			var audiences []string
			if sat.Audience != "" {
				audiences = []string{sat.Audience}
			}
			token, err := tokens.RequestToken(ctx, pod.Namespace, serviceAccountName(pod), pod, audiences, sat.ExpirationSeconds)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", errConfigPending, err)
			}
			if err := add(sat.Path, []byte(token), mode); err != nil {
				return nil, err
			}
		case proj.ConfigMap != nil:
			if err := projectedConfigMapFiles(pod.Namespace, proj.ConfigMap, add, mode, cms); err != nil {
				return nil, err
			}
		case proj.Secret != nil:
			if err := projectedSecretFiles(pod.Namespace, proj.Secret, add, mode, secrets); err != nil {
				return nil, err
			}
		case proj.DownwardAPI != nil:
			if err := projectedDownwardFiles(pod, proj.DownwardAPI, add, mode); err != nil {
				return nil, err
			}
		default:
			// clusterTrustBundle and other future sources are not honored yet; skip
			// rather than fail so the rest of the volume still materializes.
		}
	}
	return files, nil
}

// projectedConfigMapFiles materializes a projected ConfigMap source (the cluster
// CA in the default volume). Unlike a standalone ConfigMap volume it has no
// defaultMode of its own, so the projected volume's mode is used.
func projectedConfigMapFiles(namespace string, src *corev1.ConfigMapProjection, add func(string, []byte, os.FileMode) error, mode os.FileMode, cms ConfigMapGetter) error {
	optional := src.Optional != nil && *src.Optional
	cm, err := getConfigMap(cms, namespace, src.Name, optional)
	if err != nil {
		return err
	}
	if cm == nil {
		return nil // optional and absent
	}
	keyData := func(key string) ([]byte, bool) {
		if v, ok := cm.Data[key]; ok {
			return []byte(v), true
		}
		if v, ok := cm.BinaryData[key]; ok {
			return v, true
		}
		return nil, false
	}
	if len(src.Items) > 0 {
		for _, it := range src.Items {
			data, ok := keyData(it.Key)
			if !ok {
				if optional {
					continue
				}
				return fmt.Errorf("%w: configMap %q has no key %q", errConfigPending, src.Name, it.Key)
			}
			fileMode := mode
			if it.Mode != nil {
				fileMode = os.FileMode(*it.Mode) & os.ModePerm
			}
			if err := add(it.Path, data, fileMode); err != nil {
				return err
			}
		}
		return nil
	}
	for _, k := range sortedKeys(cm.Data) {
		if err := add(k, []byte(cm.Data[k]), mode); err != nil {
			return err
		}
	}
	return nil
}

// projectedSecretFiles materializes a projected Secret source using the
// projected volume's mode (a projected Secret source carries no defaultMode).
func projectedSecretFiles(namespace string, src *corev1.SecretProjection, add func(string, []byte, os.FileMode) error, mode os.FileMode, secrets SecretGetter) error {
	optional := src.Optional != nil && *src.Optional
	sec, err := loadSecret(secrets, namespace, src.Name, optional)
	if err != nil {
		return err
	}
	if sec == nil {
		return nil // optional and absent
	}
	if len(src.Items) > 0 {
		for _, it := range src.Items {
			data, ok := sec.Data[it.Key]
			if !ok {
				if optional {
					continue
				}
				return fmt.Errorf("%w: secret %q has no key %q", errSecretUnavailable, src.Name, it.Key)
			}
			fileMode := mode
			if it.Mode != nil {
				fileMode = os.FileMode(*it.Mode) & os.ModePerm
			}
			if err := add(it.Path, data, fileMode); err != nil {
				return err
			}
		}
		return nil
	}
	for _, k := range sortedDataKeys(sec.Data) {
		if err := add(k, sec.Data[k], mode); err != nil {
			return err
		}
	}
	return nil
}

// projectedDownwardFiles materializes a projected downward-API source (the
// namespace file in the default volume). Only Pod-stable fieldRef paths are
// supported; status.* fields, unknown at translation time, are rejected.
func projectedDownwardFiles(pod *corev1.Pod, src *corev1.DownwardAPIProjection, add func(string, []byte, os.FileMode) error, mode os.FileMode) error {
	for _, it := range src.Items {
		if it.FieldRef == nil {
			return fmt.Errorf("downwardAPI projected item %q has no fieldRef (resourceFieldRef is not supported in projected volumes yet)", it.Path)
		}
		val, err := resolveFieldRef(pod, it.FieldRef.FieldPath)
		if err != nil {
			return err
		}
		fileMode := mode
		if it.Mode != nil {
			fileMode = os.FileMode(*it.Mode) & os.ModePerm
		}
		if err := add(it.Path, []byte(val), fileMode); err != nil {
			return err
		}
	}
	return nil
}

// serviceAccountName returns the Pod's effective ServiceAccount name, defaulting
// to "default" as Kubernetes does when the Pod names none.
func serviceAccountName(pod *corev1.Pod) string {
	if pod.Spec.ServiceAccountName != "" {
		return pod.Spec.ServiceAccountName
	}
	return "default"
}
