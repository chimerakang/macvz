// swift-tools-version: 6.2

import PackageDescription

let package = Package(
    name: "macvz-linuxpod-poc",
    platforms: [.macOS("26.0")],
    products: [
        .executable(name: "linuxpod-shared-namespace-poc", targets: ["LinuxPodSharedNamespacePoC"]),
        // The real LinuxPod backend helper (CRI-L1, #126): speaks the
        // pkg/runtime/linuxpod NDJSON protocol over a unix socket and drives a real
        // Apple Containerization LinuxPod + the R9/R16 late-rootfs identity primitive,
        // replacing the in-memory stub's model while keeping the wire protocol.
        .executable(name: "linuxpod-helper", targets: ["LinuxPodHelper"]),
    ],
    dependencies: [
        .package(url: "https://github.com/apple/swift-argument-parser.git", from: "1.7.0"),
        .package(url: "https://github.com/apple/swift-log.git", from: "1.10.1"),
        .package(url: "https://github.com/apple/swift-nio.git", from: "2.80.0"),
        .package(url: "https://github.com/apple/swift-system.git", from: "1.6.4"),
        .package(path: "containerization"),
    ],
    targets: [
        .executableTarget(
            name: "LinuxPodSharedNamespacePoC",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
                .product(name: "Logging", package: "swift-log"),
                .product(name: "Containerization", package: "containerization"),
                .product(name: "ContainerizationArchive", package: "containerization"),
                .product(name: "ContainerizationEXT4", package: "containerization"),
                .product(name: "ContainerizationExtras", package: "containerization"),
                .product(name: "ContainerizationOCI", package: "containerization"),
                .product(name: "NIOCore", package: "swift-nio"),
                .product(name: "NIOPosix", package: "swift-nio"),
                .product(name: "SystemPackage", package: "swift-system"),
            ]
        ),
        .executableTarget(
            name: "LinuxPodHelper",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
                .product(name: "Logging", package: "swift-log"),
                .product(name: "Containerization", package: "containerization"),
                .product(name: "ContainerizationArchive", package: "containerization"),
                .product(name: "ContainerizationEXT4", package: "containerization"),
                .product(name: "ContainerizationExtras", package: "containerization"),
                .product(name: "ContainerizationOCI", package: "containerization"),
            ]
        ),
    ]
)
