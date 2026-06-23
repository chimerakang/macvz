import Containerization
import ContainerizationArchive
import ContainerizationEXT4
import ContainerizationExtras
import ContainerizationOCI
import Darwin
import Foundation
import Logging

// Primitives.swift ports the proven CRI-R9 late-rootfs transport and bootstrap
// helpers from the LinuxPodSharedNamespacePoC into reusable pieces the real helper
// backend drives: VM/image-store bootstrap, the vminitd Copy(COPY_IN/COPY_OUT)
// host<->guest transport, vsock stream capture, the OCI process spec for a staged
// late container, and a CRI-format log writer.

// HelperRuntime holds the shared, pod-independent runtime: the VM manager and the
// image store the backend uses to boot each Pod VM and unpack rootfs.
final class HelperRuntime: @unchecked Sendable {
    let vmm: VZVirtualMachineManager
    let imageStore: ImageStore
    let image: String
    let workRoot: URL
    let logger: Logger

    private init(vmm: VZVirtualMachineManager, imageStore: ImageStore, image: String, workRoot: URL, logger: Logger) {
        self.vmm = vmm
        self.imageStore = imageStore
        self.image = image
        self.workRoot = workRoot
        self.logger = logger
    }

    static func bootstrap(
        kernel: String,
        initfsReference: String,
        containerizationRoot: String?,
        image: String,
        workDir: String,
        rosetta: Bool,
        logger: Logger
    ) async throws -> HelperRuntime {
        let fm = FileManager.default
        let workRoot = URL(fileURLWithPath: workDir)
        try? fm.removeItem(at: workRoot)
        try fm.createDirectory(at: workRoot, withIntermediateDirectories: true)

        guard fm.fileExists(atPath: kernel) else {
            throw BackendError(code: "Internal", message: "kernel does not exist: \(kernel)")
        }
        let stateRoot = containerizationRoot.map { URL(fileURLWithPath: $0) }
            ?? fm.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
                .appendingPathComponent("com.apple.containerization")
        try fm.createDirectory(at: stateRoot, withIntermediateDirectories: true)

        let imageStore = try ImageStore(path: stateRoot)
        let initfs = try await Self.prepareInitfs(
            imageStore: imageStore, stateRoot: stateRoot, reference: initfsReference, fm: fm)
        let vmm = VZVirtualMachineManager(
            kernel: Kernel(path: URL(fileURLWithPath: kernel), platform: .linuxArm),
            initialFilesystem: initfs,
            rosetta: rosetta,
            logger: logger
        )
        return HelperRuntime(vmm: vmm, imageStore: imageStore, image: image, workRoot: workRoot, logger: logger)
    }

    private static func prepareInitfs(
        imageStore: ImageStore, stateRoot: URL, reference: String, fm: FileManager
    ) async throws -> Containerization.Mount {
        let initPath = stateRoot.appendingPathComponent("initfs.ext4")
        let initImage = try await imageStore.getInitImage(reference: reference)
        do {
            return try await initImage.initBlock(at: initPath, for: .linuxArm)
        } catch {
            if fm.fileExists(atPath: initPath.path) {
                return .block(format: "ext4", source: initPath.path, destination: "/", options: ["ro"])
            }
            throw error
        }
    }

    func unpackRootfs(at url: URL) async throws -> Containerization.Mount {
        if FileManager.default.fileExists(atPath: url.path) {
            return .block(format: "ext4", source: url.path, destination: "/", options: [])
        }
        let baseImage = try await imageStore.get(reference: image, pull: true)
        let unpacker = EXT4Unpacker(blockSizeInBytes: 2 * 1024 * 1024 * 1024)
        return try await unpacker.unpack(baseImage, for: .current, at: url)
    }
}

// Transport ports the R9 vminitd Copy host<->guest primitive and vsock capture.
enum Transport {
    static func randomVsockPort() -> UInt32 {
        UInt32.random(in: 0x2000_0000 ... 0x2fff_ffff)
    }

    static func captureVsockStream(_ listener: VsockListener) async throws -> String {
        guard let conn = await listener.first(where: { _ in true }) else {
            throw BackendError(code: "Internal", message: "vsock connection not established")
        }
        try listener.finish()
        defer { conn.closeFile() }
        let data = try conn.readToEnd() ?? Data()
        return String(decoding: data, as: UTF8.self)
    }

