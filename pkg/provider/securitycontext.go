package provider

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/chimerakang/macvz/internal/types"
	corev1 "k8s.io/api/core/v1"
)

// securityContext (#52)
//
// MacVz runs each Pod in a dedicated Linux micro-VM (its own kernel, hardware
// isolation), which is a stronger boundary than a shared-kernel container. The
// provider maps the securityContext fields apple/container can honor onto runtime
// flags, accepts the fields the VM boundary already satisfies, and rejects — with
// a clear, terminal status — the fields it cannot honor, so a request never
// silently no-ops.
//
//   - Mapped to the runtime: runAsUser/runAsGroup (--user),
//     readOnlyRootFilesystem (--read-only), capabilities add/drop
//     (--cap-add/--cap-drop).
//   - Accepted as a no-op (satisfied by VM isolation, documented): privileged
//     false, allowPrivilegeEscalation, runAsNonRoot (enforced via runAsUser),
//     seccomp/appArmor RuntimeDefault and Unconfined, fsGroup/supplementalGroups.
//   - Rejected: privileged true, seLinuxOptions, windowsOptions, procMount other
//     than Default, seccomp/appArmor Localhost, sysctls, and contradictory
//     identity requests (runAsNonRoot with runAsUser 0; runAsGroup without
//     runAsUser).

// securityContextReasons returns a reason for every securityContext field on the
// Pod (pod-level and on its single container) that MacVz cannot honor. An empty
// result means the securityContext is fully supported. Validating both levels
// keeps an unsupported field a hard, explicit failure rather than a silent no-op.
func securityContextReasons(pod *corev1.Pod) []string {
	var reasons []string
	if len(pod.Spec.Containers) == 0 {
		return reasons
	}
	c := pod.Spec.Containers[0]
	psc := pod.Spec.SecurityContext
	csc := c.SecurityContext

	if psc != nil {
		if len(psc.Sysctls) > 0 {
			reasons = append(reasons, "securityContext.sysctls are not supported (MacVz does not set guest sysctls)")
		}
		if psc.SELinuxOptions != nil {
			reasons = append(reasons, "securityContext.seLinuxOptions are not supported (SELinux is not applied to micro-VM guests)")
		}
		if psc.WindowsOptions != nil {
			reasons = append(reasons, "securityContext.windowsOptions are not supported on a Linux node")
		}
		if !seccompSupported(psc.SeccompProfile) {
			reasons = append(reasons, "securityContext.seccompProfile type Localhost is not supported (a custom profile cannot be loaded into the guest)")
		}
		if !appArmorSupported(psc.AppArmorProfile) {
			reasons = append(reasons, "securityContext.appArmorProfile type Localhost is not supported (a custom profile cannot be loaded into the guest)")
		}
	}

	if csc != nil {
		if csc.Privileged != nil && *csc.Privileged {
			reasons = append(reasons, fmt.Sprintf("container %q securityContext.privileged is not supported (MacVz grants no host-device privilege; each Pod is already isolated in its own micro-VM)", c.Name))
		}
		if csc.SELinuxOptions != nil {
			reasons = append(reasons, fmt.Sprintf("container %q securityContext.seLinuxOptions are not supported", c.Name))
		}
		if csc.WindowsOptions != nil {
			reasons = append(reasons, fmt.Sprintf("container %q securityContext.windowsOptions are not supported on a Linux node", c.Name))
		}
		if csc.ProcMount != nil && *csc.ProcMount != corev1.DefaultProcMount {
			reasons = append(reasons, fmt.Sprintf("container %q securityContext.procMount %q is not supported (only Default)", c.Name, *csc.ProcMount))
		}
		if !seccompSupported(csc.SeccompProfile) {
			reasons = append(reasons, fmt.Sprintf("container %q securityContext.seccompProfile type Localhost is not supported", c.Name))
		}
		if !appArmorSupported(csc.AppArmorProfile) {
			reasons = append(reasons, fmt.Sprintf("container %q securityContext.appArmorProfile type Localhost is not supported", c.Name))
		}
	}

	// Identity is resolved across both levels (container over pod), then checked
	// for requests the runtime's single --user spec cannot express.
	uid := effectiveRunAsUser(psc, csc)
	gid := effectiveRunAsGroup(psc, csc)
	nonRoot := effectiveRunAsNonRoot(psc, csc)
	if gid != nil && uid == nil {
		reasons = append(reasons, fmt.Sprintf("container %q securityContext.runAsGroup requires runAsUser to be set (the runtime sets the group through the user spec)", c.Name))
	}
	if nonRoot != nil && *nonRoot && uid != nil && *uid == 0 {
		reasons = append(reasons, fmt.Sprintf("container %q sets runAsNonRoot: true but runAsUser: 0, a conflicting request", c.Name))
	}

	return reasons
}

