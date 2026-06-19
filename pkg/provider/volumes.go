package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chimerakang/macvz/internal/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// VolumePolicy governs which Pod volumes this node mounts into micro-VMs and
// where ephemeral storage is backed. The zero value is safe and restrictive:
// hostPath is disabled and no ephemeral root is configured (so any emptyDir use
// is rejected until Root is set).
type VolumePolicy struct {
	// Root is the host directory under which per-Pod emptyDir volumes are
	// created, as "<Root>/<podUID>/<volumeName>".
	Root string
	// HostPathAllowedPrefixes is the allowlist of absolute, cleaned host path
	// prefixes a hostPath volume may resolve under. Empty disables hostPath.
	HostPathAllowedPrefixes []string
}

// resolvedVolumes is the outcome of translating a Pod's volumes: the mounts to
// pass to the runtime, and the host directories that must exist before the VM
// starts (emptyDir backing dirs, created on CreatePod and removed on DeletePod).
type resolvedVolumes struct {
	mounts        []types.Mount
	ephemeralDirs []string
	// configFiles are ConfigMap- and Secret-projected files to materialize under
	// their volume's backing dir before the micro-VM starts (#46/#47).
	configFiles []fileToWrite
}

// resolveVolumes validates every volume a Pod declares and maps the container's
// volumeMounts into runtime mounts. It enforces the volume policy: the only
// supported sources are hostPath (when allowlisted) and emptyDir; anything else
// is rejected with a clear, Kubernetes-facing error. Validating all declared
// volumes — not just mounted ones — keeps an unsupported volume a hard failure
// even when no container references it.
func resolveVolumes(ctx context.Context, pod *corev1.Pod, policy VolumePolicy, cms ConfigMapGetter, secrets SecretGetter, tokens TokenRequester) (resolvedVolumes, error) {
	byName := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		// A projected service-account token volume (the auto-injected
		// kube-api-access) is only materialized when this node has a token issuer
		// configured (#51). Without one it is tolerated but not mounted, preserving
		// the prior behavior on nodes that grant Pods no in-cluster credentials.
		if isDefaultProjectedToken(v) && tokens == nil {
			continue
		}
		if err := validateVolumeSource(v, policy); err != nil {
			return resolvedVolumes{}, err
		}
		byName[v.Name] = v
	}

	if len(pod.Spec.Containers) == 0 {
		return resolvedVolumes{}, nil
	}

	var out resolvedVolumes
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		vol, ok := byName[vm.Name]
		if !ok {
			// The mount references the tolerated SA token (or an unknown volume);
			// the token is mounted by the guest's own tooling, not by MacVz.
			continue
		}
		mount, back, err := resolveMount(ctx, pod, vol, vm, policy, cms, secrets, tokens)
		if err != nil {
			return resolvedVolumes{}, err
		}
		out.mounts = append(out.mounts, mount)
		if back.dir != "" {
			out.ephemeralDirs = append(out.ephemeralDirs, back.dir)
		}
		out.configFiles = append(out.configFiles, back.files...)
	}
	return out, nil
}

// validateVolumeSource confirms a declared volume is a type MacVz supports and,
// for hostPath, that its path is allowlisted.
func validateVolumeSource(v corev1.Volume, policy VolumePolicy) error {
	switch {
	case v.HostPath != nil:
		if len(policy.HostPathAllowedPrefixes) == 0 {
			return fmt.Errorf("volume %q is a hostPath, which is disabled on this node (set node.volumes.hostPathAllowedPrefixes to allow specific prefixes)", v.Name)
		}
		if err := validateHostPath(v.Name, v.HostPath, policy); err != nil {
			return err
		}
		return nil
	case v.EmptyDir != nil:
		if policy.Root == "" {
			return fmt.Errorf("volume %q is an emptyDir, but no ephemeral volume root is configured (set node.volumes.root)", v.Name)
		}
		if !filepath.IsAbs(policy.Root) {
			return fmt.Errorf("volume %q is an emptyDir, but node.volumes.root %q is not absolute", v.Name, policy.Root)
		}
		return nil
	case v.ConfigMap != nil:
		// ConfigMap volumes are materialized as files under the ephemeral root
		// before the VM starts (#46), so they need the same backing storage as an
		// emptyDir.
		if policy.Root == "" {
			return fmt.Errorf("volume %q is a configMap, but no volume root is configured to back it (set node.volumes.root)", v.Name)
		}
		if !filepath.IsAbs(policy.Root) {
			return fmt.Errorf("volume %q is a configMap, but node.volumes.root %q is not absolute", v.Name, policy.Root)
		}
		return nil
	case v.Secret != nil:
		// Secret volumes are materialized as files under the ephemeral root before
		// the VM starts (#47), so they need the same backing storage as a configMap.
		if policy.Root == "" {
			return fmt.Errorf("volume %q is a secret, but no volume root is configured to back it (set node.volumes.root)", v.Name)
		}
		if !filepath.IsAbs(policy.Root) {
			return fmt.Errorf("volume %q is a secret, but node.volumes.root %q is not absolute", v.Name, policy.Root)
		}
		return nil
	case v.Projected != nil:
		// Projected volumes (the auto-injected kube-api-access token, CA, and
		// namespace) are materialized as files under the ephemeral root before the
		// VM starts (#51), so they need the same backing storage as a configMap.
		if policy.Root == "" {
			return fmt.Errorf("volume %q is projected, but no volume root is configured to back it (set node.volumes.root)", v.Name)
		}
		if !filepath.IsAbs(policy.Root) {
			return fmt.Errorf("volume %q is projected, but node.volumes.root %q is not absolute", v.Name, policy.Root)
		}
		return nil
	default:
		return fmt.Errorf("volume %q uses an unsupported source type (only hostPath, emptyDir, configMap, secret, and projected are supported in the beta)", v.Name)
	}
}

