package provider

import (
	"errors"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// errSecretUnavailable marks a Secret-resolution failure that may clear on its
// own once the referenced Secret (or key) appears: a missing, non-optional
// Secret reference. Callers return it as a transient error so Virtual Kubelet
// keeps the Pod Pending and retries — matching Kubernetes, which leaves such
// Pods Pending — rather than recording a sticky terminal failure.
var errSecretUnavailable = errors.New("secret unavailable")

// SecretGetter reads Secrets so the provider can resolve Secret-backed
// configuration: today, the dockerconfigjson pull secrets a Pod names in
// imagePullSecrets (#49). It is satisfied by a view over the shared informer's
// SecretLister; a nil getter disables Secret support, so any Pod that references
// a Secret fails to start with a clear error.
type SecretGetter interface {
	GetSecret(namespace, name string) (*corev1.Secret, error)
}

// NewSecretLister adapts a client-go SecretLister to the SecretGetter the
// provider consumes, so Secret reads are served from the informer cache without
// extra API calls.
func NewSecretLister(lister corev1listers.SecretLister) SecretGetter {
	return listerSecretGetter{lister: lister}
}

type listerSecretGetter struct{ lister corev1listers.SecretLister }

func (g listerSecretGetter) GetSecret(namespace, name string) (*corev1.Secret, error) {
	return g.lister.Secrets(namespace).Get(name)
}

// loadSecret fetches a Secret, returning (nil, nil) when it is absent and
// optional, and errSecretUnavailable when it is absent but required. A nil getter
// means Secret support was not wired into this node.
func loadSecret(getter SecretGetter, namespace, name string, optional bool) (*corev1.Secret, error) {
	if getter == nil {
		return nil, fmt.Errorf("secret %q is referenced but this node has no Secret access configured", name)
	}
	sec, err := getter.GetSecret(namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if optional {
				return nil, nil
			}
			return nil, fmt.Errorf("%w: secret %q not found", errSecretUnavailable, name)
		}
		return nil, fmt.Errorf("read secret %q: %w", name, err)
	}
	return sec, nil
}

// secretFileMode is the default permission for a materialized Secret file when
// the volume sets no defaultMode, matching Kubernetes.
const secretFileMode os.FileMode = 0o644

// secretVolumeFiles resolves a secret volume source into the set of files to
// materialize under dir. It honors items (key subset with target paths and
// per-file modes), defaultMode, and optional. A missing required Secret or item
// key returns an errSecretUnavailable error so the Pod waits and retries; an
// optional, absent Secret yields no files (an empty mounted directory). Secret
// values are never placed in the errors this returns, and every returned path is
// confined to dir.
func secretVolumeFiles(namespace string, src *corev1.SecretVolumeSource, dir string, secrets SecretGetter) ([]fileToWrite, error) {
	optional := src.Optional != nil && *src.Optional
	sec, err := loadSecret(secrets, namespace, src.SecretName, optional)
	if err != nil {
		return nil, err
	}

	mode := secretFileMode
	if src.DefaultMode != nil {
		mode = os.FileMode(*src.DefaultMode) & os.ModePerm
	}
	if sec == nil {
		return nil, nil // optional and absent → empty directory
	}

	var files []fileToWrite
	add := func(relPath string, data []byte, fileMode os.FileMode) error {
		// A key/path may contain slashes (subdirectories); reject any that would
		// escape the volume directory.
		dest, err := safeJoinUnderRoot(dir, strings.Split(relPath, "/")...)
		if err != nil {
			return fmt.Errorf("secret volume %q path %q: %w", src.SecretName, relPath, err)
		}
		files = append(files, fileToWrite{path: dest, mode: fileMode, data: data})
		return nil
	}

	if len(src.Items) > 0 {
		for _, it := range src.Items {
			data, ok := sec.Data[it.Key]
			if !ok {
				if optional {
					continue
				}
				return nil, fmt.Errorf("%w: secret %q has no key %q", errSecretUnavailable, src.SecretName, it.Key)
			}
			fileMode := mode
			if it.Mode != nil {
				fileMode = os.FileMode(*it.Mode) & os.ModePerm
			}
			if err := add(it.Path, data, fileMode); err != nil {
				return nil, err
			}
		}
		return files, nil
	}

	// No items: project every key to a file named after the key, in a stable
	// order so materialization is deterministic.
	for _, k := range sortedDataKeys(sec.Data) {
		if err := add(k, sec.Data[k], mode); err != nil {
			return nil, err
		}
	}
	return files, nil
}
