# CRI LinuxPod C2 Ordering Probe Report (#89)

Date: 2026-06-21T16:57:23Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: /tmp/apple-containerization
- Kernel: /tmp/apple-containerization/bin/vmlinux-arm64
- Initfs reference: vminit:latest
- Image: docker.io/library/busybox:1.36.1
- Work dir: /tmp/macvz-linuxpod-c2
- Probe: c2
- vmnet interface: 0

## Result

```json
{
  "durationSeconds" : 1.5379669666290283,
  "errors" : {
    "lateAdd" : "ContainerizationError: unsupported: \"hotplug not supported\""
  },
  "fallback" : "all containers must be registered before pod.create(), or the runtime must use a stop\/recreate model for late containers",
  "image" : "docker.io\/library\/busybox:1.36.1",
  "kernel" : "\/tmp\/apple-containerization\/bin\/vmlinux-arm64",
  "lateAddSupported" : false,
  "lateStartSupported" : false,
  "localhostAfterLateAdd" : false,
  "logs" : {
    "boot" : "\/tmp\/macvz-linuxpod-c2\/logs\/boot.log",
    "exec" : "\/tmp\/macvz-linuxpod-c2\/logs\/exec.log",
    "lateClient" : "\/tmp\/macvz-linuxpod-c2\/logs\/late-client.log",
    "server" : "\/tmp\/macvz-linuxpod-c2\/logs\/server.log"
  },
  "podID" : "macvz-c2-1782061042",
  "statsCount" : 1,
  "workDir" : "\/tmp\/macvz-linuxpod-c2"
}
```

## Acceptance / Interpretation
- [x] One LinuxPod was created.
- [x] Server was registered before pod.create().
- [x] Pod and server were started before the late add attempt.
- [x] The post-create addContainer/start/probe outcome was recorded.
- [x] The fallback model is included in the JSON result.

