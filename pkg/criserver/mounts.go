package criserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	macvzruntime "github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// This file implements the CRI-P7 mount translation (#79). In CRI mode the
// kubelet — not MacVz — materializes a Pod's ConfigMaps, Secrets, projected
// ServiceAccount tokens, Downward API data, and emptyDir storage on the host
// filesystem, then passes them to the runtime as host bind mounts in
// CreateContainerRequest.Config.Mounts. The adapter's job is therefore narrow and
// honest: validate each kubelet-provided mount against a conservative policy and
// translate it into the runtime's VirtioFS share, never re-projecting content the
// kubelet already wrote.
//
// emptyDir lifecycle and cleanup are owned by the kubelet (it creates and removes
// the per-Pod volume directories under its pods root); the adapter only binds the
// directory the kubelet provides. This keeps CRI-P7 within scope: no dynamic PV
// provisioning and no MacVz-side volume materialization.

// defaultKubeletPodsDir is the kubelet per-Pod volume root on a standard install.
// Mounts whose host source is under this prefix are kubelet-managed projected
// volumes (ConfigMap, Secret, projected SA token, Downward API) and emptyDir
// storage, and are always allowed regardless of the hostPath allowlist.
const defaultKubeletPodsDir = "/var/lib/kubelet/pods"

// reservedRuntimePrefix is the MacVz-owned runtime namespace inside a container.
// The runtime (pkg/runtime) injects its private handoff bind mount at
// reservedHandoffPath and stages rootfs/handoff state under this namespace
// (R16 handoff design, #108). A kubelet- or user-provided mount that targets
// this namespace would shadow, hijack, or corrupt the runtime-private identity
// and result handoff, so such targets are rejected outright. This is a
// destination (container_path) guard and is independent of the hostPath source
// allowlist: no operator opt-in can re-enable a reserved target.
const reservedRuntimePrefix = macvzruntime.HandoffRuntimeRoot

// reservedHandoffPath is the runtime-private handoff bind-mount point. It lives
// under reservedRuntimePrefix and is named here only to make rejection errors
// actionable; the guard covers the whole reservedRuntimePrefix namespace, not
// just this single path.
const reservedHandoffPath = macvzruntime.HandoffMountPoint

// MountPolicy governs which kubelet-provided host mounts the CRI adapter binds
// into a micro-VM (CRI-P7, #79). The zero value is safe and restrictive: only
// kubelet-managed projected/emptyDir volumes under the kubelet pods directory are
// allowed, and arbitrary hostPath volumes are rejected until explicitly allowed.
type MountPolicy struct {
	// KubeletPodsDir is the kubelet per-Pod volume root. Mounts whose cleaned host
	// source is at or below this prefix are kubelet-managed (projected volumes and
	// emptyDir) and always allowed. Empty falls back to defaultKubeletPodsDir.
	KubeletPodsDir string
	// HostPathAllowedPrefixes is the allowlist of absolute, cleaned host path
	// prefixes a hostPath mount (one outside KubeletPodsDir) may resolve under.
	// Empty disables arbitrary hostPath — the conservative macOS default — so a Pod
	// cannot bind host directories the operator did not opt into.
	HostPathAllowedPrefixes []string
}

// kubeletPodsDir returns the configured kubelet pods root or the default.
func (p MountPolicy) kubeletPodsDir() string {
	if p.KubeletPodsDir == "" {
		return defaultKubeletPodsDir
	}
	return filepath.Clean(p.KubeletPodsDir)
}

