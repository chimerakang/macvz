import Darwin
import Foundation
import Logging

// Router.swift is the main `linuxpod-helper` process after the CRI-L6-4 (#139)
// ownership inversion. The router owns the public CRI NDJSON socket, the durable
// supervisor journal, and routing — but it owns NO VM. Each Pod's live
// LinuxPod / VZVirtualMachineInstance handle lives in a separate per-Pod supervisor
// process (`linuxpod-helper supervise-pod`), so when the router restarts it can
// reconnect to the surviving supervisor sockets and keep Pods Running without a
// kubelet recreate — the true adoption that the public Apple Containerization API
// (create-only VirtualMachineManager, no VM lookup/reattach) cannot give a single
// process.
//
// Routing model:
//   CreatePod -> spawn a detached supervisor, forward CreatePod to it, journal it.
//   pod-scoped ops -> forward verbatim to that Pod's supervisor over its socket.
//   Adopt -> reconnect to the journaled supervisor: reachable => adopted:true with
//            live container status; unreachable/incomplete => adopted:false so the
//            adapter falls back through BackendLost/recreate (#136).
//   Cleanup -> tell the supervisor to tear down VM/rootfs/interface, terminate it,
//              drop the journal entry; idempotent if the supervisor is already gone.
//   startup -> scan the journal and reconnect to every supervisor: reachable bumps
//              AdoptedPods, unreachable bumps LostPods and drops the dead entry.

// SupervisorEntry records one journaled per-Pod supervisor: enough to reconnect to it
// after a router restart and to report adoption state. The live container/VM state is
// owned by the supervisor and re-fetched through Adopt/Status, not duplicated here.
struct SupervisorEntry: Codable {
    var podID: String
    var socket: String
    var pid: Int32
    var startUnix: Double
    var sandboxAddress: String
    var sandboxNamespace: String
}

struct SupervisorJournal: Codable {
    var protocolVersion = helperProtocolVersion
    var pods: [String: SupervisorEntry] = [:]
}

// RouterConfig captures everything the router needs to spawn a supervisor that owns a
// Pod VM with the same runtime configuration the operator gave the main helper.
struct RouterConfig: Sendable {
    // supervisorCommand is the argv prefix used to launch a supervisor. Defaults to
    // [selfExecutable, "supervise-pod"]; tests override it (e.g. the in-memory stub)
    // to exercise routing/journal/adopt/fallback without booting a real VM.
    var supervisorCommand: [String]
    var kernel: String
    var initfsReference: String
    var containerizationRoot: String?
    var image: String
    var workDir: String
    var rosetta: Bool
    var vmnet: Bool
}

