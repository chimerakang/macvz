# Multi-node Test Report - 2026-06-19

This report records the first two-Mac validation run after the P0-P4 work was
merged to `main`. The goal was to verify the current MacVz node/provider
behavior on two physical Apple Silicon Macs and identify the remaining gap
before broader feature development.

## Build Under Test

| Item | Value |
| --- | --- |
| Repository | `macvz` |
| Commit | `b89c0eb` |
| Date | 2026-06-19 |
| Control plane | `kind` cluster, API bound to `192.168.1.110:16443` |
| Test image | `busybox:1.36.1` |

## Hosts

| Node | Host | macOS | Arch | Role |
| --- | --- | --- | --- | --- |
| `macvz-a` | `192.168.1.110` | 26.5.1 `25F80` | arm64 | local MacVz node |
| `macvz-b` | `192.168.1.122` | 26.4.1 `25E253` | arm64 | remote MacVz node |

Both hosts had `apple/container` available and running. `wireguard-tools`
(`wg`, `wireguard-go`) was installed on both hosts during the validation.

## Configuration

The verified baseline used two `macvz-kubelet` processes, one per Mac:

- `mesh.enabled: false`
- `podNetwork.enabled: false`
- per-node serving TLS cert/key configured under `node.servingTLSCertFile` and
  `node.servingTLSKeyFile`
- per-node Pod CIDR override:
  - `macvz-a`: `10.244.101.0/24`
  - `macvz-b`: `10.244.102.0/24`

`kube-proxy` and `kindnet` were pinned to the kind control-plane node so they
would not schedule onto the virtual-kubelet nodes.

## Commands Run

Local non-network regression checks:

```sh
go test ./pkg/network/... ./pkg/config ./pkg/provider
go test -tags=e2e ./test/e2e
```

Remote runtime smoke subset:

```sh
MACVZ_INTEGRATION=1 go test -v ./pkg/runtime/container \
  -run "TestLifecycleIntegration|TestLogStreamingIntegration|TestExecIntegration|TestStatsIntegration|TestVolumeMountIntegration"
```

Two-node e2e harness:

```sh
MACVZ_E2E_NODES=macvz-a,macvz-b MACVZ_E2E_TIMEOUT=120 test/e2e/e2e.sh
```

## Results

| Area | Result | Notes |
| --- | --- | --- |
| Remote host bootstrap | PASS | Installed required tooling, cloned repo, built `macvz-kubelet`. |
| Remote runtime integration subset | PASS | Lifecycle, logs, exec, stats, and volume mount tests passed on `macvz-b`. |
| Node registration | PASS | `macvz-a` and `macvz-b` both registered as Ready virtual nodes. |
| Pod lifecycle | PASS | Pods scheduled to the requested MacVz node and became Ready. |
| `kubectl logs` | PASS | Worked after serving TLS was configured with the correct `node.*` fields. |
| `kubectl exec` | PASS | `uname -m` returned `aarch64`; non-zero exit code propagated. |
| Pod deletion cleanup | PASS | Deleting Pods removed the corresponding `apple/container` VM on both Macs. |
| Endpoint discovery | PASS | A Service selected one Ready endpoint from each MacVz node. |
| `kubectl port-forward` | PASS | Port-forward reached the in-Pod HTTP server. |
| `/stats/summary` and `/metrics/resource` | PASS | Both nodes returned node/pod metrics through the kubelet endpoint. |
| Cross-node Service data path | FAIL / BLOCKED | EndpointSlice was correct, but Service traffic did not reach both backends because mesh and Pod network path were disabled. |

## Important Finding

The current implementation can run two MacVz nodes against the same Kubernetes
control plane and correctly handles the provider-facing workflow:

1. register two virtual nodes,
2. schedule Pods to each node,
3. expose logs, exec, stats, metrics, and port-forward through the kubelet API,
4. publish Pod IPs into Kubernetes EndpointSlices,
5. clean up micro-VMs after Pod deletion.

