// swift-tools-version:6.0
import PackageDescription

// LinuxPodHelperStub is the Swift side of the CRI-R17 LinuxPod backend contract
// (#124). It implements the pkg/runtime/linuxpod NDJSON protocol over a unix
// socket with an in-memory model that mirrors the Go FakeBackend, proving the
// contract is implementable in Swift with only Foundation/Darwin — no gRPC, no
// code generation, and no dependency on Apple Containerization. A production
// helper replaces the in-memory model with real LinuxPod calls while keeping this
// exact wire protocol, so the Go adapter does not change.
let package = Package(
    name: "LinuxPodHelperStub",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(
            name: "LinuxPodHelperStub",
            path: "Sources/LinuxPodHelperStub"
        )
    ]
)
