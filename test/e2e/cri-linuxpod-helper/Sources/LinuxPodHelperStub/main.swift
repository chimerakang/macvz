import Darwin
import Foundation

// LinuxPodHelperStub serves the CRI-R17 LinuxPod backend contract (#124) over a
// unix socket using newline-delimited JSON (NDJSON), the same protocol the Go
// pkg/runtime/linuxpod HelperClient speaks. It models the LinuxPod lifecycle in
// memory — a Pod VM with one shared network namespace, late-binding container
// creation after the pod is running, rootfs identity verification at start, and a
// cleanup that leaves no state — exactly mirroring the Go FakeBackend. It boots no
// real VM (Ping reports simulated=true). A production helper swaps the model for
// Apple Containerization LinuxPod calls and keeps this wire protocol unchanged.
//
// Usage: linuxpod-helper-stub --socket /path/to/helper.sock

let protocolVersion = 3

// Capabilities advertised in Ping. The stub backs all three kubelet surfaces
// (logs, exec, stats) in simulated form, matching the Go FakeBackend (CRI-L4, #129).
// Held in an enum namespace (not top-level code) so it is a nonisolated Sendable
// global: top-level `let` in main.swift is @MainActor-isolated under Swift 6,
// which the nonisolated lifecycle methods below cannot reference.
enum Capabilities {
    static let all: [String: Bool] = ["logs": true, "exec": true, "stats": true]
}

// MARK: - In-memory lifecycle model (mirrors the Go FakeBackend)

final class Pod {
    let id: String
    let namespace: String
    let sandboxAddress: String
    var phase: String = "Running"
    var containers: [String: Container] = [:]
    init(id: String, sandboxAddress: String) {
        self.id = id
        self.namespace = "linuxpod-ns-\(id)"
        self.sandboxAddress = sandboxAddress
    }
}

final class Rootfs {
    let token: String
    let podID: String
    let name: String
    let expectedIdentity: String
    let path: String
    var bound = false
    init(token: String, podID: String, name: String, expectedIdentity: String) {
        self.token = token
        self.podID = podID
        self.name = name
        self.expectedIdentity = expectedIdentity
        self.path = "/run/macvz/containers/\(token)/rootfs"
    }
}

final class Container {
    let id: String
    let name: String
    let podID: String
    let rootfsToken: String
    let expectedIdentity: String
    let logPath: String
    var phase: String = "Created"
    var exitCode = 0
    var message = ""
    var observedIdentity = ""
    var identityVerified = false
    let createdAfterPodRunning: Bool
    init(id: String, name: String, podID: String, rootfsToken: String,
         expectedIdentity: String, logPath: String, createdAfterPodRunning: Bool) {
        self.id = id
        self.name = name
        self.podID = podID
        self.rootfsToken = rootfsToken
        self.expectedIdentity = expectedIdentity
        self.logPath = logPath
        self.createdAfterPodRunning = createdAfterPodRunning
    }
}

// BackendError carries a wire error code so failures classify exactly like the Go
// sentinels (errors.Is on the client side).
struct BackendError: Error {
    let code: String
    let message: String
}

/// Model is the single-threaded lifecycle state. The server serializes all access
/// on one queue, so no internal locking is needed.
final class Model {
    private var pods: [String: Pod] = [:]
    private var rootfs: [String: Rootfs] = [:]
    private var seq = 0

    private func next() -> Int { seq += 1; return seq }

    func ping() -> [String: Any] {
        ["name": "linuxpod-helper-stub", "protocolVersion": protocolVersion,
         "simulated": true, "capabilities": Capabilities.all]
    }

    // appendCRILog writes one CRI-format log line ("<rfc3339nano> <stream> F <msg>")
    // for a container that has a kubelet log path. Best-effort: a write failure is
    // ignored so it cannot wedge a lifecycle op, matching the Go FakeBackend.
    private func appendCRILog(_ c: Container, _ stream: String, _ msg: String) {
        guard !c.logPath.isEmpty else { return }
        let url = URL(fileURLWithPath: c.logPath)
        try? FileManager.default.createDirectory(at: url.deletingLastPathComponent(),
                                                  withIntermediateDirectories: true)
        let fmt = ISO8601DateFormatter()
        fmt.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let line = "\(fmt.string(from: Date())) \(stream) F \(msg)\n"
        guard let data = line.data(using: .utf8) else { return }
        if let handle = try? FileHandle(forWritingTo: url) {
            handle.seekToEndOfFile()
            handle.write(data)
            try? handle.close()
        } else {
            try? data.write(to: url)
        }
    }

