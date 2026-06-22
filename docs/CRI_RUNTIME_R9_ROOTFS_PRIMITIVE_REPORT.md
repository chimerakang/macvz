# CRI-R9 vminitd Rootfs Primitive Launch Report (#101)

Date: 2026-06-22T08:18:51Z

## Environment

- Host: Darwin chimeras-Mac-mini-2.local 25.5.0 Darwin Kernel Version 25.5.0: Mon Apr 27 20:38:56 PDT 2026; root:xnu-12377.121.6~2/RELEASE_ARM64_T6000 arm64
- Swift: Apple Swift version 6.2.1 (swiftlang-6.2.1.4.8 clang-1700.4.4.1)
- Containerization checkout: /tmp/apple-containerization
- Kernel: /tmp/apple-containerization/bin/vmlinux-arm64
- Initfs reference: vminit:macvz-r10
- Image: docker.io/library/busybox:1.36.1
- Work dir: /tmp/macvz-runtime-r9
- Probe: r9
- vmnet interface: 0

## Result

```json
{
  "attempt" : {
    "cleanupOutput" : "cleanup_ok container=r9-late-alpha rootfs=\/run\/container\/r9-late-alpha\/rootfs",
    "cleanupSucceeded" : true,
    "errors" : {
      "experimentalApiShape" : "existing vminitd Copy(COPY_OUT\/COPY_IN archive) used as PrepareContainerRootfs",
      "rootfsPath" : "\/run\/container\/r9-late-alpha\/rootfs",
      "startOrWaitProcess" : "RPCError: internalError: \"startProcess: failed to start process: internalError: \"vmexec error: internalError: \"macvz-r10-vmexec-diagnostics=1\nbundlePath=\/run\/container\/r9-late-alpha\noriginalError=internalError: \"macvz-r10-errno=1 stage=remove(ptmx) errno=2 strerror=No such file or directory info= posix=Error Domain=NSPOSIXErrorDomain Code=2 \"No such file or directory\"\"\nroot.path=\/run\/container\/r9-late-alpha\/rootfs\nroot.readonly=false\nprocess.args=\/bin\/sh -c set -eu\nidentity=\"$(cat \/etc\/macvz-r9-identity)\"\n{\n  echo \"identity=${identity}\"\n  echo \"expected=macvz-r9-id=late-alpha\"\n  echo \"pwd=$(pwd)\"\n  echo \"proc_root=$(readlink \/proc\/self\/root 2>\/dev\/null || true)\"\n  echo \"root_mount=$(grep ' \/ ' \/proc\/mounts 2>\/dev\/null || true)\"\n  echo \"root_listing=$(ls -1 \/ 2>\/dev\/null | tr '\\n' ',' || true)\"\n} > \/macvz-r9-result\ntest \"${identity}\" = \"macvz-r9-id=late-alpha\"\nprocess.cwd=\/\nnamespaces=cgroup:,ipc:,mount:,pid:,uts:\nmounts=proc:proc->\/proc,tmpfs:tmpfs->\/dev,devpts:devpts->\/dev\/pts,sysfs:sysfs->\/sys,tmpfs:tmpfs->\/dev\/shm\npath.bundle=\/run\/container\/r9-late-alpha:exists type=dir mode=755 size=80 readable=true executable=true\npath.bundle.config=\/run\/container\/r9-late-alpha\/config.json:exists type=file mode=644 size=1866 readable=true executable=false\npath.rootfs=\/run\/container\/r9-late-alpha\/rootfs:exists type=dir mode=755 size=200 readable=true executable=true\npath.rootfs.etc.identity=\/run\/container\/r9-late-alpha\/rootfs\/etc\/macvz-r9-identity:exists type=file mode=644 size=23 readable=true executable=false\npath.rootfs.bin.busybox=\/run\/container\/r9-late-alpha\/rootfs\/bin\/busybox:exists type=file mode=755 size=1119816 readable=true executable=true\npath.rootfs.bin.sh=\/run\/container\/r9-late-alpha\/rootfs\/bin\/sh:exists type=file mode=755 size=1119816 readable=true executable=true\npath.rootfs.lib.ld-linux-aarch64=\/run\/container\/r9-late-alpha\/rootfs\/lib\/ld-linux-aarch64.so.1:exists type=file mode=755 size=201344 readable=true executable=true\npath.rootfs.lib64.ld-linux-aarch64=\/run\/container\/r9-late-alpha\/rootfs\/lib64\/ld-linux-aarch64.so.1:missing errno=2 No such file or directory\npath.rootfs.lib.libc=\/run\/container\/r9-late-alpha\/rootfs\/lib\/libc.so.6:exists type=file mode=755 size=1716616 readable=true executable=true\npath.rootfs.usr.lib.libc=\/run\/container\/r9-late-alpha\/rootfs\/usr\/lib\/libc.so.6:missing errno=2 No such file or directory\npath.rootfs.cwd=\/run\/container\/r9-late-alpha\/rootfs\/:exists type=dir mode=755 size=200 readable=true executable=true\npath.rootfs.executable=\/run\/container\/r9-late-alpha\/rootfs\/bin\/sh:exists type=file mode=755 size=1119816 readable=true executable=true\nmountinfo.relevant=40 38 254:16 \/ \/run\/container\/utility\/rootfs rw,relatime - ext4 \/dev\/vdb rw | 55 38 0:20 \/container\/r9-late-alpha\/rootfs \/run\/container\/r9-late-alpha\/rootfs rw,relatime - tmpfs tmpfs rw | 56 55 0:32 \/ \/run\/container\/r9-late-alpha\/rootfs\/proc rw,relatime - proc proc rw | 57 55 0:33 \/ \/run\/container\/r9-late-alpha\/rootfs\/dev rw,nosuid,relatime - tmpfs tmpfs rw,size=65536k,mode=755 | 58 57 0:34 \/ \/run\/container\/r9-late-alpha\/rootfs\/dev\/pts rw,nosuid,noexec,relatime - devpts devpts rw,gid=5,mode=620,ptmxmode=666 | 59 55 0:21 \/ \/run\/container\/r9-late-alpha\/rootfs\/sys rw,nosuid,nodev,noexec,relatime - sysfs sysfs rw | 60 57 0:35 \/ \/run\/container\/r9-late-alpha\/rootfs\/dev\/shm rw,nosuid,nodev,noexec,relatime - tmpfs tmpfs rw,size=65536k\"\"\"",
      "verify" : "skipped because process did not start and exit successfully",
      "vmexecDiagnosticsUnavailable" : "ContainerizationError: notFound: \"stat: path not found '\/run\/container\/r9-late-alpha\/rootfs\/run\/macvz-r10-vmexec-diagnostics.txt'\" (cause: \"notFound: \"stat: path not found '\/run\/container\/r9-late-alpha\/rootfs\/run\/macvz-r10-vmexec-diagnostics.txt'\"\")",
      "vmexecErrorPipeUnavailable" : "ContainerizationError: notFound: \"stat: path not found '\/run\/container\/r9-late-alpha\/rootfs\/run\/macvz-r10-vmexec-errorpipe.txt'\" (cause: \"notFound: \"stat: path not found '\/run\/container\/r9-late-alpha\/rootfs\/run\/macvz-r10-vmexec-errorpipe.txt'\"\")"
    },
    "namespaceVerified" : false,
    "outcome" : "vminitdContainerStartFailed",
    "processContainerID" : "r9-late-alpha",
    "processCreateSucceeded" : true,
    "processID" : "r9-late-alpha",
    "processStartSucceeded" : false,
    "requestID" : "late-alpha",
    "resultPath" : "\/run\/container\/r9-late-alpha\/rootfs\/macvz-r9-result",
    "resultVerified" : false,
    "stageOutput" : "prepare_ok source=\/run\/container\/utility\/rootfs\/bin\/busybox rootfs=\/run\/container\/r9-late-alpha\/rootfs",
    "stagePath" : "\/run\/container\/r9-late-alpha\/rootfs",
    "stageSucceeded" : true,
    "verifyOutput" : ""
  },
  "durationSeconds" : 2.2947570085525513,
  "errors" : {

  },
  "image" : "docker.io\/library\/busybox:1.36.1",
  "kernel" : "\/tmp\/apple-containerization\/bin\/vmlinux-arm64",
  "logs" : {
    "boot" : "\/tmp\/macvz-runtime-r9\/logs\/boot.log",
    "exec" : "\/tmp\/macvz-runtime-r9\/logs\/exec.log",
    "utility" : "\/tmp\/macvz-runtime-r9\/logs\/utility.log"
  },
  "note" : "R9 uses existing vminitd Copy archive transport as a local experimental PrepareContainerRootfs shape, then launches through the existing new-container process path",
  "outcome" : "vminitdContainerStartFailed",
  "podCreated" : true,
  "podID" : "macvz-r9-1782116328",
  "transportAvailable" : true,
  "utilityStarted" : true,
  "workDir" : "\/tmp\/macvz-runtime-r9"
}
```

