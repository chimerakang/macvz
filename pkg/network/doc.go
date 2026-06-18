// Package network provides cross-host Pod connectivity for MacVz: a WireGuard
// mesh between Macs (P3, #21) plus Pod IPAM coordinated through Kubernetes, so
// Pods scheduled on different nodes reach each other directly.
//
// The Pod IPAM piece (#20) is implemented here as PodIPAM. Each macvz-kubelet
// allocates Pod IPs only from its own node's Kubernetes-assigned Spec.PodCIDR;
// because Kubernetes hands every node a disjoint CIDR, two nodes can never
// allocate the same address for different Pods. See PodIPAM for the full MVP
// allocation model, and docs/NETWORKING.md for operational behavior.
package network