// validateHostPath enforces the hostPath security policy: an absolute, cleaned
// path within an allowed prefix, of a supported (directory) type.
func validateHostPath(name string, hp *corev1.HostPathVolumeSource, policy VolumePolicy) error {
	if !filepath.IsAbs(hp.Path) {
		return fmt.Errorf("hostPath volume %q path %q must be absolute", name, hp.Path)
	}
	clean := filepath.Clean(hp.Path)

	switch t := hostPathType(hp); t {
	case corev1.HostPathUnset, corev1.HostPathDirectory, corev1.HostPathDirectoryOrCreate:
		// supported
	default:
		return fmt.Errorf("hostPath volume %q type %q is not supported (only directory types are supported in the beta)", name, t)
	}

	if !withinAllowedPrefix(clean, policy.HostPathAllowedPrefixes) {
		return fmt.Errorf("hostPath volume %q path %q is not within an allowed prefix %v", name, clean, policy.HostPathAllowedPrefixes)
	}
	return nil
}

// backing is the host storage a mount needs prepared before the micro-VM starts:
// a directory to create (emptyDir, configMap) and, for a configMap, the files to
// materialize inside it. A zero backing means the mount needs no preparation
// (hostPath, or a tmpfs emptyDir).
type backing struct {
	dir   string
	files []fileToWrite
}

// resolveMount builds the runtime mount for one volumeMount, plus the host
// storage that must be prepared before the VM starts.
func resolveMount(ctx context.Context, pod *corev1.Pod, vol corev1.Volume, vm corev1.VolumeMount, policy VolumePolicy, cms ConfigMapGetter, secrets SecretGetter, tokens TokenRequester) (types.Mount, backing, error) {
	if !filepath.IsAbs(vm.MountPath) {
		return types.Mount{}, backing{}, fmt.Errorf("volume %q mountPath %q must be absolute", vm.Name, vm.MountPath)
	}
	// subPath needs careful traversal handling; defer it rather than mount the
	// wrong directory.
	if vm.SubPath != "" || vm.SubPathExpr != "" {
		return types.Mount{}, backing{}, fmt.Errorf("volume %q uses subPath, which is not supported yet", vm.Name)
	}
	target := filepath.Clean(vm.MountPath)

	switch {
	case vol.HostPath != nil:
		return types.Mount{
			Source:   filepath.Clean(vol.HostPath.Path),
			Target:   target,
			ReadOnly: vm.ReadOnly,
		}, backing{}, nil

	case vol.EmptyDir != nil:
		// A Memory-medium emptyDir is guest-local tmpfs with no host backing.
		if vol.EmptyDir.Medium == corev1.StorageMediumMemory {
			return types.Mount{Target: target, Tmpfs: true}, backing{}, nil
		}
		dir, err := safeVolumeDir(policy.Root, string(pod.UID), vm.Name)
		if err != nil {
			return types.Mount{}, backing{}, err
		}
		return types.Mount{
			Source:   dir,
			Target:   target,
			ReadOnly: vm.ReadOnly,
		}, backing{dir: dir}, nil

	case vol.ConfigMap != nil:
		// Materialize the ConfigMap's keys as files under a per-Pod dir, then bind
		// it read-only into the guest (#46). Kubernetes always mounts projected
		// ConfigMap volumes read-only.
		dir, err := safeVolumeDir(policy.Root, string(pod.UID), vm.Name)
		if err != nil {
			return types.Mount{}, backing{}, err
		}
		files, err := configMapVolumeFiles(pod.Namespace, vol.ConfigMap, dir, cms)
		if err != nil {
			return types.Mount{}, backing{}, err
		}
		return types.Mount{
			Source:   dir,
			Target:   target,
			ReadOnly: true,
		}, backing{dir: dir, files: files}, nil

	case vol.Secret != nil:
		// Materialize the Secret's keys as files under a per-Pod dir, then bind it
		// read-only into the guest (#47). Kubernetes always mounts projected Secret
		// volumes read-only; keeping them off the runtime's writable path also avoids
		// leaking values back through a shared mount.
		dir, err := safeVolumeDir(policy.Root, string(pod.UID), vm.Name)
		if err != nil {
			return types.Mount{}, backing{}, err
		}
		files, err := secretVolumeFiles(pod.Namespace, vol.Secret, dir, secrets)
		if err != nil {
			return types.Mount{}, backing{}, err
		}
		return types.Mount{
			Source:   dir,
			Target:   target,
			ReadOnly: true,
		}, backing{dir: dir, files: files}, nil

	case vol.Projected != nil:
		// Materialize the projected sources (service-account token, cluster CA, and
		// namespace for the auto-injected kube-api-access volume) as files under a
		// per-Pod dir, then bind it read-only into the guest (#51). Kubernetes always
		// mounts projected volumes read-only.
		dir, err := safeVolumeDir(policy.Root, string(pod.UID), vm.Name)
		if err != nil {
			return types.Mount{}, backing{}, err
		}
		files, err := projectedVolumeFiles(ctx, pod, vol.Projected, dir, cms, secrets, tokens)
		if err != nil {
			return types.Mount{}, backing{}, err
		}
		return types.Mount{
			Source:   dir,
			Target:   target,
			ReadOnly: true,
		}, backing{dir: dir, files: files}, nil

	default:
		// validateVolumeSource already rejected other types; defensive.
		return types.Mount{}, backing{}, fmt.Errorf("volume %q uses an unsupported source type", vm.Name)
	}
}

