// Package wireguard brings up the encrypted host-to-host mesh that carries Pod
// traffic between MacVz nodes (issue #21, milestone P3).
//
// Each Apple Silicon Mac runs one WireGuard interface. Peers are other MacVz
// nodes; a peer's AllowedIPs is its Kubernetes-assigned Pod CIDR (see
// pkg/network IPAM, #20), so traffic destined for a remote Pod is encrypted and
// tunnelled to the Mac that hosts it. Host routes for those CIDRs are installed
// through the interface so the kernel forwards Pod-bound packets into the tunnel.
//
// macOS has no in-kernel WireGuard, so the Mesh drives the userspace toolchain
// shipped by Homebrew's wireguard-tools (`wg`, `wg-quick`) plus `route`, exactly
// as pkg/runtime/container drives the apple/container CLI. All command execution
// goes through the runner interface, so the orchestration logic is unit-tested
// against a fake without touching the host network.
//
// The MVP takes peer identity, endpoints, and Pod CIDRs from configuration;
// reconciliation (`wg syncconf`) applies peer additions and removals without
// tearing the interface down. Dynamic peer discovery via Kubernetes Node
// metadata is layered on in later P3 work.
package wireguard
