# CRI-R3 NBD-Backed Pre-Create Rootfs Identity Report (#95)

Date: 2026-06-21T18:08:24Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: /tmp/apple-containerization
- Kernel: /tmp/apple-containerization/bin/vmlinux-arm64
- Initfs reference: vminit:latest
- Image: docker.io/library/busybox:1.36.1
- Work dir: /tmp/macvz-runtime-r3
- Probe: r3
- vmnet interface: 0

## Result

```json
{
  "alphaMarkerHost" : "alpha-rootfs",
  "alphaNBDURL" : "nbd+unix:\/\/\/?socket=\/tmp\/macvz-runtime-r3\/rootfs\/alpha.sock",
  "alphaOutput" : "2026-06-21T18:08:24Z stdout F \/dev\/vdb \/ ext4 rw,relatime 0 0\n2026-06-21T18:08:24Z stdout F alpha-rootfs\n",
  "betaMarkerHost" : "beta-rootfs",
  "betaNBDURL" : "nbd+unix:\/\/\/?socket=\/tmp\/macvz-runtime-r3\/rootfs\/beta.sock",
  "betaOutput" : "2026-06-21T18:08:24Z stdout F \/dev\/vdc \/ ext4 rw,relatime 0 0\n2026-06-21T18:08:24Z stdout F beta-rootfs\n",
  "containerStartSucceeded" : true,
  "durationSeconds" : 1.927812933921814,
  "errors" : {

  },
  "image" : "docker.io\/library\/busybox:1.36.1",
  "kernel" : "\/tmp\/apple-containerization\/bin\/vmlinux-arm64",
  "logs" : {
    "alpha" : "\/tmp\/macvz-runtime-r3\/logs\/alpha.log",
    "beta" : "\/tmp\/macvz-runtime-r3\/logs\/beta.log",
    "boot" : "\/tmp\/macvz-runtime-r3\/logs\/boot.log",
    "exec" : "\/tmp\/macvz-runtime-r3\/logs\/exec.log"
  },
  "mountEvidenceVerified" : true,
  "nbdServersStarted" : true,
  "note" : "pre-create NBD rootfs identity does not solve post-create CreateContainer ordering",
  "outcome" : "nbdRootfsPrecreateSucceeded",
  "podCreated" : true,
  "podID" : "macvz-r3-1782065302",
  "rootfsMarkersVerified" : true,
  "workDir" : "\/tmp\/macvz-runtime-r3"
}
```

## Acceptance / Interpretation
- [x] Two rootfs ext4 images were served through local NBD Unix sockets, or the setup failure was recorded.
- [x] Two LinuxPod containers were pre-registered before pod.create() with NBD-backed rootfs mounts.
- [x] The containers started or the failure boundary was recorded.
- [x] Guest output records rootfs mount evidence from /proc/mounts.
- [x] Host-side ext4 reads verify each container wrote to its own backing rootfs image.
- [x] The report states that pre-create NBD rootfs does not solve post-create CreateContainer ordering.
- [x] No guessed /dev/sdX or /dev/vdX path is counted as success.

