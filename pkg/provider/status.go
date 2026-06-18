package provider

import (
	"context"
	"errors"

	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// reconcileStatus rebuilds a Pod's status from observed runtime state. It
// preserves the original StartTime and PodIP, queries each backing workload,
// maps runtime phases to container statuses, and aggregates the Pod phase.
func (p *Provider) reconcileStatus(ctx context.Context, st *podState) corev1.PodStatus {
	// A Pod that can never run keeps its sticky Failed status.
	if st.terminalStatus != nil {
		return *st.terminalStatus
	}

	status := corev1.PodStatus{
		StartTime: st.pod.Status.StartTime,
		PodIP:     st.pod.Status.PodIP,
	}

	byName := make(map[string]workload, len(st.workloads))
	for _, w := range st.workloads {
		byName[w.container] = w
	}

	statuses := make([]corev1.ContainerStatus, 0, len(st.pod.Spec.Containers))
	for _, c := range st.pod.Spec.Containers {
		w, ok := byName[c.Name]
		if !ok {
			statuses = append(statuses, waitingStatus(c, "NotCreated", "container has no backing workload"))
			continue
		}
		rs, err := p.rt.Status(ctx, w.id)
		if err != nil {
			if errors.Is(err, runtime.ErrNotFound) {
				// The workload vanished from the runtime; report it terminated.
				statuses = append(statuses, terminatedStatus(c, w.id, 0, "Lost", "runtime workload not found"))
				continue
			}
			// A transient inspect/runtime error should not regress an already
			// running Pod back to Pending. Preserve the last observed state when
			// available, and expose the runtime error through Pod conditions.
			if prev, ok := previousContainerStatus(st.pod.Status, c.Name); ok {
				statuses = append(statuses, prev)
				status.Message = err.Error()
				continue
			}
			statuses = append(statuses, waitingStatus(c, "RuntimeError", err.Error()))
			continue
		}
		statuses = append(statuses, containerStatus(c, w.id, rs))
		if status.PodIP == "" && rs.IP != "" {
			status.PodIP = rs.IP
		}
	}

	status.ContainerStatuses = statuses
	status.Phase = aggregatePhase(statuses)

	// Publish the Pod's address in both the singular and plural fields. The
	// EndpointSlice controller reads PodIPs, so populating it is what makes a
	// MacVz-backed Pod show up as a usable Service endpoint.
	if status.PodIP != "" {
		status.PodIPs = []corev1.PodIP{{IP: status.PodIP}}
	}
	// HostIP lets `kubectl get pod -o wide` and topology-aware routing resolve the
	// node hosting the Pod.
	if p.hostIP != "" {
		status.HostIP = p.hostIP
		status.HostIPs = []corev1.HostIP{{IP: p.hostIP}}
	}

	// Readiness drives EndpointSlice membership: a Pod is Ready only when it is
	// running AND has an address, so endpoints never point at an unreachable Pod.
	ready := status.Phase == corev1.PodRunning && status.PodIP != ""
	status.Conditions = podConditions(status.Phase, ready)
	switch {
	case status.Message != "":
		status.Conditions = withConditionReason(status.Conditions, "RuntimeStatusError", status.Message)
	case status.Phase == corev1.PodRunning && status.PodIP == "":
		status.Conditions = withConditionReason(status.Conditions, "PodNetworkNotReady",
			"waiting for Pod IP allocation and network attach")
	}
	return status
}

func previousContainerStatus(status corev1.PodStatus, name string) (corev1.ContainerStatus, bool) {
	for _, s := range status.ContainerStatuses {
		if s.Name == name {
			return *s.DeepCopy(), true
		}
	}
	return corev1.ContainerStatus{}, false
}

// containerStatus maps a single runtime status to a Kubernetes container status.
func containerStatus(c corev1.Container, id string, rs runtime.Status) corev1.ContainerStatus {
	cs := corev1.ContainerStatus{
		Name:        c.Name,
		Image:       c.Image,
		ImageID:     c.Image,
		ContainerID: "macvz://" + id,
	}
	switch rs.Phase {
	case runtime.PhaseRunning:
		cs.State.Running = &corev1.ContainerStateRunning{
			StartedAt: metav1.NewTime(rs.StartedAt),
		}
		cs.Ready = true
		cs.Started = boolPtr(true)
	case runtime.PhaseStopped:
		cs.State.Terminated = terminated(int32(rs.ExitCode), "Completed", rs.Message, id)
	case runtime.PhaseFailed:
		cs.State.Terminated = terminated(nonZero(int32(rs.ExitCode)), "Error", rs.Message, id)
	case runtime.PhaseCreated:
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "Created", Message: rs.Message}
	default: // PhaseUnknown
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "Unknown", Message: rs.Message}
	}
	return cs
}

