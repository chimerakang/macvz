package provider

import (
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

// podRoot returns the host directory holding this Pod's ephemeral volumes.
func (vp VolumePolicy) podRoot(podUID string) string {
	if vp.Root == "" {
		return ""
	}
	return filepath.Join(vp.Root, podUID)
}

// resolvedVolumes is the outcome of translating a Pod's volumes: the mounts to
// pass to the runtime, and the host directories that must exist before the VM
// starts (emptyDir backing dirs, created on CreatePod and removed on DeletePod).
type resolvedVolumes struct {
	mounts        []types.Mount
	ephemeralDirs []string
}

// resolveVolumes validates every volume a Pod declares and maps the container's
// volumeMounts into runtime mounts. It enforces the volume policy: the only
// supported sources are hostPath (when allowlisted) and emptyDir; anything else
// is rejected with a clear, Kubernetes-facing error. Validating all declared
// volumes — not just mounted ones — keeps an unsupported volume a hard failure
// even when no container references it.
func resolveVolumes(pod *corev1.Pod, policy VolumePolicy) (resolvedVolumes, error) {
	byName := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		// The auto-injected service-account token is tolerated but never mounted.
		if isDefaultProjectedToken(v) {
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
		mount, ephemeral, err := resolveMount(pod, vol, vm, policy)
		if err != nil {
			return resolvedVolumes{}, err
		}
		out.mounts = append(out.mounts, mount)
		if ephemeral != "" {
			out.ephemeralDirs = append(out.ephemeralDirs, ephemeral)
		}
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
		return nil
	default:
		return fmt.Errorf("volume %q uses an unsupported source type (only hostPath and emptyDir are supported in the beta)", v.Name)
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

// resolveMount builds the runtime mount for one volumeMount, returning the host
// directory to pre-create when the volume is a disk-backed emptyDir.
func resolveMount(pod *corev1.Pod, vol corev1.Volume, vm corev1.VolumeMount, policy VolumePolicy) (types.Mount, string, error) {
	if !filepath.IsAbs(vm.MountPath) {
		return types.Mount{}, "", fmt.Errorf("volume %q mountPath %q must be absolute", vm.Name, vm.MountPath)
	}
	// subPath needs careful traversal handling; defer it rather than mount the
	// wrong directory.
	if vm.SubPath != "" || vm.SubPathExpr != "" {
		return types.Mount{}, "", fmt.Errorf("volume %q uses subPath, which is not supported yet", vm.Name)
	}
	target := filepath.Clean(vm.MountPath)

	switch {
	case vol.HostPath != nil:
		return types.Mount{
			Source:   filepath.Clean(vol.HostPath.Path),
			Target:   target,
			ReadOnly: vm.ReadOnly,
		}, "", nil

	case vol.EmptyDir != nil:
		// A Memory-medium emptyDir is guest-local tmpfs with no host backing.
		if vol.EmptyDir.Medium == corev1.StorageMediumMemory {
			return types.Mount{Target: target, Tmpfs: true}, "", nil
		}
		dir := filepath.Join(policy.podRoot(string(pod.UID)), vm.Name)
		return types.Mount{
			Source:   dir,
			Target:   target,
			ReadOnly: vm.ReadOnly,
		}, dir, nil

	default:
		// validateVolumeSource already rejected other types; defensive.
		return types.Mount{}, "", fmt.Errorf("volume %q uses an unsupported source type", vm.Name)
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

// cleanupVolumeDirs removes a Pod's ephemeral volume tree. It is best-effort:
// a failure to remove scratch storage is logged, not surfaced, so it never
// blocks Pod deletion. hostPath sources are never touched.
func (p *Provider) cleanupVolumeDirs(pod *corev1.Pod) {
	root := p.volumes.podRoot(string(pod.UID))
	if root == "" {
		return
	}
	if err := os.RemoveAll(root); err != nil {
		klog.ErrorS(err, "failed to remove Pod ephemeral volume dir", "path", root, "pod", podKey(pod.Namespace, pod.Name))
	}
}