## Acceptance / Interpretation
- [x] One LinuxPod was created with a predeclared utility container.
- [x] The probe used existing vminitd Copy(COPY_OUT/COPY_IN archive) as a local experimental PrepareContainerRootfs shape.
- [x] The prepared rootfs was copied to /run/container/<containerID>/rootfs, or the precise prepare failure was recorded.
- [x] vminitd createProcess/startProcess was called with id == containerID to target the new-container path.
- [x] Identity, namespace/rootfs evidence, deleteProcess cleanup, and the final R9 outcome were recorded.

## Experimental API Shape

No production MacVz runtime code was changed for this run. The local
experimental shape used by the harness was:

1. Treat existing `vminitd.copy(direction: .copyOut/.copyIn, isArchive: true)`
   as a temporary `PrepareContainerRootfs` transport.
2. Copy `/run/container/utility/rootfs/bin/busybox` and
   `/run/container/utility/rootfs/lib` out of the running Pod VM.
3. Build a minimal host-side rootfs with `/bin/sh`, `/bin/busybox`, `/lib`,
   `/etc/macvz-r9-identity`, `/proc`, `/sys`, `/dev`, `/run`, and `/tmp`.
4. Copy that rootfs back into the VM at
   `/run/container/r9-late-alpha/rootfs`.
5. Call the existing vminitd new-container path:
   `createProcess(id: "r9-late-alpha", containerID: "r9-late-alpha", spec.root.path: "/run/container/r9-late-alpha/rootfs")`.
6. Call `startProcess`, then `deleteProcess`, and verify the prepared rootfs is
   removed.

## R10 Instrumentation Note

This report was regenerated during CRI-R10 with an instrumented local
apple/containerization initfs (`vminit:macvz-r10`). The outcome remains
`vminitdContainerStartFailed`, but the error is now explained:

```text
macvz-r10-errno=1 stage=remove(ptmx) errno=2 strerror=No such file or directory
```

The diagnostic payload shows the bundle path, `config.json`, prepared rootfs,
`/bin/sh`, `/lib/ld-linux-aarch64.so.1`, and `/lib/libc.so.6` all existed and
were readable/executable where relevant. The R10 conclusion is published at
[CRI_RUNTIME_R10_VMEXEC_START_REPORT.md](CRI_RUNTIME_R10_VMEXEC_START_REPORT.md).
