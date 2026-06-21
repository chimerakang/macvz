import ArgumentParser
import Containerization
import ContainerizationEXT4
import ContainerizationExtras
import ContainerizationOCI
import Foundation
import Logging
@preconcurrency import Virtualization

@main
struct LinuxPodSharedNamespacePoC: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "linuxpod-shared-namespace-poc",
        abstract: "Validate two-container shared-network LinuxPod semantics for MacVz CRI route C."
    )

    @Option(help: "Linux kernel path from apple/containerization, for example containerization/bin/vmlinux.")
    var kernel: String = "containerization/bin/vmlinux"

    @Option(help: "Init filesystem OCI reference in the local Apple Containerization image store.")
    var initfsReference: String = "vminit:latest"

    @Option(help: "Apple Containerization image/root state root.")
    var containerizationRoot: String?

    @Option(help: "OCI image to unpack for both containers.")
    var image: String = "docker.io/library/busybox:1.36.1"

    @Option(help: "Working directory for generated rootfs, logs, and report artifacts.")
    var workDir: String = "/tmp/macvz-linuxpod-poc"

    @Option(help: "TCP port inside the shared Pod namespace.")
    var port: Int = 18080

    @Option(help: "Probe to run: c1 for pre-create two-container semantics, c2 for post-create addContainer ordering, c4 for HotplugProvider boundary.")
    var probe: String = "c1"

    @Flag(help: "Enable Rosetta for linux/amd64 images.")
    var rosetta = false

    @Flag(help: "Attach a vmnet interface to the LinuxPod. Disabled by default for the C1 shared-namespace probe.")
    var vmnet = false

    func run() async throws {
        LoggingSystem.bootstrap(StreamLogHandler.standardError)
        let logger = Logger(label: "macvz.linuxpod-poc")
        let startedAt = Date()

        let fileManager = FileManager.default
        let workURL = URL(fileURLWithPath: workDir)
        let logsURL = workURL.appendingPathComponent("logs")
        let rootfsURL = workURL.appendingPathComponent("rootfs")
        try? fileManager.removeItem(at: workURL)
        try fileManager.createDirectory(at: logsURL, withIntermediateDirectories: true)
        try fileManager.createDirectory(at: rootfsURL, withIntermediateDirectories: true)

        let stateRoot = containerizationRoot.map { URL(fileURLWithPath: $0) }
            ?? fileManager.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
                .appendingPathComponent("com.apple.containerization")
        try fileManager.createDirectory(at: stateRoot, withIntermediateDirectories: true)

        guard fileManager.fileExists(atPath: kernel) else {
            throw ValidationError("kernel does not exist: \(kernel)")
        }

        let imageStore = try ImageStore(path: stateRoot)
        let initfs = try await prepareInitfs(imageStore: imageStore, stateRoot: stateRoot, fileManager: fileManager)
        let vmm = VZVirtualMachineManager(
            kernel: Kernel(path: URL(fileURLWithPath: kernel), platform: .linuxArm),
            initialFilesystem: initfs,
            rosetta: rosetta,
            logger: logger
        )

        if probe == "c2" {
            try await runOrderingProbe(
                imageStore: imageStore,
                vmm: vmm,
                logger: logger,
                startedAt: startedAt,
                logsURL: logsURL,
                rootfsURL: rootfsURL
            )
            return
        }
        if probe == "c4" {
            try await runHotplugProviderProbe(
                imageStore: imageStore,
                vmm: vmm,
                logger: logger,
                startedAt: startedAt,
                logsURL: logsURL,
                rootfsURL: rootfsURL
            )
            return
        }
        guard probe == "c1" else {
            throw ValidationError("unsupported probe \(probe); expected c1, c2, or c4")
        }

        let podID = "macvz-poc-\(Int(startedAt.timeIntervalSince1970))"
        var network: VmnetNetwork?
        var podInterface: Interface?
        if vmnet {
            network = try VmnetNetwork()
            podInterface = try network?.createInterface(podID)
            guard podInterface != nil else {
                throw ValidationError("failed to allocate vmnet interface for \(podID)")
            }
        }
        defer {
            try? network?.releaseInterface(podID)
        }

        let pod = try LinuxPod(podID, vmm: vmm, logger: logger) { config in
            config.cpus = 2
            config.memoryInBytes = 1024 * 1024 * 1024
            if let podInterface {
                config.interfaces = [podInterface]
            }
            config.hostname = "macvz-linuxpod-poc"
            config.bootLog = .file(path: logsURL.appendingPathComponent("boot.log"))
        }

        let baseImage = try await imageStore.get(reference: image, pull: true)
        let serverRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("server.ext4"))
        let clientRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("client.ext4"))

        let serverLog = try FileLogWriter(path: logsURL.appendingPathComponent("server.log"), stream: "stdout")
        let serverErr = try FileLogWriter(path: logsURL.appendingPathComponent("server.log"), stream: "stderr")
        let clientLog = try FileLogWriter(path: logsURL.appendingPathComponent("client.log"), stream: "stdout")
        let clientErr = try FileLogWriter(path: logsURL.appendingPathComponent("client.log"), stream: "stderr")
        let execLog = try FileLogWriter(path: logsURL.appendingPathComponent("exec.log"), stream: "stdout")

        try await pod.addContainer("server", rootfs: serverRootfs) { config in
            config.process.arguments = [
                "/bin/sh",
                "-c",
                "mkdir -p /www; echo macvz-linuxpod-localhost-ok > /www/index.html; exec httpd -f -p 127.0.0.1:\(port) -h /www",
            ]
            config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
            config.process.workingDirectory = "/"
            config.process.stdout = serverLog
            config.process.stderr = serverErr
            config.useInit = true
        }

        try await pod.addContainer("client", rootfs: clientRootfs) { config in
            config.process.arguments = [
                "/bin/sh",
                "-c",
                "for i in $(seq 1 60); do if wget -qO /tmp/localhost-result http://127.0.0.1:\(port); then grep -q macvz-linuxpod-localhost-ok /tmp/localhost-result && touch /tmp/localhost-ok && exec sleep 300; fi; sleep 1; done; exit 42",
            ]
            config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
            config.process.workingDirectory = "/"
            config.process.stdout = clientLog
            config.process.stderr = clientErr
            config.useInit = true
        }

        var podCreated = false
        do {
            try await pod.create()
            podCreated = true
            try await pod.startContainer("server")
            try await pod.startContainer("client")

            try await waitForClientProbe(pod, stdout: execLog)

            let exec = try await pod.execInContainer("server", processID: "ip-probe") { config in
                config.arguments = ["/bin/sh", "-c", "hostname; ip -o -4 addr show scope global 2>/dev/null || true"]
                config.stdout = execLog
            }
            try await exec.start()
            let execStatus = try await exec.wait()
            try await exec.delete()
            guard execStatus.exitCode == 0 else {
                throw ValidationError("exec probe failed with exit code \(execStatus.exitCode)")
            }

            let stats = try await pod.statistics(containerIDs: ["server", "client"], categories: [.cpu, .memory])
            guard stats.count == 2 else {
                throw ValidationError("expected stats for two containers, got \(stats.count)")
            }

            try await pod.stopContainer("server")
            let postStopStats = try await pod.statistics(containerIDs: ["client"], categories: [.memory])
            guard !postStopStats.isEmpty else {
                throw ValidationError("client stats unavailable after stopping server first")
            }

            try await pod.stopContainer("client")
            try await pod.stop()
            try close(serverLog, serverErr, clientLog, clientErr, execLog)

            let finishedAt = Date()
            let result = ResultSummary(
                podID: podID,
                podIP: podInterface?.ipv4Address.address.description,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: finishedAt.timeIntervalSince(startedAt),
                logs: [
                    "server": logsURL.appendingPathComponent("server.log").path,
                    "client": logsURL.appendingPathComponent("client.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        } catch {
            if podCreated {
                try? await pod.stop()
            }
            try? close(serverLog, serverErr, clientLog, clientErr, execLog)
            throw error
        }
    }

    private func prepareInitfs(
        imageStore: ImageStore,
        stateRoot: URL,
        fileManager: FileManager
    ) async throws -> Containerization.Mount {
        let initPath = stateRoot.appendingPathComponent("initfs.ext4")
        let initImage = try await imageStore.getInitImage(reference: initfsReference)
        do {
            return try await initImage.initBlock(at: initPath, for: .linuxArm)
        } catch {
            if fileManager.fileExists(atPath: initPath.path) {
                return .block(format: "ext4", source: initPath.path, destination: "/", options: ["ro"])
            }
            throw error
        }
    }

    private func unpackRootfs(_ image: Containerization.Image, at url: URL) async throws -> Containerization.Mount {
        if FileManager.default.fileExists(atPath: url.path) {
            return .block(format: "ext4", source: url.path, destination: "/", options: [])
        }
        let unpacker = EXT4Unpacker(blockSizeInBytes: 2 * 1024 * 1024 * 1024)
        return try await unpacker.unpack(image, for: .current, at: url)
    }

    private func runOrderingProbe(
        imageStore: ImageStore,
        vmm: VZVirtualMachineManager,
        logger: Logger,
        startedAt: Date,
        logsURL: URL,
        rootfsURL: URL
    ) async throws {
        let podID = "macvz-c2-\(Int(startedAt.timeIntervalSince1970))"
        let pod = try LinuxPod(podID, vmm: vmm, logger: logger) { config in
            config.cpus = 2
            config.memoryInBytes = 1024 * 1024 * 1024
            config.hostname = "macvz-linuxpod-c2"
            config.bootLog = .file(path: logsURL.appendingPathComponent("boot.log"))
        }

        let baseImage = try await imageStore.get(reference: image, pull: true)
        let serverRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("server.ext4"))
        let clientRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("late-client.ext4"))

        let serverLog = try FileLogWriter(path: logsURL.appendingPathComponent("server.log"), stream: "stdout")
        let serverErr = try FileLogWriter(path: logsURL.appendingPathComponent("server.log"), stream: "stderr")
        let clientLog = try FileLogWriter(path: logsURL.appendingPathComponent("late-client.log"), stream: "stdout")
        let clientErr = try FileLogWriter(path: logsURL.appendingPathComponent("late-client.log"), stream: "stderr")
        let execLog = try FileLogWriter(path: logsURL.appendingPathComponent("exec.log"), stream: "stdout")

        var podCreated = false
        var serverStarted = false
        var clientStarted = false
        var lateAddSupported = false
        var lateStartSupported = false
        var localhostAfterLateAdd = false
        var errors: [String: String] = [:]
        var statsCount: Int?

        do {
            try await pod.addContainer("server", rootfs: serverRootfs) { config in
                config.process.arguments = [
                    "/bin/sh",
                    "-c",
                    "mkdir -p /www; echo macvz-linuxpod-localhost-ok > /www/index.html; exec httpd -f -p 127.0.0.1:\(port) -h /www",
                ]
                config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
                config.process.workingDirectory = "/"
                config.process.stdout = serverLog
                config.process.stderr = serverErr
                config.useInit = true
            }

            try await pod.create()
            podCreated = true
            try await pod.startContainer("server")
            serverStarted = true

            do {
                try await pod.addContainer("late-client", rootfs: clientRootfs) { config in
                    config.process.arguments = [
                        "/bin/sh",
                        "-c",
                        "for i in $(seq 1 30); do if wget -qO /tmp/localhost-result http://127.0.0.1:\(port); then grep -q macvz-linuxpod-localhost-ok /tmp/localhost-result && touch /tmp/localhost-ok && exec sleep 300; fi; sleep 1; done; exit 42",
                    ]
                    config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
                    config.process.workingDirectory = "/"
                    config.process.stdout = clientLog
                    config.process.stderr = clientErr
                    config.useInit = true
                }
                lateAddSupported = true
            } catch {
                errors["lateAdd"] = describe(error)
            }

            if lateAddSupported {
                do {
                    try await pod.startContainer("late-client")
                    clientStarted = true
                    lateStartSupported = true
                    try await waitForClientProbe(pod, containerID: "late-client", stdout: execLog, timeoutSeconds: 45)
                    localhostAfterLateAdd = true
                } catch {
                    errors["lateStartOrProbe"] = describe(error)
                }
            }

            if let stats = try? await pod.statistics(
                containerIDs: lateAddSupported ? ["server", "late-client"] : ["server"],
                categories: [.cpu, .memory]
            ) {
                statsCount = stats.count
            }

            if clientStarted {
                try? await pod.stopContainer("late-client")
            }
            if serverStarted {
                try? await pod.stopContainer("server")
            }
            try await pod.stop()
            try close(serverLog, serverErr, clientLog, clientErr, execLog)

            let fallback: String
            if lateAddSupported && lateStartSupported && localhostAfterLateAdd {
                fallback = "post-create addContainer works in this probe; route C may preserve kubelet ordering for this narrow case"
            } else if !lateAddSupported {
                fallback = "all containers must be registered before pod.create(), or the runtime must use a stop/recreate model for late containers"
            } else {
                fallback = "post-create addContainer returned successfully, but start/probe failed; treat late containers as unsupported until resolved"
            }

            let result = OrderingProbeSummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                lateAddSupported: lateAddSupported,
                lateStartSupported: lateStartSupported,
                localhostAfterLateAdd: localhostAfterLateAdd,
                statsCount: statsCount,
                fallback: fallback,
                errors: errors,
                logs: [
                    "server": logsURL.appendingPathComponent("server.log").path,
                    "lateClient": logsURL.appendingPathComponent("late-client.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        } catch {
            if podCreated {
                try? await pod.stop()
            }
            try? close(serverLog, serverErr, clientLog, clientErr, execLog)
            throw error
        }
    }

    private func runHotplugProviderProbe(
        imageStore: ImageStore,
        vmm: VZVirtualMachineManager,
        logger: Logger,
        startedAt: Date,
        logsURL: URL,
        rootfsURL: URL
    ) async throws {
        let podID = "macvz-c4-\(Int(startedAt.timeIntervalSince1970))"
        let probeState = HotplugProbeState()
        let hotplugExtension = ProbeHotplugExtension(state: probeState)
        let pod = try LinuxPod(podID, vmm: vmm, logger: logger) { config in
            config.cpus = 2
            config.memoryInBytes = 1024 * 1024 * 1024
            config.hostname = "macvz-linuxpod-c4"
            config.bootLog = .file(path: logsURL.appendingPathComponent("boot.log"))
            config.extensions.append(hotplugExtension)
        }

        let baseImage = try await imageStore.get(reference: image, pull: true)
        let serverRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("server.ext4"))
        let clientRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("late-client.ext4"))

        let serverLog = try FileLogWriter(path: logsURL.appendingPathComponent("server.log"), stream: "stdout")
        let serverErr = try FileLogWriter(path: logsURL.appendingPathComponent("server.log"), stream: "stderr")
        let clientLog = try FileLogWriter(path: logsURL.appendingPathComponent("late-client.log"), stream: "stdout")
        let clientErr = try FileLogWriter(path: logsURL.appendingPathComponent("late-client.log"), stream: "stderr")
        let execLog = try FileLogWriter(path: logsURL.appendingPathComponent("exec.log"), stream: "stdout")

        var podCreated = false
        var serverStarted = false
        var lateAddReturned = false
        var lateStartSucceeded = false
        var localhostAfterLateAdd = false
        var errors: [String: String] = [:]

        do {
            try await pod.addContainer("server", rootfs: serverRootfs) { config in
                config.process.arguments = [
                    "/bin/sh",
                    "-c",
                    "mkdir -p /www; echo macvz-linuxpod-hotplug-ok > /www/index.html; exec httpd -f -p 127.0.0.1:\(port) -h /www",
                ]
                config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
                config.process.workingDirectory = "/"
                config.process.stdout = serverLog
                config.process.stderr = serverErr
                config.useInit = true
            }

            try await pod.create()
            podCreated = true
            try await pod.startContainer("server")
            serverStarted = true

            do {
                try await pod.addContainer("late-client", rootfs: clientRootfs) { config in
                    config.process.arguments = [
                        "/bin/sh",
                        "-c",
                        "for i in $(seq 1 30); do if wget -qO /tmp/localhost-result http://127.0.0.1:\(port); then grep -q macvz-linuxpod-hotplug-ok /tmp/localhost-result && touch /tmp/localhost-ok && exec sleep 300; fi; sleep 1; done; exit 42",
                    ]
                    config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
                    config.process.workingDirectory = "/"
                    config.process.stdout = clientLog
                    config.process.stderr = clientErr
                    config.useInit = true
                }
                lateAddReturned = true
            } catch {
                errors["lateAdd"] = describe(error)
            }

            if lateAddReturned {
                do {
                    try await pod.startContainer("late-client")
                    lateStartSucceeded = true
                    try await waitForClientProbe(pod, containerID: "late-client", stdout: execLog, timeoutSeconds: 45)
                    localhostAfterLateAdd = true
                } catch {
                    errors["lateStartOrProbe"] = describe(error)
                }
            }

            if lateStartSucceeded {
                try? await pod.stopContainer("late-client")
            }
            if serverStarted {
                try? await pod.stopContainer("server")
            }
            try await pod.stop()
            try close(serverLog, serverErr, clientLog, clientErr, execLog)

            let snapshot = probeState.snapshot()
            let outcome = hotplugOutcome(
                snapshot: snapshot,
                lateAddReturned: lateAddReturned,
                lateStartSucceeded: lateStartSucceeded,
                localhostAfterLateAdd: localhostAfterLateAdd
            )
            let result = HotplugProbeSummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                providerInstalled: snapshot.providerInstalled,
                providerCalled: snapshot.providerCalled,
                usbControllerConfigured: snapshot.usbControllerConfigured,
                usbAttachAttempted: snapshot.usbAttachAttempted,
                usbAttachSucceeded: snapshot.usbAttachSucceeded,
                guestPathResolved: snapshot.guestPathResolved,
                lateAddReturned: lateAddReturned,
                lateStartSucceeded: lateStartSucceeded,
                localhostAfterLateAdd: localhostAfterLateAdd,
                outcome: outcome,
                providerEvents: snapshot.events,
                errors: errors,
                logs: [
                    "server": logsURL.appendingPathComponent("server.log").path,
                    "lateClient": logsURL.appendingPathComponent("late-client.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        } catch {
            if podCreated {
                try? await pod.stop()
            }
            try? close(serverLog, serverErr, clientLog, clientErr, execLog)
            throw error
        }
    }

    private func hotplugOutcome(
        snapshot: HotplugProbeSnapshot,
        lateAddReturned: Bool,
        lateStartSucceeded: Bool,
        localhostAfterLateAdd: Bool
    ) -> String {
        if !snapshot.providerInstalled {
            return "providerCannotBeInstalled"
        }
        if !snapshot.providerCalled {
            return "providerInstalledButNotCalled"
        }
        if snapshot.usbAttachAttempted && !snapshot.usbAttachSucceeded {
            return "providerCalledButPublicApiCannotAttachRootfs"
        }
        if snapshot.usbAttachSucceeded && !snapshot.guestPathResolved {
            return "providerCalledUsbAttachedNoGuestPath"
        }
        if lateAddReturned && !lateStartSucceeded {
            return "rootfsAttachedButLateContainerDidNotStart"
        }
        if lateStartSucceeded && localhostAfterLateAdd {
            return "lateContainerStartedSuccessfully"
        }
        return "unknown"
    }

    private func waitForClientProbe(_ pod: LinuxPod, stdout: FileLogWriter) async throws {
        try await waitForClientProbe(pod, containerID: "client", stdout: stdout, timeoutSeconds: 90)
    }

    private func waitForClientProbe(
        _ pod: LinuxPod,
        containerID: String,
        stdout: FileLogWriter,
        timeoutSeconds: TimeInterval
    ) async throws {
        let deadline = Date().addingTimeInterval(timeoutSeconds)
        var attempts = 0
        while Date() < deadline {
            attempts += 1
            let exec = try await pod.execInContainer(containerID, processID: "\(containerID)-probe-\(attempts)") { config in
                config.arguments = [
                    "/bin/sh",
                    "-c",
                    "test -f /tmp/localhost-ok && grep -q macvz-linuxpod-localhost-ok /tmp/localhost-result",
                ]
                config.stdout = stdout
            }
            try await exec.start()
            let status = try await exec.wait()
            try await exec.delete()
            if status.exitCode == 0 {
                return
            }
            try await Task.sleep(nanoseconds: 1_000_000_000)
        }
        throw ValidationError("\(containerID) did not confirm localhost probe before timeout")
    }

    private func describe(_ error: Error) -> String {
        "\(type(of: error)): \(error)"
    }
}

private struct ResultSummary: Encodable {
    let podID: String
    let podIP: String?
    let image: String
    let kernel: String
    let workDir: String
    let durationSeconds: TimeInterval
    let logs: [String: String]

    func jsonString() throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return String(decoding: try encoder.encode(self), as: UTF8.self)
    }
}