    static func copyGuestPathToHost(
        vm: any VirtualMachineInstance,
        vminitd: Vminitd,
        guestPath: String,
        destination: URL,
        chunkSize: Int = 1024 * 1024
    ) async throws {
        try FileManager.default.createDirectory(
            at: destination.deletingLastPathComponent(), withIntermediateDirectories: true)
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
                    throw BackendError(code: "Internal", message: "copyOut: no metadata received")
                }
                guard let conn = await listener.first(where: { _ in true }) else {
                    throw BackendError(code: "Internal", message: "copyOut: vsock connection not established")
                }
                try listener.finish()
                try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Void, any Error>) in
                    DispatchQueue.global(qos: .utility).async {
                        do {
                            defer { conn.closeFile() }
                            if metadata.isArchive {
                                try FileManager.default.createDirectory(
                                    at: destination, withIntermediateDirectories: true)
                                let fh = FileHandle(fileDescriptor: dup(conn.fileDescriptor), closeOnDealloc: true)
                                let reader = try ArchiveReader(format: .pax, filter: .gzip, fileHandle: fh)
                                _ = try reader.extractContents(to: destination)
                            } else {
                                let destFd = open(destination.path, O_WRONLY | O_CREAT | O_TRUNC, 0o644)
                                guard destFd != -1 else {
                                    throw BackendError(code: "Internal", message: "copyOut: open \(destination.path): \(String(cString: strerror(errno)))")
                                }
                                defer { close(destFd) }
                                var buf = [UInt8](repeating: 0, count: chunkSize)
                                while true {
                                    let n = read(conn.fileDescriptor, &buf, buf.count)
                                    if n == 0 { break }
                                    guard n > 0 else {
                                        throw BackendError(code: "Internal", message: "copyOut: read: \(String(cString: strerror(errno)))")
                                    }
                                    var written = 0
                                    while written < n {
                                        let w = buf.withUnsafeBytes { write(destFd, $0.baseAddress! + written, n - written) }
                                        guard w > 0 else {
                                            throw BackendError(code: "Internal", message: "copyOut: write: \(String(cString: strerror(errno)))")
                                        }
                                        written += w
                                    }
                                }
                            }
                            cont.resume()
                        } catch {
                            cont.resume(throwing: error)
                        }
                    }
                }
            }
            try await group.waitForAll()
        }
    }

    static func copyHostPathToGuest(
        vm: any VirtualMachineInstance,
        vminitd: Vminitd,
        source: URL,
        guestPath: String,
        chunkSize: Int = 1024 * 1024
    ) async throws {
        var isDirectory: ObjCBool = false
        guard FileManager.default.fileExists(atPath: source.path, isDirectory: &isDirectory) else {
            throw BackendError(code: "Internal", message: "copyIn: source not found \(source.path)")
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
                    throw BackendError(code: "Internal", message: "copyIn: vsock connection not established")
                }
                try listener.finish()
                try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Void, any Error>) in
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
                                    throw BackendError(code: "Internal", message: "copyIn: open \(source.path): \(String(cString: strerror(errno)))")
                                }
                                defer { close(srcFd) }
                                var buf = [UInt8](repeating: 0, count: chunkSize)
                                while true {
                                    let n = read(srcFd, &buf, buf.count)
                                    if n == 0 { break }
                                    guard n > 0 else {
                                        throw BackendError(code: "Internal", message: "copyIn: read: \(String(cString: strerror(errno)))")
                                    }
                                    var written = 0
                                    while written < n {
                                        let w = buf.withUnsafeBytes { write(conn.fileDescriptor, $0.baseAddress! + written, n - written) }
                                        guard w > 0 else {
                                            throw BackendError(code: "Internal", message: "copyIn: write: \(String(cString: strerror(errno)))")
                                        }
                                        written += w
                                    }
                                }
                            }
                            cont.resume()
                        } catch {
                            cont.resume(throwing: error)
                        }
                    }
                }
            }
            try await group.waitForAll()
        }
    }
}

// FileLogWriter appends container output to a CRI-format log file: one
// "<rfc3339nano> <stdout|stderr> F <message>" line per write (LogInfo.Format "cri").
final class FileLogWriter: Writer, @unchecked Sendable {
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

    func close() throws { try handle.close() }
}
