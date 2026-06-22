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
   runtime-private evidence channel (#116). Run the harness with `MACVZ_HANDOFF=1`
   (and optionally `MACVZ_HANDOFF_STATUS_CMD` to surface on-node
   `identityVerified` diagnostics). See `docs/CRI_RUNTIME_I4_2_INLOOP_HANDOFF_REPORT.md`.

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

## Cleanup

```sh
./scripts/macvz-cri-install.sh uninstall --purge
```

Uninstall removes the LaunchAgent, binary, and socket; `--purge` also deletes the
state dir. The suite asserts no stale socket, workload, or sandbox remains.

## Known limitations

See `docs/CRI_FEASIBILITY.md` (CRI-P8) for the precise list — multi-container
Pods, host-namespace Pods (rejected with a clear diagnostic), and the kubelet's
ownership of probes/projected volumes in CRI mode.
