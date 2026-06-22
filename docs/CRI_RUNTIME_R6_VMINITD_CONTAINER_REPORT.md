# CRI-R6 vminitd Container Rootfs Process Path Report (#98)

Date: 2026-06-22T06:47:14Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: /tmp/apple-containerization
- Kernel: /tmp/apple-containerization/bin/vmlinux-arm64
- Initfs reference: vminit:latest
- Image: docker.io/library/busybox:1.36.1
- Work dir: /tmp/macvz-runtime-r6
- Probe: r6
- vmnet interface: 0

## Result

```json
{
  "attempt" : {
    "cleanupOutput" : "cleanup_ok stage=\/run\/macvz-r6\/staged\/late-alpha\n",
    "cleanupSucceeded" : true,
    "errors" : {
      "startOrWaitProcess" : "RPCError: internalError: \"startProcess: failed to start process: internalError: \"vmexec error: internalError: \" Error Domain=NSPOSIXErrorDomain Code=2 \"No such file or directory\"\"\"\"",
      "verify" : "exit 51: result_missing=\/run\/macvz-r6\/staged\/late-alpha\/rootfs\/macvz-r6-result\n"
    },
    "namespaceVerified" : false,
    "outcome" : "vminitdContainerRootfsPathFound",
    "processContainerID" : "r6-late-alpha",
    "processCreateSucceeded" : true,
    "processID" : "r6-late-alpha",
    "processStartSucceeded" : false,
    "requestID" : "late-alpha",
    "resultPath" : "\/run\/macvz-r6\/staged\/late-alpha\/rootfs\/macvz-r6-result",
    "resultVerified" : false,
    "stageOutput" : "stage_ok request=late-alpha rootfs=\/run\/macvz-r6\/staged\/late-alpha\/rootfs\n",
    "stagePath" : "\/run\/macvz-r6\/staged\/late-alpha\/rootfs",
    "stageSucceeded" : true,
    "verifyOutput" : "result_missing=\/run\/macvz-r6\/staged\/late-alpha\/rootfs\/macvz-r6-result\n"
  },
  "durationSeconds" : 1.2489889860153198,
  "errors" : {

  },
  "image" : "docker.io\/library\/busybox:1.36.1",
  "kernel" : "\/tmp\/apple-containerization\/bin\/vmlinux-arm64",
  "logs" : {
    "boot" : "\/tmp\/macvz-runtime-r6\/logs\/boot.log",
    "exec" : "\/tmp\/macvz-runtime-r6\/logs\/exec.log",
    "utility" : "\/tmp\/macvz-runtime-r6\/logs\/utility.log"
  },
  "note" : "R6 tests vminitd new-container process creation with id == containerID against a post-create staged rootfs; it does not implement production LinuxPod state integration",
  "outcome" : "vminitdContainerRootfsPathFound",
  "podCreated" : true,
  "podID" : "macvz-r6-1782110833",
  "transportAvailable" : true,
  "utilityStarted" : true,
  "workDir" : "\/tmp\/macvz-runtime-r6"
}
```

## Acceptance / Interpretation
- [x] One LinuxPod was created with a predeclared keeper utility container.
- [x] Guest agent transport was attempted only after pod.create() and utility start.
- [x] vminitd createProcess/startProcess was called with id == containerID to target the new-container path.
- [x] A post-create staged rootfs was used as the OCI root for that vminitd container path, or the precise failure boundary was recorded.
- [x] Identity is checked by explicit request ID, not by guessed /dev/sdX or /dev/vdX names.
- [x] The report distinguishes unavailable transport, staged rootfs unavailable, upstream primitive missing, exec-like rootfs constraint, cleanup failure, and success.

## Source Call Graph

Source checkout: `/tmp/apple-containerization`, commit
`6b7b42ca3efeee8c706070e4355e6a807c5336ae` (`0.34.0`).

LinuxPod's normal container path is:

1. `LinuxPod.addContainer` records a `PodContainer`. Before `pod.create()` this
   is only registered state; after `pod.create()` it attempts rootfs hotplug,
   guest mount, DNS/hosts setup, and then marks the container created.
2. `LinuxPod.create` builds `mountsByID`, boots the VM, asks the agent to mount
   each container rootfs at `/run/container/<containerID>/rootfs`, and marks
   registered containers created.
3. `LinuxPod.startContainer` generates an OCI spec whose `root.path` is
   `/run/container/<containerID>/rootfs`, creates `LinuxProcess` with
   `owningContainer == containerID`, and calls VM-agent
   `createProcess/startProcess`.
4. `vminitd` `Server+GRPC.createProcess` rejects requests without
   `containerID` using `processes in the root of the vm not implemented`.
5. If `state.containers[containerID]` already exists, `vminitd` treats the
   request as an exec and calls `ManagedContainer.createExec`. That writes only
   the OCI `process` config into the existing bundle; the supplied `root.path`,
   mounts, and Linux namespace settings are not used for a new rootfs.
6. If `state.containers[containerID]` does not exist, `vminitd` requires
   `id == containerID`, creates a new `ManagedContainer` from the full OCI spec,
   writes the bundle under `/run/container/<id>`, and starts it with
   `vmexec run --bundle-path <bundle>`.

The vmexec split explains R5 and R6:

- Init containers run through `vmexec run`, which loads the full OCI bundle,
  prepares `root.path`, mounts, and pivots root.
- Exec processes run through `vmexec exec --parent-pid ... --process-path ...`,
  which joins the existing container namespaces and uses only the process JSON.

## Interpretation

R5's `containerID=utility` fallback did not execute from the staged rootfs
because `utility` was already present in `vminitd` state. The request therefore
entered the exec path, and `ManagedContainer.createExec` preserved only the
process section of the submitted OCI spec. This is an intentional state-model
branch rather than a missing flag on the R5 request.

R6 changed the request to `id == containerID == r6-late-alpha`, which targets the
new-container branch. Live evidence confirms `createProcess` succeeded and the
boot log shows a new cgroup and `created vmexec init process` for
`r6-late-alpha`. That is the lower-level vminitd container/rootfs path.

The process still did not start: `startProcess` failed with `No such file or
directory`, and no result file appeared under the staged rootfs. The likely
reason is namespace ownership of the staged tree. The R6 staging command ran as
an exec inside the predeclared `utility` container, so it proved utility-namespace
file staging. It did not prove that the same tree is populated in vminitd's init
mount namespace. `vminitd` could create the new container object and bundle, but
`vmexec run` could not execute `/bin/sh` from the rootfs path it resolved.

Outcome: `vminitdContainerRootfsPathFound`.

This is not yet production-ready CRI behavior. It means the next research step
should focus on staging or exposing the rootfs in the namespace where vminitd
creates the init container, or on adding an upstream/LinuxPod primitive that
combines rootfs preparation, vminitd state registration, and process start.
