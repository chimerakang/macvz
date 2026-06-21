# CRI-R5 VM-Agent Process From Staged Rootfs Report (#97)

Date: 2026-06-21T18:51:09Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: /tmp/apple-containerization
- Kernel: /tmp/apple-containerization/bin/vmlinux-arm64
- Initfs reference: vminit:latest
- Image: docker.io/library/busybox:1.36.1
- Work dir: /tmp/macvz-runtime-r5
- Probe: r5
- vmnet interface: 0

## Result

```json
{
  "attempt" : {
    "cleanupOutput" : "cleanup_ok stage=\/run\/macvz-r5\/staged\/late-alpha\n",
    "cleanupSucceeded" : true,
    "errors" : {
      "createProcessRoot" : "RPCError: invalidArgument: \"createProcess: failed to create process: invalidArgument: \"processes in the root of the vm not implemented\"\"",
      "processExit" : "exit 1",
      "verify" : "exit 51: result_missing=\/run\/macvz-r5\/staged\/late-alpha\/rootfs\/macvz-r5-result\n"
    },
    "namespaceVerified" : false,
    "outcome" : "processStartedButIdentityMismatch",
    "processContainerID" : "utility",
    "processCreateSucceeded" : true,
    "processExitCode" : 1,
    "processID" : "r5-process-late-alpha",
    "processStartSucceeded" : true,
    "requestID" : "late-alpha",
    "resultPath" : "\/run\/macvz-r5\/staged\/late-alpha\/rootfs\/macvz-r5-result",
    "resultVerified" : false,
    "stageOutput" : "stage_ok request=late-alpha rootfs=\/run\/macvz-r5\/staged\/late-alpha\/rootfs\n",
    "stagePath" : "\/run\/macvz-r5\/staged\/late-alpha\/rootfs",
    "stageSucceeded" : true,
    "verifyOutput" : "result_missing=\/run\/macvz-r5\/staged\/late-alpha\/rootfs\/macvz-r5-result\n"
  },
  "durationSeconds" : 1.2825939655303955,
  "errors" : {

  },
  "image" : "docker.io\/library\/busybox:1.36.1",
  "kernel" : "\/tmp\/apple-containerization\/bin\/vmlinux-arm64",
  "logs" : {
    "boot" : "\/tmp\/macvz-runtime-r5\/logs\/boot.log",
    "exec" : "\/tmp\/macvz-runtime-r5\/logs\/exec.log",
    "utility" : "\/tmp\/macvz-runtime-r5\/logs\/utility.log"
  },
  "note" : "R5 tests VM-agent process execution from a post-create staged rootfs; it does not implement the production CRI image pipeline",
  "outcome" : "processStartedButIdentityMismatch",
  "podCreated" : true,
  "podID" : "macvz-r5-1782067867",
  "transportAvailable" : true,
  "utilityStarted" : true,
  "workDir" : "\/tmp\/macvz-runtime-r5"
}
```

## Acceptance / Interpretation
- [x] One LinuxPod was created with a predeclared keeper utility container.
- [x] Guest agent transport was attempted only after pod.create() and utility start.
- [x] A post-create staged rootfs was used as the root of a VM-agent-created process, or the precise failure boundary was recorded.
- [x] Identity is checked by explicit request ID, not by guessed /dev/sdX or /dev/vdX names.
- [x] The report distinguishes VM-agent process API unavailable, staged rootfs unavailable, process creation failure, identity mismatch, namespace/rootfs mismatch, cleanup failure, and success.

## Finding

The root-level VM process path is unavailable in the current `vminitd` path:

```text
processes in the root of the vm not implemented
```

The fallback `containerID=utility` path could create and start a process, but it
exited with code 1 and did not write `/macvz-r5-result` into the staged rootfs.
This run therefore does not prove a late-container rootfs launch primitive. It
shows that post-create file staging works, while arbitrary process execution
from that staged rootfs remains blocked.
