# CRI-R15 Late Rootfs Evidence Channel Report (#107)

Date: 2026-06-22T09:33:09Z

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

R15 adds a vminitd-visible evidence channel to the R9 prepared-rootfs harness:

1. The harness prepares `/run/macvz-r9-evidence/<processID>` inside the Pod VM
   through the existing vminitd Copy(COPY_IN archive) transport.
2. The late rootfs process receives that path as a bind mount at
   `/macvz-r9-evidence`.
3. The process writes the same identity payload to `/macvz-r9-result` and to
   `/macvz-r9-evidence/macvz-r9-result`.
4. The harness verifies identity by stat/copyOut from
   `/run/macvz-r9-evidence/<processID>/macvz-r9-result`.

The harness also captures stderr for failed late processes so future failures
include command-level diagnostics.

## Result

Outcome: `vminitdRootfsPrimitiveLaunchSucceeded`.

The R15 live run starts the late prepared-rootfs process, exits 0, and verifies
the identity payload through the bind-mounted handoff path:

```json
"processStartSucceeded" : true,
"processExitCode" : 0,
"resultVerified" : true,
"outcome" : "vminitdRootfsPrimitiveLaunchSucceeded"
```

The verified payload includes:

```text
identity=macvz-r9-id=late-alpha
expected=macvz-r9-id=late-alpha
proc_root=/
root_mount=tmpfs / tmpfs rw,relatime 0 0
```

The full live payload is captured in
[CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md](CRI_RUNTIME_R9_ROOTFS_PRIMITIVE_REPORT.md).

## Interpretation

R15 proves that the late prepared-rootfs launch can report expected rootfs
identity to MacVz/vminitd when the harness provides an explicit shared handoff
path. The prior R14 blocker was not process execution; it was relying on the
prepared rootfs path as the post-exit evidence channel.

`proc_root=/` is expected from inside the process mount namespace and does not
expose the host-visible `/run/container/<id>/rootfs` path. The identity file is
the rootfs identity signal: it is staged into the prepared rootfs before launch,
read by the late process, and copied back through the vminitd-visible handoff.

## Next Work

The next step can move from harness-only proof toward the CRI runtime design:

- define the production evidence/result path shape for launched containers;
- decide whether this handoff remains a runtime-private bind mount or becomes
  part of a broader rootfs preparation primitive;
- keep production CRI wiring gated until the runtime can create and clean up
  this evidence path per container.
