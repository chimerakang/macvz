# CRI-R9 vminitd Rootfs Primitive Launch Report (#101)

Date: 2026-06-22T09:31:12Z

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
      "namespaceEvidence" : "bind-mounted handoff verified rootfs identity; proc_root did not expose the host-visible prepared-rootfs path",
      "rootfsPath" : "\/run\/container\/r9-late-alpha\/rootfs"
    },
    "namespaceVerified" : false,
    "outcome" : "vminitdRootfsPrimitiveLaunchSucceeded",
    "processContainerID" : "r9-late-alpha",
    "processCreateSucceeded" : true,
    "processExitCode" : 0,
    "processID" : "r9-late-alpha",
    "processStartSucceeded" : true,
    "requestID" : "late-alpha",
    "resultPath" : "\/run\/container\/r9-late-alpha\/rootfs\/macvz-r9-result",
    "resultVerified" : true,
    "stageOutput" : "prepare_ok source=\/run\/container\/utility\/rootfs\/bin\/busybox rootfs=\/run\/container\/r9-late-alpha\/rootfs evidence=\/run\/macvz-r9-evidence\/r9-late-alpha",
    "stagePath" : "\/run\/container\/r9-late-alpha\/rootfs",
    "stageSucceeded" : true,
    "verifyOutput" : "identity=macvz-r9-id=late-alpha\nexpected=macvz-r9-id=late-alpha\npwd=\/\nproc_root=\/\nroot_mount=tmpfs \/ tmpfs rw,relatime 0 0\nroot_listing=bin,dev,etc,lib,macvz-r9-evidence,macvz-r9-result,proc,run,sys,tmp,\n"
  },
  "durationSeconds" : 3.176005005836487,
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
  "outcome" : "vminitdRootfsPrimitiveLaunchSucceeded",
  "podCreated" : true,
  "podID" : "macvz-r9-1782120669",
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

## R15 Instrumentation Note

This report was regenerated during CRI-R15. The harness now verifies late-rootfs
identity through a vminitd-visible bind-mounted handoff path:

```text
/run/macvz-r9-evidence/r9-late-alpha/macvz-r9-result
```

The late process exits 0, `resultVerified=true`, and the outcome is
`vminitdRootfsPrimitiveLaunchSucceeded`. The R15 conclusion is published at
[CRI_RUNTIME_R15_EVIDENCE_CHANNEL_REPORT.md](CRI_RUNTIME_R15_EVIDENCE_CHANNEL_REPORT.md).
