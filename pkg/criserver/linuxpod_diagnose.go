package criserver

import (
	"context"
	"os"
	"sort"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
)

// linuxpod_diagnose.go adds CRI-L6-2 (#136) operator diagnostics for stale
// LinuxPod-backed CRI state. After a crash, a partial cleanup, a helper restart,
// or a kubelet retry loop, persisted sandbox/container records can disagree with
// the live LinuxPod helper backend. The diagnostic classifies that residual state
// into machine-readable categories so an operator (or a script) can tell at a
// glance which records are healthy, which are stale and will be recreated, and
// which point at leftover helper-work — without having to read raw JSON and probe
// the helper by hand.
//
// The diagnostic is strictly READ-ONLY. It probes the backend with PodStatus,
// reads the IPAM reservation view, and stats materialized mount sources, but it
// never detaches a Pod network path, releases an IP, mutates a record, or touches
// any host route. Repairing stale state is the job of the live service paths
// (StopPodSandbox/RemovePodSandbox, the backend reconciler, and the NotReady
// discard on recreate); the diagnostic only describes what is there.

// LinuxPodResidualCategory is a machine-readable classification of one residual
// LinuxPod CRI record. The values are stable strings so scripts can match on them.
type LinuxPodResidualCategory string

const (
	// ResidualSandboxReadyBackendLive is a Ready sandbox the helper still backs:
	// healthy, no action needed.
	ResidualSandboxReadyBackendLive LinuxPodResidualCategory = "sandbox-ready-backend-live"
	// ResidualSandboxReadyBackendLost is a Ready sandbox the helper no longer backs
	// (helper restart / state loss). It is stale Running-but-unusable state; the
	// reconciler marks it NotReady so kubelet recreates the Pod (BackendLost).
	ResidualSandboxReadyBackendLost LinuxPodResidualCategory = "sandbox-ready-backend-lost"
	// ResidualSandboxReadyBackendUnprobed is a Ready sandbox whose backend could not
	// be probed because the diagnostic was run without a helper socket. Backend
	// liveness is unknown.
	ResidualSandboxReadyBackendUnprobed LinuxPodResidualCategory = "sandbox-ready-backend-unprobed"
	// ResidualSandboxReadyBackendError is a Ready sandbox whose backend probe failed
	// for a reason other than a missing Pod (helper unreachable / protocol error).
	// Liveness is indeterminate; the last known CRI state is retained.
	ResidualSandboxReadyBackendError LinuxPodResidualCategory = "sandbox-ready-backend-error"
	// ResidualSandboxNotReady is a stopped/lost sandbox whose record is retained
	// until RemovePodSandbox or the next same-Pod recreate discards it. It cannot
	// block kubelet replacement.
	ResidualSandboxNotReady LinuxPodResidualCategory = "sandbox-notready-retained"

	// ResidualContainerRunningBackendLive is a Running container in a backend-live
	// sandbox: healthy.
	ResidualContainerRunningBackendLive LinuxPodResidualCategory = "container-running-backend-live"
	// ResidualContainerRunningBackendLost is a container still recorded Running whose
	// sandbox backend is gone. The reconciler marks it Exited/BackendLost.
	ResidualContainerRunningBackendLost LinuxPodResidualCategory = "container-running-backend-lost"
	// ResidualContainerRunningBackendUnprobed is a Running container whose sandbox
	// liveness was not probed because no helper backend was available to the
	// diagnostic. It is unknown, not proven live.
	ResidualContainerRunningBackendUnprobed LinuxPodResidualCategory = "container-running-backend-unprobed"
	// ResidualContainerRunningBackendError is a Running container whose sandbox
	// liveness is indeterminate because the backend probe failed for a reason other
	// than a missing Pod.
	ResidualContainerRunningBackendError LinuxPodResidualCategory = "container-running-backend-error"
	// ResidualContainerCreatedRetained is a created-but-never-started container record.
	ResidualContainerCreatedRetained LinuxPodResidualCategory = "container-created-retained"
	// ResidualContainerExitedRetained is an exited container record retained until
	// RemoveContainer/RemovePodSandbox deletes it.
	ResidualContainerExitedRetained LinuxPodResidualCategory = "container-exited-retained"
	// ResidualContainerOrphaned is a container record whose owning sandbox record is
	// gone — partially removed state a removal retry should reap.
	ResidualContainerOrphaned LinuxPodResidualCategory = "container-orphaned"
)