private struct OrderingProbeSummary: Encodable {
    let podID: String
    let image: String
    let kernel: String
    let workDir: String
    let durationSeconds: TimeInterval
    let lateAddSupported: Bool
    let lateStartSupported: Bool
    let localhostAfterLateAdd: Bool
    let statsCount: Int?
    let fallback: String
    let errors: [String: String]
    let logs: [String: String]

    func jsonString() throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return String(decoding: try encoder.encode(self), as: UTF8.self)
    }
}

private struct HotplugProbeSummary: Encodable {
    let podID: String
    let image: String
    let kernel: String
    let workDir: String
    let durationSeconds: TimeInterval
    let providerInstalled: Bool
    let providerCalled: Bool
    let usbControllerConfigured: Bool
    let usbAttachAttempted: Bool
    let usbAttachSucceeded: Bool
    let guestPathResolved: Bool
    let lateAddReturned: Bool
    let lateStartSucceeded: Bool
    let localhostAfterLateAdd: Bool
    let outcome: String
    let providerEvents: [String]
    let errors: [String: String]
    let logs: [String: String]

    func jsonString() throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return String(decoding: try encoder.encode(self), as: UTF8.self)
    }
}

private struct HotplugProbeSnapshot {
    let providerInstalled: Bool
    let providerCalled: Bool
    let usbControllerConfigured: Bool
    let usbAttachAttempted: Bool
    let usbAttachSucceeded: Bool
    let guestPathResolved: Bool
    let events: [String]
}