actor RouterBackend: LineHandler {
    private let config: RouterConfig
    private let logger: Logger
    private let workRoot: URL
    private var journal: SupervisorJournal
    private var clients: [String: SupervisorClient] = [:]
    private let adoptedPods: Int
    private let lostPods: Int

    init(config: RouterConfig, logger: Logger) {
        self.config = config
        self.logger = logger
        self.workRoot = URL(fileURLWithPath: config.workDir)
        try? FileManager.default.createDirectory(at: workRoot, withIntermediateDirectories: true)

        var loaded = Self.loadJournal(from: Self.journalURL(workRoot: workRoot), logger: logger)
        // Startup adoption pass (#138/#139): reconnect to each journaled supervisor.
        // A reachable supervisor still owns its live Pod VM -> adopted. An unreachable
        // one means the supervisor died while the router was down -> lost, and its
        // stale entry is dropped so the adapter recreates rather than wedging.
        var adopted = 0
        var lost = 0
        var live: [String: SupervisorClient] = [:]
        for (podID, entry) in loaded.pods {
            let client = SupervisorClient(socketPath: entry.socket)
            if Self.probe(client) {
                live[podID] = client
                adopted += 1
            } else {
                client.close()
                loaded.pods.removeValue(forKey: podID)
                lost += 1
                logger.warning("supervisor unreachable at startup; dropping journal entry",
                    metadata: ["podID": "\(podID)", "socket": "\(entry.socket)"])
            }
        }
        self.journal = loaded
        self.clients = live
        self.adoptedPods = adopted
        self.lostPods = lost
        if adopted > 0 || lost > 0 {
            logger.info("router startup adoption pass complete",
                metadata: ["adoptedPods": "\(adopted)", "lostPods": "\(lost)"])
        }
        // Persist the pruned journal (dropped any unreachable supervisors). Called via
        // the static helper because the actor is not yet fully initialized here.
        Self.writeJournal(loaded, to: Self.journalURL(workRoot: workRoot), logger: logger)
    }

    // probe returns true when a supervisor answers a Ping over its socket: the router's
    // liveness signal for adoption. Closes the connection on failure so a dead socket
    // never leaves a half-open fd.
    private static func probe(_ client: SupervisorClient) -> Bool {
        do {
            let resp = try client.call(op: "Ping", payload: [:])
            return (resp["ok"] as? Bool) == true
        } catch {
            return false
        }
    }

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
            case "CreatePod": return try await createPod(payload)
            case "Adopt": return try adopt(payload)
            case "Cleanup": return try await cleanup(payload)
            default:
                return try forward(op: op, payload: payload)
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

    // MARK: Ping / adoption surface

    func ping() -> [String: Any] {
        // The router fronts supervisors that back the full kubelet surface set and the
        // adoption protocol, so it advertises the same capabilities and reports the
        // startup adoption pass it just completed (#138 AdoptionStatus shape).
        [
            "name": "linuxpod-helper",
            "protocolVersion": helperProtocolVersion,
            "simulated": false,
            "capabilities": ["logs": true, "exec": true, "stats": true, "adopt": true],
            "adoption": ["supported": true, "adoptedPods": adoptedPods, "lostPods": lostPods],
        ]
    }

    func adopt(_ p: [String: Any]) throws -> [String: Any] {
        let podID = (p["podID"] as? String) ?? ""
        if podID.isEmpty {
            throw BackendError(code: "Invalid", message: "podID is required")
        }
        // Reconnect on demand: a supervisor that survived the router restart answers
        // Adopt with adopted:true and its live container statuses. A transport failure
        // here means the supervisor died mid-flight, so fall through to the honest
        // adopted:false fallback rather than erroring.
        if let client = liveClient(for: podID) {
            if let resp = try? client.call(op: "Adopt", payload: p), (resp["ok"] as? Bool) == true {
                return resp
            }
            markSupervisorLost(podID, reason: "adopt")
        }
        // Journaled but unreachable: honest fallback (adopted:false, no error) so the
        // adapter routes through BackendLost/recreate and cleans the stale entry (#136).
        guard journal.pods[podID] != nil else {
            throw BackendError(code: "PodNotFound", message: podID)
        }
        return [
            "ok": true,
            "result": [
                "podID": podID,
                "adopted": false,
                "reason": "per-Pod supervisor was unreachable after helper restart; Pod VM ownership was lost",
            ],
        ]
    }

    // MARK: Pod lifecycle

    func createPod(_ p: [String: Any]) async throws -> [String: Any] {
        guard let id = p["id"] as? String, !id.isEmpty else {
            throw BackendError(code: "Invalid", message: "pod id is required")
        }
        if clients[id] != nil || journal.pods[id] != nil {
            throw BackendError(code: "Invalid", message: "pod \(id) already exists")
        }

        let socketPath = supervisorSocketPath(for: id)
        let podWorkDir = workRoot.appendingPathComponent("sup-\(safeShort(id))").path
        let pid = try spawnSupervisor(podID: id, socket: socketPath, podWorkDir: podWorkDir)

        let client = SupervisorClient(socketPath: socketPath)
        do {
            try waitForSupervisor(client: client, socketPath: socketPath)
        } catch {
            terminate(pid: pid)
            throw error
        }

        let resp: [String: Any]
        do {
            resp = try client.call(op: "CreatePod", payload: p)
        } catch {
            client.close()
            terminate(pid: pid)
            throw error
        }
        guard (resp["ok"] as? Bool) == true, let result = resp["result"] as? [String: Any] else {
            client.close()
            terminate(pid: pid)
            // Surface the supervisor's own error envelope verbatim.
            return resp
        }

        clients[id] = client
        journal.pods[id] = SupervisorEntry(
            podID: id,
            socket: socketPath,
            pid: pid,
            startUnix: Date().timeIntervalSince1970,
            sandboxAddress: (result["sandboxAddress"] as? String) ?? "",
            sandboxNamespace: (result["sandboxNamespace"] as? String) ?? "")
        persistJournal()
        return resp
    }

    func cleanup(_ p: [String: Any]) async throws -> [String: Any] {
        let podID = (p["podID"] as? String) ?? ""
        var envelope: [String: Any] = wrap([
            "podID": podID, "removedContainers": 0, "removedRootfs": 0,
            "podRemoved": false, "staleState": false,
        ])
        // Best-effort: drive the supervisor's own Cleanup (tears down VM/rootfs/
        // interface) before terminating it, so VM teardown is owned where the VM is.
        if let client = liveClient(for: podID) {
            if let resp = try? client.call(op: "Cleanup", payload: p) {
                envelope = resp
            }
            client.close()
        }
        clients.removeValue(forKey: podID)
        if let entry = journal.pods[podID] {
            terminate(pid: entry.pid)
            try? FileManager.default.removeItem(atPath: entry.socket)
            try? FileManager.default.removeItem(at: workRoot.appendingPathComponent("sup-\(safeShort(podID))"))
            journal.pods.removeValue(forKey: podID)
            persistJournal()
        }
        return envelope
    }

    // MARK: Generic routing

    private func forward(op: String, payload: [String: Any]) throws -> [String: Any] {
        let podID = (payload["podID"] as? String) ?? ""
        if podID.isEmpty {
            throw BackendError(code: "Invalid", message: "podID is required")
        }
        guard let client = liveClient(for: podID) else {
            // No reachable supervisor: a routed status that fails is exactly the signal
            // the reconciler uses to mark the Pod BackendLost (#139 AC).
            throw BackendError(code: "PodNotFound", message: podID)
        }
        do {
            return try client.call(op: op, payload: payload)
        } catch let e as BackendError {
            // Transport failure mid-op: the supervisor died. Drop the dead client so a
            // later Adopt/status falls back, and surface the failure to the caller.
            markSupervisorLost(podID, reason: op)
            throw e
        }
    }

    // liveClient returns a connected client for the pod, reconnecting from the journal
    // when the router holds no live handle (e.g. right after its own restart) and
    // verifying liveness with a Ping. Returns nil when the supervisor is gone.
    private func liveClient(for podID: String) -> SupervisorClient? {
        if let client = clients[podID], client.isConnected {
            return client
        }
        guard let entry = journal.pods[podID] else { return nil }
        let client = clients[podID] ?? SupervisorClient(socketPath: entry.socket)
        if Self.probe(client) {
            clients[podID] = client
            return client
        }
        markSupervisorLost(podID, reason: "probe")
        return nil
    }

    private func markSupervisorLost(_ podID: String, reason: String) {
        clients.removeValue(forKey: podID)?.close()
        guard let entry = journal.pods.removeValue(forKey: podID) else { return }
        terminate(pid: entry.pid)
        try? FileManager.default.removeItem(atPath: entry.socket)
        try? FileManager.default.removeItem(at: workRoot.appendingPathComponent("sup-\(safeShort(podID))"))
        persistJournal()
        logger.warning("supervisor lost; dropping journal entry",
            metadata: ["podID": "\(podID)", "reason": "\(reason)", "pid": "\(entry.pid)"])
    }

    // MARK: Supervisor process management

    private func spawnSupervisor(podID: String, socket: String, podWorkDir: String) throws -> Int32 {
        var argv = config.supervisorCommand
        argv += [
            "--socket", socket,
            "--pod-id", podID,
            "--work-dir", podWorkDir,
            "--kernel", config.kernel,
            "--initfs-reference", config.initfsReference,
            "--image", config.image,
        ]
        if let root = config.containerizationRoot {
            argv += ["--containerization-root", root]
        }
        if config.rosetta { argv.append("--rosetta") }
        if config.vmnet { argv.append("--vmnet") }

        guard let exe = argv.first else {
            throw BackendError(code: "Internal", message: "empty supervisor command")
        }
        let process = Process()
        process.executableURL = URL(fileURLWithPath: exe)
        process.arguments = Array(argv.dropFirst())
        // The supervisor must outlive the router: it calls setsid() at startup to
        // detach from the router's process group, and we never wait() on it here, so a
        // SIGTERM/SIGKILL to the router alone leaves the Pod VM running for adoption.
        do {
            try process.run()
        } catch {
            throw BackendError(code: "Internal", message: "spawn supervisor for \(podID): \(error)")
        }
        logger.info("spawned per-Pod supervisor",
            metadata: ["podID": "\(podID)", "pid": "\(process.processIdentifier)", "socket": "\(socket)"])
        return process.processIdentifier
    }

    private func waitForSupervisor(client: SupervisorClient, socketPath: String) throws {
        // Poll for the supervisor's socket + a successful Ping. Bounded so a supervisor
        // that fails to boot surfaces as a CreatePod error instead of hanging kubelet.
        for _ in 0..<300 {
            if FileManager.default.fileExists(atPath: socketPath), Self.probe(client) {
                return
            }
            usleep(100_000)  // 100ms; up to ~30s total for VM-backed supervisors
        }
        throw BackendError(code: "Internal", message: "supervisor did not become ready: \(socketPath)")
    }

    private func terminate(pid: Int32) {
        guard pid > 0 else { return }
        kill(pid, SIGTERM)
        // Reap if it is our child (no-op for a reparented supervisor after a restart).
        var status: Int32 = 0
        _ = waitpid(pid, &status, WNOHANG)
    }

    // MARK: Journal persistence

    private func supervisorSocketPath(for podID: String) -> String {
        workRoot.appendingPathComponent("s-\(safeShort(podID)).sock").path
    }

    private func safeShort(_ podID: String) -> String {
        let mapped = podID.unicodeScalars.map { scalar -> Character in
            CharacterSet.alphanumerics.contains(scalar) ? Character(scalar) : "-"
        }
        return String(String(mapped).prefix(24))
    }

    private static func journalURL(workRoot: URL) -> URL {
        workRoot.appendingPathComponent("supervisor-journal.json")
    }

    private static func loadJournal(from url: URL, logger: Logger) -> SupervisorJournal {
        guard FileManager.default.fileExists(atPath: url.path) else { return SupervisorJournal() }
        do {
            let data = try Data(contentsOf: url)
            let journal = try JSONDecoder().decode(SupervisorJournal.self, from: data)
            if journal.protocolVersion == helperProtocolVersion { return journal }
            logger.warning("ignoring supervisor journal with mismatched protocol",
                metadata: ["path": "\(url.path)", "version": "\(journal.protocolVersion)"])
        } catch {
            logger.warning("ignoring unreadable supervisor journal",
                metadata: ["path": "\(url.path)", "error": "\(error)"])
        }
        return SupervisorJournal()
    }

    private func persistJournal() {
        Self.writeJournal(journal, to: Self.journalURL(workRoot: workRoot), logger: logger)
    }

    private static func writeJournal(_ journal: SupervisorJournal, to url: URL, logger: Logger) {
        do {
            try JSONEncoder().encode(journal).write(to: url, options: .atomic)
        } catch {
            logger.error("failed to persist supervisor journal",
                metadata: ["path": "\(url.path)", "error": "\(error)"])
        }
    }
}