// LinuxPodResidualReport is the machine-readable result of a residual-state scan.
type LinuxPodResidualReport struct {
	// GeneratedAt is when the report was built (unix nanoseconds).
	GeneratedAt int64 `json:"generatedAt"`
	// BackendProbed is true when a helper backend was available to probe; false
	// makes every sandbox liveness "unprobed".
	BackendProbed bool `json:"backendProbed"`
	// NetworkEnabled reflects whether the Pod network path was wired for this scan;
	// it tells an operator whether NetworkResidual fields are authoritative.
	NetworkEnabled bool                        `json:"networkEnabled"`
	Sandboxes      []LinuxPodSandboxResidual   `json:"sandboxes"`
	Containers     []LinuxPodContainerResidual `json:"containers"`
	// Summary counts records per category, so a script can fail fast on any
	// non-empty residual bucket without walking the full lists.
	Summary map[LinuxPodResidualCategory]int `json:"summary"`
}

// LinuxPodSandboxResidual is one sandbox's residual-state classification.
type LinuxPodSandboxResidual struct {
	SandboxID         string                   `json:"sandboxID"`
	Namespace         string                   `json:"namespace"`
	Name              string                   `json:"name"`
	UID               string                   `json:"uid"`
	State             string                   `json:"state"`
	Category          LinuxPodResidualCategory `json:"category"`
	BackendLive       bool                     `json:"backendLive"`
	BackendProbeError string                   `json:"backendProbeError,omitempty"`
	LinuxPodNamespace string                   `json:"linuxPodNamespace,omitempty"`
	Network           LinuxPodNetworkResidual  `json:"network"`
	ContainerIDs      []string                 `json:"containerIDs,omitempty"`
}

// LinuxPodNetworkResidual is the residual Pod network state for a sandbox, read
// from the persisted record (and the IPAM view when available). Reporting it does
// not change any host route.
type LinuxPodNetworkResidual struct {
	PodIP            string `json:"podIP,omitempty"`
	VMIP             string `json:"vmIP,omitempty"`
	Interface        string `json:"interface,omitempty"`
	Attached         bool   `json:"attached"`
	IPReservedForKey bool   `json:"ipReservedForKey"`
}

// LinuxPodContainerResidual is one container's residual-state classification.
type LinuxPodContainerResidual struct {
	ContainerID        string                   `json:"containerID"`
	SandboxID          string                   `json:"sandboxID"`
	Name               string                   `json:"name"`
	State              string                   `json:"state"`
	Reason             string                   `json:"reason,omitempty"`
	Category           LinuxPodResidualCategory `json:"category"`
	BackendContainerID string                   `json:"backendContainerID,omitempty"`
	Mounts             []LinuxPodMountResidual  `json:"mounts,omitempty"`
}

// LinuxPodMountResidual reports a materialized mount the container record carries
// and whether its host source still exists on disk, so an operator can spot
// leftover materialized mount state after a partial teardown.
type LinuxPodMountResidual struct {
	HostPath       string `json:"hostPath,omitempty"`
	ContainerPath  string `json:"containerPath"`
	ReadOnly       bool   `json:"readOnly,omitempty"`
	Tmpfs          bool   `json:"tmpfs,omitempty"`
	HostPathExists bool   `json:"hostPathExists"`
}

