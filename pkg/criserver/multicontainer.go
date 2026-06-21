package criserver

import (
	"context"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// This file implements the CRI-P9 follow-up (#82): multi-container Pod
// feasibility for the experimental macvz-cri adapter.
//
// A Kubernetes Pod sandbox is expected to give every container in the Pod a
// SHARED network namespace: one Pod IP, mutual localhost reachability, and a
// shared port space (the "pause container" model). apple/container cannot model
// this today, and the reason is architectural rather than a missing flag:
//
//   - apple/container runs ONE Linux micro-VM (one Linux kernel) per container.
//   - A network namespace is a per-kernel construct. Two micro-VMs are two
//     separate kernels, so there is no shared kernel in which a single network
//     namespace could live across them.
//   - The CLI surface confirms the gap (probed on this host, container CLI
//     1.0.0): `--network <name>[,mac,mtu]` only attaches a container to an L3
//     vmnet subnet (a distinct IP per container, like a Docker bridge) — that is
//     shared L3 connectivity, NOT a shared namespace. There is no
//     `--net=container:<id>` namespace join, no `--pid`/`--ipc` sharing, and
//     `container exec` shares an already-running VM's namespaces but cannot bring
//     a second image's root filesystem, lifecycle, or resource accounting.
//
// The missing primitive, stated precisely, is recorded in
// missingSharedNetnsPrimitive below. The single-container restriction (CRI-P3)
// stays the default and honest behavior. An operator can opt into the
// experimental probe with --experimental-multi-container; without a runtime that
// implements the pause-VM create/join capability that probe still rejects, but
// with a richer, actionable diagnostic naming the exact apple/container gap.

// missingSharedNetnsPrimitive is the operator-facing statement of the capability
// apple/container would need for honest multi-container Pods. It is returned
// verbatim from the experimental CreateContainer rejection so the blocker is
// actionable rather than a flat "unsupported".
const missingSharedNetnsPrimitive = "apple/container exposes no shared network namespace across micro-VMs: " +
	"it runs one Linux kernel per container, and a network namespace is per-kernel, so two micro-VMs cannot share one. " +
	"The missing primitive is the ability to run a second OCI image (its own rootfs, OCI config, lifecycle, and resource " +
	"limits) as an additional container INSIDE an existing Pod sandbox VM, sharing that VM's network (and ideally IPC) " +
	"namespace — the pause-VM model used by Kata/Firecracker CRI runtimes. Until apple/container offers an equivalent " +
	"sandbox-VM-with-container-joins primitive, honest multi-container Pods are not representable (see docs/CRI_FEASIBILITY.md #82)."

// SharedPodNetworkRuntime is the optional capability a ContainerRuntime
// implements when it CAN create additional container workloads inside an existing
// Pod sandbox's micro-VM sharing one network namespace (the pause-VM model
// described above). apple/container's driver does not implement it today.
//
// The important part is CreateInPodSandbox: a plain Create would launch a second
// independent micro-VM, which is exactly the dishonest model this spike rejects.
// A runtime must expose an explicit join operation before the adapter admits a
// second live container.
type SharedPodNetworkRuntime interface {
	// SupportsSharedPodNetwork reports whether the runtime can join a new
	// container to an existing sandbox VM's network namespace. Support also means
	// the sandbox namespace lifetime is not tied to one ordinary workload
	// container: stopping/removing the first container must not implicitly destroy
	// the shared namespace while another joined container is still live. When
	// false, the returned string names the missing primitive for diagnostics.
	SupportsSharedPodNetwork() (bool, string)
	// CreateInPodSandbox creates spec as an additional container inside the sandbox
	// VM identified by sandboxWorkloadID, sharing that VM's Pod network namespace.
	// The returned workload id must be addressable by the normal Start/Stop/Destroy,
	// Logs, Exec, Status, and Stats methods.
	CreateInPodSandbox(ctx context.Context, sandboxWorkloadID string, spec types.ContainerSpec) (id string, err error)
}

// sharedPodNetworkRuntime reports whether the configured runtime implements the
// pause-VM shared-netns create/join capability. apple/container does not implement
// SharedPodNetworkRuntime, so the default — and current production — answer is
// (false, missingSharedNetnsPrimitive).
func sharedPodNetworkRuntime(rt ContainerRuntime) (SharedPodNetworkRuntime, bool, string) {
	if c, ok := rt.(SharedPodNetworkRuntime); ok {
		if supported, reason := c.SupportsSharedPodNetwork(); supported {
			return c, true, ""
		} else {
			return nil, false, reason
		}
	}
	return nil, false, missingSharedNetnsPrimitive
}

// admitAdditionalContainer decides whether a second (or later) live container is
// allowed in a sandbox that already holds one. The single-container restriction
// (CRI-P3) is the default and remains the honest behavior on apple/container.
//
// It returns nil — permitting the additional container — only when BOTH the
// operator opted into the experimental path (--experimental-multi-container) AND
// the runtime implements the pause-VM shared-netns create/join capability.
// Otherwise it returns
// a typed CRI error explaining exactly why: the default case names the existing
// container and points at the experimental flag; the experimental-but-unsupported
// case names the missing apple/container primitive, so the rejection is an
// actionable capability statement, not a flat refusal.
func (s *Server) admitAdditionalContainer(sandboxID string, existing store.Container) (SharedPodNetworkRuntime, string, error) {
	if !s.multiContainer {
		return nil, "", status.Errorf(codes.FailedPrecondition,
			"CreateContainer: sandbox %q already has a live container (%s, %s); CRI mode supports one container per Pod sandbox. "+
				"Multi-container Pods need the pause-VM shared-netns model (docs/CRI_FEASIBILITY.md #82); "+
				"opt into the experimental probe with --experimental-multi-container",
			sandboxID, existing.ID, existing.State)
	}
	if rt, ok, reason := sharedPodNetworkRuntime(s.containerRuntime); !ok {
		return nil, "", status.Errorf(codes.Unimplemented,
			"CreateContainer: multi-container Pod requested for sandbox %q (existing live container %s) but unsupported: %s",
			sandboxID, existing.ID, reason)
	} else {
		return rt, existing.WorkloadID, nil
	}
}

// multiContainerInfo reports the experimental multi-container probe state for the
// Status --verbose info map. It never claims support that the runtime does not
// declare.
func (s *Server) multiContainerInfo() string {
	if !s.multiContainer {
		return "disabled (single container per Pod sandbox; --experimental-multi-container to probe)"
	}
	if _, ok, reason := sharedPodNetworkRuntime(s.containerRuntime); !ok {
		return "experimental probe enabled, runtime unsupported: " + reason
	}
	return "experimental probe enabled; runtime provides pause-VM shared-netns create/join support"
}
