# CRI-R9 vminitd Rootfs Primitive Launch Report (#101)

Date: 2026-06-22T08:43:01Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: /tmp/apple-containerization
- Kernel: /tmp/apple-containerization/bin/vmlinux-arm64
- Initfs reference: vminit:macvz-r12
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
      "processExit" : "exit 127",
      "rootfsPath" : "\/run\/container\/r9-late-alpha\/rootfs",
      "verify" : "skipped because process did not start and exit successfully"
    },
    "namespaceVerified" : false,
    "outcome" : "vminitdRootfsIdentityMismatch",
    "processContainerID" : "r9-late-alpha",
    "processCreateSucceeded" : true,
    "processExitCode" : 127,
    "processID" : "r9-late-alpha",
    "processStartSucceeded" : true,
    "requestID" : "late-alpha",
    "resultPath" : "\/run\/container\/r9-late-alpha\/rootfs\/macvz-r9-result",
    "resultVerified" : false,
    "stageOutput" : "prepare_ok source=\/run\/container\/utility\/rootfs\/bin\/busybox rootfs=\/run\/container\/r9-late-alpha\/rootfs",
    "stagePath" : "\/run\/container\/r9-late-alpha\/rootfs",
    "stageSucceeded" : true,
    "verifyOutput" : ""
  },
  "durationSeconds" : 2.4181909561157227,
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
  "outcome" : "vminitdRootfsIdentityMismatch",
  "podCreated" : true,
  "podID" : "macvz-r9-1782117779",
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

No production MacVz runtime code was changed for this run. The local
experimental shape used by the harness was:

1. Treat existing `vminitd.copy(direction: .copyOut/.copyIn, isArchive: true)`
   as a temporary `PrepareContainerRootfs` transport.
2. Copy `/run/container/utility/rootfs/bin/busybox` and
   `/run/container/utility/rootfs/lib` out of the running Pod VM.
3. Build a minimal host-side rootfs with `/bin/sh`, `/bin/busybox`, `/lib`,
   `/etc/macvz-r9-identity`, `/proc`, `/sys`, `/dev`, `/run`, and `/tmp`.
4. Copy that rootfs back into the VM at
   `/run/container/r9-late-alpha/rootfs`.
5. Call the existing vminitd new-container path:
   `createProcess(id: "r9-late-alpha", containerID: "r9-late-alpha", spec.root.path: "/run/container/r9-late-alpha/rootfs")`.
6. Call `startProcess`, then `deleteProcess`, and verify the prepared rootfs is
   removed.

## R10/R11/R12 Instrumentation Notes

This report has been regenerated across the R10-R12 probes using local
apple/containerization initfs images:

- R10 (`vminit:macvz-r10`) explained the original start failure as
  `stage=remove(ptmx) errno=2`.
- R11 (`vminit:macvz-r11`) patched ptmx ENOENT handling and advanced to
  `stage=open(/dev/null) errno=2`.
- R12 (`vminit:macvz-r12`) created `/dev/null` after OCI `/dev` tmpfs setup.
  The process now starts and exits from userspace with code 127.

The R12 conclusion is published at
[CRI_RUNTIME_R12_DEVNULL_PROBE_REPORT.md](CRI_RUNTIME_R12_DEVNULL_PROBE_REPORT.md).
