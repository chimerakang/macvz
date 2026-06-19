package provider

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ConfigMapGetter resolves ConfigMaps referenced by a Pod's env vars and
// volumes (#46). It is satisfied by a namespaced lister over the pod
// controller's ConfigMap informer. Nil disables ConfigMap support.
type ConfigMapGetter interface {
	GetConfigMap(namespace, name string) (*corev1.ConfigMap, error)
}

// WithConfigMapGetter wires the resolver used for ConfigMap-backed env vars and
// volumes (#46).
func WithConfigMapGetter(g ConfigMapGetter) Option {
	return func(p *Provider) { p.configMaps = g }
}

// errConfigPending marks a configuration dependency that is not yet satisfiable
// — a referenced ConfigMap or required key that is not present. CreatePod
// returns it (rather than sticking a terminal Failed status), so Virtual Kubelet
// surfaces a clear Pending status with the message and retries; the Pod
// self-heals once the ConfigMap appears.
var errConfigPending = errors.New("pod configuration not ready")

// getConfigMap fetches a referenced ConfigMap. A missing optional reference
// returns (nil, nil); a missing required one returns an errConfigPending error
// so the Pod waits and retries instead of failing permanently.
func getConfigMap(cms ConfigMapGetter, namespace, name string, optional bool) (*corev1.ConfigMap, error) {
	if cms == nil {
		return nil, fmt.Errorf("configMap %q is referenced but ConfigMap resolution is not configured on this node", name)
	}
	cm, err := cms.GetConfigMap(namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if optional {
				return nil, nil
			}
			return nil, fmt.Errorf("%w: configMap %q not found", errConfigPending, name)
		}
		return nil, fmt.Errorf("%w: get configMap %q: %v", errConfigPending, name, err)
	}
	return cm, nil
}

// resolveEnv builds the full environment for a container, layering ConfigMap,
// Secret, and Downward API sources onto the literal values per Kubernetes
// precedence: envFrom sources (lowest), then explicit env entries (literal,
// configMapKeyRef, secretKeyRef, fieldRef, resourceFieldRef) which override.
// Literal values have $(VAR) references expanded against the variables defined
// before them. Secret sources are honored only once #47 wires a SecretGetter and
// removes the upstream rejection; until then they are unreachable. It returns nil
// when there is no environment to set.
func resolveEnv(pod *corev1.Pod, c corev1.Container, cms ConfigMapGetter, secrets SecretGetter) (map[string]string, error) {
	env := map[string]string{}

	// envFrom: every key of each referenced ConfigMap/Secret, lowest precedence.
	for _, ef := range c.EnvFrom {
		switch {
		case ef.ConfigMapRef != nil:
			optional := ef.ConfigMapRef.Optional != nil && *ef.ConfigMapRef.Optional
			cm, err := getConfigMap(cms, pod.Namespace, ef.ConfigMapRef.Name, optional)
			if err != nil {
				return nil, err
			}
			if cm == nil {
				continue // optional and absent
			}
			for _, k := range sortedKeys(cm.Data) {
				addEnvFromKey(env, ef.Prefix, k, cm.Data[k])
			}
		case ef.SecretRef != nil:
			optional := ef.SecretRef.Optional != nil && *ef.SecretRef.Optional
			sec, err := loadSecret(secrets, pod.Namespace, ef.SecretRef.Name, optional)
			if err != nil {
				return nil, err
			}
			if sec == nil {
				continue // optional and absent
			}
			for _, k := range sortedDataKeys(sec.Data) {
				addEnvFromKey(env, ef.Prefix, k, string(sec.Data[k]))
			}
		}
	}

	// Explicit env entries override envFrom. A literal value has $(VAR)
	// references expanded against the variables defined before it (envFrom and
	// earlier env entries); valueFrom values are used verbatim.
	for _, e := range c.Env {
		switch {
		case e.ValueFrom == nil:
			env[e.Name] = expandEnv(e.Value, env)
		case e.ValueFrom.FieldRef != nil, e.ValueFrom.ResourceFieldRef != nil:
			val, err := resolveDownwardField(pod, c, e.ValueFrom)
			if err != nil {
				return nil, fmt.Errorf("env %q: %w", e.Name, err)
			}
			env[e.Name] = val
		case e.ValueFrom.ConfigMapKeyRef != nil:
			ref := e.ValueFrom.ConfigMapKeyRef
			optional := ref.Optional != nil && *ref.Optional
			cm, err := getConfigMap(cms, pod.Namespace, ref.Name, optional)
			if err != nil {
				return nil, err
			}
			if cm == nil {
				continue // optional ConfigMap absent
			}
			val, ok := cm.Data[ref.Key]
			if !ok {
				if optional {
					continue
				}
				return nil, fmt.Errorf("%w: configMap %q has no key %q (for env %q)", errConfigPending, ref.Name, ref.Key, e.Name)
			}
			env[e.Name] = val
		case e.ValueFrom.SecretKeyRef != nil:
			ref := e.ValueFrom.SecretKeyRef
			optional := ref.Optional != nil && *ref.Optional
			sec, err := loadSecret(secrets, pod.Namespace, ref.Name, optional)
			if err != nil {
				return nil, err
			}
			if sec == nil {
				continue // optional Secret absent
			}
			raw, ok := sec.Data[ref.Key]
			if !ok {
				if optional {
					continue
				}
				return nil, fmt.Errorf("%w: secret %q has no key %q (for env %q)", errSecretUnavailable, ref.Name, ref.Key, e.Name)
			}
			env[e.Name] = string(raw)
		default:
			// An unknown valueFrom source is a defensive skip.
		}
	}

	if len(env) == 0 {
		return nil, nil
	}
	return env, nil
}

