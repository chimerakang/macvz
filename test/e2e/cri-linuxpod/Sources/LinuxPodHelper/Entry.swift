import ArgumentParser
import Containerization
import Darwin
import Foundation
import Logging

// LinuxPodHelper is the real CRI-L1 (#126) LinuxPod backend helper. It speaks the
// pkg/runtime/linuxpod NDJSON protocol (the same wire protocol as the in-memory
// LinuxPodHelperStub) over a unix socket, but drives a real Apple Containerization
// LinuxPod and the R9/R16 late-rootfs identity primitive instead of an in-memory
// model. The Go adapter and the protocol are unchanged; only the model behind the
// socket is real (Ping reports simulated=false).
//
// Lifecycle mapping (docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md, R9 primitive):
//   CreatePod              -> boot a LinuxPod VM with one long-lived "holder"
//                             container so the VM (and its shared namespace) stays
//                             up for late containers.
//   PrepareContainerRootfs -> stage a prepared rootfs + its expected identity into
//                             the already-running Pod VM via the vminitd Copy
//                             primitive (the late-rootfs primitive proven in R9).
//   CreateContainer        -> late-bind a vminitd process onto the prepared rootfs
//                             (works after other containers are already running).
//   StartContainer         -> start the process and gate Running on host-side
//                             identity verification through the bind-mounted handoff
//                             evidence channel (CRI-R16). Never Running on mismatch.
//   StopContainer/RemoveContainer/Status/Cleanup -> stop/delete the process and its
//                             staged rootfs, leaving no residual VM/rootfs/handoff.
//
// Usage: linuxpod-helper --socket /path/to/helper.sock --kernel <vmlinux> ...

@main
struct LinuxPodHelper: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "linuxpod-helper",
        abstract: "Real Apple Containerization LinuxPod backend for the macvz-cri linuxpod NDJSON protocol (CRI-L1)."
    )

    @Option(help: "Unix socket path to serve the NDJSON protocol on.")
    var socket: String

    @Option(help: "Linux kernel path from apple/containerization, e.g. containerization/bin/vmlinux.")
    var kernel: String = "containerization/bin/vmlinux"

    @Option(help: "Init filesystem OCI reference in the local Apple Containerization image store.")
    var initfsReference: String = "vminit:latest"

    @Option(help: "Apple Containerization image/root state root.")
    var containerizationRoot: String?

    @Option(help: "OCI image unpacked for the Pod's holder container (provides busybox for staged rootfs).")
    var image: String = "docker.io/library/busybox:1.36.1"

    @Option(help: "Working directory for generated rootfs, evidence, logs, and state.")
    var workDir: String = "/tmp/macvz-linuxpod-helper"

    @Flag(help: "Enable Rosetta for linux/amd64 images.")
    var rosetta = false

    @Flag(help: "Attach a vmnet interface to each Pod and report its IPv4 as sandboxAddress.")
    var vmnet = false

    func run() async throws {
        LoggingSystem.bootstrap(StreamLogHandler.standardError)
        let logger = Logger(label: "macvz.linuxpod-helper")

        let runtime = try await HelperRuntime.bootstrap(
            kernel: kernel,
            initfsReference: initfsReference,
            containerizationRoot: containerizationRoot,
            image: image,
            workDir: workDir,
            rosetta: rosetta,
            enableVmnet: vmnet,
            logger: logger
        )
        let backend = LinuxPodBackend(runtime: runtime, logger: logger)

        let server = NDJSONServer(socketPath: socket, backend: backend, logger: logger)
        try await server.serve()
    }
}