// translateMounts converts kubelet-provided CRI mounts into runtime mounts and
// their persisted records, enforcing the mount policy. It returns a
// FailedPrecondition for any mount the policy rejects so the violation surfaces to
// the kubelet rather than booting a workload with a silently dropped volume.
//
// A CRI Mount with an empty HostPath is treated as a guest-local tmpfs: the
// kubelet uses this for a Memory-medium emptyDir, which has no host backing.
func (s *Server) translateMounts(mounts []*runtimeapi.Mount) ([]types.Mount, []store.Mount, error) {
	if len(mounts) == 0 {
		return nil, nil, nil
	}
	policy := s.mountPolicy
	podsDir := policy.kubeletPodsDir()

	rtMounts := make([]types.Mount, 0, len(mounts))
	recMounts := make([]store.Mount, 0, len(mounts))
	for _, m := range mounts {
		target := m.GetContainerPath()
		if !filepath.IsAbs(target) {
			return nil, nil, status.Errorf(codes.InvalidArgument,
				"CreateContainer: mount container_path %q must be absolute", target)
		}
		target = filepath.Clean(target)

		// The runtime owns the /run/macvz namespace for its private handoff bind
		// mount (R16, #108). A kubelet mount targeting it — tmpfs or bind — would
		// collide with that handoff, so reject the target regardless of source.
		if withinPrefix(target, reservedRuntimePrefix) {
			return nil, nil, status.Errorf(codes.FailedPrecondition,
				"CreateContainer: mount container_path %q targets the reserved MacVz runtime namespace %q (used for the runtime-private handoff at %q); choose a different mount point",
				target, reservedRuntimePrefix, reservedHandoffPath)
		}

		// An empty host path is a guest-local tmpfs (Memory-medium emptyDir): no
		// host source to validate, allocate in-guest memory at the target.
		if m.GetHostPath() == "" {
			rtMounts = append(rtMounts, types.Mount{Target: target, Tmpfs: true})
			recMounts = append(recMounts, store.Mount{ContainerPath: target, Tmpfs: true})
			continue
		}

		source := m.GetHostPath()
		if !filepath.IsAbs(source) {
			return nil, nil, status.Errorf(codes.InvalidArgument,
				"CreateContainer: mount host_path %q must be absolute", source)
		}
		source = filepath.Clean(source)

		// A kubelet-managed mount (projected volume or emptyDir) lives under the
		// kubelet pods dir and is always allowed. Anything else is an operator
		// hostPath and must be explicitly allowlisted.
		if !withinPrefix(source, podsDir) && !withinAnyPrefix(source, policy.HostPathAllowedPrefixes) {
			return nil, nil, status.Errorf(codes.FailedPrecondition,
				"CreateContainer: hostPath mount %q is not under the kubelet pods dir %q and not within an allowed prefix %v; "+
					"allow it with --volume-host-path-allowed or mount a supported volume type",
				source, podsDir, policy.HostPathAllowedPrefixes)
		}

		// Bidirectional propagation would require shared mount semantics VirtioFS
		// cannot honor; rejecting it is more honest than silently downgrading to a
		// private bind, which could hide data that never propagates back to the host.
		if m.GetPropagation() == runtimeapi.MountPropagation_PROPAGATION_BIDIRECTIONAL {
			return nil, nil, status.Errorf(codes.FailedPrecondition,
				"CreateContainer: mount %q requests bidirectional propagation, which the VirtioFS share does not support", source)
		}

		rtMounts = append(rtMounts, types.Mount{
			Source:   source,
			Target:   target,
			ReadOnly: m.GetReadonly(),
		})
		recMounts = append(recMounts, store.Mount{
			HostPath:      source,
			ContainerPath: target,
			ReadOnly:      m.GetReadonly(),
		})
	}
	return rtMounts, recMounts, nil
}

const (
	mountSourceReadyTimeout  = 5 * time.Second
	mountSourceReadyInterval = 50 * time.Millisecond
)

func (s *Server) waitForKubeletMountSources(ctx context.Context, mounts []types.Mount) error {
	podsDir := s.mountPolicy.kubeletPodsDir()
	var pending []string
	for _, m := range mounts {
		if m.Source != "" && withinPrefix(filepath.Clean(m.Source), podsDir) {
			pending = append(pending, m.Source)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	deadline := time.NewTimer(mountSourceReadyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(mountSourceReadyInterval)
	defer ticker.Stop()

	for {
		missing := pending[:0]
		for _, source := range pending {
			if _, err := os.Stat(source); err == nil {
				continue
			} else if os.IsNotExist(err) {
				missing = append(missing, source)
			} else {
				return status.Errorf(codes.FailedPrecondition,
					"CreateContainer: kubelet mount source %q is not ready: %v", source, err)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		pending = missing

		select {
		case <-ctx.Done():
			return status.Errorf(codes.Canceled,
				"CreateContainer: waiting for kubelet mount source %q: %v", pending[0], ctx.Err())
		case <-deadline.C:
			return status.Errorf(codes.FailedPrecondition,
				"CreateContainer: kubelet mount source %q did not appear within %s", pending[0], mountSourceReadyTimeout)
		case <-ticker.C:
		}
	}
}

// withinPrefix reports whether clean (an absolute, cleaned path) is at or below
// prefix. Matching is path-segment aware so "/data" does not admit "/database".
func withinPrefix(clean, prefix string) bool {
	prefix = filepath.Clean(prefix)
	return clean == prefix || strings.HasPrefix(clean, prefix+string(filepath.Separator))
}

func withinAnyPrefix(clean string, prefixes []string) bool {
	for _, p := range prefixes {
		if withinPrefix(clean, p) {
			return true
		}
	}
	return false
}

// toCRIMounts maps persisted mount records to CRI ContainerStatus mounts so
// `crictl inspect` reports the volumes a container was created with.
func toCRIMounts(ms []store.Mount) []*runtimeapi.Mount {
	if len(ms) == 0 {
		return nil
	}
	out := make([]*runtimeapi.Mount, 0, len(ms))
	for _, m := range ms {
		out = append(out, &runtimeapi.Mount{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			Readonly:      m.ReadOnly,
		})
	}
	return out
}

// mountSummary renders mounts for verbose status/logging without dumping host
// paths verbatim into structured logs.
func mountSummary(ms []store.Mount) string {
	if len(ms) == 0 {
		return "none"
	}
	return fmt.Sprintf("%d mount(s)", len(ms))
}