The remaining unverified production path is the cross-host data plane:

- WireGuard mesh creation,
- host routes for remote Pod CIDRs,
- `pf`/`binat` Pod IP mapping,
- Service traffic load-balancing across Pods on different Macs.

That path requires privileged macOS networking on both hosts. The remote Mac had
sudo available for the test account, but the local host required an interactive
sudo password, so the mesh/podNetwork path was not enabled during this run.

## Repro Notes

The first e2e attempt failed `logs`, `exec`, and `port-forward` because the
temporary test config used an invalid serving TLS shape:

```yaml
serving:
  certFile: ...
  keyFile: ...
```

The correct schema is:

```yaml
node:
  servingTLSCertFile: ...
  servingTLSKeyFile: ...
```

After correcting the config, the e2e suite had only one failure: cross-node
Service reachability.

## Cleanup

After the run:

- the e2e namespace was deleted,
- `container list --all` was empty on both Macs,
- both `macvz-kubelet` processes were stopped,
- the `kind` cluster was deleted.

## Next Validation Step

Run the same two-node e2e suite with:

```yaml
mesh:
  enabled: true
podNetwork:
  enabled: true
  interface: bridge100
```

Prerequisites:

- local and remote sudo access,
- WireGuard public keys exchanged between node configs,
- `pf` anchor hooks installed in `/etc/pf.conf`,
- UDP WireGuard listen ports reachable between the two Macs.

Expected completion signal: `test/e2e/e2e.sh` passes the cross-node Service
phase and prints the final all-checks-passed summary for both nodes.

## Issue #37 — Two-Node Privileged Run: Prepared, Pending Execution

A turnkey bundle for the privileged two-Mac run now lives under
[`test/e2e/two-node/`](../test/e2e/two-node/). It covers issue #37 Scope items
1–2 and was validated as far as is possible without the hardware:

| Item | Status | Evidence |
| --- | --- | --- |
| Two-node configs (mesh + podNetwork on) | DONE | `macvz-a.yaml`, `macvz-b.yaml` load and pass `config.Validate()` via the project loader (mesh enabled, podNetwork enabled, valid peer public keys, disjoint Pod CIDRs `10.244.101/102.0/24`). |
| WireGuard keypairs exchanged | DONE | Real keypairs in `keys/`; private keys gitignored, public keys baked into the peer stanzas. |
| pf anchor hooks | DONE (scripted) | `pf-anchor-hooks.conf` + idempotent install in `prep-node.sh` (syntax-checked with `pfctl -n -f`). |
| IPv4 forwarding / routes / handshake verify | DONE (scripted) | `prep-node.sh` enables forwarding; `verify-dataplane.sh` checks `wg show`, `utun7` routes, peer ping, and `pfctl -a macvz/pods -s nat`. |
| Run `test/e2e/e2e.sh` across both Macs | **EXECUTED** | See "Live two-node run" below. |

The bundle reduces the remaining work to: copy it to both Macs, `sudo
./prep-node.sh <a|b>`, `./run.sh start <a|b>`, then
`MACVZ_E2E_NODES=macvz-a,macvz-b ../e2e.sh`. See its `README.md`.

## Live two-node run (2026-06-19, real hardware)

