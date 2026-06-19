# Privileged Networking: Setup & Recovery (P5)

A complete, repeatable runbook for enabling the **full cross-Mac data plane** —
the WireGuard mesh plus the host Pod-network path — and recovering it when it
breaks. Following this on a fresh Apple Silicon Mac prepares it for the P5
two-node end-to-end (`test/e2e/two-node/`).

This is the operator's guide. For *why* each layer exists and how it works, read
[NETWORKING.md](NETWORKING.md); for the turnkey two-node bundle and its scripts,
read [`test/e2e/two-node/README.md`](../test/e2e/two-node/README.md). This
document ties them together into one ordered procedure with verification output
and failure recovery.

- [The sudo / helper model](#the-sudo--helper-model)
- [Prerequisites](#prerequisites)
- [Setup, in order](#setup-in-order)
  - [1. Install tools](#1-install-tools)
  - [2. WireGuard key exchange](#2-wireguard-key-exchange)
  - [3. Mesh config](#3-mesh-config)
  - [4. podNetwork config & bridge100 selection](#4-podnetwork-config--bridge100-selection)
  - [5. pf.conf anchor hooks](#5-pfconf-anchor-hooks)
  - [6. Install the privileged helper (`macvz-netd`)](#6-install-the-privileged-helper-macvz-netd)
  - [7. Start the kubelet](#7-start-the-kubelet)
- [Verification](#verification)
- [Recovery procedures](#recovery-procedures)
  - [Stale routes](#stale-routes)
  - [Stale pf rules](#stale-pf-rules)
  - [Helper failures](#helper-failures)
- [Teardown](#teardown)

## The sudo / helper model

The cross-host data plane needs **root** tools — `pfctl`, `route`, `sysctl`,
`ifconfig`, `wg`, `wireguard-go`. But `macvz-kubelet` must run as the operator's
**user**, because `apple/container` is a per-user service that refuses to run as
root. These two facts are in direct tension, so MacVz offers two models:

| Model | How privileged commands run | When to use |
| --- | --- | --- |
| **Helper daemon (recommended)** | `macvz-netd` runs as root (a LaunchDaemon) and executes a fixed, config-validated allowlist on behalf of the user-run kubelet over a unix socket. | Production and the P5 e2e. The kubelet needs **no** `sudo`. |
| **In-process (dev only)** | The kubelet runs the privileged commands itself, so the **whole kubelet** must be root. | Incompatible with `apple/container` — only useful for unit-level network testing without real micro-VMs. |

Choose the helper model. With it, the only `sudo` you ever type is the one-time
`macvz-netd install` (and the per-host prep below); day-to-day kubelet starts and
restarts need no elevation. Root is confined to the allowlisted, config-pinned
commands rather than the entire node process. The confinement rules (which
anchors, interfaces, CIDRs, and peers a request may name) are detailed in
[NETWORKING.md → Confining the helper](NETWORKING.md#confining-the-helper-to-this-nodes-config-41).

## Control API (kubelet ↔ helper, #39)

The kubelet and `macvz-netd` talk over a single newline-delimited JSON
request/response on a short-lived unix-socket connection (one request per
connection, mirroring `exec.Command`).

- **Socket**: `--socket` path (default `/var/run/macvz-netd.sock`), mode `0660`,
  owned by the kubelet's user (`$SUDO_UID:$SUDO_GID`, or `--owner uid:gid` under
  launchd). No other user can connect; root holds the daemon.
- **Versioning**: every request carries a `protocol` field. The current version
  is `1`. The helper refuses a request whose `protocol` is set and does not match
  with `errorCode: "unsupported_protocol"`, so mismatched kubelet/helper builds
  fail fast. `protocol: 0` (an older unversioned client) is accepted as the
  current version.
- **Operations** (`op` field):
  - `""` / `exec` — run the allowlisted command in `name` with `args`/`stdin`.
    The reply carries `stdout`, `stderr`, and `exitCode` (a non-zero exit is a
    normal command result, not an API error).
  - `status` — the helper reports `version`, `protocol`, `allowedCommands`,
    `policyEnforced` (whether #41 per-request validation is on),
    `policyReloadable`, `pid`, `startedAt`, and `uptime`. No command runs. The
    kubelet calls this at startup and logs it, then issues a `sysctl` probe to
    confirm the exec path works.
  - `reloadPolicy` — the helper reloads its config-derived policy before a
    SIGHUP mesh-peer reconciliation, so newly configured peer keys and CIDRs are
    permitted without restarting `macvz-netd`.
- **Structured errors**: a refusal sets `errorCode` to one of
  `unsupported_protocol`, `malformed` (undecodable or over the 1 MiB request
  cap), `not_allowed` (command off the allowlist), `not_permitted` (allowlisted
  but outside this node's policy, see #41), `unknown_op`, or `exec_error` (the
  command could not be spawned). The client surfaces these as a typed
  `privhelper.APIError`, so a caller can map a failure to a Pod condition without
  parsing the human-readable message.

A missing daemon, a wrong-version daemon, or a daemon that answers but cannot run
commands each surface as a distinct, actionable startup error rather than failing
later on the first `pfctl`/`wg` call.

## Prerequisites

On **each** Mac that will be a node:

- **macOS 26 (Tahoe)+ on Apple Silicon**, with `apple/container` installed and
  working (`container list` succeeds).
- `wireguard-tools` (provides `wg`, `wireguard-go`) and `kubectl`:
  ```sh
  brew install wireguard-tools kubernetes-cli
  ```
- A **shared Kubernetes control plane** reachable from every node, with
  `--allocate-node-cidrs=true` and a `--cluster-cidr` so each node gets a
  disjoint `Spec.PodCIDR`. (Override with `node.podCIDR` only if your cluster
  does not allocate node CIDRs.)
- **UDP 51820 open** between the hosts (the WireGuard endpoint port). If a host
  firewall is on, allow it explicitly.
- The built `bin/macvz-kubelet` and `bin/macvz-netd` (`make build`) copied to
  each host.

Throughout, the worked example uses the two-node topology from
[`test/e2e/two-node`](../test/e2e/two-node/README.md):

| Node | Host | Mesh addr | Pod CIDR | WG endpoint |
| --- | --- | --- | --- | --- |
| `macvz-a` | 192.168.1.110 | 10.99.0.1/32 | 10.244.101.0/24 | 192.168.1.110:51820 |
| `macvz-b` | 192.168.1.122 | 10.99.0.2/32 | 10.244.102.0/24 | 192.168.1.122:51820 |

## Setup, in order

### 1. Install tools

Confirm the userspace WireGuard toolchain and the runtime are present:

```sh
wg --version            # wireguard-tools is on PATH
which wireguard-go      # userspace utun backend
container list          # apple/container responds
```

### 2. WireGuard key exchange

Each node has a **stable identity**: one private key, kept mode `0600`, that
never enters config or git. The kubelet auto-generates it at
`mesh.privateKeyFile` on first start if absent — but for a planned two-node
bring-up, generate both keypairs up front so you can fill in peer public keys
before starting anything:

```sh
# Run once, anywhere. Produces a private + public key per node.
wg genkey | tee macvz-a.key | wg pubkey > macvz-a.pub
wg genkey | tee macvz-b.key | wg pubkey > macvz-b.pub
chmod 600 *.key
```

Then **exchange public keys**: each node's config lists the *other* node as a
peer, using that peer's **public** key. The private keys stay on their own host:

- Install `macvz-a.key` as `mesh.privateKeyFile` on `macvz-a` only.
- Put the contents of `macvz-a.pub` into `macvz-b`'s `peers[].publicKey` for
  `macvz-a` — and vice-versa.

If you ever need a running node's public key, it is logged at startup, or:

```sh
wg pubkey < /etc/macvz/wireguard.key
```

> The two-node bundle automates this: `keys/*.pub` are committed and baked into
> the peer stanzas; `keys/*.key` are gitignored and installed by `prep-node.sh`.

### 3. Mesh config

Add a `mesh` stanza to each node's `config.yaml`. The mesh is **off by default**,
so single-host development needs none of this. A peer's `podCIDR` becomes its
WireGuard `AllowedIPs`, and a host route for that CIDR is installed through the
interface — that is what forwards Pod-bound packets into the tunnel.

**macvz-a** (`/etc/macvz/macvz-a.yaml`):

```yaml
mesh:
  enabled: true
  interface: utun7
  privateKeyFile: /etc/macvz/wireguard.key
  address: 10.99.0.1/32
  listenPort: 51820
  peers:
    - name: macvz-b
      publicKey: <contents of macvz-b.pub>
      endpoint: 192.168.1.122:51820
      podCIDR: 10.244.102.0/24
      address: 10.99.0.2/32
      persistentKeepalive: 25
```

**macvz-b** is the mirror image: `address: 10.99.0.2/32`, and its single peer is
`macvz-a` with `macvz-a.pub`, endpoint `192.168.1.110:51820`, podCIDR
`10.244.101.0/24`, address `10.99.0.1/32`.

Field reference: see [NETWORKING.md → mesh Configuration](NETWORKING.md#configuration-1).

### 4. podNetwork config & bridge100 selection

`podNetwork` enables the host-side path that maps each Pod IP to its micro-VM's
host-only vmnet address (1:1 NAT via a pf `binat`) and programs ClusterIP `rdr`
rules. Add to each node's config:

```yaml
podNetwork:
  enabled: true
  interface: bridge100        # the vmnet bridge apple/container attaches VMs to
  anchor: macvz/pods          # default; the only anchor the helper will touch
  enableForwarding: true      # default; flips net.inet.ip.forwarding to 1
  vmNetCIDRs: ["192.168.64.0/24"] # default; adjust if your vmnet range differs
```

**Selecting the bridge.** `apple/container` attaches each micro-VM to a
vmnet-backed bridge, **commonly `bridge100`**, but the number is assigned by
macOS and can differ. The bridge only appears **after the first micro-VM
starts** — so confirm it once a VM is running:

```sh
# List bridges and the members each one has (VM vmnet interfaces show as members):
ifconfig | grep -A6 '^bridge'
# Or watch for the vmnet bridge to come up:
ifconfig bridge100 2>/dev/null && echo "bridge100 is up"
```

If the runtime uses a different bridge, set `podNetwork.interface` to match
before starting the kubelet. A wrong interface produces `binat`/`rdr` rules that
never match traffic — see [Recovery](#stale-pf-rules).

To make cluster DNS resolve from inside micro-VMs, also set `node.clusterDNS` to
the CoreDNS ClusterIP (see
[NETWORKING.md → ClusterIP Services and DNS](NETWORKING.md#clusterip-services-and-dns)).

### 5. pf.conf anchor hooks

`pf` only evaluates the MacVz anchor if `/etc/pf.conf` references it. The four
hook lines must respect pf's ordering rule — **translation anchors
(nat/rdr/binat) before filter anchors**:

```
nat-anchor "macvz/pods/*"
rdr-anchor "macvz/pods/*"
binat-anchor "macvz/pods/*"
anchor "macvz/pods/*"
```

On a stock macOS `pf.conf` (which ships `com.apple` anchors), the translation
hooks go right after `rdr-anchor "com.apple/*"` and the filter hook right after
`anchor "com.apple/*"`. The two-node bundle's `prep-node.sh` does this insertion
idempotently and **backs up `pf.conf`** to `pf.conf.macvz.bak.<ts>` first; prefer
it over hand-editing:

```sh
# From test/e2e/two-node, on each host (installs key, TLS, pf hooks, forwarding):
sudo ./prep-node.sh a      # or b
```

If you edit by hand, validate before loading — a syntax error disables pf:

```sh
sudo pfctl -n -f /etc/pf.conf      # dry-run syntax check; fails loud
sudo pfctl -f /etc/pf.conf         # load
sudo pfctl -e                      # enable (tolerate "already enabled")
```

### 6. Install the privileged helper (`macvz-netd`)

Install once per host as a root LaunchDaemon, pinned to this node's config so
every request is validated against it:

```sh
sudo macvz-netd install \
  --socket /var/run/macvz-netd.sock \
  --config /etc/macvz/macvz-a.yaml      # macvz-b.yaml on the other host
```

`install` copies the binary to `/usr/local/sbin/macvz-netd`, records the invoking
user (`$SUDO_UID:$SUDO_GID`) as the socket owner, writes the plist with
`RunAtLoad`/`KeepAlive`, and bootstraps the job. Always pass `--config` — without
it the daemon refuses to start because per-request policy would be disabled.

Confirm it is up:

```sh
macvz-netd status        # no sudo needed to read; reports plist/binary/socket/loaded
```

Then point the kubelet at the socket in its `config.yaml`:

```yaml
privilegedHelperSocket: /var/run/macvz-netd.sock
```

> **Upgrading:** re-run `sudo macvz-netd install` from the newer binary; it boots
> out the old job and replaces in place — no `uninstall` first.

### 7. Start the kubelet

With the helper running and `privilegedHelperSocket` set, start the kubelet as
your **normal user** (no sudo):

```sh
KUBECONFIG=/path/to/kubeconfig macvz-kubelet --config /etc/macvz/macvz-a.yaml
```

On start it brings up the mesh interface, applies peers, installs peer-CIDR
routes, enables forwarding, and begins reconciling Pods and Services through the
helper. Repeat on the other host. The two-node bundle's `run.sh start <a|b>`
wraps this.

## Verification

Run these after both nodes are up. The bundle's
[`verify-dataplane.sh`](../test/e2e/two-node/verify-dataplane.sh) automates the
first three and exits non-zero if any fail.

**1. WireGuard handshake** — a recent handshake with the peer:

```sh
sudo wg show utun7
```
```
interface: utun7
  public key: <this node>
  listening port: 51820
peer: <peer public key>
  endpoint: 192.168.1.122:51820
  allowed ips: 10.244.102.0/24, 10.99.0.2/32
  latest handshake: 23 seconds ago        # <- must be present and recent
  transfer: 1.85 KiB received, 2.10 KiB sent
```

**2. Routes for the remote Pod CIDR** — present via the mesh interface:

```sh
netstat -rn -f inet | grep utun7
```
```
10.244.102/24      utun7              USc      utun7      # remote Pod CIDR
10.99.0.2          utun7              UH       utun7      # peer mesh addr
```

**3. IPv4 forwarding** — on:

```sh
sysctl net.inet.ip.forwarding
```
```
net.inet.ip.forwarding: 1
```

**4. Reach the peer across the tunnel:**

```sh
ping -c2 10.99.0.2        # WARN-only if ICMP is filtered; not fatal
```

**5. pf anchor rules** — `binat` per attached Pod, plus `rdr` per ClusterIP:

```sh
sudo pfctl -a macvz/pods -s nat        # binat rules, one per Pod on this node
sudo pfctl -a macvz/pods -s rdr        # rdr rules, one per ClusterIP:port
```
```
binat on bridge100 inet from 192.168.64.3 to any -> 10.244.101.5
```
> No rules until a Pod is scheduled on this node — that is expected, not a fault.

**6. End-to-end** — a Pod on one Mac reaches a Pod hosted on the other, and a
ClusterIP Service load-balances across both. Use the worked example in
[NETWORKING.md → End-to-end](NETWORKING.md#end-to-end-a-service-across-two-macs),
or run the suite:

```sh
MACVZ_E2E_NODES=macvz-a,macvz-b MACVZ_E2E_TIMEOUT=120 \
  MACVZ_E2E_DIAG_DIR=./diag test/e2e/e2e.sh
```

A fresh Mac prepared with this runbook should make the e2e "Service across nodes"
phase **PASS** rather than SKIP.

## Recovery procedures

Privileged network state lives in the kernel (routes, pf rules, the utun
interface) and outlives the kubelet process. A crash, a config change, or a
host/peer reconfiguration can leave **stale** state that silently misroutes or
blackholes traffic. Each of these is read-only to diagnose and safe to re-run.

### Stale routes

Symptom: traffic to a remote Pod CIDR blackholes, or `netstat` shows a route to
a CIDR/interface that no longer exists (e.g. an old peer's CIDR, or a route on a
utun number from a previous run).

```sh
# 1. See what is installed via the mesh interface:
netstat -rn -f inet | grep utun

# 2. Normal recovery: a kubelet restart re-reconciles routes from config —
#    Mesh.Sync adds/removes only the affected host routes, no full teardown.
#    Just restart macvz-kubelet.

# 3. If a route persists for a CIDR no longer in any peer (orphan), remove it:
sudo route -n delete -inet 10.244.199.0/24 -interface utun7

# 4. If the utun interface itself is wedged (wrong number, half-torn-down),
#    destroy it and let the kubelet recreate on next start:
sudo ifconfig utun7 down
sudo ifconfig utun7 destroy        # bring-up is idempotent; restart re-creates it
```

Because bring-up tolerates "already exists" and `Mesh.Sync` reconciles
incrementally, the safe default is **restart the kubelet** and only hand-delete
orphans the restart cannot know about (CIDRs removed from a since-changed config).

### Stale pf rules

Symptom: `binat`/`rdr` rules reference an old Pod's vmnet address or the wrong
interface; traffic is NAT'd to a VM that no longer exists, or rules never match.

```sh
# 1. Inspect the anchor (this is the only anchor MacVz touches):
sudo pfctl -a macvz/pods -s nat
sudo pfctl -a macvz/pods -s rdr

# 2. The kubelet regenerates this anchor wholesale and loads it atomically on
#    every change, and FLUSHES it on clean shutdown. So a stop+start clears
#    stragglers. If the kubelet died uncleanly, flush by hand:
sudo pfctl -a macvz/pods -F all        # flush all rules in the macvz anchor only

# 3. If rules are present but never match, the bridge is wrong: confirm
#    podNetwork.interface equals the live vmnet bridge (see step 4 of setup),
#    fix the config, and restart.

# 4. If the host default route points at the vmnet bridge, the kubelet removes it
#    at podNetwork start and whenever a Pod attaches. If it reappears, check
#    macvz-netd logs for a refused route delete on podNetwork.interface.
netstat -rn -f inet | grep '^default'

# 5. If NO rules ever appear, pf is not evaluating the anchor. Confirm the hooks
#    and that pf is enabled:
grep 'macvz/pods' /etc/pf.conf         # the four anchor lines must be present
sudo pfctl -s info | grep Status       # must read: Status: Enabled
```

Flushing only ever targets `-a macvz/pods`; the main ruleset and `com.apple`
anchors are never touched. If pf got disabled by a bad hand-edit, restore from
the `pf.conf.macvz.bak.<ts>` backup `prep-node.sh` left and reload.

### Helper failures

Symptom: a privileged operation fails even though the command looks valid; the
kubelet logs an error attaching a Pod or syncing the mesh.

```sh
# 1. Is the daemon loaded and the socket present?
macvz-netd status

# 2. First place to look: the helper logs. A refused request is logged as
#    "request not permitted: ..." with an audit line; applied changes log at info.
tail -n 50 /var/log/macvz-netd.log     # stdout: socket, allowlist, audit trail
tail -n 50 /var/log/macvz-netd.err.log # stderr

# 3. "request not permitted" means the request is out-of-scope for the loaded
#    --config: a CIDR/interface/peer/anchor not pinned in THIS node's config.
#    Reconcile the config with the request (e.g. add the peer's podCIDR). A
#    kubelet SIGHUP reload asks macvz-netd to refresh policy before applying
#    mesh changes; otherwise restart macvz-netd to reload manually:
sudo macvz-netd unload && sudo macvz-netd load

# 4. Socket permission denied from the kubelet: the socket owner was recorded at
#    install time from $SUDO_UID. If you now run the kubelet as a different user,
#    reinstall with --owner uid:gid for that user.

# 5. Restart the job without reinstalling:
sudo macvz-netd unload && sudo macvz-netd load
```

If `--config` was omitted at install, the daemon refuses to start. Reinstall with
`--config` so config-pinned validation is active.

## Teardown

Privileged state does not revert on its own. To leave a host clean:

```sh
# Stop the kubelet (flushes the macvz/pods anchor and best-effort tears down mesh):
#   run.sh stop a    # if using the bundle, or just stop the macvz-kubelet process

sudo pfctl -a macvz/pods -F all            # belt-and-suspenders anchor flush
sudo route -n flush -inet                  # only if you must clear stale routes
sudo ifconfig utun7 destroy 2>/dev/null    # remove the mesh interface
sudo sysctl -w net.inet.ip.forwarding=0    # revert forwarding, if desired

# Remove the helper entirely (plist, binary, socket):
sudo macvz-netd uninstall

# Restore /etc/pf.conf from the prep-node.sh backup, if you want the hooks gone:
#   sudo cp /etc/pf.conf.macvz.bak.<ts> /etc/pf.conf && sudo pfctl -f /etc/pf.conf

# Verify nothing is orphaned:
container list --all                       # expect no leftover micro-VMs
netstat -rn -f inet | grep utun7           # expect no stale remote Pod CIDR routes
sudo pfctl -a macvz/pods -s all            # expect empty
```