// applySecurityContext maps the supported securityContext fields onto the runtime
// spec: the effective user/group, a read-only root filesystem, and capability
// adjustments. It assumes securityContextReasons has already cleared the Pod, so
// the fields it reads are honorable.
func applySecurityContext(pod *corev1.Pod, c corev1.Container, spec *types.ContainerSpec) {
	psc := pod.Spec.SecurityContext
	csc := c.SecurityContext

	if uid := effectiveRunAsUser(psc, csc); uid != nil {
		spec.User = strconv.FormatInt(*uid, 10)
		if gid := effectiveRunAsGroup(psc, csc); gid != nil {
			spec.User += ":" + strconv.FormatInt(*gid, 10)
		}
	}

	if csc == nil {
		return
	}
	if csc.ReadOnlyRootFilesystem != nil && *csc.ReadOnlyRootFilesystem {
		spec.ReadOnlyRootFS = true
	}
	if caps := csc.Capabilities; caps != nil {
		for _, a := range caps.Add {
			spec.CapAdd = append(spec.CapAdd, runtimeCapName(a))
		}
		for _, d := range caps.Drop {
			spec.CapDrop = append(spec.CapDrop, runtimeCapName(d))
		}
	}
}

// runtimeCapName converts a Kubernetes capability (e.g. "NET_ADMIN", "ALL") to
// the form apple/container expects ("CAP_NET_ADMIN", "ALL").
func runtimeCapName(c corev1.Capability) string {
	s := string(c)
	if s == "ALL" || strings.HasPrefix(s, "CAP_") {
		return s
	}
	return "CAP_" + s
}

// seccompSupported reports whether a seccompProfile is one MacVz honors: unset,
// RuntimeDefault, or Unconfined. A Localhost profile names a node-local file the
// guest cannot load, so it is rejected.
func seccompSupported(p *corev1.SeccompProfile) bool {
	return p == nil || p.Type != corev1.SeccompProfileTypeLocalhost
}

// appArmorSupported mirrors seccompSupported for AppArmor profiles.
func appArmorSupported(p *corev1.AppArmorProfile) bool {
	return p == nil || p.Type != corev1.AppArmorProfileTypeLocalhost
}

// effectiveRunAsUser returns the container's runAsUser, falling back to the
// pod-level value (matching Kubernetes precedence).
func effectiveRunAsUser(psc *corev1.PodSecurityContext, csc *corev1.SecurityContext) *int64 {
	if csc != nil && csc.RunAsUser != nil {
		return csc.RunAsUser
	}
	if psc != nil {
		return psc.RunAsUser
	}
	return nil
}

func effectiveRunAsGroup(psc *corev1.PodSecurityContext, csc *corev1.SecurityContext) *int64 {
	if csc != nil && csc.RunAsGroup != nil {
		return csc.RunAsGroup
	}
	if psc != nil {
		return psc.RunAsGroup
	}
	return nil
}

func effectiveRunAsNonRoot(psc *corev1.PodSecurityContext, csc *corev1.SecurityContext) *bool {
	if csc != nil && csc.RunAsNonRoot != nil {
		return csc.RunAsNonRoot
	}
	if psc != nil {
		return psc.RunAsNonRoot
	}
	return nil
}
