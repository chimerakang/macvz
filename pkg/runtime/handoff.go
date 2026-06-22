package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// handoff.go owns the runtime-private handoff *path lifecycle* (CRI-I1-1, #109):
// deriving, creating, and cleaning up the per-container directories described in
// docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md. The handoff is a runtime-private
// per-container directory that the runtime prepares before process creation,
// bind-mounts into the late rootfs, and removes during cleanup. It is NOT a
// kubelet-visible volume and NOT a Kubernetes API surface: only the runtime
// driver creates, reads, and deletes it.
//
// The reserved path layout and metadata shape are the single source of truth in
// handoffmeta.go (CRI-I1-2, #110): this file builds on HandoffContainersRoot,
// the rootfs/handoff subdir names, HandoffMountPoint, and IdentityFile rather
// than redeclaring them. This file adds the filesystem side those constants
// describe — sanitization, directory creation with intended modes, and
// idempotent removal — and deliberately does NOT wire CreateContainer/
// StartContainer or inject the OCI bind mount; that is a later task.

// The reserved runtime-private path layout constants (HandoffRuntimeRoot,
// HandoffContainersRoot, HandoffMountPoint, IdentityFile, rootfsSubdir,
// handoffSubdir) are defined in handoffmeta.go (#110), the single source of
// truth for the layout. This file consumes them.

const (
	// containerDirMode and rootfsDirMode are runtime-owned and not generally
	// writable by container processes.
	containerDirMode = 0o755
	rootfsDirMode    = 0o755

	// handoffDirMode is intentionally world-writable so the container's
	// configured process user can write evidence even when it is not root.
	// R15 showed that a root-owned 0755 handoff directory causes "Permission
	// denied" for non-root late processes. This is safe because the directory
	// is private to one container and deleted with that container; it is not a
	// shared Pod volume or a host filesystem escape. Future hardening can narrow
	// this to runAsUser/runAsGroup once user mapping lands in the LinuxPod path.
	handoffDirMode = 0o777
)

// ErrInvalidHandoffID means a container/workload ID could not be used to derive
// a handoff path because it was empty or contained a path separator, parent
// reference, or shell metacharacter. Callers should treat this as a precondition
// failure and never persist a container record for it.
var ErrInvalidHandoffID = errors.New("runtime: invalid handoff container id")

// validHandoffID accepts a single path element: it must start with an
// alphanumeric and contain only alphanumerics, dot, underscore, and hyphen. This
// matches the shape of store.DeriveWorkloadID ("macvz-cri-<hex>") while
// rejecting separators and shell metacharacters. ".." is additionally rejected
// in sanitizeHandoffID as defense in depth.
var validHandoffID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// HandoffLayout is the set of runtime-private paths for a single container.
type HandoffLayout struct {
	// ContainerDir is HandoffContainersRoot/<containerID>, the per-container
	// subtree (rooted at the manager's configured root in tests).
	ContainerDir string
	// RootfsDir is where the prepared (late) rootfs is staged.
	RootfsDir string
	// HandoffDir is the host-visible source of the handoff bind mount; the
	// container writes evidence here, the runtime reads it back through vminitd.
	HandoffDir string
	// MountPoint is the destination the handoff directory is mounted to inside
	// the container (HandoffMountPoint). It is data for OCI mount injection done
	// elsewhere; this package does not create or mount it.
	MountPoint string
	// IdentityFile is the host path of the minimum required evidence file
	// (HandoffDir/identity).
	IdentityFile string
}

// HandoffManager derives and manages runtime-private handoff paths under a root.
// The zero value is not usable; construct one with NewHandoffManager. It holds
// no per-container state, so a single manager serves every container on the
// node.
type HandoffManager struct {
	root string
}

// NewHandoffManager returns a manager rooted at root. An empty root falls back to
// HandoffContainersRoot (the production /run/macvz/containers); tests pass a temp
// directory so the helper stays hermetic and never touches the real /run.
func NewHandoffManager(root string) *HandoffManager {
	if root == "" {
		root = HandoffContainersRoot
	}
	return &HandoffManager{root: root}
}

// Layout derives the runtime-private paths for containerID without touching the
// filesystem. It returns ErrInvalidHandoffID if the ID cannot be safely used as
// a single path element. When the manager uses the default root, the rootfs and
// handoff paths match HandoffPaths(id) from handoffmeta.go.
func (m *HandoffManager) Layout(containerID string) (HandoffLayout, error) {
	id, err := sanitizeHandoffID(containerID)
	if err != nil {
		return HandoffLayout{}, err
	}
	containerDir := filepath.Join(m.root, id)
	handoffDir := filepath.Join(containerDir, handoffSubdir)
	return HandoffLayout{
		ContainerDir: containerDir,
		RootfsDir:    filepath.Join(containerDir, rootfsSubdir),
		HandoffDir:   handoffDir,
		MountPoint:   HandoffMountPoint,
		IdentityFile: filepath.Join(handoffDir, IdentityFile),
	}, nil
}

