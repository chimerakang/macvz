import Darwin
import Foundation

// SupervisorClient is the router's NDJSON client to one per-Pod supervisor process
// (CRI-L6-4 / #139). The main helper owns the public CRI socket and routes every
// VM-mutating op for an existing Pod over this connection to the supervisor that
// owns that Pod's LinuxPod / VZVirtualMachineInstance handle. It speaks the exact
// same wire envelope the NDJSONServer answers ({"op","payload"} -> {"ok","result"|
// "code","error"}), so the supervisor is just a LinuxPodBackend served on a private
// socket and this client is symmetric with the Go HelperClient.
//
// One connection, one in-flight request at a time: the RouterBackend actor
// serializes calls, mirroring the supervisor's actor serialization. Blocking
// syscalls are acceptable here because they run inside the actor's executor and the
// supervisor answers one line per request in order.
final class SupervisorClient: @unchecked Sendable {
    private static let ioTimeoutSeconds: Int = 2

    let socketPath: String
    private var fd: Int32 = -1
    private var readBuffer = Data()

    init(socketPath: String) {
        self.socketPath = socketPath
    }

    var isConnected: Bool { fd >= 0 }

    // connect opens the unix stream socket to the supervisor. Throws a BackendError
    // (so router callers classify it like any backend failure) when the supervisor is
    // unreachable — the signal the adoption fallback uses to declare a Pod lost.
    func connect() throws {
        if fd >= 0 { return }
        let s = socket(AF_UNIX, SOCK_STREAM, 0)
        guard s >= 0 else { throw err("socket") }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = Array(socketPath.utf8)
        guard pathBytes.count < MemoryLayout.size(ofValue: addr.sun_path) else {
            Darwin.close(s)
            throw BackendError(code: "Internal", message: "supervisor socket path too long")
        }
        withUnsafeMutablePointer(to: &addr.sun_path) {
            $0.withMemoryRebound(to: CChar.self, capacity: pathBytes.count + 1) { dst in
                for (i, b) in pathBytes.enumerated() { dst[i] = CChar(bitPattern: b) }
                dst[pathBytes.count] = 0
            }
        }
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.connect(s, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard rc == 0 else {
            Darwin.close(s)
            throw err("connect")
        }
        // Writing to a supervisor that has died would raise SIGPIPE and take the router
        // down; SO_NOSIGPIPE turns that into an EPIPE the router handles as a dead
        // supervisor (adoption fallback). Belt-and-suspenders with the process-wide
        // SIG_IGN set at startup.
        var on: Int32 = 1
        _ = setsockopt(s, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
        var timeout = timeval(tv_sec: Self.ioTimeoutSeconds, tv_usec: 0)
        _ = setsockopt(s, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
        _ = setsockopt(s, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
        fd = s
        readBuffer.removeAll(keepingCapacity: true)
    }

    func close() {
        if fd >= 0 { Darwin.close(fd); fd = -1 }
        readBuffer.removeAll(keepingCapacity: false)
    }

    // call sends one op and returns the parsed response envelope. A transport failure
    // throws (router treats it as supervisor-unreachable); a backend-level failure is
    // carried in the envelope's ok/code/error fields for the router to re-encode.
    func call(op: String, payload: [String: Any]) throws -> [String: Any] {
        try connect()
        let request: [String: Any] = ["op": op, "payload": payload]
        var line = try JSONSerialization.data(withJSONObject: request)
        line.append(UInt8(ascii: "\n"))
        try writeAll(line)
        let respLine = try readLine()
        guard let obj = try? JSONSerialization.jsonObject(with: respLine) as? [String: Any] else {
            throw BackendError(code: "Internal", message: "supervisor returned malformed response")
        }
        return obj
    }

    private func writeAll(_ data: Data) throws {
        try data.withUnsafeBytes { raw in
            var off = 0
            let base = raw.baseAddress!
            while off < data.count {
                try waitFD(events: Int16(POLLOUT), what: "write timeout")
                let w = write(fd, base + off, data.count - off)
                if w <= 0 {
                    close()
                    throw err("write")
                }
                off += w
            }
        }
    }

    // readLine returns the next complete NDJSON line, reading from the socket until a
    // newline arrives. A closed/half-open connection (read <= 0) is a transport error.
    private func readLine() throws -> Data {
        while true {
            if let nl = readBuffer.firstIndex(of: UInt8(ascii: "\n")) {
                let line = readBuffer.subdata(in: readBuffer.startIndex..<nl)
                readBuffer.removeSubrange(readBuffer.startIndex...nl)
                return line
            }
            var chunk = [UInt8](repeating: 0, count: 8192)
            try waitFD(events: Int16(POLLIN), what: "read timeout")
            let n = read(fd, &chunk, chunk.count)
            if n <= 0 {
                close()
                throw err("read")
            }
            readBuffer.append(contentsOf: chunk[0..<n])
        }
    }

    private func waitFD(events: Int16, what: String) throws {
        var pfd = pollfd(fd: fd, events: events, revents: 0)
        let rc = poll(&pfd, 1, Int32(Self.ioTimeoutSeconds * 1000))
        if rc == 0 {
            close()
            throw BackendError(code: "Internal", message: "supervisor \(socketPath): \(what)")
        }
        if rc < 0 {
            close()
            throw err("poll")
        }
        if (pfd.revents & Int16(POLLERR | POLLHUP | POLLNVAL)) != 0 {
            close()
            throw BackendError(code: "Internal", message: "supervisor \(socketPath): disconnected")
        }
    }

    private func err(_ what: String) -> BackendError {
        BackendError(code: "Internal", message: "supervisor \(socketPath): \(what): \(String(cString: strerror(errno)))")
    }
}
