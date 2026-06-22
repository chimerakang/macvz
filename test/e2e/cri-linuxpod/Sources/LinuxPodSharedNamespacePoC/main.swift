import ArgumentParser
import Containerization
import ContainerizationArchive
import ContainerizationEXT4
import ContainerizationExtras
import ContainerizationOCI
import Darwin
import Foundation
import Logging
import NIOCore
import NIOPosix
import SystemPackage
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

    @Option(help: "Probe to run: c1 for pre-create two-container semantics, c2 for post-create addContainer ordering, c4 for HotplugProvider boundary, r1 for guest-side hotplug device discovery, r3 for NBD pre-create rootfs identity, r4 for guest-side late rootfs staging, r5 for VM-agent process execution from staged rootfs, r6 for vminitd new-container process from staged rootfs, r7 for vminitd-visible staged rootfs launch, r9 for vminitd Copy-based rootfs primitive launch.")
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
        if probe == "r1" {
            try await runRuntimeDeviceDiscoveryProbe(
                imageStore: imageStore,
                vmm: vmm,
                logger: logger,
                startedAt: startedAt,
                logsURL: logsURL,
                rootfsURL: rootfsURL
            )
            return
        }
        if probe == "r3" {
            try await runNBDRootfsIdentityProbe(
                imageStore: imageStore,
                vmm: vmm,
                logger: logger,
                startedAt: startedAt,
                logsURL: logsURL,
                rootfsURL: rootfsURL
            )
            return
        }
        if probe == "r4" {
            try await runGuestRootfsStagingProbe(
                imageStore: imageStore,
                vmm: vmm,
                logger: logger,
                startedAt: startedAt,
                logsURL: logsURL,
                rootfsURL: rootfsURL
            )
            return
        }
        if probe == "r5" {
            try await runStagedRootfsProcessProbe(
                imageStore: imageStore,
                vmm: vmm,
                logger: logger,
                startedAt: startedAt,
                logsURL: logsURL,
                rootfsURL: rootfsURL
            )
            return
        }
        if probe == "r6" {
            try await runVminitdContainerRootfsProbe(
                imageStore: imageStore,
                vmm: vmm,
                logger: logger,
                startedAt: startedAt,
                logsURL: logsURL,
                rootfsURL: rootfsURL
            )
            return
        }
        if probe == "r7" {
            try await runVminitdVisibleRootfsProbe(
                imageStore: imageStore,
                vmm: vmm,
                logger: logger,
                startedAt: startedAt,
                logsURL: logsURL,
                rootfsURL: rootfsURL
            )
            return
        }
        if probe == "r9" {
            try await runVminitdRootfsPrimitiveProbe(
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
            throw ValidationError("unsupported probe \(probe); expected c1, c2, c4, r1, r3, r4, r5, r6, r7, or r9")
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

    private func runRuntimeDeviceDiscoveryProbe(
        imageStore: ImageStore,
        vmm: VZVirtualMachineManager,
        logger: Logger,
        startedAt: Date,
        logsURL: URL,
        rootfsURL: URL
    ) async throws {
        let podID = "macvz-r1-\(Int(startedAt.timeIntervalSince1970))"
        let probeState = DeviceDiscoveryProbeState()
        let r1Extension = DeviceDiscoveryProbeExtension(state: probeState)
        let pod = try LinuxPod(podID, vmm: vmm, logger: logger) { config in
            config.cpus = 2
            config.memoryInBytes = 1024 * 1024 * 1024
            config.hostname = "macvz-runtime-r1"
            config.bootLog = .file(path: logsURL.appendingPathComponent("boot.log"))
            config.extensions.append(r1Extension)
        }

        let baseImage = try await imageStore.get(reference: image, pull: true)
        let utilityRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("utility.ext4"))
        let targetRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("target-rootfs.ext4"))
        let targetRootfsSize = try fileSize(at: URL(fileURLWithPath: targetRootfs.source))
        let expectedSectors = targetRootfsSize / 512

        let utilityLog = try FileLogWriter(path: logsURL.appendingPathComponent("utility.log"), stream: "stdout")
        let utilityErr = try FileLogWriter(path: logsURL.appendingPathComponent("utility.log"), stream: "stderr")
        let execLog = try FileLogWriter(path: logsURL.appendingPathComponent("exec.log"), stream: "stdout")

        var podCreated = false
        var utilityStarted = false
        var attachedDevice: VZUSBMassStorageDevice?
        var usbAttachSucceeded = false
        var usbDetachSucceeded = false
        var guestBaseline: [GuestBlockDevice] = []
        var discoveredDevice: String?
        var guestObservedNewDevice = false
        var guestCorrelatedDevice = false
        var guestMountSucceeded = false
        var markerVerified = false
        var guestUnmountSucceeded = false
        var guestDeviceGoneAfterDetach = false
        var errors: [String: String] = [:]
        var discoveryOutput = ""
        var detachOutput = ""

        do {
            try await pod.addContainer("utility", rootfs: utilityRootfs) { config in
                config.process.arguments = ["/bin/sh", "-c", "exec sleep 300"]
                config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
                config.process.workingDirectory = "/"
                config.process.stdout = utilityLog
                config.process.stderr = utilityErr
                config.useInit = true
            }

            try await pod.create()
            podCreated = true
            try await pod.startContainer("utility")
            utilityStarted = true

            let baseline = try await execCapture(
                pod,
                containerID: "utility",
                processID: "r1-baseline",
                script: blockDeviceListScript(),
                log: execLog
            )
            if baseline.exitCode == 0 {
                guestBaseline = parseBlockDevices(from: baseline.output)
            } else {
                errors["guestBaseline"] = "exit \(baseline.exitCode): \(baseline.output)"
            }

            do {
                probeState.mark("usbAttachAttempted")
                attachedDevice = try await attachUSBMassStorage(
                    instance: probeState.requireInstance(),
                    path: targetRootfs.source,
                    readOnly: true
                )
                usbAttachSucceeded = true
                probeState.mark("usbAttachSucceeded")
            } catch {
                errors["usbAttach"] = describe(error)
            }

            if usbAttachSucceeded {
                let discovery = try await execCapture(
                    pod,
                    containerID: "utility",
                    processID: "r1-discovery",
                    script: deviceDiscoveryScript(
                        baselineDeviceNames: guestBaseline.map(\.name),
                        expectedSectors: expectedSectors
                    ),
                    log: execLog
                )
                discoveryOutput = discovery.output
                let discoveryState = parseDiscoveryOutput(discovery.output)
                discoveredDevice = discoveryState.device
                guestObservedNewDevice = discoveryState.observedNewDevice
                guestCorrelatedDevice = discoveryState.correlatedDevice
                guestMountSucceeded = discoveryState.mountSucceeded
                markerVerified = discoveryState.markerVerified
                guestUnmountSucceeded = discoveryState.unmountSucceeded
                if discovery.exitCode != 0 {
                    errors["guestDiscovery"] = "exit \(discovery.exitCode): \(discovery.output)"
                }
            }

            if let attachedDevice {
                do {
                    try await detachUSBMassStorage(instance: probeState.requireInstance(), device: attachedDevice)
                    usbDetachSucceeded = true
                    probeState.mark("usbDetachSucceeded")
                } catch {
                    errors["usbDetach"] = describe(error)
                }
            }

            if usbDetachSucceeded, let discoveredDevice {
                let detachProbe = try await execCapture(
                    pod,
                    containerID: "utility",
                    processID: "r1-detach-probe",
                    script: deviceDetachScript(device: discoveredDevice),
                    log: execLog
                )
                detachOutput = detachProbe.output
                guestDeviceGoneAfterDetach = detachProbe.exitCode == 0
                if detachProbe.exitCode != 0 {
                    errors["guestDetachObserve"] = "exit \(detachProbe.exitCode): \(detachProbe.output)"
                }
            }

            if utilityStarted {
                try? await pod.stopContainer("utility")
            }
            try await pod.stop()
            try close(utilityLog, utilityErr, execLog)

            let snapshot = probeState.snapshot()
            let result = RuntimeDeviceDiscoverySummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                targetRootfs: targetRootfs.source,
                targetRootfsBytes: targetRootfsSize,
                expectedSectors: expectedSectors,
                usbControllerConfigured: snapshot.usbControllerConfigured,
                instanceCaptured: snapshot.instanceCaptured,
                usbAttachAttempted: snapshot.usbAttachAttempted,
                usbAttachSucceeded: usbAttachSucceeded,
                guestBaseline: guestBaseline,
                guestObservedNewDevice: guestObservedNewDevice,
                guestCorrelatedDevice: guestCorrelatedDevice,
                discoveredDevice: discoveredDevice,
                guestMountSucceeded: guestMountSucceeded,
                markerVerified: markerVerified,
                guestUnmountSucceeded: guestUnmountSucceeded,
                usbDetachSucceeded: usbDetachSucceeded,
                guestDeviceGoneAfterDetach: guestDeviceGoneAfterDetach,
                outcome: runtimeDeviceDiscoveryOutcome(
                    instanceCaptured: snapshot.instanceCaptured,
                    usbAttachSucceeded: usbAttachSucceeded,
                    guestObservedNewDevice: guestObservedNewDevice,
                    guestCorrelatedDevice: guestCorrelatedDevice,
                    guestMountSucceeded: guestMountSucceeded,
                    markerVerified: markerVerified,
                    guestUnmountSucceeded: guestUnmountSucceeded,
                    usbDetachSucceeded: usbDetachSucceeded,
                    guestDeviceGoneAfterDetach: guestDeviceGoneAfterDetach
                ),
                discoveryMethod: "baseline /sys/block snapshot, new /sys/block entry, exact sector count match, read-only ext4 mount, busybox rootfs marker",
                discoveryOutput: discoveryOutput,
                detachOutput: detachOutput,
                events: snapshot.events,
                errors: errors,
                logs: [
                    "utility": logsURL.appendingPathComponent("utility.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        } catch {
            if let attachedDevice {
                try? await detachUSBMassStorage(instance: probeState.requireInstance(), device: attachedDevice)
            }
            if utilityStarted {
                try? await pod.stopContainer("utility")
            }
            if podCreated {
                try? await pod.stop()
            }
            try? close(utilityLog, utilityErr, execLog)
            throw error
        }
    }

    private func runtimeDeviceDiscoveryOutcome(
        instanceCaptured: Bool,
        usbAttachSucceeded: Bool,
        guestObservedNewDevice: Bool,
        guestCorrelatedDevice: Bool,
        guestMountSucceeded: Bool,
        markerVerified: Bool,
        guestUnmountSucceeded: Bool,
        usbDetachSucceeded: Bool,
        guestDeviceGoneAfterDetach: Bool
    ) -> String {
        if !instanceCaptured {
            return "instanceNotCaptured"
        }
        if !usbAttachSucceeded {
            return "usbAttachFailed"
        }
        if !guestObservedNewDevice {
            return "guestCouldNotObserveNewDevice"
        }
        if !guestCorrelatedDevice {
            return "guestObservedButCouldNotCorrelate"
        }
        if !guestMountSucceeded {
            return "deviceCorrelatedButMountFailed"
        }
        if !markerVerified {
            return "mountSucceededMarkerVerificationFailed"
        }
        if !guestUnmountSucceeded {
            return "markerVerifiedButUnmountFailed"
        }
        if !usbDetachSucceeded {
            return "unmountSucceededButDetachFailed"
        }
        if !guestDeviceGoneAfterDetach {
            return "detachSucceededButGuestStillObservedDevice"
        }
        return "discoveryMountVerifyUnmountDetachSucceeded"
    }

    private func fileSize(at url: URL) throws -> Int64 {
        let values = try url.resourceValues(forKeys: [.fileSizeKey])
        guard let size = values.fileSize else {
            throw ValidationError("could not read file size for \(url.path)")
        }
        return Int64(size)
    }

    private func execCapture(
        _ pod: LinuxPod,
        containerID: String,
        processID: String,
        script: String,
        log: FileLogWriter
    ) async throws -> ExecCaptureResult {
        let buffer = BufferLogWriter()
        let exec = try await pod.execInContainer(containerID, processID: processID) { config in
            config.arguments = ["/bin/sh", "-c", script]
            config.stdout = TeeLogWriter(writers: [buffer, log])
            config.stderr = log
        }
        try await exec.start()
        let status = try await exec.wait()
        try await exec.delete()
        return ExecCaptureResult(exitCode: status.exitCode, output: buffer.string())
    }

    private func blockDeviceListScript() -> String {
        """
        for path in /sys/block/*; do
          dev="${path##*/}"
          case "${dev}" in loop*|ram*|zram*) continue;; esac
          size="$(cat "${path}/size" 2>/dev/null || echo 0)"
          echo "${dev} ${size}"
        done
        """
    }

    private func deviceDiscoveryScript(baselineDeviceNames: [String], expectedSectors: Int64) -> String {
        let baseline = baselineDeviceNames.joined(separator: " ")
        return """
        baseline=\(shellSingleQuoted(baseline))
        expected=\(expectedSectors)
        mountpoint=/tmp/macvz-r1-rootfs
        errfile=/tmp/macvz-r1-mount.err
        rm -f "${errfile}"
        for i in $(seq 1 30); do
          for path in /sys/block/*; do
            dev="${path##*/}"
            case "${dev}" in loop*|ram*|zram*) continue;; esac
            case " ${baseline} " in *" ${dev} "*) continue;; esac
            size="$(cat "${path}/size" 2>/dev/null || echo 0)"
            echo "observed=${dev} size=${size}"
            if [ "${size}" = "${expected}" ]; then
              echo "correlated=${dev} method=sysfs-new-device-and-size size=${size}"
              mkdir -p "${mountpoint}"
              if mount -o ro -t ext4 "/dev/${dev}" "${mountpoint}" 2>"${errfile}"; then
                echo "mounted=${dev}"
                if [ -x "${mountpoint}/bin/busybox" ] || [ -e "${mountpoint}/bin/sh" ]; then
                  echo "marker=busybox-rootfs"
                  marker_ok=1
                else
                  echo "marker_missing=${dev}"
                  marker_ok=0
                fi
                if umount "${mountpoint}"; then
                  echo "unmounted=${dev}"
                else
                  echo "unmount_failed=${dev}"
                  exit 15
                fi
                if [ "${marker_ok}" = "1" ]; then
                  exit 0
                fi
                exit 14
              fi
              echo "mount_failed=${dev} error=$(cat "${errfile}" 2>/dev/null)"
              exit 13
            fi
            echo "uncorrelated=${dev} expected=${expected} actual=${size}"
          done
          sleep 1
        done
        echo "diagnostic_usb_devices=$(ls /sys/bus/usb/devices 2>/dev/null | tr '\\n' ' ' || true)"
        echo "diagnostic_scsi_disks=$(ls /sys/class/scsi_disk 2>/dev/null | tr '\\n' ' ' || true)"
        echo "diagnostic_block_devices=$(ls /sys/block 2>/dev/null | tr '\\n' ' ' || true)"
        echo "no_new_device"
        exit 11
        """
    }

    private func deviceDetachScript(device: String) -> String {
        """
        dev=\(shellSingleQuoted(device))
        for i in $(seq 1 15); do
          if [ ! -e "/sys/block/${dev}" ]; then
            echo "detached=${dev}"
            exit 0
          fi
          sleep 1
        done
        echo "still_present=${dev}"
        exit 21
        """
    }

    private func shellSingleQuoted(_ value: String) -> String {
        "'\(value.replacingOccurrences(of: "'", with: "'\"'\"'"))'"
    }

    private func parseBlockDevices(from output: String) -> [GuestBlockDevice] {
        output.split(whereSeparator: \.isNewline).compactMap { line in
            let parts = line.split(separator: " ", maxSplits: 1).map(String.init)
            guard parts.count == 2, let sectors = Int64(parts[1]) else {
                return nil
            }
            return GuestBlockDevice(name: parts[0], sectors: sectors)
        }
    }

    private func parseDiscoveryOutput(_ output: String) -> DiscoveryOutputState {
        var state = DiscoveryOutputState()
        for line in output.split(whereSeparator: \.isNewline).map(String.init) {
            if line.hasPrefix("observed=") {
                state.observedNewDevice = true
            }
            if line.hasPrefix("correlated=") {
                state.correlatedDevice = true
                state.device = line
                    .dropFirst("correlated=".count)
                    .split(separator: " ", maxSplits: 1)
                    .first
                    .map(String.init)
            }
            if line.hasPrefix("mounted=") {
                state.mountSucceeded = true
            }
            if line.hasPrefix("marker=busybox-rootfs") {
                state.markerVerified = true
            }
            if line.hasPrefix("unmounted=") {
                state.unmountSucceeded = true
            }
        }
        return state
    }

    private func attachUSBMassStorage(
        instance: VZVirtualMachineInstance,
        path: String,
        readOnly: Bool
    ) async throws -> VZUSBMassStorageDevice {
        guard let controller = instance.vzVirtualMachine.usbControllers.first else {
            throw HotplugProbeFailure(description: "no VZ USB controller is available on the running VM")
        }
        let attachment = try VZDiskImageStorageDeviceAttachment(
            url: URL(fileURLWithPath: path),
            readOnly: readOnly,
            cachingMode: .cached,
            synchronizationMode: .fsync
        )
        let configuration = VZUSBMassStorageDeviceConfiguration(attachment: attachment)
        let device = VZUSBMassStorageDevice(configuration: configuration)
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
        return device
    }

    private func detachUSBMassStorage(instance: VZVirtualMachineInstance, device: VZUSBMassStorageDevice) async throws {
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

    private func runNBDRootfsIdentityProbe(
        imageStore: ImageStore,
        vmm: VZVirtualMachineManager,
        logger: Logger,
        startedAt: Date,
        logsURL: URL,
        rootfsURL: URL
    ) async throws {
        let podID = "macvz-r3-\(Int(startedAt.timeIntervalSince1970))"
        let pod = try LinuxPod(podID, vmm: vmm, logger: logger) { config in
            config.cpus = 2
            config.memoryInBytes = 1024 * 1024 * 1024
            config.hostname = "macvz-runtime-r3"
            config.bootLog = .file(path: logsURL.appendingPathComponent("boot.log"))
        }

        let baseImage = try await imageStore.get(reference: image, pull: true)
        let alphaDisk = rootfsURL.appendingPathComponent("alpha.ext4")
        let betaDisk = rootfsURL.appendingPathComponent("beta.ext4")
        let alphaRootfs = try await unpackRootfs(baseImage, at: alphaDisk)
        let betaRootfs = try await unpackRootfs(baseImage, at: betaDisk)

        let alphaLog = try FileLogWriter(path: logsURL.appendingPathComponent("alpha.log"), stream: "stdout")
        let alphaErr = try FileLogWriter(path: logsURL.appendingPathComponent("alpha.log"), stream: "stderr")
        let betaLog = try FileLogWriter(path: logsURL.appendingPathComponent("beta.log"), stream: "stdout")
        let betaErr = try FileLogWriter(path: logsURL.appendingPathComponent("beta.log"), stream: "stderr")
        let execLog = try FileLogWriter(path: logsURL.appendingPathComponent("exec.log"), stream: "stdout")

        var podCreated = false
        var alphaStarted = false
        var betaStarted = false
        var nbdServersStarted = false
        var containerStartSucceeded = false
        var rootfsMarkersVerified = false
        var mountEvidenceVerified = false
        var errors: [String: String] = [:]
        var alphaOutput = ""
        var betaOutput = ""
        var alphaMarkerHost: String?
        var betaMarkerHost: String?

        let alphaServer: MiniNBDServer
        let betaServer: MiniNBDServer
        do {
            alphaServer = try MiniNBDServer(
                filePath: alphaRootfs.source,
                socketPath: rootfsURL.appendingPathComponent("alpha.sock").path,
                logger: logger
            )
            betaServer = try MiniNBDServer(
                filePath: betaRootfs.source,
                socketPath: rootfsURL.appendingPathComponent("beta.sock").path,
                logger: logger
            )
            nbdServersStarted = true
        } catch {
            try? close(alphaLog, alphaErr, betaLog, betaErr, execLog)
            let result = NBDRootfsIdentitySummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                nbdServersStarted: false,
                podCreated: false,
                containerStartSucceeded: false,
                rootfsMarkersVerified: false,
                mountEvidenceVerified: false,
                alphaNBDURL: nil,
                betaNBDURL: nil,
                alphaOutput: "",
                betaOutput: "",
                alphaMarkerHost: nil,
                betaMarkerHost: nil,
                outcome: "nbdServerFailed",
                note: "pre-create NBD rootfs identity does not solve post-create CreateContainer ordering",
                errors: ["nbdServer": describe(error)],
                logs: [
                    "alpha": logsURL.appendingPathComponent("alpha.log").path,
                    "beta": logsURL.appendingPathComponent("beta.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
            return
        }
        defer {
            alphaServer.stop()
            betaServer.stop()
        }

        do {
            try await pod.addContainer("alpha", rootfs: nbdRootfs(from: alphaRootfs, nbdURL: alphaServer.url)) { config in
                config.process.arguments = [
                    "/bin/sh",
                    "-c",
                    """
                    set -eu
                    printf 'alpha-rootfs\\n' > /macvz-r3-identity
                    sync
                    grep ' / ' /proc/mounts
                    cat /macvz-r3-identity
                    """,
                ]
                config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
                config.process.workingDirectory = "/"
                config.process.stdout = alphaLog
                config.process.stderr = alphaErr
                config.useInit = true
            }

            try await pod.addContainer("beta", rootfs: nbdRootfs(from: betaRootfs, nbdURL: betaServer.url)) { config in
                config.process.arguments = [
                    "/bin/sh",
                    "-c",
                    """
                    set -eu
                    printf 'beta-rootfs\\n' > /macvz-r3-identity
                    sync
                    grep ' / ' /proc/mounts
                    cat /macvz-r3-identity
                    """,
                ]
                config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
                config.process.workingDirectory = "/"
                config.process.stdout = betaLog
                config.process.stderr = betaErr
                config.useInit = true
            }

            do {
                try await pod.create()
                podCreated = true
            } catch {
                errors["podCreate"] = describe(error)
                throw error
            }

            do {
                try await pod.startContainer("alpha")
                alphaStarted = true
                try await pod.startContainer("beta")
                betaStarted = true

                let alphaStatus = try await pod.waitContainer("alpha")
                let betaStatus = try await pod.waitContainer("beta")
                guard alphaStatus.exitCode == 0 else {
                    errors["alphaStart"] = "exit \(alphaStatus.exitCode)"
                    throw ValidationError("alpha exited with \(alphaStatus.exitCode)")
                }
                guard betaStatus.exitCode == 0 else {
                    errors["betaStart"] = "exit \(betaStatus.exitCode)"
                    throw ValidationError("beta exited with \(betaStatus.exitCode)")
                }
                containerStartSucceeded = true
            } catch {
                if errors["alphaStart"] == nil && errors["betaStart"] == nil {
                    errors["containerStart"] = describe(error)
                }
                throw error
            }

            alphaOutput = try readTextFile(logsURL.appendingPathComponent("alpha.log"))
            betaOutput = try readTextFile(logsURL.appendingPathComponent("beta.log"))
            mountEvidenceVerified = alphaOutput.contains("/dev/vd")
                && betaOutput.contains("/dev/vd")
                && alphaOutput.contains("alpha-rootfs")
                && betaOutput.contains("beta-rootfs")

            alphaMarkerHost = try readExt4File(alphaDisk, path: "/macvz-r3-identity")
            betaMarkerHost = try readExt4File(betaDisk, path: "/macvz-r3-identity")
            rootfsMarkersVerified = alphaMarkerHost == "alpha-rootfs" && betaMarkerHost == "beta-rootfs"

            try? await pod.stopContainer("alpha")
            try? await pod.stopContainer("beta")
            try await pod.stop()
            try close(alphaLog, alphaErr, betaLog, betaErr, execLog)

            let result = NBDRootfsIdentitySummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                nbdServersStarted: nbdServersStarted,
                podCreated: podCreated,
                containerStartSucceeded: containerStartSucceeded,
                rootfsMarkersVerified: rootfsMarkersVerified,
                mountEvidenceVerified: mountEvidenceVerified,
                alphaNBDURL: alphaServer.url,
                betaNBDURL: betaServer.url,
                alphaOutput: alphaOutput,
                betaOutput: betaOutput,
                alphaMarkerHost: alphaMarkerHost,
                betaMarkerHost: betaMarkerHost,
                outcome: nbdRootfsOutcome(
                    nbdServersStarted: nbdServersStarted,
                    podCreated: podCreated,
                    containerStartSucceeded: containerStartSucceeded,
                    mountEvidenceVerified: mountEvidenceVerified,
                    rootfsMarkersVerified: rootfsMarkersVerified
                ),
                note: "pre-create NBD rootfs identity does not solve post-create CreateContainer ordering",
                errors: errors,
                logs: [
                    "alpha": logsURL.appendingPathComponent("alpha.log").path,
                    "beta": logsURL.appendingPathComponent("beta.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        } catch {
            if alphaStarted {
                try? await pod.stopContainer("alpha")
            }
            if betaStarted {
                try? await pod.stopContainer("beta")
            }
            if podCreated {
                try? await pod.stop()
            }
            try? close(alphaLog, alphaErr, betaLog, betaErr, execLog)

            let result = NBDRootfsIdentitySummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                nbdServersStarted: nbdServersStarted,
                podCreated: podCreated,
                containerStartSucceeded: containerStartSucceeded,
                rootfsMarkersVerified: rootfsMarkersVerified,
                mountEvidenceVerified: mountEvidenceVerified,
                alphaNBDURL: alphaServer.url,
                betaNBDURL: betaServer.url,
                alphaOutput: alphaOutput,
                betaOutput: betaOutput,
                alphaMarkerHost: alphaMarkerHost,
                betaMarkerHost: betaMarkerHost,
                outcome: nbdRootfsOutcome(
                    nbdServersStarted: nbdServersStarted,
                    podCreated: podCreated,
                    containerStartSucceeded: containerStartSucceeded,
                    mountEvidenceVerified: mountEvidenceVerified,
                    rootfsMarkersVerified: rootfsMarkersVerified
                ),
                note: "pre-create NBD rootfs identity does not solve post-create CreateContainer ordering",
                errors: errors.merging(["probe": describe(error)]) { current, _ in current },
                logs: [
                    "alpha": logsURL.appendingPathComponent("alpha.log").path,
                    "beta": logsURL.appendingPathComponent("beta.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        }
    }

    private func nbdRootfs(from rootfs: Containerization.Mount, nbdURL: String) -> Containerization.Mount {
        .block(
            format: rootfs.type,
            source: nbdURL,
            destination: rootfs.destination,
            options: rootfs.options
        )
    }

    private func nbdRootfsOutcome(
        nbdServersStarted: Bool,
        podCreated: Bool,
        containerStartSucceeded: Bool,
        mountEvidenceVerified: Bool,
        rootfsMarkersVerified: Bool
    ) -> String {
        if !nbdServersStarted {
            return "nbdServerFailed"
        }
        if !podCreated {
            return "vzNbdAttachmentOrGuestRootfsMountFailed"
        }
        if !containerStartSucceeded {
            return "containerStartFailed"
        }
        if !mountEvidenceVerified {
            return "rootfsMountEvidenceMismatch"
        }
        if !rootfsMarkersVerified {
            return "rootfsIdentityMismatch"
        }
        return "nbdRootfsPrecreateSucceeded"
    }

    private func runGuestRootfsStagingProbe(
        imageStore: ImageStore,
        vmm: VZVirtualMachineManager,
        logger: Logger,
        startedAt: Date,
        logsURL: URL,
        rootfsURL: URL
    ) async throws {
        let podID = "macvz-r4-\(Int(startedAt.timeIntervalSince1970))"
        let pod = try LinuxPod(podID, vmm: vmm, logger: logger) { config in
            config.cpus = 2
            config.memoryInBytes = 1024 * 1024 * 1024
            config.hostname = "macvz-runtime-r4"
            config.bootLog = .file(path: logsURL.appendingPathComponent("boot.log"))
        }

        let baseImage = try await imageStore.get(reference: image, pull: true)
        let utilityRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("utility.ext4"))
        let utilityLog = try FileLogWriter(path: logsURL.appendingPathComponent("utility.log"), stream: "stdout")
        let utilityErr = try FileLogWriter(path: logsURL.appendingPathComponent("utility.log"), stream: "stderr")
        let execLog = try FileLogWriter(path: logsURL.appendingPathComponent("exec.log"), stream: "stdout")

        var podCreated = false
        var utilityStarted = false
        var transportAvailable = false
        var attempts: [GuestRootfsStagingAttemptSummary] = []
        var errors: [String: String] = [:]

        do {
            try await pod.addContainer("utility", rootfs: utilityRootfs) { config in
                config.process.arguments = ["/bin/sh", "-c", "exec sleep 300"]
                config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
                config.process.workingDirectory = "/"
                config.process.stdout = utilityLog
                config.process.stderr = utilityErr
                config.useInit = true
            }

            try await pod.create()
            podCreated = true
            try await pod.startContainer("utility")
            utilityStarted = true

            attempts = try await pod.withVirtualMachineInstance { vm in
                let agent = try await vm.dialAgent()
                var collectedAttempts: [GuestRootfsStagingAttemptSummary] = []
                for requestID in ["late-alpha", "late-beta"] {
                    let attempt = try await runGuestRootfsStagingAttempt(
                        pod: pod,
                        agent: agent,
                        requestID: requestID,
                        execLog: execLog
                    )
                    collectedAttempts.append(attempt)
                    if attempt.outcome != "guestSideStagingSucceeded" {
                        break
                    }
                }
                try? await agent.close()
                return collectedAttempts
            }
            transportAvailable = true

            if utilityStarted {
                try? await pod.stopContainer("utility")
            }
            try await pod.stop()
            try close(utilityLog, utilityErr, execLog)

            let result = GuestRootfsStagingSummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                podCreated: podCreated,
                utilityStarted: utilityStarted,
                transportAvailable: transportAvailable,
                attempts: attempts,
                outcome: guestRootfsStagingOutcome(
                    podCreated: podCreated,
                    utilityStarted: utilityStarted,
                    transportAvailable: transportAvailable,
                    attempts: attempts
                ),
                note: "guest-side staging avoids guessed guest block devices, but it is not yet a full late-container process creation path",
                errors: errors,
                logs: [
                    "utility": logsURL.appendingPathComponent("utility.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        } catch {
            if !transportAvailable {
                errors["transport"] = describe(error)
            } else {
                errors["probe"] = describe(error)
            }
            if utilityStarted {
                try? await pod.stopContainer("utility")
            }
            if podCreated {
                try? await pod.stop()
            }
            try? close(utilityLog, utilityErr, execLog)

            let result = GuestRootfsStagingSummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                podCreated: podCreated,
                utilityStarted: utilityStarted,
                transportAvailable: transportAvailable,
                attempts: attempts,
                outcome: guestRootfsStagingOutcome(
                    podCreated: podCreated,
                    utilityStarted: utilityStarted,
                    transportAvailable: transportAvailable,
                    attempts: attempts
                ),
                note: "guest-side staging avoids guessed guest block devices, but it is not yet a full late-container process creation path",
                errors: errors,
                logs: [
                    "utility": logsURL.appendingPathComponent("utility.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        }
    }

    private func runGuestRootfsStagingAttempt(
        pod: LinuxPod,
        agent: some VirtualMachineAgent,
        requestID: String,
        execLog: FileLogWriter
    ) async throws -> GuestRootfsStagingAttemptSummary {
        let escapedID = requestID.replacingOccurrences(of: "/", with: "_")
        let stageBase = "/run/macvz-r4/staged/\(escapedID)"
        let rootfsPath = "\(stageBase)/rootfs"
        let mountTarget = "/run/macvz-r4/mounts/\(escapedID)"
        let identityPath = "\(rootfsPath)/etc/macvz-r4-identity"
        let requestPath = "\(rootfsPath)/metadata/request-id"
        var stageSucceeded = false
        var mountSucceeded = false
        var identityVerified = false
        var cleanupSucceeded = false
        var verifyOutput = ""
        var cleanupOutput = ""
        var errors: [String: String] = [:]

        do {
            try await agent.mkdir(path: rootfsPath, all: true, perms: 0o755)
            try await agent.mkdir(path: mountTarget, all: true, perms: 0o755)
            let stage = try await execCapture(
                pod,
                containerID: "utility",
                processID: "r4-stage-\(escapedID)",
                script: guestRootfsStageScript(
                    requestID: requestID,
                    rootfsPath: rootfsPath,
                    identityPath: identityPath,
                    requestPath: requestPath
                ),
                log: execLog
            )
            if stage.exitCode != 0 {
                throw ValidationError("stage command exited with \(stage.exitCode): \(stage.output)")
            }
            try await agent.sync()
            stageSucceeded = true
        } catch {
            errors["stage"] = describe(error)
        }

        if stageSucceeded {
            do {
                try await agent.mount(ContainerizationOCI.Mount(
                    type: "none",
                    source: rootfsPath,
                    destination: mountTarget,
                    options: ["bind"]
                ))
                mountSucceeded = true
            } catch {
                errors["mount"] = describe(error)
            }
        }

        if mountSucceeded {
            let verify = try await execCapture(
                pod,
                containerID: "utility",
                processID: "r4-verify-\(escapedID)",
                script: guestRootfsVerifyScript(
                    requestID: requestID,
                    mountTarget: mountTarget,
                    identityPath: identityPath
                ),
                log: execLog
            )
            verifyOutput = verify.output
            identityVerified = verify.exitCode == 0
            if verify.exitCode != 0 {
                errors["identity"] = "exit \(verify.exitCode): \(verify.output)"
            }
        }

        if mountSucceeded {
            do {
                try await agent.umount(path: mountTarget, flags: 0)
            } catch {
                errors["umount"] = describe(error)
            }
        }

        let cleanup = try await execCapture(
            pod,
            containerID: "utility",
            processID: "r4-cleanup-\(escapedID)",
            script: guestRootfsCleanupScript(stageBase: stageBase, mountTarget: mountTarget),
            log: execLog
        )
        cleanupOutput = cleanup.output
        cleanupSucceeded = cleanup.exitCode == 0
        if cleanup.exitCode != 0 {
            errors["cleanup"] = "exit \(cleanup.exitCode): \(cleanup.output)"
        }

        return GuestRootfsStagingAttemptSummary(
            requestID: requestID,
            stagePath: rootfsPath,
            mountTarget: mountTarget,
            stageSucceeded: stageSucceeded,
            mountSucceeded: mountSucceeded,
            identityVerified: identityVerified,
            cleanupSucceeded: cleanupSucceeded,
            verifyOutput: verifyOutput,
            cleanupOutput: cleanupOutput,
            outcome: guestRootfsStagingAttemptOutcome(
                stageSucceeded: stageSucceeded,
                mountSucceeded: mountSucceeded,
                identityVerified: identityVerified,
                cleanupSucceeded: cleanupSucceeded
            ),
            errors: errors
        )
    }

    private func guestRootfsVerifyScript(requestID: String, mountTarget: String, identityPath: String) -> String {
        """
        set -u
        expected=\(shellSingleQuoted("macvz-r4-id=\(requestID)"))
        mounted_identity=\(shellSingleQuoted("\(mountTarget)/etc/macvz-r4-identity"))
        direct_identity=\(shellSingleQuoted(identityPath))
        mount_target=\(shellSingleQuoted(mountTarget))
        direct_value="$(cat "${direct_identity}" 2>/dev/null)"
        direct_status=$?
        mounted_value="$(cat "${mounted_identity}" 2>/dev/null)"
        mounted_status=$?
        mount_line="$(grep " ${mount_target} " /proc/mounts 2>/dev/null)"
        mount_status=$?
        echo "direct_status=${direct_status}"
        echo "direct_identity=${direct_value}"
        echo "mounted_status=${mounted_status}"
        echo "mounted_identity=${mounted_value}"
        echo "mount_status=${mount_status}"
        echo "mount_line=${mount_line}"
        echo "mount_target_listing=$(ls -la "${mount_target}" 2>/dev/null | tr '\\n' ';' || true)"
        if [ "${direct_status}" != "0" ] || [ "${direct_value}" != "${expected}" ]; then
          exit 41
        fi
        if [ "${mounted_status}" != "0" ] || [ "${mounted_value}" != "${expected}" ]; then
          exit 42
        fi
        if [ "${mount_status}" != "0" ]; then
          exit 43
        fi
        """
    }

    private func guestRootfsStageScript(
        requestID: String,
        rootfsPath: String,
        identityPath: String,
        requestPath: String
    ) -> String {
        """
        set -eu
        rootfs_path=\(shellSingleQuoted(rootfsPath))
        identity_path=\(shellSingleQuoted(identityPath))
        request_path=\(shellSingleQuoted(requestPath))
        request_id=\(shellSingleQuoted(requestID))
        mkdir -p "${rootfs_path}/etc" "${rootfs_path}/metadata"
        printf 'macvz-r4-id=%s\\n' "${request_id}" > "${identity_path}"
        printf '%s\\n' "${request_id}" > "${request_path}"
        sync
        test -f "${identity_path}"
        test -f "${request_path}"
        echo "stage_ok request=${request_id} rootfs=${rootfs_path}"
        """
    }

    private func guestRootfsCleanupScript(stageBase: String, mountTarget: String) -> String {
        """
        set -eu
        stage_base=\(shellSingleQuoted(stageBase))
        mount_target=\(shellSingleQuoted(mountTarget))
        rm -rf "${stage_base}" "${mount_target}"
        if [ -e "${stage_base}" ] || [ -e "${mount_target}" ]; then
          echo "cleanup_failed stage=${stage_base} target=${mount_target}"
          exit 31
        fi
        echo "cleanup_ok stage=${stage_base} target=${mount_target}"
        """
    }

    private func guestRootfsStagingAttemptOutcome(
        stageSucceeded: Bool,
        mountSucceeded: Bool,
        identityVerified: Bool,
        cleanupSucceeded: Bool
    ) -> String {
        if !stageSucceeded {
            return "rootfsCopyUnpackFailed"
        }
        if !mountSucceeded {
            return "mountBindFailed"
        }
        if !identityVerified {
            return "stagedRootfsIdentityMismatch"
        }
        if !cleanupSucceeded {
            return "cleanupFailed"
        }
        return "guestSideStagingSucceeded"
    }

    private func guestRootfsStagingOutcome(
        podCreated: Bool,
        utilityStarted: Bool,
        transportAvailable: Bool,
        attempts: [GuestRootfsStagingAttemptSummary]
    ) -> String {
        if !podCreated || !utilityStarted || !transportAvailable {
            return "guestStagingTransportUnavailable"
        }
        guard let first = attempts.first else {
            return "guestStagingTransportUnavailable"
        }
        if first.outcome != "guestSideStagingSucceeded" {
            return first.outcome
        }
        guard attempts.count > 1 else {
            return "guestSideStagingSucceeded"
        }
        return attempts.allSatisfy { $0.outcome == "guestSideStagingSucceeded" }
            ? "guestSideStagingSucceeded"
            : (attempts.first { $0.outcome != "guestSideStagingSucceeded" }?.outcome ?? "guestSideStagingSucceeded")
    }

    private func runStagedRootfsProcessProbe(
        imageStore: ImageStore,
        vmm: VZVirtualMachineManager,
        logger: Logger,
        startedAt: Date,
        logsURL: URL,
        rootfsURL: URL
    ) async throws {
        let podID = "macvz-r5-\(Int(startedAt.timeIntervalSince1970))"
        let pod = try LinuxPod(podID, vmm: vmm, logger: logger) { config in
            config.cpus = 2
            config.memoryInBytes = 1024 * 1024 * 1024
            config.hostname = "macvz-runtime-r5"
            config.bootLog = .file(path: logsURL.appendingPathComponent("boot.log"))
        }

        let baseImage = try await imageStore.get(reference: image, pull: true)
        let utilityRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("utility.ext4"))
        let utilityLog = try FileLogWriter(path: logsURL.appendingPathComponent("utility.log"), stream: "stdout")
        let utilityErr = try FileLogWriter(path: logsURL.appendingPathComponent("utility.log"), stream: "stderr")
        let execLog = try FileLogWriter(path: logsURL.appendingPathComponent("exec.log"), stream: "stdout")

        var podCreated = false
        var utilityStarted = false
        var transportAvailable = false
        var attempt: StagedRootfsProcessAttemptSummary?
        var errors: [String: String] = [:]

        do {
            try await pod.addContainer("utility", rootfs: utilityRootfs) { config in
                config.process.arguments = ["/bin/sh", "-c", "exec sleep 300"]
                config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
                config.process.workingDirectory = "/"
                config.process.stdout = utilityLog
                config.process.stderr = utilityErr
                config.useInit = true
            }

            try await pod.create()
            podCreated = true
            try await pod.startContainer("utility")
            utilityStarted = true

            attempt = try await pod.withVirtualMachineInstance { vm in
                let agent = try await vm.dialAgent()
                let result = try await runStagedRootfsProcessAttempt(
                    pod: pod,
                    agent: agent,
                    podID: podID,
                    requestID: "late-alpha",
                    execLog: execLog
                )
                try? await agent.close()
                return result
            }
            transportAvailable = true

            if utilityStarted {
                try? await pod.stopContainer("utility")
            }
            try await pod.stop()
            try close(utilityLog, utilityErr, execLog)

            let result = StagedRootfsProcessSummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                podCreated: podCreated,
                utilityStarted: utilityStarted,
                transportAvailable: transportAvailable,
                attempt: attempt,
                outcome: stagedRootfsProcessOutcome(
                    podCreated: podCreated,
                    utilityStarted: utilityStarted,
                    transportAvailable: transportAvailable,
                    attempt: attempt
                ),
                note: "R5 tests VM-agent process execution from a post-create staged rootfs; it does not implement the production CRI image pipeline",
                errors: errors,
                logs: [
                    "utility": logsURL.appendingPathComponent("utility.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        } catch {
            if !transportAvailable {
                errors["transport"] = describe(error)
            } else {
                errors["probe"] = describe(error)
            }
            if utilityStarted {
                try? await pod.stopContainer("utility")
            }
            if podCreated {
                try? await pod.stop()
            }
            try? close(utilityLog, utilityErr, execLog)

            let result = StagedRootfsProcessSummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                podCreated: podCreated,
                utilityStarted: utilityStarted,
                transportAvailable: transportAvailable,
                attempt: attempt,
                outcome: stagedRootfsProcessOutcome(
                    podCreated: podCreated,
                    utilityStarted: utilityStarted,
                    transportAvailable: transportAvailable,
                    attempt: attempt
                ),
                note: "R5 tests VM-agent process execution from a post-create staged rootfs; it does not implement the production CRI image pipeline",
                errors: errors,
                logs: [
                    "utility": logsURL.appendingPathComponent("utility.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        }
    }

    private func runStagedRootfsProcessAttempt(
        pod: LinuxPod,
        agent: some VirtualMachineAgent,
        podID: String,
        requestID: String,
        execLog: FileLogWriter
    ) async throws -> StagedRootfsProcessAttemptSummary {
        let escapedID = requestID.replacingOccurrences(of: "/", with: "_")
        let processID = "r5-process-\(escapedID)"
        let stageBase = "/run/macvz-r5/staged/\(escapedID)"
        let rootfsPath = "\(stageBase)/rootfs"
        let identityPath = "\(rootfsPath)/etc/macvz-r5-identity"
        let resultPath = "\(rootfsPath)/macvz-r5-result"
        var stageSucceeded = false
        var processCreateSucceeded = false
        var processStartSucceeded = false
        var processExitCode: Int32?
        var resultVerified = false
        var namespaceVerified = false
        var cleanupSucceeded = false
        var stageOutput = ""
        var verifyOutput = ""
        var cleanupOutput = ""
        var errors: [String: String] = [:]
        var processContainerID: String?

        do {
            let stage = try await execCapture(
                pod,
                containerID: "utility",
                processID: "r5-stage-\(escapedID)",
                script: stagedProcessRootfsStageScript(
                    requestID: requestID,
                    rootfsPath: rootfsPath,
                    identityPath: identityPath,
                    markerPrefix: "macvz-r5-id"
                ),
                log: execLog
            )
            stageOutput = stage.output
            if stage.exitCode != 0 {
                throw ValidationError("stage command exited with \(stage.exitCode): \(stage.output)")
            }
            try await agent.sync()
            stageSucceeded = true
        } catch {
            errors["stage"] = describe(error)
        }

        if stageSucceeded {
            let spec = stagedRootfsProcessSpec(
                podID: podID,
                requestID: requestID,
                rootfsPath: rootfsPath,
                markerPrefix: "macvz-r5-id",
                identityFileName: "macvz-r5-identity",
                resultFileName: "macvz-r5-result",
                hostnamePrefix: "macvz-r5"
            )
            do {
                try await agent.createProcess(
                    id: processID,
                    containerID: nil,
                    stdinPort: nil,
                    stdoutPort: nil,
                    stderrPort: nil,
                    ociRuntimePath: nil,
                    configuration: spec,
                    options: nil
                )
                processCreateSucceeded = true
            } catch {
                let rootCreateError = describe(error)
                errors["createProcessRoot"] = rootCreateError
                if rootCreateError.contains("processes in the root of the vm not implemented") {
                    do {
                        try await agent.createProcess(
                            id: processID,
                            containerID: "utility",
                            stdinPort: nil,
                            stdoutPort: nil,
                            stderrPort: nil,
                            ociRuntimePath: nil,
                            configuration: spec,
                            options: nil
                        )
                        processContainerID = "utility"
                        processCreateSucceeded = true
                    } catch {
                        errors["createProcessUtility"] = describe(error)
                    }
                }
            }
        }

        if processCreateSucceeded {
            do {
                _ = try await agent.startProcess(id: processID, containerID: processContainerID)
                processStartSucceeded = true
                let status = try await agent.waitProcess(id: processID, containerID: processContainerID, timeoutInSeconds: 10)
                processExitCode = status.exitCode
                if status.exitCode != 0 {
                    errors["processExit"] = "exit \(status.exitCode)"
                }
            } catch {
                errors["startOrWaitProcess"] = describe(error)
            }
        }

        if processCreateSucceeded {
            do {
                try await agent.deleteProcess(id: processID, containerID: processContainerID)
            } catch {
                errors["deleteProcess"] = describe(error)
            }
        }

        if stageSucceeded {
            let verify = try await execCapture(
                pod,
                containerID: "utility",
                processID: "r5-verify-\(escapedID)",
                script: stagedProcessRootfsVerifyScript(
                    requestID: requestID,
                    resultPath: resultPath,
                    markerPrefix: "macvz-r5-id",
                    rootfsPrefix: "/run/macvz-r5/staged"
                ),
                log: execLog
            )
            verifyOutput = verify.output
            let state = parseStagedProcessVerifyOutput(
                verify.output,
                markerPrefix: "macvz-r5-id",
                rootfsPrefix: "/run/macvz-r5/staged"
            )
            resultVerified = verify.exitCode == 0 && state.identityMatched
            namespaceVerified = state.rootfsMatched
            if verify.exitCode != 0 {
                errors["verify"] = "exit \(verify.exitCode): \(verify.output)"
            }
        }

        let cleanup = try await execCapture(
            pod,
            containerID: "utility",
            processID: "r5-cleanup-\(escapedID)",
            script: stagedProcessRootfsCleanupScript(stageBase: stageBase),
            log: execLog
        )
        cleanupOutput = cleanup.output
        cleanupSucceeded = cleanup.exitCode == 0
        if cleanup.exitCode != 0 {
            errors["cleanup"] = "exit \(cleanup.exitCode): \(cleanup.output)"
        }

        return StagedRootfsProcessAttemptSummary(
            requestID: requestID,
            processID: processID,
            processContainerID: processContainerID,
            stagePath: rootfsPath,
            resultPath: resultPath,
            stageSucceeded: stageSucceeded,
            processCreateSucceeded: processCreateSucceeded,
            processStartSucceeded: processStartSucceeded,
            processExitCode: processExitCode,
            resultVerified: resultVerified,
            namespaceVerified: namespaceVerified,
            cleanupSucceeded: cleanupSucceeded,
            stageOutput: stageOutput,
            verifyOutput: verifyOutput,
            cleanupOutput: cleanupOutput,
            outcome: stagedRootfsProcessAttemptOutcome(
                stageSucceeded: stageSucceeded,
                processCreateSucceeded: processCreateSucceeded,
                processStartSucceeded: processStartSucceeded,
                processExitCode: processExitCode,
                resultVerified: resultVerified,
                namespaceVerified: namespaceVerified,
                cleanupSucceeded: cleanupSucceeded
            ),
            errors: errors
        )
    }

    private func stagedRootfsProcessSpec(
        podID: String,
        requestID: String,
        rootfsPath: String,
        markerPrefix: String,
        identityFileName: String,
        resultFileName: String,
        evidenceSourcePath: String? = nil,
        evidenceDestinationPath: String? = nil,
        hostnamePrefix: String
    ) -> ContainerizationOCI.Spec {
        var mounts = [
            ContainerizationOCI.Mount(type: "proc", source: "proc", destination: "/proc"),
            ContainerizationOCI.Mount(type: "tmpfs", source: "tmpfs", destination: "/dev", options: ["nosuid", "mode=755", "size=65536k"]),
            ContainerizationOCI.Mount(type: "devpts", source: "devpts", destination: "/dev/pts", options: ["nosuid", "noexec", "newinstance", "gid=5", "mode=0620", "ptmxmode=0666"]),
            ContainerizationOCI.Mount(type: "sysfs", source: "sysfs", destination: "/sys", options: ["nosuid", "noexec", "nodev"]),
            ContainerizationOCI.Mount(type: "tmpfs", source: "tmpfs", destination: "/dev/shm", options: ["nosuid", "noexec", "nodev", "mode=1777", "size=65536k"]),
        ]
        if let evidenceSourcePath, let evidenceDestinationPath {
            mounts.append(
                ContainerizationOCI.Mount(
                    type: "bind",
                    source: evidenceSourcePath,
                    destination: evidenceDestinationPath,
                    options: ["rbind", "rw"]
                )
            )
        }

        return ContainerizationOCI.Spec(
            version: "1.0.2",
            process: ContainerizationOCI.Process(
                args: [
                    "/bin/sh",
                    "-c",
                    """
                    set -eu
                    identity="$(cat /etc/\(identityFileName))"
                    {
                      echo "identity=${identity}"
                      echo "expected=\(markerPrefix)=\(requestID)"
                      echo "pwd=$(pwd)"
                      echo "proc_root=$(readlink /proc/self/root 2>/dev/null || true)"
                      echo "root_mount=$(grep ' / ' /proc/mounts 2>/dev/null || true)"
                      echo "root_listing=$(ls -1 / 2>/dev/null | tr '\\n' ',' || true)"
                    } > /\(resultFileName)
                    test "${identity}" = "\(markerPrefix)=\(requestID)"
                    if [ -d "\(evidenceDestinationPath ?? "/__macvz_no_handoff")" ]; then
                      cat /\(resultFileName) > "\(evidenceDestinationPath ?? "/__macvz_no_handoff")/\(resultFileName)"
                      sync
                    fi
                    """,
                ],
                cwd: "/",
                env: ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
            ),
            hostname: "\(hostnamePrefix)-\(requestID)",
            mounts: mounts,
            root: ContainerizationOCI.Root(path: rootfsPath, readonly: false),
            linux: ContainerizationOCI.Linux(
                resources: ContainerizationOCI.LinuxResources(),
                cgroupsPath: "/container/pod/\(podID)/\(requestID)",
                namespaces: [
                    ContainerizationOCI.LinuxNamespace(type: .cgroup),
                    ContainerizationOCI.LinuxNamespace(type: .ipc),
                    ContainerizationOCI.LinuxNamespace(type: .mount),
                    ContainerizationOCI.LinuxNamespace(type: .pid),
                    ContainerizationOCI.LinuxNamespace(type: .uts),
                ]
            )
        )
    }

    private func stagedProcessRootfsStageScript(
        requestID: String,
        rootfsPath: String,
        identityPath: String,
        markerPrefix: String
    ) -> String {
        """
        set -eu
        rootfs_path=\(shellSingleQuoted(rootfsPath))
        identity_path=\(shellSingleQuoted(identityPath))
        request_id=\(shellSingleQuoted(requestID))
        rm -rf "${rootfs_path}"
        mkdir -p "${rootfs_path}"
        for path in /bin /etc /home /lib /lib64 /root /sbin /usr /var; do
          if [ -e "${path}" ]; then
            cp -a "${path}" "${rootfs_path}/"
          fi
        done
        mkdir -p "${rootfs_path}/proc" "${rootfs_path}/sys" "${rootfs_path}/dev" "${rootfs_path}/tmp" "${rootfs_path}/run"
        printf '\(markerPrefix)=%s\\n' "${request_id}" > "${identity_path}"
        chmod 1777 "${rootfs_path}/tmp"
        sync
        test -x "${rootfs_path}/bin/sh"
        test -f "${identity_path}"
        echo "stage_ok request=${request_id} rootfs=${rootfs_path}"
        """
    }

    private func stagedProcessRootfsVerifyScript(
        requestID: String,
        resultPath: String,
        markerPrefix: String,
        rootfsPrefix: String
    ) -> String {
        """
        set -u
        expected=\(shellSingleQuoted("\(markerPrefix)=\(requestID)"))
        result_path=\(shellSingleQuoted(resultPath))
        if [ ! -f "${result_path}" ]; then
          echo "result_missing=${result_path}"
          exit 51
        fi
        cat "${result_path}"
        if ! grep -q "^identity=${expected}$" "${result_path}"; then
          exit 52
        fi
        if ! grep -q "^proc_root=\(rootfsPrefix)/\(requestID)/rootfs$" "${result_path}"; then
          exit 53
        fi
        echo "verify_ok request=\(requestID)"
        """
    }

    private func stagedProcessRootfsCleanupScript(stageBase: String) -> String {
        """
        set -eu
        stage_base=\(shellSingleQuoted(stageBase))
        rm -rf "${stage_base}"
        if [ -e "${stage_base}" ]; then
          echo "cleanup_failed stage=${stage_base}"
          exit 61
        fi
        echo "cleanup_ok stage=${stage_base}"
        """
    }

    private func parseStagedProcessVerifyOutput(
        _ output: String,
        markerPrefix: String,
        rootfsPrefix: String
    ) -> StagedProcessVerifyState {
        var state = StagedProcessVerifyState()
        for line in output.split(whereSeparator: \.isNewline).map(String.init) {
            if line.hasPrefix("identity=\(markerPrefix)=") {
                state.identityMatched = true
            }
            if line.hasPrefix("proc_root=\(rootfsPrefix)/") {
                state.rootfsMatched = true
            }
        }
        return state
    }

    private func stagedRootfsProcessAttemptOutcome(
        stageSucceeded: Bool,
        processCreateSucceeded: Bool,
        processStartSucceeded: Bool,
        processExitCode: Int32?,
        resultVerified: Bool,
        namespaceVerified: Bool,
        cleanupSucceeded: Bool
    ) -> String {
        if !stageSucceeded {
            return "stagedRootfsUnavailableToAgentProcess"
        }
        if !processCreateSucceeded {
            return "processCreationFailed"
        }
        if !processStartSucceeded {
            return "processStartedButIdentityMismatch"
        }
        if processExitCode != 0 {
            return "processStartedButIdentityMismatch"
        }
        if !resultVerified {
            return "processStartedButIdentityMismatch"
        }
        if !namespaceVerified {
            return "processNamespaceRootfsMismatch"
        }
        if !cleanupSucceeded {
            return "cleanupFailed"
        }
        return "stagedRootfsProcessExecutionSucceeded"
    }

    private func stagedRootfsProcessOutcome(
        podCreated: Bool,
        utilityStarted: Bool,
        transportAvailable: Bool,
        attempt: StagedRootfsProcessAttemptSummary?
    ) -> String {
        if !podCreated || !utilityStarted || !transportAvailable {
            return "vmAgentProcessApiUnavailable"
        }
        return attempt?.outcome ?? "vmAgentProcessApiUnavailable"
    }

    private func runVminitdContainerRootfsProbe(
        imageStore: ImageStore,
        vmm: VZVirtualMachineManager,
        logger: Logger,
        startedAt: Date,
        logsURL: URL,
        rootfsURL: URL
    ) async throws {
        let podID = "macvz-r6-\(Int(startedAt.timeIntervalSince1970))"
        let pod = try LinuxPod(podID, vmm: vmm, logger: logger) { config in
            config.cpus = 2
            config.memoryInBytes = 1024 * 1024 * 1024
            config.hostname = "macvz-runtime-r6"
            config.bootLog = .file(path: logsURL.appendingPathComponent("boot.log"))
        }

        let baseImage = try await imageStore.get(reference: image, pull: true)
        let utilityRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("utility.ext4"))
        let utilityLog = try FileLogWriter(path: logsURL.appendingPathComponent("utility.log"), stream: "stdout")
        let utilityErr = try FileLogWriter(path: logsURL.appendingPathComponent("utility.log"), stream: "stderr")
        let execLog = try FileLogWriter(path: logsURL.appendingPathComponent("exec.log"), stream: "stdout")

        var podCreated = false
        var utilityStarted = false
        var transportAvailable = false
        var attempt: StagedRootfsProcessAttemptSummary?
        var errors: [String: String] = [:]

        do {
            try await pod.addContainer("utility", rootfs: utilityRootfs) { config in
                config.process.arguments = ["/bin/sh", "-c", "exec sleep 300"]
                config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
                config.process.workingDirectory = "/"
                config.process.stdout = utilityLog
                config.process.stderr = utilityErr
                config.useInit = true
            }

            try await pod.create()
            podCreated = true
            try await pod.startContainer("utility")
            utilityStarted = true

            attempt = try await pod.withVirtualMachineInstance { vm in
                let agent = try await vm.dialAgent()
                let result = try await runVminitdContainerRootfsAttempt(
                    pod: pod,
                    agent: agent,
                    podID: podID,
                    requestID: "late-alpha",
                    execLog: execLog
                )
                try? await agent.close()
                return result
            }
            transportAvailable = true

            if utilityStarted {
                try? await pod.stopContainer("utility")
            }
            try await pod.stop()
            try close(utilityLog, utilityErr, execLog)

            let result = StagedRootfsProcessSummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                podCreated: podCreated,
                utilityStarted: utilityStarted,
                transportAvailable: transportAvailable,
                attempt: attempt,
                outcome: vminitdContainerRootfsOutcome(
                    podCreated: podCreated,
                    utilityStarted: utilityStarted,
                    transportAvailable: transportAvailable,
                    attempt: attempt
                ),
                note: "R6 tests vminitd new-container process creation with id == containerID against a post-create staged rootfs; it does not implement production LinuxPod state integration",
                errors: errors,
                logs: [
                    "utility": logsURL.appendingPathComponent("utility.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        } catch {
            if !transportAvailable {
                errors["transport"] = describe(error)
            } else {
                errors["probe"] = describe(error)
            }
            if utilityStarted {
                try? await pod.stopContainer("utility")
            }
            if podCreated {
                try? await pod.stop()
            }
            try? close(utilityLog, utilityErr, execLog)

            let result = StagedRootfsProcessSummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                podCreated: podCreated,
                utilityStarted: utilityStarted,
                transportAvailable: transportAvailable,
                attempt: attempt,
                outcome: vminitdContainerRootfsOutcome(
                    podCreated: podCreated,
                    utilityStarted: utilityStarted,
                    transportAvailable: transportAvailable,
                    attempt: attempt
                ),
                note: "R6 tests vminitd new-container process creation with id == containerID against a post-create staged rootfs; it does not implement production LinuxPod state integration",
                errors: errors,
                logs: [
                    "utility": logsURL.appendingPathComponent("utility.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        }
    }

    private func runVminitdContainerRootfsAttempt(
        pod: LinuxPod,
        agent: some VirtualMachineAgent,
        podID: String,
        requestID: String,
        execLog: FileLogWriter
    ) async throws -> StagedRootfsProcessAttemptSummary {
        let escapedID = requestID.replacingOccurrences(of: "/", with: "_")
        let processID = "r6-\(escapedID)"
        let stageBase = "/run/macvz-r6/staged/\(escapedID)"
        let rootfsPath = "\(stageBase)/rootfs"
        let identityPath = "\(rootfsPath)/etc/macvz-r6-identity"
        let resultPath = "\(rootfsPath)/macvz-r6-result"
        var stageSucceeded = false
        var processCreateSucceeded = false
        var processStartSucceeded = false
        var processExitCode: Int32?
        var resultVerified = false
        var namespaceVerified = false
        var cleanupSucceeded = false
        var stageOutput = ""
        var verifyOutput = ""
        var cleanupOutput = ""
        var errors: [String: String] = [:]

        do {
            let stage = try await execCapture(
                pod,
                containerID: "utility",
                processID: "r6-stage-\(escapedID)",
                script: stagedProcessRootfsStageScript(
                    requestID: requestID,
                    rootfsPath: rootfsPath,
                    identityPath: identityPath,
                    markerPrefix: "macvz-r6-id"
                ),
                log: execLog
            )
            stageOutput = stage.output
            if stage.exitCode != 0 {
                throw ValidationError("stage command exited with \(stage.exitCode): \(stage.output)")
            }
            try await agent.sync()
            stageSucceeded = true
        } catch {
            errors["stage"] = describe(error)
        }

        if stageSucceeded {
            let spec = stagedRootfsProcessSpec(
                podID: podID,
                requestID: requestID,
                rootfsPath: rootfsPath,
                markerPrefix: "macvz-r6-id",
                identityFileName: "macvz-r6-identity",
                resultFileName: "macvz-r6-result",
                hostnamePrefix: "macvz-r6"
            )
            do {
                try await agent.createProcess(
                    id: processID,
                    containerID: processID,
                    stdinPort: nil,
                    stdoutPort: nil,
                    stderrPort: nil,
                    ociRuntimePath: nil,
                    configuration: spec,
                    options: nil
                )
                processCreateSucceeded = true
            } catch {
                errors["createProcessContainer"] = describe(error)
            }
        }

        if processCreateSucceeded {
            do {
                _ = try await agent.startProcess(id: processID, containerID: processID)
                processStartSucceeded = true
                let status = try await agent.waitProcess(id: processID, containerID: processID, timeoutInSeconds: 10)
                processExitCode = status.exitCode
                if status.exitCode != 0 {
                    errors["processExit"] = "exit \(status.exitCode)"
                }
            } catch {
                errors["startOrWaitProcess"] = describe(error)
            }
        }

        if processCreateSucceeded {
            do {
                try await agent.deleteProcess(id: processID, containerID: processID)
            } catch {
                errors["deleteProcess"] = describe(error)
            }
        }

        if stageSucceeded {
            let verify = try await execCapture(
                pod,
                containerID: "utility",
                processID: "r6-verify-\(escapedID)",
                script: stagedProcessRootfsVerifyScript(
                    requestID: requestID,
                    resultPath: resultPath,
                    markerPrefix: "macvz-r6-id",
                    rootfsPrefix: "/run/macvz-r6/staged"
                ),
                log: execLog
            )
            verifyOutput = verify.output
            let state = parseStagedProcessVerifyOutput(
                verify.output,
                markerPrefix: "macvz-r6-id",
                rootfsPrefix: "/run/macvz-r6/staged"
            )
            resultVerified = verify.exitCode == 0 && state.identityMatched
            namespaceVerified = state.rootfsMatched
            if verify.exitCode != 0 {
                errors["verify"] = "exit \(verify.exitCode): \(verify.output)"
            }
        }

        let cleanup = try await execCapture(
            pod,
            containerID: "utility",
            processID: "r6-cleanup-\(escapedID)",
            script: stagedProcessRootfsCleanupScript(stageBase: stageBase),
            log: execLog
        )
        cleanupOutput = cleanup.output
        cleanupSucceeded = cleanup.exitCode == 0
        if cleanup.exitCode != 0 {
            errors["cleanup"] = "exit \(cleanup.exitCode): \(cleanup.output)"
        }

        return StagedRootfsProcessAttemptSummary(
            requestID: requestID,
            processID: processID,
            processContainerID: processID,
            stagePath: rootfsPath,
            resultPath: resultPath,
            stageSucceeded: stageSucceeded,
            processCreateSucceeded: processCreateSucceeded,
            processStartSucceeded: processStartSucceeded,
            processExitCode: processExitCode,
            resultVerified: resultVerified,
            namespaceVerified: namespaceVerified,
            cleanupSucceeded: cleanupSucceeded,
            stageOutput: stageOutput,
            verifyOutput: verifyOutput,
            cleanupOutput: cleanupOutput,
            outcome: vminitdContainerRootfsAttemptOutcome(
                stageSucceeded: stageSucceeded,
                processCreateSucceeded: processCreateSucceeded,
                processStartSucceeded: processStartSucceeded,
                processExitCode: processExitCode,
                resultVerified: resultVerified,
                namespaceVerified: namespaceVerified,
                cleanupSucceeded: cleanupSucceeded
            ),
            errors: errors
        )
    }

    private func vminitdContainerRootfsAttemptOutcome(
        stageSucceeded: Bool,
        processCreateSucceeded: Bool,
        processStartSucceeded: Bool,
        processExitCode: Int32?,
        resultVerified: Bool,
        namespaceVerified: Bool,
        cleanupSucceeded: Bool
    ) -> String {
        if !stageSucceeded {
            return "stagedRootfsUnavailableToAgentProcess"
        }
        if !processCreateSucceeded {
            return "upstreamChangeRequired"
        }
        if !processStartSucceeded || processExitCode != 0 || !resultVerified || !namespaceVerified {
            return "vminitdContainerRootfsPathFound"
        }
        if !cleanupSucceeded {
            return "cleanupFailed"
        }
        return "vminitdContainerRootfsPathFound"
    }

    private func vminitdContainerRootfsOutcome(
        podCreated: Bool,
        utilityStarted: Bool,
        transportAvailable: Bool,
        attempt: StagedRootfsProcessAttemptSummary?
    ) -> String {
        if !podCreated || !utilityStarted || !transportAvailable {
            return "vmAgentProcessApiUnavailable"
        }
        return attempt?.outcome ?? "vmAgentProcessApiUnavailable"
    }

    private func runVminitdVisibleRootfsProbe(
        imageStore: ImageStore,
        vmm: VZVirtualMachineManager,
        logger: Logger,
        startedAt: Date,
        logsURL: URL,
        rootfsURL: URL
    ) async throws {
        let podID = "macvz-r7-\(Int(startedAt.timeIntervalSince1970))"
        let pod = try LinuxPod(podID, vmm: vmm, logger: logger) { config in
            config.cpus = 2
            config.memoryInBytes = 1024 * 1024 * 1024
            config.hostname = "macvz-runtime-r7"
            config.bootLog = .file(path: logsURL.appendingPathComponent("boot.log"))
        }

        let baseImage = try await imageStore.get(reference: image, pull: true)
        let utilityRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("utility.ext4"))
        let utilityLog = try FileLogWriter(path: logsURL.appendingPathComponent("utility.log"), stream: "stdout")
        let utilityErr = try FileLogWriter(path: logsURL.appendingPathComponent("utility.log"), stream: "stderr")
        let execLog = try FileLogWriter(path: logsURL.appendingPathComponent("exec.log"), stream: "stdout")

        var podCreated = false
        var utilityStarted = false
        var transportAvailable = false
        var attempt: StagedRootfsProcessAttemptSummary?
        var errors: [String: String] = [:]

        do {
            try await pod.addContainer("utility", rootfs: utilityRootfs) { config in
                config.process.arguments = ["/bin/sh", "-c", "exec sleep 300"]
                config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
                config.process.workingDirectory = "/"
                config.process.stdout = utilityLog
                config.process.stderr = utilityErr
                config.useInit = true
            }

            try await pod.create()
            podCreated = true
            try await pod.startContainer("utility")
            utilityStarted = true

            attempt = try await pod.withVirtualMachineInstance { vm in
                let agent = try await vm.dialAgent()
                let result = try await runVminitdVisibleRootfsAttempt(
                    pod: pod,
                    agent: agent,
                    podID: podID,
                    requestID: "late-alpha",
                    execLog: execLog
                )
                try? await agent.close()
                return result
            }
            transportAvailable = true

            if utilityStarted {
                try? await pod.stopContainer("utility")
            }
            try await pod.stop()
            try close(utilityLog, utilityErr, execLog)

            let result = StagedRootfsProcessSummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                podCreated: podCreated,
                utilityStarted: utilityStarted,
                transportAvailable: transportAvailable,
                attempt: attempt,
                outcome: vminitdVisibleRootfsOutcome(
                    podCreated: podCreated,
                    utilityStarted: utilityStarted,
                    transportAvailable: transportAvailable,
                    attempt: attempt
                ),
                note: "R7 tests whether a rootfs staged inside the utility rootfs can be addressed through vminitd's init-namespace path for new-container start",
                errors: errors,
                logs: [
                    "utility": logsURL.appendingPathComponent("utility.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        } catch {
            if !transportAvailable {
                errors["transport"] = describe(error)
            } else {
                errors["probe"] = describe(error)
            }
            if utilityStarted {
                try? await pod.stopContainer("utility")
            }
            if podCreated {
                try? await pod.stop()
            }
            try? close(utilityLog, utilityErr, execLog)

            let result = StagedRootfsProcessSummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                podCreated: podCreated,
                utilityStarted: utilityStarted,
                transportAvailable: transportAvailable,
                attempt: attempt,
                outcome: vminitdVisibleRootfsOutcome(
                    podCreated: podCreated,
                    utilityStarted: utilityStarted,
                    transportAvailable: transportAvailable,
                    attempt: attempt
                ),
                note: "R7 tests whether a rootfs staged inside the utility rootfs can be addressed through vminitd's init-namespace path for new-container start",
                errors: errors,
                logs: [
                    "utility": logsURL.appendingPathComponent("utility.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        }
    }

    private func runVminitdVisibleRootfsAttempt(
        pod: LinuxPod,
        agent: some VirtualMachineAgent,
        podID: String,
        requestID: String,
        execLog: FileLogWriter
    ) async throws -> StagedRootfsProcessAttemptSummary {
        let escapedID = requestID.replacingOccurrences(of: "/", with: "_")
        let processID = "r7-\(escapedID)"
        let utilityStageBase = "/macvz-r7/staged/\(escapedID)"
        let utilityRootfsPath = "\(utilityStageBase)/rootfs"
        let vminitdStageBase = "/run/container/utility/rootfs\(utilityStageBase)"
        let vminitdRootfsPath = "\(vminitdStageBase)/rootfs"
        let identityPath = "\(utilityRootfsPath)/etc/macvz-r7-identity"
        let resultPath = "\(utilityRootfsPath)/macvz-r7-result"
        var stageSucceeded = false
        var processCreateSucceeded = false
        var processStartSucceeded = false
        var processExitCode: Int32?
        var resultVerified = false
        var namespaceVerified = false
        var cleanupSucceeded = false
        var stageOutput = ""
        var verifyOutput = ""
        var cleanupOutput = ""
        var errors: [String: String] = [
            "vminitdRootfsPath": vminitdRootfsPath
        ]

        do {
            let stage = try await execCapture(
                pod,
                containerID: "utility",
                processID: "r7-stage-\(escapedID)",
                script: stagedProcessRootfsStageScript(
                    requestID: requestID,
                    rootfsPath: utilityRootfsPath,
                    identityPath: identityPath,
                    markerPrefix: "macvz-r7-id"
                ),
                log: execLog
            )
            stageOutput = stage.output
            if stage.exitCode != 0 {
                throw ValidationError("stage command exited with \(stage.exitCode): \(stage.output)")
            }
            try await agent.sync()
            stageSucceeded = true
        } catch {
            errors["stage"] = describe(error)
        }

        if stageSucceeded {
            let spec = stagedRootfsProcessSpec(
                podID: podID,
                requestID: requestID,
                rootfsPath: vminitdRootfsPath,
                markerPrefix: "macvz-r7-id",
                identityFileName: "macvz-r7-identity",
                resultFileName: "macvz-r7-result",
                hostnamePrefix: "macvz-r7"
            )
            do {
                try await agent.createProcess(
                    id: processID,
                    containerID: processID,
                    stdinPort: nil,
                    stdoutPort: nil,
                    stderrPort: nil,
                    ociRuntimePath: nil,
                    configuration: spec,
                    options: nil
                )
                processCreateSucceeded = true
            } catch {
                errors["createProcessContainer"] = describe(error)
            }
        }

        if processCreateSucceeded {
            do {
                _ = try await agent.startProcess(id: processID, containerID: processID)
                processStartSucceeded = true
                let status = try await agent.waitProcess(id: processID, containerID: processID, timeoutInSeconds: 10)
                processExitCode = status.exitCode
                if status.exitCode != 0 {
                    errors["processExit"] = "exit \(status.exitCode)"
                }
            } catch {
                errors["startOrWaitProcess"] = describe(error)
            }
        }

        if processCreateSucceeded {
            do {
                try await agent.deleteProcess(id: processID, containerID: processID)
            } catch {
                errors["deleteProcess"] = describe(error)
            }
        }

        if stageSucceeded {
            let verify = try await execCapture(
                pod,
                containerID: "utility",
                processID: "r7-verify-\(escapedID)",
                script: stagedProcessRootfsVerifyScript(
                    requestID: requestID,
                    resultPath: resultPath,
                    markerPrefix: "macvz-r7-id",
                    rootfsPrefix: "/run/container/utility/rootfs/macvz-r7/staged"
                ),
                log: execLog
            )
            verifyOutput = verify.output
            let state = parseStagedProcessVerifyOutput(
                verify.output,
                markerPrefix: "macvz-r7-id",
                rootfsPrefix: "/run/container/utility/rootfs/macvz-r7/staged"
            )
            resultVerified = verify.exitCode == 0 && state.identityMatched
            namespaceVerified = state.rootfsMatched
            if verify.exitCode != 0 {
                errors["verify"] = "exit \(verify.exitCode): \(verify.output)"
            }
        }

        let cleanup = try await execCapture(
            pod,
            containerID: "utility",
            processID: "r7-cleanup-\(escapedID)",
            script: stagedProcessRootfsCleanupScript(stageBase: utilityStageBase),
            log: execLog
        )
        cleanupOutput = cleanup.output
        cleanupSucceeded = cleanup.exitCode == 0
        if cleanup.exitCode != 0 {
            errors["cleanup"] = "exit \(cleanup.exitCode): \(cleanup.output)"
        }

        return StagedRootfsProcessAttemptSummary(
            requestID: requestID,
            processID: processID,
            processContainerID: processID,
            stagePath: utilityRootfsPath,
            resultPath: resultPath,
            stageSucceeded: stageSucceeded,
            processCreateSucceeded: processCreateSucceeded,
            processStartSucceeded: processStartSucceeded,
            processExitCode: processExitCode,
            resultVerified: resultVerified,
            namespaceVerified: namespaceVerified,
            cleanupSucceeded: cleanupSucceeded,
            stageOutput: stageOutput,
            verifyOutput: verifyOutput,
            cleanupOutput: cleanupOutput,
            outcome: vminitdVisibleRootfsAttemptOutcome(
                stageSucceeded: stageSucceeded,
                processCreateSucceeded: processCreateSucceeded,
                processStartSucceeded: processStartSucceeded,
                processExitCode: processExitCode,
                resultVerified: resultVerified,
                namespaceVerified: namespaceVerified,
                cleanupSucceeded: cleanupSucceeded
            ),
            errors: errors
        )
    }

    private func vminitdVisibleRootfsAttemptOutcome(
        stageSucceeded: Bool,
        processCreateSucceeded: Bool,
        processStartSucceeded: Bool,
        processExitCode: Int32?,
        resultVerified: Bool,
        namespaceVerified: Bool,
        cleanupSucceeded: Bool
    ) -> String {
        if !stageSucceeded {
            return "vminitdVisibleRootfsPrimitiveMissing"
        }
        if !processCreateSucceeded || !processStartSucceeded {
            return "vminitdVisibleRootfsPrimitiveMissing"
        }
        if processExitCode != 0 || !resultVerified || !namespaceVerified {
            return "vminitdVisibleRootfsPrimitiveMissing"
        }
        if !cleanupSucceeded {
            return "cleanupFailed"
        }
        return "vminitdVisibleRootfsLaunchSucceeded"
    }

    private func vminitdVisibleRootfsOutcome(
        podCreated: Bool,
        utilityStarted: Bool,
        transportAvailable: Bool,
        attempt: StagedRootfsProcessAttemptSummary?
    ) -> String {
        if !podCreated || !utilityStarted || !transportAvailable {
            return "vminitdVisibleRootfsPrimitiveMissing"
        }
        return attempt?.outcome ?? "vminitdVisibleRootfsPrimitiveMissing"
    }

    private func runVminitdRootfsPrimitiveProbe(
        imageStore: ImageStore,
        vmm: VZVirtualMachineManager,
        logger: Logger,
        startedAt: Date,
        logsURL: URL,
        rootfsURL: URL
    ) async throws {
        let podID = "macvz-r9-\(Int(startedAt.timeIntervalSince1970))"
        let pod = try LinuxPod(podID, vmm: vmm, logger: logger) { config in
            config.cpus = 2
            config.memoryInBytes = 1024 * 1024 * 1024
            config.hostname = "macvz-runtime-r9"
            config.bootLog = .file(path: logsURL.appendingPathComponent("boot.log"))
        }

        let baseImage = try await imageStore.get(reference: image, pull: true)
        let utilityRootfs = try await unpackRootfs(baseImage, at: rootfsURL.appendingPathComponent("utility.ext4"))
        let utilityLog = try FileLogWriter(path: logsURL.appendingPathComponent("utility.log"), stream: "stdout")
        let utilityErr = try FileLogWriter(path: logsURL.appendingPathComponent("utility.log"), stream: "stderr")
        let execLog = try FileLogWriter(path: logsURL.appendingPathComponent("exec.log"), stream: "stdout")

        var podCreated = false
        var utilityStarted = false
        var transportAvailable = false
        var attempt: StagedRootfsProcessAttemptSummary?
        var errors: [String: String] = [:]

        do {
            try await pod.addContainer("utility", rootfs: utilityRootfs) { config in
                config.process.arguments = ["/bin/sh", "-c", "exec sleep 300"]
                config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
                config.process.workingDirectory = "/"
                config.process.stdout = utilityLog
                config.process.stderr = utilityErr
                config.useInit = true
            }

            try await pod.create()
            podCreated = true
            try await pod.startContainer("utility")
            utilityStarted = true

            attempt = try await pod.withVirtualMachineInstance { vm in
                let agent = try await vm.dialAgent()
                let result = try await runVminitdRootfsPrimitiveAttempt(
                    vm: vm,
                    agent: agent,
                    podID: podID,
                    requestID: "late-alpha",
                    rootfsURL: rootfsURL
                )
                try? await agent.close()
                return result
            }
            transportAvailable = true

            if utilityStarted {
                try? await pod.stopContainer("utility")
            }
            try await pod.stop()
            try close(utilityLog, utilityErr, execLog)

            let result = StagedRootfsProcessSummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                podCreated: podCreated,
                utilityStarted: utilityStarted,
                transportAvailable: transportAvailable,
                attempt: attempt,
                outcome: vminitdRootfsPrimitiveOutcome(
                    podCreated: podCreated,
                    utilityStarted: utilityStarted,
                    transportAvailable: transportAvailable,
                    attempt: attempt
                ),
                note: "R9 uses existing vminitd Copy archive transport as a local experimental PrepareContainerRootfs shape, then launches through the existing new-container process path",
                errors: errors,
                logs: [
                    "utility": logsURL.appendingPathComponent("utility.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        } catch {
            if !transportAvailable {
                errors["transport"] = describe(error)
            } else {
                errors["probe"] = describe(error)
            }
            if utilityStarted {
                try? await pod.stopContainer("utility")
            }
            if podCreated {
                try? await pod.stop()
            }
            try? close(utilityLog, utilityErr, execLog)

            let result = StagedRootfsProcessSummary(
                podID: podID,
                image: image,
                kernel: kernel,
                workDir: workDir,
                durationSeconds: Date().timeIntervalSince(startedAt),
                podCreated: podCreated,
                utilityStarted: utilityStarted,
                transportAvailable: transportAvailable,
                attempt: attempt,
                outcome: vminitdRootfsPrimitiveOutcome(
                    podCreated: podCreated,
                    utilityStarted: utilityStarted,
                    transportAvailable: transportAvailable,
                    attempt: attempt
                ),
                note: "R9 uses existing vminitd Copy archive transport as a local experimental PrepareContainerRootfs shape, then launches through the existing new-container process path",
                errors: errors,
                logs: [
                    "utility": logsURL.appendingPathComponent("utility.log").path,
                    "exec": logsURL.appendingPathComponent("exec.log").path,
                    "boot": logsURL.appendingPathComponent("boot.log").path,
                ]
            )
            print(try result.jsonString())
        }
    }

    private func runVminitdRootfsPrimitiveAttempt(
        vm: any VirtualMachineInstance,
        agent: some VirtualMachineAgent,
        podID: String,
        requestID: String,
        rootfsURL: URL
    ) async throws -> StagedRootfsProcessAttemptSummary {
        guard let vminitd = agent as? Vminitd else {
            throw ValidationError("R9 requires Vminitd agent")
        }

        let escapedID = requestID.replacingOccurrences(of: "/", with: "_")
        let processID = "r9-\(escapedID)"
        let rootfsPath = "/run/container/\(processID)/rootfs"
        let resultPath = "\(rootfsPath)/macvz-r9-result"
        let evidencePath = "/run/macvz-r9-evidence/\(processID)"
        let evidenceResultPath = "\(evidencePath)/macvz-r9-result"
        let hostPreparedRootfs = rootfsURL.appendingPathComponent("r9-prepared-\(escapedID)")
        let hostResultPath = rootfsURL.appendingPathComponent("r9-result-\(escapedID).txt")
        let hostEvidenceDir = rootfsURL.appendingPathComponent("r9-evidence-\(escapedID)")
        let hostEvidenceResultPath = rootfsURL.appendingPathComponent("r9-evidence-result-\(escapedID).txt")
        let stderrPort = randomVsockPort()
        let stderrListener = try vm.listen(stderrPort)
        var stderrOutput = ""
        var prepareSucceeded = false
        var processCreateSucceeded = false
        var processStartSucceeded = false
        var processExitCode: Int32?
        var resultVerified = false
        var namespaceVerified = false
        var cleanupSucceeded = false
        var prepareOutput = ""
        var verifyOutput = ""
        var cleanupOutput = ""
        var errors: [String: String] = [
            "experimentalApiShape": "existing vminitd Copy(COPY_OUT/COPY_IN archive) used as PrepareContainerRootfs",
            "rootfsPath": rootfsPath,
        ]

        do {
            try? FileManager.default.removeItem(at: hostPreparedRootfs)
            try? FileManager.default.removeItem(at: hostResultPath)
            try? FileManager.default.removeItem(at: hostEvidenceDir)
            try? FileManager.default.removeItem(at: hostEvidenceResultPath)
            let binDir = hostPreparedRootfs.appendingPathComponent("bin")
            let identityDir = hostPreparedRootfs.appendingPathComponent("etc")
            for dir in ["dev", "macvz-r9-evidence", "proc", "run", "sys", "tmp"] {
                try FileManager.default.createDirectory(
                    at: hostPreparedRootfs.appendingPathComponent(dir),
                    withIntermediateDirectories: true
                )
            }
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o777],
                ofItemAtPath: hostPreparedRootfs.path
            )
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o777],
                ofItemAtPath: hostPreparedRootfs.appendingPathComponent("macvz-r9-evidence").path
            )
            try FileManager.default.createDirectory(at: binDir, withIntermediateDirectories: true)
            try FileManager.default.createDirectory(at: identityDir, withIntermediateDirectories: true)
            try await copyGuestPathToHost(
                vm: vm,
                vminitd: vminitd,
                guestPath: "/run/container/utility/rootfs/bin/busybox",
                destination: binDir.appendingPathComponent("busybox")
            )
            try await copyGuestPathToHost(
                vm: vm,
                vminitd: vminitd,
                guestPath: "/run/container/utility/rootfs/lib",
                destination: hostPreparedRootfs.appendingPathComponent("lib")
            )
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o755],
                ofItemAtPath: binDir.appendingPathComponent("busybox").path
            )
            try FileManager.default.copyItem(
                at: binDir.appendingPathComponent("busybox"),
                to: binDir.appendingPathComponent("sh")
            )
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o755],
                ofItemAtPath: binDir.appendingPathComponent("sh").path
            )
            for applet in ["cat", "grep", "ls", "readlink", "sync", "tr"] {
                let appletPath = binDir.appendingPathComponent(applet)
                try? FileManager.default.removeItem(at: appletPath)
                try FileManager.default.createSymbolicLink(
                    atPath: appletPath.path,
                    withDestinationPath: "busybox"
                )
            }
            try "macvz-r9-id=\(requestID)\n".write(
                to: identityDir.appendingPathComponent("macvz-r9-identity"),
                atomically: true,
                encoding: .utf8
            )
            try? FileManager.default.removeItem(at: hostPreparedRootfs.appendingPathComponent("macvz-r9-result"))
            try FileManager.default.createDirectory(at: hostEvidenceDir, withIntermediateDirectories: true)
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o777],
                ofItemAtPath: hostEvidenceDir.path
            )
            try "macvz-r9-evidence\n".write(
                to: hostEvidenceDir.appendingPathComponent(".keep"),
                atomically: true,
                encoding: .utf8
            )

            try await copyHostPathToGuest(
                vm: vm,
                vminitd: vminitd,
                source: hostPreparedRootfs,
                guestPath: rootfsPath
            )
            try await copyHostPathToGuest(
                vm: vm,
                vminitd: vminitd,
                source: hostEvidenceDir,
                guestPath: evidencePath
            )
            _ = try await vminitd.stat(path: URL(fileURLWithPath: "\(rootfsPath)/bin/sh"))
            _ = try await vminitd.stat(path: URL(fileURLWithPath: "\(rootfsPath)/etc/macvz-r9-identity"))
            _ = try await vminitd.stat(path: URL(fileURLWithPath: "\(evidencePath)/.keep"))
            try await vminitd.sync()
            prepareSucceeded = true
            prepareOutput = "prepare_ok source=/run/container/utility/rootfs/bin/busybox rootfs=\(rootfsPath) evidence=\(evidencePath)"
        } catch {
            errors["prepare"] = describe(error)
        }

        if prepareSucceeded {
            let spec = stagedRootfsProcessSpec(
                podID: podID,
                requestID: requestID,
                rootfsPath: rootfsPath,
                markerPrefix: "macvz-r9-id",
                identityFileName: "macvz-r9-identity",
                resultFileName: "macvz-r9-result",
                evidenceSourcePath: evidencePath,
                evidenceDestinationPath: "/macvz-r9-evidence",
                hostnamePrefix: "macvz-r9"
            )
            do {
                try await vminitd.createProcess(
                    id: processID,
                    containerID: processID,
                    stdinPort: nil,
                    stdoutPort: nil,
                    stderrPort: stderrPort,
                    ociRuntimePath: nil,
                    configuration: spec,
                    options: nil
                )
                processCreateSucceeded = true
            } catch {
                errors["createProcessContainer"] = describe(error)
            }
        }

        if processCreateSucceeded {
            do {
                try await withThrowingTaskGroup(of: StdioCaptureEvent.self) { group in
                    group.addTask {
                        let output = try await captureVsockStream(stderrListener)
                        return .stderr(output)
                    }
                    group.addTask {
                        _ = try await vminitd.startProcess(id: processID, containerID: processID)
                        let status = try await vminitd.waitProcess(id: processID, containerID: processID, timeoutInSeconds: 10)
                        return .exitCode(status.exitCode)
                    }

                    for try await event in group {
                        switch event {
                        case .stderr(let output):
                            stderrOutput = output
                        case .exitCode(let exitCode):
                            processStartSucceeded = true
                            processExitCode = exitCode
                        }
                    }
                }
                if processExitCode != 0, let exitCode = processExitCode {
                    errors["processExit"] = "exit \(exitCode)"
                    if !stderrOutput.isEmpty {
                        errors["processStderr"] = stderrOutput
                    }
                }
            } catch {
                errors["startOrWaitProcess"] = describe(error)
                let diagnostics = await copyVminitdDiagnosticIfPresent(
                    vm: vm,
                    vminitd: vminitd,
                    guestPath: "\(rootfsPath)/run/macvz-r10-vmexec-diagnostics.txt",
                    hostPath: rootfsURL.appendingPathComponent("r10-vmexec-diagnostics-\(escapedID).txt"),
                    key: "vmexecDiagnostics"
                )
                errors[diagnostics.key] = diagnostics.value
                let errorPipe = await copyVminitdDiagnosticIfPresent(
                    vm: vm,
                    vminitd: vminitd,
                    guestPath: "\(rootfsPath)/run/macvz-r10-vmexec-errorpipe.txt",
                    hostPath: rootfsURL.appendingPathComponent("r10-vmexec-errorpipe-\(escapedID).txt"),
                    key: "vmexecErrorPipe"
                )
                errors[errorPipe.key] = errorPipe.value
            }
        }

        if prepareSucceeded && processStartSucceeded && processExitCode == 0 {
            do {
                _ = try await vminitd.stat(path: URL(fileURLWithPath: evidenceResultPath))
                try await copyGuestPathToHost(
                    vm: vm,
                    vminitd: vminitd,
                    guestPath: evidenceResultPath,
                    destination: hostEvidenceResultPath
                )
                verifyOutput = try readTextFile(hostEvidenceResultPath)
                let state = parseStagedProcessVerifyOutput(
                    verifyOutput,
                    markerPrefix: "macvz-r9-id",
                    rootfsPrefix: "/run/container/r9-late-alpha"
                )
                resultVerified = state.identityMatched
                namespaceVerified = state.rootfsMatched
                if resultVerified && !namespaceVerified {
                    errors["namespaceEvidence"] = "bind-mounted handoff verified rootfs identity; proc_root did not expose the host-visible prepared-rootfs path"
                }
                if !resultVerified {
                    errors["verify"] = "identityMatched=\(resultVerified) namespaceVerified=\(namespaceVerified): \(verifyOutput)"
                }
            } catch {
                errors["evidenceChannel"] = describe(error)
                do {
                    _ = try await vminitd.stat(path: URL(fileURLWithPath: resultPath))
                    errors["rootfsResultVisibility"] = "rootfs result unexpectedly visible at \(resultPath)"
                } catch {
                    errors["rootfsResultVisibility"] = describe(error)
                }
            }
        } else if prepareSucceeded {
            errors["verify"] = "skipped because process did not start and exit successfully"
            if !stderrOutput.isEmpty {
                verifyOutput = stderrOutput
                errors["processStderr"] = stderrOutput
            }
        }

        if processCreateSucceeded {
            do {
                try await vminitd.deleteProcess(id: processID, containerID: processID)
                do {
                    _ = try await vminitd.stat(path: URL(fileURLWithPath: rootfsPath))
                    errors["cleanupVerify"] = "rootfs still exists after deleteProcess: \(rootfsPath)"
                } catch {
                    cleanupSucceeded = true
                    cleanupOutput = "cleanup_ok container=\(processID) rootfs=\(rootfsPath)"
                }
            } catch {
                errors["deleteProcess"] = describe(error)
            }
        } else if prepareSucceeded {
            cleanupOutput = "cleanup_skipped_no_container rootfs=\(rootfsPath)"
        }

        return StagedRootfsProcessAttemptSummary(
            requestID: requestID,
            processID: processID,
            processContainerID: processID,
            stagePath: rootfsPath,
            resultPath: resultPath,
            stageSucceeded: prepareSucceeded,
            processCreateSucceeded: processCreateSucceeded,
            processStartSucceeded: processStartSucceeded,
            processExitCode: processExitCode,
            resultVerified: resultVerified,
            namespaceVerified: namespaceVerified,
            cleanupSucceeded: cleanupSucceeded,
            stageOutput: prepareOutput,
            verifyOutput: verifyOutput,
            cleanupOutput: cleanupOutput,
            outcome: vminitdRootfsPrimitiveAttemptOutcome(
                prepareSucceeded: prepareSucceeded,
                processCreateSucceeded: processCreateSucceeded,
                processStartSucceeded: processStartSucceeded,
                processExitCode: processExitCode,
                resultVerified: resultVerified,
                namespaceVerified: namespaceVerified,
                cleanupSucceeded: cleanupSucceeded
            ),
            errors: errors
        )
    }

    private func vminitdRootfsPrimitiveAttemptOutcome(
        prepareSucceeded: Bool,
        processCreateSucceeded: Bool,
        processStartSucceeded: Bool,
        processExitCode: Int32?,
        resultVerified: Bool,
        namespaceVerified: Bool,
        cleanupSucceeded: Bool
    ) -> String {
        if !prepareSucceeded {
            return "vminitdRootfsPrepareFailed"
        }
        if !processCreateSucceeded {
            return "vminitdContainerCreateFailed"
        }
        if !processStartSucceeded {
            return "vminitdContainerStartFailed"
        }
        if processExitCode == 0 && !resultVerified {
            return "lateRootfsEvidenceChannelStillBlocked"
        }
        if processExitCode != 0 || !resultVerified {
            return "vminitdRootfsIdentityMismatch"
        }
        if !cleanupSucceeded {
            return "vminitdRootfsCleanupFailed"
        }
        return "vminitdRootfsPrimitiveLaunchSucceeded"
    }

    private func vminitdRootfsPrimitiveOutcome(
        podCreated: Bool,
        utilityStarted: Bool,
        transportAvailable: Bool,
        attempt: StagedRootfsProcessAttemptSummary?
    ) -> String {
        if !podCreated || !utilityStarted || !transportAvailable {
            return "vminitdRootfsPrepareFailed"
        }
        return attempt?.outcome ?? "vminitdRootfsPrepareFailed"
    }

    private func copyVminitdDiagnosticIfPresent(
        vm: any VirtualMachineInstance,
        vminitd: Vminitd,
        guestPath: String,
        hostPath: URL,
        key: String
    ) async -> (key: String, value: String) {
        do {
            _ = try await vminitd.stat(path: URL(fileURLWithPath: guestPath))
            try? FileManager.default.removeItem(at: hostPath)
            try await copyGuestPathToHost(
                vm: vm,
                vminitd: vminitd,
                guestPath: guestPath,
                destination: hostPath
            )
            return (key, try readTextFile(hostPath))
        } catch {
            return ("\(key)Unavailable", describe(error))
        }
    }

    private func readTextFile(_ url: URL) throws -> String {
        String(decoding: try Data(contentsOf: url), as: UTF8.self)
    }

    private func captureVsockStream(_ listener: VsockListener) async throws -> String {
        guard let conn = await listener.first(where: { _ in true }) else {
            throw probeError("R9 stdio capture: vsock connection not established")
        }
        try listener.finish()
        defer { conn.closeFile() }
        let data = try conn.readToEnd() ?? Data()
        return String(decoding: data, as: UTF8.self)
    }

    private func copyGuestPathToHost(
        vm: any VirtualMachineInstance,
        vminitd: Vminitd,
        guestPath: String,
        destination: URL,
        chunkSize: Int = 1024 * 1024
    ) async throws {
        try FileManager.default.createDirectory(
            at: destination.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )

        let port = randomVsockPort()
        let listener = try vm.listen(port)
        let (metadataStream, metadataCont) = AsyncStream.makeStream(of: Vminitd.CopyMetadata.self)

        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask {
                try await vminitd.copy(
                    direction: .copyOut,
                    guestPath: URL(fileURLWithPath: guestPath),
                    vsockPort: port,
                    onMetadata: { metadata in
                        metadataCont.yield(metadata)
                        metadataCont.finish()
                    }
                )
            }

            group.addTask {
                guard let metadata = await metadataStream.first(where: { _ in true }) else {
                    throw probeError("R9 copyOut: no metadata received")
                }
                guard let conn = await listener.first(where: { _ in true }) else {
                    throw probeError("R9 copyOut: vsock connection not established")
                }
                try listener.finish()

                try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, any Error>) in
                    DispatchQueue.global(qos: .utility).async {
                        do {
                            defer { conn.closeFile() }

                            if metadata.isArchive {
                                try FileManager.default.createDirectory(
                                    at: destination,
                                    withIntermediateDirectories: true
                                )
                                let fh = FileHandle(fileDescriptor: dup(conn.fileDescriptor), closeOnDealloc: true)
                                let reader = try ArchiveReader(format: .pax, filter: .gzip, fileHandle: fh)
                                _ = try reader.extractContents(to: destination)
                            } else {
                                let destFd = open(destination.path, O_WRONLY | O_CREAT | O_TRUNC, 0o644)
                                guard destFd != -1 else {
                                    throw probeError("R9 copyOut: failed to open '\(destination.path)': \(String(cString: strerror(errno)))")
                                }
                                defer { close(destFd) }

                                var buf = [UInt8](repeating: 0, count: chunkSize)
                                while true {
                                    let n = read(conn.fileDescriptor, &buf, buf.count)
                                    if n == 0 {
                                        break
                                    }
                                    guard n > 0 else {
                                        throw probeError("R9 copyOut: vsock read error: \(String(cString: strerror(errno)))")
                                    }
                                    var written = 0
                                    while written < n {
                                        let w = buf.withUnsafeBytes { ptr in
                                            write(destFd, ptr.baseAddress! + written, n - written)
                                        }
                                        guard w > 0 else {
                                            throw probeError("R9 copyOut: write error: \(String(cString: strerror(errno)))")
                                        }
                                        written += w
                                    }
                                }
                            }
                            continuation.resume()
                        } catch {
                            continuation.resume(throwing: error)
                        }
                    }
                }
            }

            try await group.waitForAll()
        }
    }

    private func copyHostPathToGuest(
        vm: any VirtualMachineInstance,
        vminitd: Vminitd,
        source: URL,
        guestPath: String,
        chunkSize: Int = 1024 * 1024
    ) async throws {
        var isDirectory: ObjCBool = false
        guard FileManager.default.fileExists(atPath: source.path, isDirectory: &isDirectory) else {
            throw probeError("R9 copyIn: source not found '\(source.path)'")
        }

        let port = randomVsockPort()
        let listener = try vm.listen(port)
        let isArchive = isDirectory.boolValue

        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask {
                try await vminitd.copy(
                    direction: .copyIn,
                    guestPath: URL(fileURLWithPath: guestPath),
                    vsockPort: port,
                    createParents: true,
                    isArchive: isArchive
                )
            }

            group.addTask {
                guard let conn = await listener.first(where: { _ in true }) else {
                    throw probeError("R9 copyIn: vsock connection not established")
                }
                try listener.finish()

                try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, any Error>) in
                    DispatchQueue.global(qos: .utility).async {
                        do {
                            defer { conn.closeFile() }

                            if isArchive {
                                let writer = try ArchiveWriter(configuration: .init(format: .pax, filter: .gzip))
                                try writer.open(fileDescriptor: conn.fileDescriptor)
                                try writer.archiveDirectory(source)
                                try writer.finishEncoding()
                            } else {
                                let srcFd = open(source.path, O_RDONLY)
                                guard srcFd != -1 else {
                                    throw probeError("R9 copyIn: failed to open '\(source.path)': \(String(cString: strerror(errno)))")
                                }
                                defer { close(srcFd) }

                                var buf = [UInt8](repeating: 0, count: chunkSize)
                                while true {
                                    let n = read(srcFd, &buf, buf.count)
                                    if n == 0 {
                                        break
                                    }
                                    guard n > 0 else {
                                        throw probeError("R9 copyIn: read error: \(String(cString: strerror(errno)))")
                                    }
                                    var written = 0
                                    while written < n {
                                        let w = buf.withUnsafeBytes { ptr in
                                            write(conn.fileDescriptor, ptr.baseAddress! + written, n - written)
                                        }
                                        guard w > 0 else {
                                            throw probeError("R9 copyIn: vsock write error: \(String(cString: strerror(errno)))")
                                        }
                                        written += w
                                    }
                                }
                            }
                            continuation.resume()
                        } catch {
                            continuation.resume(throwing: error)
                        }
                    }
                }
            }

            try await group.waitForAll()
        }
    }

    private func randomVsockPort() -> UInt32 {
        UInt32.random(in: 0x2000_0000 ... 0x2fff_ffff)
    }

    private func probeError(_ message: String) -> ValidationError {
        ValidationError(message)
    }

    private func readExt4File(_ diskURL: URL, path: String) throws -> String {
        let reader = try EXT4.EXT4Reader(blockDevice: .init(diskURL.path))
        let data = try reader.readFile(at: .init(path))
        return String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
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

private struct RuntimeDeviceDiscoverySummary: Encodable {
    let podID: String
    let image: String
    let kernel: String
    let workDir: String
    let durationSeconds: TimeInterval
    let targetRootfs: String
    let targetRootfsBytes: Int64
    let expectedSectors: Int64
    let usbControllerConfigured: Bool
    let instanceCaptured: Bool
    let usbAttachAttempted: Bool
    let usbAttachSucceeded: Bool
    let guestBaseline: [GuestBlockDevice]
    let guestObservedNewDevice: Bool
    let guestCorrelatedDevice: Bool
    let discoveredDevice: String?
    let guestMountSucceeded: Bool
    let markerVerified: Bool
    let guestUnmountSucceeded: Bool
    let usbDetachSucceeded: Bool
    let guestDeviceGoneAfterDetach: Bool
    let outcome: String
    let discoveryMethod: String
    let discoveryOutput: String
    let detachOutput: String
    let events: [String]
    let errors: [String: String]
    let logs: [String: String]

    func jsonString() throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return String(decoding: try encoder.encode(self), as: UTF8.self)
    }
}

private struct NBDRootfsIdentitySummary: Encodable {
    let podID: String
    let image: String
    let kernel: String
    let workDir: String
    let durationSeconds: TimeInterval
    let nbdServersStarted: Bool
    let podCreated: Bool
    let containerStartSucceeded: Bool
    let rootfsMarkersVerified: Bool
    let mountEvidenceVerified: Bool
    let alphaNBDURL: String?
    let betaNBDURL: String?
    let alphaOutput: String
    let betaOutput: String
    let alphaMarkerHost: String?
    let betaMarkerHost: String?
    let outcome: String
    let note: String
    let errors: [String: String]
    let logs: [String: String]

    func jsonString() throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return String(decoding: try encoder.encode(self), as: UTF8.self)
    }
}

private struct GuestRootfsStagingSummary: Encodable {
    let podID: String
    let image: String
    let kernel: String
    let workDir: String
    let durationSeconds: TimeInterval
    let podCreated: Bool
    let utilityStarted: Bool
    let transportAvailable: Bool
    let attempts: [GuestRootfsStagingAttemptSummary]
    let outcome: String
    let note: String
    let errors: [String: String]
    let logs: [String: String]

    func jsonString() throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return String(decoding: try encoder.encode(self), as: UTF8.self)
    }
}

private struct GuestRootfsStagingAttemptSummary: Encodable {
    let requestID: String
    let stagePath: String
    let mountTarget: String
    let stageSucceeded: Bool
    let mountSucceeded: Bool
    let identityVerified: Bool
    let cleanupSucceeded: Bool
    let verifyOutput: String
    let cleanupOutput: String
    let outcome: String
    let errors: [String: String]
}

private struct StagedRootfsProcessSummary: Encodable {
    let podID: String
    let image: String
    let kernel: String
    let workDir: String
    let durationSeconds: TimeInterval
    let podCreated: Bool
    let utilityStarted: Bool
    let transportAvailable: Bool
    let attempt: StagedRootfsProcessAttemptSummary?
    let outcome: String
    let note: String
    let errors: [String: String]
    let logs: [String: String]

    func jsonString() throws -> String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return String(decoding: try encoder.encode(self), as: UTF8.self)
    }
}

private struct StagedRootfsProcessAttemptSummary: Encodable {
    let requestID: String
    let processID: String
    let processContainerID: String?
    let stagePath: String
    let resultPath: String
    let stageSucceeded: Bool
    let processCreateSucceeded: Bool
    let processStartSucceeded: Bool
    let processExitCode: Int32?
    let resultVerified: Bool
    let namespaceVerified: Bool
    let cleanupSucceeded: Bool
    let stageOutput: String
    let verifyOutput: String
    let cleanupOutput: String
    let outcome: String
    let errors: [String: String]
}

private struct GuestBlockDevice: Encodable {
    let name: String
    let sectors: Int64
}

private struct ExecCaptureResult {
    let exitCode: Int32
    let output: String
}

private enum StdioCaptureEvent {
    case stderr(String)
    case exitCode(Int32)
}

private struct DiscoveryOutputState {
    var observedNewDevice = false
    var correlatedDevice = false
    var device: String?
    var mountSucceeded = false
    var markerVerified = false
    var unmountSucceeded = false
}

private struct StagedProcessVerifyState {
    var identityMatched = false
    var rootfsMatched = false
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

private struct DeviceDiscoveryProbeSnapshot {
    let usbControllerConfigured: Bool
    let instanceCaptured: Bool
    let usbAttachAttempted: Bool
    let usbAttachSucceeded: Bool
    let usbDetachSucceeded: Bool
    let events: [String]
}

private final class DeviceDiscoveryProbeState: @unchecked Sendable {
    private let lock = NSLock()
    private var instanceValue: VZVirtualMachineInstance?
    private var usbControllerConfiguredValue = false
    private var usbAttachAttemptedValue = false
    private var usbAttachSucceededValue = false
    private var usbDetachSucceededValue = false
    private var eventsValue: [String] = []

    func setInstance(_ instance: VZVirtualMachineInstance) {
        lock.lock()
        instanceValue = instance
        eventsValue.append("instanceCaptured")
        lock.unlock()
    }

    func mark(_ event: String) {
        lock.lock()
        eventsValue.append(event)
        switch event {
        case "usbControllerConfigured":
            usbControllerConfiguredValue = true
        case "usbAttachAttempted":
            usbAttachAttemptedValue = true
        case "usbAttachSucceeded":
            usbAttachSucceededValue = true
        case "usbDetachSucceeded":
            usbDetachSucceededValue = true
        default:
            break
        }
        lock.unlock()
    }

    func requireInstance() throws -> VZVirtualMachineInstance {
        lock.lock()
        let instance = instanceValue
        lock.unlock()
        guard let instance else {
            throw HotplugProbeFailure(description: "VZVirtualMachineInstance was not captured by the R1 extension")
        }
        return instance
    }

    func snapshot() -> DeviceDiscoveryProbeSnapshot {
        lock.lock()
        defer { lock.unlock() }
        return DeviceDiscoveryProbeSnapshot(
            usbControllerConfigured: usbControllerConfiguredValue,
            instanceCaptured: instanceValue != nil,
            usbAttachAttempted: usbAttachAttemptedValue,
            usbAttachSucceeded: usbAttachSucceededValue,
            usbDetachSucceeded: usbDetachSucceededValue,
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

private final class DeviceDiscoveryProbeExtension: VZInstanceExtension, @unchecked Sendable {
    private let state: DeviceDiscoveryProbeState

    init(state: DeviceDiscoveryProbeState) {
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
        state.setInstance(instance)
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

private final class BufferLogWriter: Writer, @unchecked Sendable {
    private let lock = NSLock()
    private var value = ""

    func write(_ data: Data) throws {
        guard !data.isEmpty else { return }
        lock.lock()
        value += String(decoding: data, as: UTF8.self)
        lock.unlock()
    }

    func string() -> String {
        lock.lock()
        defer { lock.unlock() }
        return value
    }

    func close() throws {}
}

private final class TeeLogWriter: Writer, @unchecked Sendable {
    private let writers: [any Writer]

    init(writers: [any Writer]) {
        self.writers = writers
    }

    func write(_ data: Data) throws {
        for writer in writers {
            try writer.write(data)
        }
    }

    func close() throws {}
}

private final class MiniNBDServer: @unchecked Sendable {
    private let channel: Channel
    private let group: EventLoopGroup
    private let socketPath: String
    let url: String

    init(filePath: String, socketPath: String, logger: Logger? = nil) throws {
        self.socketPath = socketPath
        self.group = MultiThreadedEventLoopGroup(numberOfThreads: 1)
        try? FileManager.default.removeItem(atPath: socketPath)
        self.channel = try ServerBootstrap(group: group)
            .serverChannelOption(.socketOption(.so_reuseaddr), value: 1)
            .childChannelInitializer { channel in
                channel.eventLoop.makeCompletedFuture {
                    try channel.pipeline.syncOperations.addHandler(
                        MiniNBDConnectionHandler(filePath: filePath, logger: logger)
                    )
                }
            }
            .bind(unixDomainSocketPath: socketPath)
            .wait()
        self.url = "nbd+unix:///?socket=\(socketPath)"
    }

    func stop() {
        try? channel.close().wait()
        try? group.syncShutdownGracefully()
        try? FileManager.default.removeItem(atPath: socketPath)
    }
}

private final class MiniNBDConnectionHandler: ChannelInboundHandler {
    typealias InboundIn = ByteBuffer
    typealias OutboundOut = ByteBuffer

    private static let magic: UInt64 = 0x4e42_444d_4147_4943
    private static let ihaveopt: UInt64 = 0x4948_4156_454f_5054
    private static let replyMagic: UInt64 = 0x3e88_9045_565a_9
    private static let requestMagic: UInt32 = 0x2560_9513
    private static let simpleReplyMagic: UInt32 = 0x6744_6698

    private static let optExportName: UInt32 = 1
    private static let optAbort: UInt32 = 2
    private static let optInfo: UInt32 = 6
    private static let optGo: UInt32 = 7

    private static let cmdRead: UInt16 = 0
    private static let cmdWrite: UInt16 = 1
    private static let cmdDisc: UInt16 = 2
    private static let cmdFlush: UInt16 = 3

    private static let flagFixedNewstyle: UInt16 = 0x1
    private static let flagNoZeroes: UInt16 = 0x2
    private static let clientFlagFixedNewstyle: UInt32 = 0x1
    private static let clientFlagNoZeroes: UInt32 = 0x2
    private static let transmitHasFlags: UInt16 = 0x1
    private static let transmitSendFlush: UInt16 = 0x4
    private static let transmitSendFUA: UInt16 = 0x8

    private static let repACK: UInt32 = 1
    private static let repInfo: UInt32 = 3
    private static let repErrUnsup: UInt32 = 0x8000_0001
    private static let infoExport: UInt16 = 0
    private static let infoBlockSize: UInt16 = 3

    private static let errOK: UInt32 = 0
    private static let errIO: UInt32 = 5
    private static let errNotsup: UInt32 = 95

    private let fileFD: Int32
    private let fileSize: UInt64
    private let logger: Logger?
    private var buffer = ByteBuffer()
    private var state: ConnectionState = .handshake

    private enum ConnectionState {
        case handshake
        case options(noZeroes: Bool)
        case transmission
    }

    init(filePath: String, logger: Logger?) {
        self.fileFD = open(filePath, O_RDWR)
        self.logger = logger
        guard fileFD >= 0 else {
            self.fileSize = 0
            logger?.error("NBD server failed to open backing file", metadata: ["path": "\(filePath)", "errno": "\(errno)"])
            return
        }
        var st = stat()
        if fstat(fileFD, &st) == 0 {
            self.fileSize = UInt64(st.st_size)
        } else {
            self.fileSize = 0
        }
    }

    func channelActive(context: ChannelHandlerContext) {
        guard fileFD >= 0 else {
            context.close(promise: nil)
            return
        }
        var buf = context.channel.allocator.buffer(capacity: 18)
        buf.writeInteger(Self.magic)
        buf.writeInteger(Self.ihaveopt)
        buf.writeInteger(Self.flagFixedNewstyle | Self.flagNoZeroes)
        context.writeAndFlush(wrapOutboundOut(buf), promise: nil)
    }

    func channelInactive(context: ChannelHandlerContext) {
        if fileFD >= 0 {
            close(fileFD)
        }
    }

    func channelRead(context: ChannelHandlerContext, data: NIOAny) {
        var incoming = unwrapInboundIn(data)
        buffer.writeBuffer(&incoming)
        processBuffer(context: context)
    }

    private func processBuffer(context: ChannelHandlerContext) {
        while true {
            switch state {
            case .handshake:
                guard buffer.readableBytes >= 4, let clientFlags = buffer.readInteger(as: UInt32.self) else {
                    return
                }
                guard clientFlags & Self.clientFlagFixedNewstyle != 0 else {
                    context.close(promise: nil)
                    return
                }
                state = .options(noZeroes: clientFlags & Self.clientFlagNoZeroes != 0)

            case .options(let noZeroes):
                guard buffer.readableBytes >= 16 else {
                    return
                }
                let readerIndex = buffer.readerIndex
                guard let magic = buffer.getInteger(at: readerIndex, as: UInt64.self),
                    let optType = buffer.getInteger(at: readerIndex + 8, as: UInt32.self),
                    let dataLen = buffer.getInteger(at: readerIndex + 12, as: UInt32.self)
                else {
                    context.close(promise: nil)
                    return
                }
                guard buffer.readableBytes >= 16 + Int(dataLen) else {
                    return
                }
                buffer.moveReaderIndex(forwardBy: 16)
                guard magic == Self.ihaveopt else {
                    context.close(promise: nil)
                    return
                }

                let transmitFlags = Self.transmitHasFlags | Self.transmitSendFlush | Self.transmitSendFUA
                switch optType {
                case Self.optExportName:
                    if dataLen > 0 {
                        buffer.moveReaderIndex(forwardBy: Int(dataLen))
                    }
                    var reply = context.channel.allocator.buffer(capacity: noZeroes ? 10 : 134)
                    reply.writeInteger(fileSize)
                    reply.writeInteger(transmitFlags)
                    if !noZeroes {
                        reply.writeRepeatingByte(0, count: 124)
                    }
                    context.writeAndFlush(wrapOutboundOut(reply), promise: nil)
                    state = .transmission

                case Self.optInfo, Self.optGo:
                    let requestedBlockSize = consumeInfoRequest(dataLen: dataLen)
                    var reply = context.channel.allocator.buffer(capacity: 64)
                    writeOptReply(&reply, optType: optType, replyType: Self.repInfo, dataLen: 12)
                    reply.writeInteger(Self.infoExport)
                    reply.writeInteger(fileSize)
                    reply.writeInteger(transmitFlags)
                    if requestedBlockSize {
                        writeOptReply(&reply, optType: optType, replyType: Self.repInfo, dataLen: 14)
                        reply.writeInteger(Self.infoBlockSize)
                        reply.writeInteger(UInt32(1))
                        reply.writeInteger(UInt32(4096))
                        reply.writeInteger(UInt32(4096 * 32))
                    }
                    writeOptReply(&reply, optType: optType, replyType: Self.repACK, dataLen: 0)
                    context.writeAndFlush(wrapOutboundOut(reply), promise: nil)
                    if optType == Self.optGo {
                        state = .transmission
                    }

                case Self.optAbort:
                    if dataLen > 0 {
                        buffer.moveReaderIndex(forwardBy: Int(dataLen))
                    }
                    context.close(promise: nil)
                    return

                default:
                    if dataLen > 0 {
                        buffer.moveReaderIndex(forwardBy: Int(dataLen))
                    }
                    var reply = context.channel.allocator.buffer(capacity: 20)
                    writeOptReply(&reply, optType: optType, replyType: Self.repErrUnsup, dataLen: 0)
                    context.writeAndFlush(wrapOutboundOut(reply), promise: nil)
                }

            case .transmission:
                guard buffer.readableBytes >= 28 else {
                    return
                }
                let readerIndex = buffer.readerIndex
                guard let magic = buffer.getInteger(at: readerIndex, as: UInt32.self),
                    let cmdType = buffer.getInteger(at: readerIndex + 6, as: UInt16.self),
                    let cookie = buffer.getInteger(at: readerIndex + 8, as: UInt64.self),
                    let offset = buffer.getInteger(at: readerIndex + 16, as: UInt64.self),
                    let length = buffer.getInteger(at: readerIndex + 24, as: UInt32.self)
                else {
                    context.close(promise: nil)
                    return
                }
                guard magic == Self.requestMagic else {
                    context.close(promise: nil)
                    return
                }

                switch cmdType {
                case Self.cmdWrite:
                    guard buffer.readableBytes >= 28 + Int(length) else {
                        return
                    }
                    buffer.moveReaderIndex(forwardBy: 28)
                    var writeData = [UInt8](repeating: 0, count: Int(length))
                    buffer.readWithUnsafeReadableBytes { ptr in
                        writeData.withUnsafeMutableBytes { dst in
                            guard let dstBase = dst.baseAddress, let srcBase = ptr.baseAddress else {
                                return
                            }
                            memcpy(dstBase, srcBase, Int(length))
                        }
                        return Int(length)
                    }
                    let n = pwrite(fileFD, &writeData, Int(length), off_t(offset))
                    var reply = context.channel.allocator.buffer(capacity: 16)
                    writeSimpleReply(&reply, cookie: cookie, error: n < 0 ? Self.errIO : Self.errOK)
                    context.writeAndFlush(wrapOutboundOut(reply), promise: nil)

                case Self.cmdRead:
                    buffer.moveReaderIndex(forwardBy: 28)
                    var readData = [UInt8](repeating: 0, count: Int(length))
                    let n = pread(fileFD, &readData, Int(length), off_t(offset))
                    var reply = context.channel.allocator.buffer(capacity: 16 + Int(length))
                    writeSimpleReply(&reply, cookie: cookie, error: n < 0 ? Self.errIO : Self.errOK)
                    if n >= 0 {
                        reply.writeBytes(readData[0..<Int(length)])
                    }
                    context.writeAndFlush(wrapOutboundOut(reply), promise: nil)

                case Self.cmdDisc:
                    buffer.moveReaderIndex(forwardBy: 28)
                    context.close(promise: nil)
                    return

                case Self.cmdFlush:
                    buffer.moveReaderIndex(forwardBy: 28)
                    fsync(fileFD)
                    var reply = context.channel.allocator.buffer(capacity: 16)
                    writeSimpleReply(&reply, cookie: cookie, error: Self.errOK)
                    context.writeAndFlush(wrapOutboundOut(reply), promise: nil)

                default:
                    buffer.moveReaderIndex(forwardBy: 28)
                    var reply = context.channel.allocator.buffer(capacity: 16)
                    writeSimpleReply(&reply, cookie: cookie, error: Self.errNotsup)
                    context.writeAndFlush(wrapOutboundOut(reply), promise: nil)
                }
            }
        }
    }

    private func consumeInfoRequest(dataLen: UInt32) -> Bool {
        var requestedBlockSize = false
        if dataLen >= 6 {
            let start = buffer.readerIndex
            let nameLen = Int(buffer.getInteger(at: start, as: UInt32.self) ?? 0)
            let infoOffset = start + 4 + nameLen
            if infoOffset + 2 <= start + Int(dataLen) {
                let numReqs = Int(buffer.getInteger(at: infoOffset, as: UInt16.self) ?? 0)
                for i in 0..<numReqs {
                    let reqOffset = infoOffset + 2 + i * 2
                    if reqOffset + 2 <= start + Int(dataLen) {
                        let infoType = buffer.getInteger(at: reqOffset, as: UInt16.self) ?? 0
                        requestedBlockSize = requestedBlockSize || infoType == Self.infoBlockSize
                    }
                }
            }
        }
        if dataLen > 0 {
            buffer.moveReaderIndex(forwardBy: Int(dataLen))
        }
        return requestedBlockSize
    }

    private func writeOptReply(_ buffer: inout ByteBuffer, optType: UInt32, replyType: UInt32, dataLen: UInt32) {
        buffer.writeInteger(Self.replyMagic)
        buffer.writeInteger(optType)
        buffer.writeInteger(replyType)
        buffer.writeInteger(dataLen)
    }

    private func writeSimpleReply(_ buffer: inout ByteBuffer, cookie: UInt64, error: UInt32) {
        buffer.writeInteger(Self.simpleReplyMagic)
        buffer.writeInteger(error)
        buffer.writeInteger(cookie)
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