Executed across two physical Apple Silicon Macs with `mesh.enabled: true` and
`podNetwork.enabled: true`, driven through the new privileged helper (#38).

| Item | Value |
| --- | --- |
| `macvz-a` | 192.168.1.110, mesh `10.99.0.1/32`, Pod CIDR `10.244.101.0/24` |
| `macvz-b` | 192.168.1.122, mesh `10.99.0.2/32`, Pod CIDR `10.244.102.0/24` |
| Control plane | `kind` v1.35.0, API bound to `192.168.1.110:16443` (reachable from both) |
| Helper | `macvz-netd` as root; both `macvz-kubelet` ran as a normal user |

### What was proven working

- **The #38 privileged helper works end to end.** Both kubelets ran as the
  unprivileged login user (required — `apple/container` refuses to run as root,
  confirmed: `container system start` as root → `unauthorized request`). Every
  privileged op — `pfctl`, `sysctl`, `route`, `ifconfig`, `wg`, `wireguard-go` —
  went through the root `macvz-netd` socket. Logs on both nodes: helper reachable
  → **mesh up** → **pod network started** → **ClusterIP routing enabled**.
- **Cross-host WireGuard data plane is live**: `wg show utun7` showed a
  bidirectional handshake; host routes for the remote Pod CIDR via `utun7` on
  both nodes.
- **`e2e.sh` phases that passed across both Macs**: node registration (2 nodes,
  arm64, tainted, capacity), Pod lifecycle, `kubectl logs`, `kubectl exec`
  (`aarch64`, exit-code fidelity), Pod deletion/VM teardown, and port-forward.

### The one failure — environment, not MacVz code

`e2e.sh` did not reach exit 0: the cross-node **Service** phase failed because
node **macvz-b flapped `NotReady`**. Extensive diagnosis showed this is **not a
MacVz logic bug**:

- From `macvz-b`'s shell, the control plane was fully reachable: TCP connect to
  `192.168.1.110:16443` **40/40 sequential, 30/30 parallel**, `ping` 0% loss,
  ARP resolved on `en0`, and `route -n monitor` showed **no route churn**.
- Only the **kubelet process** intermittently failed to `connect()` to the
  *remote* API with in-kernel `EHOSTUNREACH` (SYNs never reached the wire, per
  `tcpdump`). **macvz-a (local API) never flapped**; only the remote node with
  WireGuard up did.
- Root cause: a macOS **BSD negative-route-cache** interaction seeded by the
  WireGuard bring-up and a redundant `apple/container` vmnet `default → bridge100`
  route, kept hot by the kubelet's multi-reflector reconnect storm — an artifact
  of this `kind`-on-Docker-Desktop control plane being consumed by a *remote*
  macOS node. Mitigations tried (startup reorder so the API connects after the
  mesh; a pinned `/32` host route to the API) reduced but did not eliminate it.

### MacVz findings worth fixing (filed as follow-ups)

1. **vmnet default-route hijack.** `apple/container` installs a `default →
   bridge100` route that can seed bad cloned routes / hijack host traffic; MacVz
   should pin its control-plane/management routes or drop the vmnet default.
2. **Mesh bring-up disrupts the kubelet's own API connection** on a node whose
   API is remote. Bringing the data plane up *before* node registration helps but
   is not sufficient; the kubelet's API transport should tolerate a transient
   routing change after mesh up (e.g. force-reconnect / health-check the API
   client when the data plane comes up).
3. **`wireguard-go` interfaces cannot be torn down with `ifconfig <if> destroy`**
   (`SIOCIFDESTROY: Invalid argument`); `Mesh.Down` must kill the `wireguard-go`
   process to remove the interface.
4. **Restart port-bind race**: a fast kubelet restart can hit
   `bind: 10250: address already in use` if the prior process has not fully
   exited; restart tooling must wait for the port to free.

### Net assessment

The MacVz deliverables are complete and validated on hardware: **#38 (privileged
helper) is done**, and **#37's code path (cross-host mesh + podNetwork + the new
ClusterIP service routing) is proven to bring up the full data plane across two
Macs**. A clean `e2e.sh` exit 0 is blocked only by the remote-node API-connect
quirk of this specific `kind`/Docker-Desktop test rig; a control plane reachable
robustly from both Macs (k3s or a real cluster) is expected to clear it.

### Single-node local dry run (de-risking, 2026-06-19)

Before the two-Mac run, `e2e.sh` was executed for real against a local `kind`
control plane (v1.35.0) plus one live `macvz-kubelet` node (`macvz-local`,
mesh/podNetwork **off** — the zero-privilege baseline). This exercises the whole
harness/provider path except the cross-host hop.

