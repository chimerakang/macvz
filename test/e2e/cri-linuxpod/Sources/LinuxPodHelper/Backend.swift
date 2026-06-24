import Containerization
import ContainerizationOCI
import Foundation
import Logging

// Backend.swift is the real LinuxPod lifecycle behind the NDJSON protocol. It holds
// one LinuxPod VM per pod (kept up by a long-lived "holder" container) and models
// each late container as a vminitd process launched from a rootfs staged into the
// already-running VM via the R9 Copy primitive, with Running gated on host-side
// identity verification through a bind-mounted handoff evidence channel (CRI-R16).
//
// The backend is an actor: NDJSON connections may arrive concurrently, but every
// VM-mutating op serializes here, so the LinuxPod and vminitd calls never race.

// helperProtocolVersion is the NDJSON wire-protocol version this helper speaks; it
// must equal pkg/runtime/linuxpod ProtocolVersion.
let helperProtocolVersion = 5

// parseEvidenceText extracts the observed identity and net-namespace inode the late
// process wrote into the handoff evidence channel. Free (nonisolated) so the
// @Sendable VM closure can call it without crossing actor isolation.
func parseEvidenceText(_ text: String) -> (identity: String, netns: String) {
    var identity = ""
    var netns = ""
    for line in text.split(whereSeparator: \.isNewline).map(String.init) {
        if line.hasPrefix("identity=") { identity = String(line.dropFirst("identity=".count)) }
        if line.hasPrefix("netns=") { netns = String(line.dropFirst("netns=".count)) }
    }
    return (identity, netns)
}

struct GuestMount: Sendable {
    let source: String
    let guestSource: String
    let target: String
    let readOnly: Bool
    let tmpfs: Bool
}

func safeGuestMountName(_ source: String) -> String {
    let mapped = source.unicodeScalars.map { scalar -> Character in
        if CharacterSet.alphanumerics.contains(scalar) { return Character(scalar) }
        return "-"
    }
    let body = String(mapped).trimmingCharacters(in: CharacterSet(charactersIn: "-"))
    return String((body.isEmpty ? "mount" : body).prefix(80)) + "-" + stableMountHash(source)
}

func stableMountHash(_ source: String) -> String {
    var hash: UInt64 = 14695981039346656037
    for byte in source.utf8 {
        hash ^= UInt64(byte)
        hash &*= 1099511628211
    }
    return String(hash, radix: 16)
}

// MARK: - State

final class RootfsState {
    let token: String
    let podID: String
    let name: String
    let expectedIdentity: String
    let guestRootfsPath: String
    let guestEvidencePath: String
    let hostPreparedRootfs: URL
    let hostEvidenceDir: URL
    var bound = false
    init(token: String, podID: String, name: String, expectedIdentity: String,
         guestRootfsPath: String, guestEvidencePath: String,
         hostPreparedRootfs: URL, hostEvidenceDir: URL) {
        self.token = token
        self.podID = podID
        self.name = name
        self.expectedIdentity = expectedIdentity
        self.guestRootfsPath = guestRootfsPath
        self.guestEvidencePath = guestEvidencePath
        self.hostPreparedRootfs = hostPreparedRootfs
        self.hostEvidenceDir = hostEvidenceDir
    }
}

final class ContainerState {
    let id: String
    let name: String
    let podID: String
    let rootfsToken: String
    let processID: String
    let expectedIdentity: String
    let createdAfterPodRunning: Bool
    var phase = "Created"
    var exitCode = 0
    var message = ""
    var observedIdentity = ""
    var identityVerified = false
    // logPath is the CRI log file the kubelet tails; "" when CreateContainer gave none.
    // logTasks are the background stdout/stderr vsock capture tasks streaming the
    // container's output into logPath; cancelled on remove (CRI-L4 follow-up #133).
    var logPath = ""
    var logTasks: [Task<Void, Never>] = []
    init(id: String, name: String, podID: String, rootfsToken: String, processID: String,
         expectedIdentity: String, createdAfterPodRunning: Bool) {
        self.id = id
        self.name = name
        self.podID = podID
        self.rootfsToken = rootfsToken
        self.processID = processID
        self.expectedIdentity = expectedIdentity
        self.createdAfterPodRunning = createdAfterPodRunning
    }
}

final class PodState {
    let id: String
    let pod: LinuxPod
    let interfaceID: String?
    let holderName = "holder"
    var phase = "Running"
    var sandboxNamespace: String
    var sandboxAddress: String
    var containers: [String: ContainerState] = [:]
    init(id: String, pod: LinuxPod, sandboxAddress: String, interfaceID: String?) {
        self.id = id
        self.pod = pod
        self.sandboxAddress = sandboxAddress
        self.interfaceID = interfaceID
        // Placeholder until a container reports the VM's real net namespace inode;
        // stable and non-empty so CreatePod can return it immediately.
        self.sandboxNamespace = "linuxpod-ns-\(id)"
    }
}

// MARK: - Backend

