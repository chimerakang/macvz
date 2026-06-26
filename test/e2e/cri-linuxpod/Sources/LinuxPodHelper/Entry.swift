import ArgumentParser
import Containerization
import Darwin
import Foundation
import Logging

// LinuxPodHelper is the real CRI-L1 (#126) LinuxPod backend helper. It speaks the
// pkg/runtime/linuxpod NDJSON protocol (the same wire protocol as the in-memory
// LinuxPodHelperStub) over a unix socket, but drives real Apple Containerization
// LinuxPod VMs and the R9/R16 late-rootfs identity primitive.
//
// CRI-L6-4 (#139) split the single process into two roles so a helper restart can
// preserve running Pods (true adoption), which the create-only public
// VirtualMachineManager API cannot give one process:
//   serve         (default) -> the router: owns the public CRI socket, the durable
//                              supervisor journal, and routing. Owns no VM.
//   supervise-pod (hidden)  -> a per-Pod supervisor: owns exactly one LinuxPod /
//                              VZVirtualMachineInstance handle and serves the same
//                              NDJSON protocol on a private socket the router routes to.
//
// `linuxpod-helper --socket ... --kernel ...` still works: bare options route to the
// default `serve` subcommand, so existing operators and tests are unaffected.
//
// Lifecycle mapping (docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md, R9 primitive) is
// unchanged inside each supervisor: CreatePod boots a LinuxPod VM with a long-lived
// holder, PrepareContainerRootfs/CreateContainer late-bind a vminitd process onto a
// staged rootfs, StartContainer gates Running on host-side identity verification,
// and Stop/Remove/Cleanup leave no residual VM/rootfs/handoff state.

@main
struct LinuxPodHelper: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "linuxpod-helper",
        abstract: "Real Apple Containerization LinuxPod backend for the macvz-cri linuxpod NDJSON protocol (CRI-L1).",
        subcommands: [Serve.self, SupervisePod.self],
        defaultSubcommand: Serve.self
    )
}

// Runtime options shared by the router (to pass to spawned supervisors) and by a
// supervisor (to bootstrap its own VM runtime).
struct RuntimeOptions: ParsableArguments {
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
}

// Serve is the router: the main helper process operators and the Go adapter talk to.
struct Serve: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "serve",
        abstract: "Serve the public CRI socket and route pod operations to per-Pod supervisors."
    )

    @Option(help: "Unix socket path to serve the NDJSON protocol on.")
    var socket: String

    @OptionGroup var runtime: RuntimeOptions

    // supervisorCommand overrides the argv prefix used to launch a supervisor. Hidden
    // and defaulting to `<self> supervise-pod`; tests point it at the in-memory stub to
    // exercise routing/journal/adopt/fallback without booting a real VM.
    @Option(parsing: .upToNextOption, help: .hidden)
    var supervisorCommand: [String] = []

    func run() async throws {
        // A supervisor can die while the router holds an open connection to it; writing
        // to that dead socket must surface as EPIPE (adoption fallback), not SIGPIPE.
        signal(SIGPIPE, SIG_IGN)
        LoggingSystem.bootstrap(StreamLogHandler.standardError)
        let logger = Logger(label: "macvz.linuxpod-helper")

        let command = supervisorCommand.isEmpty
            ? [Self.selfExecutable(), "supervise-pod"]
            : supervisorCommand
        let config = RouterConfig(
            supervisorCommand: command,
            kernel: runtime.kernel,
            initfsReference: runtime.initfsReference,
            containerizationRoot: runtime.containerizationRoot,
            image: runtime.image,
            workDir: runtime.workDir,
            rosetta: runtime.rosetta,
            vmnet: runtime.vmnet)
        let router = RouterBackend(config: config, logger: logger)
        let server = NDJSONServer(socketPath: socket, backend: router, logger: logger)
        try await server.serve()
    }

    private static func selfExecutable() -> String {
        if let path = Bundle.main.executablePath { return path }
        return CommandLine.arguments.first ?? "linuxpod-helper"
    }
}

// SupervisePod is one per-Pod supervisor: it owns a single LinuxPod VM and serves the
// NDJSON protocol on a private socket. Hidden from normal operators; the router spawns
// it. It detaches with setsid() so a router restart leaves the Pod VM running.
struct SupervisePod: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "supervise-pod",
        abstract: "Internal: own one Pod VM and serve it on a private socket.",
        shouldDisplay: false
    )

    @Option(help: "Private unix socket path to serve this Pod on.")
    var socket: String

    @Option(help: "The Pod this supervisor owns (informational; the VM is created on CreatePod).")
    var podId: String

    @OptionGroup var runtime: RuntimeOptions

    func run() async throws {
        // Detach from the router's process group/session so a SIGTERM/SIGKILL to the
        // router alone does not take down this Pod's VM — the basis for adoption.
        _ = setsid()
        signal(SIGPIPE, SIG_IGN)

        LoggingSystem.bootstrap(StreamLogHandler.standardError)
        let logger = Logger(label: "macvz.linuxpod-supervisor.\(podId.prefix(12))")

        let runtimeEnv = try await HelperRuntime.bootstrap(
            kernel: runtime.kernel,
            initfsReference: runtime.initfsReference,
            containerizationRoot: runtime.containerizationRoot,
            image: runtime.image,
            workDir: runtime.workDir,
            rosetta: runtime.rosetta,
            enableVmnet: runtime.vmnet,
            logger: logger
        )
        let backend = LinuxPodBackend(runtime: runtimeEnv, logger: logger)
        let server = NDJSONServer(socketPath: socket, backend: backend, logger: logger)
        try await server.serve()
    }
}
