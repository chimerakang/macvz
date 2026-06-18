# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project status

This repository is at the **design / pre-MVP stage**. Currently it contains only `README.md` (a Chinese-language architecture document). There is no `go.mod`, no source code, and no git repository yet. When implementing, follow the layout and phasing below, which are derived from the README.

## What this is

MacVz-Cluster (working name) is a lightweight, minimal distributed orchestration system for **Apple Silicon (M-series)** Macs. Instead of Docker Desktop's single large Linux VM, it calls macOS's native **Virtualization.framework** directly to run **one micro-VM per pod/container** — high density, low latency, low power, full use of unified-memory bandwidth.

It is written entirely in **Go**, deliberately avoids K8s's `etcd` and CRI abstractions, and prefers integrating mature open-source libraries over reinventing them.

## Architecture (Master–Worker / Control Plane–Data Plane)

- **mvz-master** (control plane): API Server (REST or gRPC) that accepts deployment manifests (YAML); Scheduler that monitors each node's CPU/RAM and dispatches workloads to the node with the most free RAM (in-memory scheduling, no etcd).
- **mvz-agent** (data plane, one resident per Mac mini): VM Lifecycle Manager that uses `github.com/Code-Hex/vz` to start/stop/destroy Linux micro-VMs in seconds; Image Puller that downloads standard OCI/Docker images and unpacks them into a RootFS disk image the micro-VM can boot.
- **mvz-net** (mesh networking): embedded virtual NIC built on WireGuard-Go that connects micro-VMs across hosts into a flat, encrypted P2P network.

## Tech stack

- **Go** — whole system.
- **github.com/Code-Hex/vz** — Go binding for Apple's Virtualization.framework (the macOS virtualization bridge).
- **gRPC / Protocol Buffers** — Master↔Agent RPC.
- **go-yaml** — parse user-defined cluster deployment configs (Compose-like).
- **WireGuard (Go-native)** — cross-machine encrypted mesh, the CNI replacement.

## Planned project layout (standard Go layout)

```
macvz-cluster/
├── cmd/
│   ├── mvz-master/main.go   # control-plane entrypoint
│   └── mvz-agent/main.go    # worker-node agent entrypoint
├── pkg/
│   ├── api/                 # gRPC protobuf defs + generated code
│   ├── config/              # YAML config parsing
│   ├── scheduler/           # cluster resource scheduling
│   ├── virt/                # Code-Hex/vz VM lifecycle (vm.go) + OCI image handling (image.go)
│   └── network/             # WireGuard virtual network setup
├── deployments/             # example deployment YAML
└── go.mod
```

## Development phases (build in this order)

1. **Single-host VM MVP** — initialize the project and `go.mod`; bring up `pkg/virt` with Code-Hex/vz so Go can boot an Alpine micro-VM in seconds; build a tool that converts a Docker image into a RootFS disk image. **Get "Go drives a native VM" working on one machine before adding networking or multi-host.**
2. **Control plane + gRPC** — define `cluster.proto` with `StartPod`, `StopPod`, `GetNodeStatus`; implement in-memory scheduling in `mvz-master` (prefer node with most free RAM); have `mvz-agent` serve gRPC and run single-host VM ops.
3. **Cross-host mesh** — each `mvz-agent` self-assigns a cluster-internal IP range on startup; integrate WireGuard for host-to-host tunnels; configure Virtualization.framework networking (NAT/bridged) so VM traffic routes correctly into the WireGuard interface, letting a VM on host A ping a VM on host B.

## Notes

- This is a macOS-only project — Virtualization.framework requires building and running on Apple Silicon macOS, with the appropriate entitlements/signing for the `vz` binding.
- Global security rules in `~/.claude/CLAUDE.md` apply (no hardcoded secrets/config, env vars for credentials, etc.).


<claude-mem-context>

</claude-mem-context>