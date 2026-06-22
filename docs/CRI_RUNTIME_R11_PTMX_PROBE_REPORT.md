# CRI-R11 vmexec /dev/ptmx Probe Report (#103)

Date: 2026-06-22T08:31:54Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: `/tmp/apple-containerization`
- Containerization base: `6b7b42c` with the R10 diagnostic patch plus the R11 ptmx patch
- Kernel: `/tmp/apple-containerization/bin/vmlinux-arm64`
- Initfs reference: `vminit:macvz-r11`
- Image: `docker.io/library/busybox:1.36.1`
- Work dir: `/tmp/macvz-runtime-r9`
- Probe command: `MACVZ_LINUXPOD_POC=1 make cri-linuxpod-r9`

## Local Patch

R11 keeps the R10 diagnostic patch recorded at
[CRI_RUNTIME_R10_APPLE_CONTAINERIZATION_DEBUG.patch](CRI_RUNTIME_R10_APPLE_CONTAINERIZATION_DEBUG.patch)
and adds the ptmx-specific change recorded at
[CRI_RUNTIME_R11_APPLE_CONTAINERIZATION_PTMX.patch](CRI_RUNTIME_R11_APPLE_CONTAINERIZATION_PTMX.patch).

The R11 behavioral patch is intentionally small:

```swift
guard remove(ptmx) == 0 || errno == ENOENT else {
    throw App.Errno(stage: "remove(ptmx)")
}
```

That treats a disappearing or already-absent `<rootfs>/dev/ptmx` as idempotent,
while preserving fatal behavior for other `remove` errors.

## Result

Outcome: `vmexecPtmxFixAdvancedToExec`.

The R11 live run used `vminit:macvz-r11` and reran the same late prepared
rootfs harness. The previous R10 blocker:

```text
stage=remove(ptmx) errno=2
```

did not recur. `vmexec` advanced past console `/dev/ptmx` setup and failed at
the next concrete device invariant:

```text
macvz-r10-errno=1 stage=open(/dev/null) errno=2 strerror=No such file or directory
```

The complete live payload is captured in
[CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md](CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md).
The harness also copied the diagnostic files out of the rootfs to:

- `/tmp/macvz-runtime-r9/rootfs/r10-vmexec-diagnostics-late-alpha.txt`
- `/tmp/macvz-runtime-r9/rootfs/r10-vmexec-errorpipe-late-alpha.txt`

## Interpretation

R11 proves the `/dev/ptmx` fix is directionally correct for this harness: the
late-created container start no longer fails at `remove(ptmx)`. The next failure
is later in vmexec setup, where `open("/dev/null")` returns ENOENT before the
container process can execute and write `/macvz-r9-result`.

The R11 diagnostic path reports bundle/rootfs paths as missing because the
diagnostic is captured after the child has advanced into its changed filesystem
view. This does not invalidate the R11 conclusion: R10 already proved the
bundle/rootfs/init/dynamic-linker paths existed before the ptmx fix, and the
specific failure stage advanced from `remove(ptmx)` to `open(/dev/null)` under
the same harness.

## Next Work

The next probe should address the `/dev/null` invariant in the late prepared
rootfs path. Candidate directions:

- create or bind a usable `/dev/null` inside the prepared rootfs before
  `startProcess`;
- patch vmexec device setup to ensure `/dev/null` exists after OCI `/dev` tmpfs
  is mounted;
- record whether advancing past `/dev/null` reaches `chroot`, `execvpe`, or
  the expected `/etc/macvz-r9-identity` evidence.

Production CRI wiring remains blocked until the late container actually starts
and reports rootfs identity.
