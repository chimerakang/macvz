package provider

import (
	"context"
	"errors"
	"time"

	"github.com/chimerakang/macvz/pkg/network/podnet"
	"github.com/chimerakang/macvz/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// Restart loop tuning (#45). A workload governed by an Always or OnFailure
// restart policy is rebuilt as a fresh micro-VM when it exits. Repeated exits
// back off exponentially from defaultRestartBackoffBase up to restartBackoffMax,
// mirroring the kubelet's CrashLoopBackOff so a hot-looping container does not
// pin the host. restartOpTimeout bounds a single restart's runtime I/O.
const (
	defaultRestartBackoffBase = 10 * time.Second
	restartBackoffMax         = 5 * time.Minute
	restartOpTimeout          = 2 * time.Minute
)

// effectiveRestartPolicy returns the Pod's restart policy, defaulting an unset
// value to Always to match Kubernetes, which defaults it on write so a real Pod
// rarely arrives empty.
func effectiveRestartPolicy(pod *corev1.Pod) corev1.RestartPolicy {
	if pod.Spec.RestartPolicy == "" {
		return corev1.RestartPolicyAlways
	}
	return pod.Spec.RestartPolicy
}

// shouldRestart reports whether a container that terminated with exitCode should
// be restarted under policy: Always restarts on any exit, OnFailure only on a
// non-zero exit, Never never.
func shouldRestart(policy corev1.RestartPolicy, exitCode int) bool {
	switch policy {
	case corev1.RestartPolicyAlways:
		return true
	case corev1.RestartPolicyOnFailure:
		return exitCode != 0
	default: // Never (and any unrecognized value, which translatePod rejects)
		return false
	}
}

// restartBackoff returns the delay before a restart given how many restarts have
// already completed: base for the first, doubling each time, capped at
// restartBackoffMax.
func restartBackoff(base time.Duration, count int32) time.Duration {
	d := base
	for i := int32(0); i < count; i++ {
		d *= 2
		if d >= restartBackoffMax {
			return restartBackoffMax
		}
	}
	return d
}

// maybeTriggerRestart decides whether a terminated (or lost) container should be
// restarted and, if so, launches the restart asynchronously. It must be called
// with Provider.mu held (reconcileStatus's callers hold it), as it reads and
// mutates restart bookkeeping on st. It returns true when the container is
// restarting — already in flight or just scheduled — so the caller reports a
// CrashLoopBackOff Waiting state instead of a terminal one.
func (p *Provider) maybeTriggerRestart(st *podState, container, oldID string, exitCode int) bool {
	if !shouldRestart(st.restartPolicy, exitCode) {
		return false
	}
	if st.restarting {
		return true
	}
	st.restarting = true
	backoff := restartBackoff(p.restartBackoffBase, st.restarts[container])
	go p.restartWorkload(podKey(st.pod.Namespace, st.pod.Name), oldID, backoff)
	return true
}

// restartWorkload rebuilds a Pod's single workload as a fresh micro-VM after its
// previous instance exited. It waits out the backoff, tears down the dead VM and
// its stale Pod-network mapping, creates and starts a new VM from the stored
// spec, and re-attaches it to the Pod network so the Pod stays reachable at its
// stable Pod IP. It runs in its own goroutine off a background context and
// commits the result through finishRestart.
func (p *Provider) restartWorkload(key, oldID string, backoff time.Duration) {
	if backoff > 0 {
		timer := time.NewTimer(backoff)
		<-timer.C
	}

	ctx, cancel := context.WithTimeout(context.Background(), restartOpTimeout)
	defer cancel()

	// Snapshot what the restart needs, and bail if the Pod was deleted or is
	// terminal (it can never run) while we were waiting out the backoff.
	p.mu.Lock()
	st, ok := p.pods[key]
	if !ok || st.terminalStatus != nil {
		if ok {
			st.restarting = false
		}
		p.mu.Unlock()
		return
	}
	spec := st.spec
	podIP := st.pod.Status.PodIP
	wasAttached := st.attached
	// Cancel the dying workload's probers; fresh ones start against the new
	// micro-VM in finishRestart so startup/readiness are re-evaluated (#50).
	p.stopProbes(st)
	p.mu.Unlock()

	pn := p.podNetRef()

	// Drop the stale network mapping pointing at the dead micro-VM, then destroy
	// it. A workload the runtime already lost (ErrNotFound) is tolerated.
	if wasAttached && pn != nil {
		if err := pn.Detach(ctx, key); err != nil {
			klog.ErrorS(err, "restart: detach stale network mapping", "pod", key)
		}
	}
	_ = p.rt.Stop(ctx, oldID, defaultStopTimeout)
	if err := p.rt.Destroy(ctx, oldID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		klog.ErrorS(err, "restart: destroy exited workload", "pod", key, "id", oldID)
	}

	// Recreate and start the workload from the retained spec. The image is
	// already local, so no pull is needed. On failure, clear the in-flight flag
	// and let the next status reconcile retry.
	newID, err := p.rt.Create(ctx, spec)
	if err != nil {
		klog.ErrorS(err, "restart: create workload", "pod", key)
		p.finishRestart(key, oldID, "", false, "")
		return
	}
	if err := p.rt.Start(ctx, newID); err != nil {
		klog.ErrorS(err, "restart: start workload", "pod", key)
		if derr := p.rt.Destroy(ctx, newID); derr != nil && !errors.Is(derr, runtime.ErrNotFound) {
			klog.ErrorS(derr, "restart: destroy half-started workload", "pod", key, "id", newID)
		}
		p.finishRestart(key, oldID, "", false, "")
		return
	}

	// Re-attach the new micro-VM to the Pod network path. The VM gets a new
	// host-only address over DHCP, so the mapping must be rebuilt; the Pod IP is
	// stable. A missing VM address leaves the Pod running but not yet reachable —
	// the next reconcile re-observes it (the Pod is not failed for this).
	vmIP := ""
	attached := false
	if pn != nil && podIP != "" {
		if vmIP = p.observeVMIPByID(ctx, newID); vmIP != "" {
			if err := pn.Attach(ctx, podnet.Endpoint{PodKey: key, PodIP: podIP, VMIP: vmIP}); err != nil {
				klog.ErrorS(err, "restart: attach network path", "pod", key, "podIP", podIP, "vmIP", vmIP)
				vmIP = ""
			} else {
				attached = true
			}
		} else {
			klog.InfoS("restart: micro-VM address not yet available; network re-attach deferred", "pod", key)
		}
	}
	p.finishRestart(key, oldID, newID, attached, vmIP)
}

// finishRestart commits the outcome of a restart attempt: on success it swaps in
// the new workload ID, increments the container's restart count, and records the
// new network state; on failure (newID == "") it just clears the in-flight flag
// so the next reconcile retries. An orphaned new workload is destroyed when the
// Pod was deleted mid-restart.
func (p *Provider) finishRestart(key, oldID, newID string, attached bool, vmIP string) {
	p.mu.Lock()
	st, ok := p.pods[key]
	if !ok {
		p.mu.Unlock()
		if newID != "" {
			// The Pod was deleted while we were restarting; reap the orphan VM.
			ctx, cancel := context.WithTimeout(context.Background(), restartOpTimeout)
			defer cancel()
			if err := p.rt.Destroy(ctx, newID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
				klog.ErrorS(err, "restart: destroy orphaned workload after pod deletion", "pod", key, "id", newID)
			}
		}
		return
	}
	defer p.mu.Unlock()

	st.restarting = false
	if newID == "" {
		return
	}
	for i := range st.workloads {
		if st.workloads[i].id == oldID {
			st.workloads[i].id = newID
		}
	}
	if st.restarts == nil {
		st.restarts = map[string]int32{}
	}
	if len(st.pod.Spec.Containers) > 0 {
		st.restarts[st.pod.Spec.Containers[0].Name]++
	}
	st.vmIP = vmIP
	st.attached = attached
	// Re-arm probers against the fresh micro-VM (#50).
	p.startProbes(st)
	klog.InfoS("restarted Pod workload", "pod", key, "oldID", oldID, "newID", newID,
		"restartCount", st.restarts[st.pod.Spec.Containers[0].Name])
}