// addEnvFromKey sets one envFrom-derived variable, skipping keys that would not
// be a valid environment variable name once prefixed — Kubernetes emits an
// InvalidVariableNames event and skips them, so the Pod still runs.
func addEnvFromKey(env map[string]string, prefix, key, value string) {
	name := prefix + key
	if !isEnvVarName(name) {
		return
	}
	env[name] = value
}

// sortedDataKeys returns a Secret data map's keys in lexical order, so env and
// volume materialization is deterministic regardless of map iteration order.
func sortedDataKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedKeys returns the map's keys in lexical order, so env materialization is
// deterministic regardless of map iteration order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// isEnvVarName reports whether s is a valid environment variable name (a C
// identifier: a letter or underscore followed by letters, digits, or
// underscores), matching Kubernetes' envFrom validation.
func isEnvVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// configMapFileMode is the default permission for a materialized ConfigMap file
// when the volume sets no defaultMode.
const configMapFileMode os.FileMode = 0o644

// fileToWrite is a single file the provider materializes on the host before a
// micro-VM starts, so a projected ConfigMap volume appears as files in the guest.
type fileToWrite struct {
	path string
	mode os.FileMode
	data []byte
}

// configMapVolumeFiles resolves a configMap volume source into the set of files
// to materialize under dir. It honors items (key subset with target paths and
// per-file modes), defaultMode, binaryData, and optional. A missing required
// ConfigMap or item key returns an errConfigPending error; an optional, absent
// ConfigMap yields no files (an empty mounted directory).
func configMapVolumeFiles(namespace string, src *corev1.ConfigMapVolumeSource, dir string, cms ConfigMapGetter) ([]fileToWrite, error) {
	optional := src.Optional != nil && *src.Optional
	cm, err := getConfigMap(cms, namespace, src.Name, optional)
	if err != nil {
		return nil, err
	}

	mode := configMapFileMode
	if src.DefaultMode != nil {
		mode = os.FileMode(*src.DefaultMode) & os.ModePerm
	}
	if cm == nil {
		return nil, nil // optional and absent → empty directory
	}

	var files []fileToWrite
	add := func(relPath string, data []byte, fileMode os.FileMode) error {
		// A key/path may contain slashes (subdirectories); reject any that would
		// escape the volume directory.
		dest, err := safeJoinUnderRoot(dir, strings.Split(relPath, "/")...)
		if err != nil {
			return fmt.Errorf("configMap volume %q path %q: %w", src.Name, relPath, err)
		}
		files = append(files, fileToWrite{path: dest, mode: fileMode, data: data})
		return nil
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
				return nil, fmt.Errorf("%w: configMap %q has no key %q", errConfigPending, src.Name, it.Key)
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

	// No items: project every key to a file named after the key.
	for _, k := range sortedKeys(cm.Data) {
		if err := add(k, []byte(cm.Data[k]), mode); err != nil {
			return nil, err
		}
	}
	bkeys := make([]string, 0, len(cm.BinaryData))
	for k := range cm.BinaryData {
		bkeys = append(bkeys, k)
	}
	sort.Strings(bkeys)
	for _, k := range bkeys {
		if err := add(k, cm.BinaryData[k], mode); err != nil {
			return nil, err
		}
	}
	return files, nil
}
