package runtime

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/chimerakang/macvz/internal/types"
)

// handoffmount.go injects the runtime-private handoff bind mount into a
// late-rootfs container's spec (CRI-I2-1, #112), following the production design
// in docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md. CRI-R15 proved the handoff works
// when the late process can write evidence into a writable bind mount of the
// runtime-owned handoff directory.
//
// The R16 design states the OCI mount shape as:
//
//	type: bind
//	source:      /run/macvz/containers/<id>/handoff   (host-visible guest path)
//	destination: /run/macvz/handoff                   (HandoffMountPoint)
//	options:     ["rbind", "rw"]
//
// MacVz realizes a writable bind mount as a VirtioFS share: the apple/container
// driver maps a types.Mount with ReadOnly=false to a writable
// "--volume source:target", which is the rbind+rw semantics the design requires.
// So the OCI options are implicit in this model and ReadOnly is false. All
// apple/container and vminitd assumptions stay inside the runtime package.
//
// This file does the spec mutation and prepares the in-rootfs mount destination.
// It deliberately does NOT verify identity evidence (that is StartContainer
// wiring, a later task) and the mount is always runtime-derived, never built
// from a CRI/kubelet mount.

// mountpointDirMode is the mode of the in-rootfs handoff mount destination. It is
// only a mountpoint — the writable handoff source directory (handoffDirMode) is
// what the late process writes into — so a normal runtime-owned 0755 is enough.
const mountpointDirMode = 0o755

// ErrHandoffMountConflict means a container spec already contains a mount whose
// target is inside the reserved /run/macvz runtime namespace but is not the
// runtime's own handoff mount. Such a mount can only come from an untrusted
// source (a CRI/kubelet mount that escaped the #111 guard, or a caller bug); it
// would collide with the runtime-private handoff, so injection refuses it.
var ErrHandoffMountConflict = errors.New("runtime: spec mount targets the reserved handoff namespace")

// HandoffMount returns the runtime-private handoff bind mount for a prepared
// container. Source is the host-visible handoff directory, Target is the
// in-container HandoffMountPoint, and it is writable (ReadOnly=false) so the late
// process can write evidence the runtime later reads back. It is the MacVz
// realization of the R16 OCI bind mount {type:bind, options:[rbind,rw]}.
func HandoffMount(layout HandoffLayout) types.Mount {
	return types.Mount{
		Source:   layout.HandoffDir,
		Target:   layout.MountPoint,
		ReadOnly: false,
		Tmpfs:    false,
	}
}

// InjectHandoffMount appends the handoff bind mount to spec.Mounts. It is a pure
// spec mutation and touches no filesystem; call PrepareHandoffMountpoint to
// create the in-rootfs destination before launch.
//
// It rejects a layout with empty paths and any pre-existing mount that targets
// the reserved /run/macvz namespace from a non-handoff source
// (ErrHandoffMountConflict). Re-injecting the identical handoff mount is a no-op
// so the call is idempotent.
func InjectHandoffMount(spec *types.ContainerSpec, layout HandoffLayout) error {
	if spec == nil {
		return errors.New("runtime: inject handoff mount: nil spec")
	}
	if layout.HandoffDir == "" || layout.MountPoint == "" {
		return errors.New("runtime: inject handoff mount: incomplete handoff layout")
	}
	if !path.IsAbs(layout.MountPoint) {
		return fmt.Errorf("runtime: inject handoff mount: mount point %q must be absolute", layout.MountPoint)
	}
	if !withinReservedRuntimeNamespace(layout.MountPoint) {
		return fmt.Errorf("runtime: inject handoff mount: mount point %q is outside reserved runtime namespace %q",
			layout.MountPoint, HandoffRuntimeRoot)
	}
	want := HandoffMount(layout)
	for _, m := range spec.Mounts {
		if !withinReservedRuntimeNamespace(m.Target) {
			continue
		}
		if m == want {
			// Already injected: idempotent success, do not duplicate.
			return nil
		}
		return fmt.Errorf("%w: existing mount target %q (source %q) blocks the handoff at %q",
			ErrHandoffMountConflict, m.Target, m.Source, layout.MountPoint)
	}
	spec.Mounts = append(spec.Mounts, want)
	return nil
}

// PrepareHandoffMountpoint creates the bind-mount destination inside the prepared
// rootfs so the handoff mount has a target directory. The destination mirrors
// HandoffMountPoint under the prepared rootfs, e.g.
// <RootfsDir>/run/macvz/handoff. It is idempotent and returns the created path.
// A failure to prepare the destination is reported with the offending path.
func PrepareHandoffMountpoint(layout HandoffLayout) (string, error) {
	if layout.RootfsDir == "" || layout.MountPoint == "" {
		return "", errors.New("runtime: prepare handoff mountpoint: incomplete handoff layout")
	}
	if !withinReservedRuntimeNamespace(layout.MountPoint) {
		return "", fmt.Errorf("runtime: prepare handoff mountpoint: mount point %q is outside reserved runtime namespace %q",
			layout.MountPoint, HandoffRuntimeRoot)
	}
	dest, err := rootfsGuestPath(layout.RootfsDir, layout.MountPoint)
	if err != nil {
		return "", fmt.Errorf("runtime: prepare handoff mountpoint: %w", err)
	}
	if err := os.MkdirAll(dest, mountpointDirMode); err != nil {
		return "", fmt.Errorf("runtime: prepare handoff mountpoint %s: %w", dest, err)
	}
	return dest, nil
}

// withinReservedRuntimeNamespace reports whether a cleaned, absolute target is at
// or under HandoffRuntimeRoot (/run/macvz). It mirrors the segment-aware match
// the CRI mount guard (#111) uses, so a sibling like /run/macvz-data is not
// treated as inside the namespace.
func withinReservedRuntimeNamespace(target string) bool {
	if !path.IsAbs(target) {
		return false
	}
	clean := path.Clean(target)
	root := path.Clean(HandoffRuntimeRoot)
	return clean == root || strings.HasPrefix(clean, root+"/")
}

// rootfsGuestPath maps a guest-absolute path to its host path under a prepared
// rootfs. The guest path is cleaned with POSIX semantics before conversion so
// callers cannot escape rootfsDir with ".." or a platform-specific separator.
func rootfsGuestPath(rootfsDir, guestPath string) (string, error) {
	if rootfsDir == "" {
		return "", errors.New("empty rootfs dir")
	}
	if !path.IsAbs(guestPath) {
		return "", fmt.Errorf("guest path %q must be absolute", guestPath)
	}
	clean := path.Clean(guestPath)
	if clean == "/" {
		return "", errors.New("guest path must not be root")
	}
	rel := strings.TrimPrefix(clean, "/")
	return filepath.Join(rootfsDir, filepath.FromSlash(rel)), nil
}
