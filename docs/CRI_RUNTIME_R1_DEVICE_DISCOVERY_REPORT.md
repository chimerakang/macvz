# CRI-R1 Guest-Side Hotplug Device Discovery Report (#93)

Date: 2026-06-21T17:50:16Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: /tmp/apple-containerization
- Kernel: /tmp/apple-containerization/bin/vmlinux-arm64
- Initfs reference: vminit:latest
- Image: docker.io/library/busybox:1.36.1
- Work dir: /tmp/macvz-runtime-r1
- Probe: r1
- vmnet interface: 0

## Result

```json
{
  "detachOutput" : "",
  "discoveryMethod" : "baseline \/sys\/block snapshot, new \/sys\/block entry, exact sector count match, read-only ext4 mount, busybox rootfs marker",
  "discoveryOutput" : "diagnostic_usb_devices=\ndiagnostic_scsi_disks=\ndiagnostic_block_devices=loop0 loop1 loop2 loop3 loop4 loop5 loop6 loop7 ram0 ram1 ram10 ram11 ram12 ram13 ram14 ram15 ram2 ram3 ram4 ram5 ram6 ram7 ram8 ram9 vda vdb \nno_new_device\n",
  "durationSeconds" : 31.80816090106964,
  "errors" : {
    "guestDiscovery" : "exit 11: diagnostic_usb_devices=\ndiagnostic_scsi_disks=\ndiagnostic_block_devices=loop0 loop1 loop2 loop3 loop4 loop5 loop6 loop7 ram0 ram1 ram10 ram11 ram12 ram13 ram14 ram15 ram2 ram3 ram4 ram5 ram6 ram7 ram8 ram9 vda vdb \nno_new_device\n"
  },
  "events" : [
    "usbControllerConfigured",
    "instanceCaptured",
    "usbAttachAttempted",
    "usbAttachSucceeded",
    "usbDetachSucceeded"
  ],
  "expectedSectors" : 4194304,
  "guestBaseline" : [
    {
      "name" : "vda",
      "sectors" : 1048576
    },
    {
      "name" : "vdb",
      "sectors" : 4194304
    }
  ],
  "guestCorrelatedDevice" : false,
  "guestDeviceGoneAfterDetach" : false,
  "guestMountSucceeded" : false,
  "guestObservedNewDevice" : false,
  "guestUnmountSucceeded" : false,
  "image" : "docker.io\/library\/busybox:1.36.1",
  "instanceCaptured" : true,
  "kernel" : "\/tmp\/apple-containerization\/bin\/vmlinux-arm64",
  "logs" : {
    "boot" : "\/tmp\/macvz-runtime-r1\/logs\/boot.log",
    "exec" : "\/tmp\/macvz-runtime-r1\/logs\/exec.log",
    "utility" : "\/tmp\/macvz-runtime-r1\/logs\/utility.log"
  },
  "markerVerified" : false,
  "outcome" : "guestCouldNotObserveNewDevice",
  "podID" : "macvz-r1-1782064185",
  "targetRootfs" : "\/tmp\/macvz-runtime-r1\/rootfs\/target-rootfs.ext4",
  "targetRootfsBytes" : 2147483648,
  "usbAttachAttempted" : true,
  "usbAttachSucceeded" : true,
  "usbControllerConfigured" : true,
  "usbDetachSucceeded" : true,
  "workDir" : "\/tmp\/macvz-runtime-r1"
}
```

## Acceptance / Interpretation
- [x] One LinuxPod was created.
- [x] A custom VZInstanceExtension configured an XHCI controller and captured the running VM instance.
- [x] Guest /sys/block baseline was recorded before host attach.
- [x] A second ext4 rootfs image was attached through public VZ USB mass storage or the attach failure was recorded.
- [x] Guest-side discovery distinguishes observation, correlation by exact sector count, mount, marker verification, unmount, detach, and post-detach cleanup.
- [x] No guessed /dev/sdX or /dev/vdX path is counted as success.

