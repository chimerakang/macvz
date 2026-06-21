# CRI LinuxPod PoC Report (#88)

Date: 2026-06-21T16:30:52Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: /tmp/apple-containerization
- Kernel: /tmp/apple-containerization/bin/vmlinux-arm64
- Initfs reference: vminit:latest
- Image: docker.io/library/busybox:1.36.1
- Work dir: /tmp/macvz-linuxpod-poc
- vmnet interface: 0

## Result

```json
{
  "durationSeconds" : 19.078837037086487,
  "image" : "docker.io\/library\/busybox:1.36.1",
  "kernel" : "\/tmp\/apple-containerization\/bin\/vmlinux-arm64",
  "logs" : {
    "boot" : "\/tmp\/macvz-linuxpod-poc\/logs\/boot.log",
    "client" : "\/tmp\/macvz-linuxpod-poc\/logs\/client.log",
    "exec" : "\/tmp\/macvz-linuxpod-poc\/logs\/exec.log",
    "server" : "\/tmp\/macvz-linuxpod-poc\/logs\/server.log"
  },
  "podID" : "macvz-poc-1782059433",
  "workDir" : "\/tmp\/macvz-linuxpod-poc"
}
```

## Acceptance

- [x] One LinuxPod was created.
- [x] Two containers were registered before pod.create().
- [x] Server container listened on 127.0.0.1.
- [x] Client container reached server through localhost.
- [x] Exec worked inside the server container.
- [x] CPU/memory stats were returned for both containers.
- [x] Stopping the server first left the client observable.
- [x] Pod stop completed cleanly.