    func createPod(_ p: [String: Any]) throws -> [String: Any] {
        guard let id = p["id"] as? String, !id.isEmpty else {
            throw BackendError(code: "Invalid", message: "pod id is required")
        }
        if pods[id] != nil {
            throw BackendError(code: "Invalid", message: "pod \(id) already exists")
        }
        // Deterministic, plausible host-only address so a client reading it sees an
        // IP-shaped value (CRI-L3, #128). A production helper returns the VM's actual
        // vmnet address once acquired. The stub has it ready immediately.
        let pod = Pod(id: id, sandboxAddress: "192.168.66.\((next() % 253) + 2)")
        pods[id] = pod
        return podStatus(pod)
    }

    func podStatusOf(_ p: [String: Any]) throws -> [String: Any] {
        let podID = (p["podID"] as? String) ?? ""
        guard let pod = pods[podID] else {
            throw BackendError(code: "PodNotFound", message: podID)
        }
        return podStatus(pod)
    }

    private func podStatus(_ pod: Pod) -> [String: Any] {
        ["id": pod.id, "phase": pod.phase, "sandboxNamespace": pod.namespace,
         "sandboxAddress": pod.sandboxAddress]
    }

    func prepareRootfs(_ p: [String: Any]) throws -> [String: Any] {
        let podID = (p["podID"] as? String) ?? ""
        let name = (p["containerName"] as? String) ?? ""
        let expected = (p["expectedIdentity"] as? String) ?? ""
        if podID.isEmpty || name.isEmpty || expected.isEmpty {
            throw BackendError(code: "Invalid", message: "podID, containerName, and expectedIdentity are required")
        }
        guard let pod = pods[podID] else {
            throw BackendError(code: "PodNotFound", message: podID)
        }
        if pod.phase != "Running" {
            throw BackendError(code: "Invalid", message: "pod \(podID) is \(pod.phase), cannot stage rootfs")
        }
        let token = "rootfs-\(podID)-\(name)-\(next())"
        let rf = Rootfs(token: token, podID: podID, name: name, expectedIdentity: expected)
        rootfs[token] = rf
        return ["token": token, "rootfsPath": rf.path]
    }

    func createContainer(_ p: [String: Any]) throws -> [String: Any] {
        let podID = (p["podID"] as? String) ?? ""
        let name = (p["name"] as? String) ?? ""
        let token = (p["rootfsToken"] as? String) ?? ""
        if podID.isEmpty || name.isEmpty || token.isEmpty {
            throw BackendError(code: "Invalid", message: "podID, name, and rootfsToken are required")
        }
        guard let pod = pods[podID] else {
            throw BackendError(code: "PodNotFound", message: podID)
        }
        guard let rf = rootfs[token], rf.podID == podID else {
            throw BackendError(code: "RootfsNotFound", message: token)
        }
        if rf.bound {
            throw BackendError(code: "Invalid", message: "rootfs token \(token) already bound")
        }
        if pod.containers.values.contains(where: { $0.name == name }) {
            throw BackendError(code: "Invalid", message: "container \(name) already exists in pod \(podID)")
        }
        rf.bound = true
        let logPath = (p["logPath"] as? String) ?? ""
        let podHasRunning = pod.containers.values.contains { $0.phase == "Running" }
        let c = Container(id: "\(podID)/\(name)-\(next())", name: name, podID: podID,
                          rootfsToken: token, expectedIdentity: rf.expectedIdentity,
                          logPath: logPath, createdAfterPodRunning: podHasRunning)
        pod.containers[c.id] = c
        appendCRILog(c, "stdout", "container created (simulated LinuxPod backend; no real stdout)")
        return status(pod, c)
    }

    func startContainer(_ p: [String: Any]) throws -> [String: Any] {
        let (pod, c) = try lookup(p)
        if c.phase == "Running" { return status(pod, c) }
        if c.phase != "Created" {
            throw BackendError(code: "Invalid", message: "container \(c.id) is \(c.phase), expected Created")
        }
        // The real helper reads the identity the late process reports through the
        // handoff channel; the stub models a faithful process that reports the
        // expected identity, then verifies with exact match (CRI-R16).
        c.observedIdentity = c.expectedIdentity
        if c.observedIdentity != c.expectedIdentity || c.expectedIdentity.isEmpty {
            c.phase = "Failed"
            c.exitCode = 1
            c.identityVerified = false
            c.message = "rootfs identity not verified"
            throw BackendError(code: "IdentityUnverified", message: "container \(c.id) identity mismatch")
        }
        c.phase = "Running"
        c.identityVerified = true
        c.message = ""
        appendCRILog(c, "stdout", "container started (identity verified)")
        return status(pod, c)
    }

