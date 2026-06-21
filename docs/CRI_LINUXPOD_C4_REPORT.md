# CRI LinuxPod C4 HotplugProvider Boundary Probe Report (#91)

Date: 2026-06-21T17:20:49Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: /tmp/apple-containerization
- Kernel: /tmp/apple-containerization/bin/vmlinux-arm64
- Initfs reference: vminit:latest
- Image: docker.io/library/busybox:1.36.1
- Work dir: /tmp/macvz-linuxpod-c4
- Probe: c4
- vmnet interface: 0

## Result

```json
{
  "durationSeconds" : 1.6202490329742432,
  "errors" : {
    "lateAdd" : "HotplugProbeFailure: USB mass storage attach succeeded, but no public API provided a deterministic Linux guest block path for the ext4 rootfs; refusing to return a guessed AttachedFilesystem"
  },
  "guestPathResolved" : false,
  "image" : "docker.io\/library\/busybox:1.36.1",
  "kernel" : "\/tmp\/apple-containerization\/bin\/vmlinux-arm64",
  "lateAddReturned" : false,
  "lateStartSucceeded" : false,
  "localhostAfterLateAdd" : false,
  "logs" : {
    "boot" : "\/tmp\/macvz-linuxpod-c4\/logs\/boot.log",
    "exec" : "\/tmp\/macvz-linuxpod-c4\/logs\/exec.log",
    "lateClient" : "\/tmp\/macvz-linuxpod-c4\/logs\/late-client.log",
    "server" : "\/tmp\/macvz-linuxpod-c4\/logs\/server.log"
  },
  "outcome" : "providerCalledUsbAttachedNoGuestPath",
  "podID" : "macvz-c4-1782062447",
  "providerCalled" : true,
  "providerEvents" : [
    "usbControllerConfigured",
    "providerInstalled",
    "providerCalled",
    "usbAttachAttempted",
    "usbAttachSucceeded"
  ],
  "providerInstalled" : true,
  "usbAttachAttempted" : true,
  "usbAttachSucceeded" : true,
  "usbControllerConfigured" : true,
  "workDir" : "\/tmp\/macvz-linuxpod-c4"
}
```

## Acceptance / Interpretation
- Result: **blocked after provider call**. A custom provider can be installed and
  called, and public USB mass-storage attach can succeed, but the probe could
  not obtain a deterministic Linux guest block path for the ext4 rootfs through
  public APIs. The probe intentionally refused to return a guessed
  `AttachedFilesystem`, so the late container was not started.
- [x] One LinuxPod was created.
- [x] A custom VZInstanceExtension / HotplugProvider was installed or the failure was recorded.
- [x] Server was registered before pod.create().
- [x] Pod and server were started before the late add attempt.
- [x] The post-create addContainer path recorded whether the provider was called.
- [x] The report distinguishes provider install/call, public rootfs attach, guest path resolution, and late-container start.
- [x] No guessed guest block path is counted as success.