private final class HotplugProbeState: @unchecked Sendable {
    private let lock = NSLock()
    private var providerInstalledValue = false
    private var providerCalledValue = false
    private var usbControllerConfiguredValue = false
    private var usbAttachAttemptedValue = false
    private var usbAttachSucceededValue = false
    private var guestPathResolvedValue = false
    private var eventsValue: [String] = []

    func mark(_ event: String) {
        lock.lock()
        eventsValue.append(event)
        switch event {
        case "providerInstalled":
            providerInstalledValue = true
        case "providerCalled":
            providerCalledValue = true
        case "usbControllerConfigured":
            usbControllerConfiguredValue = true
        case "usbAttachAttempted":
            usbAttachAttemptedValue = true
        case "usbAttachSucceeded":
            usbAttachSucceededValue = true
        case "guestPathResolved":
            guestPathResolvedValue = true
        default:
            break
        }
        lock.unlock()
    }

    func snapshot() -> HotplugProbeSnapshot {
        lock.lock()
        defer { lock.unlock() }
        return HotplugProbeSnapshot(
            providerInstalled: providerInstalledValue,
            providerCalled: providerCalledValue,
            usbControllerConfigured: usbControllerConfiguredValue,
            usbAttachAttempted: usbAttachAttemptedValue,
            usbAttachSucceeded: usbAttachSucceededValue,
            guestPathResolved: guestPathResolvedValue,
            events: eventsValue
        )
    }
}

