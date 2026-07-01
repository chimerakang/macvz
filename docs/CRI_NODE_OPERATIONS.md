# Operating a LinuxPod-backed k3s Node (CRI-L9-2, #150)

Operator runbook for standing up, wiring, validating, and diagnosing a Mac that
joins a k3s cluster as a **LinuxPod-backed CRI node**. It promotes the k3s-agent
CRI-socket integration knowledge from the test harnesses
(`test/e2e/cri-k3s/README.md` and its hooks) into one operator-facing document.

## 1. Overview

A LinuxPod-backed k3s node is an Apple Silicon Mac running:

- a **real kubelet** — a `k3s agent` joined to an external cluster;
- **`macvz-cri`** — the CRI runtime the kubelet drives over a unix socket,
  started with `--experimental-linuxpod-backend`;
- **`linuxpod-helper`** — a signed Swift helper that boots each Pod as an
  isolated Linux **micro-VM** via Apple Containerization and speaks the
  `pkg/runtime/linuxpod` NDJSON contract to `macvz-cri`;
- **`macvz-netd`** — the privileged root helper that owns pf/routes for Pod
  networking (see `docs/PRIVILEGED_NETWORKING.md`).

MacVz never runs a control plane. The API server, scheduler, etcd, Services,
and RBAC belong to a normal k3s/Kubernetes cluster elsewhere; the Mac is only a
node. See `README.md` for positioning, and `docs/MASTER_TASKS.md` for phase
status. The apple/container CRI backend (no `--experimental-linuxpod-backend`)
remains available as an alternative; this document covers the LinuxPod path.

## 2. Prerequisites

- **Apple Silicon Mac** with a macOS release supported by Apple
  Containerization.
- **Apple Containerization assets**: a Linux kernel image (e.g.
  `vmlinux-arm64`) and an initfs reference (e.g. `vminit:latest`) for the
  helper's `--kernel` / `--containerization-root` / `--initfs-reference` flags.
- **apple/container** installed and its per-user service started
  (`container system start`). It refuses to run as root, which is why the
  whole CRI stack runs as the operator user.
- **`linuxpod-helper` binary signed with the
  `com.apple.security.virtualization` entitlement** — an unsigned or
  wrongly-signed helper cannot boot VMs. The restart hook verifies this after
  signing (`codesign -d --entitlements :-` must show the entitlement).
- **`macvz-netd` installed as a root LaunchDaemon** (`sudo macvz-netd
  install`), with `podNetwork.vmNetCIDRs` narrowed to the real vmnet range.
  See `docs/PRIVILEGED_NETWORKING.md` for install, pf.conf anchor hooks, and
  recovery.
- **A non-root operator user** that runs apple/container, the helper,
  `macvz-cri`, and the k3s agent. Only `macvz-netd` is privileged.

## 3. Component wiring reference

Every socket, directory, and flag pair, and who reads/writes it:

| Wiring | Flag / path | Written / served by | Read / consumed by |
|---|---|---|---|
| CRI socket | `macvz-cri --listen unix://<path>` (e.g. `$HOME/.macvz/cri/macvz-cri.sock`) | `macvz-cri` (serves gRPC) | kubelet/k3s `--container-runtime-endpoint`, `crictl` |
| Helper socket | `macvz-cri --linuxpod-helper-socket <path>` = `linuxpod-helper --socket <path>` | `linuxpod-helper` (NDJSON server) | `macvz-cri` (handshakes at startup, exits on failure) |
| Helper work dir | `linuxpod-helper --work-dir <dir>` | `linuxpod-helper` (per-Pod VM state, rootfs staging) | `linuxpod-helper` |
| Adapter state dir | `macvz-cri --state-dir <dir>` | `macvz-cri` (sandbox/container records, IP reservations) | `macvz-cri`, `--diagnose-linuxpod` |
| Kubelet pods dir | `macvz-cri --kubelet-pods-dir <dir>` (kubelet `--root-dir`/pods) | kubelet (projected ConfigMap/Secret/token volumes) | `macvz-cri` + helper (stage volume sources into the Pod VM) |
| hostPath allowlist | `macvz-cri --volume-host-path-allowed <dir>` (repeatable) | operator policy | `macvz-cri` (denies any other `hostPath`) |
| LinuxPod log root | `macvz-cri --linuxpod-log-root <dir>` | helper (container logs) | kubelet (`kubectl logs`); only needed when `/var/log/pods` is not shared/writable by both |
| Pod CIDR | `macvz-cri --pod-cidr <cidr>` (e.g. `10.244.102.0/24`) | `macvz-cri` IPAM (assigns Pod IPs) | must match this node's PodCIDR expectation in the cluster and peer routes |
| Pod network interface | `macvz-cri --pod-network-interface bridge100` | vmnet (bridge carrying the Pod VMs) | `macvz-cri`/`macvz-netd` (binat/NAT attach) |
| netd socket | `macvz-cri --pod-network-helper-socket /var/run/macvz-netd.sock` | `macvz-netd` (root LaunchDaemon) | `macvz-cri` (route/pf requests) |
| Ingress interface | `macvz-cri --pod-network-ingress-interface en0` (+ `--pod-network-enable-forwarding`) | operator | `macvz-cri`/`macvz-netd` (external reachability of Pod IPs) |
| Streaming addr | `macvz-cri --streaming-addr <host-ip>:0` | `macvz-cri` (exec/attach/port-forward streaming server) | kubelet/API server (must be reachable from the control plane) |
| Cluster DNS | **not a `macvz-cri` flag** — kubelet passes it in the CRI sandbox `DnsConfig` (k3s default `10.43.0.10`) | kubelet | helper writes it into the Pod rootfs `/etc/resolv.conf` (#142) |

Helper-side flags (from the canonical restart hook): `--socket`, `--work-dir`,
`--kernel`, `--containerization-root`, `--initfs-reference`, `--image`,
`--vmnet`.

## 4. Startup order

From `test/e2e/cri-k3s/hooks/node-bootstrap.sh`, the documented order is:

1. **apple/container** — `container system start` (per-user service).
2. **macvz-netd** — verified reachable on `/var/run/macvz-netd.sock` (launchd
   brings it back on boot; the bootstrap only waits for it).
3. **linuxpod-helper** — the Pod-VM router.
4. **macvz-cri** — the CRI adapter.
5. **kubelet / k3s agent** — its own supervision; assert node `Ready`.
6. **kind socket forward** — test-topology only (appendix below).

Why this order: `macvz-netd` must exist before any Pod network attach can
succeed; `macvz-cri` **handshakes with the helper at startup and exits on
failure**, so the helper must be serving first; and the kubelet needs a live
CRI endpoint or the node registers NotReady. Stale sockets and dead
per-Pod supervisor journals are cleaned before bring-up; live `supervise-pod`
children are never killed (they own running Pod VMs and are re-adopted).

### Canonical launch command lines

The production-shape command lines, as exercised by the harness hooks
(`test/e2e/cri-k3s/hooks/linuxpod-cri-restart.sh` and
`hooks/linuxpod-helper-restart.sh`; substitute your paths):

```sh
# linuxpod-helper (signed with com.apple.security.virtualization)
linuxpod-helper \
  --socket "$service_dir/linuxpod-helper.sock" \
  --work-dir "$service_dir/helper-work" \
  --kernel .../containerization/bin/vmlinux-arm64 \
  --containerization-root .../containerization/bin \
  --initfs-reference vminit:latest \
  --image docker.io/library/busybox:1.36.1 \
  --vmnet
```

```sh
# macvz-cri (LinuxPod backend)
macvz-cri \
  --listen unix://"$socket" \
  --state-dir "$state_dir" \
  --experimental-linuxpod-backend \
  --linuxpod-helper-socket "$helper_socket" \
  --linuxpod-log-root "$log_root" \
  --kubelet-pods-dir "$kubelet_pods_dir" \
  --volume-host-path-allowed "$volume_allowed" \
  --pod-cidr "$pod_cidr" \
  --pod-network-interface "$pod_network_interface" \
  --pod-network-helper-socket "$pod_network_helper_socket" \
  --pod-network-ingress-interface "$pod_network_ingress_interface" \
  --pod-network-enable-forwarding \
  --streaming-addr "$streaming_addr" \
  -v=4
```

For a managed install instead of a raw process, use
`scripts/macvz-cri-install.sh` (per-user LaunchAgent with `KeepAlive`); see its
`--help` and `status` output. Pass the LinuxPod flags through
`MACVZ_CRI_EXTRA` / `MACVZ_CRI_EXTRA_ARGS_FILE` as shown in
`test/e2e/cri-k3s/README.md`.

## 5. k3s agent integration

Point the k3s agent at the external CRI endpoint instead of its bundled
containerd:

```sh
k3s agent \
  --container-runtime-endpoint "unix://$HOME/.macvz/cri/macvz-cri.sock" \
  --node-label node.macvz.io/runtime=apple-container \
  --node-label node.macvz.io/host-namespace=unsupported \
  --node-taint node.macvz.io/host-namespace-unsupported=true:NoSchedule \
  --server https://<k3s-server>:6443 --token <node-token>
```

- `macvz-cri --preflight` prints the exact labels/taint for your build. For a
  raw kubelet the equivalents are `--node-labels` and `--register-with-taints`.
- The `NoSchedule` taint repels ordinary Pods; workloads that fit the
  per-Pod-VM constraints must select the runtime label **and** tolerate the
  taint (see the `nodeSelector`/`tolerations` snippet in
  `test/e2e/cri-k3s/README.md`).
- **PodCIDR**: `--pod-cidr` must be a per-node CIDR consistent with what the
  cluster assigns/routes for this node; other nodes need a route to it (see
  §7).
- **Cluster DNS**: the kubelet's cluster DNS (k3s default `10.43.0.10`) flows
  through the CRI `DnsConfig` into the Pod rootfs `/etc/resolv.conf`; no extra
  node-side flag is needed, but CoreDNS must be reachable over the Pod/Service
  path (`docs/NETWORKING.md`, "ClusterIP Services and DNS").

### Appendix: test topology (kind + ssh socket forward)

The live lab (`kind-macvz61`) is a **test topology**, not the production
shape: the control plane is a local kind cluster, the kind node's kubelet
reaches the Mac's CRI socket through an in-container ssh unix-socket forward
(the "I5 forwarder"), and Pod CIDRs are wired with per-CIDR routes instead of
touching any default route:

```sh
# Local Mac: send the remote CRI node Pod CIDR to the remote Mac.
sudo route -n add -net 10.244.102.0/24 192.168.1.122

# Remote Mac: return the kind Linux-node Pod CIDR to the local Mac.
ssh -t test@192.168.1.122 \
  'sudo route -n add -inet 10.244.0.0/24 192.168.1.110'

# kind control-plane: SNAT Linux Pod -> Mac Pod traffic.
docker exec macvz61-control-plane iptables -t nat -I POSTROUTING 1 \
  -s 10.244.0.0/24 -d 10.244.102.0/24 -j MASQUERADE
```

A production node runs the k3s agent locally on the Mac against the local
socket and does not need the forwarder or the MASQUERADE rule.

## 6. Validation

1. **Node Ready**:

   ```sh
   kubectl get node <node-name>   # STATUS Ready
   ```

2. **RuntimeReady / NetworkReady** straight from the socket:

   ```sh
   crictl --runtime-endpoint "unix://$HOME/.macvz/cri/macvz-cri.sock" info
   # .status.conditions: RuntimeReady=true, NetworkReady=true
   ```

3. **Genuine LinuxPod backend** (not the simulated stub) — the helper's Ping
   must report `simulated:false`:

   ```sh
   printf '{"op":"Ping"}\n' | nc -U "$helper_socket" | head -n 1
   # {"ok":true,...,"simulated":false,...}
   ```

   The harnesses enforce this as the honesty gate via
   `MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD`; `macvz-cri` also logs
   `simulated=false` in its startup handshake line.

4. **Workload smoke** — a Deployment + ClusterIP Service that selects the
   runtime label and tolerates the taint (e.g.
   `test/e2e/cri-k3s/fixtures/linuxpod-workload.yaml`), then:

   ```sh
   kubectl rollout status deploy/<name>
   kubectl logs <pod>
   kubectl exec <pod> -- sh -c 'echo ok'
   ```

   The automated version is `MACVZ_INTEGRATION=1 KUBECONFIG=... make
   cri-linuxpod-inloop` (plus `make cri-linuxpod-dns`, `make
   cri-conformance-smoke`, `make cri-linuxpod-reboot` for the deeper matrices).

## 7. Failure → diagnosis

| Symptom | Likely cause | Diagnosis / fix |
|---|---|---|
| k3s agent NotReady; kubelet logs `connection refused` / `no such file` on the CRI endpoint | Wrong `--container-runtime-endpoint` path, or `macvz-cri` not running | Compare the kubelet flag with `--listen`; `scripts/macvz-cri-install.sh status`; probe with `crictl ... info` |
| `macvz-cri` exits immediately at startup with a handshake error | Helper socket dead/wrong path, or **helper/adapter protocol version mismatch** — the adapter handshakes before serving and exits loudly rather than failing mid-Pod | Read the `macvz-cri` log tail; restart the helper (its restart hook gates readiness on a real NDJSON Ping, not just a socket file); rebuild helper+adapter from the same tree |
| `crictl info` shows `NetworkReady=false` with `LinuxPodNetworkNotConfigured` | Pod network flags not passed (`--pod-cidr`/`--pod-network-*`) | Add the pod-network flags from §3; re-check with `crictl info` |
| Pods stuck `ContainerCreating`; network attach errors in `macvz-cri` log | `macvz-netd` not running (`/var/run/macvz-netd.sock` missing), or `vmNetCIDRs` does not cover the vmnet range | `sudo launchctl print system/com.github.chimerakang.macvz-netd`; see `docs/PRIVILEGED_NETWORKING.md` (install, `vmNetCIDRs`) |
| Pod gets an IP but is unreachable from the cluster; Service endpoints time out | `--pod-cidr` disagrees with the CIDR the cluster routes to this node, or the per-CIDR peer routes are missing | Verify the node's assigned PodCIDR vs the flag; add per-CIDR routes as in §5 — **never** change a default route |
| In-Pod DNS fails (`nslookup kubernetes.default` times out or NXDOMAINs) | Cluster DNS not delivered (Pod `dnsPolicy`, kubelet cluster-DNS config) vs CoreDNS unreachable (a routing fault) | Follow the `linuxpod-dns.sh` discipline: check `/etc/resolv.conf` in the Pod first; NXDOMAIN on a known-bad name means CoreDNS is reachable (config fault), timeout means it is not (routing fault); curl the Service by IP *and* by name to split DNS from Service routing. `make cri-linuxpod-dns` |
| Host loses connectivity after node bring-up | Something replaced the host default route | The MacVz route guard rule: no component may touch the host default route — `macvz-netd` installs per-CIDR routes only, and every harness asserts the default gateway/interface is unchanged. Recover via `docs/PRIVILEGED_NETWORKING.md` "Recovery procedures" |

## 8. Where to get help

- `macvz-cri --preflight` — environment/flag preflight, prints the k3s
  label/taint flags.
- `macvz-cri --diagnose-linuxpod` — read-only JSON scan of persisted LinuxPod
  state for residual/stale records (add `--linuxpod-helper-socket` to probe
  the live helper).
- `macvz-cri --support-bundle` — one-shot diagnostics bundle, being added by
  #151 (CRI-L9-3).
- Harness evidence reports: `docs/CRI_LINUXPOD_L8_2_DNS_SERVICE_REPORT.md`,
  `docs/CRI_LINUXPOD_L8_3_VOLUME_MATRIX.md`,
  `docs/CRI_LINUXPOD_L8_4_IMAGE_LIFECYCLE_REPORT.md`,
  `docs/CRI_LINUXPOD_L8_5_REBOOT_RECOVERY.md`,
  `docs/CRI_LINUXPOD_L8_6_CONFORMANCE_SMOKE.md`.
- Harness runbook: `test/e2e/cri-k3s/README.md`.