// withinAllowedPrefix reports whether clean (an absolute, cleaned path) is at or
// below one of the allowed prefixes. Matching is path-segment aware so
// "/data" does not admit "/database".
func withinAllowedPrefix(clean string, prefixes []string) bool {
	for _, p := range prefixes {
		p = filepath.Clean(p)
		if clean == p || strings.HasPrefix(clean, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// safePodRoot returns the per-Pod volume directory, rejecting empty or escaping
// identities before any filesystem operation can target the volume root itself
// or a path outside it.
func safePodRoot(root, podUID string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("emptyDir volume root is not configured")
	}
	if podUID == "" {
		return "", fmt.Errorf("emptyDir volumes require a non-empty Pod UID")
	}
	return safeJoinUnderRoot(root, podUID)
}

func safeVolumeDir(root, podUID, volumeName string) (string, error) {
	if volumeName == "" {
		return "", fmt.Errorf("emptyDir volume name must not be empty")
	}
	podRoot, err := safePodRoot(root, podUID)
	if err != nil {
		return "", err
	}
	return safeJoinUnderRoot(podRoot, volumeName)
}

func safeJoinUnderRoot(root string, elems ...string) (string, error) {
	cleanRoot := filepath.Clean(root)
	if !filepath.IsAbs(cleanRoot) {
		return "", fmt.Errorf("volume root %q must be absolute", root)
	}
	parts := append([]string{cleanRoot}, elems...)
	joined := filepath.Clean(filepath.Join(parts...))
	rel, err := filepath.Rel(cleanRoot, joined)
	if err != nil {
		return "", fmt.Errorf("resolve volume path %q under %q: %w", joined, cleanRoot, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("resolved volume path %q escapes volume root %q", joined, cleanRoot)
	}
	return joined, nil
}

// hostPathType returns the volume's hostPath type, defaulting to unset.
func hostPathType(hp *corev1.HostPathVolumeSource) corev1.HostPathType {
	if hp.Type == nil {
		return corev1.HostPathUnset
	}
	return *hp.Type
}

// ensureVolumeDirs creates the host backing directories for a Pod's emptyDir
// volumes before its micro-VM starts. The 0o770 mode keeps the share private to
// the node owner and the runtime group.
func (p *Provider) ensureVolumeDirs(dirs []string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o770); err != nil {
			return fmt.Errorf("create ephemeral volume dir %q: %w", d, err)
		}
	}
	return nil
}

// writeConfigFiles materializes ConfigMap-projected files on the host before the
// micro-VM starts (#46). Each file's parent directory is created (a projected
// key may sit in a subdirectory), and an explicit Chmod follows WriteFile so the
// requested mode is not narrowed by the process umask.
func (p *Provider) writeConfigFiles(files []fileToWrite) error {
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o770); err != nil {
			return fmt.Errorf("create configMap volume dir for %q: %w", f.path, err)
		}
		if err := os.WriteFile(f.path, f.data, f.mode); err != nil {
			return fmt.Errorf("write configMap volume file %q: %w", f.path, err)
		}
		if err := os.Chmod(f.path, f.mode); err != nil {
			return fmt.Errorf("set mode on configMap volume file %q: %w", f.path, err)
		}
	}
	return nil
}

// cleanupVolumeDirs removes a Pod's ephemeral volume tree. It is best-effort:
// a failure to remove scratch storage is logged, not surfaced, so it never
// blocks Pod deletion. hostPath sources are never touched.
func (p *Provider) cleanupVolumeDirs(pod *corev1.Pod) {
	root, err := safePodRoot(p.volumes.Root, string(pod.UID))
	if err != nil {
		klog.ErrorS(err, "refusing to remove Pod ephemeral volume dir", "pod", podKey(pod.Namespace, pod.Name))
		return
	}
	if err := os.RemoveAll(root); err != nil {
		klog.ErrorS(err, "failed to remove Pod ephemeral volume dir", "path", root, "pod", podKey(pod.Namespace, pod.Name))
	}
}
