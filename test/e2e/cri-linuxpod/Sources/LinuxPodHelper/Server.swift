import Darwin
import Foundation
import Logging

// Server.swift implements the unix-socket NDJSON server: one JSON request object
// per line, one JSON response object per line, in order, mirroring the stub and
// the Go server. It bridges the blocking accept/read/write syscalls to async so
// the dispatch can await the real (VM-driven) backend. Each connection is handled
// on its own Task; the backend is an actor, so concurrent connections serialize
// safely without an explicit lock.

// BackendError carries a wire error code so failures classify exactly like the Go
// sentinels (errors.Is on the client side). Codes match pkg/runtime/linuxpod
// protocol.go: PodNotFound, ContainerNotFound, RootfsNotFound, Invalid,
// IdentityUnverified, Unsupported, Internal.
struct BackendError: Error {
    let code: String
    let message: String
}

final class NDJSONServer: @unchecked Sendable {
    private let socketPath: String
    private let backend: LinuxPodBackend
    private let logger: Logger

    init(socketPath: String, backend: LinuxPodBackend, logger: Logger) {
        self.socketPath = socketPath
        self.backend = backend
        self.logger = logger
    }

    func serve() async throws {
        unlink(socketPath)
        let listenFD = socket(AF_UNIX, SOCK_STREAM, 0)
        guard listenFD >= 0 else { throw posixError("socket") }

        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = Array(socketPath.utf8)
        guard pathBytes.count < MemoryLayout.size(ofValue: addr.sun_path) else {
            throw BackendError(code: "Internal", message: "socket path too long for sun_path")
        }
        withUnsafeMutablePointer(to: &addr.sun_path) {
            $0.withMemoryRebound(to: CChar.self, capacity: pathBytes.count + 1) { dst in
                for (i, b) in pathBytes.enumerated() { dst[i] = CChar(bitPattern: b) }
                dst[pathBytes.count] = 0
            }
        }
        let bound = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                bind(listenFD, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard bound == 0 else { throw posixError("bind") }
        guard listen(listenFD, 16) == 0 else { throw posixError("listen") }
        logger.info("linuxpod-helper listening", metadata: ["socket": .string(socketPath)])

        while true {
            let conn = try await acceptOne(listenFD)
            Task { await self.handleConnection(conn) }
        }
    }

    // acceptOne bridges the blocking accept() to async on a background queue so the
    // cooperative pool is never blocked waiting for the next connection.
    private func acceptOne(_ listenFD: Int32) async throws -> Int32 {
        try await withCheckedThrowingContinuation { (cont: CheckedContinuation<Int32, Error>) in
            DispatchQueue.global(qos: .userInitiated).async {
                let fd = accept(listenFD, nil, nil)
                if fd < 0 {
                    cont.resume(throwing: self.posixError("accept"))
                } else {
                    cont.resume(returning: fd)
                }
            }
        }
    }

    private func handleConnection(_ fd: Int32) async {
        defer { close(fd) }
        var buffer = Data()
        while true {
            // Drain any complete lines already buffered before another read. The
            // actor parses the request and serializes the response, returning a
            // Sendable Data line (no non-Sendable dict crosses the actor boundary).
            while let nl = buffer.firstIndex(of: UInt8(ascii: "\n")) {
                let line = buffer.subdata(in: buffer.startIndex..<nl)
                buffer.removeSubrange(buffer.startIndex...nl)
                var framed = await backend.handle(line)
                framed.append(UInt8(ascii: "\n"))
                _ = await blockingWrite(fd, framed)
            }
            guard let chunk = await blockingRead(fd), !chunk.isEmpty else { return }
            buffer.append(chunk)
        }
    }

    private func blockingRead(_ fd: Int32) async -> Data? {
        await withCheckedContinuation { (cont: CheckedContinuation<Data?, Never>) in
            DispatchQueue.global(qos: .userInitiated).async {
                var chunk = [UInt8](repeating: 0, count: 8192)
                let n = read(fd, &chunk, chunk.count)
                if n <= 0 {
                    cont.resume(returning: nil)
                } else {
                    cont.resume(returning: Data(chunk[0..<n]))
                }
            }
        }
    }

    private func blockingWrite(_ fd: Int32, _ data: Data) async -> Bool {
        await withCheckedContinuation { (cont: CheckedContinuation<Bool, Never>) in
            DispatchQueue.global(qos: .userInitiated).async {
                var ok = true
                data.withUnsafeBytes { raw in
                    var off = 0
                    let base = raw.baseAddress!
                    while off < data.count {
                        let w = write(fd, base + off, data.count - off)
                        if w <= 0 { ok = false; break }
                        off += w
                    }
                }
                cont.resume(returning: ok)
            }
        }
    }

    private func posixError(_ what: String) -> BackendError {
        BackendError(code: "Internal", message: "\(what): \(String(cString: strerror(errno)))")
    }
}
