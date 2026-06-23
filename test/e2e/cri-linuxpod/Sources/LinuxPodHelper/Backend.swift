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
// must equal pkg/runtime/linuxpod ProtocolVersion (currently 3).
let helperProtocolVersion = 3

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
            case "ContainerLogPath": return wrap(try containerLogPath(payload))
            case "ExecSync": return wrap(try execSync(payload))
            case "ContainerStats": return wrap(try containerStats(payload))
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
        // The real helper backs the core LinuxPod lifecycle + identity (CRI-L1).
        // The kubelet surfaces (logs/exec/stats) are owned by CRI-L4 (#129); the real
        // helper advertises them false until then, so the adapter calls only what it
        // truly backs and those ops return Unsupported rather than faking results.
        [
            "name": "linuxpod-helper",
            "protocolVersion": helperProtocolVersion,
            "simulated": false,
            "capabilities": ["logs": false, "exec": false, "stats": false],
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
        for applet in ["cat", "grep", "ls", "mkdir", "readlink", "sleep", "sync", "tr", "wget"] {
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
            evidenceGuestPath: rf.guestEvidencePath, realCommand: realCommand)

        // Hoist the staging paths into Sendable locals (URL/String) so the
        // @Sendable withVirtualMachineInstance closure does not capture the
        // non-Sendable RootfsState.
        let hostPreparedRootfs = rf.hostPreparedRootfs
        let guestRootfsPath = rf.guestRootfsPath
        let hostEvidenceDir = rf.hostEvidenceDir
        let guestEvidencePath = rf.guestEvidencePath

        try await pod.pod.withVirtualMachineInstance { vm in
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
            try await vminitd.sync()
            do {
                try await vminitd.createProcess(
                    id: processID, containerID: processID,
                    stdinPort: nil, stdoutPort: nil, stderrPort: nil,
                    ociRuntimePath: nil, configuration: spec, options: nil)
            } catch {
                throw BackendError(code: "Internal", message: "createProcess \(processID): \(error)")
            }
            try? await vminitd.close()
        }

        let c = ContainerState(
            id: cid, name: name, podID: podID, rootfsToken: token, processID: processID,
            expectedIdentity: rf.expectedIdentity, createdAfterPodRunning: createdAfterPodRunning)
        pod.containers[cid] = c
        return status(pod, c)
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
            // until RemoveContainer (CRI-R16 stop-preserves-evidence).
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

    // MARK: kubelet surfaces (CRI-L4 / #129 owns backing these; honest Unsupported here)

    func containerLogPath(_ p: [String: Any]) throws -> [String: Any] {
        throw BackendError(code: "Unsupported", message: "linuxpod-helper does not yet back container logs (CRI-L4 #129)")
    }

    func execSync(_ p: [String: Any]) throws -> [String: Any] {
        throw BackendError(code: "Unsupported", message: "linuxpod-helper does not yet back exec (CRI-L4 #129)")
    }

    func containerStats(_ p: [String: Any]) throws -> [String: Any] {
        throw BackendError(code: "Unsupported", message: "linuxpod-helper does not yet back stats (CRI-L4 #129)")
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
    private func stagedContainerSpec(podID: String, processID: String, rootfsPath: String, evidenceGuestPath: String, realCommand: [String]) -> ContainerizationOCI.Spec {
        let exec: String
        if realCommand.isEmpty {
            exec = "exec sleep 2147483647"
        } else {
            exec = "exec " + realCommand.map { "'" + $0.replacingOccurrences(of: "'", with: "'\"'\"'") + "'" }.joined(separator: " ")
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
        let mounts = [
            ContainerizationOCI.Mount(type: "proc", source: "proc", destination: "/proc"),
            ContainerizationOCI.Mount(type: "tmpfs", source: "tmpfs", destination: "/dev", options: ["nosuid", "mode=755", "size=65536k"]),
            ContainerizationOCI.Mount(type: "devpts", source: "devpts", destination: "/dev/pts", options: ["nosuid", "noexec", "newinstance", "gid=5", "mode=0620", "ptmxmode=0666"]),
            ContainerizationOCI.Mount(type: "sysfs", source: "sysfs", destination: "/sys", options: ["nosuid", "noexec", "nodev"]),
            ContainerizationOCI.Mount(type: "tmpfs", source: "tmpfs", destination: "/dev/shm", options: ["nosuid", "noexec", "nodev", "mode=1777", "size=65536k"]),
            ContainerizationOCI.Mount(type: "bind", source: evidenceGuestPath, destination: "/macvz-evidence", options: ["rbind", "rw"]),
        ]
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
