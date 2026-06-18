package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// CreatePod launches each of a Pod's containers as a runtime workload.
//
// It is idempotent: a Pod already tracked by the provider is treated as a
// successful no-op (Virtual Kubelet may re-issue CreatePod on reconcile). On a
// partial failure the started workloads are rolled back and nothing is stored,
// so a later retry starts from a clean slate.
func (p *Provider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	key := podKey(pod.Namespace, pod.Name)

	p.mu.RLock()
	_, exists := p.pods[key]
	p.mu.RUnlock()
	if exists {
		klog.InfoS("CreatePod is a no-op for an already-tracked Pod", "pod", key)
		return nil
	}

	// Translate the Pod into a single workload spec. An unsupported shape is
	// terminal: record a clear Failed status instead of retrying forever.
	spec, err := translatePod(pod)
	if err != nil {
		return p.markTerminalFailure(key, pod, "UnsupportedPodSpec", err)
	}
	c := pod.Spec.Containers[0]

	st := &podState{pod: pod.DeepCopy()}
	now := metav1.Now()
	st.pod.Status.StartTime = &now

	if err := p.rt.Pull(ctx, spec.Image); err != nil {
		// An image with no arm64 variant can never run here (P1 surfaces this
		// signal); make it a terminal Failed status. Other pull errors (e.g.
		// runtime not ready) are transient and worth retrying.
		if errors.Is(err, runtime.ErrIncompatibleArch) {
			return p.markTerminalFailure(key, pod, "ImageArchitectureMismatch",
				fmt.Errorf("pull image %q for container %q: %w", spec.Image, c.Name, err))
		}
		return fmt.Errorf("pull image %q for container %q: %w", spec.Image, c.Name, err)
	}
	id, err := p.rt.Create(ctx, spec)
	if err != nil {
		p.rollback(ctx, st)
		return fmt.Errorf("create container %q: %w", c.Name, err)
	}
	st.workloads = append(st.workloads, workload{container: c.Name, id: id})
	if err := p.rt.Start(ctx, id); err != nil {
		p.rollback(ctx, st)
		return fmt.Errorf("start container %q: %w", c.Name, err)
	}

	st.pod.Status = p.reconcileStatus(ctx, st)
	p.mu.Lock()
	p.pods[key] = st
	p.mu.Unlock()
	klog.InfoS("created Pod", "pod", key, "workloadID", spec.Name)
	return nil
}

// markTerminalFailure records a sticky Failed status for a Pod that can never
// run on this node, then returns nil so Virtual Kubelet stops retrying and
// surfaces the status/message to Kubernetes (no silent partial behavior).
func (p *Provider) markTerminalFailure(key string, pod *corev1.Pod, reason string, cause error) error {
	now := metav1.Now()
	status := corev1.PodStatus{
		Phase:     corev1.PodFailed,
		Reason:    reason,
		Message:   cause.Error(),
		StartTime: &now,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodInitialized, Status: corev1.ConditionFalse, Reason: reason},
			{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: reason},
			{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, Reason: reason},
		},
	}
	st := &podState{pod: pod.DeepCopy(), terminalStatus: &status}
	st.pod.Status = status
	p.mu.Lock()
	p.pods[key] = st
	p.mu.Unlock()
	klog.ErrorS(cause, "Pod cannot run on this node", "pod", key, "reason", reason)
	return nil
}

// rollback destroys any workloads already started for a failed CreatePod.
func (p *Provider) rollback(ctx context.Context, st *podState) {
	for _, w := range st.workloads {
		if err := p.rt.Destroy(ctx, w.id); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			klog.ErrorS(err, "rollback: failed to destroy workload", "id", w.id, "container", w.container)
		}
	}
}

// UpdatePod reconciles metadata for a tracked Pod. Container specs are treated
// as immutable in the MVP: only the stored Pod's labels/annotations are
// refreshed; in-place spec changes are out of scope (see #17).
func (p *Provider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	key := podKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.pods[key]
	if !ok {
		return errdefs.NotFoundf("pod %q is not known to this node", key)
	}
	st.pod.Labels = pod.Labels
	st.pod.Annotations = pod.Annotations
	klog.InfoS("updated Pod metadata", "pod", key)
	return nil
}

// DeletePod tears down the workloads backing a Pod and forgets it.
//
// It is idempotent: deleting an unknown Pod returns an errdefs.NotFound error,
// which Virtual Kubelet treats as already-deleted. Workloads that the runtime
// already lost (ErrNotFound) are tolerated.
func (p *Provider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	key := podKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	st, ok := p.pods[key]
	if ok {
		delete(p.pods, key)
	}
	p.mu.Unlock()
	if !ok {
		return errdefs.NotFoundf("pod %q is not known to this node", key)
	}

	timeout := stopTimeout(pod)
	for _, w := range st.workloads {
		if err := p.rt.Stop(ctx, w.id, timeout); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			klog.ErrorS(err, "DeletePod: failed to stop workload", "id", w.id, "container", w.container)
		}
		if err := p.rt.Destroy(ctx, w.id); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			klog.ErrorS(err, "DeletePod: failed to destroy workload", "id", w.id, "container", w.container)
		}
	}
	klog.InfoS("deleted Pod", "pod", key, "containers", len(st.workloads))
	return nil
}

// GetPod returns a copy of the tracked Pod, or an errdefs.NotFound error.
func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	key := podKey(namespace, name)
	p.mu.RLock()
	st, ok := p.pods[key]
	p.mu.RUnlock()
	if !ok {
		return nil, errdefs.NotFoundf("pod %q is not known to this node", key)
	}
	return st.pod.DeepCopy(), nil
}

// GetPodStatus reconciles the Pod's status from observed runtime state and
// returns a copy. Returns an errdefs.NotFound error for an unknown Pod.
func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	key := podKey(namespace, name)
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.pods[key]
	if !ok {
		return nil, errdefs.NotFoundf("pod %q is not known to this node", key)
	}
	st.pod.Status = p.reconcileStatus(ctx, st)
	return st.pod.Status.DeepCopy(), nil
}

// GetPods returns copies of all tracked Pods with freshly reconciled status.
func (p *Provider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*corev1.Pod, 0, len(p.pods))
	for _, st := range p.pods {
		st.pod.Status = p.reconcileStatus(ctx, st)
		out = append(out, st.pod.DeepCopy())
	}
	return out, nil
}
