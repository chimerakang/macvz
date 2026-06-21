# CRI-R4 Guest-Side Rootfs Staging Report (#96)

Date: 2026-06-21T18:25:06Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: /tmp/apple-containerization
- Kernel: /tmp/apple-containerization/bin/vmlinux-arm64
- Initfs reference: vminit:latest
- Image: docker.io/library/busybox:1.36.1
- Work dir: /tmp/macvz-runtime-r4
- Probe: r4
- vmnet interface: 0

## Result

```json
{
  "attempts" : [
    {
      "cleanupOutput" : "cleanup_ok stage=\/run\/macvz-r4\/staged\/late-alpha target=\/run\/macvz-r4\/mounts\/late-alpha\n",
      "cleanupSucceeded" : true,
      "errors" : {
        "identity" : "exit 42: direct_status=0\ndirect_identity=macvz-r4-id=late-alpha\nmounted_status=1\nmounted_identity=\nmount_status=1\nmount_line=\nmount_target_listing=\n"
      },
      "identityVerified" : false,
      "mountSucceeded" : true,
      "mountTarget" : "\/run\/macvz-r4\/mounts\/late-alpha",
      "outcome" : "stagedRootfsIdentityMismatch",
      "requestID" : "late-alpha",
      "stagePath" : "\/run\/macvz-r4\/staged\/late-alpha\/rootfs",
      "stageSucceeded" : true,
      "verifyOutput" : "direct_status=0\ndirect_identity=macvz-r4-id=late-alpha\nmounted_status=1\nmounted_identity=\nmount_status=1\nmount_line=\nmount_target_listing=\n"
    }
  ],
  "durationSeconds" : 1.2387590408325195,
  "errors" : {

  },
  "image" : "docker.io\/library\/busybox:1.36.1",
  "kernel" : "\/tmp\/apple-containerization\/bin\/vmlinux-arm64",
  "logs" : {
    "boot" : "\/tmp\/macvz-runtime-r4\/logs\/boot.log",
    "exec" : "\/tmp\/macvz-runtime-r4\/logs\/exec.log",
    "utility" : "\/tmp\/macvz-runtime-r4\/logs\/utility.log"
  },
  "note" : "guest-side staging avoids guessed guest block devices, but it is not yet a full late-container process creation path",
  "outcome" : "stagedRootfsIdentityMismatch",
  "podCreated" : true,
  "podID" : "macvz-r4-1782066305",
  "transportAvailable" : true,
  "utilityStarted" : true,
  "workDir" : "\/tmp\/macvz-runtime-r4"
}
```

## Acceptance / Interpretation
- [x] One LinuxPod was created with a predeclared utility container.
- [x] Guest agent transport was attempted only after pod.create() and utility start.
- [x] Rootfs-like guest staging records stage/copy, bind mount, identity verification, cleanup, and retry boundaries.
- [x] Identity is checked by explicit request ID, not by guessed /dev/sdX or /dev/vdX names.
- [x] The report distinguishes guest staging transport unavailable, rootfs copy/unpack failure, identity mismatch, bind failure, cleanup failure, and success.

## Interpretation

Outcome: `stagedRootfsIdentityMismatch`.

This is a useful negative result, not a host-device failure. The running Pod VM
was reachable through the VM agent after `pod.create()`, and the guest-side
staging command wrote the expected request identity:

- `direct_status=0`
- `direct_identity=macvz-r4-id=late-alpha`

The VM agent also accepted the bind-mount request, but a later `exec` inside the
predeclared utility container did not see the mounted target:

- `mounted_status=1`
- `mount_status=1`
- no `/proc/mounts` line for `/run/macvz-r4/mounts/late-alpha`

So R4 proves that post-create guest-side staging is possible without guessed
`/dev/*` paths, but it does not yet prove that an already-running LinuxPod
container namespace can observe an agent-created bind mount. The next useful
probe is to create or start a process through the VM agent in the same sandbox
namespace as the staged rootfs, then verify whether that process can use the
staged tree as its rootfs.
