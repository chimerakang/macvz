package criserver

import (
	"context"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"k8s.io/klog/v2"
)

// This file implements CRI-P7 adapter restart recovery (#79). The sandbox and
// container stores already reload their JSON records on startup, so a restarted
// macvz-cri rediscovers what it was running. Recovery closes the remaining gaps so
// the kubelet's view stays consistent across a restart:
//
//   - Container state is reconciled against the live apple/container workload, so a
//     container that exited while the adapter was down is reported Exited with its
//     real exit code instead of a stale Running.
//   - Log pumps are restarted for containers still Running, so `kubectl logs`
//     keeps working after a restart without waiting for the next StartContainer.
//
// Recovery never creates or starts workloads: deterministic workload IDs plus the
// one-live-container-per-sandbox guard in CreateContainer already prevent a restart
// from duplicating a running workload. Recovery only observes and reconciles, so it
// cannot orphan or double-run a Pod.

// RecoverContainers reconciles persisted containers against their live workloads
// and restarts log pumps for containers that are still Running. It is best-effort:
// a per-container failure is logged and skipped rather than failing adapter
// startup. Call it once at startup, after the stores load and the runtime is wired.
// With no runtime configured it is a no-op, matching the honest-but-inert skeleton.
func (s *Server) RecoverContainers(ctx context.Context) {
	if s.containerRuntime == nil {
		return
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	reconciled, exited, pumps := 0, 0, 0
	for _, c := range s.containers.List() {
		updated, changed := s.reconcile(ctx, c)
		if changed {
			if err := s.containers.Put(&updated); err != nil {
				klog.ErrorS(err, "CRI restart recovery: failed to persist reconciled container", "containerID", c.ID)
				continue
			}
			c = updated
			reconciled++
			if c.State == store.ContainerExited {
				exited++
				// A container that exited while the adapter was down must release its
				// Pod network path so a later sandbox teardown does not leak host rules.
				if err := s.detachContainerNetwork(ctx, c.SandboxID, c.ID, "RecoverContainers"); err != nil {
					klog.ErrorS(err, "CRI restart recovery: failed to detach network for exited container", "containerID", c.ID)
				}
			}
		}

		// Resume logging for a container that is still Running so `kubectl logs`
		// works immediately after the restart, not only after the next start.
		if c.State == store.ContainerRunning {
			if sb, ok := s.sandboxes.Get(c.SandboxID); ok {
				s.startLogPump(&c, &sb)
				pumps++
			}
		}
	}
	if reconciled > 0 || pumps > 0 {
		klog.InfoS("recovered CRI container state after restart",
			"reconciled", reconciled, "exited", exited, "resumedLogPumps", pumps)
	}
}