func waitingStatus(c corev1.Container, reason, msg string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:    c.Name,
		Image:   c.Image,
		ImageID: c.Image,
		State:   corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}},
	}
}

func terminatedStatus(c corev1.Container, id string, code int32, reason, msg string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:        c.Name,
		Image:       c.Image,
		ImageID:     c.Image,
		ContainerID: "macvz://" + id,
		State:       corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason, Message: msg}},
	}
}

func terminated(code int32, reason, msg, id string) *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{
		ExitCode:    code,
		Reason:      reason,
		Message:     msg,
		ContainerID: "macvz://" + id,
	}
}

// aggregatePhase derives the Pod phase from its container statuses, following
// the kubelet's rules closely enough for the MVP:
//   - any container still waiting  -> Pending
//   - any container terminated with a non-zero exit -> Failed
//   - all containers terminated with exit 0 -> Succeeded
//   - otherwise (at least one running) -> Running
func aggregatePhase(statuses []corev1.ContainerStatus) corev1.PodPhase {
	if len(statuses) == 0 {
		return corev1.PodPending
	}
	var running, succeeded, failed, waiting int
	for _, s := range statuses {
		switch {
		case s.State.Waiting != nil:
			waiting++
		case s.State.Running != nil:
			running++
		case s.State.Terminated != nil:
			if s.State.Terminated.ExitCode == 0 {
				succeeded++
			} else {
				failed++
			}
		}
	}
	switch {
	case failed > 0:
		return corev1.PodFailed
	case waiting > 0:
		return corev1.PodPending
	case running > 0:
		return corev1.PodRunning
	case succeeded == len(statuses):
		return corev1.PodSucceeded
	default:
		return corev1.PodPending
	}
}

// podConditions returns the standard Pod conditions for a phase. Readiness is
// passed in explicitly because a Pod is only Ready when it is both running and
// addressable (see reconcileStatus), not merely running.
func podConditions(phase corev1.PodPhase, ready bool) []corev1.PodCondition {
	cond := func(t corev1.PodConditionType, status bool) corev1.PodCondition {
		s := corev1.ConditionFalse
		if status {
			s = corev1.ConditionTrue
		}
		return corev1.PodCondition{Type: t, Status: s}
	}
	// Initialized stays true once the Pod has been started; ContainersReady and
	// Ready track the live ready state.
	initialized := phase != corev1.PodPending
	return []corev1.PodCondition{
		cond(corev1.PodInitialized, initialized),
		cond(corev1.ContainersReady, ready),
		cond(corev1.PodReady, ready),
	}
}

// withConditionReason forces the readiness conditions False and annotates them
// with a reason/message, used when a running Pod is not yet a valid endpoint
// (transient runtime error, or no Pod IP yet).
func withConditionReason(conditions []corev1.PodCondition, reason, msg string) []corev1.PodCondition {
	out := append([]corev1.PodCondition(nil), conditions...)
	for i := range out {
		if out[i].Type == corev1.PodReady || out[i].Type == corev1.ContainersReady {
			out[i].Status = corev1.ConditionFalse
			out[i].Reason = reason
			out[i].Message = msg
		}
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

// nonZero ensures a failed container reports a non-zero exit code even when the
// runtime did not supply one.
func nonZero(code int32) int32 {
	if code == 0 {
		return 1
	}
	return code
}