// Diagnose builds a read-only residual-state report for the running service. It
// probes the live backend without holding s.mu over the calls and never mutates
// any record, IP reservation, or host route. Use it to surface stale state for an
// operator; repair happens through the normal service paths.
func (s *LinuxPodService) Diagnose(ctx context.Context) LinuxPodResidualReport {
	sandboxes := s.sandboxes.List()
	containers := s.containers.List()
	return buildLinuxPodResidualReport(ctx, residualInputs{
		sandboxes:      sandboxes,
		containers:     containers,
		backend:        s.backend,
		ipam:           s.ipam,
		networkEnabled: s.networkEnabled(),
		now:            s.now,
	})
}

// DiagnoseLinuxPodStores builds a residual-state report directly from persisted
// stores and an optional backend, for the standalone `macvz-cri
// --diagnose-linuxpod` CLI path where no live service exists. backend may be nil,
// which leaves sandbox liveness unprobed. It is read-only: it never mutates a
// record, an IP reservation, or a host route. Network residual fields come from
// the persisted records (the authoritative reservation view a restart rebuilds
// IPAM from), so no live IPAM is required.
func DiagnoseLinuxPodStores(ctx context.Context, sandboxes *store.Store, containers *store.ContainerStore, backend linuxpod.Backend) LinuxPodResidualReport {
	return buildLinuxPodResidualReport(ctx, residualInputs{
		sandboxes:      sandboxes.List(),
		containers:     containers.List(),
		backend:        backend,
		networkEnabled: false,
		now:            time.Now,
	})
}

// residualInputs are the dependencies of buildLinuxPodResidualReport, shared by
// the service Diagnose method and the cmd/macvz-cri standalone diagnostic so both
// classify identically.
type residualInputs struct {
	sandboxes      []store.Sandbox
	containers     []store.Container
	backend        linuxpod.Backend
	ipam           PodIPAllocator
	networkEnabled bool
	now            func() time.Time
}

// buildLinuxPodResidualReport classifies the snapshot in residualInputs. It is a
// pure-ish read: the only side effects are read-only backend probes and os.Stat on
// mount sources. backend may be nil (no helper socket), which leaves sandbox
// liveness unprobed rather than guessing.
func buildLinuxPodResidualReport(ctx context.Context, in residualInputs) LinuxPodResidualReport {
	now := in.now
	if now == nil {
		now = time.Now
	}
	rep := LinuxPodResidualReport{
		GeneratedAt:    now().UnixNano(),
		BackendProbed:  in.backend != nil,
		NetworkEnabled: in.networkEnabled,
		Summary:        map[LinuxPodResidualCategory]int{},
	}

	// Index which sandbox ids actually have a record, so a container can be flagged
	// orphaned when its owner is gone (partial-removal state).
	sandboxExists := make(map[string]bool, len(in.sandboxes))
	sandboxCategories := make(map[string]LinuxPodResidualCategory, len(in.sandboxes))
	for _, sb := range in.sandboxes {
		sandboxExists[sb.ID] = true
	}

	containersBySandbox := make(map[string][]string)
	for _, c := range in.containers {
		containersBySandbox[c.SandboxID] = append(containersBySandbox[c.SandboxID], c.ID)
	}

	for _, sb := range in.sandboxes {
		sb := sb
		live, probeErr := probeSandboxBackend(ctx, in.backend, sb.ID)
		cat := classifySandbox(sb.State, in.backend != nil, live, probeErr)
		sandboxCategories[sb.ID] = cat
		entry := LinuxPodSandboxResidual{
			SandboxID:         sb.ID,
			Namespace:         sb.Metadata.Namespace,
			Name:              sb.Metadata.Name,
			UID:               sb.Metadata.UID,
			State:             string(sb.State),
			Category:          cat,
			BackendLive:       live,
			LinuxPodNamespace: sb.LinuxPodNamespace,
			Network:           sandboxNetworkResidual(&sb, in.ipam),
			ContainerIDs:      containersBySandbox[sb.ID],
		}
		if probeErr != nil {
			entry.BackendProbeError = probeErr.Error()
		}
		rep.Sandboxes = append(rep.Sandboxes, entry)
		rep.Summary[cat]++
	}

	for _, c := range in.containers {
		c := c
		cat := classifyContainer(c.State, sandboxExists[c.SandboxID], sandboxCategories[c.SandboxID])
		entry := LinuxPodContainerResidual{
			ContainerID: c.ID,
			SandboxID:   c.SandboxID,
			Name:        c.Metadata.Name,
			State:       string(c.State),
			Reason:      c.Reason,
			Category:    cat,
			Mounts:      mountResiduals(c.Mounts),
		}
		if c.LinuxPod != nil {
			entry.BackendContainerID = c.LinuxPod.BackendContainerID
		}
		rep.Containers = append(rep.Containers, entry)
		rep.Summary[cat]++
	}

	sort.Slice(rep.Sandboxes, func(i, j int) bool { return rep.Sandboxes[i].SandboxID < rep.Sandboxes[j].SandboxID })
	sort.Slice(rep.Containers, func(i, j int) bool { return rep.Containers[i].ContainerID < rep.Containers[j].ContainerID })
	return rep
}

