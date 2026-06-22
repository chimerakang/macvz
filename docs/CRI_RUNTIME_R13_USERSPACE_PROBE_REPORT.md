# CRI-R13 Late Rootfs Userspace Probe Report (#105)

Date: 2026-06-22T08:59:09Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: `/tmp/apple-containerization`
- Containerization base: `6b7b42c` with R10 diagnostics, R11 ptmx, and R12 devnull local patches
- Kernel: `/tmp/apple-containerization/bin/vmlinux-arm64`
- Initfs reference: `vminit:macvz-r12`
- Image: `docker.io/library/busybox:1.36.1`
- Work dir: `/tmp/macvz-runtime-r9`
- Probe command: `MACVZ_LINUXPOD_POC=1 make cri-linuxpod-r9`

## Harness Patch

R13 changes only the MacVz R9 harness. The minimal prepared rootfs now creates
relative BusyBox applet symlinks for commands used by the identity script:

```text
/bin/cat -> busybox
/bin/grep -> busybox
/bin/ls -> busybox
/bin/readlink -> busybox
/bin/tr -> busybox
```

It also maps `processExitCode == 0` with missing result/namespace verification
to the R13 acceptance outcome `lateRootfsUserspaceAdvanced`.

## Result

Outcome: `lateRootfsUserspaceAdvanced`.

The R13 live run used `vminit:macvz-r12` and reran the same late prepared
rootfs harness. The previous userspace blocker:

```text
processExitCode=127
```

did not recur. The late container process started and exited successfully:

```json
"processStartSucceeded" : true,
"processExitCode" : 0,
"outcome" : "lateRootfsUserspaceAdvanced"
```

The complete live payload is captured in
[CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md](CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md).

## Interpretation

R13 proves the minimal rootfs is now runnable enough for the identity shell
script to exit successfully. This advances the path beyond vmexec setup and
beyond the prior userspace command-not-found failure.

The harness still cannot verify the result file from the vminitd namespace:

```text
stat: path not found '/run/container/r9-late-alpha/rootfs/macvz-r9-result'
```

That means R13 does not yet prove `vminitdRootfsPrimitiveLaunchSucceeded`.
The next question is result visibility across the mount namespace used by
`vmexec` after `pivotRoot`, not basic userspace execution.

## Next Work

The next probe should determine where the process writes `/macvz-r9-result` and
why vminitd cannot see it at `/run/container/<id>/rootfs/macvz-r9-result`.
Useful probes include:

- writing the result to an additional path known to survive namespace changes;
- capturing stdout/stderr from the late process;
- adding mountinfo or file visibility diagnostics after process exit.

Production CRI wiring remains blocked until the harness can verify rootfs
identity evidence.
