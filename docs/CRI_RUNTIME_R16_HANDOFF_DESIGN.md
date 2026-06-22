# CRI-R16 Production Evidence Handoff Design (#108)

Date: 2026-06-22

Outcome: `runtimeHandoffDesignAccepted`

## Context

CRI-R15 proved that a late prepared-rootfs process can report rootfs identity to
MacVz/vminitd through an explicit shared handoff path. The live probe outcome was
`vminitdRootfsPrimitiveLaunchSucceeded`:

```text
processStartSucceeded=true
processExitCode=0
resultVerified=true
```

The important R15 lesson is that the process-local path
`/macvz-r9-result` is not a reliable post-exit verification channel from
vminitd. A runtime-managed handoff path is reliable because it is prepared in a
vminitd-visible location and bind-mounted into the late rootfs.

## Design Decision

MacVz should model the evidence/result handoff as a runtime-private per-container
directory, not as a kubelet-visible volume and not as a Kubernetes API surface.

The handoff is part of the runtime's container launch contract:

1. The runtime prepares a per-container host-visible guest path before process
   creation.
2. The runtime injects that path into the OCI spec as a bind mount.
3. The launched container writes runtime-owned evidence or result files there.
4. The runtime reads those files through vminitd after process start or exit.
5. The runtime removes the handoff directory during container cleanup.

This preserves the current architecture boundary: Kubernetes remains the control
plane, MacVz remains a node/runtime provider, and apple/container assumptions stay
inside the runtime integration layer.

## Path Layout

Production paths should avoid the R9/R15 probe names and use a MacVz-owned
runtime namespace:

```text
/run/macvz/containers/<containerID>/
  rootfs/
  handoff/
    identity
    start-result
    exit-result
    stderr
```

Recommended mappings:

```text
guest prepared rootfs: /run/macvz/containers/<containerID>/rootfs
guest handoff source:  /run/macvz/containers/<containerID>/handoff
container mount point: /run/macvz/handoff
```

`<containerID>` should be the runtime workload ID or a sanitized deterministic
name derived from the CRI container ID. It must not include `/`, `..`, or shell
metacharacters. The existing `store.DeriveWorkloadID` shape is the right source
for this once production CRI wiring reaches this path.

## Ownership

`pkg/runtime` owns the handoff lifecycle.

The CRI server should not create handoff directories directly. The CRI server
maps CRI requests into runtime specs and persists container state; the runtime
driver owns runtime-private paths, vminitd Copy usage, OCI mount injection, and
cleanup.

Ownership by operation:

| Operation | Owner | Responsibility |
| --- | --- | --- |
| CreateContainer / runtime Create | runtime | Create rootfs and handoff directories, copy/stage rootfs content, inject bind mount into OCI spec, persist runtime-local metadata if needed. |
| StartContainer / runtime Start | runtime | Start the late process, read required start evidence from handoff, return a precise error if identity evidence is missing or mismatched. |
| StopContainer / runtime Stop | runtime | Stop process/VM and leave handoff files available for status/debug until RemoveContainer. |
| RemoveContainer / runtime Destroy | runtime | Delete process/container state, rootfs, handoff directory, temporary archives, and runtime metadata. Missing paths are tolerated. |
| ContainerStatus / runtime Status | runtime plus CRI server | Runtime reports process state and optional handoff-derived message; CRI server persists user-facing state and exposes debug info only under verbose status. |

## Bind Mount Shape

The OCI spec should include a writable bind mount:

```text
type: bind
source: /run/macvz/containers/<containerID>/handoff
destination: /run/macvz/handoff
options: ["rbind", "rw"]
```

The destination should be runtime-private and unlikely to collide with normal
images. It should not be exposed as a CRI mount, and kubelet-provided mounts must
not be allowed to target the same path.

The process should write evidence to the mounted path, not to the prepared rootfs
path. The prepared rootfs may still contain an identity file such as
`/etc/macvz-container-identity`; that file proves which rootfs was used, while the
handoff path proves the process reported that fact back to the runtime.

## Permissions

The handoff directory must be writable by the container's configured process
user. R15 showed that root-owned `0755` directories cause `Permission denied`
when the late process does not run as root.

Recommended initial policy:

- directory mode: `0777` for the handoff directory in the guest;
- files written by the container are runtime-owned evidence and never mounted
  into other containers;