private struct HotplugProbeFailure: Error, CustomStringConvertible {
    let description: String
}

private final class ProbeHotplugExtension: VZInstanceExtension, @unchecked Sendable {
    private let state: HotplugProbeState

    init(state: HotplugProbeState) {
        self.state = state
    }

    func configureVZ(
        _ config: inout VZVirtualMachineConfiguration,
        allocator: any AddressAllocator<Character>,
        storageDeviceCount: Int,
        mountsByID: [String: [Containerization.Mount]]
    ) throws {
        config.usbControllers.append(VZXHCIControllerConfiguration())
        state.mark("usbControllerConfigured")
    }

    func didCreate(_ instance: VZVirtualMachineInstance) throws {
        instance.hotplugProvider = ProbeHotplugProvider(instance: instance, state: state)
        state.mark("providerInstalled")
    }
}

private final class ProbeHotplugProvider: HotplugProvider, @unchecked Sendable {
    private let instance: VZVirtualMachineInstance
    private let state: HotplugProbeState
    private let lock = NSLock()
    private var devicesByID: [String: VZUSBMassStorageDevice] = [:]

    init(instance: VZVirtualMachineInstance, state: HotplugProbeState) {
        self.instance = instance
        self.state = state
    }

    func hotplug(_ block: Containerization.Mount, id: String) async throws -> AttachedFilesystem {
        state.mark("providerCalled")
        guard block.isBlock else {
            throw HotplugProbeFailure(description: "hotplug probe only handles block rootfs mounts")
        }
        state.mark("usbAttachAttempted")

        let attachment = try VZDiskImageStorageDeviceAttachment(
            url: URL(fileURLWithPath: block.source),
            readOnly: block.options.contains("ro"),
            cachingMode: .cached,
            synchronizationMode: .fsync
        )
        let configuration = VZUSBMassStorageDeviceConfiguration(attachment: attachment)
        let device = VZUSBMassStorageDevice(configuration: configuration)

        try await attach(device: device)
        storeDevice(device, id: id)
        state.mark("usbAttachSucceeded")

        throw HotplugProbeFailure(
            description: "USB mass storage attach succeeded, but no public API provided a deterministic Linux guest block path for the ext4 rootfs; refusing to return a guessed AttachedFilesystem"
        )
    }

