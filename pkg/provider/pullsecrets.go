package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
)

// defaultRegistryHost is the implied registry for an image reference that names
// no registry (e.g. "alpine" or "library/nginx"): Docker Hub. dockerconfigjson
// secrets conventionally key Docker Hub under "https://index.docker.io/v1/", so
// matching treats the several Docker Hub spellings as equivalent.
const defaultRegistryHost = "docker.io"

// dockerConfigJSON is the structure of a kubernetes.io/dockerconfigjson Secret's
// .dockerconfigjson value: registry host -> credential entry under "auths".
type dockerConfigJSON struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

// dockerAuthEntry is one registry credential. Kubernetes accepts the credential
// as explicit username/password, as a base64 "user:password" in auth, or both;
// password and auth carry the secret material.
type dockerAuthEntry struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

// resolvePullAuth selects the registry credential for image from the Pod's
// imagePullSecrets (#49). It returns:
//   - (nil, nil) when the Pod names no pull secrets, or none holds a credential
//     for the image's registry: the caller then pulls anonymously.
//   - (auth, nil) for the first pull secret that matches the image's registry.
//   - (nil, err) when a named Secret is missing (a transient errSecretUnavailable,
//     so the Pod waits and retries) or is not a usable docker pull secret.
//
// Credentials are never placed in the returned errors.
func resolvePullAuth(getter SecretGetter, pod *corev1.Pod, image string) (*runtime.RegistryAuth, error) {
	if len(pod.Spec.ImagePullSecrets) == 0 {
		return nil, nil
	}
	host := registryHost(image)
	for _, ref := range pod.Spec.ImagePullSecrets {
		if ref.Name == "" {
			continue
		}
		sec, err := loadSecret(getter, pod.Namespace, ref.Name, false)
		if err != nil {
			return nil, err
		}
		auths, err := parsePullSecret(sec)
		if err != nil {
			return nil, fmt.Errorf("imagePullSecret %q: %w", ref.Name, err)
		}
		auth, err := credentialsFor(auths, host)
		if err != nil {
			return nil, fmt.Errorf("imagePullSecret %q: %w", ref.Name, err)
		}
		if auth != nil {
			return auth, nil
		}
	}
	return nil, nil
}

// parsePullSecret reads the docker credentials from a pull Secret, accepting both
// the modern kubernetes.io/dockerconfigjson form and the legacy
// kubernetes.io/dockercfg form. The Secret's declared type is not enforced (some
// tooling stores the data under an Opaque type); the presence of a recognized
// key is what matters.
func parsePullSecret(sec *corev1.Secret) (map[string]dockerAuthEntry, error) {
	if data, ok := sec.Data[corev1.DockerConfigJsonKey]; ok {
		var cfg dockerConfigJSON
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", corev1.DockerConfigJsonKey, err)
		}
		if len(cfg.Auths) == 0 {
			return nil, fmt.Errorf("%s has no %q entries", corev1.DockerConfigJsonKey, "auths")
		}
		return cfg.Auths, nil
	}
	if data, ok := sec.Data[corev1.DockerConfigKey]; ok {
		// Legacy dockercfg: a top-level map of registry host to credential entry,
		// with no "auths" wrapper.
		var auths map[string]dockerAuthEntry
		if err := json.Unmarshal(data, &auths); err != nil {
			return nil, fmt.Errorf("parse %s: %w", corev1.DockerConfigKey, err)
		}
		if len(auths) == 0 {
			return nil, fmt.Errorf("%s has no registry entries", corev1.DockerConfigKey)
		}
		return auths, nil
	}
	return nil, fmt.Errorf("secret is not a docker pull secret (missing %s or %s)", corev1.DockerConfigJsonKey, corev1.DockerConfigKey)
}

// credentialsFor returns the credential whose registry key matches host, or
// (nil, nil) when none matches. A matched-but-unusable entry (no username/password
// and no decodable auth) is an error: the user clearly intended a credential for
// this registry, so falling back to an anonymous pull would mask the mistake.
func credentialsFor(auths map[string]dockerAuthEntry, host string) (*runtime.RegistryAuth, error) {
	for key, entry := range auths {
		if !registryMatches(host, normalizeRegistryKey(key)) {
			continue
		}
		user, pass, err := entry.credentials()
		if err != nil {
			return nil, fmt.Errorf("registry %q: %w", key, err)
		}
		return &runtime.RegistryAuth{Server: host, Username: user, Password: pass}, nil
	}
	return nil, nil
}

// credentials extracts the username and password from a docker auth entry,
// preferring the explicit fields and falling back to the base64 "user:password"
// auth field.
func (e dockerAuthEntry) credentials() (string, string, error) {
	if e.Username != "" || e.Password != "" {
		return e.Username, e.Password, nil
	}
	if e.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(e.Auth))
		if err != nil {
			return "", "", fmt.Errorf("decode auth field: %w", err)
		}
		user, pass, ok := strings.Cut(string(decoded), ":")
		if !ok {
			return "", "", fmt.Errorf("auth field is not in user:password form")
		}
		return user, pass, nil
	}
	return "", "", fmt.Errorf("entry has neither username/password nor auth")
}

// registryHost extracts the registry host from an OCI image reference, returning
// defaultRegistryHost when the reference names no registry. The first
// slash-separated component is the registry only when it looks like a host: it
// contains a "." or ":" or is "localhost". Otherwise the reference is a Docker
// Hub short name like "library/nginx" or "nginx".
func registryHost(image string) string {
	slash := strings.IndexByte(image, '/')
	if slash < 0 {
		return defaultRegistryHost
	}
	first := image[:slash]
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return first
	}
	return defaultRegistryHost
}

// normalizeRegistryKey reduces a dockerconfigjson registry key to a bare host[:port]
// by stripping any scheme and path, so "https://index.docker.io/v1/" and
// "index.docker.io" compare equal.
func normalizeRegistryKey(key string) string {
	k := key
	if i := strings.Index(k, "://"); i >= 0 {
		k = k[i+3:]
	}
	if i := strings.IndexByte(k, '/'); i >= 0 {
		k = k[:i]
	}
	return k
}

// registryMatches reports whether a normalized credential key applies to the
// image's registry host. Exact host[:port] equality matches any registry; the
// several spellings of Docker Hub are additionally treated as equivalent.
func registryMatches(host, key string) bool {
	if host == key {
		return true
	}
	return isDockerHub(host) && isDockerHub(key)
}

// isDockerHub reports whether host is one of the interchangeable Docker Hub
// registry spellings.
func isDockerHub(host string) bool {
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return true
	default:
		return false
	}
}