    func containerLogPath(_ p: [String: Any]) throws -> [String: Any] {
        guard Capabilities.all["logs"] == true else {
            throw BackendError(code: "Unsupported", message: "logs")
        }
        let (_, c) = try lookup(p)
        if c.logPath.isEmpty {
            throw BackendError(code: "Invalid", message: "container \(c.id) was created without a log path")
        }
        return ["podID": c.podID, "containerID": c.id, "path": c.logPath, "format": "cri"]
    }

    func execSync(_ p: [String: Any]) throws -> [String: Any] {
        guard Capabilities.all["exec"] == true else {
            throw BackendError(code: "Unsupported", message: "exec")
        }
        let (_, c) = try lookup(p)
        let command = (p["command"] as? [String]) ?? []
        if command.isEmpty {
            throw BackendError(code: "Invalid", message: "exec command is required")
        }
        if c.phase != "Running" {
            throw BackendError(code: "Invalid", message: "container \(c.id) is \(c.phase), exec requires Running")
        }
        // Simulated exec: echo the command back, base64-encoded to match the Go
        // []byte JSON encoding the client decodes.
        let stdout = Data((command.joined(separator: " ") + "\n").utf8).base64EncodedString()
        let stderr = Data("linuxpod: simulated exec (no real Pod VM)\n".utf8).base64EncodedString()
        return ["stdout": stdout, "stderr": stderr, "exitCode": 0]
    }

    func containerStats(_ p: [String: Any]) throws -> [String: Any] {
        guard Capabilities.all["stats"] == true else {
            throw BackendError(code: "Unsupported", message: "stats")
        }
        let (_, c) = try lookup(p)
        return [
            "podID": c.podID,
            "containerID": c.id,
            "timestampNanos": Int(Date().timeIntervalSince1970 * 1_000_000_000),
            "cpuUsageNanoCores": 0,
            "memoryWorkingSetBytes": 0,
            "simulated": true,
        ]
    }

    func stopContainer(_ p: [String: Any]) throws -> [String: Any] {
        let podID = (p["podID"] as? String) ?? ""
        let cid = (p["containerID"] as? String) ?? ""
        let (pod, c) = try lookup(["podID": podID, "containerID": cid])
        if c.phase == "Running" {
            c.phase = "Stopped"
            c.exitCode = 0
        }
        return status(pod, c)
    }

    func removeContainer(_ p: [String: Any]) throws {
        let podID = (p["podID"] as? String) ?? ""
        let cid = (p["containerID"] as? String) ?? ""
        guard let pod = pods[podID], let c = pod.containers[cid] else { return } // idempotent
        pod.containers.removeValue(forKey: cid)
        rootfs.removeValue(forKey: c.rootfsToken)
    }

    func statusOf(_ p: [String: Any]) throws -> [String: Any] {
        let (pod, c) = try lookup(p)
        return status(pod, c)
    }

    func cleanup(_ p: [String: Any]) -> [String: Any] {
        let podID = (p["podID"] as? String) ?? ""
        guard let pod = pods[podID] else {
            return ["podID": podID, "removedContainers": 0, "removedRootfs": 0,
                    "podRemoved": false, "staleState": false]
        }
        let removedContainers = pod.containers.count
        var removedRootfs = 0
        for (token, rf) in rootfs where rf.podID == podID {
            rootfs.removeValue(forKey: token)
            removedRootfs += 1
        }
        pods.removeValue(forKey: podID)
        return ["podID": podID, "removedContainers": removedContainers,
                "removedRootfs": removedRootfs, "podRemoved": true, "staleState": false]
    }

    private func lookup(_ p: [String: Any]) throws -> (Pod, Container) {
        let podID = (p["podID"] as? String) ?? ""
        let cid = (p["containerID"] as? String) ?? ""
        guard let pod = pods[podID] else {
            throw BackendError(code: "PodNotFound", message: podID)
        }
        guard let c = pod.containers[cid] else {
            throw BackendError(code: "ContainerNotFound", message: cid)
        }
        return (pod, c)
    }

    private func status(_ pod: Pod, _ c: Container) -> [String: Any] {
        [
            "podID": c.podID,
            "id": c.id,
            "name": c.name,
            "phase": c.phase,
            "exitCode": c.exitCode,
            "message": c.message,
            "sandboxNamespace": pod.namespace,
            "createdAfterPodRunning": c.createdAfterPodRunning,
            "localhostReachable": c.phase == "Running",
            "expectedIdentity": c.expectedIdentity,
            "observedIdentity": c.observedIdentity,
            "identityVerified": c.identityVerified,
        ]
    }
}

