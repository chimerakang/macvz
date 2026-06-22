# CRI-R9 vminitd Rootfs Primitive Launch Report (#101)

Date: 2026-06-22T07:46:59Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: /tmp/apple-containerization
- Kernel: /tmp/apple-containerization/bin/vmlinux-arm64
- Initfs reference: vminit:latest
- Image: docker.io/library/busybox:1.36.1
- Work dir: /tmp/macvz-runtime-r9
- Probe: r9
- vmnet interface: 0

## Result

```json
{
  "attempt" : {
    "cleanupOutput" : "cleanup_ok container=r9-late-alpha rootfs=\/run\/container\/r9-late-alpha\/rootfs",
    "cleanupSucceeded" : true,
    "errors" : {
      "experimentalApiShape" : "existing vminitd Copy(COPY_OUT\/COPY_IN archive) used as PrepareContainerRootfs",
      "rootfsPath" : "\/run\/container\/r9-late-alpha\/rootfs",
      "startOrWaitProcess" : "RPCError: internalError: \"startProcess: failed to start process: internalError: \"vmexec error: internalError: \" Error Domain=NSPOSIXErrorDomain Code=2 \"No such file or directory\"\"\"\"",
      "verify" : "skipped because process did not start and exit successfully"
    },
    "namespaceVerified" : false,
    "outcome" : "vminitdContainerStartFailed",
    "processContainerID" : "r9-late-alpha",
    "processCreateSucceeded" : true,
    "processID" : "r9-late-alpha",
    "processStartSucceeded" : false,
    "requestID" : "late-alpha",
    "resultPath" : "\/run\/container\/r9-late-alpha\/rootfs\/macvz-r9-result",
    "resultVerified" : false,
    "stageOutput" : "prepare_ok source=\/run\/container\/utility\/rootfs\/bin\/busybox rootfs=\/run\/container\/r9-late-alpha\/rootfs",
    "stagePath" : "\/run\/container\/r9-late-alpha\/rootfs",
    "stageSucceeded" : true,
    "verifyOutput" : ""
  },
  "durationSeconds" : 1.511884093284607,
  "errors" : {

  },
  "image" : "docker.io\/library\/busybox:1.36.1",
  "kernel" : "\/tmp\/apple-containerization\/bin\/vmlinux-arm64",
  "logs" : {
    "boot" : "\/tmp\/macvz-runtime-r9\/logs\/boot.log",
    "exec" : "\/tmp\/macvz-runtime-r9\/logs\/exec.log",
    "utility" : "\/tmp\/macvz-runtime-r9\/logs\/utility.log"
  },
  "note" : "R9 uses existing vminitd Copy archive transport as a local experimental PrepareContainerRootfs shape, then launches through the existing new-container process path",
  "outcome" : "vminitdContainerStartFailed",
  "podCreated" : true,
  "podID" : "macvz-r9-1782114418",
  "transportAvailable" : true,
  "utilityStarted" : true,
  "workDir" : "\/tmp\/macvz-runtime-r9"
}
```

## Acceptance / Interpretation
- [x] One LinuxPod was created with a predeclared utility container.
- [x] The probe used existing vminitd Copy(COPY_OUT/COPY_IN archive) as a local experimental PrepareContainerRootfs shape.
- [x] The prepared rootfs was copied to /run/container/<containerID>/rootfs, or the precise prepare failure was recorded.
- [x] vminitd createProcess/startProcess was called with id == containerID to target the new-container path.
- [x] Identity, namespace/rootfs evidence, deleteProcess cleanup, and the final R9 outcome were recorded.

## Experimental API Shape

No source patch was applied to `/tmp/apple-containerization` for this run. The
local experimental shape used by the MacVz harness was:

1. Treat existing `vminitd.copy(direction: .copyOut/.copyIn, isArchive: true)`
   as a temporary `PrepareContainerRootfs` transport.
2. Copy `/run/container/utility/rootfs/bin/busybox` and
   `/run/container/utility/rootfs/lib` out of the running Pod VM.
3. Build a minimal host-side rootfs with `/bin/sh`, `/bin/busybox`, `/lib`,
   `/etc/macvz-r9-identity`, `/proc`, `/sys`, `/dev`, and `/tmp`.
4. Copy that rootfs back into the VM at
   `/run/container/r9-late-alpha/rootfs`.
5. Call the existing vminitd new-container path:
   `createProcess(id: "r9-late-alpha", containerID: "r9-late-alpha", spec.root.path: "/run/container/r9-late-alpha/rootfs")`.
6. Call `startProcess`, then `deleteProcess`, and verify the prepared rootfs is
   removed.

## Interpretation

R9 proves that the existing vminitd Copy transport can prepare a rootfs-like
tree in a vminitd-visible path and that `createProcess(id == containerID)` can
create the new vminitd container object from that path.

The final outcome is `vminitdContainerStartFailed`. `startProcess` reaches
`vmexec run --bundle-path /run/container/r9-late-alpha`, but `vmexec` returns
`NSPOSIXErrorDomain Code=2 "No such file or directory"` before the init process
writes identity evidence. Cleanup succeeds through `deleteProcess`, and the
rootfs path is gone after deletion.

This means R9 did not prove `vminitdRootfsPrimitiveLaunchSucceeded`. The next
useful work is not production CRI wiring; it is a narrower upstream/vminitd
debug patch that instruments why `vmexec run` cannot start from a rootfs that
was prepared through `Copy` at `/run/container/<containerID>/rootfs`.
