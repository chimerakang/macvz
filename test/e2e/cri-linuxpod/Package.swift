// swift-tools-version: 6.2

import PackageDescription

let package = Package(
    name: "macvz-linuxpod-poc",
    platforms: [.macOS("26.0")],
    products: [
        .executable(name: "linuxpod-shared-namespace-poc", targets: ["LinuxPodSharedNamespacePoC"])
    ],
    dependencies: [
        .package(url: "https://github.com/apple/swift-argument-parser.git", from: "1.7.0"),
        .package(url: "https://github.com/apple/swift-log.git", from: "1.10.1"),
        .package(path: "containerization"),
    ],
    targets: [
        .executableTarget(
            name: "LinuxPodSharedNamespacePoC",
            dependencies: [
                .product(name: "ArgumentParser", package: "swift-argument-parser"),
                .product(name: "Logging", package: "swift-log"),
                .product(name: "Containerization", package: "containerization"),
                .product(name: "ContainerizationEXT4", package: "containerization"),
                .product(name: "ContainerizationExtras", package: "containerization"),
                .product(name: "ContainerizationOCI", package: "containerization"),
            ]
        )
    ]
)