// Create derives the layout and creates the container, rootfs, and handoff
// directories with their intended modes. It is safe to call more than once for
// the same container: existing directories are reused and their modes are
// re-applied. The handoff directory mode is forced with Chmod so a restrictive
// umask cannot leave it non-writable for a non-root container process.
//
// On any failure after this call creates a new container subtree, Create removes
// that subtree so a first-time failed Create leaves nothing staged. If the
// subtree already existed, failures leave it intact so retry cannot destroy
// handoff evidence or debug state from an earlier attempt.
func (m *HandoffManager) Create(containerID string) (HandoffLayout, error) {
	layout, err := m.Layout(containerID)
	if err != nil {
		return HandoffLayout{}, err
	}

	containerExisted := false
	if fi, err := os.Stat(layout.ContainerDir); err == nil {
		if !fi.IsDir() {
			return HandoffLayout{}, fmt.Errorf("runtime: handoff container path exists but is not a directory: %s", layout.ContainerDir)
		}
		containerExisted = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return HandoffLayout{}, fmt.Errorf("runtime: stat handoff container dir: %w", err)
	}

	if err := os.MkdirAll(layout.ContainerDir, containerDirMode); err != nil {
		return HandoffLayout{}, fmt.Errorf("runtime: create handoff container dir: %w", err)
	}
	// From here on, undo only a subtree created by this call. A repeat Create
	// must never delete existing handoff state if it encounters a later error.
	cleanup := func(cause error) (HandoffLayout, error) {
		if !containerExisted {
			_ = os.RemoveAll(layout.ContainerDir)
		}
		return HandoffLayout{}, cause
	}
	if err := os.Chmod(layout.ContainerDir, containerDirMode); err != nil {
		return cleanup(fmt.Errorf("runtime: chmod handoff container dir: %w", err))
	}

	if err := os.MkdirAll(layout.RootfsDir, rootfsDirMode); err != nil {
		return cleanup(fmt.Errorf("runtime: create handoff rootfs dir: %w", err))
	}
	if err := os.Chmod(layout.RootfsDir, rootfsDirMode); err != nil {
		return cleanup(fmt.Errorf("runtime: chmod handoff rootfs dir: %w", err))
	}
	if err := os.MkdirAll(layout.HandoffDir, handoffDirMode); err != nil {
		return cleanup(fmt.Errorf("runtime: create handoff dir: %w", err))
	}
	// MkdirAll applies the umask, so force the world-writable mode explicitly.
	if err := os.Chmod(layout.HandoffDir, handoffDirMode); err != nil {
		return cleanup(fmt.Errorf("runtime: chmod handoff dir: %w", err))
	}
	return layout, nil
}

// Cleanup removes the entire per-container subtree (rootfs, handoff, and any
// staged archives or metadata). It is idempotent: a missing subtree is a
// success, matching the RemoveContainer/Destroy contract. An invalid ID is also
// treated as nothing-to-remove rather than an error, so cleanup of a container
// that never got a valid ID does not wedge teardown.
func (m *HandoffManager) Cleanup(containerID string) error {
	layout, err := m.Layout(containerID)
	if err != nil {
		if errors.Is(err, ErrInvalidHandoffID) {
			return nil
		}
		return err
	}
	if err := os.RemoveAll(layout.ContainerDir); err != nil {
		return fmt.Errorf("runtime: cleanup handoff for %q: %w", containerID, err)
	}
	return nil
}

// ListContainerIDs returns the container/workload IDs of every per-container
// handoff subtree currently present under the manager's root. It is the basis for
// restart-time orphan detection (CRI-I4-3, #120): the adapter compares this
// on-disk set against its known container records and reclaims any subtree with no
// backing record.
//
// A missing root means no handoff was ever prepared and yields an empty list, not
// an error. Non-directory entries and entries whose names are not valid handoff
// IDs are skipped: only well-formed per-container subtrees this manager could have
// created are reported, so a stray file under the root never masquerades as an
// orphan workload.
func (m *HandoffManager) ListContainerIDs() ([]string, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("runtime: list handoff subtrees under %s: %w", m.root, err)
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := sanitizeHandoffID(e.Name()); err != nil {
			continue
		}
		ids = append(ids, e.Name())
	}
	sort.Strings(ids)
	return ids, nil
}

// sanitizeHandoffID validates that id is usable as a single path element and
// returns it unchanged. It rejects empty IDs, separators, parent references, and
// shell metacharacters so a hostile or malformed CRI container ID cannot escape
// the handoff root.
func sanitizeHandoffID(id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("%w: empty id", ErrInvalidHandoffID)
	}
	if id == "." || id == ".." || strings.Contains(id, "..") {
		return "", fmt.Errorf("%w: %q is a relative path reference", ErrInvalidHandoffID, id)
	}
	if !validHandoffID.MatchString(id) {
		return "", fmt.Errorf("%w: %q contains a separator or disallowed character", ErrInvalidHandoffID, id)
	}
	return id, nil
}