    func registerMounts(id: String, rootfs: AttachedFilesystem, additionalMounts: [Containerization.Mount]) throws {
        throw HotplugProbeFailure(description: "registerMounts should not be reached unless a real guest rootfs path is resolved")
    }

    func releaseHotplug(id: String) async throws {
        let device = removeDevice(id: id)
        if let device {
            try await detach(device: device)
        }
    }

    func hotplugVirtioFS(_ mounts: [Containerization.Mount], id: String) async throws {
        throw HotplugProbeFailure(description: "virtiofs hotplug is out of scope for the C4 rootfs boundary probe")
    }

    func releaseVirtioFS(id: String) async throws {}

    func cleanup() {
        lock.lock()
        devicesByID.removeAll()
        lock.unlock()
    }

    private func storeDevice(_ device: VZUSBMassStorageDevice, id: String) {
        lock.lock()
        devicesByID[id] = device
        lock.unlock()
    }

    private func removeDevice(id: String) -> VZUSBMassStorageDevice? {
        lock.lock()
        let device = devicesByID.removeValue(forKey: id)
        lock.unlock()
        return device
    }

    private func attach(device: VZUSBMassStorageDevice) async throws {
        guard let controller = instance.vzVirtualMachine.usbControllers.first else {
            throw HotplugProbeFailure(description: "no VZ USB controller is available on the running VM")
        }
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            instance.vmQueue.async {
                controller.attach(device: device) { error in
                    if let error {
                        continuation.resume(throwing: error)
                    } else {
                        continuation.resume(returning: ())
                    }
                }
            }
        }
    }

    private func detach(device: VZUSBMassStorageDevice) async throws {
        guard let controller = instance.vzVirtualMachine.usbControllers.first else {
            return
        }
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            instance.vmQueue.async {
                controller.detach(device: device) { error in
                    if let error {
                        continuation.resume(throwing: error)
                    } else {
                        continuation.resume(returning: ())
                    }
                }
            }
        }
    }
}

private final class FileLogWriter: Writer, @unchecked Sendable {
    private let handle: FileHandle
    private let stream: String

    init(path: URL, stream: String) throws {
        self.stream = stream
        if !FileManager.default.fileExists(atPath: path.path) {
            FileManager.default.createFile(atPath: path.path, contents: nil)
        }
        self.handle = try FileHandle(forWritingTo: path)
        try self.handle.seekToEnd()
    }

    func write(_ data: Data) throws {
        guard !data.isEmpty else { return }
        let timestamp = ISO8601DateFormatter().string(from: Date())
        let line = "\(timestamp) \(stream) F \(String(decoding: data, as: UTF8.self))"
        if let encoded = line.data(using: .utf8) {
            try handle.write(contentsOf: encoded)
        }
    }

    func close() throws {
        try handle.close()
    }
}

private func close(_ writers: FileLogWriter...) throws {
    for writer in writers {
        try writer.close()
    }
}
