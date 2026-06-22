# CRI-R12 vmexec /dev/null Probe Report (#104)

Date: 2026-06-22T08:43:01Z

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

## Local Patch

R12 keeps the prior local diagnostic and ptmx patches:

- [CRI_RUNTIME_R10_APPLE_CONTAINERIZATION_DEBUG.patch](CRI_RUNTIME_R10_APPLE_CONTAINERIZATION_DEBUG.patch)
- [CRI_RUNTIME_R11_APPLE_CONTAINERIZATION_PTMX.patch](CRI_RUNTIME_R11_APPLE_CONTAINERIZATION_PTMX.patch)

It adds the devnull-specific patch recorded at
[CRI_RUNTIME_R12_APPLE_CONTAINERIZATION_DEVNULL.patch](CRI_RUNTIME_R12_APPLE_CONTAINERIZATION_DEVNULL.patch).

The R12 patch creates a usable `/dev/null` after OCI mounts have populated the
container `/dev` tmpfs and before `pivotRoot`:

```swift
try ensureDevNull(rootfs: rootfs.path)
```

The helper creates character device `1:3` with mode `0666`, replacing a
non-character existing path if needed.

## Result

Outcome: `vmexecDevNullFixAdvancedToExec`.

The R12 live run used `vminit:macvz-r12`. The previous R11 blocker:

```text
stage=open(/dev/null) errno=2
```

did not recur. The late container process started successfully, then exited from
userspace with status 127:

```json
"processStartSucceeded" : true,
"processExitCode" : 127,
"outcome" : "vminitdRootfsIdentityMismatch"
```

The full live payload is captured in
[CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md](CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md).

## Interpretation

R12 proves the `/dev/null` invariant can be satisfied with a small vmexec
device setup patch. The launch path now reaches the container process instead
of failing inside vmexec setup.

The exit 127 is consistent with the current R9 minimal rootfs rather than a
vmexec device failure. The host-side prepared rootfs contains only:

```text
/bin/busybox
/bin/sh
/etc/macvz-r9-identity
/lib/*
```

The verification script invokes applet names such as `cat`, `readlink`, `grep`,
`ls`, and `tr`, but the rootfs does not install those BusyBox applet symlinks.
With `set -e`, the first missing applet can terminate the shell with 127 before
`/macvz-r9-result` is written.

## Next Work

The next probe should make the minimal rootfs usable enough for the identity
script, either by installing BusyBox applet symlinks in the prepared rootfs or
by rewriting the probe command to call `/bin/busybox <applet>` explicitly.

Production CRI wiring remains blocked until the late container reports the
expected rootfs identity.