// probeSandboxBackend reports whether the helper still backs a sandbox. A nil
// backend means "not probed" (live=false, err=nil). A missing-pod error means the
// backend lost it (live=false, err=nil). Any other error is indeterminate and
// returned so the caller can record it.
func probeSandboxBackend(ctx context.Context, backend linuxpod.Backend, sandboxID string) (bool, error) {
	if backend == nil {
		return false, nil
	}
	if _, err := backend.PodStatus(ctx, sandboxID); err == nil {
		return true, nil
	} else if linuxpodBackendMissing(err) {
		return false, nil
	} else {
		return false, err
	}
}

func classifySandbox(state store.State, probed, live bool, probeErr error) LinuxPodResidualCategory {
	if state == store.StateNotReady {
		return ResidualSandboxNotReady
	}
	switch {
	case !probed:
		return ResidualSandboxReadyBackendUnprobed
	case probeErr != nil:
		return ResidualSandboxReadyBackendError
	case live:
		return ResidualSandboxReadyBackendLive
	default:
		return ResidualSandboxReadyBackendLost
	}
}

func classifyContainer(state store.ContainerState, sandboxExists bool, sandboxCategory LinuxPodResidualCategory) LinuxPodResidualCategory {
	if !sandboxExists {
		return ResidualContainerOrphaned
	}
	switch state {
	case store.ContainerExited:
		return ResidualContainerExitedRetained
	case store.ContainerRunning:
		switch sandboxCategory {
		case ResidualSandboxReadyBackendLive:
			return ResidualContainerRunningBackendLive
		case ResidualSandboxReadyBackendUnprobed:
			return ResidualContainerRunningBackendUnprobed
		case ResidualSandboxReadyBackendError:
			return ResidualContainerRunningBackendError
		default:
			return ResidualContainerRunningBackendLost
		}
	default:
		return ResidualContainerCreatedRetained
	}
}

func sandboxNetworkResidual(sb *store.Sandbox, ipam PodIPAllocator) LinuxPodNetworkResidual {
	res := LinuxPodNetworkResidual{
		PodIP:     sb.Network.PodIP,
		VMIP:      sb.Network.VMIP,
		Interface: sb.Network.Interface,
		Attached:  sb.Network.Attached,
	}
	if ipam != nil {
		res.IPReservedForKey = ipam.IP(sandboxKey(sb)) != ""
	} else {
		// Without a live IPAM the persisted Pod IP is the authoritative reservation
		// view: RecoverNetwork rebuilds IPAM from exactly this field on restart.
		res.IPReservedForKey = sb.Network.PodIP != ""
	}
	return res
}

func mountResiduals(mounts []store.Mount) []LinuxPodMountResidual {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]LinuxPodMountResidual, 0, len(mounts))
	for _, m := range mounts {
		entry := LinuxPodMountResidual{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			ReadOnly:      m.ReadOnly,
			Tmpfs:         m.Tmpfs,
		}
		if m.HostPath != "" {
			if _, err := os.Stat(m.HostPath); err == nil {
				entry.HostPathExists = true
			}
		}
		out = append(out, entry)
	}
	return out
}