- parent directories remain runtime-owned and not generally writable;
- the runtime sanitizes and fixes ownership/mode before launch;
- future hardening may narrow this to the container's `runAsUser/runAsGroup` once
  user mapping is implemented in the LinuxPod runtime path.

`0777` is acceptable for the first production implementation because the handoff
directory is private to one container and deleted with that container. It is not a
shared Pod volume and not a host filesystem escape.

## Evidence Files

Minimum required file:

```text
/run/macvz/handoff/identity
```

Recommended content:

```text
containerID=<containerID>
rootfsIdentity=<expected identity>
pid=<optional guest pid>
startedAt=<optional unix nanos or RFC3339 timestamp>
```

For the first implementation, the same line-oriented format used by R15 is fine:

```text
identity=macvz-r9-id=late-alpha
expected=macvz-r9-id=late-alpha
```

The runtime should parse the file with exact key matching, not substring matching.
The success condition is:

```text
identity == expected
```

Namespace diagnostics such as `proc_root=/` are useful debug evidence but should
not be required for success. R15 proved that `proc_root` reflects the container's
private mount namespace and does not expose the host-visible rootfs path.

## Failure Behavior

The runtime must fail early and explicitly:

| Failure | Runtime behavior | CRI mapping |
| --- | --- | --- |
| Cannot create handoff directory | Create returns error; no container record should be persisted. | `CreateContainer` returns Internal or FailedPrecondition with clear detail. |
| Cannot inject bind mount | Create returns error; cleanup staged rootfs/handoff. | `CreateContainer` fails. |
| Process exits before writing evidence | Start returns error, captures stderr/diagnostics when available, marks container Exited if the process was created. | `StartContainer` fails or subsequent status reports Exited with message. |
| Evidence file missing | Start returns a runtime identity error. | `StartContainer` fails with FailedPrecondition-like runtime error text. |
| Evidence mismatched | Start returns a runtime identity mismatch error. | `StartContainer` fails; status message includes expected/observed identity. |
| Cleanup path missing | Destroy treats missing paths as success. | `RemoveContainer` remains idempotent. |

If a container exits after a successful start, status should not require rereading
identity evidence. The evidence is a start invariant. Exit code, reason, and logs
remain the normal status/debug channels.

## CRI Lifecycle Mapping

### CreateContainer

Future LinuxPod-backed CreateContainer should:

1. allocate or derive the workload/container runtime ID;
2. create `/run/macvz/containers/<id>/rootfs`;
3. create `/run/macvz/containers/<id>/handoff`;
4. make the handoff directory writable by the configured container user;
5. stage the prepared rootfs and identity file;
6. inject the handoff bind mount into the OCI spec;
7. create the late container process;
8. persist only enough CRI state to recover the workload and status.

### StartContainer

StartContainer should:

1. start the late container process;
2. wait for the start evidence file within a bounded timeout;
3. read it through vminitd-visible state;
4. verify identity;
5. mark the container Running only after identity verification succeeds.

For long-running containers, the evidence file is a readiness-of-launch signal,
not application readiness. Kubernetes readiness probes remain separate.

### StopContainer

StopContainer should stop the process/VM and record exit information. It should
not delete the handoff directory, because status and debug evidence may still be
needed until RemoveContainer.

### RemoveContainer

RemoveContainer should delete:

- vminitd process/container state;
- prepared rootfs;
- handoff directory;
- temporary archives;
- runtime metadata for this container.

Cleanup is idempotent. Missing guest paths are tolerated and logged at debug
level.

### ContainerStatus

ContainerStatus should expose only stable CRI state by default. Verbose status can
include runtime-private diagnostics such as:

```text
handoffPath=/run/macvz/containers/<id>/handoff
identityVerified=true
identitySource=handoff
```

The handoff path must not become a user-visible mount or Kubernetes API contract.

## Implementation Plan

Split implementation after this design:

1. Add a LinuxPod runtime-local handoff helper with hermetic unit tests for path
   derivation, sanitization, permissions, and cleanup.
2. Extend the experimental LinuxPod rootfs launch path to create/inject the
   handoff bind mount and verify identity.
3. Add gated integration coverage based on the R9/R15 harness.
4. Wire CRI lifecycle only after the runtime path can prove create/start/status
   and cleanup without relying on harness-only code.

## Decision

The R15 evidence is strong enough to accept the design. No additional harness
probe is required before beginning the runtime implementation tasks.

Accepted outcome:

```text
runtimeHandoffDesignAccepted
```