actor LinuxPodBackend {
    private let runtime: HelperRuntime
    private let logger: Logger
    private var pods: [String: PodState] = [:]
    private var rootfsByToken: [String: RootfsState] = [:]
    private var seq = 0

    init(runtime: HelperRuntime, logger: Logger) {
        self.runtime = runtime
        self.logger = logger
    }

    private func next() -> Int { seq += 1; return seq }

    // handle parses one NDJSON request line, routes the op, and serializes the
    // response envelope to a line of Data. It runs entirely inside the actor so no
    // non-Sendable [String: Any] crosses the actor boundary. Backend failures
    // classify through BackendError wire codes; any other throw becomes Internal.
    func handle(_ line: Data) async -> Data {
        let resp = await dispatch(line)
        return (try? JSONSerialization.data(withJSONObject: resp))
            ?? Data(#"{"ok":false,"code":"Internal","error":"response encode failed"}"#.utf8)
    }

    private func dispatch(_ line: Data) async -> [String: Any] {
        guard
            let obj = try? JSONSerialization.jsonObject(with: line) as? [String: Any],
            let op = obj["op"] as? String
        else {
            return ["ok": false, "code": "Invalid", "error": "malformed request"]
        }
        let payload = (obj["payload"] as? [String: Any]) ?? [:]
        do {
            switch op {
            case "Ping": return wrap(ping())
            case "CreatePod": return wrap(try await createPod(payload))
            case "PodStatus": return wrap(try podStatus(payload))
            case "PrepareContainerRootfs": return wrap(try await prepareRootfs(payload))
            case "CreateContainer": return wrap(try await createContainer(payload))
            case "StartContainer": return wrap(try await startContainer(payload))
            case "StopContainer": return wrap(try await stopContainer(payload))
            case "RemoveContainer":
                try await removeContainer(payload)
                return ["ok": true]
            case "Status": return wrap(try statusOf(payload))
            case "Cleanup": return wrap(try await cleanup(payload))
            case "ContainerLogPath": return wrap(try containerLogPath(payload))  // sync: returns the path
            case "ExecSync": return wrap(try await execSync(payload))
            case "ContainerStats": return wrap(try await containerStats(payload))
            default:
                return ["ok": false, "code": "Invalid", "error": "unknown op \(op)"]
            }
        } catch let e as BackendError {
            return ["ok": false, "code": e.code, "error": e.message]
        } catch {
            return ["ok": false, "code": "Internal", "error": "\(error)"]
        }
    }

    private func wrap(_ result: [String: Any]) -> [String: Any] {
        ["ok": true, "result": result]
    }

    // MARK: Handshake

    func ping() -> [String: Any] {
        // The real helper backs the core LinuxPod lifecycle + identity (CRI-L1) and,
        // per CRI-L4 follow-up (#133), the kubelet surfaces logs/exec/stats with real
        // Pod-VM data (measured cgroup stats, in-VM exec, streamed container output).
        // Interactive ExecStream/Attach/PortForward remain out of scope here.
        [
            "name": "linuxpod-helper",
            "protocolVersion": helperProtocolVersion,
            "simulated": false,
            "capabilities": ["logs": true, "exec": true, "stats": true],
        ]
    }

    // MARK: Pod lifecycle

    func createPod(_ p: [String: Any]) async throws -> [String: Any] {
        guard let id = p["id"] as? String, !id.isEmpty else {
            throw BackendError(code: "Invalid", message: "pod id is required")
        }
        if pods[id] != nil {
            throw BackendError(code: "Invalid", message: "pod \(id) already exists")
        }
        let cpus = (p["cpus"] as? Int) ?? 2
        let memoryBytes = (p["memoryBytes"] as? Int).map { UInt64($0) } ?? (1024 * 1024 * 1024)
        // The guest runs sethostname(), which rejects an empty name (and any name
        // over HOST_NAME_MAX=64) with EINVAL. CRI clients may send an empty hostname,
        // and the pod id is a 64-hex string that is itself too long, so derive a
        // valid, bounded hostname rather than passing either through verbatim.
        var hostname = (p["hostname"] as? String) ?? ""
        if hostname.isEmpty {
            hostname = "macvz-pod-" + String(id.prefix(12))
        }
        if hostname.count > 63 {
            hostname = String(hostname.prefix(63))
        }

        let podDir = runtime.workRoot.appendingPathComponent(id)
        try FileManager.default.createDirectory(at: podDir, withIntermediateDirectories: true)
        let holderRootfs = try await runtime.unpackRootfs(at: podDir.appendingPathComponent("holder.ext4"))
        let podInterface = try runtime.createPodInterface(id)
        let sandboxAddress = podInterface?.ipv4Address.address.description ?? ""
        let interfaceID = podInterface == nil ? nil : id
        var releaseInterfaceOnFailure = interfaceID != nil
        defer {
            if releaseInterfaceOnFailure {
                runtime.releasePodInterface(id)
            }
        }

        let pod = try LinuxPod(id, vmm: runtime.vmm, logger: logger) { config in
            config.cpus = cpus
            config.memoryInBytes = memoryBytes
            config.hostname = hostname
            if let podInterface {
                config.interfaces = [podInterface]
            }
            config.bootLog = .file(path: podDir.appendingPathComponent("boot.log"))
        }
        // A long-lived holder keeps the VM (and its shared namespace) up so late
        // containers can be staged into a running Pod VM. The holder process must
        // have stdout/stderr wired: Apple Containerization's vmexec rejects a process
        // configured with no output streams (startProcess fails EINVAL), so mirror the
        // PoC and point them at a CRI-format holder log.
        let holderLog = try FileLogWriter(path: podDir.appendingPathComponent("holder.log"), stream: "stdout")
        let holderErr = try FileLogWriter(path: podDir.appendingPathComponent("holder.log"), stream: "stderr")
        try await pod.addContainer("holder", rootfs: holderRootfs) { config in
            config.process.arguments = ["/bin/sh", "-c", "exec sleep 2147483647"]
            config.process.environmentVariables = ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
            config.process.workingDirectory = "/"
            config.process.stdout = holderLog
            config.process.stderr = holderErr
            config.useInit = true
        }
        do {
            try await pod.create()
            try await pod.startContainer("holder")
        } catch {
            try? await pod.stop()
            throw BackendError(code: "Internal", message: "create pod \(id): \(error)")
        }

        let state = PodState(id: id, pod: pod, sandboxAddress: sandboxAddress, interfaceID: interfaceID)
        pods[id] = state
        releaseInterfaceOnFailure = false
        return [
            "id": id, "phase": state.phase,
            "sandboxNamespace": state.sandboxNamespace, "sandboxAddress": state.sandboxAddress,
        ]
    }

    func podStatus(_ p: [String: Any]) throws -> [String: Any] {
        let podID = (p["podID"] as? String) ?? ""
        guard let pod = pods[podID] else { throw BackendError(code: "PodNotFound", message: podID) }
        return [
            "id": pod.id, "phase": pod.phase,
            "sandboxNamespace": pod.sandboxNamespace, "sandboxAddress": pod.sandboxAddress,
        ]
    }

    // MARK: Late-rootfs primitive

    func prepareRootfs(_ p: [String: Any]) async throws -> [String: Any] {
        let podID = (p["podID"] as? String) ?? ""
        let name = (p["containerName"] as? String) ?? ""
        let expected = (p["expectedIdentity"] as? String) ?? ""
        if podID.isEmpty || name.isEmpty || expected.isEmpty {
            throw BackendError(code: "Invalid", message: "podID, containerName, and expectedIdentity are required")
        }
        guard let pod = pods[podID] else { throw BackendError(code: "PodNotFound", message: podID) }
        if pod.phase != "Running" {
            throw BackendError(code: "Invalid", message: "pod \(podID) is \(pod.phase), cannot stage rootfs")
        }

        let token = "rootfs-\(podID)-\(name)-\(next())"
        let safe = token.replacingOccurrences(of: "/", with: "_")
        let guestRootfsPath = "/run/container/\(safe)/rootfs"
        let guestEvidencePath = "/run/macvz-evidence/\(safe)"
        let podDir = runtime.workRoot.appendingPathComponent(podID)
        let hostPreparedRootfs = podDir.appendingPathComponent("prepared-\(safe)")
        let hostEvidenceDir = podDir.appendingPathComponent("evidence-\(safe)")

        try await stagePreparedRootfs(
            pod: pod, expectedIdentity: expected,
            hostPreparedRootfs: hostPreparedRootfs, hostEvidenceDir: hostEvidenceDir,
            guestRootfsPath: guestRootfsPath, guestEvidencePath: guestEvidencePath)

        let rf = RootfsState(
            token: token, podID: podID, name: name, expectedIdentity: expected,
            guestRootfsPath: guestRootfsPath, guestEvidencePath: guestEvidencePath,
            hostPreparedRootfs: hostPreparedRootfs, hostEvidenceDir: hostEvidenceDir)
        rootfsByToken[token] = rf
        return ["token": token, "rootfsPath": guestRootfsPath]
    }

    // stagePreparedRootfs builds a minimal busybox rootfs on the host (copying the
    // holder's busybox/lib out of the running VM), stages the expected identity into
    // /etc/macvz-container-identity, and copies both the rootfs and a writable
    // evidence directory into the running Pod VM via the R9 Copy primitive.
    private func stagePreparedRootfs(
        pod: PodState, expectedIdentity: String,
        hostPreparedRootfs: URL, hostEvidenceDir: URL,
        guestRootfsPath: String, guestEvidencePath: String
    ) async throws {
        let fm = FileManager.default
        try? fm.removeItem(at: hostPreparedRootfs)
        try? fm.removeItem(at: hostEvidenceDir)
        let binDir = hostPreparedRootfs.appendingPathComponent("bin")
        let etcDir = hostPreparedRootfs.appendingPathComponent("etc")
        for dir in ["dev", "etc", "macvz-evidence", "proc", "run", "sys", "tmp"] {
            try fm.createDirectory(at: hostPreparedRootfs.appendingPathComponent(dir), withIntermediateDirectories: true)
        }
        try fm.setAttributes([.posixPermissions: 0o777], ofItemAtPath: hostPreparedRootfs.path)
        try fm.createDirectory(at: binDir, withIntermediateDirectories: true)

        try await pod.pod.withVirtualMachineInstance { vm in
            let agent = try await vm.dialAgent()
            guard let vminitd = agent as? Vminitd else {
                throw BackendError(code: "Internal", message: "agent is not vminitd")
            }
            try await Transport.copyGuestPathToHost(
                vm: vm, vminitd: vminitd,
                guestPath: "/run/container/holder/rootfs/bin/busybox",
                destination: binDir.appendingPathComponent("busybox"))
            try await Transport.copyGuestPathToHost(
                vm: vm, vminitd: vminitd,
                guestPath: "/run/container/holder/rootfs/lib",
                destination: hostPreparedRootfs.appendingPathComponent("lib"))
            try? await vminitd.close()
        }

        try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: binDir.appendingPathComponent("busybox").path)
        try fm.copyItem(at: binDir.appendingPathComponent("busybox"), to: binDir.appendingPathComponent("sh"))
        try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: binDir.appendingPathComponent("sh").path)
        for applet in ["cat", "grep", "httpd", "ls", "mkdir", "readlink", "seq", "sleep", "sync", "tail", "tr", "wget"] {
            let appletPath = binDir.appendingPathComponent(applet)
            try? fm.removeItem(at: appletPath)
            try fm.createSymbolicLink(atPath: appletPath.path, withDestinationPath: "busybox")
        }
        try "\(expectedIdentity)\n".write(
            to: etcDir.appendingPathComponent("macvz-container-identity"), atomically: true, encoding: .utf8)

        try fm.createDirectory(at: hostEvidenceDir, withIntermediateDirectories: true)
        try fm.setAttributes([.posixPermissions: 0o777], ofItemAtPath: hostEvidenceDir.path)
        try "macvz-evidence\n".write(to: hostEvidenceDir.appendingPathComponent(".keep"), atomically: true, encoding: .utf8)

        try await pod.pod.withVirtualMachineInstance { vm in
            let agent = try await vm.dialAgent()
            guard let vminitd = agent as? Vminitd else {
                throw BackendError(code: "Internal", message: "agent is not vminitd")
            }
            try await Transport.copyHostPathToGuest(
                vm: vm, vminitd: vminitd, source: hostPreparedRootfs, guestPath: guestRootfsPath)
            try await Transport.copyHostPathToGuest(
                vm: vm, vminitd: vminitd, source: hostEvidenceDir, guestPath: guestEvidencePath)
            _ = try await vminitd.stat(path: URL(fileURLWithPath: "\(guestRootfsPath)/bin/sh"))
            _ = try await vminitd.stat(path: URL(fileURLWithPath: "\(guestRootfsPath)/etc/macvz-container-identity"))
            try await vminitd.sync()
            try? await vminitd.close()
        }
    }

    // MARK: Container lifecycle

    func createContainer(_ p: [String: Any]) async throws -> [String: Any] {
        let podID = (p["podID"] as? String) ?? ""
        let name = (p["name"] as? String) ?? ""
        let token = (p["rootfsToken"] as? String) ?? ""
        if podID.isEmpty || name.isEmpty || token.isEmpty {
            throw BackendError(code: "Invalid", message: "podID, name, and rootfsToken are required")
        }
        guard let pod = pods[podID] else { throw BackendError(code: "PodNotFound", message: podID) }
        guard let rf = rootfsByToken[token], rf.podID == podID else {
            throw BackendError(code: "RootfsNotFound", message: token)
        }
        if rf.bound { throw BackendError(code: "Invalid", message: "rootfs token \(token) already bound") }
        if pod.containers.values.contains(where: { $0.name == name }) {
            throw BackendError(code: "Invalid", message: "container \(name) already exists in pod \(podID)")
        }

        let command = (p["command"] as? [String]) ?? []
        let args = (p["args"] as? [String]) ?? []
        let realCommand = command + args
        let mounts = try materializeMounts(parseMounts(p["mounts"] as? [[String: Any]], podID: podID))
        defer { cleanupMaterializedMounts(mounts) }

        // Late-sidecar evidence: the holder is excluded from pods[].containers, so a
        // running peer here means an app/sidecar is already up — the late case.
        let createdAfterPodRunning = pod.containers.values.contains { $0.phase == "Running" }
        rf.bound = true

        let n = next()
        // The late container's vminitd id MUST equal the rootfs bundle directory
        // (the sanitized rootfs token): vminitd resolves a container's OCI root.path
        // under /run/container/<containerID>, and prepareRootfs staged the rootfs at
        // /run/container/<safeToken>/rootfs. Using a different process id (the R9
        // failure mode) leaves root.path outside the container's bundle and the new
        // mount namespace cannot see it (errno=2). Matching them is the R9 convention.
        let processID = token.replacingOccurrences(of: "/", with: "_")
        let cid = "\(podID)/\(name)-\(n)"

        let spec = stagedContainerSpec(
            podID: podID, processID: processID, rootfsPath: rf.guestRootfsPath,
            evidenceGuestPath: rf.guestEvidencePath, realCommand: realCommand, extraMounts: mounts,
            redirectStdoutToStderr: !((p["logPath"] as? String) ?? "").isEmpty)

        // Hoist the staging paths into Sendable locals (URL/String) so the
        // @Sendable withVirtualMachineInstance closure does not capture the
        // non-Sendable RootfsState.
        let hostPreparedRootfs = rf.hostPreparedRootfs
        let guestRootfsPath = rf.guestRootfsPath
        let hostEvidenceDir = rf.hostEvidenceDir
        let guestEvidencePath = rf.guestEvidencePath
        let guestMounts = mounts

        // CRI-L4 follow-up (#133): when the kubelet supplied a log path, wire the
        // container's stdout/stderr to host vsock ports and stream them into the CRI
        // log file. The guest connects to these ports when the process starts.
        let logPath = (p["logPath"] as? String) ?? ""

        let logTasks: [Task<Void, Never>] = try await pod.pod.withVirtualMachineInstance { vm in
            let agent = try await vm.dialAgent()
            guard let vminitd = agent as? Vminitd else {
                throw BackendError(code: "Internal", message: "agent is not vminitd")
            }
            // Re-stage the prepared rootfs and evidence into the guest in the SAME
            // vminitd session as createProcess. The working R9 probe copies and
            // creates the process in one agent session; splitting the Copy (in
            // PrepareContainerRootfs) from createProcess (here) leaves root.path
            // unresolved for the new container (errno=2). Copying in-session mirrors
            // R9 so the staged rootfs is present when the container is created.
            try await Transport.copyHostPathToGuest(
                vm: vm, vminitd: vminitd, source: hostPreparedRootfs, guestPath: guestRootfsPath)
            try await Transport.copyHostPathToGuest(
                vm: vm, vminitd: vminitd, source: hostEvidenceDir, guestPath: guestEvidencePath)
            for mount in guestMounts where !mount.tmpfs {
                try await Transport.copyHostPathToGuest(
                    vm: vm, vminitd: vminitd, source: URL(fileURLWithPath: mount.source), guestPath: mount.guestSource)
            }
            try await vminitd.sync()

            var outPort: UInt32? = nil
            var errPort: UInt32? = nil
            var tasks: [Task<Void, Never>] = []
            if !logPath.isEmpty {
                let op = Transport.randomVsockPort()
                let ep = Transport.randomVsockPort()
                let outListener = try vm.listen(op)
                let errListener = try vm.listen(ep)
                outPort = op
                errPort = ep
                tasks.append(self.streamVsockToLog(listener: outListener, logPath: logPath, stream: "stdout"))
                tasks.append(self.streamVsockToLog(listener: errListener, logPath: logPath, stream: "stderr"))
            }
            do {
                try await vminitd.createProcess(
                    id: processID, containerID: processID,
                    stdinPort: nil, stdoutPort: outPort, stderrPort: errPort,
                    ociRuntimePath: nil, configuration: spec, options: nil)
            } catch {
                for t in tasks { t.cancel() }
                throw BackendError(code: "Internal", message: "createProcess \(processID): \(error)")
            }
            try? await vminitd.close()
            return tasks
        }

        let c = ContainerState(
            id: cid, name: name, podID: podID, rootfsToken: token, processID: processID,
            expectedIdentity: rf.expectedIdentity, createdAfterPodRunning: createdAfterPodRunning)
        c.logPath = logPath
        c.logTasks = logTasks
        pod.containers[cid] = c
        return status(pod, c)
    }

    // streamVsockToLog accepts the container's stdout/stderr vsock connection and
    // appends each line to the CRI log file as "<rfc3339nano> <stream> F <message>"
    // until the stream closes (the process exits). Runs detached for the container's
    // lifetime; cancelled on remove.
    nonisolated private func streamVsockToLog(listener: VsockListener, logPath: String, stream: String) -> Task<Void, Never> {
        Task.detached {
            guard let conn = await listener.first(where: { _ in true }) else { return }
            try? listener.finish()
            defer { conn.closeFile() }
            let url = URL(fileURLWithPath: logPath)
            try? FileManager.default.createDirectory(
                at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
            if !FileManager.default.fileExists(atPath: logPath) {
                FileManager.default.createFile(atPath: logPath, contents: nil)
            }
            guard let out = try? FileHandle(forWritingTo: url) else { return }
            defer { try? out.close() }
            _ = try? out.seekToEnd()
            let fmt = ISO8601DateFormatter()
            fmt.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            var pending = Data()
            while !Task.isCancelled {
                let chunk = conn.availableData
                if chunk.isEmpty { break }  // EOF: the process closed this stream
                pending.append(chunk)
                // Emit complete lines; keep any trailing partial for the next chunk.
                while let nl = pending.firstIndex(of: 0x0A) {
                    let lineData = pending[pending.startIndex..<nl]
                    pending.removeSubrange(pending.startIndex...nl)
                    let msg = String(decoding: lineData, as: UTF8.self)
                    let entry = "\(fmt.string(from: Date())) \(stream) F \(msg)\n"
                    if let d = entry.data(using: .utf8) { try? out.write(contentsOf: d) }
                }
            }
            if !pending.isEmpty {
                let msg = String(decoding: pending, as: UTF8.self)
                let entry = "\(fmt.string(from: Date())) \(stream) F \(msg)\n"
                if let d = entry.data(using: .utf8) { try? out.write(contentsOf: d) }
            }
        }
    }

    func startContainer(_ p: [String: Any]) async throws -> [String: Any] {
        let (pod, c) = try lookup(p)
        if c.phase == "Running" { return status(pod, c) }
        if c.phase != "Created" {
            throw BackendError(code: "Invalid", message: "container \(c.id) is \(c.phase), expected Created")
        }
        guard let rf = rootfsByToken[c.rootfsToken] else {
            throw BackendError(code: "RootfsNotFound", message: c.rootfsToken)
        }

        let observed = try await startAndVerify(pod: pod, container: c, rootfs: rf)
        c.observedIdentity = observed.identity
        if observed.identity == c.expectedIdentity && !c.expectedIdentity.isEmpty {
            c.phase = "Running"
            c.identityVerified = true
            c.message = ""
            if !observed.netns.isEmpty { pod.sandboxNamespace = observed.netns }
            return status(pod, c)
        }
        // Identity did not verify: tear the process down and leave it non-Running.
        c.phase = "Failed"
        c.exitCode = 1
        c.identityVerified = false
        c.message = "rootfs identity not verified (observed=\(observed.identity), expected=\(c.expectedIdentity))"
        try? await deleteProcess(pod: pod, processID: c.processID)
        throw BackendError(code: "IdentityUnverified", message: c.message)
    }

    private struct Observed: Sendable { let identity: String; let netns: String }

    private func parseMounts(_ raw: [[String: Any]]?, podID: String) -> [GuestMount] {
        guard let raw else { return [] }
        return raw.compactMap { item in
            let target = (item["target"] as? String) ?? ""
            if target.isEmpty { return nil }
            let source = (item["source"] as? String) ?? ""
            let tmpfs = (item["tmpfs"] as? Bool) ?? false
            let guestSource = tmpfs ? "" : "/run/macvz-mounts/\(podID)/\(safeGuestMountName(source))"
            return GuestMount(
                source: source,
                guestSource: guestSource,
                target: target,
                readOnly: (item["readOnly"] as? Bool) ?? false,
                tmpfs: tmpfs)
        }
    }

    private func materializeMounts(_ mounts: [GuestMount]) throws -> [GuestMount] {
        try mounts.map { mount in
            if mount.tmpfs || mount.source.isEmpty { return mount }
            var isDir: ObjCBool = false
            if !FileManager.default.fileExists(atPath: mount.source, isDirectory: &isDir) || !isDir.boolValue {
                return mount
            }
            let names = try FileManager.default.contentsOfDirectory(atPath: mount.source)
            if mount.readOnly && !names.contains(where: { !$0.hasPrefix(".") }) {
                return mount
            }
            let materialized = runtime.workRoot
                .appendingPathComponent("_materialized-mounts")
                .appendingPathComponent(safeGuestMountName(mount.source))
            try? FileManager.default.removeItem(at: materialized)
            try FileManager.default.createDirectory(at: materialized, withIntermediateDirectories: true)
            for name in names where !name.hasPrefix(".") {
                let src = URL(fileURLWithPath: mount.source).appendingPathComponent(name)
                let dst = materialized.appendingPathComponent(name)
                var childIsDir: ObjCBool = false
                if FileManager.default.fileExists(atPath: src.path, isDirectory: &childIsDir), childIsDir.boolValue {
                    try FileManager.default.copyItem(at: src, to: dst)
                    continue
                }
                let data = try Data(contentsOf: src)
                try data.write(to: dst, options: .atomic)
            }
            if !mount.readOnly {
                try makeWritableVolumeTree(materialized)
            }
            return GuestMount(
                source: materialized.path,
                guestSource: mount.guestSource,
                target: mount.target,
                readOnly: mount.readOnly,
                tmpfs: mount.tmpfs)
        }
    }

    private func makeWritableVolumeTree(_ root: URL) throws {
        let fm = FileManager.default
        try fm.setAttributes([.posixPermissions: 0o777], ofItemAtPath: root.path)
        guard let enumerator = fm.enumerator(at: root, includingPropertiesForKeys: [.isDirectoryKey]) else {
            return
        }
        for case let url as URL in enumerator {
            let values = try url.resourceValues(forKeys: [.isDirectoryKey])
            try fm.setAttributes(
                [.posixPermissions: values.isDirectory == true ? 0o777 : 0o666],
                ofItemAtPath: url.path)
        }
    }

    private func cleanupMaterializedMounts(_ mounts: [GuestMount]) {
        let root = runtime.workRoot.appendingPathComponent("_materialized-mounts").standardizedFileURL.path
        for mount in mounts where !mount.tmpfs && !mount.source.isEmpty {
            let source = URL(fileURLWithPath: mount.source).standardizedFileURL.path
            if source == root || source.hasPrefix(root + "/") {
                try? FileManager.default.removeItem(atPath: source)
            }
        }
        if let entries = try? FileManager.default.contentsOfDirectory(atPath: root), entries.isEmpty {
            try? FileManager.default.removeItem(atPath: root)
        }
    }

    // startAndVerify starts the staged process and bounded-waits for the identity
    // evidence the process writes to the bind-mounted handoff channel, reading it
    // back to the host (the host, not the guest, decides verification — CRI-R16).
    // It captures only Sendable locals so the @Sendable VM closure stays valid.
    private func startAndVerify(pod: PodState, container c: ContainerState, rootfs rf: RootfsState) async throws -> Observed {
        let processID = c.processID
        let cid = c.id
        let guestEvidence = "\(rf.guestEvidencePath)/identity"
        let readback = rf.hostEvidenceDir.appendingPathComponent("identity.readback")
        return try await pod.pod.withVirtualMachineInstance { vm in
            let agent = try await vm.dialAgent()
            guard let vminitd = agent as? Vminitd else {
                throw BackendError(code: "Internal", message: "agent is not vminitd")
            }
            _ = try await vminitd.startProcess(id: processID, containerID: processID)

            var lastErr = ""
            for _ in 0..<120 {
                do {
                    _ = try await vminitd.stat(path: URL(fileURLWithPath: guestEvidence))
                    try? FileManager.default.removeItem(at: readback)
                    try await Transport.copyGuestPathToHost(
                        vm: vm, vminitd: vminitd, guestPath: guestEvidence, destination: readback)
                    let text = String(decoding: (try? Data(contentsOf: readback)) ?? Data(), as: UTF8.self)
                    let parsed = parseEvidenceText(text)
                    if !parsed.identity.isEmpty {
                        try? await vminitd.close()
                        return Observed(identity: parsed.identity, netns: parsed.netns)
                    }
                } catch {
                    lastErr = "\(error)"
                }
                try await Task.sleep(nanoseconds: 250_000_000)
            }
            try? await vminitd.close()
            throw BackendError(code: "IdentityUnverified",
                message: "identity evidence did not arrive for \(cid): \(lastErr)")
        }
    }

    func stopContainer(_ p: [String: Any]) async throws -> [String: Any] {
        let podID = (p["podID"] as? String) ?? ""
        let cid = (p["containerID"] as? String) ?? ""
        let (pod, c) = try lookup(["podID": podID, "containerID": cid])
        if c.phase == "Running" {
            // Stop the workload but KEEP the staged rootfs/evidence for post-mortem
            // until RemoveContainer (CRI-R16 stop-preserves-evidence). The process
            // exit closes its stdout/stderr, so the log-capture tasks drain and end.
            try? await deleteProcess(pod: pod, processID: c.processID)
            c.phase = "Stopped"
            c.exitCode = 0
        }
        return status(pod, c)
    }

    func removeContainer(_ p: [String: Any]) async throws {
        let podID = (p["podID"] as? String) ?? ""
        let cid = (p["containerID"] as? String) ?? ""
        guard let pod = pods[podID], let c = pod.containers[cid] else { return } // idempotent
        for t in c.logTasks { t.cancel() }
        c.logTasks = []
        try? await deleteProcess(pod: pod, processID: c.processID)
        if let rf = rootfsByToken[c.rootfsToken] {
            try? FileManager.default.removeItem(at: rf.hostPreparedRootfs)
            try? FileManager.default.removeItem(at: rf.hostEvidenceDir)
            rootfsByToken.removeValue(forKey: c.rootfsToken)
        }
        pod.containers.removeValue(forKey: cid)
    }

    func statusOf(_ p: [String: Any]) throws -> [String: Any] {
        let (pod, c) = try lookup(p)
        return status(pod, c)
    }

    func cleanup(_ p: [String: Any]) async throws -> [String: Any] {
        let podID = (p["podID"] as? String) ?? ""
        guard let pod = pods[podID] else {
            return ["podID": podID, "removedContainers": 0, "removedRootfs": 0,
                    "podRemoved": false, "staleState": false]
        }
        let removedContainers = pod.containers.count
        for c in pod.containers.values {
            try? await deleteProcess(pod: pod, processID: c.processID)
        }
        var removedRootfs = 0
        for (token, rf) in rootfsByToken where rf.podID == podID {
            try? FileManager.default.removeItem(at: rf.hostPreparedRootfs)
            try? FileManager.default.removeItem(at: rf.hostEvidenceDir)
            rootfsByToken.removeValue(forKey: token)
            removedRootfs += 1
        }
        try? await pod.pod.stopContainer("holder")
        try? await pod.pod.stop()
        if let interfaceID = pod.interfaceID {
            runtime.releasePodInterface(interfaceID)
        }
        try? FileManager.default.removeItem(at: runtime.workRoot.appendingPathComponent(podID))
        pods.removeValue(forKey: podID)
        return ["podID": podID, "removedContainers": removedContainers,
                "removedRootfs": removedRootfs, "podRemoved": true, "staleState": false]
    }

    // MARK: kubelet surfaces (CRI-L4 / #133 backs these with real Pod-VM data)

    // containerLogPath returns the CRI log file the container's stdout/stderr is
    // streamed into (CRI-L4 follow-up #133). ErrInvalid when the container was
    // created without a log path (no kubelet log file to tail).
    func containerLogPath(_ p: [String: Any]) throws -> [String: Any] {
        let (_, c) = try lookup(p)
        if c.logPath.isEmpty {
            throw BackendError(code: "Invalid", message: "container \(c.id) was created without a log path")
        }
        return ["podID": c.podID, "containerID": c.id, "path": c.logPath, "format": "cri"]
    }

    // execSync runs a one-shot command inside the already-running container's
    // namespaces (CRI-L4 follow-up #133) and returns its real stdout/stderr/exit.
    // It uses the low-level vminitd exec path (createProcess with a fresh process id
    // but the container's id), wiring stdout/stderr to host vsock ports captured via
    // the existing Transport machinery. No simulation: the command actually runs.
    func execSync(_ p: [String: Any]) async throws -> [String: Any] {
        let (pod, c) = try lookup(p)
        guard c.phase == "Running" else {
            throw BackendError(code: "Invalid", message: "container \(c.id) is \(c.phase), exec requires Running")
        }
        let command = (p["command"] as? [String]) ?? []
        if command.isEmpty {
            throw BackendError(code: "Invalid", message: "exec command is required")
        }
        guard let rf = rootfsByToken[c.rootfsToken] else {
            throw BackendError(code: "RootfsNotFound", message: c.rootfsToken)
        }
        let containerID = c.processID
        let execID = "exec-\(next())-\(containerID)".replacingOccurrences(of: "/", with: "_")
        let reqTimeout = (p["timeoutSeconds"] as? Int) ?? 0
        let timeoutSeconds: Int64 = reqTimeout > 0 ? Int64(reqTimeout) : 30
        // The exec process gets a full runtime spec mirroring the container's
        // (the guest agent expects one, like the high-level execInContainer path);
        // only the process args differ.
        let spec = execSpec(
            podID: c.podID, containerProcessID: containerID,
            rootfsPath: rf.guestRootfsPath, evidenceGuestPath: rf.guestEvidencePath,
            command: command)

        let result: (out: String, err: String, code: Int32) = try await pod.pod.withVirtualMachineInstance { vm in
            let agent = try await vm.dialAgent()
            guard let vminitd = agent as? Vminitd else {
                throw BackendError(code: "Internal", message: "agent is not vminitd")
            }
            let outPort = Transport.randomVsockPort()
            let errPort = Transport.randomVsockPort()
            let outListener = try vm.listen(outPort)
            let errListener = try vm.listen(errPort)
            try await vminitd.createProcess(
                id: execID, containerID: containerID,
                stdinPort: nil, stdoutPort: outPort, stderrPort: errPort,
                ociRuntimePath: nil, configuration: spec, options: nil)
            // Start the host-side capture before launching so the guest's connect is
            // accepted, then run and wait for exit.
            async let outStr = Transport.captureVsockStream(outListener)
            async let errStr = Transport.captureVsockStream(errListener)
            _ = try await vminitd.startProcess(id: execID, containerID: containerID)
            let exit = try await vminitd.waitProcess(
                id: execID, containerID: containerID, timeoutInSeconds: timeoutSeconds)
            let out = (try? await outStr) ?? ""
            let err = (try? await errStr) ?? ""
            try? await vminitd.deleteProcess(id: execID, containerID: containerID)
            try? await vminitd.close()
            return (out, err, exit.exitCode)
        }
        return [
            "stdout": Data(result.out.utf8).base64EncodedString(),
            "stderr": Data(result.err.utf8).base64EncodedString(),
            "exitCode": Int(result.code),
        ]
    }

    // execSpec builds a full OCI runtime spec for an exec process, mirroring the
    // container's staged spec (root, mounts, namespaces, cgroup) so the guest agent
    // accepts it, but running the requested command directly instead of the staged
    // identity script.
    private func execSpec(podID: String, containerProcessID: String, rootfsPath: String,
                          evidenceGuestPath: String, command: [String]) -> ContainerizationOCI.Spec {
        let mounts = [
            ContainerizationOCI.Mount(type: "proc", source: "proc", destination: "/proc"),
            ContainerizationOCI.Mount(type: "tmpfs", source: "tmpfs", destination: "/dev", options: ["nosuid", "mode=755", "size=65536k"]),
            ContainerizationOCI.Mount(type: "devpts", source: "devpts", destination: "/dev/pts", options: ["nosuid", "noexec", "newinstance", "gid=5", "mode=0620", "ptmxmode=0666"]),
            ContainerizationOCI.Mount(type: "sysfs", source: "sysfs", destination: "/sys", options: ["nosuid", "noexec", "nodev"]),
            ContainerizationOCI.Mount(type: "tmpfs", source: "tmpfs", destination: "/dev/shm", options: ["nosuid", "noexec", "nodev", "mode=1777", "size=65536k"]),
            ContainerizationOCI.Mount(type: "bind", source: evidenceGuestPath, destination: "/macvz-evidence", options: ["rbind", "rw"]),
        ]
        let containerHostname = String("macvz-\(containerProcessID)".prefix(63))
        return ContainerizationOCI.Spec(
            version: "1.0.2",
            process: ContainerizationOCI.Process(
                args: command,
                cwd: "/",
                env: ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
            ),
            hostname: containerHostname,
            mounts: mounts,
            root: ContainerizationOCI.Root(path: rootfsPath, readonly: false),
            linux: ContainerizationOCI.Linux(
                resources: ContainerizationOCI.LinuxResources(),
                cgroupsPath: "/container/pod/\(podID)/\(containerProcessID)",
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

    // containerStats samples real cgroup CPU/memory from the Pod VM via vminitd
    // (CRI-L4 follow-up #133). CPUUsageNanoCores maps to the kubelet's UsageNanoCores
    // (a rate), so it is derived from two cumulative-usage samples taken a short
    // window apart; memory working-set is the point-in-time cgroup usage. simulated
    // is false — these are measured, not modeled.
    func containerStats(_ p: [String: Any]) async throws -> [String: Any] {
        let (pod, c) = try lookup(p)
        guard c.phase == "Running" else {
            throw BackendError(code: "Invalid", message: "container \(c.id) is \(c.phase), stats requires Running")
        }
        let cid = c.processID
        let windowNanos: UInt64 = 100_000_000  // 100ms sampling window for the CPU rate
        let sample: (cpuNanoCores: UInt64, memBytes: UInt64) = try await pod.pod.withVirtualMachineInstance { vm in
            let agent = try await vm.dialAgent()
            guard let vminitd = agent as? Vminitd else {
                throw BackendError(code: "Internal", message: "agent is not vminitd")
            }
            let s1 = try await vminitd.containerStatistics(containerIDs: [cid], categories: .all)
            let cpu1 = s1.first?.cpu?.usageUsec ?? 0
            try await Task.sleep(nanoseconds: windowNanos)
            let s2 = try await vminitd.containerStatistics(containerIDs: [cid], categories: .all)
            try? await vminitd.close()
            let cpu2 = s2.first?.cpu?.usageUsec ?? 0
            let mem = s2.first?.memory?.usageBytes ?? 0
            // usageUsec is cumulative CPU microseconds; nanocores = CPU-nanoseconds
            // consumed per wall-second over the window.
            let deltaCpuNanos = (cpu2 >= cpu1 ? cpu2 - cpu1 : 0) &* 1000
            let nanoCores = windowNanos == 0 ? 0 : (deltaCpuNanos &* 1_000_000_000) / windowNanos
            return (nanoCores, mem)
        }
        return [
            "podID": c.podID,
            "containerID": c.id,
            "timestampNanos": Int(Date().timeIntervalSince1970 * 1_000_000_000),
            "cpuUsageNanoCores": Int(sample.cpuNanoCores),
            "memoryWorkingSetBytes": Int(sample.memBytes),
            "simulated": false,
        ]
    }

    // MARK: helpers

    private func deleteProcess(pod: PodState, processID: String) async throws {
        try await pod.pod.withVirtualMachineInstance { vm in
            let agent = try await vm.dialAgent()
            guard let vminitd = agent as? Vminitd else { return }
            try? await vminitd.deleteProcess(id: processID, containerID: processID)
            try? await vminitd.close()
        }
    }

    private func lookup(_ p: [String: Any]) throws -> (PodState, ContainerState) {
        let podID = (p["podID"] as? String) ?? ""
        let cid = (p["containerID"] as? String) ?? ""
        guard let pod = pods[podID] else { throw BackendError(code: "PodNotFound", message: podID) }
        guard let c = pod.containers[cid] else { throw BackendError(code: "ContainerNotFound", message: cid) }
        return (pod, c)
    }

    private func status(_ pod: PodState, _ c: ContainerState) -> [String: Any] {
        [
            "podID": c.podID, "id": c.id, "name": c.name, "phase": c.phase,
            "exitCode": c.exitCode, "message": c.message,
            "sandboxNamespace": pod.sandboxNamespace,
            "createdAfterPodRunning": c.createdAfterPodRunning,
            "localhostReachable": c.phase == "Running",
            "expectedIdentity": c.expectedIdentity,
            "observedIdentity": c.observedIdentity,
            "identityVerified": c.identityVerified,
        ]
    }

    // stagedContainerSpec builds the OCI spec for a late container: it reports the
    // staged rootfs identity and its net namespace into the bind-mounted handoff
    // evidence channel, syncs, then execs the real workload. It shares the Pod VM's
    // network (no net namespace) so containers reach each other over localhost.
    private func stagedContainerSpec(
        podID: String,
        processID: String,
        rootfsPath: String,
        evidenceGuestPath: String,
        realCommand: [String],
        extraMounts: [GuestMount],
        redirectStdoutToStderr: Bool
    ) -> ContainerizationOCI.Spec {
        let exec: String
        if realCommand.isEmpty {
            exec = "exec sleep 2147483647"
        } else {
            let command = realCommand.map { "'" + $0.replacingOccurrences(of: "'", with: "'\"'\"'") + "'" }.joined(separator: " ")
            let stdoutRedirect = redirectStdoutToStderr ? " 1>&2" : ""
            exec = "exec " + command + stdoutRedirect
        }
        let script = """
        set -eu
        identity="$(cat /etc/macvz-container-identity 2>/dev/null || echo MISSING)"
        mkdir -p /macvz-evidence
        {
          echo "identity=${identity}"
          echo "netns=$(readlink /proc/self/ns/net 2>/dev/null || true)"
          echo "proc_root=$(readlink /proc/self/root 2>/dev/null || true)"
        } > /macvz-evidence/identity
        sync
        \(exec)
        """
        var mounts = [
            ContainerizationOCI.Mount(type: "proc", source: "proc", destination: "/proc"),
            ContainerizationOCI.Mount(type: "tmpfs", source: "tmpfs", destination: "/dev", options: ["nosuid", "mode=755", "size=65536k"]),
            ContainerizationOCI.Mount(type: "devpts", source: "devpts", destination: "/dev/pts", options: ["nosuid", "noexec", "newinstance", "gid=5", "mode=0620", "ptmxmode=0666"]),
            ContainerizationOCI.Mount(type: "sysfs", source: "sysfs", destination: "/sys", options: ["nosuid", "noexec", "nodev"]),
            ContainerizationOCI.Mount(type: "tmpfs", source: "tmpfs", destination: "/dev/shm", options: ["nosuid", "noexec", "nodev", "mode=1777", "size=65536k"]),
            ContainerizationOCI.Mount(type: "bind", source: evidenceGuestPath, destination: "/macvz-evidence", options: ["rbind", "rw"]),
        ]
        for mount in extraMounts {
            if mount.tmpfs {
                mounts.append(ContainerizationOCI.Mount(type: "tmpfs", source: "tmpfs", destination: mount.target, options: ["nosuid", "nodev", "mode=1777"]))
                continue
            }
            mounts.append(ContainerizationOCI.Mount(
                type: "bind", source: mount.guestSource, destination: mount.target,
                options: ["rbind", mount.readOnly ? "ro" : "rw"]))
        }
        // The guest runs sethostname(), which rejects names over HOST_NAME_MAX=64
        // with EINVAL. processID is a long sanitized rootfs token, so "macvz-<id>"
        // overflows; clamp to 63 chars (non-empty, valid label characters).
        let containerHostname = String("macvz-\(processID)".prefix(63))
        return ContainerizationOCI.Spec(
            version: "1.0.2",
            process: ContainerizationOCI.Process(
                args: ["/bin/sh", "-c", script],
                cwd: "/",
                env: ["PATH=\(LinuxProcessConfiguration.defaultPath)"]
            ),
            hostname: containerHostname,
            mounts: mounts,
            root: ContainerizationOCI.Root(path: rootfsPath, readonly: false),
            linux: ContainerizationOCI.Linux(
                resources: ContainerizationOCI.LinuxResources(),
                cgroupsPath: "/container/pod/\(podID)/\(processID)",
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
}
