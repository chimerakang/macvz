# CRI-R7 vminitd-Visible Rootfs Launch Report (#99)

Date: 2026-06-22T06:57:46Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: /tmp/apple-containerization
- Kernel: /tmp/apple-containerization/bin/vmlinux-arm64
- Initfs reference: vminit:latest
- Image: docker.io/library/busybox:1.36.1
- Work dir: /tmp/macvz-runtime-r7
- Probe: r7
- vmnet interface: 0

## Result

```json
{
  "attempt" : {
    "cleanupOutput" : "cleanup_ok stage=\/macvz-r7\/staged\/late-alpha\n",
    "cleanupSucceeded" : true,
    "errors" : {
      "startOrWaitProcess" : "RPCError: internalError: \"startProcess: failed to start process: internalError: \"vmexec error: internalError: \" Error Domain=NSPOSIXErrorDomain Code=2 \"No such file or directory\"\"\"\"",
      "verify" : "exit 51: result_missing=\/macvz-r7\/staged\/late-alpha\/rootfs\/macvz-r7-result\n",
      "vminitdRootfsPath" : "\/run\/container\/utility\/rootfs\/macvz-r7\/staged\/late-alpha\/rootfs"
    },
    "namespaceVerified" : false,
    "outcome" : "vminitdVisibleRootfsPrimitiveMissing",
    "processContainerID" : "r7-late-alpha",
    "processCreateSucceeded" : true,
    "processID" : "r7-late-alpha",
    "processStartSucceeded" : false,
    "requestID" : "late-alpha",
    "resultPath" : "\/macvz-r7\/staged\/late-alpha\/rootfs\/macvz-r7-result",
    "resultVerified" : false,
    "stageOutput" : "stage_ok request=late-alpha rootfs=\/macvz-r7\/staged\/late-alpha\/rootfs\n",
    "stagePath" : "\/macvz-r7\/staged\/late-alpha\/rootfs",
    "stageSucceeded" : true,
    "verifyOutput" : "result_missing=\/macvz-r7\/staged\/late-alpha\/rootfs\/macvz-r7-result\n"
  },
  "durationSeconds" : 1.388113021850586,
  "errors" : {

  },
  "image" : "docker.io\/library\/busybox:1.36.1",
  "kernel" : "\/tmp\/apple-containerization\/bin\/vmlinux-arm64",
  "logs" : {
    "boot" : "\/tmp\/macvz-runtime-r7\/logs\/boot.log",
    "exec" : "\/tmp\/macvz-runtime-r7\/logs\/exec.log",
    "utility" : "\/tmp\/macvz-runtime-r7\/logs\/utility.log"
  },
  "note" : "R7 tests whether a rootfs staged inside the utility rootfs can be addressed through vminitd's init-namespace path for new-container start",
  "outcome" : "vminitdVisibleRootfsPrimitiveMissing",
  "podCreated" : true,
  "podID" : "macvz-r7-1782111464",
  "transportAvailable" : true,
  "utilityStarted" : true,
  "workDir" : "\/tmp\/macvz-runtime-r7"
}
```

## Acceptance / Interpretation
- [x] One LinuxPod was created with a predeclared keeper utility container.
- [x] Guest agent transport was attempted only after pod.create() and utility start.
- [x] The probe staged rootfs data through the utility container and addressed the same tree through vminitd's init-namespace path.
- [x] vminitd createProcess/startProcess was called with id == containerID to target the new-container path.
- [x] The report distinguishes unavailable transport, vminitd-visible rootfs launch success, missing primitive, cleanup failure, and upstream-change boundaries.

## Interpretation

R7 tested the most direct hypothesis from R6: a rootfs staged by a utility
container might be consumable by vminitd if the OCI `root.path` uses the
init-namespace view of that same tree.

The first R7 attempt staged under the utility container's `/run`, then addressed
it as `/run/container/utility/rootfs/run/...` from vminitd. That produced the
same `No such file or directory` start failure as R6. The likely reason is that
`/run` is container-local mount state, not durable utility-rootfs contents.

The final R7 attempt staged under `/macvz-r7/...` in the utility container and
used `/run/container/utility/rootfs/macvz-r7/...` as the vminitd-visible OCI
`root.path`. This also reached vminitd's new-container branch:

- `createProcess` succeeded.
- vminitd created a cgroup and vmexec init process for `r7-late-alpha`.
- `startProcess` still failed inside `vmexec run` with `No such file or
  directory`.
- No identity/result file appeared in the staged rootfs.

This means the remaining boundary is not simply the path prefix. A process
running as an exec inside the utility container can create files it can later
observe, but current public APIs still do not provide a proven way to prepare a
rootfs in the exact namespace/state that vminitd's new-container `vmexec run`
can launch.

Outcome: `vminitdVisibleRootfsPrimitiveMissing`.

## Decision

Do not wire this path into production CRI code yet. The current LinuxPod/vminitd
surface has the new-container process branch, but lacks the companion primitive
MacVz needs for kubelet ordering:

1. prepare or receive rootfs data in vminitd-visible state;
2. register the new vminitd container from that rootfs;
3. start the init process; and
4. clean up the rootfs/container state consistently on failure.

The next step should be a design issue for the smallest upstream-compatible
LinuxPod/vminitd primitive, rather than another wrapper around utility-container
exec staging.
