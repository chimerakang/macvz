# CRI-P8 k3s compatibility suite

Experimental MacVz CRI feasibility track (`develop`), issue #80. This directory
documents how a k3s/kubelet points at the experimental `macvz-cri` adapter and
runs a realistic `crictl` compatibility, restart, and soak suite. It is **not**
the shipped Virtual Kubelet path (`test/e2e/e2e.sh`) and must not gate the VK
release.

## Layout

- `run.sh` — gated compatibility suite: adapter handshake, single-container Pod
  lifecycle, logs, exec, projected config mount, unsupported-shape diagnostic,
  adapter restart recovery, and cleanup verification. Driven by `crictl`.
- `soak.sh` — gated bounded soak: repeated create/delete cycles sampling adapter
  RSS and orphan counts, with leak/orphan guards. Driven by `crictl`.
- `k3s-inloop.sh` — gated **real kubelet/k3s in-loop** suite (CRI-P9 follow-up
  #85): schedules `fixtures/workload.yaml` through a real k3s control plane and
  proves `kubectl rollout status`/`logs`/`exec`/`port-forward`, ClusterIP Service
  reachability, macvz-cri and k3s restart recovery, and a sustained soak. This is
  the layer `run.sh`/`soak.sh` cannot reach — `crictl` is not a control-plane loop.
- `fixtures/workload.yaml` — the #85 single-container fixture: selects the #84
  runtime label and tolerates the host-namespace taint, with projected
  ConfigMap/Secret, an HTTP probe, and a ClusterIP Service.
- `linuxpod-inloop.sh` — gated **LinuxPod-backed** in-loop suite (CRI-L5 #130),
  the sibling of `k3s-inloop.sh` for a node running `macvz-cri
  --experimental-linuxpod-backend --linuxpod-helper-socket=<sock>`. Schedules
  `fixtures/linuxpod-workload.yaml` (an app + a *late* sidecar sharing one Pod
  sandbox) and proves the LinuxPod surface — shared namespace, sidecar localhost,
  rootfs identity, Pod IP, logs/exec/port-forward/Service, adapter + helper
  restart recovery, and a residual LinuxPod VM/container/rootfs/handoff/network
  audit. It includes a **honesty gate**: it refuses to pass any LinuxPod-specific
  acceptance unless `MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD` proves the Pod is served
  by a genuine, non-simulated LinuxPod backend (a `simulated=true` handshake is
  skipped loudly, never passed). `make cri-linuxpod-inloop`. Gated by
  `MACVZ_INTEGRATION=1` + `KUBECONFIG`. Report:
  `docs/CRI_LINUXPOD_L5_INLOOP_VALIDATION_REPORT.md`.
- `linuxpod-soak.sh` — gated **LinuxPod soak/churn** harness (CRI-L6-1 #135), the
  soak sibling of `linuxpod-inloop.sh`. It drives the same real kubelet/k3s
  topology and `fixtures/linuxpod-workload.yaml`, but instead of a single
  lifecycle pass it loops restarts and lifecycle churn over
  `MACVZ_SOAK_ITERATIONS` iterations, round-robin over the enabled
  `MACVZ_SOAK_CHURN_MODES` (`rollout`, `cri`, `helper`, `netd`). Each iteration
  records Pod UID, Pod IP, restartCount, adapter RSS, helper RSS, and residual
  LinuxPod-state count to `soak-samples.csv`, and re-asserts the host default
  route is unchanged. It proves the #134 recovery acceptances: a `cri` restart
  preserves the Pod UID with no duplicate backend state; a `helper` restart
  recovers live or via bounded recreate with exec/logs working afterward; a
  `netd` restart/reload never mutates the default route and restores
  reachability; and cleanup leaves zero residual state. It inherits the #130
  **honesty gate** (`MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD`): LinuxPod-backend
  claims are skipped loudly unless the Pod is proven non-simulated. A churn mode
  whose hook is unset is dropped with a loud skip. `make cri-linuxpod-soak`.
  Gated by `MACVZ_INTEGRATION=1` + `KUBECONFIG`.
  For the `netd` mode, prefer a policy reload through the existing helper socket
  when validating route safety without a privileged shell:

  ```sh
  MACVZ_NETD_SSH_TARGET=test@<mac> \
  MACVZ_RESTART_NETD_CMD='./test/e2e/cri-k3s/hooks/netd-reload-policy.sh'
  ```

  The hook sends `reloadPolicy` and `status` to `/var/run/macvz-netd.sock` over
  SSH as the normal user. It intentionally avoids `sudo`, `launchctl`,
  `route`, and `pfctl`; pair it with `MACVZ_ROUTE_AUDIT_CMD` so the soak proves
  the host default route stays unchanged.
- `linuxpod-multipod.sh` — gated **LinuxPod multi-Pod concurrency** suite
  (CRI-L6-3 #137), the concurrency sibling of `linuxpod-inloop.sh`. It schedules
  `fixtures/linuxpod-multipod-workload.yaml` (a Deployment of N>=3 app+late-sidecar
  replicas, all landing on the single MacVz node) and proves several concurrent
  LinuxPod micro-VMs run side by side: >=3 Pods reach Ready, each gets a **unique**
  Pod IP from the node PodCIDR, the ClusterIP Service load-balances across distinct
  Pod backends, every Pod is reachable on its **direct Pod IP**, and logs/exec/
  port-forward work per Pod. It then exercises concurrency recovery — a `macvz-cri`
  restart keeps every Pod UID with no doubled residual state; a `linuxpod-helper`
  restart yields bounded recreate with exec working on every Pod afterward — and
  asserts **no duplicate pf/binat or helper-work** record is left behind
  (`MACVZ_LINUXPOD_DUP_AUDIT_CMD`). A scale-churn loop
  (`MACVZ_MULTIPOD_CHURN_CYCLES`) proves Pod IPs stay unique and residual state +
  helper/process count return to baseline, and the soak tracks adapter RSS and
  helper/process count under concurrency against documented pass/fail thresholds
  (`MACVZ_INLOOP_RSS_GROWTH_KB`, `MACVZ_MULTIPOD_PROC_GROWTH` via
  `MACVZ_LINUXPOD_HELPER_PROC_CMD`). It inherits the #130 **honesty gate**: it
  requires `MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD` to prove **every** Pod is a
  genuine, non-simulated LinuxPod backend or the LinuxPod acceptances skip loudly.
  `make cri-linuxpod-multipod`. Gated by `MACVZ_INTEGRATION=1` + `KUBECONFIG`.
- `linuxpod-dns.sh` — gated **LinuxPod k3s DNS + Service-discovery** suite
  (CRI-L8-2 #142), the DNS sibling of `linuxpod-inloop.sh`. Where inloop proves
  the Pod lifecycle, shared namespace, and a *manually probed* ClusterIP curl,
  this harness proves the **normal k3s DNS path** works from inside a LinuxPod
  Pod by `kubectl exec`-ing `nslookup`/`wget` in the app container: CoreDNS
  reachability, `*.svc` and `*.svc.cluster.local` resolution, a headless Service
  returning the Pod's A record, and same-namespace vs other-namespace
  (`kubernetes.default`, `kube-dns.kube-system`) lookups. Its central discipline
  is to **distinguish a DNS failure from a Pod-networking or Service-routing
  failure**: it checks resolver config (kubelet dnsPolicy) apart from resolution,
  uses a known-good name for CoreDNS reachability and a known-bad name for an
  authoritative-NXDOMAIN control (NXDOMAIN vs timeout = CoreDNS unreachable), and
  curls the Service **both** by name and by the resolved ClusterIP so a DNS-layer
  fault (by-IP-ok/by-name-fail) is never confused with a Service-routing fault
  (by-IP-fail). It re-runs the DNS core after **rollout-restart**, **macvz-cri
  restart**, **LinuxPod helper restart**, and **netd reload**, and asserts the
  host default route is unchanged across the run and across the netd reload. It
  inherits the #130 **honesty gate**: the DNS checks run on either backend, but
  the LinuxPod-specific framing is only asserted when
  `MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD` proves a non-simulated LinuxPod Pod;
  absent that the result is reported against the apple/container path, never
  silently claimed as a LinuxPod result. `make cri-linuxpod-dns`. Gated by
  `MACVZ_INTEGRATION=1` + `KUBECONFIG`. Report:
  `docs/CRI_LINUXPOD_L8_2_DNS_SERVICE_REPORT.md`.
- `linuxpod-volumes.sh` — gated **LinuxPod k3s volume-projection matrix** suite
  (CRI-L8-3 #145), the volume sibling of `linuxpod-inloop.sh`. Where inloop proves
  the Pod lifecycle and `linuxpod-dns.sh` proves the resolver path, this harness
  proves the **kubelet-managed Kubernetes volume matrix** an ordinary k3s workload
  depends on is honored on a LinuxPod Pod by `kubectl exec`-ing inside the
  containers: `configMap`, `secret`, downward API, the projected service-account
  token (`/var/run/secrets/kubernetes.io/serviceaccount`: token + ca.crt +
  namespace), a disk `emptyDir` **shared** across the app and the late sidecar,
  and a Memory-medium `emptyDir`. Its central discipline is to **distinguish a
  volume fault from a Pod-networking/Service fault, and a policy outcome from a
  plumbing one**: every read-only mount is proven read-only by a *failed* write
  probe (a writable RO mount is a translation bug), read-write scratch is proven
  writable, content is matched against exact markers, the shared `emptyDir` is
  verified visible **both** ways across containers, and a ConfigMap patch is
  asserted to **propagate** into the running Pod (kubelet projected-volume update
  behavior). Arbitrary `hostPath` is denied by the macOS default mount policy, so
  allow/deny is covered hermetically (`pkg/criserver` `TestLinuxPodVolumePolicyErrors`
  / `TestLinuxPodVolumeProjectionMatrix`); an allowlisted-hostPath live probe runs
  only with `MACVZ_VOLUME_HOSTPATH_PROBE_CMD`. It re-runs the core matrix after
  **rollout-restart**, **macvz-cri restart** (Pod UID preserved), and **LinuxPod
  helper restart**, asserts cleanup leaves **no residual** materialized
  mount/rootfs state (`MACVZ_LINUXPOD_RESIDUAL_CMD`), and keeps the host default
  route unchanged. It inherits the #130 **honesty gate**: the volume checks run on
  either backend, but the LinuxPod-specific framing is only asserted when
  `MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD` proves a non-simulated LinuxPod Pod.
  `make cri-linuxpod-volumes`. Gated by `MACVZ_INTEGRATION=1` + `KUBECONFIG`.
  Report: `docs/CRI_LINUXPOD_L8_3_VOLUME_MATRIX.md`.
- `fixtures/linuxpod-workload.yaml` — the #130 two-container fixture (app + late
  sidecar) for the LinuxPod backend; the multi-container shared-namespace shape
  the default apple/container path excludes (#82/#86).
- `fixtures/linuxpod-multipod-workload.yaml` — the #137 multi-replica fixture: N>=3
  app+late-sidecar Pods plus one ClusterIP Service, each app serving a per-Pod
  `/whoami` identity so the Service phase can prove it reached distinct backends.
- `fixtures/linuxpod-dns-workload.yaml` — the #142 DNS fixture: the app+late-sidecar
  LinuxPod shape plus a ClusterIP Service and a headless Service, where the late
  sidecar performs a boot-time `nslookup` of its own Service as boot-time DNS
  evidence to complement `linuxpod-dns.sh`'s exec-time probes.
- `fixtures/linuxpod-volumes-workload.yaml` — the #145 volume-matrix fixture: the
  app+late-sidecar LinuxPod shape mounting `configMap`/`secret`/downward API
  (read-only), a disk `emptyDir` shared with the sidecar, and a Memory `emptyDir`;
  the app records a marker into the shared volume and the sidecar records one back,
  so `linuxpod-volumes.sh` can prove cross-container sharing in both directions.
- `node-reboot-recovery.sh` — gated **node reboot / bootstrap recovery** check
  (CRI-L8-5 #144), the recovery sibling of `linuxpod-soak.sh`. Where the soak
  loops service-level churn while the rest of the stack stays up, this proves a
  *full restart of the node stack* — a remote Mac reboot (`MACVZ_REBOOT_CMD`) or
  an ordered service-stack restart (`MACVZ_BOOTSTRAP_CMD`) over
  `MACVZ_RECOVERY_SCENARIOS` (`services,reboot`) — returns the LinuxPod-backed
  k3s node to a known-good `Ready` state without manual cleanup. It documents the
  expected startup order (apple/container → macvz-netd → linuxpod-helper →
  macvz-cri → kubelet/k3s → kind socket forward) and, with
  `MACVZ_STARTUP_PROBE_CMD`, asserts each component is ready in order. Per
  scenario it asserts the node returns `Ready`, the workload Pod is usable
  (exec+logs serve the marker; a reboot expects a *fresh* Pod since VMs do not
  survive reboot), `MACVZ_STALE_STATE_CMD` settles to zero (no leftover helper
  sockets / supervisor journals / VM state / kubelet sandbox records), and the
  default route is unchanged AND still the expected gateway/interface
  (`MACVZ_EXPECTED_DEFAULT_GW` via `MACVZ_EXPECTED_DEFAULT_IF`, default
  `192.168.1.1` via `en0`) before and after recovery. It inherits the #130
  **honesty gate**: the LinuxPod-VM portion of the stale-state claim is only
  enforced when `MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD` proves a non-simulated
  LinuxPod Pod; the kubelet/CRI-visible recovery runs either way. A scenario
  whose required hook is unset is dropped with a loud skip. `hooks/node-bootstrap.sh`
  is a route-preserving reference bring-up that delegates to the existing
  helper/cri restart hooks. `make cri-linuxpod-reboot`. Gated by
  `MACVZ_INTEGRATION=1` + `KUBECONFIG`. Report:
  `docs/CRI_LINUXPOD_L8_5_REBOOT_RECOVERY.md`.
- `conformance-smoke.sh` — gated **k3s conformance smoke subset** (CRI-L8-6 #147),
  the "ordinary workload compatibility" sibling of `linuxpod-inloop.sh`. Where the
  in-loop/soak/multipod suites prove the LinuxPod-specific surface (shared
  namespace, identity, recovery, concurrency), this runs a small, repeatable
  subset of the everyday k3s behaviors a real chart relies on, in **one** apply:
  Deployments, ClusterIP + headless Services, DNS, ConfigMaps, Secrets, a
  multi-source projected volume, readiness/liveness probes, `restartPolicy:
  Always` (a backend-agnostic cycling container), logs, exec, port-forward,
  cleanup, and node readiness. It is explicitly a **curated subset, NOT full
  upstream conformance**, and records what it does not cover
  (StatefulSets/Jobs/CSI/etc., and the deeper matrices owned by sibling issues
  #142/#143/#144/#145/#146) so the support claim stays honest. It inherits the
  #130 **honesty gate**: universal control-plane behaviors run on either backend,
  but the LinuxPod-path surfaces (CRI log streaming, in-Pod cluster DNS, CRI
  restart surfacing) are **gated** — a hard FAIL on a proven LinuxPod-backed node
  (`MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD`), a loud known-limitation SKIP otherwise,
  so a surface is never silently claimed. Each phase writes layer-tagged
  diagnostics (kubelet/CRI/helper/network/workload) and the run asserts the host
  default route is unchanged. `make cri-conformance-smoke`. Gated by
  `MACVZ_INTEGRATION=1` + `KUBECONFIG`. Ran repeatably on `test@192.168.1.122`
  (apple/container path, `0 FAIL`). Report:
  `docs/CRI_LINUXPOD_L8_6_CONFORMANCE_SMOKE.md`.
- `fixtures/conformance-smoke.yaml` — the #147 conformance-smoke fixture: an
  app+late-sidecar Deployment (ConfigMap, Secret, projected volume, probes,
  ClusterIP + headless Service) bundled with a separate cycling-container
  `restartPolicy: Always` Deployment (exits 0 on a loop so `restartCount` climbs
  with no reliance on volume persistence), so a single apply exercises the whole
  subset.

`run.sh`/`soak.sh` are gated by `MACVZ_INTEGRATION=1`; `k3s-inloop.sh`
additionally needs a reachable `KUBECONFIG`. Without their gates each prints its
plan and exits 0, so they are safe in `go test`-style CI.

## Quick start

```sh
make cri
MACVZ_INTEGRATION=1 ./test/e2e/cri-k3s/run.sh
MACVZ_INTEGRATION=1 ./test/e2e/cri-k3s/soak.sh
```

These self-manage a throwaway adapter on a temp socket — no cluster required.
They exercise the CRI contract a kubelet drives, including the explicit
`PullImage` step before `CreateContainer`.

## Pointing k3s at macvz-cri

apple/container is a per-user service and refuses to run as root, so the adapter
runs as the operator (a per-user LaunchAgent via `scripts/macvz-cri-install.sh`),
and k3s must be configured to use that external CRI endpoint rather than its
bundled containerd.

1. Install the adapter as a managed per-user service:

   ```sh
   make cri
   ./scripts/macvz-cri-install.sh install --from ./bin \
     --socket "$HOME/.macvz/cri/macvz-cri.sock" \
     --state-dir "$HOME/.macvz/cri/state"
   ./scripts/macvz-cri-install.sh status
   ```

   For Pod networking, pass adapter flags through `MACVZ_CRI_EXTRA`, e.g.
   `MACVZ_CRI_EXTRA="--pod-cidr 10.42.0.0/24 --pod-network-interface bridge100"`.
   If an argument contains spaces, put one argument per line in a file and set
   `MACVZ_CRI_EXTRA_ARGS_FILE=/path/to/args`.

   To exercise the **experimental handoff-aware runtime** (CRI-I, #109..#117)
   instead of the default apple/container path, add the handoff flags. The
   production handoff root `/run/macvz/containers` is not writable on macOS, so
   point `--handoff-root` at a writable per-user directory:

   ```sh
   MACVZ_CRI_EXTRA="--experimental-handoff --handoff-root $HOME/.macvz/cri/handoff"
   ```

   With the node running this way, the k3s in-loop harness exercises the handoff
   path end to end: StartContainer gates a Pod's container to Running only after
   the launched process reports the expected rootfs identity through the
   runtime-private evidence channel (#116). The in-loop fixture writes the
   staged `/etc/macvz-container-identity` value to
   `/run/macvz/handoff/identity` when those runtime-private paths are present,
   and skips that branch on the default adapter path. Run the harness with
   `MACVZ_HANDOFF=1` (and optionally `MACVZ_HANDOFF_STATUS_CMD` to surface
   on-node `identityVerified` diagnostics). See
   `docs/CRI_RUNTIME_I4_2_INLOOP_HANDOFF_REPORT.md`.

2. Start k3s (agent) against the external endpoint:

   ```sh
   k3s agent \
     --container-runtime-endpoint "unix://$HOME/.macvz/cri/macvz-cri.sock" \
     --node-label node.macvz.io/runtime=apple-container \
     --node-label node.macvz.io/host-namespace=unsupported \
     --node-taint node.macvz.io/host-namespace-unsupported=true:NoSchedule \
     --server https://<k3s-server>:6443 --token <node-token>
   ```

   The `--node-label`/`--node-taint` flags are the CRI-P9 follow-up (#84)
   host-namespace scheduling-exclusion scheme: host-namespace Pods cannot be
   honored on the per-Pod-VM model, so the node is registered with scheduling
   metadata and the adapter rejects any incompatible Pod that still lands (see
   `docs/CRI_FEASIBILITY.md`, "CRI-P9 Follow-up
   (#84)"). `macvz-cri --preflight` prints the exact flags. For a raw kubelet the
   equivalents are `--node-labels` and `--register-with-taints`.

   The taint is intentionally opt-in: it also repels ordinary Pods unless they
   tolerate it. Workloads that are known to fit the MacVz constraints should both
   select the runtime label and tolerate the taint:

   ```yaml
   spec:
     template:
       spec:
         nodeSelector:
           node.macvz.io/runtime: apple-container
         tolerations:
           - key: node.macvz.io/host-namespace-unsupported
             operator: Equal
             value: "true"
             effect: NoSchedule
   ```

   Equivalent `config.yaml`:

   ```yaml
   container-runtime-endpoint: "unix:///Users/<you>/.macvz/cri/macvz-cri.sock"
   ```

   Startup ordering: apple/container and (optionally) `macvz-netd` must be up
   before the adapter, and the adapter before k3s. The LaunchAgent's `KeepAlive`
   plus the adapter's restart recovery make a relaunch safe.

3. Run the CRI suite against the managed adapter (do not let it manage its own):

   ```sh
   MACVZ_INTEGRATION=1 MACVZ_CRI_MANAGE=0 \
     MACVZ_CRI_SOCKET="$HOME/.macvz/cri/macvz-cri.sock" \
     ./test/e2e/cri-k3s/run.sh
   ```

4. Run the **real kubelet/k3s in-loop** suite (CRI-P9 follow-up #85) against the
   live cluster — this is the full `kubectl` fixture deployment, Service
   reachability, restart-recovery, and soak that `crictl` cannot cover:

   ```sh
   export KUBECONFIG=/path/to/k3s.yaml
   MACVZ_INTEGRATION=1 make cri-k3s-inloop
   ```

   It auto-detects the MacVz node by its runtime label, applies
   `fixtures/workload.yaml`, and runs the in-loop phases. Restart/audit phases
   take operator hooks (`MACVZ_RESTART_CRI_CMD`, `MACVZ_RESTART_K3S_CMD`,
   `MACVZ_ADAPTER_RSS_CMD`, `MACVZ_HOST_AUDIT_CMD`); an unset hook skips its phase
   loudly. See `docs/CRI_K3S_INLOOP_REPORT.md` for the runbook and evidence
   template.

   When the Linux control plane is a local kind cluster and the CRI adapter runs
   on a separate Mac, wire only the fixture CIDRs instead of changing either
   host's default route:

   ```sh
   # Local Mac: send the remote CRI node Pod CIDR to the remote Mac.
   sudo route -n add -net <remote-pod-cidr> <remote-mac-lan-ip>

   # Remote Mac: return the kind Linux-node Pod CIDR to the local Mac.
   ssh -t <remote-mac> 'sudo route -n add -net <kind-pod-cidr> <local-mac-lan-ip>'

   # kind control-plane node: SNAT Linux Pod -> remote MacVz Pod traffic so
   # replies return through Docker Desktop's host path.
   docker exec <kind-control-plane> iptables -t nat -I POSTROUTING 1 \
     -s <kind-pod-cidr> -d <remote-pod-cidr> -j MASQUERADE
   ```

   Example from the local two-Mac lab:

   ```sh
   sudo route -n add -net 10.244.102.0/24 192.168.1.122
   ssh -t test@192.168.1.122 \
     'sudo route -n add -inet 10.244.0.0/24 192.168.1.110'
   docker exec macvz61-control-plane iptables -t nat -I POSTROUTING 1 \
     -s 10.244.0.0/24 -d 10.244.102.0/24 -j MASQUERADE
   ```

## Cleanup

```sh
./scripts/macvz-cri-install.sh uninstall --purge
```

Uninstall removes the LaunchAgent, binary, and socket; `--purge` also deletes the
state dir. The suite asserts no stale socket, workload, or sandbox remains.

## LinuxPod backend (CRI-L, #125)

To point the node at the experimental **LinuxPod-backed** CRI path instead of the
default apple/container path, run a LinuxPod helper and start the adapter with
`--experimental-linuxpod-backend`. `linuxpod-inloop.sh` then drives a two-container
**app + late-sidecar** Pod (`fixtures/linuxpod-workload.yaml`) through k3s.

1. Build and start a LinuxPod helper on a unix socket. Until the real
   Apple Containerization helper (#126) lands, this is the simulated Swift stub
   (`Ping`→`simulated=true`), which proves the contract and serving chain but does
   **not** boot a real Pod VM:

   ```sh
   swift build --package-path test/e2e/cri-linuxpod-helper -c release
   ./test/e2e/cri-linuxpod-helper/.build/release/LinuxPodHelperStub \
     --socket "$HOME/.macvz/cri/linuxpod-helper.sock" &
   ```

2. Install the adapter with the LinuxPod backend selected (via `MACVZ_CRI_EXTRA`):

   ```sh
   make cri
   MACVZ_CRI_EXTRA="--experimental-linuxpod-backend \
     --linuxpod-helper-socket $HOME/.macvz/cri/linuxpod-helper.sock" \
     ./scripts/macvz-cri-install.sh install --from ./bin \
       --socket "$HOME/.macvz/cri/macvz-cri.sock" \
       --state-dir "$HOME/.macvz/cri/state"
   ```

   Point k3s at the endpoint exactly as in "Pointing k3s at macvz-cri" above.

3. Run the LinuxPod in-loop suite (gated; plan-only and exit 0 without the gates):

   ```sh
   MACVZ_INTEGRATION=1 KUBECONFIG=/path/to/k3s.yaml \
     MACVZ_CRI_SSH_TARGET=test@192.168.1.122 \
     MACVZ_RESTART_CRI_CMD="./test/e2e/cri-k3s/hooks/linuxpod-cri-restart.sh" \
     MACVZ_HELPER_SSH_TARGET=test@192.168.1.122 \
     MACVZ_RESTART_HELPER_CMD="./test/e2e/cri-k3s/hooks/linuxpod-helper-restart.sh" \
     MACVZ_ADAPTER_RSS_CMD="…" MACVZ_LINUXPOD_AUDIT_CMD="…" MACVZ_ROUTE_AUDIT_CMD="…" \
     ./test/e2e/cri-k3s/linuxpod-inloop.sh
   ```

   **Honesty gate:** the suite asserts shared sandbox namespace, sidecar localhost,
   rootfs identity verification, Pod IP readiness, logs, exec, stop/remove, restart
   recovery, and a residual-state audit *only when the Pod is genuinely
   LinuxPod-backed*. Against the simulated stub those assertions **skip loudly**
   with the #126/#127/#128/#129 blocker rather than passing falsely, so the report
   never claims live evidence the runtime did not produce. See the script header
   for the full env contract and `docs/CRI_RUNTIME_R17_LINUXPOD_BACKEND_REPORT.md`.

4. Run the LinuxPod **multi-Pod concurrency** suite (CRI-L6-3 #137; same gates,
   plan-only and exit 0 without them). It deploys N>=3 app+late-sidecar replicas
   onto the single MacVz node, asserts unique Pod IPs, distinct Service backends,
   direct Pod-IP reachability, per-Pod logs/exec/port-forward, bounded `macvz-cri`
   and `linuxpod-helper` restart recovery, no duplicate pf/binat/helper-work
   state, and bounded RSS + helper/process growth across scale churn:

   ```sh
   MACVZ_INTEGRATION=1 KUBECONFIG=/path/to/k3s.yaml \
     MACVZ_MULTIPOD_REPLICAS=3 \
     MACVZ_CRI_SSH_TARGET=test@192.168.1.122 \
     MACVZ_RESTART_CRI_CMD="./test/e2e/cri-k3s/hooks/linuxpod-cri-restart.sh" \
     MACVZ_HELPER_SSH_TARGET=test@192.168.1.122 \
     MACVZ_RESTART_HELPER_CMD="./test/e2e/cri-k3s/hooks/linuxpod-helper-restart.sh" \
     MACVZ_ADAPTER_RSS_CMD="…" MACVZ_LINUXPOD_HELPER_PROC_CMD="…" \
     MACVZ_LINUXPOD_AUDIT_CMD="…" MACVZ_LINUXPOD_DUP_AUDIT_CMD="…" \
     MACVZ_ROUTE_AUDIT_CMD="…" \
     ./test/e2e/cri-k3s/linuxpod-multipod.sh
   ```

   The same **honesty gate** applies per Pod: every replica must prove
   `simulated=false` via `MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD` or the LinuxPod
   acceptances skip loudly. See the script header for the full env contract.

## Known limitations

See `docs/CRI_FEASIBILITY.md` (CRI-P8) for the precise list — multi-container
Pods, host-namespace Pods (rejected with a clear diagnostic), and the kubelet's
ownership of probes/projected volumes in CRI mode.
