# CRI-R10 vminitd/vmexec Start Failure Report (#102)

Date: 2026-06-22T08:18:51Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: `/tmp/apple-containerization`
- Containerization base: `6b7b42c` with local diagnostic patch
- Kernel: `/tmp/apple-containerization/bin/vmlinux-arm64`
- Initfs reference: `vminit:macvz-r10`
- Image: `docker.io/library/busybox:1.36.1`
- Work dir: `/tmp/macvz-runtime-r9`
- Probe command: `MACVZ_LINUXPOD_POC=1 make cri-linuxpod-r9`

## Local Diagnostic Patch

The apple/containerization checkout was patched locally only for diagnostics.
The patch is recorded at
[CRI_RUNTIME_R10_APPLE_CONTAINERIZATION_DEBUG.patch](CRI_RUNTIME_R10_APPLE_CONTAINERIZATION_DEBUG.patch).

The patch adds:

- explicit `errno`, stage, and strerror reporting in `vmexec`;
- bundle/rootfs/process/mount/path diagnostics around `execInNamespace`;
- best-effort diagnostic file writes under `/run`.

The stale cached initfs at
`~/Library/Application Support/com.apple.containerization/initfs.ext4` was
removed before the run so the probe used the rebuilt `vminit:macvz-r10` initfs.

## Result

Outcome: `vmexecStartFailureExplained`.

R10 explains the R9 `vminitdContainerStartFailed` result. The late prepared
rootfs was present, the new vminitd container object was created, and `vmexec`
entered the start path. It then failed before executing `/bin/sh`:

```text
macvz-r10-errno=1 stage=remove(ptmx) errno=2 strerror=No such file or directory
```

The full diagnostic payload is captured in
[CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md](CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md).
Key evidence:

```text
bundlePath=/run/container/r9-late-alpha
root.path=/run/container/r9-late-alpha/rootfs
path.bundle=/run/container/r9-late-alpha:exists type=dir
path.bundle.config=/run/container/r9-late-alpha/config.json:exists type=file
path.rootfs=/run/container/r9-late-alpha/rootfs:exists type=dir
path.rootfs.bin.sh=/run/container/r9-late-alpha/rootfs/bin/sh:exists type=file readable=true executable=true
path.rootfs.lib.ld-linux-aarch64=/run/container/r9-late-alpha/rootfs/lib/ld-linux-aarch64.so.1:exists type=file readable=true executable=true
path.rootfs.lib.libc=/run/container/r9-late-alpha/rootfs/lib/libc.so.6:exists type=file readable=true executable=true
mounts=proc:proc->/proc,tmpfs:tmpfs->/dev,devpts:devpts->/dev/pts,sysfs:sysfs->/sys,tmpfs:tmpfs->/dev/shm
```

This rules out the main missing-path hypotheses:

| Hypothesis | Result |
| --- | --- |
| `vmexecMissingBundlePath` | Ruled out: bundle exists and is readable/executable. |
| `vmexecMissingRootfsPath` | Ruled out: rootfs exists and is readable/executable. |
| `vmexecMissingInitBinary` | Ruled out: `/bin/sh` exists and is executable. |
| `vmexecMissingDynamicLinker` | Ruled out: `/lib/ld-linux-aarch64.so.1` exists and is executable. |
| `vmexecMountNamespaceMismatch` | Not the primary failure: relevant mountinfo shows the bundle/rootfs and OCI mounts. |
| `vmexecBundleLifecycleMismatch` | Not the primary failure: `createProcess` created bundle state and `startProcess` reached `vmexec`. |

## Interpretation

The failure is inside `vmexec` console/device setup, specifically the
`configureConsole` path that handles `/dev/ptmx` after mounting OCI `/dev` as
tmpfs and `/dev/pts` as devpts. The code attempts to remove
`<rootfs>/dev/ptmx`; that operation returns ENOENT and is surfaced as a fatal
start error.

The rootfs primitive is therefore past the "can vminitd see the rootfs and
bundle?" question. The next blocker is a narrower `vmexec` `/dev/ptmx`
invariant triggered by this late prepared rootfs shape.

The best-effort diagnostic files under the rootfs were unavailable after
failure, but the same diagnostic payload was delivered through the existing
`vmexec` error pipe, so the missing files do not block the R10 conclusion.

## Next Work

The next minimal probe should patch `vmexec` console setup so `remove(ptmx)` is
idempotent when the target disappears or is already provided by the tmpfs/devpts
setup. After that, rerun the same R9 harness. A successful follow-up should
advance to either:

- process identity evidence from `/etc/macvz-r9-identity`;
- a later, more specific `execvpe`/chroot/mount error;
- or `vminitdRootfsPrimitiveLaunchSucceeded`.

Do not wire this path into production CRI yet. R10 only explains the start
failure; it does not prove a complete kubelet-compatible container lifecycle.