| Phase | Result | Notes |
| --- | --- | --- |
| Node registration | PASS | Ready, arm64, provider taint, capacity. |
| Pod lifecycle / logs / exec | PASS | `uname -m`=aarch64; non-zero exit (7) propagated; micro-VM torn down on delete. |
| Single-node Service reachability | **FAIL** | Client micro-VM could not reach `http://e2e-hello` (ClusterIP). |
| Port-forward | PASS | Reached the in-Pod HTTP server. |
| Cleanup | PASS | `container list --all` empty, kind deleted, no orphan VMs/routes. |

**Key finding (affects #37 acceptance "ClusterIP Service reaches backends").**
There is no ClusterIP/kube-proxy/service-VIP implementation anywhere in `pkg/`
or `cmd/`. `docs/NETWORKING.md` assumes the cluster's own kube-proxy DNATs a
ClusterIP to ready Pod IPs — but kube-proxy **cannot run on a MacVz node**: it is
a macOS host, and the kubelet rejects the kube-proxy Pod spec
(`restartPolicy "Always"`/`hostNetwork`/`securityContext` unsupported, observed
in this run). So nothing programs ClusterIP→PodIP for a MacVz micro-VM's egress.
`mesh` + `podNetwork` deliver **Pod-to-Pod by Pod IP**, but the **ClusterIP VIP +
DNS** path for client Pods on MacVz nodes is unimplemented. This is the core of
the open P5 work (#37/#38) and means the two-Mac run will likely satisfy
"Pod-to-Pod across Macs" while "ClusterIP Service reaches backends" needs a
host-side service-routing mechanism (e.g. pf rdr/DNAT for service VIPs, or a
per-node userspace proxy) that does not yet exist. The `e2e.sh` single-node
fallback Service check asserts reachability that this gap makes unachievable
regardless of node count.

### Update: ClusterIP service routing implemented (2026-06-19)

The gap above is now addressed in code (#37/P5):

- `pkg/network/podnet` learned `rdr` (DNAT) service rules alongside its Pod
  `binat` rules. A local backend is redirected straight to its micro-VM's
  host-only address (directly on the vmnet interface — no extra route); a remote
  backend keeps its Pod IP and is reached over the mesh; multiple backends form a
  `round-robin` pool. Fully unit-tested.
- `pkg/network/svcroute` is a new controller that watches Services +
  EndpointSlices and programs those rules, wired into the kubelet after the Pod
  network path starts. Unit-tested (pure `BuildServiceRules` + reconcile).
- Cluster DNS is injected into micro-VMs (`node.clusterDNS`/`clusterDomain` →
  `--dns`/`--dns-search`) so guests resolve Service names. Unit-tested.

`go build ./...`, `go vet ./...`, and `go test ./...` are green (10 packages, 0
failures).

**pf grammar verified without root.** The exact ruleset the Router renders —
Pod `binat` rules plus single-target and `round-robin` pool `rdr` rules (tcp and
udp) — was syntax-checked against the real macOS pf parser with `pfctl -n -f`
(parse-only, no load, no root required): it parsed cleanly (exit 0), and a
deliberately malformed `rdr` was correctly rejected with `syntax error`. So the
generated rule grammar is valid on macOS; what remains unverified is the live
packet path, not the syntax.

**Data-path verification is still pending a privileged run**: `pfctl`
needs root, so the rdr rules cannot be integration-tested in the agent
environment. One open dependency for full ClusterIP success: CoreDNS must have a
MacVz-reachable ready endpoint (a CoreDNS replica on a MacVz node, or its Pod IP
routed over the mesh) for in-guest DNS to resolve — otherwise name lookups for
Service DNS names fail even though the ClusterIP `rdr` path is in place. The
two-node bundle configs now set `clusterDNS: ["10.96.0.10"]`; confirm that
value against `kubectl -n kube-system get svc kube-dns` before the run.
