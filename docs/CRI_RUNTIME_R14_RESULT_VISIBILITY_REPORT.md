# CRI-R14 Late Rootfs Result Visibility Report (#106)

Date: 2026-06-22T09:13:14Z

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

R14 keeps the R13 userspace changes and adds an explicit visibility diagnostic:
if the late process exits 0 but vminitd cannot stat the expected result path,
the report records `resultVisibility`.

## Result

Outcome: `lateRootfsResultVisibilityExplained`.

The R14 live run used `vminit:macvz-r12` and reran the same late prepared
rootfs harness. The process starts and exits 0:

```json
"processStartSucceeded" : true,
"processExitCode" : 0,
"outcome" : "lateRootfsResultVisibilityExplained"
```

The harness records:

```text
process exit 0 proves the script completed after writing /macvz-r9-result inside the vmexec namespace, but vminitd could not stat /run/container/r9-late-alpha/rootfs/macvz-r9-result
```

The full live payload is captured in
[CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md](CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md).

## Interpretation

R14 explains the current verification failure as a result visibility boundary,
not a process launch failure. The process reaches userspace, writes the result
file in its own vmexec mount namespace, and exits successfully. Afterward,
vminitd cannot see that file at the prepared rootfs path.

This does not yet prove `vminitdRootfsPrimitiveLaunchSucceeded`, because the
harness still lacks a vminitd-verifiable identity evidence channel.

## Next Work

The next probe should add a verifiable evidence channel for identity output,
such as:

- capturing stdout/stderr from the late process;
- bind-mounting an explicit result directory that is visible to vminitd;
- writing to a handoff path managed outside the private root pivot.

Production CRI wiring remains blocked until the harness can verify rootfs
identity evidence.