// MARK: - Dispatch

func dispatch(_ model: Model, _ line: Data) -> [String: Any] {
    guard
        let obj = try? JSONSerialization.jsonObject(with: line) as? [String: Any],
        let op = obj["op"] as? String
    else {
        return ["ok": false, "code": "Invalid", "error": "malformed request"]
    }
    let payload = (obj["payload"] as? [String: Any]) ?? [:]
    do {
        switch op {
        case "Ping":
            return ok(model.ping())
        case "CreatePod":
            return ok(try model.createPod(payload))
        case "PodStatus":
            return ok(try model.podStatusOf(payload))
        case "PrepareContainerRootfs":
            return ok(try model.prepareRootfs(payload))
        case "CreateContainer":
            return ok(try model.createContainer(payload))
        case "StartContainer":
            return ok(try model.startContainer(payload))
        case "StopContainer":
            return ok(try model.stopContainer(payload))
        case "RemoveContainer":
            try model.removeContainer(payload)
            return ["ok": true]
        case "Status":
            return ok(try model.statusOf(payload))
        case "Cleanup":
            return ok(model.cleanup(payload))
        case "ContainerLogPath":
            return ok(try model.containerLogPath(payload))
        case "ExecSync":
            return ok(try model.execSync(payload))
        case "ContainerStats":
            return ok(try model.containerStats(payload))
        default:
            return ["ok": false, "code": "Invalid", "error": "unknown op \(op)"]
        }
    } catch let e as BackendError {
        return ["ok": false, "code": e.code, "error": e.message]
    } catch {
        return ["ok": false, "code": "Internal", "error": "\(error)"]
    }
}

func ok(_ result: [String: Any]) -> [String: Any] {
    ["ok": true, "result": result]
}

// MARK: - Unix socket server (NDJSON, one response per request, in order)

func serve(socketPath: String) -> Never {
    unlink(socketPath)
    let listenFD = socket(AF_UNIX, SOCK_STREAM, 0)
    guard listenFD >= 0 else { fatalErrno("socket") }

    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let pathBytes = Array(socketPath.utf8)
    precondition(pathBytes.count < MemoryLayout.size(ofValue: addr.sun_path),
                 "socket path too long for sun_path")
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
    guard bound == 0 else { fatalErrno("bind") }
    guard listen(listenFD, 16) == 0 else { fatalErrno("listen") }

    let model = Model()
    FileHandle.standardError.write(Data("linuxpod-helper-stub listening on \(socketPath)\n".utf8))

    while true {
        let conn = accept(listenFD, nil, nil)
        if conn < 0 { continue }
        serveConnection(conn, model: model)
        close(conn)
    }
}

// serveConnection reads NDJSON request lines off one connection and writes one
// response line per request until the peer closes it.
func serveConnection(_ fd: Int32, model: Model) {
    var buffer = Data()
    var chunk = [UInt8](repeating: 0, count: 4096)
    while true {
        let n = read(fd, &chunk, chunk.count)
        if n <= 0 { return }
        buffer.append(contentsOf: chunk[0..<n])
        while let nl = buffer.firstIndex(of: UInt8(ascii: "\n")) {
            let line = buffer.subdata(in: buffer.startIndex..<nl)
            buffer.removeSubrange(buffer.startIndex...nl)
            let resp = dispatch(model, line)
            guard var out = try? JSONSerialization.data(withJSONObject: resp) else { continue }
            out.append(UInt8(ascii: "\n"))
            out.withUnsafeBytes { raw in
                _ = write(fd, raw.baseAddress, raw.count)
            }
        }
    }
}

func fatalErrno(_ what: String) -> Never {
    let e = String(cString: strerror(errno))
    FileHandle.standardError.write(Data("linuxpod-helper-stub: \(what): \(e)\n".utf8))
    exit(1)
}

// MARK: - Entry point

func parseSocketArg() -> String {
    var args = Array(CommandLine.arguments.dropFirst())
    while !args.isEmpty {
        let a = args.removeFirst()
        if a == "--socket", let v = args.first {
            return v
        }
        if a.hasPrefix("--socket=") {
            return String(a.dropFirst("--socket=".count))
        }
    }
    FileHandle.standardError.write(Data("usage: linuxpod-helper-stub --socket /path/to/helper.sock\n".utf8))
    exit(2)
}

serve(socketPath: parseSocketArg())
