# MacVz Networking

This document describes MacVz cross-host networking (P3):

- [Pod IPAM (MVP)](#pod-ipam-mvp) — issue #20.
- [WireGuard mesh (MVP)](#wireguard-mesh-mvp) — issue #21.
- [Micro-VM network attach (MVP)](#micro-vm-network-attach-mvp) — issue #22.
- [Pod status & Service resolution (MVP)](#pod-status--service-resolution-mvp) — issue #23.
- [Port-forward (MVP)](#port-forward-mvp) — issue #24.
- [Privileged network helper (`macvz-netd`)](#privileged-network-helper-macvz-netd) — issues #38–#40.
- [End-to-end: a Service across two Macs](#end-to-end-a-service-across-two-macs).

For the operator runbook that ties these layers into one ordered setup +
verification + recovery procedure (issue #44), see
[PRIVILEGED_NETWORKING.md](PRIVILEGED_NETWORKING.md). This document explains how
each layer works; that one is the step-by-step guide to standing it up on a fresh
Mac and recovering it when routes, pf rules, or the helper go stale.

Together these give a Pod on one Mac an L3 path to a Pod hosted on another Mac:
IPAM assigns the address, the mesh routes the CIDR between hosts, the network
attach connects each micro-VM to that path, and the status reporting publishes
the address and readiness so Kubernetes Services resolve normally.

## Pod IPAM (MVP)

This section describes the MVP Pod IP address management (IPAM) behavior for
MacVz, delivered in issue #20.

## Allocation model

MacVz does **not** invent a new cluster-wide IPAM coordinator. It reuses state
Kubernetes already owns:

- When `kube-controller-manager` runs with `--allocate-node-cidrs=true` and a
  `--cluster-cidr`, Kubernetes assigns every node a **disjoint** `Spec.PodCIDR`
  (e.g. `10.244.1.0/24` on one Mac, `10.244.2.0/24` on another).
- Each `macvz-kubelet` allocates Pod IPs **only** from its own node's PodCIDR.

Because the per-node CIDRs are disjoint and Kubernetes-owned, two MacVz nodes can
never produce the same Pod IP for different Pods. Collision avoidance is a
property of the CIDR partitioning, not of any peer-to-peer negotiation.

### Within a node

`pkg/network.PodIPAM` hands out host addresses from the node's CIDR in order:

- The **network address** (offset 0) is reserved.
- The **first host address** (offset 1) is reserved as the gateway, matching
  typical CNI bridge conventions; the data path using it is wired in #22.
- For IPv4, the **broadcast address** (last) is reserved.
- Remaining addresses are allocated to Pods. IPv6 CIDRs are supported.

Allocation is keyed by `namespace/name` and is **idempotent**: Virtual Kubelet
may re-issue `CreatePod`, and the same Pod always gets the same IP. Released
addresses return to the pool and are reused.

## Lifecycle

- **Create** — `Provider.CreatePod` allocates an IP and records it as the Pod's
  `status.PodIP`. If the Pod fails to start (or is a terminal failure such as an
  architecture mismatch), the IP is released so it is not leaked.
- **Delete** — `Provider.DeletePod` releases the IP back to the pool.
- **Restart recovery** — on startup, `macvz-kubelet` lists the Pods assigned to
  this node and calls `Provider.RecoverAllocations`, which **reserves** the
  `status.PodIP` already recorded in Kubernetes. A restarted provider therefore
  neither leaks addresses nor reassigns a live Pod's IP. Kubernetes is the
  durable record; the in-memory allocator is rebuilt from it.

## Configuration

| Field | Default | Meaning |
| --- | --- | --- |
| `node.podCIDR` | _(empty)_ | Pod CIDR for this node. When empty, MacVz waits for and uses the Kubernetes-assigned `Node.Spec.PodCIDR`; with `privilegedHelperSocket` + `podNetwork.enabled`, set it explicitly so the helper's static policy can validate pf rules. |

Set `node.podCIDR` explicitly on clusters that do **not** allocate node CIDRs,
and also on helper-managed Pod-network nodes. In both cases give each node a
disjoint range manually to preserve collision avoidance.

### Graceful degradation

If no Pod CIDR is available (node-CIDR allocation is off and no override is set),
coordinated IPAM is disabled with a log message and Pods still run — the
`status.PodIP` is then derived from the runtime-reported address. This keeps
single-host development working without cluster-wide IPAM.

### Pod IPAM tests

- `pkg/network/ipam_test.go` — allocation, release/reuse, idempotency, reserve
  (restart recovery), exhaustion, out-of-range rejection, IPv6, concurrency, and
  the cross-node collision-avoidance property.
- `pkg/provider/ipam_test.go` — Pod IP assignment on create, release on delete,
  release on failure, recovery from existing Kubernetes Pods, and the no-IPAM
  fallback to the runtime-reported address.

## WireGuard mesh (MVP)

This section describes the encrypted host-to-host mesh that carries Pod traffic
between Macs, delivered in issue #21 (`pkg/network/wireguard`).

### Model

Each Mac runs **one WireGuard interface**. Every other MacVz node is a *peer*,
and a peer's `AllowedIPs` is its **Pod CIDR** (from [Pod IPAM](#pod-ipam-mvp))
plus its mesh address. Traffic for a remote Pod is therefore encrypted and
tunnelled to the Mac that hosts it, and a **host route** for that CIDR is
installed through the interface so the kernel forwards Pod-bound packets into the
tunnel.

macOS has no in-kernel WireGuard, so MacVz drives the userspace toolchain from
Homebrew's `wireguard-tools` (`wg`, `wireguard-go`) plus the system `route`,
`ifconfig`, and `pkill`. Bring-up runs, in order:

1. `wireguard-go <iface>` — create the userspace `utun` interface.
2. `wg setconf <iface> /dev/stdin` — apply keys and peers (config via stdin, no
   file on disk).
3. `ifconfig <iface> inet <addr> <addr> alias` + `ifconfig <iface> up` — assign
   the mesh address and bring the link up.
4. `route -q -n add -inet <cidr> -interface <iface>` — one route per peer CIDR.

Interface creation and route installation tolerate "already exists" / "File
exists", so bring-up is **idempotent** and safe to re-run.

Teardown removes peer routes, brings the interface down, then stops only the
matching `wireguard-go <iface>` process before attempting a best-effort
`ifconfig <iface> destroy`. Stopping the process is required on macOS because
userspace WireGuard owns the `utun` lifecycle.

`macvz-kubelet` builds its Kubernetes clientset only after this data-plane
bring-up and the Pod-network route cleanup complete, then probes
`/version` before starting the Virtual Kubelet controllers. That keeps transient
host-route churn from poisoning a long-lived client-go transport.

### Reconcile without restart

When nodes join or leave, `Mesh.Sync` re-applies the peer set with
`wg syncconf` (which adds/removes peers atomically) and adds/removes only the
affected host routes — the interface is never torn down, so unrelated peers keep
their tunnels. `Mesh.Down` removes routes and destroys the interface on shutdown
(best-effort).

#### Adding or removing a node (operator workflow, #42)

A running `macvz-kubelet` reconciles its mesh peers from the config on **SIGHUP**,
so adding or removing a MacVz node needs no restart and never disturbs Pods
already running on this host:

1. Edit the `mesh.peers` list in this node's config — add the joining node's
   `publicKey` / `endpoint` / `podCIDR`, or delete a departing node's entry.
2. Signal the kubelet to reload: `kill -HUP <macvz-kubelet pid>`.

On the signal the kubelet reloads the config and calls `Mesh.Sync` with the new
peer set. Expectations:

- **Adding a peer** installs its WireGuard entry and Pod-CIDR route; existing
  peers' tunnels and routes, and the local Pod network attachments (the `pf`
  anchor is a separate subsystem the mesh never touches), are left untouched —
  no Pod loses connectivity.
- **Removing a peer** removes its WireGuard entry and its route.
- **Reconciliation is idempotent** — a SIGHUP with no peer changes re-applies the
  same config and installs/removes nothing.
- The reload is validated and **fails safe**: an invalid config, an unparseable
  peer key, or a config that disables the mesh is rejected with a logged error
  and the **running peer set is kept**. (Disabling the mesh entirely still
  requires a restart.) Set `mesh.peers` to empty to drop all peers.

This requires the kubelet to have been started with `--config <path>` (the path
it reloads); without it the mesh comes up once and the SIGHUP reconciler is not
started. The same private key and interface are reused, so peer changes never
rotate this node's identity.

### Peer identity & keys

Each node's private key lives in `mesh.privateKeyFile`, generated (mode 0600) on
first start if absent, giving the node a **stable identity across restarts**
without keys ever appearing in config or git. Share each node's **public key**
(logged at startup, or `wg pubkey < key`) with its peers.

#### `macvz-mesh` — automated key/config exchange (#55)

`macvz-mesh` (build with `make mesh`) automates the error-prone parts of mesh
setup so operators **never copy private keys by hand** and never hand-edit base64
keys into config:

```sh
# 1. On each node: generate/load its stable private key, print the public key.
#    Idempotent — re-running loads the existing key, it does not rotate it.
macvz-mesh keygen --key /var/lib/macvz/wg.key

# 2. On each node: export its shareable identity (public key, endpoint, mesh
#    address, Pod CIDR). The document contains NO private key — safe to copy.
macvz-mesh export --config /etc/macvz/config.yaml --out macvz-a.meta.yaml

# 3. Collect the other nodes' metadata, then render peer entries to paste under
#    `mesh:` in THIS node's config (or use --format wg for raw [Peer] blocks).
macvz-mesh peer macvz-b.meta.yaml macvz-c.meta.yaml
```

`export` reuses `mesh.privateKeyFile`, generating the key on first call, so the
same key the kubelet later loads is the one whose public key is exported — the
exported document is consistent with the running node by construction. The
rendered `peers:` fragment is the canonical config form: paste it in and reload
with SIGHUP (see above), no restart required.

#### Key rotation

A node's identity is its private key file; rotation is deliberate, never
automatic:

- **Re-running `keygen`/`export`/the kubelet never rotates the key** — an
  existing `mesh.privateKeyFile` is loaded as-is. This is what makes identity
  stable across restarts.
- **To rotate**, delete (or move) the node's `mesh.privateKeyFile` and re-run
  `macvz-mesh keygen` (or restart the kubelet) to mint a fresh key. The public
  key changes, so you must **re-export** that node's metadata and **re-render +
  re-apply** the peer entry on every other node (edit `mesh.peers`, then
  `kill -HUP`). Until every peer has the new public key, handshakes to the
  rotated node fail — rotate during a maintenance window or roll peers one at a
  time.
- The private key file is created mode 0600 and stays on the node; only public
  metadata is ever exchanged, so a leaked metadata file is not a secret exposure.

### Prerequisites

```sh
brew install wireguard-tools   # provides wg, wg-quick, wireguard-go
```

`wireguard-go`, `ifconfig`, and `route` require elevated privileges to create
interfaces and edit the routing table; run `macvz-kubelet` accordingly.

### Configuration

Add a `mesh` stanza to each node's config. The mesh is **disabled by default**
so single-host development needs no setup.

| Field | Meaning |
| --- | --- |
| `mesh.enabled` | Turn the mesh on. |
| `mesh.interface` | Interface name to manage (e.g. `utun7`). |
| `mesh.privateKeyFile` | Path to this node's private key (auto-generated if absent). |
| `mesh.address` | This node's mesh address, CIDR (e.g. `10.99.0.1/32`). |
| `mesh.listenPort` | WireGuard UDP port (default `51820`). |
| `mesh.mtu` | Optional interface MTU. |
| `mesh.peers[]` | `name`, `publicKey`, `endpoint` (host:port), `podCIDR`, `address`, `persistentKeepalive`. |

### Two-node example

**mac-01** (`10.99.0.1`, Pod CIDR `10.244.1.0/24`):

```yaml
mesh:
  enabled: true
  interface: utun7
  privateKeyFile: /var/lib/macvz/wg.key
  address: 10.99.0.1/32
  listenPort: 51820
  peers:
    - name: mac-02
      publicKey: <mac-02 public key>
      endpoint: 192.168.1.20:51820
      podCIDR: 10.244.2.0/24
      address: 10.99.0.2/32
      persistentKeepalive: 25
```

**mac-02** (`10.99.0.2`, Pod CIDR `10.244.2.0/24`):

```yaml
mesh:
  enabled: true
  interface: utun7
  privateKeyFile: /var/lib/macvz/wg.key
  address: 10.99.0.2/32
  listenPort: 51820
  peers:
    - name: mac-01
      publicKey: <mac-01 public key>
      endpoint: 192.168.1.10:51820
      podCIDR: 10.244.1.0/24
      address: 10.99.0.1/32
      persistentKeepalive: 25
```

### Verifying

```sh
wg show utun7                       # handshake established with the peer
netstat -rn -f inet | grep utun7   # routes for remote Pod CIDRs are present
ping 10.99.0.2                      # reach the peer across the tunnel
```

### Mesh tests

- `pkg/network/wireguard/key_test.go` — key generation/clamping, base64 round
  trip, public-key derivation, and persistent load-or-create.
- `pkg/network/wireguard/config_test.go` — config validation, `wg-quick` vs `wg`
  rendering, deterministic output, and route-target de-duplication.
- `pkg/network/wireguard/mesh_test.go` — bring-up command sequence, idempotent
  tolerance of benign errors, fatal-error propagation, peer/route reconcile via
  `Sync`, and best-effort `Down`.
- `pkg/config/mesh_test.go` — mesh config validation and translation into the
  `wireguard` package's interface config.
- `pkg/config/metadata_test.go` — node metadata export (public-key derivation, no
  private-key leakage), marshal round-trip, and peer-snippet rendering that
  round-trips back into a valid config (`macvz-mesh` automation, #55).

## Micro-VM network attach (MVP)

This section describes how each `apple/container` micro-VM is connected to the
Pod network path, delivered in issue #22 (`pkg/network/podnet`). It is the piece
that makes a Pod reachable at its [assigned Pod IP](#pod-ipam-mvp) and routable
across the [mesh](#wireguard-mesh-mvp).

### The apple/container constraint

`apple/container` attaches each micro-VM to a **vmnet-backed** network and gives
it a **host-only address** (e.g. `192.168.64.x`) over DHCP. The CLI does **not**
let MacVz push an arbitrary guest IP, so a micro-VM cannot natively own its
Kubernetes Pod IP. MacVz bridges this gap on the host rather than inside the
guest.

### Chosen path: host-side 1:1 NAT (pf `binat`)

For each Pod, MacVz programs a packet-filter `binat` (bidirectional NAT) rule in
a dedicated anchor (`macvz/pods`) that maps the Pod's assigned Pod IP to the
micro-VM's host-only address:

```
binat on <vmnet-iface> from <vmIP> to any -> <podIP>
```

- **Inbound** — packets the mesh delivers for the Pod IP (the node's Pod CIDR is
  routed here, see the mesh section) are DNAT'd to the VM address and forwarded
  onto the vmnet interface.
- **Outbound** — packets the VM sends are SNAT'd to appear to originate from the
  Pod IP; the route for a remote Pod CIDR (installed by the mesh) carries them
  into the WireGuard tunnel.

IPv4 forwarding is enabled so the host routes between the mesh interface and the
vmnet interface. The result satisfies the acceptance criteria: each Pod is
addressed by its **MacVz Pod IP** (not an opaque host-only address), and a Pod
on one Mac reaches a Pod hosted on another Mac at **L3**.

`apple/container` may also install a host IPv4 `default` route through the vmnet
bridge when a micro-VM starts. MacVz removes `default` on `podNetwork.interface`
when the Pod network starts and again whenever a VM is attached, so the vmnet
bridge cannot capture the Mac's normal outbound route or sever the kubelet's API
connection.

The provider observes the VM's host-only address from the runtime once the guest
has acquired DHCP, then attaches the mapping; it detaches on Pod deletion. The
anchor ruleset is regenerated wholesale and loaded atomically (`pfctl -a
macvz/pods -f -`) on every change.

### Alternative considered: gvisor-tap-vsock

A fully userspace path — attaching the guest over `vsock`/a file handle and
terminating its traffic in a userspace network stack (gvisor-tap-vsock style) —
would let the Pod own its IP directly and avoid `pf`/root. It requires a guest
agent and a userspace gateway process, so it is **deferred past the MVP**; the
host-NAT path above needs no guest changes and works with stock
`apple/container` images.

### Prerequisites

`pf` must evaluate the MacVz anchor. Reference it from `/etc/pf.conf` once:

```
nat-anchor "macvz/pods"
rdr-anchor "macvz/pods"
binat-anchor "macvz/pods"
anchor "macvz/pods"
```

`pfctl` and `sysctl` require elevated privileges. Identify the vmnet interface
backing the micro-VMs (commonly `bridge100`) with `ifconfig` and set it as
`podNetwork.interface`.

### Configuration

| Field | Meaning |
| --- | --- |
| `podNetwork.enabled` | Turn the host Pod network path on. |
| `podNetwork.interface` | vmnet interface the micro-VMs attach to (e.g. `bridge100`). |
| `podNetwork.anchor` | pf anchor to manage (default `macvz/pods`). |
| `podNetwork.enableForwarding` | Enable IPv4 forwarding (default `true`). |
| `podNetwork.vmNetCIDRs` | Host-only vmnet CIDRs local micro-VMs may receive; used by `macvz-netd` to validate pf targets (default `192.168.64.0/22`). |

```yaml
node:
  podCIDR: 10.244.1.0/24 # required here when privilegedHelperSocket is set

podNetwork:
  enabled: true
  interface: bridge100
  vmNetCIDRs: ["192.168.64.0/22"]
```

### Verifying

```sh
sudo pfctl -a macvz/pods -s nat        # binat rules, one per Pod
sysctl net.inet.ip.forwarding          # = 1
# From a Pod on mac-01, reach a Pod hosted on mac-02 by its Pod IP:
kubectl exec <pod-on-mac01> -- ping -c1 <pod-ip-on-mac02>
```

### Failure modes

- **No binat rules appear** — the anchor is not referenced from `pf.conf`, or pf
  is disabled. Add the anchor hooks above and confirm `pfctl -s info` shows
  `Status: Enabled`.
- **`CreatePod` retries with "micro-VM address not available yet"** — the guest
  has not finished DHCP. The provider polls briefly and the next reconcile
  succeeds once the VM reports an address; persistent failure points at vmnet/DHCP
  on the host.
- **Inbound works, outbound does not (or vice-versa)** — IP forwarding is off
  (`sysctl net.inet.ip.forwarding` should be `1`) or the remote Pod CIDR has no
  mesh route (`netstat -rn` should list it via the WireGuard interface).
- **Pod IP unreachable from another Mac** — verify the mesh tunnel first
  (`wg show`), then that the destination node's Pod CIDR routes to it.

### Pod network tests

- `pkg/network/podnet/router_test.go` — Start (forwarding + pf enable, tolerating
  "already enabled"), `binat` rule rendering, attach/detach anchor reloads,
  endpoint validation, deterministic rendering, and anchor flush on Stop.
- `pkg/provider/podnet_test.go` — attach on create, detach on delete, rollback +
  IP release when attach fails, failure when the VM IP never appears, and the
  disabled-path no-op.

## Pod status & Service resolution (MVP)

This section describes how MacVz reports Pod status to Kubernetes so Endpoints /
EndpointSlices and Services work normally, delivered in issue #23
(`pkg/provider`).

### What the provider publishes

`Provider.reconcileStatus` builds the `PodStatus` that Virtual Kubelet pushes to
the API server:

- **`status.podIP` and `status.podIPs`** — both are set from the
  [assigned Pod IP](#pod-ipam-mvp). The EndpointSlice controller reads
  `podIPs`, so populating it is what makes a MacVz-backed Pod appear as a usable
  Service endpoint. (Without an IPAM allocation, the runtime-reported address is
  used as a fallback.)
- **`status.hostIP` / `status.hostIPs`** — the node's reachable address, so
  `kubectl get pod -o wide` and topology-aware routing resolve the hosting Mac.
- **Conditions** — `Initialized`, `ContainersReady`, and `Ready`.

### Readiness gates endpoint membership

A Pod is reported `Ready` only when it is **both running and addressable** — the
phase is `Running` *and* a Pod IP is present. A running Pod without an address is
held `Ready=False` with reason `PodNetworkNotReady`, and a transient runtime
error yields `Ready=False` with reason `RuntimeStatusError`. This guarantees the
EndpointSlice controller never adds an unreachable Pod to a Service, so traffic
is only sent to Pods that can actually receive it.

### ClusterIP Services and DNS

Control-plane discovery needs no MacVz-specific work: once Pods report correct
`podIPs` and `Ready` conditions, the in-cluster EndpointSlice controller and
CoreDNS treat MacVz-backed Pods like any other, so a `ClusterIP` Service's
EndpointSlice lists the ready MacVz Pod IPs.

The **data path**, however, does need MacVz work, because **kube-proxy cannot run
on a MacVz node**: a node is a macOS host (not Linux), and the kube-proxy Pod
spec — `restartPolicy: Always`, `hostNetwork`, privileged `securityContext` — is
rejected by the provider. Without kube-proxy nothing would translate a Service
ClusterIP for a micro-VM. MacVz fills this gap itself (#37, `pkg/network/svcroute`):

- A controller watches Services and their EndpointSlices and programs the
  `macvz/pods` pf anchor with `rdr` (DNAT) rules: `ClusterIP:port -> backend`.
- A backend Pod on **this** node is redirected to its micro-VM's host-only
  address (directly attached to the vmnet interface, so no extra route is
  needed); a backend on **another** Mac is redirected to its Pod IP, reached over
  the [mesh](#wireguard-mesh-mvp) route. Multiple backends become a `round-robin`
  pool.
- DNS resolution from inside a micro-VM is enabled by injecting the cluster DNS
  server (`node.clusterDNS`, the CoreDNS ClusterIP) and the standard
  `<ns>.svc.<domain>` search list into the guest's resolv.conf via the runtime's
  `--dns`/`--dns-search` flags. CoreDNS itself is reached through the same
  ClusterIP `rdr` path, so the cluster DNS Service must have a MacVz-reachable
  ready endpoint (e.g. a CoreDNS replica on a MacVz node, or its Pod IP routed
  over the mesh) for in-guest name resolution to resolve.

`pfctl`, `sysctl`, and route changes require elevated privileges, so this path is
exercised with `podNetwork.enabled: true` on a privileged run; see
[E2E.md](E2E.md) and `test/e2e/two-node/`.

### Status tests

- `pkg/provider/status_test.go` — `podIP`/`podIPs`/`hostIP` population, readiness
  True when running with an IP, `Ready=False`/`PodNetworkNotReady` when running
  without an IP, and `Ready=False`/`RuntimeStatusError` on a runtime error.

## Port-forward (MVP)

This section describes `kubectl port-forward` for MacVz-backed Pods, delivered in
issue #24 (`pkg/provider`).

### How it works

`kubectl port-forward` opens a stream to the node's kubelet API server (the same
HTTPS endpoint that serves `logs`/`exec`), which Virtual Kubelet routes to
`Provider.PortForward`. Because the kubelet runs on the **same Mac** as the Pod's
micro-VM, it dials the VM's address directly — the host can always reach the
guest's vmnet address, so port-forward works **with or without** the cross-host
[Pod network path](#micro-vm-network-attach-mvp) or [mesh](#wireguard-mesh-mvp).
Bytes are copied bidirectionally between the Kubernetes stream and the TCP
connection until either side closes or the request is cancelled; both copy
goroutines and the connection are always reaped, so closing the forward leaks
nothing.

The target address is the live runtime-reported micro-VM address, falling back
to the address observed when the Pod was attached to the network path.

### Error behavior

- **Unknown Pod** → `NotFound` (kubectl reports the Pod does not exist).
- **Non-running Pod** → a clear "container is not running" error, not `NotFound`.
- **Nothing listening on the port** → the dial is refused and the error surfaces
  to kubectl, matching normal Kubernetes behavior.
- **Out-of-range port** → rejected before dialing.

### Requirements

Port-forward uses the kubelet API server, so it needs the serving TLS cert/key
(`node.servingTLSCertFile`/`servingTLSKeyFile`) just like `logs`/`exec`; without
them the server is not started and these subcommands are unavailable.

### Smoke test

```sh
# A Pod with a process listening on 8080 inside the micro-VM:
kubectl port-forward pod/<name> 18080:8080 &
curl -s localhost:18080            # reaches the in-Pod process
kill %1                            # closing the forward cleans up cleanly
```

### Port-forward tests

- `pkg/provider/portforward_test.go` — byte proxying through a loopback target,
  clean return on stream close and on context cancellation (no goroutine leak),
  and the unknown-Pod / non-running / bad-port / nothing-listening error paths.

## Privileged network helper (`macvz-netd`)

The cross-host data plane needs root tools — `pfctl`, `route`, `sysctl`,
`ifconfig`, `wg`, `wireguard-go`, `pkill` — but `macvz-kubelet` must run as the operator's
user because `apple/container` is a per-user service that refuses to run as root.
`macvz-netd` bridges the two: a small root daemon runs a fixed allowlist of
network commands on behalf of the user-run kubelet, which connects over a unix
socket (issues #38/#39). The kubelet never needs `sudo`, and root is confined to
the allowlisted commands rather than the whole process.

Enable it by pointing the kubelet at the socket in `config.yaml`:

```yaml
privilegedHelperSocket: /var/run/macvz-netd.sock
```

When unset, the privileged commands run in-process — which then requires the
kubelet itself to be root, and is incompatible with `apple/container`.

### Confining the helper to this node's config (#41)

The command allowlist alone stops the helper from running *arbitrary* binaries,
but not from driving an allowlisted binary against arbitrary targets — a
foreign pf anchor, a default-route hijack, an attacker-controlled WireGuard
peer. Passing `--config` closes that gap: the daemon loads the same
`config.yaml` the kubelet uses and validates every request against it.

```sh
sudo macvz-netd serve --socket /var/run/macvz-netd.sock --config /etc/macvz/config.yaml
```

With a config loaded, each request must match a fixed per-command grammar whose
values are pinned to this node's configuration:

- **pf anchors** — only `podNetwork.anchor` (default `macvz/pods`) may be loaded
  or flushed; the main ruleset and any other anchor are refused. A loaded
  ruleset may contain only `binat`/`rdr` rules on the configured
  `podNetwork.interface`, and translated targets must stay inside configured
  Pod CIDRs or `podNetwork.vmNetCIDRs`.
- **interfaces/processes** — `route`, `ifconfig`, `wg`, `wireguard-go`, and the
  teardown `pkill` may only touch `mesh.interface`; the assigned address and MTU
  must equal `mesh.address` / `mesh.mtu`.
- **routes / AllowedIPs** — must fall within a configured peer's Pod CIDR or
  mesh address (`mesh.peers[].podCIDR` / `.address`). A `0.0.0.0/0` route or an
  unlisted CIDR is refused. The only default-route operation allowed is deleting
  IPv4 `default` from `podNetwork.interface`, which prevents vmnet from hijacking
  the host's outbound traffic.
- **WireGuard peers** — a `wg setconf`/`syncconf` payload may only name peer
  public keys listed in `mesh.peers[].publicKey`.
- **sysctl** — only the IPv4-forwarding toggle (and only when a Pod network is
  configured) plus the read-only `kern.ostype` health probe.

Invalid or out-of-scope requests fail closed with a clear error and an audit
line in the log; no command runs. Without `--config`, the daemon refuses to
start unless `--allow-unsafe-no-config` is explicitly passed for local
development. The same config flag works on `install`, baking the config path into
the LaunchDaemon plist:

```sh
sudo macvz-netd install --socket /var/run/macvz-netd.sock --config /etc/macvz/config.yaml
```

### Running the helper

The helper can run in the foreground for a quick test:

```sh
sudo macvz-netd serve --socket /var/run/macvz-netd.sock --config /etc/macvz/config.yaml
```

Launched via `sudo`, it chowns the socket to `$SUDO_UID:$SUDO_GID` so the
invoking (non-root) user can connect and no other user can.

### Install as a LaunchDaemon (#40)

For day-to-day use, install it once as a system LaunchDaemon so it starts at boot
and restarts on crash — no manual `sudo` on every kubelet start:

```sh
sudo macvz-netd install --socket /var/run/macvz-netd.sock --config /etc/macvz/config.yaml
```

`install` (run under `sudo`) does four things:

1. Copies the running binary to `/usr/local/sbin/macvz-netd` (a stable,
   root-owned path the plist can point at).
2. Captures the invoking user from `$SUDO_UID:$SUDO_GID` (override with
   `--owner uid:gid`) and bakes it into the plist's `--owner` flag — launchd has
   no `SUDO_UID` at boot, so the socket owner must be recorded at install time.
3. Writes `/Library/LaunchDaemons/com.github.chimerakang.macvz-netd.plist` with
   `RunAtLoad` and `KeepAlive` true.
4. Bootstraps the job (`launchctl bootstrap system …`) so it starts immediately.

Manage the installed job:

```sh
macvz-netd status                 # install + run state (no sudo needed to read)
sudo macvz-netd unload            # stop the job, keep it installed
sudo macvz-netd load              # start an installed-but-stopped job
sudo macvz-netd uninstall         # stop and remove plist, binary, and socket
```

`status` reports whether the plist, binary, and socket are present and whether
launchd has the job loaded (via `launchctl print system/…`).

### Upgrading

Re-running `sudo macvz-netd install` from a newer binary is the upgrade path. The
binary is written to a temp file and atomically renamed (so a crash never leaves
a half-written binary), and `install` boots out any previously loaded job before
bootstrapping the new plist, so the new binary takes effect. The label and paths
are stable across versions, so an upgrade replaces in place — there is no need to
`uninstall` first.

### Logs

The LaunchDaemon writes to:

- `/var/log/macvz-netd.log` — stdout (klog info, including the listening socket
  and the active allowlist on start).
- `/var/log/macvz-netd.err.log` — stderr.

A refused command — whether non-allowlisted or out-of-scope for the loaded
config (`request not permitted: …`) — is logged here, which is the first place
to look if a privileged operation is unexpectedly failing. Applied privileged
changes are logged at info level as an audit trail. When run in the foreground,
the same output goes to the terminal instead.

## End-to-end: a Service across two Macs

A smoke test that exercises #20–#23 together: a Service backed by Pods on two
different Macs, reached from a client Pod.

### Prerequisites

- Two `macvz-kubelet` nodes registered and `Ready` (`kubectl get nodes`), each
  with a Kubernetes-assigned `Spec.PodCIDR`.
- The [WireGuard mesh](#wireguard-mesh-mvp) up between them (`wg show` shows a
  handshake), and the [Pod network path](#micro-vm-network-attach-mvp) enabled on
  both (`podNetwork.enabled: true`).

### 1. Deploy Pods on both Macs

Schedule one replica to each node (the example pins by hostname; adjust to your
node names). MacVz Pods must tolerate the provider taint.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: hello }
spec:
  replicas: 2
  selector: { matchLabels: { app: hello } }
  template:
    metadata: { labels: { app: hello } }
    spec:
      tolerations:
        - key: virtual-kubelet.io/provider
          operator: Exists
          effect: NoSchedule
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: kubernetes.io/hostname
          whenUnsatisfiable: DoNotSchedule
          labelSelector: { matchLabels: { app: hello } }
      containers:
        - name: hello
          image: <arm64 http server image>
          ports: [{ containerPort: 8080 }]
---
apiVersion: v1
kind: Service
metadata: { name: hello }
spec:
  selector: { app: hello }
  ports: [{ port: 80, targetPort: 8080 }]
```

### 2. Verify Pod IPs and endpoints

```sh
# (#20/#23) Pods show MacVz-assigned Pod IPs from each node's CIDR, on both Macs:
kubectl get pods -l app=hello -o wide

# (#23) The Service has an EndpointSlice listing both ready Pod IPs:
kubectl get endpointslices -l kubernetes.io/service-name=hello -o wide
```

Expect two `Ready` endpoints, one Pod IP from each node's Pod CIDR.

### 3. Reach the Service across the mesh

```sh
# From a client Pod (on either Mac), the Service resolves and load-balances to
# both backends — including the one hosted on the *other* Mac (traffic crosses
# the WireGuard tunnel, #21/#22):
kubectl run client --rm -it --restart=Never \
  --overrides='{"spec":{"tolerations":[{"key":"virtual-kubelet.io/provider","operator":"Exists"}]}}' \
  --image=<arm64 curl image> -- sh -c 'for i in $(seq 10); do curl -s hello; done'
```

Hitting the Service repeatedly should reach Pods on both Macs.

### Troubleshooting

- **An endpoint is missing / `NotReady`** — check the Pod's conditions
  (`kubectl describe pod`): `PodNetworkNotReady` means no Pod IP yet (IPAM/
  network attach), `RuntimeStatusError` means the runtime probe failed.
- **Endpoint present but unreachable cross-host** — this is a data-path issue,
  not status: verify the [mesh](#wireguard-mesh-mvp) tunnel and the
  [network attach](#micro-vm-network-attach-mvp) failure modes.
